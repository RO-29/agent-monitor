package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// H3: a merge never overwrites an ignored file in the target checkout.
func TestMergeKeepsIgnoredFileInTargetCheckout(t *testing.T) {
	e, repo, th := mergeSetup(t)
	writeT(t, filepath.Join(repo, ".gitignore"), "*.log\n")
	gitIn(t, repo, "add", ".gitignore")
	gitIn(t, repo, "commit", "-q", "-m", "ignore logs")
	gitIn(t, th.Worktree.Path, "merge", "-q", "main")
	writeT(t, filepath.Join(repo, "local.log"), "user's precious ignored file\n")
	writeT(t, filepath.Join(th.Worktree.Path, "local.log"), "from the branch\n")
	gitIn(t, th.Worktree.Path, "add", "-f", "local.log")
	gitIn(t, th.Worktree.Path, "commit", "-q", "-m", "track local.log")
	head := gitIn(t, repo, "rev-parse", "HEAD")
	_, err := e.m.Merge(th.ID, orchMergeIn{})
	gf := wantGitFail(t, err, 409)
	if !strings.Contains(gf.output, "local.log") {
		t.Fatalf("conflict output %q", gf.output)
	}
	if readT(t, filepath.Join(repo, "local.log")) != "user's precious ignored file\n" || gitIn(t, repo, "rev-parse", "HEAD") != head {
		t.Fatal("target changed")
	}
}

// M7: a merge runs no hook and adds no worktree.
func TestMergeRunsNoHooks(t *testing.T) {
	e, repo, th := mergeSetup(t)
	marker := filepath.Join(t.TempDir(), "hooks-ran")
	for _, h := range []string{"pre-commit", "commit-msg", "post-commit", "post-checkout", "post-merge", "reference-transaction"} {
		p := filepath.Join(repo, ".git", "hooks", h)
		writeT(t, p, "#!/bin/sh\necho "+h+" >> "+marker+"\nexit 1\n")
		os.Chmod(p, 0o755)
	}
	res, err := e.m.Merge(th.ID, orchMergeIn{})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(marker); len(b) > 0 {
		t.Fatalf("hooks ran: %s", b)
	}
	if res.Applied != "checkout" || readT(t, filepath.Join(repo, "feature.txt")) != "feature\n" {
		t.Fatalf("merge %+v", res)
	}
	if n := strings.Count(gitIn(t, repo, "worktree", "list"), "\n"); n != 1 {
		t.Fatalf("worktrees %s", gitIn(t, repo, "worktree", "list"))
	}
}

// M7: a branch in a rebase in another worktree is in use.
func TestMergeRefusesBranchUnderRebase(t *testing.T) {
	root := gitTestRepo(t)
	wt := filepath.Join(filepath.Dir(root), "w2-"+filepath.Base(root))
	t.Cleanup(func() { os.RemoveAll(wt) })
	gitT(t, root, "worktree", "add", "-q", "-b", "tgt", wt, "HEAD")
	writeT(t, filepath.Join(wt, "a.txt"), "tgt side\n")
	gitT(t, wt, "commit", "-qam", "tgt")
	writeT(t, filepath.Join(root, "a.txt"), "main side\n")
	gitT(t, root, "commit", "-qam", "main")
	_ = exec.Command("git", "-C", wt, "rebase", "main").Run()
	if _, err := checkoutsOf(context.Background(), root, "tgt"); err == nil || !strings.Contains(err.Error(), "rebased") {
		t.Fatalf("rebase not seen: %v", err)
	}
}

func TestGitVersionParse(t *testing.T) {
	maj, mi, v := gitVersion(context.Background())
	if maj < 2 || v == "" {
		t.Fatalf("git version %d.%d %q", maj, mi, v)
	}
	t.Setenv("PATH", t.TempDir())
	if maj, _, _ := gitVersion(context.Background()); maj != 0 {
		t.Fatal("missing git parsed as a version")
	}
}
