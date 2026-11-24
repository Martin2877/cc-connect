# Remote Access Deployment: Feishu Bot + VPS Reverse Tunnel

This guide deploys the full "drive your coding agent from your phone" setup:

- **Feishu/Lark bot → cc-connect → local AI agent** (works with no public IP —
  Feishu uses an outbound WebSocket)
- **Browser IDE / web terminal / cc-connect console / dev-server previews**
  exposed through a VPS: local services bind `127.0.0.1` only and are reached
  through an `autossh -R` reverse tunnel + Caddy HTTPS reverse proxy.
  No Tailscale or inbound ports needed on the local machine.

Everything here is parameterized — no real domains, credentials, or user IDs
are stored in this repo.

## Topology

```
 ┌─ Feishu app on phone ─┐   ┌─ Browser ────────────────────────────────┐
 │   bot chat            │   │ https://code.EXAMPLE.COM  (IDE)          │
 └─────────┬─────────────┘   │ https://term.EXAMPLE.COM  (web terminal) │
           │ WebSocket       │ https://cc.EXAMPLE.COM    (cc-connect UI)│
           ▼ (outbound)      │ https://p{port}.EXAMPLE.COM (previews)   │
   ┌───────────────┐         └──────────────┬───────────────────────────┘
   │ Feishu cloud  │                        │ :443 (HTTPS)
   └───────┬───────┘                ┌───────▼────────┐
           │                        │  VPS: Caddy    │ reverse proxy
           │                        │  127.0.0.2:*   │ (loopback only)
           │                        └───────▲────────┘
           │                                │ autossh -R (outbound)
 ┌─────────▼────────────────────────────────┴─────────┐
 │ LOCAL MACHINE (no public IP, no inbound ports)     │
 │  cc-connect (:9820 web UI)  ← Feishu WebSocket     │
 │  code-server   127.0.0.1:8080  → VPS 127.0.0.2:18080│
 │  gotty+screen  127.0.0.1:7681  → VPS 127.0.0.2:18081│
 │  cc-connect UI 127.0.0.1:9820  → VPS 127.0.0.2:18082│
 │  dev servers   127.0.0.1:PORT  → VPS 127.0.0.2:PORT │
 └────────────────────────────────────────────────────┘
```

> The tunnel binds `127.0.0.2` (not `127.0.0.1`) on the VPS so the sshd
> `PermitListen` whitelist is unambiguous and Caddy is the only process
> allowed to reach the forwarded ports.

## Port plan (defaults — change to taste)

| Service                | Local            | VPS loopback     | Public URL                    |
|------------------------|------------------|------------------|-------------------------------|
| code-server (IDE)      | 127.0.0.1:8080   | 127.0.0.2:18080  | `https://code.EXAMPLE.COM`    |
| gotty web terminal     | 127.0.0.1:7681   | 127.0.0.2:18081  | `https://term.EXAMPLE.COM`    |
| cc-connect web console | 127.0.0.1:9820   | 127.0.0.2:18082  | `https://cc.EXAMPLE.COM`      |
| dev-server previews    | 127.0.0.1:`PORT` | 127.0.0.2:`PORT` | `https://pPORT.EXAMPLE.COM`   |

Preview ports forwarded by default: 3000, 3001, 4000, 4200, 5000, 5173,
5174, 8000, 8001, 8888, 9000 — edit the `-R` list to match what you run.
For preview URLs see the two schemes in `deploy/vps/Caddyfile` (per-port
hostnames, or `*.p.` wildcard DNS + on-demand TLS).

---

## Part 1 — Feishu bot + cc-connect

### 1. Create the Feishu app

Follow `docs/feishu.md` (企业自建应用, bot capability, event subscription).
Note the `app_id` / `app_secret`. You need **no** public callback URL —
cc-connect uses Feishu's outbound WebSocket mode.

> `cc-connect feishu setup` also works, but it creates new projects with
> agent type `codex`; write `config.toml` manually when using another agent.

### 2. Configure `~/.cc-connect/config.toml`

```toml
[[projects]]
name       = "dev-local"
work_dir   = "/path/to/your/work"
admin_from = ""           # fill after /whoami (see below)

[projects.agent]
type = "devin"            # or claudecode / codex / acp / ...
[projects.agent.options]
cmd      = "/absolute/path/to/devin"   # absolute path! launchd PATH is minimal
work_dir = "/path/to/your/work"
rpc_timeout_secs = 30                   # hung-agent self-heal (see TROUBLESHOOTING)

[[projects.platforms]]
type = "feishu"
[projects.platforms.options]
app_id     = "cli_XXXXXXXXXXXXXXXX"
app_secret = "sec_XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX"
allow_from = ""           # fill after /whoami — REQUIRED, see below
```

> Use the option key `cmd` (`command` is deprecated — it still works but logs
> a warning).

### 3. Lock down access (important)

Until `allow_from` is set, **anyone who finds the bot can run your agent**.

1. Start cc-connect, send any message to the bot, then send `/whoami` —
   the reply is your Feishu `open_id` (also visible in the log).
2. Set `allow_from = "<your open_id>"` under `[projects.platforms.options]`
   so only you can talk to the bot.
3. Set `admin_from = "<your open_id>,web-admin"` under `[[projects]]` so
   `/shell /dir /show /restart /upgrade` work for you (Feishu + web UI).
4. `cc-connect daemon restart`.

### 4. Run persistently + enable web console

```bash
cc-connect daemon install     # launchd (macOS) / systemd unit, autostarts
cc-connect web --no-browser   # writes [management] into config, restart after
cc-connect daemon restart
```

- Web console: `http://localhost:9820/login?token=<management.token>`
  (the token is written into `[management] token` by `cc-connect web`).
