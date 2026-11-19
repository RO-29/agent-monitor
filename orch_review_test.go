package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseReview(t *testing.T) {
	cases := []struct {
		name, text, summary string
		want                []OFinding
	}{
		{"fenced object", "Intro text.\n\n```json\n{\"summary\": \"S\", \"findings\": [{\"severity\": \"HIGH\", \"file\": \"a.go\", \"line\": 4, \"title\": \"T\", \"body\": \"B\"}]}\n```\n",
			"S", []OFinding{{Severity: "high", File: "a.go", Line: 4, Title: "T", Body: "B"}}},
		{"trailing commas and string line", "```json\n{\"findings\": [{\"severity\": \"nit\", \"path\": \"x.ts\", \"line\": \"12-14\", \"message\": \"M\",},],}\n```",
			"", []OFinding{{Severity: "info", File: "x.ts", Line: 12, Title: "M"}}},
		{"summary from prose", "All good overall.\n```json\n{\"findings\": []}\n```", "All good overall.", []OFinding{}},
		{"last json fence wins", "```json\n{\"findings\": [{\"title\": \"old\"}]}\n```\nthen\n```json\n{\"summary\": \"new\", \"findings\": [{\"title\": \"N\", \"severity\": \"blocker\"}]}\n```",
			"new", []OFinding{{Severity: "critical", Title: "N"}}},
		{"bare array, unlabeled fence", "```\n[{\"severity\": \"low\", \"file\": \"m.go:9\", \"description\": \"first line\\nmore\"}]\n```",
			"", []OFinding{{Severity: "low", File: "m.go", Line: 9, Title: "first line", Body: "first line\nmore"}}},
		{"no fence", "Summary here. {\"summary\": \"X\", \"findings\": [{\"title\": \"Y\", \"severity\": \"warning\"}]} trailing",
			"X", []OFinding{{Severity: "medium", Title: "Y"}}},
		{"go fence ignored, headings used", "Lead line.\n\n```go\nfunc x() {}\n```\n\n## [High] Nil map write in `store.go:42`\nThe map is nil.\n\n### Minor: naming\nRename it.\n",
			"Lead line.\n\n```go\nfunc x() {}\n```", []OFinding{{Severity: "high", File: "store.go", Line: 42, Title: "Nil map write in `store.go:42`", Body: "The map is nil."},
				{Severity: "low", Title: "naming", Body: "Rename it."}}},
		{"summary heading", "## Summary\nFine.\n## Findings\n## Missing test\nAdd one for x.go:3\n", "Fine.",
			[]OFinding{{Severity: "medium", Title: "Missing test", File: "x.go", Line: 3, Body: "Add one for x.go:3"}}},
		{"nothing", "No problems found.", "No problems found.", []OFinding{}},
	}
	for _, c := range cases {
		s, f := parseReview(c.text)
		if s != c.summary || !reflect.DeepEqual(f, c.want) {
			t.Errorf("%s:\n summary %q want %q\n findings %+v\n want     %+v", c.name, s, c.summary, f, c.want)
		}
	}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestOrchReviewChild(t *testing.T) {
	e := newOrchEnv(t)
	e.cwd = gitRepo(t)
	th := e.create("hello")
	e.idle(th.ID)
	srv := e.server()
	if code, out := postJSON(t, srv.URL+"/api/orch/threads/"+th.ID+"/review", `{}`); code != 409 || !strings.Contains(out["error"].(string), "nothing to review") {
		t.Fatalf("empty diff: %d %v", code, out)
	}
	os.WriteFile(filepath.Join(e.cwd, "b.txt"), []byte("one\ntwo\nthree\n"), 0o644)
	code, out := postJSON(t, srv.URL+"/api/orch/threads/"+th.ID+"/review", `{"focus":"loops"}`)
	if code != 201 {
		t.Fatalf("review: %d %v", code, out)
	}
	childID := out["threadId"].(string)
	child := e.thread(childID)
	if child.ParentID != th.ID || child.ReviewOf != th.ID || child.Workspace != "read-only" || child.Sandbox != "read-only" || child.notifies() || child.Agent != "codex" {
		t.Fatalf("child %+v", child)
	}
	e.until("review done", func() bool { r := e.thread(th.ID).Review; return r != nil && r.State == "done" })
	rv := e.thread(th.ID).Review
	want := []OFinding{{Severity: "high", File: "b.txt", Line: 3, Title: "Off by one", Body: "Loop ends early."}}
	if rv.ChildID != childID || rv.Summary != "One real bug." || !reflect.DeepEqual(rv.Findings, want) || rv.Focus != "loops" || rv.EndedAt == 0 {
		t.Fatalf("review %+v", rv)
	}
	log := e.fakeLog()
	if !strings.Contains(log, "b.txt (untracked") && !strings.Contains(log, "b.txt (added") {
		t.Fatalf("prompt lacks the file list: %s", log)
	}
	if !strings.Contains(log, `+three`) || !strings.Contains(log, "Review focus: loops") || !strings.Contains(log, "```json") {
		t.Fatalf("prompt lacks diff or format: %s", log)
	}
	var item *OItem
	for _, it := range e.detail(th.ID).Items {
		if it.Kind == "review" {
			item = &it
		}
	}
	if item == nil || item.ChildID != childID || item.Status != "completed" || !strings.Contains(item.Text, "1 findings (1 high)") {
		t.Fatalf("review item %+v", item)
	}
	if n := notifyRuns(e.runs(th.ID)); n != 0 {
		t.Fatalf("parent got %d subagent notices", n)
	}
	if code, _ := postJSON(t, srv.URL+"/api/orch/threads/"+childID+"/review", `{}`); code != 400 {
		t.Fatalf("review of a review: %d", code)
	}
}

func TestOrchReviewFallbackHeadings(t *testing.T) {
	t.Setenv("ORCH_FAKE_REVIEW", "Overall OK.\n\n## [Low] Typo in b.txt:1\nSpelling.")
	e := newOrchEnv(t)
	e.cwd = gitRepo(t)
	th := e.create("hello")
	e.idle(th.ID)
	os.WriteFile(filepath.Join(e.cwd, "a.txt"), []byte("changed\n"), 0o644)
	if _, err := e.m.StartReview(th.ID, orchReviewIn{}); err != nil {
		t.Fatal(err)
	}
	e.until("review done", func() bool { r := e.thread(th.ID).Review; return r != nil && r.State == "done" })
	rv := e.thread(th.ID).Review
	if len(rv.Findings) != 1 || rv.Findings[0].Severity != "low" || rv.Findings[0].File != "b.txt" || rv.Summary != "Overall OK." {
		t.Fatalf("review %+v", rv)
	}
}

// mergeSetup makes a repo, a worktree thread and one commit on its branch that adds feature.txt.
func mergeSetup(t *testing.T) (*orchEnv, string, *OThread) {
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	e := newOrchEnv(t)
	repo := gitRepo(t)
	th, err := e.m.CreateThread(orchCreate{Agent: "codex", Cwd: repo, Prompt: "hello", Workspace: "worktree"})
	if err != nil {
		t.Fatal(err)
	}
	e.idle(th.ID)
	th = e.thread(th.ID)
	if th.Worktree.OriginBranch != "main" {
		t.Fatalf("origin branch %+v", th.Worktree)
	}
	os.WriteFile(filepath.Join(th.Worktree.Path, "feature.txt"), []byte("feature\n"), 0o644)
	gitIn(t, th.Worktree.Path, "add", "feature.txt")
	gitIn(t, th.Worktree.Path, "commit", "-q", "-m", "add feature")
	return e, repo, th
}

func TestOrchMergeSquashIntoCleanCheckout(t *testing.T) {
	e, repo, th := mergeSetup(t)
	srv := e.server()
	code, out := postJSON(t, srv.URL+"/api/orch/threads/"+th.ID+"/merge", `{}`)
	if code != 200 || out["into"] != "main" || out["strategy"] != "squash" || out["applied"] != "checkout" {
		t.Fatalf("merge: %d %v", code, out)
	}
	if gitIn(t, repo, "rev-parse", "HEAD") != out["sha"] || gitIn(t, repo, "rev-list", "--count", "HEAD") != "2" {
		t.Fatalf("main not fast-forwarded to %v", out["sha"])
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "feature.txt")); string(b) != "feature\n" || gitIn(t, repo, "status", "--porcelain") != "" {
		t.Fatalf("checkout not updated: %q", b)
	}
	if parents := strings.Fields(gitIn(t, repo, "rev-list", "--parents", "-n1", "HEAD")); len(parents) != 2 {
		t.Fatalf("squash made a merge commit: %v", parents)
	}
	if n := strings.Count(gitIn(t, repo, "worktree", "list"), "\n"); n != 1 {
		t.Fatalf("temporary worktree left: %s", gitIn(t, repo, "worktree", "list"))
	}
	if m := e.thread(th.ID).Merge; m == nil || m.Into != "main" || m.Sha != out["sha"] {
		t.Fatalf("thread merge %+v", m)
	}
	if code, out := postJSON(t, srv.URL+"/api/orch/threads/"+th.ID+"/merge", `{"strategy":"squash"}`); code != 409 {
		t.Fatalf("second squash: %d %v", code, out)
	}
	var note *OItem
	for _, it := range e.detail(th.ID).Items {
		if it.Command == "git merge" {
			note = &it
		}
	}
	if note == nil || note.Status != "completed" {
		t.Fatalf("merge item %+v", note)
	}
}

