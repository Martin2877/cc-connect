# Troubleshooting: Feishu + agent + tunnel deployment

Real incidents from running this stack, what caused them, and how each was
fixed. Ordered most-important first.

## 1. Bot receives messages but never replies (agent silently wedged)

**Symptom.** Feishu delivers the message (`message received` in the log)
and then nothing: no `processing message`, no error, no reply — forever.
`/new` doesn't recover it either.

**Root cause (now fixed in this repo).** A live-but-hung `devin acp`
process deadlocked the whole session:

1. During `session/load` the agent replayed >128 `session/update`
   notifications, filling the bounded events channel while nobody was
   draining it during the handshake.
2. `emit()` blocked → the transport read loop stalled → the `session/load`
   response was never read → the RPC call hung forever.
3. `agent.StartSession` runs while the engine holds `interactiveMu`, so
   `/new` and every later message deadlocked silently.
4. The `busy_timeout_mins` self-heal only fires when the agent process is
   **dead** — a live wedged process (even a zombie) defeated it.

**Fixes shipped in this repo** (see `agent/acp/`):

- `callBound()` — every control RPC (`initialize`, `authenticate`,
  `session/load|resume`, `set_*`, `session/list`) now has a timeout
  (default 30s; `session/load`/`resume` floor of 2min for history replay).
  A hung agent is abandoned with an error instead of wedging silently.
- `writePump` + `acpWriteTimeout` (30s) — a child that stops reading stdin
  can no longer pin every caller on the write mutex.
- `emit()` never blocks — overflow goes to an internal buffer drained by a
  pump goroutine, preserving order.
- `Alive()` probes the kernel (Wait4/WNOHANG on Unix) so zombie processes
  no longer count as alive.
- Option `rpc_timeout_secs` on the agent (`[projects.agent.options]`)
  tunes the control-RPC timeout; agents embedding `acp` (devin) inherit it.

**If it still happens** (e.g. agent wedge of a different kind):

```bash
pgrep -fl "devin acp"          # look for old 0%-CPU processes
kill -9 <pid>
cc-connect daemon restart      # session locks are in-memory only
```

**Remaining caveat.** `busy_timeout_mins` still only helps once the process
is dead. A turn that runs forever with a *responsive* RPC transport (e.g.
model API hangs without the transport dying) is bounded only by the
event-idle watchdog — consider it for very long-running agents.

## 2. `localhost:9820` has nothing listening

The web console is **off by default** — gated by `[management] enabled`.

```bash
cc-connect web --no-browser   # auto-writes [management] into config.toml
cc-connect daemon restart
```

Login URL: `http://localhost:9820/login?token=<management.token>`.

`cc-connect web` rewrites `config.toml` (normalizes formatting, adds
`[management]`/`[bridge]`/`[speech]`/`[tts]` sections) — always re-read the
file before editing by hand.

## 3. Any Feishu user can drive your agent

Until `allow_from` is set, whoever finds the bot controls the agent.
Lockdown sequence: send `/whoami` to the bot → put your `open_id` into
`allow_from` (platform options) and `admin_from` (`[[projects]]`, also add
`web-admin` for the web UI) → `cc-connect daemon restart`.

## 4. `work_dir` shown by `/dir` doesn't match config.toml

`/dir` overrides persist per-project in
`~/.cc-connect/projects/<name>.state.json` (`work_dir_override`) and
survive restarts. `cc-connect send -m "/dir ..."` does **not** run bot
commands — it injects text into the agent prompt. Fix by sending `/dir`
from an admin account, or edit `state.json` + restart.

## 5. Agent options: `command` vs `cmd`, `feishu setup` defaults

- `command` is deprecated; use `cmd`. Both parse via `core.ParseCmdOpts`.
- `feishu setup` creates new projects with agent type `codex` — write
  config.toml manually for other agent types.
- Use an **absolute** path for `cmd`: launchd's PATH doesn't include
  `~/.local/bin`.

## 6. Tunnel checklist

- `ssh -i key fwd@host` must fail instantly (nologin shell) — that's correct.
- `ExitOnForwardFailure=yes` makes autossh die if a `PermitListen` port is
  refused; check `autossh.log`/journal and the VPS sshd drop-in match.
- `PermitListen` entries use the bind address `127.0.0.2`, matching the
  `-R 127.0.0.2:...` arguments — a `127.0.0.1` pattern won't match.
- gotty `--ws-origin` is an anchored regex; escape the dots
  (`^https://term\.example\.com$`).
- code-server: keep `bind-addr: 127.0.0.1:8080` + `auth: password` — Caddy
  terminates TLS but the tunnel port is still reachable from the VPS
  loopback.

## 7. Build/install gotchas (this repo)

- `web/dist` must be built before `go build` (`make web`, or
  `cd web && npm install && npm run build`) — the frontend is embedded via
  `//go:embed all:dist`.
- On some systems `cp` to `/usr/local/bin` fails where
  `install -m 755 cc-connect /usr/local/bin/` works.
- Known unrelated test failures on a clean checkout: tests comparing
  `/var` vs `/private/var` paths on macOS, workspace-skill discovery, and
  `TestCursorCLI_ACPHandshake` (needs `agent login` credentials).
