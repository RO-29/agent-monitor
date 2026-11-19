#!/usr/bin/env bash
# Counts private names in the added lines of the staged diff; exit 1 on any hit. Run it before every commit.
# Usage: bin/privacy-scan.sh   (prints the count; prints the matching lines when the count is not 0)
set -u
PATTERN="go""high""level|high""level|msg""sndr|lead""connector|g""hl|100\.68\.125\.93|brill""-symmetric"  # split so this file passes its own scan
added=$(git diff --cached | grep '^+')
count=$(printf '%s\n' "$added" | grep -ciwE "$PATTERN")
echo "$count"
if [ "$count" != 0 ]; then
  printf '%s\n' "$added" | grep -iwE "$PATTERN" >&2
  exit 1
fi
