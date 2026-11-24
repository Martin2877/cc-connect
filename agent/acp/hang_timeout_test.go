package acp

// Regression tests for the 2026-11-23 silent-wedge incident: a live-but-hung
// ACP agent (never answered session/load after replaying history) left
// StartSession blocked forever while the engine held session locks, so every
// subsequent user message was silently dropped. These tests pin the bounds
// that now make such a wedge fail fast instead of hanging.

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// startACPSilentServer wires a transport to a server that reads every
// request but only answers the first `answerFirst` of them; the rest get no
// response at all, simulating an agent that wedges mid-handshake.
func startACPSilentServer(t *testing.T, answerFirst int) (*transport, context.CancelFunc) {
	t.Helper()
	clientIn, serverOut := io.Pipe()
	serverIn, clientOut := io.Pipe()
	tr := newTransport(clientIn, clientOut, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go tr.readLoop(ctx)
	go func() {
		br := bufio.NewReader(serverIn)
		enc := json.NewEncoder(serverOut)
		for {
			line, err := br.ReadBytes('\n')
			if err != nil {
				return
			}
			if answerFirst <= 0 {
				continue
			}
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if json.Unmarshal(line, &req) != nil || req.Method == "" {
				continue
			}
			answerFirst--
			_ = enc.Encode(rpcResponseMsg{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result:  initializeResult(true, false, false),
			})
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = serverOut.Close()
		_ = clientOut.Close()
	})
	return tr, cancel
}

// TestHandshake_UnresponsiveResume_TimesOut drives handshake() against a
// server that answers initialize but never answers session/load. Before the
// fix the call blocked forever; it must now surface a timeout error bounded
// by the configured timeouts (resume times out, the session/new fallback
// then also fails fast on the silent server).
func TestHandshake_UnresponsiveResume_TimesOut(t *testing.T) {
	tr, _ := startACPSilentServer(t, 1) // answer initialize only
	s := newHandshakeTestSession(tr)
	s.rpcTimeout = 300 * time.Millisecond
	s.resumeTimeout = 300 * time.Millisecond

	start := time.Now()
	err := s.handshake("sess-1", "")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("handshake succeeded against a silent session/load; want timeout")
	}
	if !strings.Contains(err.Error(), "no response within") {
		t.Fatalf("handshake error does not look like a timeout: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("handshake took %s; want it bounded near the configured timeouts", elapsed)
	}
}

// TestCallBound_UnresponsiveServer is the transport-level counterpart:
// callBound must return a bounded timeout error when the peer never replies.
func TestCallBound_UnresponsiveServer(t *testing.T) {
	tr, _ := startACPSilentServer(t, 0) // never answers
	start := time.Now()
	_, err := tr.callBound(context.Background(), 300*time.Millisecond, "initialize", map[string]any{})
	if err == nil {
		t.Fatal("callBound succeeded against a silent peer; want timeout")
	}
	if !strings.Contains(err.Error(), "no response within") {
		t.Fatalf("callBound error = %v; want 'no response within'", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("callBound took %s; want ~300ms", elapsed)
	}
}

// TestEmit_NeverBlocksProducer fills the consumer-visible events channel
// beyond capacity without draining it. Before the fix emit() blocked on the
// full channel, which stalled the transport read loop mid-handshake during
// session/load replay (the incident's root cause). emit must now return
// immediately and the pump must still deliver every event once a reader
// shows up.
func TestEmit_NeverBlocksProducer(t *testing.T) {
	const n = 300 // far beyond the events channel capacity
	s := &acpSession{
		events:        make(chan core.Event, 8),
		ctx:           context.Background(),
		permByID:      make(map[string]permState),
		toolInputByID: make(map[string]string),
		acpSessID:     "sess-x",
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < n; i++ {
			s.emit(core.Event{Type: core.EventText, Content: "chunk"})
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("emit blocked on a full events channel (pre-fix backpressure wedge)")
	}

	got := 0
	timeout := time.After(5 * time.Second)
	for got < n {
		select {
		case ev := <-s.events:
			if ev.Type != core.EventText {
				t.Fatalf("unexpected event type %v", ev.Type)
			}
			got++
		case <-timeout:
			t.Fatalf("pump delivered %d/%d events then stalled", got, n)
		}
	}
}

// TestWriteJSON_BlockedStdinTimesOut pins the write-path bound: when the
// child stops reading stdin, writeJSON must fail after acpWriteTimeout
// instead of pinning the writer mutex for every subsequent call.
func TestWriteJSON_BlockedStdinTimesOut(t *testing.T) {
	old := acpWriteTimeout
	acpWriteTimeout = 200 * time.Millisecond
	defer func() { acpWriteTimeout = old }()

	// An io.Pipe blocks on the first Write while nobody reads — the stall
	// being bounded here.
	r, w := io.Pipe()
	defer r.Close()
	tr := newTransport(strings.NewReader(""), w, nil, nil)

	start := time.Now()
	err := tr.writeJSON(map[string]any{"jsonrpc": "2.0", "method": "ping"})
	if err == nil {
		t.Fatal("writeJSON succeeded with a blocked stdin pipe; want timeout")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("writeJSON blocked for %s; want ~200ms", elapsed)
	}
	// Subsequent writes must also fail fast rather than queue behind the
	// wedged pump.
	if err := tr.writeJSON(map[string]any{"a": 1}); err == nil {
		t.Fatal("second writeJSON succeeded; want fast failure")
	}
}

// supportsZombieProbe reports whether this platform's processAlive does a
// real kernel probe (unix) rather than the flag-only Windows stub.
func supportsZombieProbe() bool {
	return runtime.GOOS != "windows"
}

// TestAlive_DeadProcessProbe verifies that a killed child is reported dead
// even before the reaper goroutine runs — the zombie scenario where Alive()
// previously kept returning true and the stale-lock self-heal never fired.
func TestAlive_DeadProcessProbe(t *testing.T) {
	if !supportsZombieProbe() {
		t.Skip("no process probe on this platform")
	}
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn helper process: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	s := &acpSession{cmd: cmd}
	s.alive.Store(true)
	if !s.Alive() {
		t.Fatal("Alive()=false for a running child; want true")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !s.Alive() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("Alive() still true after child was killed (zombie masquerade)")
}
