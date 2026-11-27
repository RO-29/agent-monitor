package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type approvalEnv struct {
	t   *testing.T
	s   *ApprovalStore
	srv *httptest.Server
}

func newApprovalEnv(t *testing.T) *approvalEnv {
	t.Helper()
	s := newApprovalStore(t.TempDir())
	s.focused = func(string) bool { return false }
	prevFront, prevRules := cursorDesktopInFront, cursorDesktopRulesFn
	cursorDesktopInFront = func() bool { return false }
	cursorDesktopRulesFn = func() *cursorDesktopRules { return nil }
	t.Cleanup(func() { cursorDesktopInFront, cursorDesktopRulesFn = prevFront, prevRules })
	mux := http.NewServeMux()
	s.routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(func() {
		srv.Close()
		if s.db != nil {
			s.db.Close()
		}
	})
	return &approvalEnv{t: t, s: s, srv: srv}
}

type hookResult struct {
	code int
	body map[string]any
}

func (e *approvalEnv) post(path string, body any) (int, []byte) {
	b, _ := json.Marshal(body)
	resp, err := http.Post(e.srv.URL+path, "application/json", bytes.NewReader(b))
	if err != nil {
		e.t.Fatalf("post %s: %v", path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

// startHook fires a hook request in the background and returns its result channel.
func (e *approvalEnv) startHook(hook string, payload map[string]any) chan hookResult {
	ch := make(chan hookResult, 1)
	go func() {
		code, raw := e.post("/api/approvals/hook/"+hook, payload)
		r := hookResult{code: code}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &r.body)
		}
		ch <- r
	}()
	return ch
}

func (e *approvalEnv) waitPending() *Approval {
	e.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		e.s.mu.Lock()
		for _, p := range e.s.pending {
			a := copyApproval(p.a)
			e.s.mu.Unlock()
			return a
		}
		e.s.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatal("no pending approval")
	return nil
}

func (e *approvalEnv) decide(id string, d map[string]any) (int, []byte) {
	return e.post("/api/approvals/"+id+"/decide", d)
}

func waitResult(t *testing.T, ch chan hookResult) hookResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(20 * time.Second):
		t.Fatal("hook did not return")
	}
	return hookResult{}
}

func claudeBash(cmd string) map[string]any {
	return map[string]any{
		"session_id": "11111111-2222", "transcript_path": "/tmp/t.jsonl", "cwd": "/repo/app",
		"hook_event_name": "PermissionRequest", "tool_name": "Bash",
		"tool_input": map[string]any{"command": cmd, "description": "run it"},
		"permission_suggestions": []any{map[string]any{"type": "addRules", "behavior": "allow", "destination": "localSettings",
			"rules": []any{map[string]any{"toolName": "Bash", "ruleContent": "npm test:*"}}}},
	}
}

func decisionOf(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	hso, _ := body["hookSpecificOutput"].(map[string]any)
	if hso == nil {
		t.Fatalf("no hookSpecificOutput in %v", body)
	}
	d, _ := hso["decision"].(map[string]any)
	if d == nil {
		t.Fatalf("no decision in %v", hso)
	}
	return d
}

func TestApprovalAllow(t *testing.T) {
	e := newApprovalEnv(t)
	ch := e.startHook(hookClaudePermission, claudeBash("npm test"))
	a := e.waitPending()
	if a.SessionID != "claude:11111111-2222" || a.Summary != "npm test" || a.Kind != "permission" {
		t.Fatalf("bad approval %+v", a)
	}
	if got := pendingCount(e.s, a.SessionID); got != 1 {
		t.Fatalf("pending by session = %d", got)
	}
	code, _ := e.decide(a.ID, map[string]any{"behavior": "allow", "updatedInput": map[string]any{"command": "npm test -- -x"}})
	if code != 200 {
		t.Fatalf("decide code %d", code)
	}
	r := waitResult(t, ch)
	d := decisionOf(t, r.body)
	if r.code != 200 || d["behavior"] != "allow" {
		t.Fatalf("got %d %v", r.code, r.body)
	}
	if ui, _ := d["updatedInput"].(map[string]any); ui["command"] != "npm test -- -x" {
		t.Fatalf("updatedInput %v", d)
	}
	if _, ok := d["updatedPermissions"]; ok {
		t.Fatal("updatedPermissions without always")
	}
	if r.body["hookSpecificOutput"].(map[string]any)["hookEventName"] != "PermissionRequest" {
		t.Fatal("wrong event name")
	}
}

