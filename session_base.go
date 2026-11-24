package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// A session's base is its repository's work tree when it started, so its diff leaves out earlier edits.
type sessionBase struct {
	Root string `json:"root"`
	Tree string `json:"tree"`
	At   int64  `json:"at"`
}

const sessionBaseMax = 400

var sessionBases = struct {
	sync.Mutex
	m      map[string]sessionBase
	loaded bool
}{}

func sessionBasePath() string { return filepath.Join(stateDir(), "session-bases.json") }

func loadSessionBasesLocked() {
	if sessionBases.loaded {
		return
	}
	sessionBases.loaded, sessionBases.m = true, map[string]sessionBase{}
	if b, err := os.ReadFile(sessionBasePath()); err == nil {
		_ = json.Unmarshal(b, &sessionBases.m)
	}
}

// recordSessionBase snapshots cwd's repository once per session; a later call keeps the first snapshot.
func recordSessionBase(id, cwd string) {
	if id == "" || !filepath.IsAbs(cwd) {
		return
	}
	sessionBases.Lock()
	loadSessionBasesLocked()
	_, have := sessionBases.m[id]
	sessionBases.Unlock()
	if have {
		return
	}
	root := gitTop(cwd)
	if root == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tree, err := snapshotTree(ctx, root)
	if err != nil || tree == "" {
		return
	}
	sessionBases.Lock()
	defer sessionBases.Unlock()
	if _, have := sessionBases.m[id]; have {
		return
	}
	sessionBases.m[id] = sessionBase{Root: root, Tree: tree, At: time.Now().UnixMilli()}
	if len(sessionBases.m) > sessionBaseMax {
		ids := make([]string, 0, len(sessionBases.m))
		for k := range sessionBases.m {
			ids = append(ids, k)
		}
		sort.Slice(ids, func(i, j int) bool { return sessionBases.m[ids[i]].At < sessionBases.m[ids[j]].At })
		for _, k := range ids[:len(ids)-sessionBaseMax] {
			delete(sessionBases.m, k)
		}
	}
	_ = writeFileAtomic(sessionBasePath(), sessionBases.m)
}

// sessionBaseTree is the recorded start tree of a session in root, if git still has it.
func sessionBaseTree(ctx context.Context, id, root string) string {
	sessionBases.Lock()
	loadSessionBasesLocked()
	b, ok := sessionBases.m[id]
	sessionBases.Unlock()
	if !ok || b.Root != root {
		return ""
	}
	if _, err := git(ctx, root, "cat-file", "-e", b.Tree+"^{tree}"); err != nil {
		return "" // pruned by git gc
	}
	return b.Tree
}
