#!/bin/bash
# Checks the iPhone Insert rules against testdata/insert_cases.json (builds a small macOS binary in a temp dir).
set -euo pipefail
cd "$(dirname "$0")/.."
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT
xcrun swiftc -O -o "$out/insert-check" ios/AgentMonitor/Insert.swift ios/InsertCheck/main.swift
"$out/insert-check" testdata/insert_cases.json
