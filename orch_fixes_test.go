package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOrchCloseCascade(t *testing.T) {
	e := newOrchEnv(t)
	srv := e.server()
	parent := e.create("hello")
	e.idle(parent.ID)
	a := e.delegate(parent.ID, orchChildSpec{Prompt: "hang", Agent: "codex"})
	b := e.delegate(parent.ID, orchChildSpec{Prompt: "hello", Agent: "codex"})
	e.hanging(a.ID)
	e.idle(b.ID)
	if code, _ := postJSON(t, srv.URL+"/api/orch/threads/"+b.ID+"/close", `{"cascade":false}`); code != 200 {
		t.Fatalf("close child: %d", code)
	}
	if code, out := postJSON(t, srv.URL+"/api/orch/threads/"+parent.ID+"/close", ""); code != 200 || field(out, "thread", "status") != "closed" {
		t.Fatalf("close parent: %d %v", code, out)
	}
	for _, id := range []string{parent.ID, a.ID, b.ID} {
		if st := e.thread(id).Status; st != "closed" {
			t.Fatalf("%s status %s", id, st)
		}
	}
	p2 := e.create("hello")
	e.idle(p2.ID)
	c2 := e.delegate(p2.ID, orchChildSpec{Prompt: "hello", Agent: "codex"})
	e.idle(c2.ID)
	if code, _ := postJSON(t, srv.URL+"/api/orch/threads/"+p2.ID+"/close", `{"cascade":false}`); code != 200 {
		t.Fatal(code)
	}
	if e.thread(c2.ID).Status == "closed" {
		t.Fatal("cascade false closed the child")
	}
}

func TestOrchContinueKeepsModel(t *testing.T) {
	e := newOrchEnv(t)
	native := "21111111-2222-4333-8444-555555555555"
	tr := filepath.Join(e.dir, "watched.jsonl")
	_ = os.WriteFile(tr, []byte(`{"type":"assistant","message":{"model":"claude-sonnet-4-5"}}
{"type":"assistant","message":{"model":"claude-opus-4-6"}}
{"type":"assistant","message":{"model":"<synthetic>"}}
`), 0o644)
	ctr := filepath.Join(e.dir, "rollout.jsonl")
	_ = os.WriteFile(ctr, []byte(`{"type":"turn_context","payload":{"model":"gpt-5.5-codex"}}`+"\n"), 0o644)
	e.m.watched = func(id string) (*orchWatched, error) {
		switch id {
		case "claude:w":
			return &orchWatched{Agent: "claude", NativeID: native, Cwd: e.cwd, Title: "w", Model: "stale", Transcript: tr}, nil
		case "codex:w":
			return &orchWatched{Agent: "codex", NativeID: "thr_m", Cwd: e.cwd, Title: "c", Transcript: ctr}, nil
		}
		return nil, errOrchNotFound
	}
	th, _, err := e.m.Continue(orchContinue{SessionID: "claude:w"})
	if err != nil || th.Model != "claude-opus-4-6" {
		t.Fatalf("claude model %+v %v", th, err)
	}
	cx, _, err := e.m.Continue(orchContinue{SessionID: "codex:w"})
	if err != nil || cx.Model != "gpt-5.5-codex" {
		t.Fatalf("codex model %+v %v", cx, err)
	}
	if transcriptModel("claude", filepath.Join(e.dir, "none")) != "" {
		t.Fatal("missing transcript")
	}
}

