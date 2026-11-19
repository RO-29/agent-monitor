#!/usr/bin/env bash
# Start each am-helper mode and print its RSS (KB) and the binary size.
# Usage: rust/am-helper/measure.sh  (builds into a temp dir; the daemon is not needed or touched)
set -euo pipefail
cd "$(dirname "$0")"
export PATH="$HOME/.cargo/bin:$PATH"
tmp=$(mktemp -d /tmp/amm.XXXX)
pids=()
cleanup() { for p in "${pids[@]}"; do kill -9 "$p" 2>/dev/null || true; done; rm -rf "$tmp"; }
trap cleanup EXIT
cargo build --release -q
mkdir -p "$tmp/rs" && cp target/release/am-helper "$tmp/rs/am-helper"
export AGENT_MONITOR_HOME=$tmp/home AGENT_MONITOR_PORT=1
rss() { ps -o rss= -p "$1" | tr -d ' '; }

mcp() {
  local fifo=$tmp/fifo.$1
  mkfifo "$fifo"
  "$2" mcp-perm-server <"$fifo" >/dev/null 2>&1 &
  local p=$!; pids+=("$p")
  exec 9>"$fifo"
  printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"initialize"}' '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' >&9
  sleep 1.5
  echo "mcp-perm-server  $1  rss_kb=$(rss "$p")"
  exec 9>&-
}

host() {
  # script(1) gives the host a terminal; the agent is sleep.
  script -q /dev/null "$2" run sleep 60 </dev/null >/dev/null 2>&1 &
  local s=$!; pids+=("$s")
  sleep 2
  local h l
  h=$(pgrep -P "$s" | head -1); l=$(pgrep -P "$h" | head -1)
  pids+=("$h" "$l")
  echo "run (host)       $1  rss_kb=$(rss "$h")   pid=$h"
  echo "__pty-leader     $1  rss_kb=$(rss "$l")   pid=$l"
}

mcp rust "$tmp/rs/am-helper"
host rust "$tmp/rs/am-helper"
echo "binary $(stat -f %z "$tmp/rs/am-helper") bytes"