func TestOrchMergeCommitIntoUncheckedBranch(t *testing.T) {
	e, repo, th := mergeSetup(t)
	gitIn(t, repo, "branch", "release")
	gitIn(t, repo, "checkout", "-q", "-b", "other")
	res, err := e.m.Merge(th.ID, orchMergeIn{Into: "release", Strategy: "merge", Message: "Merge feature"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Applied != "ref" || gitIn(t, repo, "rev-parse", "release") != res.Sha {
		t.Fatalf("release %+v", res)
	}
	if parents := strings.Fields(gitIn(t, repo, "rev-list", "--parents", "-n1", "release")); len(parents) != 3 {
		t.Fatalf("not a merge commit: %v", parents)
	}
	if gitIn(t, repo, "log", "-1", "--format=%s", "release") != "Merge feature" || pathExists(filepath.Join(repo, "feature.txt")) {
		t.Fatal("message or checkout wrong")
	}
	if _, err := e.m.Merge(th.ID, orchMergeIn{Into: "nope"}); err == nil || !strings.Contains(err.Error(), "unknown branch") {
		t.Fatalf("unknown branch: %v", err)
	}
	if _, err := e.m.Merge(th.ID, orchMergeIn{Strategy: "rebase"}); err == nil {
		t.Fatal("bad strategy accepted")
	}
}

func snapshotDir(t *testing.T, dir string) map[string]string {
	out := map[string]string{}
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			if info != nil && info.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		b, _ := os.ReadFile(p)
		out[p] = string(b)
		return nil
	})
	return out
}

