package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

type rpcOutcome struct {
	result json.RawMessage
	err    *rpcErrPayload
}

type rpcErrPayload struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcErrPayload) Error() string {
	if e == nil {
		return "json-rpc error"
	}
	return fmt.Sprintf("json-rpc %d: %s", e.Code, e.Message)
}

type rpcNotifyHandler func(method string, params json.RawMessage)
type rpcRequestHandler func(method string, id json.RawMessage, params json.RawMessage)

// acpWriteTimeout bounds how long a single write to the agent's stdin may
// block before the write is abandoned. A child that wedges and stops
// reading stdin would otherwise pin t's write mutex forever and hang every
// subsequent call — this was the mechanism behind a reported silent wedge
// where a hung agent held all session locks indefinitely.
var acpWriteTimeout = 30 * time.Second

var errTransportClosed = errors.New("acp: transport closed")

// writeReq is one framed message queued for the dedicated writer pump.
type writeReq struct {
	data []byte
	res  chan error // buffered(1); receives the result of the underlying Write
}

// transport implements newline-delimited JSON-RPC 2.0 over a pair of streams.
type transport struct {
	in  *bufio.Reader
	out io.Writer

	nextID atomic.Int64

	pendingMu sync.Mutex
	pending   map[string]chan rpcOutcome

	// Writes are serialized through a single pump goroutine instead of a
	// mutex around the encoder. If the child stops draining stdin the pump
	// wedges in Write, but callers still unblock via acpWriteTimeout or
	// shutdown — the stall can no longer spread to other goroutines.
	writeCh   chan writeReq
	writeOnce sync.Once
	done      chan struct{}
	closeOnce sync.Once

	onNotif rpcNotifyHandler
	onReq   rpcRequestHandler
}

func newTransport(in io.Reader, out io.Writer, onNotif rpcNotifyHandler, onReq rpcRequestHandler) *transport {
	return &transport{
		in:      bufio.NewReader(in),
		out:     out,
		pending: make(map[string]chan rpcOutcome),
		writeCh: make(chan writeReq),
		done:    make(chan struct{}),
		onNotif: onNotif,
		onReq:   onReq,
	}
}

func (t *transport) readLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		line, err := t.readLine()
		if err != nil {
			if err != io.EOF {
				slog.Debug("acp: read error", "error", err)
			}
			t.shutdown(fmt.Errorf("acp: read closed: %w", err))
			return
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		t.dispatchLine(line)
	}
}

func (t *transport) readLine() ([]byte, error) {
	line, err := t.in.ReadBytes('\n')
	if err != nil {
		return line, err
	}
	return bytes.TrimSuffix(line, []byte("\r")), nil
}

