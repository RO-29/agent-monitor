package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixThread(t *testing.T) (*orchEnv, string, *OThread) {
	e := newOrchEnv(t)
	root := gitTestRepo(t)
	e.cwd = root
	th := e.create("hello")
	e.idle(th.ID)
	return e, root, th
}

func fixCP(t *testing.T, e *orchEnv, id string, n int) *orchCheckpoint {
	t.Helper()
	cp, _, err := e.m.checkpoint(id, n, "end", true)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	return cp
}

func trackBuildDir(t *testing.T, root string) {
	writeT(t, filepath.Join(root, ".gitignore"), "*.log\n/build\n")
	writeT(t, filepath.Join(root, "build", ".gitkeep"), "")
	gitT(t, root, "add", ".gitignore")
	gitT(t, root, "add", "-f", "build/.gitkeep")
	gitT(t, root, "commit", "-q", "-m", "keep build")
}

func wantGitFail(t *testing.T, err error, code int) *orchGitFail {
	t.Helper()
	var gf *orchGitFail
	if err == nil || !errors.As(err, &gf) || gf.code != code {
		t.Fatalf("want a %d git failure, got %v", code, err)
	}
	return gf
}

// H1: an ignored file where the target needs a directory is never replaced.
func TestRestoreKeepsIgnoredFileInTheWay(t *testing.T) {
	e, root, th := fixThread(t)
	trackBuildDir(t, root)
	cp := fixCP(t, e, th.ID, 5)
	os.RemoveAll(filepath.Join(root, "build"))
	writeT(t, filepath.Join(root, "build"), "precious ignored file\n")
	writeT(t, filepath.Join(root, "a.txt"), "changed\n")
	_, err := e.m.Restore(th.ID, cp.Ref)
	gf := wantGitFail(t, err, 409)
	if !strings.Contains(gf.output, "build") {
		t.Fatalf("conflict does not name build: %q", gf.output)
	}
	if readT(t, filepath.Join(root, "build")) != "precious ignored file\n" || readT(t, filepath.Join(root, "a.txt")) != "changed\n" {
		t.Fatal("restore changed files before it refused")
	}
}

// H1: an ignored symlink where the target needs a directory stays a symlink.
func TestRestoreKeepsIgnoredSymlink(t *testing.T) {
	e, root, th := fixThread(t)
	trackBuildDir(t, root)
	cp := fixCP(t, e, th.ID, 5)
	outside, _ := filepath.EvalSymlinks(t.TempDir())
	writeT(t, filepath.Join(outside, "o.txt"), "outside\n")
	os.RemoveAll(filepath.Join(root, "build"))
	if err := os.Symlink(outside, filepath.Join(root, "build")); err != nil {
		t.Fatal(err)
	}
	_, err := e.m.Restore(th.ID, cp.Ref)
	wantGitFail(t, err, 409)
	fi, lerr := os.Lstat(filepath.Join(root, "build"))
	if lerr != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink replaced: %v %v", fi, lerr)
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 1 {
		t.Fatalf("outside directory changed: %v", ents)
	}
}

// H2: a case-only rename is restored, not lost.
func TestRestoreCaseOnlyRename(t *testing.T) {
	e, root, th := fixThread(t)
	writeT(t, filepath.Join(root, "Readme.txt"), "content\n")
	cp := fixCP(t, e, th.ID, 5)
	os.Rename(filepath.Join(root, "Readme.txt"), filepath.Join(root, "tmpname"))
	os.Rename(filepath.Join(root, "tmpname"), filepath.Join(root, "readme.txt"))
	if _, err := e.m.Restore(th.ID, cp.Ref); err != nil {
		t.Fatal(err)
	}
	var names []string
	ents, _ := os.ReadDir(root)
	for _, x := range ents {
		names = append(names, x.Name())
	}
	if !strings.Contains(strings.Join(names, " "), "Readme.txt") || readT(t, filepath.Join(root, "Readme.txt")) != "content\n" {
		t.Fatalf("Readme.txt not restored: %v", names)
	}
}

