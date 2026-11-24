#!/usr/bin/env bash
# Installs a build copied by bin/remote-build.sh into the live checkout, then restarts the daemon.
# Usage: bin/install-build.sh <commit> [--app]   (--app also replaces ~/Applications/agent-monitor.app)
set -euo pipefail
SHA="${1:?usage: bin/install-build.sh <commit> [--app]}"
SRC="$HOME/agent-monitor-builds/$SHA"
LIVE="${AM_REPO:-$HOME/agent-monitor}"
[[ -x "$SRC/agent-monitor" && "$(cat "$SRC/COMMIT")" == "$SHA" ]] || { echo "no build for $SHA in $SRC" >&2; exit 1; }
# The scripts and hooks come from git; the binaries come from the build Mac.
git -C "$LIVE" fetch -q origin && git -C "$LIVE" merge -q --ff-only "$SHA"
# rm first: a copy over the old file keeps its inode, and macOS then kills the new binary (code signature).
rm -f "$LIVE/agent-monitor" && cp "$SRC/agent-monitor" "$LIVE/agent-monitor"
cp "$SRC/am-helper" "$LIVE/.am-helper.new" && mv -f "$LIVE/.am-helper.new" "$LIVE/am-helper" && touch "$LIVE/am-helper.hooks"
if [[ "${2:-}" == --app ]]; then
  rm -rf "$HOME/Applications/agent-monitor.app" && ditto "$SRC/agent-monitor.app" "$HOME/Applications/agent-monitor.app"
fi
launchctl kickstart -k "gui/$(id -u)/com.rohit.agent-monitor"
# The daemon loads every session before it listens: wait for it (up to 60 s) before the smoke test.
for _ in $(seq 1 60); do curl -fsS -o /dev/null --max-time 2 http://127.0.0.1:7777/api/health 2>/dev/null && break; sleep 1; done
python3 "$LIVE/e2e/smoke.py" --no-agent