func (t *transport) dispatchLine(line []byte) {
	var env struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
		Result  json.RawMessage `json:"result"`
		Error   *rpcErrPayload  `json:"error"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		slog.Debug("acp: skip non-json line", "line", string(line))
		return
	}
	if env.Method != "" {
		if isJSONRPCIDNullOrAbsent(env.ID) {
			if t.onNotif != nil {
				t.onNotif(env.Method, env.Params)
			}
			return
		}
		if t.onReq != nil {
			t.onReq(env.Method, env.ID, env.Params)
		}
		return
	}
	if !isJSONRPCIDNullOrAbsent(env.ID) {
		t.completePending(env.ID, env.Result, env.Error)
	}
}

func isJSONRPCIDNullOrAbsent(id json.RawMessage) bool {
	if len(id) == 0 {
		return true
	}
	return bytes.Equal(bytes.TrimSpace(id), []byte("null"))
}

func jsonIDKey(id json.RawMessage) string {
	id = bytes.TrimSpace(id)
	var n json.Number
	if json.Unmarshal(id, &n) == nil {
		return string(n)
	}
	var s string
	if json.Unmarshal(id, &s) == nil {
		return s
	}
	return string(id)
}

func (t *transport) completePending(id json.RawMessage, result json.RawMessage, rpcErr *rpcErrPayload) {
	key := jsonIDKey(id)
	t.pendingMu.Lock()
	ch, ok := t.pending[key]
	delete(t.pending, key)
	t.pendingMu.Unlock()
	if !ok {
		slog.Debug("acp: unmatched rpc response", "id", key)
		return
	}
	select {
	case ch <- rpcOutcome{result: result, err: rpcErr}:
	default:
	}
}

// shutdown permanently closes the transport: pending RPC calls, queued and
// in-flight writes, and future writeJSON callers all fail fast with err (or
// errTransportClosed). Idempotent; invoked when the read loop dies or the
// owning session is closed.
func (t *transport) shutdown(err error) {
	t.closeOnce.Do(func() {
		close(t.done)
		t.cancelAll(err)
	})
}

func (t *transport) cancelAll(err error) {
	t.pendingMu.Lock()
	defer t.pendingMu.Unlock()
	msg := err.Error()
	for k, ch := range t.pending {
		select {
		case ch <- rpcOutcome{err: &rpcErrPayload{Code: -32000, Message: msg}}:
		default:
		}
		delete(t.pending, k)
	}
}

func (t *transport) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := t.nextID.Add(1)
	key := fmt.Sprintf("%d", id)
	ch := make(chan rpcOutcome, 1)
	t.pendingMu.Lock()
	t.pending[key] = ch
	t.pendingMu.Unlock()

	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}
	if err := t.writeJSON(req); err != nil {
		t.pendingMu.Lock()
		delete(t.pending, key)
		t.pendingMu.Unlock()
		return nil, fmt.Errorf("acp: write %s: %w", method, err)
	}
	select {
	case <-ctx.Done():
		t.pendingMu.Lock()
		delete(t.pending, key)
		t.pendingMu.Unlock()
		return nil, ctx.Err()
	case out := <-ch:
		if out.err != nil {
			return nil, out.err
		}
		return out.result, nil
	}
}

// callBound issues an RPC that gives up after d. Use it for every request
// whose response must arrive promptly (handshake, set_mode, list, ...). Do
// NOT use it for session/prompt — that response legitimately arrives only
// when the whole turn finishes, so it stays bounded by the caller ctx and
// the engine's event-idle watchdog instead.
func (t *transport) callBound(ctx context.Context, d time.Duration, method string, params any) (json.RawMessage, error) {
	if d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	res, err := t.call(ctx, method, params)
	if errors.Is(err, context.DeadlineExceeded) {
		return nil, fmt.Errorf("acp: %s: no response within %s", method, d)
	}
	return res, err
}

func (t *transport) sendNotification(method string, params any) error {
	return t.writeJSON(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
}

// writePump serializes all outbound frames. It exits when the transport is
// shut down or a write fails permanently; a blocked Write leaves only this
// goroutine wedged, never callers (they time out via acpWriteTimeout).
func (t *transport) writePump() {
	for {
		select {
		case <-t.done:
			return
		case req := <-t.writeCh:
			_, err := t.out.Write(req.data)
			select {
			case req.res <- err:
			default:
			}
			if err != nil {
				t.shutdown(fmt.Errorf("acp: write: %w", err))
				return
			}
		}
	}
}

func (t *transport) writeJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')

	t.writeOnce.Do(func() { go t.writePump() })
	req := writeReq{data: data, res: make(chan error, 1)}
	timer := time.NewTimer(acpWriteTimeout)
	defer timer.Stop()
	select {
	case <-t.done:
		return errTransportClosed
	case t.writeCh <- req:
	case <-timer.C:
		return fmt.Errorf("acp: write timed out after %s", acpWriteTimeout)
	}
	// Once the pump took the request it always signals res (the channel is
	// buffered) — success and transport death can race here, and res must
	// win. Shutdown is still honored: a dead pipe fails the Write and the
	// error arrives through res; a Write blocked on a wedged child is
	// bounded by the remaining write timeout.
	select {
	case err := <-req.res:
		return err
	case <-timer.C:
		return fmt.Errorf("acp: write timed out after %s", acpWriteTimeout)
	}
}

type rpcResponseMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcErrPayload  `json:"error,omitempty"`
}

func (t *transport) respondSuccess(id json.RawMessage, result any) error {
	return t.writeJSON(rpcResponseMsg{JSONRPC: "2.0", ID: id, Result: result})
}

func (t *transport) respondError(id json.RawMessage, code int, message string) error {
	return t.writeJSON(rpcResponseMsg{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &rpcErrPayload{Code: code, Message: message},
	})
}
