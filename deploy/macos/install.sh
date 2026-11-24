#!/bin/sh
# Bootstrap the remote-access stack on macOS (launchd):
#   - renders the three plists into ~/Library/LaunchAgents
#   - generates the dedicated tunnel SSH key if missing
#   - loads the services (KeepAlive + RunAtLoad)
#
# Usage:
#   ./install.sh <vps-host> <term-origin-regex>
# Example:
#   ./install.sh vps.example.com '^https://term\.example\.com$'
#
# Prereqs: autossh (brew install autossh), screen, code-server and gotty
# installed per deploy/README.md. Re-run is idempotent.

set -eu

VPS_HOST="${1:?usage: install.sh <vps-host> <term-origin-regex>}"
TERM_ORIGIN="${2:?usage: install.sh <vps-host> <term-origin-regex>}"

HERE="$(cd "$(dirname "$0")" && pwd)"
AGENTS_DIR="$HOME/Library/LaunchAgents"
LOGS_DIR="$HOME/Library/Logs/ccc"
KEY="$HOME/.ssh/id_cc_tunnel"
AUTOSSH="$(command -v autossh || true)"

[ -n "$AUTOSSH" ] || { echo "autossh not found: brew install autossh" >&2; exit 1; }
mkdir -p "$AGENTS_DIR" "$LOGS_DIR" "$HOME/.ssh"

if [ ! -f "$KEY" ]; then
  ssh-keygen -t ed25519 -f "$KEY" -N "" -C "cc-tunnel-$(hostname -s)"
  echo ">> created $KEY — append $KEY.pub to the fwd user's authorized_keys on the VPS"
fi

render() { # render <template> <out>
  sed -e "s|{{HOME}}|$HOME|g" \
      -e "s|{{AUTOSSH}}|$AUTOSSH|g" \
      -e "s|{{VPS_HOST}}|$VPS_HOST|g" \
      -e "s|{{TERM_ORIGIN}}|$TERM_ORIGIN|g" \
      -e "s|{{HOSTNAME}}|$(hostname -s)|g" \
      "$HERE/launchd/$1" > "$AGENTS_DIR/$2"
  plutil -lint "$AGENTS_DIR/$2" >/dev/null
  echo "wrote $AGENTS_DIR/$2"
}

render com.ccc.code-server.plist com.ccc.code-server.plist
render com.ccc.webterm.plist     com.ccc.webterm.plist
render com.ccc.tunnel.plist      com.ccc.tunnel.plist

for label in com.ccc.code-server com.ccc.webterm com.ccc.tunnel; do
  launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true
  launchctl bootstrap "gui/$(id -u)" "$AGENTS_DIR/$label.plist"
  echo "loaded $label"
done

cat <<EOF

Done. Next steps:
  1. VPS: install deploy/vps/sshd-tunnel.conf, add $KEY.pub to fwd's
     authorized_keys, configure Caddyfile, open 443.
  2. Logs: $LOGS_DIR/
  3. Verify on the VPS:  ss -ltn | grep 127.0.0.2
EOF