func TestReadOnlyBashAllowlist(t *testing.T) {
	for cmd, want := range map[string]bool{
		"git status":                      true,
		"git --no-pager log --oneline -5": true,
		"git diff HEAD~1 -- a.go":         true,
		"ls -la":                          true,
		"rg -n foo src | head -20":        true,
		"find . -name '*.go'":             true,
		"cat a.txt | wc -l":               true,
		"git commit -m x":                 false,
		"git diff --output=/tmp/x":        false,
		"git -c core.pager=sh log":        false,
		"find . -delete":                  false,
		"find . -exec rm {} ;":            false,
		"ls > out.txt":                    false,
		"cat a && rm a":                   false,
		"echo $(rm -rf x)":                false,
		"rg --pre ./x foo":                false,
		"sort -o out a":                   false,
		"go test ./...":                   false,
		"sed -i s/a/b/ f":                 false,
		"rm -rf x":                        false,
		"ls || rm x":                      false,
		"":                                false,
	} {
		if got := readOnlyBash(cmd, "/ws"); got != want {
			t.Errorf("readOnlyBash(%q) = %v", cmd, got)
		}
	}
}

func TestOrchClaudeReadOnlyBash(t *testing.T) {
	e := newOrchEnv(t)
	if _, err := e.appr.upsertRule(Rule{Action: "allow", Agent: "claude", Tool: "Bash", Pattern: "touch *"}); err != nil {
		t.Fatal(err)
	}
	parent := e.createClaude("hello", "")
	e.idle(parent.ID)
	ro := e.delegate(parent.ID, orchChildSpec{Prompt: "bash git status", Agent: "claude", Workspace: "read-only", PermissionMode: "auto"})
	e.idle(ro.ID)
	if got := lastAgentText(e.detail(ro.ID)); got != "decision=allow" || len(e.detail(ro.ID).Requests) != 0 {
		t.Fatalf("allowlisted bash: %q", got)
	}
	if _, err := e.m.Send(ro.ID, "bash touch y"); err != nil {
		t.Fatal(err)
	}
	e.until("request", func() bool { return len(e.detail(ro.ID).Requests) == 1 })
	a := e.pending(e.detail(ro.ID).Requests[0].ApprovalID)
	if a == nil || a.Summary != "touch y" {
		t.Fatalf("approval %+v", a)
	}
	e.decide(a.ID, approvalDecision{Behavior: "deny", Message: "read-only"})
	e.idle(ro.ID)
	var args string
	for _, l := range strings.Split(e.fakeLog(), "\n") {
		if strings.HasPrefix(l, "args ") && strings.Contains(l, ro.NativeID) {
			args = l
		}
	}
	if !strings.Contains(args, `--settings {"permissions":{"ask":["Bash"]}}`) || !strings.Contains(args, "--permission-mode default") || !strings.Contains(args, "--strict-mcp-config") {
		t.Fatalf("read-only args: %s", args)
	}
}

func TestOrchClaudeAlwaysClaudeRule(t *testing.T) {
	e := newOrchEnv(t)
	e.appr.mu.Lock()
	seeded := len(e.appr.rules)
	e.appr.mu.Unlock()
	e.claudeRequestRun("approve", "command", func(a *Approval) {
		if _, code, err := e.appr.decide(a.ID, approvalDecision{Behavior: "allow", AlwaysScope: "claude"}); code != 400 || err == nil {
			t.Fatalf("scope without always: %d", code)
		}
		e.decide(a.ID, approvalDecision{Behavior: "allow", Always: true, AlwaysScope: "claude"})
	})
	if !strings.Contains(e.fakeLog(), `"updatedPermissions":[{"destination":"session","mode":"acceptEdits","type":"setMode"}]`) {
		t.Fatalf("no updatedPermissions:\n%s", e.fakeLog())
	}
	e.appr.mu.Lock()
	n := len(e.appr.rules)
	e.appr.mu.Unlock()
	if n != seeded {
		t.Fatalf("claude scope added %d agent-monitor rules", n-seeded)
	}
}

