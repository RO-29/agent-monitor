#!/bin/bash
# Build Mac, nightly: when Claude Code has a new version, capture its trust and Bash permission prompts and test them.
# Usage: bin/prompt-check.sh [--force]. It notifies the result once per version through the local daemon.
set -uo pipefail
export PATH="$HOME/.local/bin:/opt/homebrew/bin:/usr/local/go/bin:/usr/bin:/bin:/usr/sbin:/sbin"
REPO=$(cd "$(dirname "$0")/.." && pwd)
STATE="$HOME/.agent-monitor/prompt-check"
mkdir -p "$STATE"
log() { echo "$(date '+%F %T') $*"; }

CLAUDE=$(command -v claude) || { log "no claude on PATH"; exit 0; }
VER=$("$CLAUDE" --version 2>/dev/null | awk '{print $1}')
[[ "$VER" =~ ^[0-9][0-9.]*$ ]] || { log "no claude version"; exit 0; }
if [[ "${1:-}" != "--force" && "$(cat "$STATE/version" 2>/dev/null)" == "$VER" ]]; then
  log "claude $VER already checked"
  exit 0
fi

OUT="$STATE/$VER"
rm -rf "$OUT" && mkdir -p "$OUT"
WORK=$(mktemp -d /tmp/am-prompt-check.XXXXXX)
echo "scratch folder for the agent-monitor prompt check" > "$WORK/note.txt"
S="am-prompt-check"
cleanup() { tmux kill-session -t "$S" 2>/dev/null; rm -rf "$WORK"; }
trap cleanup EXIT
tmux kill-session -t "$S" 2>/dev/null
# No user settings, MCP servers or hooks: the prompts must look as a fresh install shows them.
tmux new-session -d -s "$S" -x 120 -y 40 -c "$WORK" \
  "$CLAUDE --setting-sources project --strict-mcp-config --permission-mode default --model haiku"

wait_for() { # pattern, seconds
  local i=0
  while (( i < $2 )); do
    tmux capture-pane -p -t "$S" > "$OUT/.screen" 2>/dev/null && grep -Eq "$1" "$OUT/.screen" && return 0
    sleep 1; i=$((i + 1))
  done
  return 1
}

fail="" nologin=false
if wait_for "trust this folder|Do you trust" 40; then
  cp "$OUT/.screen" "$OUT/trust.txt"
  if grep -Eq "❯.*Yes" "$OUT/trust.txt"; then tmux send-keys -t "$S" Enter; else tmux send-keys -t "$S" Down Enter; fi
else
  fail="the folder trust prompt did not show in 40 s"
fi

if [[ -z "$fail" ]]; then
  sleep 4
  tmux send-keys -t "$S" -l "Use the Bash tool to run exactly this command and nothing else: touch made-by-check.txt"
  sleep 1
  tmux send-keys -t "$S" Enter
  if wait_for "Do you want to" 150; then
    sleep 1
    tmux capture-pane -p -t "$S" > "$OUT/perm-bash.txt"
    tmux send-keys -t "$S" Escape
  else
    cp "$OUT/.screen" "$OUT/last-screen.txt"
    fail="the Bash permission prompt did not show in 150 s"
    grep -Eq "Not logged in|Login expired|Please run /login" "$OUT/.screen" && nologin=true
  fi
fi

if [[ -z "$fail" ]]; then
  if (cd "$REPO" && AM_LIVE_SCREENS="$OUT" go test -count=1 -run 'TestLiveClaudeScreens|TestClaudePromptScreens' . > "$OUT/test.log" 2>&1); then
    log "claude $VER: prompts pass"
  else
    fail=$(grep -m1 -E "prompt_screens_test.go:[0-9]+:" "$OUT/test.log" | sed -E 's/^[[:space:]]*prompt_screens_test.go:[0-9]+: //')
    fail="${fail:-go test failed, read $OUT/test.log}"
  fi
fi
[[ -n "$fail" ]] && log "claude $VER: $fail"

# Logged out is not a screen change: say so once per version and try again the next night.
if $nologin; then
  [[ -f "$STATE/nologin-$VER" ]] && exit 0
  body="{\"prompts\":\"$VER\",\"nologin\":true}"
  curl -sf -X POST -H 'Content-Type: application/json' "http://127.0.0.1:${AGENT_MONITOR_PORT:-7777}/api/notify/install" -d "$body" >/dev/null && touch "$STATE/nologin-$VER"
  exit 0
fi
body=$(VER="$VER" FAIL="$fail" /usr/bin/python3 -c 'import json,os; print(json.dumps({"prompts": os.environ["VER"], "failed": os.environ["FAIL"]}))')
if curl -sf -X POST -H 'Content-Type: application/json' "http://127.0.0.1:${AGENT_MONITOR_PORT:-7777}/api/notify/install" -d "$body" >/dev/null; then
  echo "$VER" > "$STATE/version"
else
  log "could not reach the daemon; the next run tries again"
fi