- Logs: `cc-connect daemon logs` or `~/.cc-connect/logs/cc-connect.log`.

> Note: `cc-connect web` **rewrites** `config.toml` (normalizes formatting,
> adds `[management]`/`[bridge]`/... sections). Always re-read the file
> before editing it by hand.

---

## Part 2 — VPS (any small Linux box with a public IP)

Files: `deploy/vps/`

1. **Restricted forward-only user** — apply `vps/sshd-tunnel.conf` as
   `/etc/ssh/sshd_config.d/60-tunnel-fwd.conf`, then `sshd -t && systemctl reload sshd`:
   - user `fwd`, shell `/sbin/nologin`
   - only remote TCP forwarding, only on `127.0.0.2`, only whitelisted ports
   - no TTY, no X11/agent forwarding, no tunnel devices, `GatewayPorts no`
2. **Caddy** — install the official binary (see comments in
   `vps/caddy.service`), copy `vps/Caddyfile` → `/etc/caddy/Caddyfile`,
   replace `EXAMPLE.COM` and the `basic_auth` hash, `systemctl enable --now caddy`.
   Generate a bcrypt password hash with `caddy hash-password`.
3. **Firewall** — open only 443 (and your SSH port): e.g.
   `firewall-cmd --permanent --add-service=https && firewall-cmd --reload`.
4. **DNS** — `A` records (or one wildcard `*.EXAMPLE.COM`) for
   `code`, `term`, `cc`, `p*` pointing at the VPS.
5. Add the local machine's **tunnel public key** to
   `/home/fwd/.ssh/authorized_keys` (created in Part 3 step 3).

---

## Part 3 — Local machine

Files: `deploy/macos/` (launchd) or `deploy/linux/` (systemd user units).

### code-server (browser IDE)

```bash
# Download the release tarball (brew on macOS Intel has no working bottle):
curl -fL -o /tmp/code-server.tgz \
  https://github.com/coder/code-server/releases/download/v4.104.3/code-server-4.104.3-macos-amd64.tar.gz
mkdir -p ~/.local/lib && tar -xzf /tmp/code-server.tgz -C ~/.local/lib
mv ~/.local/lib/code-server-* ~/.local/lib/code-server
ln -sf ~/.local/lib/code-server/bin/code-server ~/.local/bin/code-server
```

`~/.config/code-server/config.yaml` — keep it loopback-only (auth stays on
because Caddy terminates TLS but the tunnel endpoint is reachable by anyone
on the VPS loopback):

```yaml
bind-addr: 127.0.0.1:8080
auth: password
password: "REPLACE_ME"
cert: false
disable-telemetry: true
disable-update-check: true
```

### gotty web terminal

`ttyd` has no macOS-Intel bottle; the `sorenisanerd/gotty` static binary works:

```bash
curl -fL -o ~/.local/bin/gotty \
  https://github.com/sorenisanerd/gotty/releases/download/v1.11.0/gotty_v1.11.0_darwin_amd64
chmod +x ~/.local/bin/gotty
```

It serves `screen -xRR webterm` (attach to a persistent `screen` session
named `webterm`, create if missing) on `127.0.0.1:7681`, with `--ws-origin`
restricted to your `term.` domain. Authentication lives at the Caddy layer
(`basic_auth`) — a single outer layer is enough.

### Tunnel key + autossh

```bash
ssh-keygen -t ed25519 -f ~/.ssh/id_cc_tunnel -N "" -C "cc-tunnel"
# add id_cc_tunnel.pub to the fwd user's authorized_keys on the VPS
```

macOS: `deploy/macos/install.sh` renders the three launchd plists
(`com.ccc.{code-server,webterm,tunnel}`) into `~/Library/LaunchAgents` and
loads them (`KeepAlive` + `RunAtLoad`). Linux: install the three user units
under `~/.config/systemd/user/` from `deploy/linux/` and
`systemctl --user enable --now ...`.

Logs: `~/Library/Logs/ccc/` (macOS) or `journalctl --user -u ccc-*` (Linux).

### Verify end to end

```bash
ssh -i ~/.ssh/id_cc_tunnel fwd@VPS_HOST   # must fail: nologin shell
ss -ltnp | grep 127.0.0.2                  # on VPS: 18080/18081/18082 + ports
curl -I https://code.EXAMPLE.COM           # 401/200 via Caddy
```

---

## Part 4 — `/links` command

This fork adds a `/links` bot command that renders your endpoints as link
buttons (Feishu `multi_url`). Configure in `~/.cc-connect/config.toml`:

```toml
[projects.links]
ide     = "https://code.EXAMPLE.COM/?folder={work_dir}"
term    = "https://term.EXAMPLE.COM/"
web     = "https://cc.EXAMPLE.COM/"
preview = "https://{port}.p.EXAMPLE.COM/"   # scheme (b) in deploy/vps/Caddyfile
# preview = "https://p{port}.EXAMPLE.COM/"  # scheme (a): one hostname per port
```

- `/links` → card with buttons (omitted entries are skipped)
- `/links preview 5173` → expanded URL for a dev-server port
- Placeholders: `{work_dir}` (current session dir), `{project}`, `{port}`

See `config.example.toml` for the full commented block.

---

## Operations cheat sheet

```bash
cc-connect daemon status|logs|restart|stop|uninstall
pgrep -fl "devin acp"          # find a wedged agent process
kill -9 <pid> && cc-connect daemon restart   # recover a stuck session
tail -f ~/Library/Logs/ccc/tunnel.err        # tunnel health
```

Known failure modes and fixes (incident history → code changes shipped in
this repo): see `deploy/TROUBLESHOOTING.md`.
