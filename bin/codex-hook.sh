#!/usr/bin/env bash
# am-helper beside the agent-monitor binary runs this hook natively; AGENT_MONITOR_RUST_HELPERS=0 keeps the shell code below.
if [ "${AGENT_MONITOR_RUST_HELPERS:-}" != 0 ]; then
  case ${AGENT_MONITOR_URL:-http://} in
    http://*)
      am_helper=${BASH_SOURCE[0]%/*}/../am-helper
      # The .hooks marker is written by rust/am-helper/build.sh, so an older am-helper without hook mode is never used.
      if [ -f "$am_helper" ] && [ -x "$am_helper" ] && [ -f "$am_helper.hooks" ]; then
        shopt -s execfail
        exec "$am_helper" hook codex
      fi
      ;;
  esac
fi
set -u

PAYLOAD=$(cat 2>/dev/null) || PAYLOAD='{}'
MONITOR_URL=${AGENT_MONITOR_URL:-http://127.0.0.1:${AGENT_MONITOR_PORT:-7777}}

event=$(printf '%s' "$PAYLOAD" | /usr/bin/python3 -c 'import json,sys; print(json.load(sys.stdin).get("hook_event_name", ""))' 2>/dev/null) || event=''
session_id=$(printf '%s' "$PAYLOAD" | /usr/bin/python3 -c 'import json,sys; print(json.load(sys.stdin).get("session_id", ""))' 2>/dev/null) || session_id=''

resolve_pane() {
  local pane match=''
  pane=$(CODEX_THREAD_ID="$session_id" /bin/sh /Users/rohit/bin/tmux-pane-id.sh "$PPID") || pane=''
  if [ -n "$pane" ]; then
    printf '%s\n' "$pane"
    return 0
  fi
  while IFS= read -r pane; do
    [ -n "$pane" ] || continue
    if tmux capture-pane -p -S -100 -t "$pane" 2>/dev/null | grep -Fq -- "$session_id"; then
      [ -z "$match" ] || return 1
      match=$pane
    fi
  done < <(tmux list-panes -a -F '#{pane_id}' 2>/dev/null)
  [ -n "$match" ] || return 1
  printf '%s\n' "$match"
}

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

# Under `agent-monitor run`, name the session host so the daemon links this session to it.
events_url="$MONITOR_URL/events/codex"
case "${AM_HOST_ID:-}" in
  ''|*[!A-Za-z0-9_-]*) ;;
  *) events_url="${events_url}?host=${AM_HOST_ID}&ppid=${PPID}" ;;
esac

curl -fsS --connect-timeout 0.2 --max-time 0.5 \
  -H 'Content-Type: application/json' \
  --data-binary "$forward_payload" \
  "$events_url" >/dev/null 2>&1 || true

case "$event" in
  SessionStart|UserPromptSubmit|Stop|PermissionRequest)
    pane_id=$(resolve_pane) || pane_id=''
    if [ -n "$pane_id" ] && [ -n "$session_id" ]; then
      body=$(printf '%s' "$PAYLOAD" | /usr/bin/python3 -c '
import json
import os
import sys

p = json.load(sys.stdin)
print(json.dumps({
    "tool": "codex",
    "sessionId": p.get("session_id", ""),
    "paneId": sys.argv[1],
    "tmuxSocket": os.environ.get("TMUX", ""),
    "cwd": p.get("cwd", ""),
    "source": "hook",
}, separators=(",", ":")))
' "$pane_id" 2>/dev/null) || body='{}'
      curl -fsS --connect-timeout 0.2 --max-time 0.5 \
        -H 'Content-Type: application/json' \
        --data-binary "$body" \
        "$MONITOR_URL/api/pane/register" >/dev/null 2>&1 || true
    fi
    ;;
  SessionEnd)
    if [ -n "$session_id" ]; then
      curl -fsS --connect-timeout 0.2 --max-time 0.5 -X DELETE \
        "$MONITOR_URL/api/pane/registration/codex:${session_id}" \
        >/dev/null 2>&1 || true
    fi
    ;;
esac

# Ask the daemon for a decision; print it only on HTTP 200 so any failure falls back to the terminal prompt.
if [ "$event" = "PermissionRequest" ]; then
  settings_file="${AGENT_MONITOR_HOME:-$HOME/.agent-monitor}/approval-settings.json"
  hold=$(/usr/bin/python3 -c 'import json,sys; print(int(json.load(open(sys.argv[1])).get("holdSeconds") or 120))' "$settings_file" 2>/dev/null) || hold=120
  case "$hold" in ''|*[!0-9]*) hold=120 ;; esac
  resp=$(curl -sS --connect-timeout 0.5 --max-time $((hold + 5)) \
    -H 'Content-Type: application/json' \
    --data-binary "$PAYLOAD" \
    -w '\n%{http_code}' \
    "$MONITOR_URL/api/approvals/hook/codex" 2>/dev/null) || resp=''
  status=${resp##*$'\n'}
  body=${resp%$'\n'*}
  if [ "$status" = "200" ] && [ -n "$body" ]; then
    # Print only a well-formed Codex decision; anything else (an old daemon's HTML) prints nothing.
    printf '%s' "$body" | /usr/bin/python3 -c '
import json, sys
d = json.load(sys.stdin)
h = d["hookSpecificOutput"]
assert h["hookEventName"] == "PermissionRequest" and h["decision"]["behavior"] in ("allow", "deny")
print(json.dumps(d, separators=(",", ":")))
' 2>/dev/null || true
  fi
fi

exit 0