func pendingCount(s *ApprovalStore, sess string) int {
	old := approvalStore
	approvalStore = s
	defer func() { approvalStore = old }()
	return pendingApprovalsBySession()[sess]
}

func TestApprovalDenyWithMessage(t *testing.T) {
	e := newApprovalEnv(t)
	ch := e.startHook(hookClaudePermission, claudeBash("make deploy"))
	a := e.waitPending()
	e.decide(a.ID, map[string]any{"behavior": "deny", "message": "use staging", "interrupt": true, "surface": "phone"})
	d := decisionOf(t, waitResult(t, ch).body)
	if d["behavior"] != "deny" || d["message"] != "use staging" || d["interrupt"] != true {
		t.Fatalf("bad deny %v", d)
	}
	list := e.s.list()["recent"].([]*Approval)
	if len(list) != 1 || list[0].State != "denied" || list[0].DecidedBy != "phone" {
		t.Fatalf("recent %+v", list)
	}
	audit, _ := e.s.audit(0, 10)
	if len(audit) != 1 || audit[0].ID != a.ID {
		t.Fatalf("audit %+v", audit)
	}
}

func TestApprovalAlwaysCreatesRule(t *testing.T) {
	e := newApprovalEnv(t)
	ch := e.startHook(hookClaudePermission, claudeBash("npm test --watch=false"))
	a := e.waitPending()
	e.decide(a.ID, map[string]any{"behavior": "allow", "always": true})
	d := decisionOf(t, waitResult(t, ch).body)
	if up, _ := d["updatedPermissions"].([]any); len(up) != 1 {
		t.Fatalf("suggestions not echoed: %v", d)
	}
	var found *Rule
	for _, r := range e.s.rules {
		if r.Action == "allow" && r.Tool == "Bash" {
			r := r
			found = &r
		}
	}
	if found == nil || found.Pattern != "npm test *" || found.CwdPrefix != "/repo/app" {
		t.Fatalf("rule %+v", found)
	}
	code, raw := e.post("/api/approvals/hook/"+hookClaudePermission, claudeBash("npm test ./pkg"))
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	if code != 200 || decisionOf(t, body)["behavior"] != "allow" {
		t.Fatalf("second request not auto-allowed: %d %s", code, raw)
	}
	recent := e.s.list()["recent"].([]*Approval)
	if recent[0].State != "auto-allowed" || recent[0].RuleID != found.ID {
		t.Fatalf("recent %+v", recent[0])
	}
}

func TestApprovalTimeoutExpires(t *testing.T) {
	e := newApprovalEnv(t)
	code, _ := e.post("/api/approvals/settings", map[string]any{"holdSeconds": 1})
	if code != 200 {
		t.Fatalf("settings %d", code)
	}
	start := time.Now()
	ch := e.startHook(hookClaudePermission, claudeBash("make build"))
	r := waitResult(t, ch)
	if r.code != http.StatusNoContent || r.body != nil {
		t.Fatalf("want 204 empty, got %d %v", r.code, r.body)
	}
	if time.Since(start) < 900*time.Millisecond {
		t.Fatal("returned before hold")
	}
	recent := e.s.list()["recent"].([]*Approval)
	if len(recent) != 1 || recent[0].State != "expired" {
		t.Fatalf("recent %+v", recent)
	}
	code, _ = e.decide(recent[0].ID, map[string]any{"behavior": "allow"})
	if code != http.StatusConflict {
		t.Fatalf("decide after expiry = %d", code)
	}
}

