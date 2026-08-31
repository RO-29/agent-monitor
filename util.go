package main

import (
	"os"
	"strings"
)

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return os.Getenv("HOME")
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func clipString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// decodePathFromDashEncoded reverses the lossy "/" -> "-" encoding used by
// Claude Code (~/.claude/projects/-Users-rohit-foo) and Cursor (~/.cursor/
// projects/Users-rohit-foo) to derive workspace dirs. The encoding loses
// information when path components contain literal hyphens (e.g. "web-
// app"), so we walk the filesystem and prefer the longest segment that
// exists as a real directory at each step.
//
// leadingSlash controls whether "Users-..." or "-Users-..." is the input
// shape: Claude prefixes a leading "-", Cursor doesn't.
func decodePathFromDashEncoded(dir string, hasLeadingDash bool) string {
	if hasLeadingDash {
		if !strings.HasPrefix(dir, "-") {
			return dir
		}
		dir = dir[1:]
	}
	parts := strings.Split(dir, "-")
	path := ""
	i := 0
	for i < len(parts) {
		best := i
		end := i + 6
		if end > len(parts) {
			end = len(parts)
		}
		for j := i; j < end; j++ {
			candidate := path + "/" + strings.Join(parts[i:j+1], "-")
			if info, err := os.Stat(candidate); err == nil && info.IsDir() {
				best = j
			}
		}
		path = path + "/" + strings.Join(parts[i:best+1], "-")
		i = best + 1
	}
	if path == "" {
		return "/" + strings.ReplaceAll(dir, "-", "/")
	}
	return path
}

// stateDir is where the daemon keeps its own files (sessions.db, panes.json,
// talks.log, settings.json, password). AGENT_MONITOR_HOME overrides it so a
// second daemon (dev build, tests) never touches the live instance's state.
func stateDir() string {
	if v := os.Getenv("AGENT_MONITOR_HOME"); v != "" {
		return v
	}
	return homeDir() + "/.agent-monitor"
}
