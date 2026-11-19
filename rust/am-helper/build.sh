#!/usr/bin/env bash
# Build am-helper (release) and, given a directory, copy it next to the agent-monitor binary there.
# Usage: rust/am-helper/build.sh [target-dir]   or   rust/am-helper/build.sh --test (cargo and Go parity tests)
set -euo pipefail
cd "$(dirname "$0")"
export PATH="$HOME/.cargo/bin:$PATH"
cargo build --release
bin=target/release/am-helper
echo "built $(pwd)/$bin ($(du -h "$bin" | cut -f1))"
if [[ ${1:-} == --test ]]; then
  cargo test --release
  # Parity tests fail instead of skipping when the helper is missing.
  (cd ../.. && AM_HELPER_REQUIRED=1 go test ./...)
  exit 0
fi
if [[ $# -ge 1 ]]; then
  dest=$1
  [[ -d $dest ]] || { echo "not a directory: $dest" >&2; exit 1; }
  # Copy to a temp name and rename, so running helpers keep their old inode.
  cp "$bin" "$dest/.am-helper.new"
  mv -f "$dest/.am-helper.new" "$dest/am-helper"
  # Tells bin/*-hook.sh that this am-helper has hook mode.
  touch "$dest/am-helper.hooks"
  echo "installed $dest/am-helper"
fi
