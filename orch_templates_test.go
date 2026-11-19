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

func doJSON(t *testing.T, method, url, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestOrchTemplatesCRUD(t *testing.T) {
	e := newOrchEnv(t)
	srv := e.server()
	var list []OTemplate
	getJSONURL(t, srv.URL+"/api/orch/templates", &list)
	if len(list) != 2 || list[0].Name != "Fix and review" || !list[0].Builtin || list[1].Workspace != "read-only" || list[1].PermissionMode != "plan" {
		t.Fatalf("builtins %+v", list)
	}
	if strings.Join(list[0].Vars, ",") != "cwd,branch,task" || !list[0].Review || list[0].ReviewAgent != "codex" || list[0].Workspace != "worktree" {
		t.Fatalf("fix and review %+v", list[0])
	}
	for _, m := range []string{"PUT", "DELETE"} {
		if code, _ := doJSON(t, m, srv.URL+"/api/orch/templates/builtin-fix-and-review", `{"name":"x","prompt":"y"}`); code != 403 {
			t.Fatalf("%s builtin: %d", m, code)
		}
	}
	if code, _ := doJSON(t, "POST", srv.URL+"/api/orch/templates", `{"name":"","prompt":"x"}`); code != 400 {
		t.Fatalf("empty name %d", code)
	}
	if code, _ := doJSON(t, "POST", srv.URL+"/api/orch/templates", `{"name":"a","prompt":"x","workspace":"moon"}`); code != 400 {
		t.Fatalf("bad workspace %d", code)
	}
	code, out := doJSON(t, "POST", srv.URL+"/api/orch/templates", `{"name":"Nightly","agent":"codex","prompt":"Check {{area}} on {{date}}","children":[{"title":"{{area}} tests","prompt":"run {{area}}"}]}`)
	if code != 201 {
		t.Fatalf("create %d %v", code, out)
	}
	tpl := out["template"].(map[string]any)
	id := tpl["id"].(string)
	if !strings.HasPrefix(id, "tpl_") || tpl["workspace"] != "here" || tpl["builtin"] != nil {
		t.Fatalf("created %v", tpl)
	}
	if vars, _ := json.Marshal(tpl["vars"]); string(vars) != `["area","date"]` {
		t.Fatalf("vars %s", vars)
	}
	code, out = doJSON(t, "PUT", srv.URL+"/api/orch/templates/"+id, `{"name":"Nightly 2","prompt":"p {{x}}","builtin":true}`)
	if code != 200 || out["template"].(map[string]any)["name"] != "Nightly 2" || out["template"].(map[string]any)["builtin"] != nil {
		t.Fatalf("update %d %v", code, out)
	}
	if code, _ := doJSON(t, "PUT", srv.URL+"/api/orch/templates/tpl_missing", `{"name":"a","prompt":"b"}`); code != 404 {
		t.Fatalf("update missing %d", code)
	}
	code, out = doJSON(t, "POST", srv.URL+"/api/orch/templates/builtin-read-only-investigation/copy", `{}`)
	if code != 201 || out["template"].(map[string]any)["name"] != "Read-only investigation (copy)" || out["template"].(map[string]any)["builtin"] != nil {
		t.Fatalf("copy %d %v", code, out)
	}
	getJSONURL(t, srv.URL+"/api/orch/templates", &list)
	if len(list) != 4 {
		t.Fatalf("list %d", len(list))
	}
	if code, _ := doJSON(t, "DELETE", srv.URL+"/api/orch/templates/"+id, ""); code != 200 {
		t.Fatalf("delete %d", code)
	}
	if code, _ := doJSON(t, "DELETE", srv.URL+"/api/orch/templates/"+id, ""); code != 404 {
		t.Fatalf("delete again %d", code)
	}
	if b, err := os.ReadFile(filepath.Join(e.dir, "templates.json")); err != nil || strings.Contains(string(b), id) {
		t.Fatalf("file %s %v", b, err)
	}
}

func TestOrchTemplateRunVars(t *testing.T) {
	e := newOrchEnv(t)
	srv := e.server()
	tpl, err := e.m.SaveTemplate("", OTemplate{Name: "T", Agent: "codex", Sandbox: "read-only", Prompt: "Look at {{area}} in {{cwd}} on {{date}} branch={{branch}}"})
	if err != nil {
		t.Fatal(err)
	}
	code, out := postJSON(t, srv.URL+"/api/orch/templates/"+tpl.ID+"/run", `{"cwd":"`+e.cwd+`"}`)
	if code != 400 || out["missing"].([]any)[0] != "area" {
		t.Fatalf("missing var %d %v", code, out)
	}
	if code, _ := postJSON(t, srv.URL+"/api/orch/templates/"+tpl.ID+"/run", `{"cwd":"rel","vars":{"area":"x"}}`); code != 400 {
		t.Fatalf("relative cwd %d", code)
	}
	code, out = postJSON(t, srv.URL+"/api/orch/templates/"+tpl.ID+"/run", `{"cwd":"`+e.cwd+`","vars":{"area":"parser"}}`)
	if code != 201 {
		t.Fatalf("run %d %v", code, out)
	}
	id := out["thread"].(map[string]any)["id"].(string)
	e.idle(id)
	th := e.thread(id)
	want := "Look at parser in " + e.cwd + " on " + time.Now().Format("2006-01-02") + " branch="
	if runs := e.runs(id); len(runs) != 1 || runs[0].Prompt != want || th.TemplateID != tpl.ID || th.Sandbox != "read-only" || th.AutoReview != "" {
		t.Fatalf("thread %+v runs %+v", th, runs)
	}
}

func TestOrchTemplateAutoReview(t *testing.T) {
	e := newOrchEnv(t)
	e.cwd = gitRepo(t)
	os.WriteFile(filepath.Join(e.cwd, "b.txt"), []byte("new work\n"), 0o644)
	tpl, err := e.m.SaveTemplate("", OTemplate{Name: "R", Agent: "codex", Prompt: "hello", Review: true})
	if err != nil {
		t.Fatal(err)
	}
	th, err := e.m.RunTemplate(tpl.ID, orchTemplateRun{Cwd: e.cwd})
	if err != nil {
		t.Fatal(err)
	}
	if th.AutoReview != "codex" {
		t.Fatalf("autoReview %q", th.AutoReview)
	}
	e.until("auto review done", func() bool { r := e.thread(th.ID).Review; return r != nil && r.State == "done" })
	rv := e.thread(th.ID).Review
	if len(rv.Findings) != 1 || rv.Agent != "codex" {
		t.Fatalf("review %+v", rv)
	}
	// a later run does not start a second review
	if _, err := e.m.Send(th.ID, "hello"); err != nil {
		t.Fatal(err)
	}
	e.until("second run", func() bool { r := e.runs(th.ID); return len(r) == 2 && r[1].Status == "completed" })
	time.Sleep(300 * time.Millisecond)
	if kids := e.thread(th.ID).Children; len(kids) != 1 {
		t.Fatalf("children %v", kids)
	}
}

func TestOrchBuiltinFixAndReview(t *testing.T) {
	e := newOrchEnv(t)
	repo := gitRepo(t)
	th, err := e.m.RunTemplate("builtin-fix-and-review", orchTemplateRun{Cwd: repo, Vars: map[string]string{"task": "fix the parser"}})
	if err != nil {
		t.Fatal(err)
	}
	if th.Agent != "claude" || th.Worktree == nil || th.PermissionMode != "acceptEdits" || th.TemplateID != "builtin-fix-and-review" {
		t.Fatalf("thread %+v", th)
	}
	e.until("review recorded", func() bool { r := e.thread(th.ID).Review; return r != nil && r.State == "failed" })
	if rv := e.thread(th.ID).Review; !strings.Contains(rv.Error, "nothing to review") {
		t.Fatalf("review %+v", rv)
	}
	if runs := e.runs(th.ID); !strings.Contains(runs[0].Prompt, "fix the parser") || !strings.Contains(runs[0].Prompt, "(branch main)") {
		t.Fatalf("prompt %q", runs[0].Prompt)
	}
	ro, err := e.m.RunTemplate("builtin-read-only-investigation", orchTemplateRun{Cwd: repo, Vars: map[string]string{"question": "why"}})
	if err != nil {
		t.Fatal(err)
	}
	if ro.Workspace != "read-only" || ro.PermissionMode != "plan" {
		t.Fatalf("read-only %+v", ro)
	}
	e.idle(ro.ID)
	if !strings.Contains(e.fakeLog(), "--disallowedTools Edit,Write,MultiEdit,NotebookEdit") {
		t.Fatal("read-only root did not disallow edits")
	}
}
