package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// knownCwdExtra holds folders tests add; production roots come from sessions and threads.
var knownCwdExtra struct {
	mu   sync.Mutex
	dirs []string
}

// knownCwdRoots lists the folders of watched sessions and driven threads (cwd, worktree, worktree origin).
func knownCwdRoots() []string {
	var out []string
	if store != nil {
		for _, s := range store.All() {
			out = append(out, s.Cwd)
		}
	}
	if orchMgr != nil {
		if ths, _, err := orchMgr.store.Threads(); err == nil {
			for _, th := range ths {
				out = append(out, th.Cwd)
				if th.Worktree != nil {
					out = append(out, th.Worktree.Path, th.Worktree.Origin)
				}
			}
		}
	}
	knownCwdExtra.mu.Lock()
	out = append(out, knownCwdExtra.dirs...)
	knownCwdExtra.mu.Unlock()
	return out
}

func resolveDir(p string) string {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

// knownCwd reports whether cwd equals or is inside a folder the daemon knows.
// A root that is / or the home folder matches only itself, never its subfolders.
func knownCwd(cwd string) bool {
	if cwd == "" || !filepath.IsAbs(cwd) {
		return false
	}
	c := resolveDir(cwd)
	broad := map[string]bool{"/": true}
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		broad[resolveDir(h)] = true
	}
	for _, root := range knownCwdRoots() {
		if root == "" || !filepath.IsAbs(root) {
			continue
		}
		r := resolveDir(root)
		if c == r {
			return true
		}
		if !broad[r] && strings.HasPrefix(c, strings.TrimSuffix(r, "/")+"/") {
			return true
		}
	}
	return false
}
