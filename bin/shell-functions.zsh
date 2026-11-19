# claude/codex under the agent-monitor session host; `--tmux` or AGENT_MONITOR_TMUX=1 uses tmux instead.
# Source it after older definitions: `source ~/agent-monitor/bin/shell-functions.zsh` (see README).

: ${AGENT_MONITOR_BIN:=$HOME/agent-monitor/agent-monitor}

# am_launch_agent <agent> <want-tmux> [args...]
am_launch_agent() {
  local agent=$1 want_tmux=$2 am=$AGENT_MONITOR_BIN sess cmd
  shift 2
  if [[ ! -x $am ]]; then
    command $agent "$@"; return
  fi
  # am-helper beside the Go binary hosts the agent; without it the agent runs unmonitored.
  if [[ ! -x ${am:h}/am-helper ]]; then
    print -u2 "agent-monitor: ${am:h}/am-helper is missing; build it with rust/am-helper/build.sh"
    command $agent "$@"; return
  fi
  am=${am:h}/am-helper
  if [[ $want_tmux != 1 || -n $TMUX ]]; then
    "$am" run $agent "$@"; return
  fi
  sess="$agent-${PWD:t}-${RANDOM}"
  cmd="export TMUX_PANE=\$(/bin/sh \$HOME/bin/tmux-pane-id.sh \$\$); exec $(printf '%q ' "$am" run $agent "$@")"
  tmux new-session -d -s "$sess" "$cmd" && tmux attach-session -t "$sess"
}

claude() {
  local want_tmux=${AGENT_MONITOR_TMUX:-0}
  if [[ $1 == --tmux ]]; then want_tmux=1; shift; fi
  if [[ ! -t 1 || ! -t 0 || " $* " == *" -p "* || " $* " == *" --print "* ]]; then
    command claude "$@"; return
  fi
  am_launch_agent claude "$want_tmux" "$@"
}

codex() {
  local want_tmux=${AGENT_MONITOR_TMUX:-0}
  if [[ $1 == --tmux ]]; then want_tmux=1; shift; fi
  case "$1" in
    exec|login|logout|mcp|completion|--help|-h|--version|-V) command codex "$@"; return ;;
  esac
  if [[ ! -t 1 || ! -t 0 ]]; then
    command codex "$@"; return
  fi
  am_launch_agent codex "$want_tmux" "$@"
}
