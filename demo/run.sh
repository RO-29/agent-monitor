#!/usr/bin/env bash
# demo/run.sh — set up an agent-monitor demo scene and fire one prompt.
#
# Style: organic, no captions or step overlays. Two side-by-side Claude panes
# in a tmux session named "amdemo", both auto-registered with agent-monitor.
# A single prompt typed into the left pane drives an end-to-end agent-to-agent
# round trip: list_agents -> talk_to_agent (wait_for_reply) -> reply_to_talk.
#
# How it finishes: left Claude prints the three commit lines that the right
# Claude fetched via `git log -3 --oneline` and returned through reply_to_talk.
# Total ~60-75s once the prompt is fired. Stop recording on that print.
#
# Pre-reqs:
#   - go build -o agent-monitor . (in repo root)
#   - daemon running in another terminal: ./agent-monitor
#   - agent-monitor install (so Claude has the SessionStart hook + MCP wiring)
#   - approve the agent-monitor MCP tools once, or launch claude with
#     --dangerously-skip-permissions for a clean first take.

set -euo pipefail

SESSION="amdemo"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/agent-monitor"

PROMPT='There is another Claude running in another pane of this same tmux session. Use list_agents to find them, then use talk_to_agent (wait_for_reply: true) to ask them to run `git log -3 --oneline` and report back. When the reply arrives, show me the three commits.'

if [[ ! -x "$BIN" ]]; then
    echo "build first: (cd \"$ROOT\" && go build -o agent-monitor .)" >&2
    exit 1
fi
if ! "$BIN" list >/dev/null 2>&1; then
    echo "daemon not running. start it in another terminal: \"$BIN\"" >&2
    exit 1
fi

tmux kill-session -t "$SESSION" 2>/dev/null || true
tmux new-session -d -s "$SESSION" -c "$ROOT" -x 220 -y 50
tmux send-keys -t "$SESSION:0.0" 'clear; claude' Enter
tmux split-window -h -t "$SESSION:0" -c "$ROOT"
tmux send-keys -t "$SESSION:0.1" 'clear; claude' Enter

printf "waiting for two registered claudes"
for _ in $(seq 1 30); do
    n=$("$BIN" list 2>/dev/null | awk '/^claude/{c++} END{print c+0}')
    if [[ "$n" -ge 2 ]]; then
        printf " — ready\n"
        break
    fi
    printf "."
    sleep 1
done

cat <<EOF

Session "$SESSION" is ready with two Claude panes.

  1. in your recorder window:  tmux attach -t $SESSION
  2. start screen recording
  3. come back here and press Enter to fire the prompt

EOF
read -r

tmux send-keys -t "$SESSION:0.0" -l "$PROMPT"
sleep 0.3
tmux send-keys -t "$SESSION:0.0" Enter

echo "fired. stop recording when the left pane prints the three commits."
