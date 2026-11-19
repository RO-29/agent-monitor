#!/usr/bin/env bash
# Run each hook N times through `sh -c` against a local fake daemon, shell vs am-helper; print wall ms per event and peak RSS.
# Usage: rust/am-helper/measure-hooks.sh [N]  (default 200; the live daemon is not touched)
set -euo pipefail
cd "$(dirname "$0")"
export PATH="$HOME/.cargo/bin:$PATH"
n=${1:-200}
cargo build --release -q
tmp=$(mktemp -d /tmp/amh.XXXX)
trap 'kill "$srv" 2>/dev/null || true; rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin" "$tmp/home/.am"
cp ../../bin/claude-hook.sh ../../bin/codex-hook.sh "$tmp/bin/"
cp target/release/am-helper "$tmp/am-helper"
# A tmux that knows no panes, so codex runs never touch the real server.
mkdir -p "$tmp/fake" && printf '#!/bin/sh\nexit 0\n' >"$tmp/fake/tmux" && chmod +x "$tmp/fake/tmux"
export PATH="$tmp/fake:$PATH"
/usr/bin/python3 - "$tmp/port" <<'EOF' &
import http.server, sys, socketserver
class H(http.server.BaseHTTPRequestHandler):
    def _ok(self):
        n = int(self.headers.get("Content-Length") or 0); self.rfile.read(n)
        b = b'{"ok":true}\n'
        if self.path.startswith("/api/approvals/hook/codex"):
            b = b'{"hookSpecificOutput":{"decision":{"behavior":"allow"},"hookEventName":"PermissionRequest"}}\n'
        self.send_response(200); self.send_header("Content-Length", str(len(b))); self.end_headers(); self.wfile.write(b)
    do_POST = do_DELETE = _ok
    def log_message(self, *a): pass
class S(socketserver.ThreadingMixIn, http.server.HTTPServer): daemon_threads = True
s = S(("127.0.0.1", 0), H); open(sys.argv[1], "w").write(str(s.server_address[1])); s.serve_forever()
EOF
srv=$!
while [ ! -s "$tmp/port" ]; do sleep 0.05; done
export AGENT_MONITOR_URL="http://127.0.0.1:$(cat "$tmp/port")" AGENT_MONITOR_HOME="$tmp/home/.am" TMUX_PANE=%1 TMUX=/tmp/x,1,0
sid=3f2a9c1e-8b7d-4e6f-a1b2-c3d4e5f60789
measure() { # label script payload rust(0|1)
  local f="$tmp/payload" t0 t1 rss
  printf '%s' "$3" >"$f"
  export AGENT_MONITOR_RUST_HELPERS=$([ "$4" = 1 ] && echo 1 || echo 0)
  t0=$(/usr/bin/python3 -c 'import time;print(time.time())')
  for _ in $(seq "$n"); do /bin/sh -c "$tmp/bin/$2" <"$f" >/dev/null; done
  t1=$(/usr/bin/python3 -c 'import time;print(time.time())')
  # Peak RSS of the largest process in one run (time -l reports the max over the tree's waited children).
  rss=$( { /usr/bin/time -l /bin/sh -c "$tmp/bin/$2" <"$f" >/dev/null; } 2>&1 | awk '/maximum resident/ {print int($1/1024)}')
  printf '%-34s %-5s %7.2f ms/event  peak_rss_kb=%s\n' "$1" "$([ "$4" = 1 ] && echo rust || echo shell)" "$(echo "($t1-$t0)*1000/$n" | bc -l)" "$rss"
}
for ev in SessionStart Stop SessionEnd; do
  p="{\"session_id\":\"$sid\",\"cwd\":\"/w\",\"hook_event_name\":\"$ev\"}"
  measure "claude $ev" claude-hook.sh "$p" 0
  measure "claude $ev" claude-hook.sh "$p" 1
done
unset TMUX
for ev in UserPromptSubmit SessionEnd; do
  p="{\"session_id\":\"$sid\",\"cwd\":\"/w\",\"hook_event_name\":\"$ev\"}"
  measure "codex $ev (no tmux)" codex-hook.sh "$p" 0
  measure "codex $ev (no tmux)" codex-hook.sh "$p" 1
done
p="{\"session_id\":\"$sid\",\"hook_event_name\":\"PermissionRequest\",\"tool_name\":\"Bash\",\"tool_input\":{\"command\":\"ls\"}}"
measure "codex PermissionRequest (allow)" codex-hook.sh "$p" 0
measure "codex PermissionRequest (allow)" codex-hook.sh "$p" 1
