#!/usr/bin/env python3
"""Adds agent-monitor's approval hook to ~/.cursor/hooks.json for shell and MCP calls; keeps every other hook."""
import json, os, shutil, sys, time

path = os.path.expanduser("~/.cursor/hooks.json")
hook = os.path.join(os.path.dirname(os.path.abspath(__file__)), "cursor-hook.sh")
cfg = {"version": 1, "hooks": {}}
if os.path.exists(path):
    with open(path) as f:
        cfg = json.load(f)
    shutil.copy(path, f"{path}.bak-{time.strftime('%Y%m%d-%H%M%S')}")
hooks = cfg.setdefault("hooks", {})
for event in ("beforeShellExecution", "beforeMCPExecution"):
    entries = hooks.setdefault(event, [])
    if not any(e.get("command") == hook for e in entries):
        entries.append({"command": hook, "timeout": 180})
with open(path, "w") as f:
    json.dump(cfg, f, indent=2)
    f.write("\n")
print(f"added {hook} to {path} for beforeShellExecution and beforeMCPExecution")
