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

// ensureToolEnv fixes launchd/detached starts: PATH has no tmux, and with no
// UTF-8 locale tmux prints tabs in -F output as "_", which breaks parsing.
func ensureToolEnv() {
	if os.Getenv("LC_ALL") == "" && os.Getenv("LC_CTYPE") == "" && os.Getenv("LANG") == "" {
		_ = os.Setenv("LC_CTYPE", "UTF-8")
	}
	path := os.Getenv("PATH")
	have := map[string]bool{}
	for _, d := range strings.Split(path, ":") {
		have[d] = true
	}
	for _, d := range []string{"/opt/homebrew/bin", "/usr/local/bin"} {
		if !have[d] && pathExists(d) {
			path += ":" + d
		}
	}
	_ = os.Setenv("PATH", path)
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// clipString cuts s to at most n bytes without splitting a UTF-8 sequence.
func clipString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	// Clone so the clip does not pin the whole source string in memory.
	return strings.Clone(s[:n])
}

// decodePathFromDashEncoded reverses Claude's and Cursor's "/" to "-" project encoding.
// Hyphens are ambiguous, so it prefers the longest segment that exists on disk.
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

// stateDir holds the daemon's files; AGENT_MONITOR_HOME moves it so a second daemon never touches the live one.
func stateDir() string {
	if v := os.Getenv("AGENT_MONITOR_HOME"); v != "" {
		return v
	}
	return homeDir() + "/.agent-monitor"
}
