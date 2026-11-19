#!/usr/bin/env bash
# am-helper beside the agent-monitor binary runs this hook natively; AGENT_MONITOR_RUST_HELPERS=0 keeps the shell code below.
if [ "${AGENT_MONITOR_RUST_HELPERS:-}" != 0 ]; then
  case ${AGENT_MONITOR_URL:-http://} in
    http://*)
      am_helper=${BASH_SOURCE[0]%/*}/../am-helper
      # The .hooks marker is written by rust/am-helper/build.sh, so an older am-helper without hook mode is never used.
      if [ -f "$am_helper" ] && [ -x "$am_helper" ] && [ -f "$am_helper.hooks" ]; then
        shopt -s execfail
        exec "$am_helper" hook claude
      fi
      ;;
  esac
fi
set -euo pipefail

# Buffer stdin once: we forward it to the legacy /events/claude endpoint AND
# parse a few fields out for the new /api/pane/register call below.
PAYLOAD="$(cat)"
# A driven agent gets the URL of the daemon that drives it.
MONITOR_URL=${AGENT_MONITOR_URL:-http://127.0.0.1:${AGENT_MONITOR_PORT:-7777}}

# Under `agent-monitor run`, name the session host so the daemon links this session to it.
EVENTS_URL="${MONITOR_URL}/events/claude"
case "${AM_HOST_ID:-}" in
  ''|*[!A-Za-z0-9_-]*) ;;
  *) EVENTS_URL="${EVENTS_URL}?host=${AM_HOST_ID}&ppid=${PPID}" ;;
esac

# Always forward to the legacy event sink (records SessionStart, tool calls,
# notifications, etc).
curl -fsS \
  -H 'Content-Type: application/json' \
  --data-binary "$PAYLOAD" \
  "$EVENTS_URL" >/dev/null 2>&1 || true

# Pane registration / deregistration. Without jq we use a tiny grep+sed
# shim — the payload is line-friendly JSON.
event=$(printf '%s' "$PAYLOAD" | grep -o '"hook_event_name"[[:space:]]*:[[:space:]]*"[^"]*"' | head -1 | sed 's/.*:[[:space:]]*"//; s/"$//')
session_id=$(printf '%s' "$PAYLOAD" | grep -o '"session_id"[[:space:]]*:[[:space:]]*"[^"]*"' | head -1 | sed 's/.*:[[:space:]]*"//; s/"$//')

case "$event" in
  SessionStart)
    if [ -n "${TMUX_PANE:-}" ] && [ -n "$session_id" ]; then
      cwd=$(printf '%s' "$PAYLOAD" | grep -o '"cwd"[[:space:]]*:[[:space:]]*"[^"]*"' | head -1 | sed 's/.*:[[:space:]]*"//; s/"$//')
      # No pid: $PPID is this short-lived hook shell, so GC falls back to the pane still existing.
      body=$(printf '{"tool":"claude","sessionId":"%s","paneId":"%s","tmuxSocket":"%s","cwd":"%s","source":"hook"}' \
        "$session_id" "$TMUX_PANE" "${TMUX:-}" "$cwd")
      curl -fsS \
        -H 'Content-Type: application/json' \
        --data-binary "$body" \
        "${MONITOR_URL}/api/pane/register" >/dev/null 2>&1 || true
    fi
    ;;
  SessionEnd)
    # Clean up immediately when claude exits — don't wait for the periodic
    # GC to notice the dead pid. agentId for claude is "claude:<sessionId>".
    if [ -n "$session_id" ]; then
      curl -fsS -X DELETE \
        "${MONITOR_URL}/api/pane/registration/claude:${session_id}" \
        >/dev/null 2>&1 || true
    fi
    ;;
esac
