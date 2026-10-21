#!/usr/bin/env bash
set -u

PAYLOAD=$(cat 2>/dev/null) || PAYLOAD='{}'

event=$(printf '%s' "$PAYLOAD" | /usr/bin/python3 -c 'import json,sys; print(json.load(sys.stdin).get("hook_event_name", ""))' 2>/dev/null) || event=''
session_id=$(printf '%s' "$PAYLOAD" | /usr/bin/python3 -c 'import json,sys; print(json.load(sys.stdin).get("session_id", ""))' 2>/dev/null) || session_id=''

forward_payload=$PAYLOAD
if [ "$event" = "PermissionRequest" ]; then
  forward_payload=$(printf '%s' "$PAYLOAD" | /usr/bin/python3 -c '
import json
import sys

p = json.load(sys.stdin)
tool_input = p.get("tool_input")
detail = ""
if isinstance(tool_input, dict):
    detail = tool_input.get("command") or tool_input.get("description") or ""
if not detail and tool_input not in (None, {}, []):
    detail = json.dumps(tool_input, separators=(",", ":"))
detail = " ".join(str(detail).split())
if len(detail) > 200:
    detail = detail[:197] + "..."
tool_name = str(p.get("tool_name") or "tool")
message = "Permission required"
if detail:
    message += ": " + detail
else:
    message += " for " + tool_name
print(json.dumps({
    "session_id": p.get("session_id", ""),
    "transcript_path": p.get("transcript_path"),
    "cwd": p.get("cwd", ""),
    "hook_event_name": "Notification",
    "notification_type": "permission_prompt",
    "message": message,
    "tool_name": tool_name,
}, separators=(",", ":")))
' 2>/dev/null) || forward_payload='{}'
fi

curl -fsS --connect-timeout 0.2 --max-time 0.5 \
  -H 'Content-Type: application/json' \
  --data-binary "$forward_payload" \
  http://127.0.0.1:7777/events/codex >/dev/null 2>&1 || true

case "$event" in
  SessionStart)
    if [ -n "${TMUX_PANE:-}" ] && [ -n "$session_id" ]; then
      body=$(printf '%s' "$PAYLOAD" | /usr/bin/python3 -c '
import json
import os
import sys

p = json.load(sys.stdin)
print(json.dumps({
    "tool": "codex",
    "sessionId": p.get("session_id", ""),
    "paneId": os.environ.get("TMUX_PANE", ""),
    "tmuxSocket": os.environ.get("TMUX", ""),
    "cwd": p.get("cwd", ""),
    "source": "hook",
}, separators=(",", ":")))
' 2>/dev/null) || body='{}'
      curl -fsS --connect-timeout 0.2 --max-time 0.5 \
        -H 'Content-Type: application/json' \
        --data-binary "$body" \
        http://127.0.0.1:7777/api/pane/register >/dev/null 2>&1 || true
    fi
    ;;
  SessionEnd)
    if [ -n "$session_id" ]; then
      curl -fsS --connect-timeout 0.2 --max-time 0.5 -X DELETE \
        "http://127.0.0.1:7777/api/pane/registration/codex:${session_id}" \
        >/dev/null 2>&1 || true
    fi
    ;;
esac

exit 0