func TestApprovalQuestionAnswers(t *testing.T) {
	e := newApprovalEnv(t)
	qs := []any{map[string]any{"question": "Which framework?", "header": "Framework", "multiSelect": false,
		"options": []any{map[string]any{"label": "React"}, map[string]any{"label": "Vue"}}}}
	ch := e.startHook(hookClaudePretool, map[string]any{"session_id": "s1", "cwd": "/r", "hook_event_name": "PreToolUse",
		"tool_name": "AskUserQuestion", "tool_input": map[string]any{"questions": qs}, "tool_use_id": "tu1"})
	a := e.waitPending()
	if a.Kind != "question" || len(a.Questions) != 1 || a.Summary != "Which framework?" || a.HoldUntil-a.CreatedAt != 600000 {
		t.Fatalf("bad question %+v", a)
	}
	if code, _ := e.decide(a.ID, map[string]any{"behavior": "allow"}); code != 400 {
		t.Fatalf("allow without answers = %d", code)
	}
	e.decide(a.ID, map[string]any{"behavior": "allow", "answers": map[string]string{"Which framework?": "React"}})
	r := waitResult(t, ch)
	hso := r.body["hookSpecificOutput"].(map[string]any)
	if hso["hookEventName"] != "PreToolUse" || hso["permissionDecision"] != "allow" {
		t.Fatalf("bad %v", hso)
	}
	ui := hso["updatedInput"].(map[string]any)
	if ans := ui["answers"].(map[string]any); ans["Which framework?"] != "React" {
		t.Fatalf("answers %v", ui)
	}
	if q, _ := ui["questions"].([]any); len(q) != 1 {
		t.Fatalf("questions not echoed %v", ui)
	}
}

func TestApprovalPlanKeepsInput(t *testing.T) {
	e := newApprovalEnv(t)
	in := map[string]any{"plan": "# Ship it\n1. build\n2. test", "planFilePath": "/tmp/plan.md"}
	ch := e.startHook(hookClaudePretool, map[string]any{"session_id": "s2", "cwd": "/r", "tool_name": "ExitPlanMode", "tool_input": in})
	a := e.waitPending()
	if a.Kind != "plan" || a.Summary != "Ship it" || !strings.Contains(a.Plan, "2. test") {
		t.Fatalf("bad plan %+v", a)
	}
	e.decide(a.ID, map[string]any{"behavior": "allow"})
	hso := waitResult(t, ch).body["hookSpecificOutput"].(map[string]any)
	ui, _ := hso["updatedInput"].(map[string]any)
	if hso["permissionDecision"] != "allow" || ui["plan"] != in["plan"] || ui["planFilePath"] != "/tmp/plan.md" {
		t.Fatalf("plan allow %v", hso)
	}

	ch = e.startHook(hookClaudePretool, map[string]any{"session_id": "s2", "cwd": "/r", "tool_name": "ExitPlanMode", "tool_input": in})
	a = e.waitPending()
	e.decide(a.ID, map[string]any{"behavior": "deny", "message": "split step 2"})
	hso = waitResult(t, ch).body["hookSpecificOutput"].(map[string]any)
	if hso["permissionDecision"] != "deny" || hso["permissionDecisionReason"] != "split step 2" {
		t.Fatalf("plan deny %v", hso)
	}
}

func TestApprovalCodexShape(t *testing.T) {
	e := newApprovalEnv(t)
	payload := map[string]any{"session_id": "c-1", "turn_id": "t1", "cwd": "/r", "hook_event_name": "PermissionRequest",
		"tool_name": "Bash", "tool_input": map[string]any{"command": "git push origin main", "description": "push"}}
	ch := e.startHook(hookCodex, payload)
	a := e.waitPending()
	if a.Agent != "codex" || a.SessionID != "codex:c-1" || a.Risk != "medium" {
		t.Fatalf("bad codex approval %+v", a)
	}
	if code, body := e.decide(a.ID, map[string]any{"behavior": "allow", "updatedInput": map[string]any{"command": "x"}}); code != 400 {
		t.Fatalf("codex edited input must be refused: %d %s", code, body)
	}
	e.decide(a.ID, map[string]any{"behavior": "allow"})
	d := decisionOf(t, waitResult(t, ch).body)
	if len(d) != 1 || d["behavior"] != "allow" {
		t.Fatalf("codex allow must carry only behavior: %v", d)
	}
	ch = e.startHook(hookCodex, payload)
	a = e.waitPending()
	e.decide(a.ID, map[string]any{"behavior": "allow", "always": true})
	waitResult(t, ch)
	ch = e.startHook(hookCodex, payload)
	// The always rule from above auto-allows the same command prefix.
	if r := waitResult(t, ch); decisionOf(t, r.body)["behavior"] != "allow" {
		t.Fatalf("codex always rule not applied: %v", r.body)
	}
	payload["tool_input"] = map[string]any{"command": "make clean"}
	ch = e.startHook(hookCodex, payload)
	a = e.waitPending()
	e.decide(a.ID, map[string]any{"behavior": "deny", "message": "no", "interrupt": true})
	d = decisionOf(t, waitResult(t, ch).body)
	if d["behavior"] != "deny" || d["message"] != "no" || len(d) != 2 {
		t.Fatalf("codex deny %v", d)
	}
}

