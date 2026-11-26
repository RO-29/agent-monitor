package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// isolateGit hides the user's global and system git config from the test and the code under test.
func isolateGit(t *testing.T) {
	t.Helper()
	empty := filepath.Join(t.TempDir(), "gitconfig")
	_ = os.WriteFile(empty, nil, 0o644)
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeT(t *testing.T, path, body string) {
	t.Helper()
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readT(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// gitTestRepo makes a repo with a local identity, one commit on main, and a *.log ignore rule.
func gitTestRepo(t *testing.T) string {
	t.Helper()
	isolateGit(t)
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	gitT(t, dir, "init", "-q", "-b", "main")
	gitT(t, dir, "config", "user.name", "Test User")
	gitT(t, dir, "config", "user.email", "test@example.com")
	gitT(t, dir, "config", "commit.gpgsign", "false")
	writeT(t, filepath.Join(dir, "a.txt"), "a\n")
	writeT(t, filepath.Join(dir, "gone.txt"), "will be deleted\n")
	writeT(t, filepath.Join(dir, ".gitignore"), "*.log\n")
	gitT(t, dir, "add", ".")
	gitT(t, dir, "commit", "-q", "-m", "init")
	return dir
}

func (e *orchEnv) refExists(root, ref string) bool {
	return exec.Command("git", "-C", root, "rev-parse", "--verify", "-q", ref).Run() == nil
}

type repoState struct{ index, head, branches, stash string }

func snapshotRepo(t *testing.T, root string) repoState {
	t.Helper()
	idx, _ := os.ReadFile(filepath.Join(root, ".git", "index"))
	return repoState{string(idx), gitT(t, root, "rev-parse", "HEAD"), gitT(t, root, "for-each-ref", "refs/heads"),
		strings.TrimSpace(func() string { o, _ := exec.Command("git", "-C", root, "stash", "list").Output(); return string(o) }())}
}

// changedRun runs a hanging turn and applies edit while it runs, so the end checkpoint sees it.
func (e *orchEnv) changedRun(id string, edit func()) {
	e.t.Helper()
	if _, err := e.m.Send(id, "hang"); err != nil {
		e.t.Fatal(err)
	}
	e.hanging(id)
	edit()
	if err := e.m.Interrupt(id); err != nil {
		e.t.Fatal(err)
	}
	e.idle(id)
}

func editRun1(t *testing.T, root string) func() {
	return func() {
		writeT(t, filepath.Join(root, "a.txt"), "a\nchanged\n")
		writeT(t, filepath.Join(root, "new", "b.txt"), "new file\n")
		if err := os.Remove(filepath.Join(root, "gone.txt")); err != nil {
			t.Fatal(err)
		}
	}
}

func getJSONT(t *testing.T, url string, v any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_ = json.NewDecoder(resp.Body).Decode(v)
	return resp.StatusCode
}

func TestOrchGitCheckpoints(t *testing.T) {
	e := newOrchEnv(t)
	root := gitTestRepo(t)
	e.cwd = root
	before := snapshotRepo(t, root)
	th := e.create("hello")
	e.idle(th.ID)
	p := cpPrefix(th.ID)
	if !e.refExists(root, p+"0-start") || e.refExists(root, p+"0-end") {
		t.Fatalf("run 0 refs: %s", gitT(t, root, "for-each-ref", "refs/am"))
	}
	if got := gitT(t, root, "rev-parse", baseRef(th.ID)); got != before.head {
		t.Fatalf("base %s want %s", got, before.head)
	}
	if got := gitT(t, root, "rev-parse", p+"0-start^"); got != before.head {
		t.Fatalf("first checkpoint parent %s", got)
	}
	e.changedRun(th.ID, editRun1(t, root))
	if e.refExists(root, p+"1-start") || !e.refExists(root, p+"1-end") {
		t.Fatalf("run 1 refs: %s", gitT(t, root, "for-each-ref", "refs/am"))
	}
	if got, want := gitT(t, root, "rev-parse", p+"1-end^"), gitT(t, root, "rev-parse", p+"0-start"); got != want {
		t.Fatalf("1-end parent %s want %s", got, want)
	}
	if files := gitT(t, root, "ls-tree", "-r", "--name-only", p+"1-end"); !strings.Contains(files, "new/b.txt") || strings.Contains(files, "gone.txt") {
		t.Fatalf("1-end tree: %s", files)
	}
	after := snapshotRepo(t, root)
	if after != before {
		t.Fatalf("user repo state changed:\nindex equal %v\nhead %s→%s\nbranches %q→%q\nstash %q→%q",
			after.index == before.index, before.head, after.head, before.branches, after.branches, before.stash, after.stash)
	}
	stOut, _ := exec.Command("git", "-C", root, "status", "--porcelain").Output()
	if st := string(stOut); !strings.Contains(st, "?? new/") || !strings.Contains(st, " M a.txt") || !strings.Contains(st, " D gone.txt") {
		t.Fatalf("status shows staging: %q", st)
	}
	var info orchGitInfo
	if code := getJSONT(t, e.server().URL+"/api/orch/threads/"+th.ID+"/git", &info); code != 200 {
		t.Fatalf("git info %d", code)
	}
	if !info.IsRepo || info.Branch != "main" || info.DefaultBranch != "main" || info.Dirty != 3 || len(info.Files) != 3 || len(info.Checkpoints) != 2 {
		t.Fatalf("info %+v", info)
	}
	st := map[string]string{}
	for _, f := range info.Files {
		st[f.Path] = f.Status
	}
	if st["a.txt"] != "modified" || st["new/b.txt"] != "untracked" || st["gone.txt"] != "deleted" {
		t.Fatalf("file statuses %v", st)
	}
	end := info.Checkpoints[1]
	if end.Kind != "end" || end.RunIndex != 1 || end.Prompt != "hang" || end.Files != 3 || end.Add != 2 || end.Del != 1 {
		t.Fatalf("end checkpoint %+v", end)
	}
}

func TestOrchGitCheckpointTimeout(t *testing.T) {
	e := newOrchEnv(t)
	e.cwd = gitTestRepo(t)
	e.m.git.budget = time.Nanosecond
	th := e.create("hello")
	e.idle(th.ID)
	d := e.detail(th.ID)
	it := itemOf(d, "other")
	if it == nil || it.Command != "checkpoint" || !strings.Contains(it.Text, "Checkpoint skipped") {
		t.Fatalf("no timeout warning: %+v", d.Items)
	}
	if e.refExists(e.cwd, cpPrefix(th.ID)+"0-start") {
		t.Fatal("checkpoint written despite timeout")
	}
}

func TestOrchGitDiff(t *testing.T) {
	e := newOrchEnv(t)
	root := gitTestRepo(t)
	e.cwd = root
	th := e.create("hello")
	e.idle(th.ID)
	e.changedRun(th.ID, editRun1(t, root))
	srv := e.server()
	var d orchDiff
	if code := getJSONT(t, srv.URL+"/api/orch/threads/"+th.ID+"/diff?from=0-start&to=1-end", &d); code != 200 {
		t.Fatalf("diff %d", code)
	}
	got := map[string]orchDiffFile{}
	for _, f := range d.Files {
		got[f.Path] = f
	}
	if got["a.txt"].Status != "modified" || got["a.txt"].Add != 1 || !strings.Contains(got["a.txt"].Patch, "+changed") ||
		got["new/b.txt"].Status != "added" || got["gone.txt"].Status != "deleted" || got["gone.txt"].Del != 1 || d.Truncated {
		t.Fatalf("diff %+v", d)
	}
	var byRun orchDiff
	getJSONT(t, srv.URL+"/api/orch/threads/"+th.ID+"/diff?run=1", &byRun)
	if len(byRun.Files) != 3 || byRun.From != cpPrefix(th.ID)+"0-start" || byRun.To != cpPrefix(th.ID)+"1-end" {
		t.Fatalf("run diff %+v", byRun)
	}
	// Work tree diff: untracked files show, and big patches are capped.
	big := strings.Repeat("0123456789abcdefghij0123456789abcdefghij\n", 3000)
	for _, n := range []string{"big1.txt", "big2.txt", "big3.txt", "big4.txt"} {
		writeT(t, filepath.Join(root, n), big)
	}
	writeT(t, filepath.Join(root, "skip.log"), "ignored\n")
	var wt orchDiff
	getJSONT(t, srv.URL+"/api/orch/threads/"+th.ID+"/diff?from=1-end", &wt)
	if wt.To != "worktree" || !wt.Truncated || len(wt.Files) != 4 {
		t.Fatalf("worktree diff to=%s truncated=%v files=%d", wt.To, wt.Truncated, len(wt.Files))
	}
	total := 0
	for _, f := range wt.Files {
		if f.Status != "added" || len(f.Patch) > orchDiffFileCap || f.Add != 3000 || !f.Truncated {
			t.Fatalf("file %s status %s patch %d add %d truncated %v", f.Path, f.Status, len(f.Patch), f.Add, f.Truncated)
		}
		total += len(f.Patch)
	}
	if total > orchDiffCap || wt.Files[3].Patch != "" {
		t.Fatalf("total patch %d", total)
	}
	var one orchDiff
	getJSONT(t, srv.URL+"/api/orch/threads/"+th.ID+"/diff?from=head&path=a.txt", &one)
	if len(one.Files) != 1 || one.Files[0].Path != "a.txt" {
		t.Fatalf("path filter %+v", one.Files)
	}
	var bad map[string]any
	if code := getJSONT(t, srv.URL+"/api/orch/threads/"+th.ID+"/diff?from=--output=x", &bad); code != 400 {
		t.Fatalf("option ref %d", code)
	}
	if code := getJSONT(t, srv.URL+"/api/orch/threads/"+th.ID+"/diff?from=9-end", &bad); code != 404 {
		t.Fatalf("unknown ref %d", code)
	}
}

func TestOrchGitRestore(t *testing.T) {
	e := newOrchEnv(t)
	root := gitTestRepo(t)
	e.cwd = root
	th := e.create("hello")
	e.idle(th.ID)
	e.changedRun(th.ID, editRun1(t, root))
	writeT(t, filepath.Join(root, "keep.log"), "ignored stays\n")
	writeT(t, filepath.Join(root, "later.txt"), "after the run\n")
	srv := e.server()
	code, out := postJSON(t, srv.URL+"/api/orch/threads/"+th.ID+"/restore", `{"ref":"0-start"}`)
	if code != 200 || out["ok"] != true {
		t.Fatalf("restore %d %v", code, out)
	}
	safety, _ := out["safetyRef"].(string)
	if safety != cpPrefix(th.ID)+"1-restore" || !e.refExists(root, safety) {
		t.Fatalf("safety ref %q", safety)
	}
	if readT(t, filepath.Join(root, "a.txt")) != "a\n" || readT(t, filepath.Join(root, "gone.txt")) != "will be deleted\n" {
		t.Fatal("tracked files not restored")
	}
	if pathExists(filepath.Join(root, "new")) || pathExists(filepath.Join(root, "later.txt")) {
		t.Fatal("files absent from the target were kept")
	}
	if readT(t, filepath.Join(root, "keep.log")) != "ignored stays\n" {
		t.Fatal("ignored file touched")
	}
	if files := gitT(t, root, "ls-tree", "-r", "--name-only", safety); !strings.Contains(files, "later.txt") || !strings.Contains(files, "new/b.txt") {
		t.Fatalf("safety tree %s", files)
	}
	if st := gitT(t, root, "status", "--porcelain"); st != "" {
		t.Fatalf("work tree differs from HEAD after restore: %q", st)
	}
	// The safety checkpoint restores the undone work.
	if code, _ := postJSON(t, srv.URL+"/api/orch/threads/"+th.ID+"/restore", `{"ref":"`+safety+`"}`); code != 200 {
		t.Fatalf("restore safety %d", code)
	}
	if !pathExists(filepath.Join(root, "later.txt")) || readT(t, filepath.Join(root, "new", "b.txt")) != "new file\n" {
		t.Fatal("safety restore incomplete")
	}
	if !e.refExists(root, cpPrefix(th.ID)+"1-restore-2") {
		t.Fatal("second safety ref missing")
	}
	if it := itemOf(e.detail(th.ID), "other"); it == nil || it.Command != "restore" {
		t.Fatalf("restore item %+v", it)
	}
	if _, err := e.m.Send(th.ID, "hang"); err != nil {
		t.Fatal(err)
	}
	e.hanging(th.ID)
	if code, _ := postJSON(t, srv.URL+"/api/orch/threads/"+th.ID+"/restore", `{"ref":"0-start"}`); code != 409 {
		t.Fatalf("restore while running %d", code)
	}
	if code, _ := postJSON(t, srv.URL+"/api/orch/threads/"+th.ID+"/commit", `{"message":"x"}`); code != 409 {
		t.Fatalf("commit while running %d", code)
	}
	_ = e.m.Interrupt(th.ID)
	e.idle(th.ID)
}

func TestOrchGitCommit(t *testing.T) {
	e := newOrchEnv(t)
	root := gitTestRepo(t)
	e.cwd = root
	th := e.create("hello")
	e.idle(th.ID)
	srv := e.server()
	url := srv.URL + "/api/orch/threads/" + th.ID + "/commit"
	if code, _ := postJSON(t, url, `{"message":"  "}`); code != 400 {
		t.Fatalf("empty message %d", code)
	}
	if code, _ := postJSON(t, url, `{"message":"nothing"}`); code != 409 {
		t.Fatalf("nothing to commit %d", code)
	}
	writeT(t, filepath.Join(root, "c.txt"), "c\n")
	writeT(t, filepath.Join(root, "d.txt"), "d\n")
	code, out := postJSON(t, url, `{"message":"Add c\n\nBody line","paths":["c.txt"]}`)
	if code != 200 || out["sha"] != gitT(t, root, "rev-parse", "HEAD") {
		t.Fatalf("commit %d %v", code, out)
	}
	if msg := gitT(t, root, "log", "-1", "--format=%B"); msg != "Add c\n\nBody line" {
		t.Fatalf("message %q", msg)
	}
	if who := gitT(t, root, "log", "-1", "--format=%an <%ae> %cn <%ce>"); who != "Test User <test@example.com> Test User <test@example.com>" {
		t.Fatalf("identity %q", who)
	}
	if st := gitT(t, root, "status", "--porcelain"); st != "?? d.txt" {
		t.Fatalf("paths commit took more: %q", st)
	}
	if it := itemOf(e.detail(th.ID), "other"); it == nil || it.Command != "git commit" || !strings.Contains(it.Text, "Add c") {
		t.Fatalf("commit item %+v", it)
	}
	hooks := filepath.Join(root, ".git", "hooks")
	writeT(t, filepath.Join(hooks, "pre-commit"), "#!/bin/sh\necho 'lint failed: trailing space' >&2\nexit 1\n")
	_ = os.Chmod(filepath.Join(hooks, "pre-commit"), 0o755)
	code, out = postJSON(t, url, `{"message":"Add d"}`)
	if code != 422 || !strings.Contains(out["output"].(string), "lint failed") {
		t.Fatalf("hook failure %d %v", code, out)
	}
}

func TestOrchGitPushAndPR(t *testing.T) {
	e := newOrchEnv(t)
	root := gitTestRepo(t)
	e.cwd = root
	th := e.create("hello")
	e.idle(th.ID)
	srv := e.server()
	base := srv.URL + "/api/orch/threads/" + th.ID
	if code, _ := postJSON(t, base+"/push", ``); code != 403 {
		t.Fatalf("push main %d", code)
	}
	bare := filepath.Join(t.TempDir(), "remote.git")
	gitT(t, root, "init", "-q", "--bare", bare)
	gitT(t, root, "remote", "add", "origin", bare)
	gitT(t, root, "checkout", "-q", "-b", "feature/x")
	ghDir := t.TempDir()
	gh := filepath.Join(ghDir, "gh")
	writeT(t, gh, "#!/bin/sh\nprintf '%s\\n' \"$@\" > \""+ghDir+"/args\"\necho 'Creating pull request'\necho 'https://example.invalid/o/r/pull/7'\n")
	_ = os.Chmod(gh, 0o755)
	e.m.git.gh = gh
	if code, out := postJSON(t, base+"/pr", `{"title":"T"}`); code != 409 {
		t.Fatalf("pr without upstream %d %v", code, out)
	}
	if pathExists(filepath.Join(ghDir, "args")) {
		t.Fatal("gh ran without upstream")
	}
	code, out := postJSON(t, base+"/push", ``)
	if code != 200 || out["ok"] != true {
		t.Fatalf("push %d %v", code, out)
	}
	if gitT(t, bare, "rev-parse", "feature/x") != gitT(t, root, "rev-parse", "HEAD") {
		t.Fatal("remote lacks the branch")
	}
	if out, err := exec.Command("git", "-C", root, "config", "--get", "branch.feature/x.remote").CombinedOutput(); err == nil {
		t.Fatalf("push wrote upstream config %q", out)
	}
	if code, _ := postJSON(t, base+"/pr", `{"title":""}`); code != 400 {
		t.Fatalf("pr empty title %d", code)
	}
	code, out = postJSON(t, base+"/pr", `{"title":"My PR","body":"Why","base":"main"}`)
	if code != 200 || out["url"] != "https://example.invalid/o/r/pull/7" {
		t.Fatalf("pr %d %v", code, out)
	}
	if args := readT(t, filepath.Join(ghDir, "args")); args != "pr\ncreate\n--head\nfeature/x\n--title\nMy PR\n--body\nWhy\n--draft\n--base\nmain\n" {
		t.Fatalf("gh args %q", args)
	}
	var cmds []string
	for _, it := range e.detail(th.ID).Items {
		if it.Kind == "other" {
			cmds = append(cmds, it.Command)
		}
	}
	if strings.Join(cmds, ",") != "git push,gh pr create" {
		t.Fatalf("items %v", cmds)
	}
	gitT(t, root, "checkout", "-q", "main")
	gitT(t, root, "checkout", "-q", "-b", "master")
	if code, _ := postJSON(t, base+"/push", ``); code != 403 {
		t.Fatalf("push master %d", code)
	}
}

func TestOrchGitOpen(t *testing.T) {
	e := newOrchEnv(t)
	th := e.create("hello")
	e.idle(th.ID)
	var calls []string
	e.m.git.look = func(string) (string, error) { return "", errors.New("absent") }
	e.m.git.launch = func(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
		if name == "tmux" && args[0] == "list-sessions" {
			return []byte("100\told\n200\tnew\n"), nil
		}
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil, nil
	}
	mux := http.NewServeMux()
	e.m.routes(mux)
	do := func(remote, body string, hdr map[string]string) int {
		r := httptest.NewRequest("POST", "/api/orch/threads/"+th.ID+"/open", bytes.NewBufferString(body))
		r.RemoteAddr = remote
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w.Code
	}
	if code := do("100.101.102.103:4000", `{"target":"finder"}`, nil); code != 403 {
		t.Fatalf("tailscale caller %d", code)
	}
	if code := do("127.0.0.1:4000", `{"target":"finder"}`, map[string]string{"X-Forwarded-For": "100.1.1.1"}); code != 403 {
		t.Fatalf("proxied caller %d", code)
	}
	if len(calls) != 0 {
		t.Fatalf("refused callers launched %v", calls)
	}
	for _, target := range []string{"finder", "editor", "terminal"} {
		if code := do("127.0.0.1:4000", `{"target":"`+target+`"}`, nil); code != 200 {
			t.Fatalf("%s %d", target, code)
		}
	}
	want := []string{"open " + e.cwd, "open -t " + e.cwd, "tmux new-window -t new: -c " + e.cwd}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls %q", calls)
	}
	if code := do("[::1]:4000", `{"target":"x"}`, nil); code != 400 {
		t.Fatalf("bad target %d", code)
	}
}

func TestOrchGitRootWorktreeRemove(t *testing.T) {
	e := newOrchEnv(t)
	if _, err := e.m.CreateThread(orchCreate{Agent: "codex", Cwd: e.cwd, Prompt: "hello", Workspace: "worktree"}); err == nil || !strings.Contains(err.Error(), "not a git repository") {
		t.Fatalf("worktree outside git: %v", err)
	}
	root := gitTestRepo(t)
	th, err := e.m.CreateThread(orchCreate{Agent: "codex", Cwd: root, Prompt: "Root task", Sandbox: "read-only", Workspace: "worktree"})
	if err != nil {
		t.Fatal(err)
	}
	e.idle(th.ID)
	wt := th.Worktree
	if wt == nil || th.Cwd != wt.Path || th.Workspace != "worktree" || !strings.HasPrefix(wt.Branch, "am/root-task-") {
		t.Fatalf("root worktree %+v", th)
	}
	if th.Repo != filepath.Base(root) || e.thread(th.ID).Repo != filepath.Base(root) {
		t.Fatalf("worktree thread repo %q, want the main repo %q", th.Repo, filepath.Base(root))
	}
	if !e.refExists(root, cpPrefix(th.ID)+"0-start") {
		t.Fatal("no checkpoint in the worktree")
	}
	writeT(t, filepath.Join(wt.Path, "dirty.txt"), "x\n")
	srv := e.server()
	url := srv.URL + "/api/orch/threads/" + th.ID + "/worktree/remove"
	if code, _ := postJSON(t, url, ``); code != 409 {
		t.Fatalf("dirty remove %d", code)
	}
	code, out := postJSON(t, url, `{"force":true}`)
	if code != 200 {
		t.Fatalf("force remove %d %v", code, out)
	}
	if pathExists(wt.Path) {
		t.Fatal("worktree still on disk")
	}
	if gitT(t, root, "rev-parse", "--verify", "refs/heads/"+wt.Branch) == "" {
		t.Fatal("branch deleted")
	}
	if got := e.thread(th.ID).Worktree; got == nil || !got.Removed {
		t.Fatalf("thread worktree %+v", got)
	}
	if code, _ := postJSON(t, url, ``); code != 400 {
		t.Fatalf("second remove %d", code)
	}
}

func TestOrchGitPrune(t *testing.T) {
	e := newOrchEnv(t)
	root := gitTestRepo(t)
	e.cwd = root
	old := e.create("hello")
	e.idle(old.ID)
	keep := e.create("hello")
	e.idle(keep.ID)
	if err := e.m.Close(old.ID); err != nil {
		t.Fatal(err)
	}
	e.until("closed", func() bool { return e.thread(old.ID).Status == "closed" })
	if n, err := e.m.PruneCheckpoints(time.Now()); err != nil || n != 0 {
		t.Fatalf("fresh prune %d %v", n, err)
	}
	n, err := e.m.PruneCheckpoints(time.Now().Add(31 * 24 * time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("prune %d %v", n, err)
	}
	if e.refExists(root, cpPrefix(old.ID)+"0-start") || e.refExists(root, baseRef(old.ID)) {
		t.Fatal("closed thread refs kept")
	}
	if !e.refExists(root, cpPrefix(keep.ID)+"0-start") || len(e.m.store.checkpoints(keep.ID)) != 1 || len(e.m.store.checkpoints(old.ID)) != 0 {
		t.Fatal("prune touched the open thread or kept rows")
	}
}
