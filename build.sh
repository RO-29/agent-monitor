#!/usr/bin/env bash
# Build the web app (React + Vite → web/dist) and then the Go binary that
# embeds it. `go build` alone still works when web/dist is present (it is
# committed), so this script is only needed after a frontend change.
set -euo pipefail
cd "$(dirname "$0")"
( cd web && npm install --no-audit --no-fund >/dev/null && npm run build )
go build -o ./agent-monitor .
echo "built ./agent-monitor ($(du -h ./agent-monitor | cut -f1))"