func TestRuleDenyBeatsAllow(t *testing.T) {
	e := newApprovalEnv(t)
	e.post("/api/approvals/rules", Rule{Action: "allow", Tool: "Bash", Pattern: "git *"})
	code, raw := e.post("/api/approvals/rules", Rule{Action: "deny", Tool: "Bash", Pattern: "git push*", Note: "no pushes"})
	if code != 200 {
		t.Fatalf("rule create %d %s", code, raw)
	}
	_, raw = e.post("/api/approvals/hook/"+hookClaudePermission, claudeBash("git push origin main"))
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	d := decisionOf(t, body)
	if d["behavior"] != "deny" || !strings.Contains(d["message"].(string), "no pushes") {
		t.Fatalf("deny rule lost: %v", d)
	}
	_, raw = e.post("/api/approvals/hook/"+hookClaudePermission, claudeBash("git commit -m x"))
	body = nil
	_ = json.Unmarshal(raw, &body)
	if decisionOf(t, body)["behavior"] != "allow" {
		t.Fatalf("allow rule lost: %s", raw)
	}
	// Seeded deny rule blocks sudo.
	_, raw = e.post("/api/approvals/hook/"+hookCodex, map[string]any{"session_id": "c", "tool_name": "Bash", "tool_input": map[string]any{"command": "sudo rm /x"}})
	body = nil
	_ = json.Unmarshal(raw, &body)
	if decisionOf(t, body)["behavior"] != "deny" {
		t.Fatalf("seed deny lost: %s", raw)
	}
}

func TestApprovalFirstDecisionWins(t *testing.T) {
	e := newApprovalEnv(t)
	ch := e.startHook(hookClaudePermission, claudeBash("make"))
	a := e.waitPending()
	if code, _ := e.decide(a.ID, map[string]any{"behavior": "deny"}); code != 200 {
		t.Fatal("first decide failed")
	}
	if code, _ := e.decide(a.ID, map[string]any{"behavior": "allow"}); code != http.StatusConflict {
		t.Fatalf("second decide = %d", code)
	}
	if d := decisionOf(t, waitResult(t, ch).body); d["behavior"] != "deny" {
		t.Fatalf("first decision lost %v", d)
	}
}

func TestApprovalFocusedPasses(t *testing.T) {
	e := newApprovalEnv(t)
	e.s.focused = func(id string) bool { return id == "claude:11111111-2222" }
	r := waitResult(t, e.startHook(hookClaudePermission, claudeBash("make")))
	if r.code != http.StatusNoContent {
		t.Fatalf("focused pane should pass, got %d", r.code)
	}
	if rec := e.s.list()["recent"].([]*Approval); rec[0].State != "passed" {
		t.Fatalf("state %s", rec[0].State)
	}
}