// H2: a file that became a directory is restored as the file.
func TestRestoreDirectoryBackToFile(t *testing.T) {
	e, root, th := fixThread(t)
	writeT(t, filepath.Join(root, "conf"), "i am a file\n")
	cp := fixCP(t, e, th.ID, 5)
	os.Remove(filepath.Join(root, "conf"))
	writeT(t, filepath.Join(root, "conf", "inner.txt"), "now a dir\n")
	if _, err := e.m.Restore(th.ID, cp.Ref); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "conf")); err != nil || string(b) != "i am a file\n" {
		t.Fatalf("conf = %q %v", b, err)
	}
}

// H2: a directory that holds an ignored file is never removed to make room for a file.
func TestRestoreDirectoryWithIgnoredFileRefuses(t *testing.T) {
	e, root, th := fixThread(t)
	writeT(t, filepath.Join(root, "conf"), "i am a file\n")
	cp := fixCP(t, e, th.ID, 5)
	os.Remove(filepath.Join(root, "conf"))
	writeT(t, filepath.Join(root, "conf", "inner.txt"), "now a dir\n")
	writeT(t, filepath.Join(root, "conf", "debug.log"), "ignored\n")
	_, err := e.m.Restore(th.ID, cp.Ref)
	wantGitFail(t, err, 409)
	if readT(t, filepath.Join(root, "conf", "debug.log")) != "ignored\n" || readT(t, filepath.Join(root, "conf", "inner.txt")) != "now a dir\n" {
		t.Fatal("refused restore changed files")
	}
}

// H4: a truncated git listing aborts before any change.
func TestRestoreAbortsOnTruncatedListing(t *testing.T) {
	e, root, th := fixThread(t)
	cp := fixCP(t, e, th.ID, 5)
	for _, n := range []string{"x1.txt", "x2.txt", "x3.txt", "x4.txt"} {
		writeT(t, filepath.Join(root, n), "x\n")
	}
	old := orchRestoreReadCap
	orchRestoreReadCap = 64
	defer func() { orchRestoreReadCap = old }()
	if _, err := e.m.Restore(th.ID, cp.Ref); err == nil {
		t.Fatal("restore accepted a truncated listing")
	}
	for _, n := range []string{"x1.txt", "x2.txt", "x3.txt", "x4.txt"} {
		if !pathExists(filepath.Join(root, n)) {
			t.Fatalf("%s deleted after a truncated listing", n)
		}
	}
}

