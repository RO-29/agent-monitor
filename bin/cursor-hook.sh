#!/usr/bin/env bash
# Cursor beforeShellExecution / beforeMCPExecution hook: agent-monitor can approve or deny; anything else asks in Cursor.
set -u
PAYLOAD=$(cat 2>/dev/null) || PAYLOAD='{}'
MONITOR_URL=${AGENT_MONITOR_URL:-http://127.0.0.1:${AGENT_MONITOR_PORT:-7777}}
resp=$(curl -s --max-time 170 -X POST -H 'Content-Type: application/json' \
  --data-binary "$PAYLOAD" -w '\n%{http_code}' "$MONITOR_URL/api/approvals/hook/cursor" 2>/dev/null) || resp=''
status=${resp##*$'\n'}
body=${resp%$'\n'*}
if [ "$status" = "200" ] && [ -n "$body" ]; then
  # Print only a well-formed Cursor permission; Cursor blocks the command on invalid JSON.
  out=$(printf '%s' "$body" | /usr/bin/python3 -c '
import json, sys
d = json.load(sys.stdin)
assert d["permission"] in ("allow", "deny", "ask")
print(json.dumps(d, separators=(",", ":")))
' 2>/dev/null) && { printf '%s\n' "$out"; exit 0; }
fi
printf '%s\n' '{"permission":"ask"}'
