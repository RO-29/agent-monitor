#!/usr/bin/env bash
set -euo pipefail

# Buffer stdin once: we forward it to the legacy /events/claude endpoint AND
# parse a few fields out for the new /api/pane/register call below.
PAYLOAD="$(cat)"

# Always forward to the legacy event sink (records SessionStart, tool calls,
# notifications, etc).
curl -fsS \
  -H 'Content-Type: application/json' \
  --data-binary "$PAYLOAD" \
  http://127.0.0.1:7777/events/claude >/dev/null 2>&1 || true

# Pane registration / deregistration. Without jq we use a tiny grep+sed
# shim — the payload is line-friendly JSON.
event=$(printf '%s' "$PAYLOAD" | grep -o '"hook_event_name"[[:space:]]*:[[:space:]]*"[^"]*"' | head -1 | sed 's/.*:[[:space:]]*"//; s/"$//')
session_id=$(printf '%s' "$PAYLOAD" | grep -o '"session_id"[[:space:]]*:[[:space:]]*"[^"]*"' | head -1 | sed 's/.*:[[:space:]]*"//; s/"$//')

case "$event" in
  SessionStart)
    if [ -n "${TMUX_PANE:-}" ] && [ -n "$session_id" ]; then
      cwd=$(printf '%s' "$PAYLOAD" | grep -o '"cwd"[[:space:]]*:[[:space:]]*"[^"]*"' | head -1 | sed 's/.*:[[:space:]]*"//; s/"$//')
      # NOTE: we deliberately do NOT record a pid here. $PPID is the
      # bash subprocess that ran this hook script, which exits as soon
      # as the script returns. Recording it would make the daemon's
      # pid-aliveness GC drop the claude registration ~15s later, even
      # though claude itself is alive in the tmux pane. Without a pid,
      # GC falls back to "is the tmux pane still there?" which is the
      # correct liveness signal for hook-registered agents.
      body=$(printf '{"tool":"claude","sessionId":"%s","paneId":"%s","tmuxSocket":"%s","cwd":"%s","source":"hook"}' \
        "$session_id" "$TMUX_PANE" "${TMUX:-}" "$cwd")
      curl -fsS \
        -H 'Content-Type: application/json' \
        --data-binary "$body" \
        http://127.0.0.1:7777/api/pane/register >/dev/null 2>&1 || true
    fi
    ;;
  SessionEnd)
    # Clean up immediately when claude exits — don't wait for the periodic
    # GC to notice the dead pid. agentId for claude is "claude:<sessionId>".
    if [ -n "$session_id" ]; then
      curl -fsS -X DELETE \
        "http://127.0.0.1:7777/api/pane/registration/claude:${session_id}" \
        >/dev/null 2>&1 || true
    fi
    ;;
esac