func TestOrchFanOutRollback(t *testing.T) {
	e := newOrchEnv(t)
	reg := `[{"id":"ghost","name":"Ghost","command":"/nope/ghost-agent","args":["acp"]}]`
	if err := os.WriteFile(filepath.Join(e.dir, "acp-agents.json"), []byte(reg), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := e.server()
	body := `{"agent":"codex","cwd":"` + e.cwd + `","prompt":"hello","children":[{"title":"ok","prompt":"hang","agent":"codex"},{"title":"bad","prompt":"x","agent":"ghost"}]}`
	code, out := postJSON(t, srv.URL+"/api/orch/threads", body)
	if code != 422 || !strings.Contains(out["error"].(string), "not installed") {
		t.Fatalf("fan-out: %d %v", code, out)
	}
	ths, _, _ := e.store.Threads()
	if len(ths) != 2 {
		t.Fatalf("threads %d", len(ths))
	}
	for _, th := range ths {
		if th.Status != "closed" {
			t.Fatalf("not rolled back: %+v", th)
		}
	}
}

func TestOrchContinueInParent(t *testing.T) {
	e := newOrchEnv(t)
	e.cwd = gitRepo(t)
	srv := e.server()
	th, err := e.m.CreateThread(orchCreate{Agent: "codex", Cwd: e.cwd, Prompt: "hello", Workspace: "worktree"})
	if err != nil {
		t.Fatal(err)
	}
	e.idle(th.ID)
	if code, _ := postJSON(t, srv.URL+"/api/orch/threads/"+th.ID+"/worktree/parent", ""); code != 400 {
		t.Fatalf("before removal: %d", code)
	}
	if _, err := e.m.RemoveWorktree(th.ID, true); err != nil {
		t.Fatal(err)
	}
	code, out := postJSON(t, srv.URL+"/api/orch/threads/"+th.ID+"/worktree/parent", "")
	if code != 200 || field(out, "thread", "cwd") != e.cwd || field(out, "thread", "workspace") != "here" {
		t.Fatalf("continue in parent: %d %v", code, out)
	}
	d := e.detail(th.ID)
	if last := d.Items[len(d.Items)-1]; last.Command != "cwd" || !strings.Contains(last.Text, e.cwd) {
		t.Fatalf("note %+v", last)
	}
	if _, err := e.m.Send(th.ID, "hello"); err != nil {
		t.Fatal(err)
	}
	e.until("second run", func() bool { r := e.runs(th.ID); return len(r) == 2 && r[1].Status == "completed" })
}

func TestOrchACPSilenceTimeout(t *testing.T) {
	e := newOrchEnv(t)
	e.acpSetup(e.m)
	e.m.acpTurnTimeout = func() time.Duration { return 300 * time.Millisecond }
	th := e.createACP("hang", "")
	e.until("interrupted", func() bool {
		r := e.runs(th.ID)
		return len(r) == 1 && r[0].Status == "interrupted"
	})
	found := false
	for _, it := range e.detail(th.ID).Items {
		found = found || strings.Contains(it.Text, "acpTurnTimeoutMin")
	}
	if !found {
		t.Fatal("no timeout note")
	}
}

func TestWatchedDiff(t *testing.T) {
	e := newOrchEnv(t)
	repo := gitRepo(t)
	_ = os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\nb\n"), 0o644)
	srv := e.server()
	allowCwds(t, repo)
	get := func(q string) (int, map[string]any) {
		resp, err := http.Get(srv.URL + "/api/orch/git/diff?" + q)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	code, out := get("cwd=" + repo)
	files, _ := out["files"].([]any)
	if code != 200 || len(files) != 1 || out["from"] != "head" || out["to"] != "worktree" {
		t.Fatalf("diff %d %v", code, out)
	}
	if f := files[0].(map[string]any); f["path"] != "a.txt" || f["add"].(float64) != 1 || !strings.Contains(f["patch"].(string), "+b") {
		t.Fatalf("file %v", f)
	}
	if code, _ := get("cwd=" + repo + "&from=refs/heads/main"); code != 400 {
		t.Fatalf("bad from: %d", code)
	}
	plain := t.TempDir()
	allowCwds(t, plain)
	if code, _ := get("cwd=" + plain); code != 409 {
		t.Fatalf("not a repo: %d", code)
	}
	if code, _ := get("cwd=relative"); code != 400 {
		t.Fatalf("relative: %d", code)
	}
}
