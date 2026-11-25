package main

import (
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// hunksOf splits a file patch into its hunks, the way the web diff view sends them.
func hunksOf(patch string) []string {
	var out []string
	for _, part := range regexp.MustCompile(`(?m)^@@ `).Split(patch, -1)[1:] {
		out = append(out, "@@ "+part)
	}
	return out
}

func TestRevertHunkRemovesOnlyThatBlock(t *testing.T) {
	root := gitTestRepo(t)
	var lines []string
	for i := 1; i <= 30; i++ {
		lines = append(lines, "line "+strings.Repeat("x", i%5)+string(rune('a'+i%26)))
	}
	orig := strings.Join(lines, "\n") + "\n"
	writeT(t, filepath.Join(root, "f.txt"), orig)
	gitT(t, root, "add", "f.txt")
	gitT(t, root, "commit", "-q", "-m", "f")

	changed := strings.Replace(strings.Replace(orig, lines[1], "TOP CHANGE", 1), lines[27], "BOTTOM CHANGE", 1)
	writeT(t, filepath.Join(root, "f.txt"), changed)

	d, err := WatchedDiff(root, "head", "worktree", "f.txt")
	if err != nil || len(d.Files) != 1 {
		t.Fatalf("diff: %v %+v", err, d)
	}
	f := d.Files[0]
	if f.Hash == "" {
		t.Fatal("a modified file diffed to the work tree carries a hash")
	}
	hs := hunksOf(f.Patch)
	if len(hs) != 2 {
		t.Fatalf("want 2 hunks, got %d:\n%s", len(hs), f.Patch)
	}

	var ce *orchHTTPError
	if err := revertHunk(root, "f.txt", hs[0], "stale"); !errors.As(err, &ce) || ce.code != 409 {
		t.Fatalf("a stale hash must be refused: %v", err)
	}
	if err := revertHunk(root, "../f.txt", hs[0], f.Hash); err == nil {
		t.Fatal("a path outside the repo must be refused")
	}
	if err := revertHunk(root, "f.txt", "rm -rf /\n", f.Hash); err == nil {
		t.Fatal("text that is not a hunk must be refused")
	}

	if err := revertHunk(root, "f.txt", hs[0], f.Hash); err != nil {
		t.Fatal(err)
	}
	got := readT(t, filepath.Join(root, "f.txt"))
	if strings.Contains(got, "TOP CHANGE") || !strings.Contains(got, lines[1]) || !strings.Contains(got, "BOTTOM CHANGE") {
		t.Fatalf("only the first block goes back:\n%s", got)
	}
	if err := revertHunk(root, "f.txt", hs[1], f.Hash); err == nil {
		t.Fatal("the old hash no longer matches after the first revert")
	}
	if err := revertHunk(root, "f.txt", hs[1], worktreeHash(root, "f.txt")); err != nil {
		t.Fatal(err)
	}
	if got := readT(t, filepath.Join(root, "f.txt")); got != orig {
		t.Fatalf("both blocks reverted gives the commit back:\n%s", got)
	}
	if err := revertHunk(root, "f.txt", hs[1], worktreeHash(root, "f.txt")); err == nil {
		t.Fatal("a block that is already gone cannot be reverted again")
	}
}

func TestWorktreeHashOnlyForWorktreeDiffs(t *testing.T) {
	files := []orchDiffFile{{Path: "a.txt", Status: "modified"}, {Path: "gone.txt", Status: "deleted"}}
	addWorktreeHashes(t.TempDir(), "head", files)
	if files[0].Hash != "" {
		t.Fatal("a diff that does not end at the work tree has no hash")
	}
}