func TestApprovalClientDisconnectPasses(t *testing.T) {
	e := newApprovalEnv(t)
	b, _ := json.Marshal(claudeBash("make"))
	client := &http.Client{Timeout: 200 * time.Millisecond}
	_, err := client.Post(e.srv.URL+"/api/approvals/hook/"+hookClaudePermission, "application/json", bytes.NewReader(b))
	if err == nil {
		t.Fatal("expected client timeout")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rec := e.s.list()["recent"].([]*Approval); len(rec) == 1 {
			if rec[0].State != "passed" {
				t.Fatalf("state %s", rec[0].State)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("disconnect not recorded")
}

func TestRuleCRUDAndSnippet(t *testing.T) {
	e := newApprovalEnv(t)
	_, raw := e.post("/api/approvals/rules", Rule{Action: "hold", Tool: "Edit"})
	var r Rule
	_ = json.Unmarshal(raw, &r)
	if r.ID == "" {
		t.Fatalf("no id: %s", raw)
	}
	req, _ := http.NewRequest(http.MethodDelete, e.srv.URL+"/api/approvals/rules/"+r.ID, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("delete failed %v", err)
	}
	if code, _ := e.post("/api/approvals/rules", Rule{Action: "maybe"}); code != 400 {
		t.Fatal("bad action accepted")
	}
	resp, _ = http.Get(e.srv.URL + "/api/approvals/hooks-snippet")
	var snip map[string]map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&snip)
	resp.Body.Close()
	pr := snip["claude"]["PermissionRequest"].([]any)[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	if pr["type"] != "http" || pr["timeout"].(float64) != 150 || !strings.HasSuffix(pr["url"].(string), "/api/approvals/hook/claude-permission") {
		t.Fatalf("claude snippet %v", pr)
	}
	if _, ok := snip["codex"]["hooks"].(map[string]any)["PermissionRequest"]; !ok {
		t.Fatalf("codex snippet %v", snip["codex"])
	}
}

func TestApprovalRiskClassify(t *testing.T) {
	cases := []struct{ tool, cmd, want string }{
		{"Bash", "git status && git diff", "low"},
		{"Bash", "ls -la | rg foo", "low"},
		{"Bash", "rm -rf /tmp/x", "high"},
		{"Bash", "git push --force origin main", "high"},
		{"Bash", "kubectl -n x delete pod y", "high"},
		{"Bash", "gcloud compute instances delete vm1", "high"},
		{"Bash", "gcloud logging read x --project my-backend", "high"},
		{"Bash", "curl https://x.dev", "medium"},
		{"Bash", "echo hi > f", "medium"},
		{"Bash", "make build", "medium"},
		{"Read", "/a/b", "low"},
		{"Edit", "/a/b", "medium"},
		{"apply_patch", "x", "medium"},
	}
	for _, c := range cases {
		if got, _ := classifyRisk("permission", c.tool, c.cmd); got != c.want {
			t.Errorf("%s %q = %s, want %s", c.tool, c.cmd, got, c.want)
		}
	}
	patch := "*** Begin Patch\n*** Update File: a/b.go\n@@\n*** Add File: c.go\n*** End Patch"
	if s := approvalSummary("permission", "apply_patch", map[string]any{"command": patch}); s != "a/b.go c.go" {
		t.Errorf("patch summary %q", s)
	}
	if !globMatch("brew install*", "brew install jq") || globMatch("sudo *", "pseudo x") {
		t.Error("glob")
	}
}

func TestOriginGuard(t *testing.T) {
	h := originGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	cases := []struct {
		method, origin, site string
		want                 int
	}{
		{"POST", "", "", 200},
		{"POST", "http://127.0.0.1:7777", "", 200},
		{"POST", "http://localhost:3000", "", 403},
		{"POST", "", "same-site", 403},
		{"POST", "", "cross-site", 403},
		{"DELETE", "https://evil.example", "", 403},
		{"GET", "http://localhost:3000", "", 403},
		{"GET", "", "", 200},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, "http://127.0.0.1:7777/api/approvals/rules", strings.NewReader("{}"))
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		if c.site != "" {
			r.Header.Set("Sec-Fetch-Site", c.site)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s origin=%q site=%q: got %d, want %d", c.method, c.origin, c.site, w.Code, c.want)
		}
	}
}

func TestAlwaysRefusedForCompoundAndHighRisk(t *testing.T) {
	for _, cmd := range []string{"npm test && rm -rf /tmp/x", "echo hi | sh", "git push --force origin main"} {
		a := buildApproval(hookClaudePermission, hookPayload{SessionID: "s", ToolName: "Bash", ToolInput: map[string]any{"command": cmd}})
		if a.AlwaysOK {
			t.Errorf("%q: alwaysOk should be false", cmd)
		}
	}
	long := "echo " + strings.Repeat("a", 400) + "; rm -rf /tmp/x"
	a := buildApproval(hookClaudePermission, hookPayload{SessionID: "s", ToolName: "Bash", ToolInput: map[string]any{"command": long}})
	if a.Risk == "low" || a.AlwaysOK {
		t.Errorf("long compound command rated %s alwaysOk=%v", a.Risk, a.AlwaysOK)
	}
	ok := buildApproval(hookClaudePermission, hookPayload{SessionID: "s", ToolName: "Bash", ToolInput: map[string]any{"command": "npm test --watch=false"}})
	if !ok.AlwaysOK {
		t.Errorf("plain command should allow always")
	}
}

func TestNewlineCommandIsCompound(t *testing.T) {
	a := buildApproval(hookClaudePermission, hookPayload{SessionID: "s", ToolName: "Bash", ToolInput: map[string]any{"command": "git status\nrm victim"}})
	if a.AlwaysOK {
		t.Fatal("a newline-separated command must not be eligible for always rules")
	}
	if a.Risk == "low" {
		t.Fatal("a chained command must not be rated low risk")
	}
	if strings.Contains(a.Summary, "\n") {
		t.Fatal("summary stays on one line")
	}
}

func TestCursorShellApproval(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no Cursor allowlist: the command waits for a decision
	e := newApprovalEnv(t)
	ch := e.startHook("cursor", map[string]any{"hook_event_name": "beforeShellExecution", "conversation_id": "c1",
		"command": "rm -rf build", "cwd": "/w", "workspace_roots": []string{"/w"}})
	a := e.waitPending()
	if a.Agent != "cursor" || a.SessionID != "cursor:c1" || a.ToolName != "Bash" || a.Input["command"] != "rm -rf build" {
		t.Fatalf("approval: %+v", a)
	}
	if code, out := e.post("/api/approvals/"+a.ID+"/decide", map[string]any{"behavior": "deny", "message": "not now"}); code != 200 {
		t.Fatalf("decide: %d %s", code, out)
	}
	r := <-ch
	if r.code != 200 || r.body["permission"] != "deny" || r.body["agent_message"] != "not now" {
		t.Fatalf("reply: %+v", r)
	}
	// Bad JSON must still answer with a permission, since Cursor blocks on an empty reply.
	resp, err := http.Post(e.srv.URL+"/api/approvals/hook/cursor", "application/json", strings.NewReader("{"))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if out["permission"] != "ask" {
		t.Fatalf("bad payload: %v", out)
	}
}

func TestCursorPolicyFollowsCursorLists(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	_ = os.MkdirAll(filepath.Join(home, ".cursor"), 0o755)
	_ = os.WriteFile(filepath.Join(home, ".cursor", "cli-config.json"),
		[]byte(`{"permissions":{"allow":["Shell(git *)","Mcp(**)"],"deny":["Shell(rm -rf *)"]}}`), 0o644)
	cmd := func(c string) map[string]any { return map[string]any{"command": c} }
	for _, tc := range []struct {
		tool string
		in   map[string]any
		want string
	}{
		{"Bash", cmd("git status"), "allow"},
		{"Bash", cmd("rm -rf build"), "deny"},
		{"Bash", cmd("make deploy"), ""},
		{"mcp__github__search", map[string]any{}, "allow"},
	} {
		if got := cursorPolicy(tc.tool, tc.in); got != tc.want {
			t.Fatalf("%s %v: %q, want %q", tc.tool, tc.in, got, tc.want)
		}
	}
	e := newApprovalEnv(t)
	r := <-e.startHook("cursor", map[string]any{"hook_event_name": "beforeShellExecution", "conversation_id": "c2", "command": "git log", "cursor_version": "2026.11.17-f532c45"})
	if r.body["permission"] != "allow" || len(e.s.pending) != 0 {
		t.Fatalf("allowlisted command waited: %+v", r)
	}
}

// The desktop app ignores the CLI lists and its "allow" answer: a call it asks about gets a card, unless Cursor is in front.
func TestCursorDesktopUsesItsOwnRules(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	_ = os.MkdirAll(filepath.Join(home, ".cursor"), 0o755)
	_ = os.WriteFile(filepath.Join(home, ".cursor", "cli-config.json"), []byte(`{"permissions":{"allow":["Mcp(**)"]}}`), 0o644)
	e := newApprovalEnv(t)
	mcp := map[string]any{"hook_event_name": "beforeMCPExecution", "conversation_id": "d1", "mcp_server_name": "agent-monitor",
		"tool_name": "permission_prompt", "tool_input": map[string]any{"x": 1}, "cursor_version": "3.30.14"}
	ch := e.startHook("cursor", mcp)
	a := e.waitPending()
	if a.ToolName != "mcp__agent-monitor__permission_prompt" {
		t.Fatalf("approval: %+v", a)
	}
	e.post("/api/approvals/"+a.ID+"/decide", map[string]any{"behavior": "deny"})
	<-ch
	cursorDesktopInFront = func() bool { return true }
	if r := <-e.startHook("cursor", mcp); r.body["permission"] != "ask" || len(e.s.pending) != 0 {
		t.Fatalf("Cursor in front: %+v", r)
	}
	cursorDesktopRulesFn = func() *cursorDesktopRules {
		return &cursorDesktopRules{UseYoloMode: true, YoloCommandAllowlist: []string{"git status"}, MCPAllowedTools: []string{"agent-monitor:permission_prompt"}}
	}
	if r := <-e.startHook("cursor", mcp); r.body["permission"] != "allow" {
		t.Fatalf("desktop allowlist: %+v", r)
	}
	for cmd, want := range map[string]string{"git status": "allow", "git status; rm -rf /": "", "make": ""} {
		if got := cursorDesktopPolicy("Bash", map[string]any{"command": cmd}); got != want {
			t.Fatalf("%q: %q want %q", cmd, got, want)
		}
	}
}

func TestApprovalHoldIsDefault(t *testing.T) {
	e := newApprovalEnv(t)
	e.s.mu.Lock()
	mode, set := e.s.settings.ApprovalsMode, e.s.settings.ModeSet
	e.s.mu.Unlock()
	if mode != approvalsModeHold || set {
		t.Fatalf("default mode %q set=%v", mode, set)
	}
}

func TestApprovalPassToTerminal(t *testing.T) {
	e := newApprovalEnv(t)
	e.s.reachable = func(string) bool { return true }
	ch := e.startHook(hookClaudePermission, claudeBash("make"))
	a := e.waitPending()
	if !a.Passable {
		t.Fatal("reachable session should be passable")
	}
	if code, body := e.decide(a.ID, map[string]any{"behavior": "terminal"}); code != 200 {
		t.Fatalf("decide %d %s", code, body)
	}
	if r := waitResult(t, ch); r.code != http.StatusNoContent {
		t.Fatalf("hook got %d %v", r.code, r.body)
	}
	if rec := e.s.list()["recent"].([]*Approval); rec[0].State != "passed" {
		t.Fatalf("state %s", rec[0].State)
	}
}

func TestApprovalPassToTerminalNeedsTerminal(t *testing.T) {
	e := newApprovalEnv(t)
	e.s.reachable = func(string) bool { return false }
	ch := e.startHook(hookClaudePermission, claudeBash("make"))
	a := e.waitPending()
	if code, _ := e.decide(a.ID, map[string]any{"behavior": "terminal"}); code != http.StatusConflict {
		t.Fatalf("decide without a terminal = %d", code)
	}
	e.decide(a.ID, map[string]any{"behavior": "deny"})
	waitResult(t, ch)
}

func TestApprovalFocusDuringHoldReleases(t *testing.T) {
	e := newApprovalEnv(t)
	var mu sync.Mutex
	look := false
	e.s.focused = func(string) bool { mu.Lock(); defer mu.Unlock(); return look }
	ch := e.startHook(hookClaudePermission, claudeBash("make"))
	e.waitPending()
	mu.Lock()
	look = true
	mu.Unlock()
	if r := waitResult(t, ch); r.code != http.StatusNoContent {
		t.Fatalf("focus should release to the terminal, got %d", r.code)
	}
	if rec := e.s.list()["recent"].([]*Approval); rec[0].State != "passed" {
		t.Fatalf("state %s", rec[0].State)
	}
}