func TestOrchMergeConflictLeavesTargetUnchanged(t *testing.T) {
	e, repo, th := mergeSetup(t)
	os.WriteFile(filepath.Join(th.Worktree.Path, "a.txt"), []byte("from thread\n"), 0o644)
	gitIn(t, th.Worktree.Path, "commit", "-q", "-am", "thread edit")
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("from main\n"), 0o644)
	gitIn(t, repo, "commit", "-q", "-am", "main edit")
	head := gitIn(t, repo, "rev-parse", "HEAD")
	before := snapshotDir(t, repo)
	index, _ := os.ReadFile(filepath.Join(repo, ".git", "index"))
	srv := e.server()
	for _, strategy := range []string{"squash", "merge"} {
		code, out := postJSON(t, srv.URL+"/api/orch/threads/"+th.ID+"/merge", `{"strategy":"`+strategy+`"}`)
		files, _ := out["files"].([]any)
		if code != 409 || out["error"] != "merge conflicts" || len(files) != 1 || files[0] != "a.txt" {
			t.Fatalf("%s conflict: %d %v", strategy, code, out)
		}
	}
	if gitIn(t, repo, "rev-parse", "HEAD") != head || !reflect.DeepEqual(snapshotDir(t, repo), before) {
		t.Fatal("target changed")
	}
	if idx, _ := os.ReadFile(filepath.Join(repo, ".git", "index")); string(idx) != string(index) {
		t.Fatal("target index changed")
	}
	if gitIn(t, repo, "status", "--porcelain") != "" || strings.Count(gitIn(t, repo, "worktree", "list"), "\n") != 1 {
		t.Fatalf("leftovers: %s", gitIn(t, repo, "worktree", "list"))
	}
	if ents, _ := os.ReadDir(filepath.Join(e.dir, "merge")); len(ents) != 0 {
		t.Fatalf("temp dirs left: %v", ents)
	}
}

func TestOrchMergeRefusesDirty(t *testing.T) {
	e, repo, th := mergeSetup(t)
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("user edit\n"), 0o644)
	head := gitIn(t, repo, "rev-parse", "HEAD")
	_, err := e.m.Merge(th.ID, orchMergeIn{})
	var gf *orchGitFail
	if err == nil || !errorsAs(err, &gf) || gf.code != 409 || !strings.Contains(gf.output, "a.txt") {
		t.Fatalf("dirty target: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, "a.txt")); string(b) != "user edit\n" || gitIn(t, repo, "rev-parse", "HEAD") != head {
		t.Fatal("dirty target touched")
	}
	os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o644)
	os.WriteFile(filepath.Join(th.Worktree.Path, "wip.txt"), []byte("x"), 0o644)
	if _, err := e.m.Merge(th.ID, orchMergeIn{}); err == nil || !errorsAs(err, &gf) || gf.code != 409 || !strings.Contains(gf.msg, "worktree has uncommitted") {
		t.Fatalf("dirty worktree: %v", err)
	}
	plain := e.create("hello")
	e.idle(plain.ID)
	if _, err := e.m.Merge(plain.ID, orchMergeIn{}); err == nil || !strings.Contains(err.Error(), "no worktree") {
		t.Fatalf("no worktree: %v", err)
	}
}

func errorsAs(err error, target **orchGitFail) bool {
	gf, ok := err.(*orchGitFail)
	*target = gf
	return ok
}
