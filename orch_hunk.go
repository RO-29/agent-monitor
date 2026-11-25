package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// worktreeHash names a work tree file's content, so a later hunk revert can refuse a file that changed since.
func worktreeHash(root, rel string) string {
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		return ""
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:8])
}

// addWorktreeHashes sets Hash on modified text files when the diff ends at the work tree.
func addWorktreeHashes(root, to string, files []orchDiffFile) {
	if to != "worktree" {
		return
	}
	for i := range files {
		if files[i].Status == "modified" && !files[i].Binary {
			files[i].Hash = worktreeHash(root, files[i].Path)
		}
	}
}

// revertHunk removes one hunk of a head-to-worktree style diff from the work tree file, and nothing else.
func revertHunk(root, rel, hunk, hash string) error {
	if rel == "" || filepath.IsAbs(rel) {
		return orchBadRequest("path must be relative to the repository")
	}
	if r, ok := relInside(root, filepath.Join(root, rel)); !ok || r == "." {
		return orchBadRequest("path is outside the repository")
	}
	if !strings.HasPrefix(hunk, "@@ -") {
		return orchBadRequest("hunk must start with its @@ header")
	}
	if !strings.HasSuffix(hunk, "\n") {
		hunk += "\n"
	}
	for i, ln := range strings.Split(strings.TrimSuffix(hunk, "\n"), "\n") {
		if i > 0 && ln != "" && !strings.ContainsRune(" +-\\", rune(ln[0])) {
			return orchBadRequest("hunk has a line that is not part of a diff")
		}
	}
	if hash == "" || worktreeHash(root, rel) != hash {
		return orchConflict("the file changed since the diff was read; reload the diff")
	}
	p := filepath.ToSlash(rel)
	patch := []byte("diff --git a/" + p + " b/" + p + "\n--- a/" + p + "\n+++ b/" + p + "\n" + hunk)
	ctx, cancel := context.WithTimeout(context.Background(), orchGitTimeout)
	defer cancel()
	for _, args := range [][]string{{"apply", "-R", "--check", "-"}, {"apply", "-R", "-"}} {
		if _, _, err := (gitRun{ctx: ctx, dir: root, env: literalEnv, stdin: patch}).run(args...); err != nil {
			return orchConflict("git could not revert this block: " + firstLine(strings.TrimSpace(err.Error())))
		}
	}
	return nil
}

func (m *OrchManager) hunkRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/orch/git/revert-hunk", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Thread string `json:"thread"`
			Cwd    string `json:"cwd"`
			Path   string `json:"path"`
			Hunk   string `json:"hunk"`
			Hash   string `json:"hash"`
		}
		if !decodeOptional(w, r, &in) {
			return
		}
		cwd := in.Cwd
		if in.Thread != "" {
			th, err := m.store.Thread(in.Thread)
			if err != nil {
				orchWriteErr(w, err)
				return
			}
			cwd = th.Cwd
		} else if !filepath.IsAbs(cwd) || !knownCwd(cwd) {
			orchWriteErr(w, &orchHTTPError{http.StatusForbidden, "cwd is not the folder of a watched session or a driven thread"})
			return
		}
		root := gitTop(cwd)
		if root == "" {
			orchWriteErr(w, orchConflict("not a git repository"))
			return
		}
		if err := revertHunk(root, in.Path, in.Hunk, in.Hash); err != nil {
			gitWriteErr(w, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "hash": worktreeHash(root, in.Path)})
	})
}
