#!/bin/sh
set -u

[ -n "${TMUX-}" ] || exit 1
pid=${1-${PPID-}}
[ -n "$pid" ] || exit 1

if [ -n "${TMUX_TITLE_VERIFIED_PANE-}" ] \
  && tmux display-message -p -t "$TMUX_TITLE_VERIFIED_PANE" '#{pane_id}' >/dev/null 2>&1; then
  printf '%s\n' "$TMUX_TITLE_VERIFIED_PANE"
  exit 0
fi

registry=${AGENT_MONITOR_PANE_REGISTRY-"$HOME/.agent-monitor/panes.json"}
if [ -n "${CODEX_THREAD_ID-}" ] && [ -r "$registry" ]; then
  pane=$(/usr/bin/python3 -c '
import json, sys
with open(sys.argv[1]) as handle:
    data = json.load(handle)
matches = [r.get("paneId", "") for r in data.get("registrations", [])
           if r.get("tool") == "codex" and r.get("sessionId") == sys.argv[2]]
print(matches[0] if len(matches) == 1 else "")
' "$registry" "$CODEX_THREAD_ID" 2>/dev/null) || pane=''
  if [ -n "$pane" ] && tmux display-message -p -t "$pane" '#{pane_id}' >/dev/null 2>&1; then
    printf '%s\n' "$pane"
    exit 0
  fi
fi

panes=$(tmux list-panes -a -F '#{pane_id} #{pane_pid}' 2>/dev/null) || exit 1
count=0
while [ "$count" -lt 64 ]; do
  pane=$(printf '%s\n' "$panes" | awk -v pid="$pid" '$2 == pid { print $1; exit }')
  if [ -n "$pane" ]; then
    printf '%s\n' "$pane"
    exit 0
  fi
  parent=$(ps -o ppid= -p "$pid" 2>/dev/null | tr -d '[:space:]')
  case "$parent" in
    ''|0|1|"$pid") exit 1 ;;
  esac
  pid=$parent
  count=$((count + 1))
done
exit 1