// H4: a failing check-ignore aborts; it never counts as "nothing ignored".
func TestRestoreAbortsWhenCheckIgnoreFails(t *testing.T) {
	e, root, th := fixThread(t)
	cp := fixCP(t, e, th.ID, 5)
	writeT(t, filepath.Join(root, "forced.log"), "tracked but ignored\n")
	gitT(t, root, "add", "-f", "forced.log")
	gitT(t, root, "commit", "-q", "-m", "force")
	real, err := execLookGit()
	if err != nil {
		t.Skip(err)
	}
	bin := t.TempDir()
	writeT(t, filepath.Join(bin, "git"), "#!/bin/sh\nfor a in \"$@\"; do [ \"$a\" = check-ignore ] && exit 128; done\nexec "+real+" \"$@\"\n")
	os.Chmod(filepath.Join(bin, "git"), 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := e.m.Restore(th.ID, cp.Ref); err == nil {
		t.Fatal("restore went on without check-ignore")
	}
	if !pathExists(filepath.Join(root, "forced.log")) {
		t.Fatal("ignored file deleted")
	}
}

// M1: a path that the target's ignore rules ignore is never deleted.
func TestRestoreUsesTargetIgnoreRules(t *testing.T) {
	e, root, th := fixThread(t)
	writeT(t, filepath.Join(root, ".gitignore"), "*.log\nnm/\n")
	writeT(t, filepath.Join(root, "nm", "dep.js"), "dependency\n")
	writeT(t, filepath.Join(root, "keep.log"), "log\n")
	cp := fixCP(t, e, th.ID, 5)
	writeT(t, filepath.Join(root, ".gitignore"), "# rewritten by the agent\n")
	res, err := e.m.Restore(th.ID, cp.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if !pathExists(filepath.Join(root, "nm", "dep.js")) || !pathExists(filepath.Join(root, "keep.log")) {
		t.Fatal("ignored files deleted")
	}
	if readT(t, filepath.Join(root, ".gitignore")) != "*.log\nnm/\n" || len(res.Kept) != 2 {
		t.Fatalf("restore result %+v", res)
	}
}

// M3: a restore or commit on a parent is refused while its shared child runs.
func TestGitWritesRefusedWhileSharedChildRuns(t *testing.T) {
	e, root, th := fixThread(t)
	cp := fixCP(t, e, th.ID, 5)
	child := e.delegate(th.ID, orchChildSpec{Prompt: "hang", Title: "kid"})
	e.hanging(child.ID)
	writeT(t, filepath.Join(root, "a.txt"), "child edit\n")
	if _, err := e.m.Restore(th.ID, cp.Ref); err == nil || !strings.Contains(err.Error(), child.ID) {
		t.Fatalf("restore with a running child: %v", err)
	}
	if _, err := e.m.Commit(th.ID, "x", nil); err == nil || !strings.Contains(err.Error(), child.ID) {
		t.Fatalf("commit with a running child: %v", err)
	}
	if readT(t, filepath.Join(root, "a.txt")) != "child edit\n" {
		t.Fatal("child's file rewritten")
	}
	_ = e.m.Interrupt(child.ID)
	e.idle(child.ID)
}

// M3: the lock is per repository, so two threads in one repo share it.
func TestRepoLockSharedAcrossThreads(t *testing.T) {
	root := gitTestRepo(t)
	g := newOrchGit()
	unlock := g.lockRepo(root)
	got := make(chan bool, 1)
	go func() {
		u := g.lockRepo(filepath.Join(root, "."))
		got <- true
		u()
	}()
	select {
	case <-got:
		t.Fatal("second lock on one repository did not wait")
	case <-time.After(150 * time.Millisecond):
	}
	unlock()
	<-got
}

// M4: an embedded repository without a commit is left out with a warning.
func TestCheckpointSkipsEmbeddedRepoWithoutCommit(t *testing.T) {
	e, root, th := fixThread(t)
	gitT(t, root, "init", "-q", filepath.Join(root, "sub"))
	writeT(t, filepath.Join(root, "sub", "x.txt"), "x\n")
	writeT(t, filepath.Join(root, "a.txt"), "edited\n")
	cp, _, err := e.m.checkpoint(th.ID, 7, "end", true)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if files := gitT(t, root, "ls-tree", "-r", "--name-only", cp.Sha); strings.Contains(files, "sub") || !strings.Contains(files, "a.txt") {
		t.Fatalf("tree %s", files)
	}
	if _, _, err := gitStatus(root); err != nil {
		t.Fatalf("status: %v", err)
	}
	found := false
	for _, it := range e.detail(th.ID).Items {
		if it.Kind == "other" && strings.Contains(it.Text, "sub") {
			found = true
		}
	}
	if !found {
		t.Fatal("no warning about the skipped repository")
	}
}

// M6: prune deletes the refs even when the worktree root is gone.
func TestPruneDeletesRefsWhenRootGone(t *testing.T) {
	isolateGit(t)
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	e := newOrchEnv(t)
	repo := gitTestRepo(t)
	th, err := e.m.CreateThread(orchCreate{Agent: "codex", Cwd: repo, Prompt: "hello", Workspace: "worktree"})
	if err != nil {
		t.Fatal(err)
	}
	e.idle(th.ID)
	if gitT(t, repo, "for-each-ref", "refs/am") == "" {
		t.Fatal("no refs to prune")
	}
	if _, err := e.m.RemoveWorktree(th.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Close(th.ID); err != nil {
		t.Fatal(err)
	}
	e.until("closed", func() bool { return e.thread(th.ID).Status == "closed" })
	n, err := e.m.PruneCheckpoints(time.Now().Add(31 * 24 * time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("prune %d %v", n, err)
	}
	if refs := gitT(t, repo, "for-each-ref", "--format=%(refname)", "refs/am/cp/"+th.ID, "refs/am/base/"+th.ID); refs != "" {
		t.Fatalf("refs left: %s", refs)
	}
	if len(e.m.store.checkpoints(th.ID)) != 0 {
		t.Fatal("rows left")
	}
}

func execLookGit() (string, error) { return exec.LookPath("git") }

// M2: a failed commit leaves the index as it was.
func TestCommitFailureLeavesIndex(t *testing.T) {
	e, root, th := fixThread(t)
	hook := filepath.Join(root, ".git", "hooks", "pre-commit")
	writeT(t, hook, "#!/bin/sh\nexit 1\n")
	os.Chmod(hook, 0o755)
	writeT(t, filepath.Join(root, "a.txt"), "edited\n")
	writeT(t, filepath.Join(root, "secret.txt"), "untracked\n")
	before, _ := os.ReadFile(filepath.Join(root, ".git", "index"))
	if _, err := e.m.Commit(th.ID, "msg", nil); err == nil {
		t.Fatal("commit passed a failing hook")
	}
	if after, _ := os.ReadFile(filepath.Join(root, ".git", "index")); string(after) != string(before) {
		t.Fatalf("index changed: staged %q", gitT(t, root, "diff", "--cached", "--name-only"))
	}
}

// M2: a commit with paths leaves out what the user staged before, and keeps it staged.
func TestCommitPathsLeaveOtherStagedFiles(t *testing.T) {
	e, root, th := fixThread(t)
	writeT(t, filepath.Join(root, "a.txt"), "edited\n")
	writeT(t, filepath.Join(root, "secret.txt"), "staged by the user\n")
	gitT(t, root, "add", "secret.txt")
	if _, err := e.m.Commit(th.ID, "only a", []string{"a.txt"}); err != nil {
		t.Fatal(err)
	}
	if files := gitT(t, root, "show", "--name-only", "--format=", "HEAD"); files != "a.txt" {
		t.Fatalf("commit holds %q", files)
	}
	if st := gitT(t, root, "status", "--porcelain"); st != "A  secret.txt" {
		t.Fatalf("status after commit %q", st)
	}
}

// M2: commit paths are literal, never globs or magic pathspecs.
func TestCommitPathsAreLiteral(t *testing.T) {
	e, root, th := fixThread(t)
	writeT(t, filepath.Join(root, "app", "[id]", "page.tsx"), "1\n")
	writeT(t, filepath.Join(root, "app", "i", "page.tsx"), "1\n")
	if _, err := e.m.Commit(th.ID, "one file", []string{"app/[id]/page.tsx"}); err != nil {
		t.Fatal(err)
	}
	if files := gitT(t, root, "show", "--name-only", "--format=", "HEAD"); files != "app/[id]/page.tsx" {
		t.Fatalf("commit holds %q", files)
	}
	if _, err := e.m.Commit(th.ID, "magic", []string{":/"}); err == nil {
		t.Fatal("magic pathspec accepted")
	}
	if files := gitT(t, root, "show", "--name-only", "--format=", "HEAD"); files != "app/[id]/page.tsx" {
		t.Fatalf("magic commit holds %q", files)
	}
}

// M5: a status read writes no object and leaves the index as it was.
func TestStatusWritesNothing(t *testing.T) {
	root := gitTestRepo(t)
	gitT(t, root, "gc", "-q", "--prune=now")
	writeT(t, filepath.Join(root, "big.bin"), strings.Repeat("x", 3<<20))
	writeT(t, filepath.Join(root, "a.txt"), "a\nmore\n")
	before := gitT(t, root, "count-objects", "-v")
	idx, _ := os.ReadFile(filepath.Join(root, ".git", "index"))
	info, _, err := gitStatus(root)
	if err != nil {
		t.Fatal(err)
	}
	writeT(t, filepath.Join(root, "big.bin"), strings.Repeat("y", 3<<20))
	if _, _, err := gitStatus(root); err != nil {
		t.Fatal(err)
	}
	if after := gitT(t, root, "count-objects", "-v"); after != before {
		t.Fatalf("objects written:\n%s\n%s", before, after)
	}
	if now, _ := os.ReadFile(filepath.Join(root, ".git", "index")); string(now) != string(idx) {
		t.Fatal("index rewritten")
	}
	st := map[string]orchGitFile{}
	for _, f := range info.Files {
		st[f.Path] = f
	}
	if st["a.txt"].Status != "modified" || st["a.txt"].Add != 1 || st["big.bin"].Status != "untracked" || info.Dirty != 2 {
		t.Fatalf("status %+v", info.Files)
	}
}

// M10: worktree removal refuses ignored files without force and checkpoints before it deletes.
func TestRemoveWorktreeCheckpointsAndReportsIgnored(t *testing.T) {
	e := newOrchEnv(t)
	root := gitTestRepo(t)
	th, err := e.m.CreateThread(orchCreate{Agent: "codex", Cwd: root, Prompt: "hello", Workspace: "worktree"})
	if err != nil {
		t.Fatal(err)
	}
	e.idle(th.ID)
	wt := e.thread(th.ID).Worktree.Path
	writeT(t, filepath.Join(wt, "debug.log"), "ignored\n")
	_, err = e.m.RemoveWorktree(th.ID, false)
	gf := wantGitFail(t, err, 409)
	if !strings.Contains(gf.output, "debug.log") || !pathExists(filepath.Join(wt, "debug.log")) {
		t.Fatalf("ignored file not reported or deleted: %q", gf.output)
	}
	writeT(t, filepath.Join(wt, "dirty.txt"), "uncommitted work\n")
	if _, err := e.m.RemoveWorktree(th.ID, true); err != nil {
		t.Fatal(err)
	}
	refs := gitT(t, root, "for-each-ref", "--format=%(refname)", cpPrefix(th.ID))
	var ref string
	for _, r := range strings.Fields(refs) {
		if strings.Contains(r, "-remove") {
			ref = r
		}
	}
	if ref == "" || gitT(t, root, "show", ref+":dirty.txt") != "uncommitted work" {
		t.Fatalf("no checkpoint with the work: %s", refs)
	}
	var note string
	for _, it := range e.detail(th.ID).Items {
		if it.Command == "git worktree remove" {
			note = it.Text
		}
	}
	if !strings.Contains(note, ref) {
		t.Fatalf("note does not name the checkpoint: %q", note)
	}
}

// L2: with no origin/HEAD, develop and init.defaultBranch are refused; a push writes no upstream config.
func TestPushGuardWithoutOriginHead(t *testing.T) {
	e, root, th := fixThread(t)
	bare := filepath.Join(t.TempDir(), "remote.git")
	gitT(t, root, "init", "-q", "--bare", bare)
	gitT(t, root, "remote", "add", "origin", bare)
	for _, b := range []string{"develop", "trunk", "prod"} {
		gitT(t, root, "checkout", "-q", "-b", b)
		if b == "prod" {
			gitT(t, root, "config", "init.defaultBranch", "prod")
		}
		if _, err := e.m.Push(th.ID, false); err == nil || !strings.Contains(err.Error(), "refusing") {
			t.Fatalf("push %s: %v", b, err)
		}
	}
	if refs := gitT(t, bare, "for-each-ref"); refs != "" {
		t.Fatalf("remote got %s", refs)
	}
	gitT(t, root, "checkout", "-q", "-b", "feature/y")
	gitT(t, root, "config", "remote.origin.push", "refs/heads/feature/y:refs/heads/main")
	if _, err := e.m.Push(th.ID, false); err != nil {
		t.Fatal(err)
	}
	if refs := gitT(t, bare, "for-each-ref", "--format=%(refname)"); refs != "refs/heads/feature/y" {
		t.Fatalf("remote refs %q", refs)
	}
	if out, err := exec.Command("git", "-C", root, "config", "--get", "branch.feature/y.merge").CombinedOutput(); err == nil {
		t.Fatalf("upstream written %q", out)
	}
}
