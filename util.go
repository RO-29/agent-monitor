package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return os.Getenv("HOME")
}

// isRepoRoot reports whether dir looks like an agent-monitor checkout. We
// check for the Claude hook script specifically — it's the one non-compiled
// asset installHooks needs from the checkout, and a stronger signal than
// something generic like go.mod.
func isRepoRoot(dir string) bool {
	return pathExists(filepath.Join(dir, "bin", "claude-hook.sh"))
}

// resolveRepoDir implements checkout detection against an explicit
// candidate list, kept separate from repoDir so it can be table-tested
// without depending on the test binary's own on-disk location.
func resolveRepoDir(envOverride string, candidates []string) (string, error) {
	if envOverride != "" {
		if isRepoRoot(envOverride) {
			return envOverride, nil
		}
		return "", fmt.Errorf("AGENT_MONITOR_REPO=%s doesn't look like an agent-monitor checkout (missing bin/claude-hook.sh)", envOverride)
	}
	for _, c := range candidates {
		if isRepoRoot(c) {
			return c, nil
		}
	}
	return "", fmt.Errorf("couldn't locate an agent-monitor checkout (checked %s) — cd into the checkout or set AGENT_MONITOR_REPO", strings.Join(dedupe(candidates), ", "))
}

// repoDir returns the agent-monitor checkout directory. The running binary's
// own directory is only a guess — it's wrong for `go run` (temp build dir),
// a binary copied to /usr/local/bin, or one built to <repo>/bin — so every
// candidate is validated via isRepoRoot before being trusted. Set
// AGENT_MONITOR_REPO to skip detection entirely.
func repoDir() (string, error) {
	var candidates []string
	if exe, err := os.Executable(); err == nil {
		resolved, err := filepath.EvalSymlinks(exe)
		if err != nil {
			resolved = exe
		}
		dir := filepath.Dir(resolved)
		// dir covers a binary built at the repo root; its parent covers one
		// built to <repo>/bin.
		candidates = append(candidates, dir, filepath.Dir(dir))
	}
	// Under `go run` the exe candidates above are a temp build dir and match
	// nothing, so without the working directory we'd fall through to the HOME
	// guess and silently wire up a stale ~/agent-monitor — the exact bug this
	// detection exists to prevent. Walk up so it also works from a subdir.
	if cwd, err := os.Getwd(); err == nil {
		for d := cwd; ; {
			candidates = append(candidates, d)
			parent := filepath.Dir(d)
			if parent == d {
				break
			}
			d = parent
		}
	}
	candidates = append(candidates, filepath.Join(homeDir(), "agent-monitor"))
	return resolveRepoDir(os.Getenv("AGENT_MONITOR_REPO"), dedupe(candidates))
}

func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// isGoBuildDir matches Go's own build directory names — "go-build" for the
// cache root, "go-buildNNN" for a `go run` temp tree — without matching a
// user directory that merely starts with those letters (go-build-tools).
func isGoBuildDir(seg string) bool {
	rest, ok := strings.CutPrefix(seg, "go-build")
	if !ok {
		return false
	}
	return strings.TrimLeft(rest, "0123456789") == ""
}

// isEphemeralExe reports whether p is a `go run` build artifact. Go builds to
// $TMPDIR/go-buildNNN/b001/exe/<pkg> and deletes that tree when the process
// exits, so the path is live during install and gone by the time an agent
// tries to spawn the MCP server.
func isEphemeralExe(p string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(p), "/") {
		if isGoBuildDir(seg) {
			return true
		}
	}
	tmp := os.TempDir()
	if resolved, err := filepath.EvalSymlinks(tmp); err == nil {
		tmp = resolved
	}
	rel, err := filepath.Rel(tmp, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolveExeBin picks the binary path to bake into the MCP configs, given the
// running binary's own resolved path. Normally that's the binary itself — it's
// guaranteed to exist and run whether it lives in the checkout or in
// /usr/local/bin. The exception is `go run`: writing its temp path would leave
// every later Claude/Codex session pointing at a deleted file, and a failed MCP
// spawn is silent, so we fall back to a real build inside the checkout and
// refuse rather than persist a path we know will rot.
func resolveExeBin(exe string, repo func() (string, error)) (string, error) {
	if !isEphemeralExe(exe) {
		return exe, nil
	}
	dir, err := repo()
	if err != nil {
		return "", fmt.Errorf("running from a temporary `go run` build (%s) that's deleted on exit, and %w", exe, err)
	}
	bin := filepath.Join(dir, "agent-monitor")
	if !pathExists(bin) {
		return "", fmt.Errorf("running from a temporary `go run` build (%s) that's deleted on exit — build a real binary first: go build -o %s .", exe, bin)
	}
	return bin, nil
}

// exeBin returns the path MCP configs should invoke agent-monitor by.
func exeBin() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("couldn't determine agent-monitor's own binary path: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return resolveExeBin(exe, repoDir)
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
