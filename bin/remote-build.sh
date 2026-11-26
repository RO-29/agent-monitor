#!/usr/bin/env bash
# Builds a commit on the build Mac and copies the results here; nothing is compiled on this Mac.
# Usage: bin/remote-build.sh [commit] [--test]   (commit: default origin/main; --test also runs every test there)
# AM_BUILD_HOST (user@host) and AM_BUILD_DIR come from ~/.config/agent-monitor/build.env.
set -euo pipefail
[[ -f "$HOME/.config/agent-monitor/build.env" ]] && source "$HOME/.config/agent-monitor/build.env"
HOST="${AM_BUILD_HOST:?set AM_BUILD_HOST=user@host in ~/.config/agent-monitor/build.env}"
DIR="${AM_BUILD_DIR:-am-build}"
REF="origin/main"; TEST=0
for a in "$@"; do [[ $a == --test ]] && TEST=1 || REF=$a; done
[[ "$REF" =~ ^[A-Za-z0-9._/-]+$ ]] || { echo "bad commit: $REF" >&2; exit 2; }

SHA=$(ssh "$HOST" "set -e; cd ~/$DIR; git fetch -q origin; git rev-parse --short '$REF'")
OUT="$HOME/agent-monitor-builds/$SHA"
echo "build $SHA on $HOST"
# bash, whatever the login shell is: zsh stops on an empty glob and set -e must hold.
ssh "$HOST" bash -s <<EOF
set -euo pipefail
export PATH=\$HOME/.cargo/bin:/opt/homebrew/bin:/usr/local/go/bin:/usr/bin:/bin:/usr/sbin:/sbin
cd ~/$DIR && git checkout -q --detach $SHA
( cd web && npm ci --no-audit --no-fund >/dev/null && npx tsc -b && npm run build >/dev/null )
go vet ./... && go build -o agent-monitor ./cmd/agent-monitor
( cd rust/am-helper && cargo build --release -q )
AM_APP_COPY=0 bash desk/build.sh >/dev/null
if [[ $TEST == 1 ]]; then
  # A failing run prints everything, not only the last line.
  ( cd rust/am-helper && cargo test 2>&1 ) > /tmp/am-cargo-test.out || { cat /tmp/am-cargo-test.out; echo "CARGO TEST FAILED"; exit 1; }
  tail -3 /tmp/am-cargo-test.out
  go test ./... > /tmp/am-go-test.out 2>&1 || { cat /tmp/am-go-test.out; echo "GO TEST FAILED"; exit 1; }
  grep -E "^(ok|FAIL)" /tmp/am-go-test.out
fi
rm -rf out && mkdir out && cp agent-monitor rust/am-helper/target/release/am-helper out/ && ditto agent-monitor.app out/agent-monitor.app
echo $SHA > out/COMMIT
EOF
mkdir -p "$OUT"
rsync -a --delete "$HOST:$DIR/out/" "$OUT/"
echo "copied to $OUT"
echo "install: bin/install-build.sh $SHA"
