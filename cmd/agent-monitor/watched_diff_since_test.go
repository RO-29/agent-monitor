package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestWatchedDiffSinceAndPaths(t *testing.T) {
	dir := t.TempDir()
	run := func(env []string, args ...string) {
		c := exec.Command("git", args...)
		c.Dir, c.Env = dir, append(os.Environ(), env...)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	id := []string{"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com"}
	at := func(ts int64) []string {
		d := "@" + strconv.FormatInt(ts, 10) + " +0000"
		return append(id, "GIT_AUTHOR_DATE="+d, "GIT_COMMITTER_DATE="+d)
	}
	now := time.Now().Unix()
	run(nil, "init", "-q")
	write("a.txt", "a1\n")
	write("b.txt", "b1\n")
	run(nil, "add", ".")
	run(at(now-7200), "commit", "-q", "-m", "before")
	start := (now - 3600) * 1000
	write("a.txt", "a2\n")
	write("b.txt", "b2\n")
	run(nil, "add", ".")
	run(at(now-60), "commit", "-q", "-m", "during")
	write("c.txt", "c\n")

	has := func(d *orchDiff, p string) bool {
		for _, f := range d.Files {
			if f.Path == p {
				return true
			}
		}
		return false
	}
	d, err := WatchedDiff(dir, "since:"+strconv.FormatInt(start, 10), "worktree", "")
	if err != nil || !has(d, "a.txt") || !has(d, "b.txt") {
		t.Fatalf("since: %v %+v", err, d)
	}
	d, err = WatchedDiff(dir, "since:"+strconv.FormatInt(start, 10), "worktree", filepath.Join(dir, "a.txt")+"\nc.txt\n/elsewhere/x.txt")
	if err != nil || !has(d, "a.txt") || has(d, "b.txt") {
		t.Fatalf("paths: %v %+v", err, d)
	}
	if d, err = WatchedDiff(dir, "since:1", "worktree", "/elsewhere/x.txt"); err != nil || len(d.Files) != 0 {
		t.Fatalf("outside: %v %+v", err, d)
	}
}
