package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestSessionDiffStartsAtTheRecordedTree(t *testing.T) {
	t.Setenv("AGENT_MONITOR_HOME", t.TempDir())
	sessionBases.Lock()
	sessionBases.loaded, sessionBases.m = false, nil
	sessionBases.Unlock()
	dir := t.TempDir()
	run := func(args ...string) {
		c := exec.Command("git", args...)
		c.Dir = dir
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q")
	write("a.txt", "a1\n")
	write("b.txt", "b1\n")
	run("add", ".")
	run("commit", "-q", "-m", "base")
	write("a.txt", "edited before the session\n")
	recordSessionBase("claude:s1", dir)
	write("b.txt", "edited by the session\n")

	spec := "since:" + strconv.FormatInt(time.Now().UnixMilli(), 10)
	d, err := WatchedDiff(dir, spec+":claude:s1", "worktree", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Files) != 1 || d.Files[0].Path != "b.txt" {
		t.Fatalf("with a recorded start only the session's edit shows: %+v", d.Files)
	}
	if d, err = WatchedDiff(dir, spec+":claude:other", "worktree", ""); err != nil || len(d.Files) != 2 {
		t.Fatalf("no recorded start falls back to the last commit: %v %+v", err, d)
	}
	sessionBases.Lock()
	sessionBases.loaded, sessionBases.m = false, nil
	sessionBases.Unlock()
	if sessionBaseTree(t.Context(), "claude:s1", gitTop(dir)) == "" {
		t.Fatal("the start tree must survive a restart")
	}
}
