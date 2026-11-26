#!/usr/bin/env bash
# Screenshots of the web app with sample data only, taken on the build Mac after bin/remote-build.sh.
# Usage: bin/screenshots.sh   (output: ~/agent-monitor-shots/<commit>/, 16 PNG files)
set -euo pipefail
[[ -f "$HOME/.config/agent-monitor/build.env" ]] && source "$HOME/.config/agent-monitor/build.env"
HOST="${AM_BUILD_HOST:?set AM_BUILD_HOST=user@host in ~/.config/agent-monitor/build.env}"
DIR="${AM_BUILD_DIR:-am-build}"
SHA=$(ssh "$HOST" "cd ~/$DIR && git rev-parse --short HEAD")
ssh "$HOST" bash -s <<EOF
set -euo pipefail
export PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin
cd ~/$DIR/web
# Port 9 has no daemon: every /api call fails, so only the sample data can show.
AM_TARGET=127.0.0.1:9 npx vite --port 5199 --strictPort >/tmp/am-shots-vite.log 2>&1 &
VITE=\$!
trap 'kill \$VITE 2>/dev/null || true' EXIT
for _ in \$(seq 1 60); do curl -fsS -o /dev/null --max-time 2 http://127.0.0.1:5199/ && break; sleep 1; done
rm -rf /tmp/am-shots && node ../bin/screenshots.mjs http://127.0.0.1:5199 /tmp/am-shots
EOF
OUT="$HOME/agent-monitor-shots/$SHA"
mkdir -p "$OUT"
rsync -a --delete "$HOST:/tmp/am-shots/" "$OUT/"
echo "screenshots in $OUT"
