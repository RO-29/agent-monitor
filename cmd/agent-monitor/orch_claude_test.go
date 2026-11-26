package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestOrchFakeClaudeProcess is not a test: it is the fake `claude -p` stream-json process.
func TestOrchFakeClaudeProcess(t *testing.T) {
	if os.Getenv("ORCH_FAKE_CLAUDE") != "1" {
		t.Skip("helper process")
	}
	runFakeClaude()
	os.Exit(0)
}

type fakeClaude struct {
	in      chan map[string]any
	out     *json.Encoder
	log     *os.File
	sid     string
	dir     string
	n       int
	pending []map[string]any
}

func runFakeClaude() {
	var args []string
	_ = json.Unmarshal([]byte(os.Getenv("ORCH_FAKE_ARGS")), &args)
	f := &fakeClaude{in: make(chan map[string]any, 64), out: json.NewEncoder(os.Stdout), dir: os.Getenv("ORCH_FAKE_CLAUDE_DIR")}
	if p := os.Getenv("ORCH_FAKE_LOG"); p != "" {
		f.log, _ = os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	}
	f.logf("args %s", strings.Join(args, " "))
	resume := false
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "--session-id":
			f.sid = args[i+1]
		case "--resume":
			f.sid, resume = args[i+1], true
		}
	}
	if resume && !pathExists(f.transcript()) {
		fmt.Fprintln(os.Stderr, "No conversation found with session ID: "+f.sid)
		os.Exit(1)
	}
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) == nil {
				f.logf("in %s", sc.Bytes())
				f.in <- m
			}
		}
		close(f.in)
	}()
	for {
		m, ok := f.next()
		if !ok {
			return
		}
		f.handle(m)
	}
}

func (f *fakeClaude) transcript() string {
	return filepath.Join(f.dir, "projects", "fake", f.sid+".jsonl")
}

func (f *fakeClaude) logf(format string, a ...any) {
	if f.log != nil {
		fmt.Fprintf(f.log, format+"\n", a...)
	}
}

func (f *fakeClaude) next() (map[string]any, bool) {
	if len(f.pending) > 0 {
		m := f.pending[0]
		f.pending = f.pending[1:]
		return m, true
	}
	m, ok := <-f.in
	return m, ok
}

func (f *fakeClaude) send(v map[string]any) {
	if _, ok := v["session_id"]; !ok {
		v["session_id"] = f.sid
	}
	_ = f.out.Encode(v)
}

func ctlReq(m map[string]any) (string, map[string]any) {
	r, _ := m["request"].(map[string]any)
	id, _ := m["request_id"].(string)
	return id, r
}

// waitFor reads until match; control requests are answered, everything else is kept for later.
func (f *fakeClaude) waitFor(match func(map[string]any) bool) map[string]any {
	for {
		m, ok := <-f.in
		if !ok {
			os.Exit(0)
		}
		if match(m) {
			return m
		}
		if m["type"] == "control_request" {
			f.handle(m)
			continue
		}
		f.pending = append(f.pending, m)
	}
}

func (f *fakeClaude) handle(m map[string]any) {
	switch m["type"] {
	case "control_request":
		id, r := ctlReq(m)
		switch r["subtype"] {
		case "initialize":
			f.send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id, "response": map[string]any{"commands": []any{}}}})
		default:
			f.send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": id, "error": "no active turn"}})
		}
	case "user":
		msg, _ := m["message"].(map[string]any)
		text, _ := msg["content"].(string)
		if f.dir != "" && f.sid != "" {
			_ = os.MkdirAll(filepath.Dir(f.transcript()), 0o755)
			_ = os.WriteFile(f.transcript(), []byte("{}\n"), 0o644)
		}
		f.send(map[string]any{"type": "system", "subtype": "init", "model": "claude-fake-1", "permissionMode": "default"})
		f.scenario(text)
	}
}

func (f *fakeClaude) msgID() string {
	f.n++
	return fmt.Sprintf("msg_%d", f.n)
}

func (f *fakeClaude) stream(ev map[string]any) {
	f.send(map[string]any{"type": "stream_event", "event": ev, "parent_tool_use_id": nil})
}

func (f *fakeClaude) text(text string, deltas ...string) {
	id := f.msgID()
	f.stream(map[string]any{"type": "message_start", "message": map[string]any{"id": id}})
	f.stream(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
	for _, d := range deltas {
		f.stream(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": d}})
	}
	f.send(map[string]any{"type": "assistant", "parent_tool_use_id": nil, "message": map[string]any{"id": id, "content": []any{map[string]any{"type": "text", "text": text}}}})
	f.stream(map[string]any{"type": "content_block_stop", "index": 0})
}

func (f *fakeClaude) toolUse(id, name string, input map[string]any) {
	mid := f.msgID()
	f.stream(map[string]any{"type": "message_start", "message": map[string]any{"id": mid}})
	f.stream(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}}})
	f.send(map[string]any{"type": "assistant", "parent_tool_use_id": nil, "message": map[string]any{"id": mid, "content": []any{map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}}}})
}

func (f *fakeClaude) toolResult(id, content string, isErr bool) {
	f.send(map[string]any{"type": "user", "parent_tool_use_id": nil, "message": map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": id, "content": content, "is_error": isErr}}}})
}

func (f *fakeClaude) result(subtype string, isErr bool, text string, cost float64) {
	f.send(map[string]any{"type": "result", "subtype": subtype, "is_error": isErr, "result": text, "total_cost_usd": cost,
		"usage":                         map[string]any{"input_tokens": 10, "cache_creation_input_tokens": 100, "cache_read_input_tokens": 1000, "output_tokens": 30},
		"model_usage_since_last_result": map[string]any{"claude-fake-1": map[string]any{"costUSD": cost}}})
}

// permission asks the host and returns its can_use_tool answer.
func (f *fakeClaude) permission(toolUseID, name string, input map[string]any) map[string]any {
	reqID := "perm-" + toolUseID
	f.send(map[string]any{"type": "control_request", "request_id": reqID, "request": map[string]any{
		"subtype": "can_use_tool", "tool_name": name, "input": input, "tool_use_id": toolUseID,
		"permission_suggestions": []any{map[string]any{"type": "setMode", "mode": "acceptEdits", "destination": "session"}}}})
	m := f.waitFor(func(m map[string]any) bool {
		r, _ := m["response"].(map[string]any)
		return m["type"] == "control_response" && r["request_id"] == reqID
	})
	r := m["response"].(map[string]any)
	resp, _ := r["response"].(map[string]any)
	return resp
}

func (f *fakeClaude) scenario(text string) {
	bashCmd, isBash := strings.CutPrefix(text, "bash ")
	if isBash {
		text = "approve"
	}
	switch text {
	case "approve":
		in := map[string]any{"command": "touch x", "description": "make x"}
		if isBash {
			in["command"] = bashCmd
		}
		f.toolUse("tu1", "Bash", in)
		resp := f.permission("tu1", "Bash", in)
		if resp["behavior"] == "allow" {
			f.toolResult("tu1", "", false)
			f.text("decision=allow")
		} else {
			f.toolResult("tu1", fmt.Sprint(resp["message"]), true)
			f.text("decision=deny:" + fmt.Sprint(resp["message"]))
		}
		f.result("success", false, "", 0.002)
	case "ask":
		in := map[string]any{"questions": []any{map[string]any{"question": "Favourite colour?", "header": "Colour", "multiSelect": false,
			"options": []any{map[string]any{"label": "Red", "description": ""}, map[string]any{"label": "Blue", "description": ""}}}}}
		f.toolUse("tq", "AskUserQuestion", in)
		resp := f.permission("tq", "AskUserQuestion", in)
		upd, _ := resp["updatedInput"].(map[string]any)
		b, _ := json.Marshal(upd["answers"])
		f.toolResult("tq", "answered", false)
		f.text(fmt.Sprintf("%v answers=%s", resp["behavior"], b))
		f.result("success", false, "", 0.001)
	case "plan":
		in := map[string]any{"plan": "## Plan\n1. do it"}
		f.toolUse("tp", "ExitPlanMode", in)
		resp := f.permission("tp", "ExitPlanMode", in)
		if resp["behavior"] == "allow" {
			upd, _ := resp["updatedInput"].(map[string]any)
			f.send(map[string]any{"type": "system", "subtype": "status", "status": nil, "permissionMode": "default"})
			f.toolResult("tp", "User has approved your plan.", false)
			f.text(fmt.Sprintf("approved plan=%v", upd["plan"]))
		} else {
			f.toolResult("tp", fmt.Sprint(resp["message"]), true)
			f.text("keep:" + fmt.Sprint(resp["message"]))
		}
		f.result("success", false, "", 0.001)
	case "hang", "steer":
		f.text("working")
		m := f.waitFor(func(m map[string]any) bool {
			_, r := ctlReq(m)
			return m["type"] == "user" || (m["type"] == "control_request" && r["subtype"] == "interrupt")
		})
		if m["type"] == "control_request" {
			id, _ := ctlReq(m)
			f.send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id, "response": map[string]any{"still_queued": []any{}}}})
			f.send(map[string]any{"type": "user", "parent_tool_use_id": nil, "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "[Request interrupted by user]"}}}})
			f.send(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "terminal_reason": "aborted_tools", "total_cost_usd": 0.001})
			return
		}
		msg, _ := m["message"].(map[string]any)
		f.text(fmt.Sprintf("steered: %v", msg["content"]))
		f.result("success", false, "", 0.001)
	case "crash":
		fmt.Fprintln(os.Stderr, "starting turn")
		fmt.Fprintln(os.Stderr, "boom: fatal error in fake claude")
		os.Exit(3)
	case "fail":
		f.send(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "errors": []any{"model refused"}, "total_cost_usd": 0})
	default:
		f.text("pong", "po", "ng")
		f.toolUse("tb", "Bash", map[string]any{"command": "ls", "description": "list"})
		f.toolResult("tb", "file1\n", false)
		f.toolUse("te", "Edit", map[string]any{"file_path": "/tmp/a.go", "old_string": "old", "new_string": "new\nmore"})
		f.toolResult("te", "ok", false)
		f.toolUse("tw", "WebFetch", map[string]any{"url": "https://example.com", "prompt": "x"})
		f.toolResult("tw", "page", false)
		f.toolUse("tt", "TodoWrite", map[string]any{"todos": []any{map[string]any{"content": "a", "status": "pending", "activeForm": "doing a"}}})
		f.toolResult("tt", "ok", false)
		f.result("success", false, "pong", 0.0125)
	}
}

// ── harness ──

func (e *orchEnv) claudeDir() string { return filepath.Join(e.dir, "claude") }

func (e *orchEnv) fakeClaudeConfig() orchClaudeConfig {
	return orchClaudeConfig{
		Command: func(args []string) *exec.Cmd {
			b, _ := json.Marshal(args)
			cmd := exec.Command(os.Args[0], "-test.run=^TestOrchFakeClaudeProcess$")
			cmd.Env = append(os.Environ(), "ORCH_FAKE_CLAUDE=1", "ORCH_FAKE_LOG="+e.log, "ORCH_FAKE_ARGS="+string(b), "ORCH_FAKE_CLAUDE_DIR="+e.claudeDir())
			return cmd
		},
		SessionExists: func(native string) bool {
			return pathExists(filepath.Join(e.claudeDir(), "projects", "fake", native+".jsonl"))
		},
	}
}

func (e *orchEnv) createClaude(prompt, mode string) *OThread {
	e.t.Helper()
	th, err := e.m.CreateThread(orchCreate{Agent: "claude", Cwd: e.cwd, Prompt: prompt, PermissionMode: mode})
	if err != nil {
		e.t.Fatal(err)
	}
	return th
}

func (e *orchEnv) pending(id string) *Approval {
	e.appr.mu.Lock()
	defer e.appr.mu.Unlock()
	if p := e.appr.pending[id]; p != nil {
		return copyApproval(p.a)
	}
	return nil
}

// claudeRequestRun creates a thread, waits for its request of kind, lets decide answer it and waits for the run end.
func (e *orchEnv) claudeRequestRun(prompt, kind string, decide func(a *Approval)) *OThreadDetail {
	e.t.Helper()
	th := e.createClaude(prompt, "")
	if decide != nil {
		opened := e.wait("request.opened", nil)
		if field(opened, "request", "kind") != kind {
			e.t.Fatalf("request %v", opened)
		}
		e.wait("thread.updated", func(d map[string]any) bool { return field(d, "thread", "status") == "waiting" })
		a := e.pending(field(opened, "request", "approvalId"))
		if a == nil || a.Agent != "claude" || a.DrivenID != th.ID || a.SessionID != "claude:"+th.NativeID {
			e.t.Fatalf("pending approval %+v", a)
		}
		decide(a)
		e.wait("request.closed", nil)
	}
	e.waitRunEnd()
	return e.detail(th.ID)
}

func (e *orchEnv) decide(id string, d approvalDecision) {
	e.t.Helper()
	if _, code, err := e.appr.decide(id, d); err != nil {
		e.t.Fatalf("decide %d %v", code, err)
	}
}

// ── tests ──

func TestOrchClaudeTurnWithDeltasAndTools(t *testing.T) {
	e := newOrchEnv(t)
	th := e.createClaude("hello", "")
	if th.Agent != "claude" || th.PermissionMode != "manual" || len(th.NativeID) != 36 || th.Sandbox != "" {
		t.Fatalf("created %+v", th)
	}
	if e.m.DrivenID(th.NativeID) != th.ID {
		t.Fatal("native id not indexed at create")
	}
	e.wait("item.delta", nil)
	run := e.waitRunEnd()
	if field(run, "run", "status") != "completed" {
		t.Fatalf("run %v", run)
	}
	e.wait("thread.updated", func(d map[string]any) bool { return field(d, "thread", "status") == "idle" })
	d := e.detail(th.ID)
	if d.Thread.Model != "claude-fake-1" || d.Thread.Status != "idle" {
		t.Fatalf("thread %+v", d.Thread)
	}
	if a := itemOf(d, "agent"); a == nil || a.Text != "pong" || a.Status != "completed" {
		t.Fatalf("agent %+v", a)
	}
	c := itemOf(d, "command")
	if c == nil || c.Command != "ls" || c.Text != "list" || c.Output != "file1\n" || c.ExitCode == nil || *c.ExitCode != 0 || c.Status != "completed" || c.Cwd != e.cwd {
		t.Fatalf("command %+v", c)
	}
	f := itemOf(d, "fileChange")
	if f == nil || len(f.Files) != 1 || f.Files[0].Path != "/tmp/a.go" || f.Files[0].Add != 2 || f.Files[0].Del != 1 {
		t.Fatalf("file %+v", f)
	}
	w := itemOf(d, "web")
	if w == nil || w.Command != "WebFetch" || w.Text != "https://example.com" || w.Output != "page" {
		t.Fatalf("web %+v", w)
	}
	p := itemOf(d, "plan")
	if p == nil || !strings.Contains(p.Text, `"todos"`) || !strings.Contains(p.Text, `"activeForm":"doing a"`) {
		t.Fatalf("plan %+v", p)
	}
	u := d.Runs[0].Usage
	if u == nil || u.InTokens != 1110 || u.OutTokens != 30 || u.CostUSD != 0.0125 {
		t.Fatalf("usage %+v", u)
	}
	lg := e.fakeLog()
	for _, want := range []string{"--input-format stream-json", "--permission-prompt-tool stdio", "--include-partial-messages", "--session-id " + th.NativeID, `"subtype":"initialize"`} {
		if !strings.Contains(lg, want) {
			t.Fatalf("fake log lacks %q:\n%s", want, lg)
		}
	}
	if !strings.Contains(lg, "--permission-mode default") || strings.Contains(lg, "--model") {
		t.Fatalf("manual mode must pass --permission-mode default and the default model no flag:\n%s", lg)
	}
	if !strings.Contains(lg, `--settings {"permissions":{"ask":["Bash","Write","Edit","MultiEdit","NotebookEdit","WebFetch","WebSearch"`) {
		t.Fatalf("manual mode must ask for write tools over user allow rules:\n%s", lg)
	}
}

func TestOrchClaudePermissionAllowed(t *testing.T) {
	e := newOrchEnv(t)
	d := e.claudeRequestRun("approve", "command", func(a *Approval) {
		if a.ToolName != "Bash" || a.Summary != "touch x" || a.Kind != "permission" || len(a.Suggestions) != 1 {
			t.Fatalf("approval %+v", a)
		}
		e.decide(a.ID, approvalDecision{Behavior: "allow"})
	})
	if got := lastAgentText(d); got != "decision=allow" {
		t.Fatalf("agent saw %q", got)
	}
	if c := itemOf(d, "command"); c.Status != "completed" {
		t.Fatalf("command %+v", c)
	}
	if len(d.Requests) != 1 || d.Requests[0].State != "closed" || !strings.HasSuffix(d.Requests[0].ItemID, ".tu1") {
		t.Fatalf("requests %+v", d.Requests)
	}
	if !strings.Contains(e.fakeLog(), `"updatedInput":{"command":"touch x","description":"make x"}`) {
		t.Fatalf("allow must echo the input:\n%s", e.fakeLog())
	}
}

func TestOrchClaudePermissionDenied(t *testing.T) {
	e := newOrchEnv(t)
	d := e.claudeRequestRun("approve", "command", func(a *Approval) {
		e.decide(a.ID, approvalDecision{Behavior: "deny", Message: "not now"})
	})
	if got := lastAgentText(d); got != "decision=deny:not now" {
		t.Fatalf("agent saw %q", got)
	}
	if c := itemOf(d, "command"); c.Status != "declined" {
		t.Fatalf("command %+v", c)
	}
}

func TestOrchClaudePermissionRule(t *testing.T) {
	e := newOrchEnv(t)
	if _, err := e.appr.upsertRule(Rule{Action: "allow", Agent: "claude", Tool: "Bash", Pattern: "touch *"}); err != nil {
		t.Fatal(err)
	}
	d := e.claudeRequestRun("approve", "", nil)
	if got := lastAgentText(d); got != "decision=allow" || len(d.Requests) != 0 {
		t.Fatalf("rule: agent saw %q, requests %+v", got, d.Requests)
	}
	if recent := e.appr.list()["recent"].([]*Approval); recent[0].State != "auto-allowed" || recent[0].Agent != "claude" {
		t.Fatalf("audit %+v", recent[0])
	}
}

func TestOrchClaudeQuestion(t *testing.T) {
	e := newOrchEnv(t)
	d := e.claudeRequestRun("ask", "question", func(a *Approval) {
		if a.Kind != "question" || len(a.Questions) != 1 || a.Questions[0]["question"] != "Favourite colour?" {
			t.Fatalf("approval %+v", a)
		}
		e.decide(a.ID, approvalDecision{Behavior: "allow", Answers: map[string]string{"Favourite colour?": "Blue"}})
	})
	if got := lastAgentText(d); got != `allow answers={"Favourite colour?":"Blue"}` {
		t.Fatalf("agent saw %q", got)
	}
}

func TestOrchClaudePlanApprovedAndRejected(t *testing.T) {
	e := newOrchEnv(t)
	d := e.claudeRequestRun("plan", "plan", func(a *Approval) {
		if a.Kind != "plan" || a.Plan != "## Plan\n1. do it" {
			t.Fatalf("approval %+v", a)
		}
		e.decide(a.ID, approvalDecision{Behavior: "allow", UpdatedInput: a.Input})
	})
	if got := lastAgentText(d); got != "approved plan=## Plan\n1. do it" {
		t.Fatalf("agent saw %q", got)
	}
	if d.Thread.PermissionMode != "manual" {
		t.Fatalf("mode %q", d.Thread.PermissionMode)
	}

	e2 := newOrchEnv(t)
	d2 := e2.claudeRequestRun("plan", "plan", func(a *Approval) {
		e2.decide(a.ID, approvalDecision{Behavior: "deny", Message: "add tests"})
	})
	if got := lastAgentText(d2); got != "keep:add tests" {
		t.Fatalf("agent saw %q", got)
	}
}

func TestOrchClaudeTimeout(t *testing.T) {
	e := newOrchEnv(t)
	e.m.holdFor = func() time.Duration { return 200 * time.Millisecond }
	d := e.claudeRequestRun("approve", "", nil)
	if got := lastAgentText(d); got != "decision=deny:no decision in time" || len(d.Requests) != 1 {
		t.Fatalf("agent saw %q requests %+v", got, d.Requests)
	}
}

func TestOrchClaudeInterrupt(t *testing.T) {
	e := newOrchEnv(t)
	th := e.createClaude("hang", "")
	e.wait("item.completed", func(d map[string]any) bool { return field(d, "item", "kind") == "agent" })
	if err := e.m.Interrupt(th.ID); err != nil {
		t.Fatal(err)
	}
	if run := e.waitRunEnd(); field(run, "run", "status") != "interrupted" {
		t.Fatalf("run %v", run)
	}
	if err := e.m.Interrupt(th.ID); err == nil {
		t.Fatal("interrupt with no turn must fail")
	}
}

func TestOrchClaudeSteer(t *testing.T) {
	e := newOrchEnv(t)
	th := e.createClaude("steer", "acceptEdits")
	e.wait("item.completed", func(d map[string]any) bool { return field(d, "item", "kind") == "agent" })
	mode, err := e.m.Send(th.ID, "also do y")
	if err != nil || mode != "steer" {
		t.Fatalf("steer mode=%q err=%v", mode, err)
	}
	e.waitRunEnd()
	d := e.detail(th.ID)
	if got := lastAgentText(d); got != "steered: also do y" || len(d.Runs) != 1 {
		t.Fatalf("agent saw %q runs %d", got, len(d.Runs))
	}
	if !strings.Contains(e.fakeLog(), "--permission-mode acceptEdits") {
		t.Fatalf("no permission mode flag:\n%s", e.fakeLog())
	}
	if !strings.Contains(e.fakeLog(), `--settings {"permissions":{"ask":["Bash","WebFetch","WebSearch"`) {
		t.Fatalf("acceptEdits must still ask for Bash:\n%s", e.fakeLog())
	}
}

func TestOrchClaudeCrashAndResume(t *testing.T) {
	e := newOrchEnv(t)
	th := e.createClaude("crash", "")
	if run := e.waitRunEnd(); field(run, "run", "status") != "failed" {
		t.Fatalf("run %v", run)
	}
	up := e.wait("thread.updated", func(d map[string]any) bool { return field(d, "thread", "status") == "error" })
	if msg := field(up, "thread", "error"); !strings.Contains(msg, "boom: fatal error in fake claude") || !strings.Contains(msg, "exit status 3") {
		t.Fatalf("error %q", msg)
	}
	if _, err := e.m.Send(th.ID, "hello"); err != nil {
		t.Fatal(err)
	}
	if run := e.waitRunEnd(); field(run, "run", "status") != "completed" {
		t.Fatalf("run after crash %v", run)
	}
	if !strings.Contains(e.fakeLog(), "--resume "+th.NativeID) {
		t.Fatalf("no resume:\n%s", e.fakeLog())
	}
	if d := e.detail(th.ID); d.Thread.Status != "idle" || d.Thread.Error != "" {
		t.Fatalf("thread after recovery %+v", d.Thread)
	}
}

func TestOrchClaudeFailedTurnAndRestart(t *testing.T) {
	e := newOrchEnv(t)
	th := e.createClaude("fail", "")
	if run := e.waitRunEnd(); field(run, "run", "status") != "failed" {
		t.Fatalf("run %v", run)
	}
	if it := itemOf(e.detail(th.ID), "error"); it == nil || it.Text != "model refused" {
		t.Fatalf("error item %+v", it)
	}
	e.m.Shutdown()
	e.m = e.manager()
	if e.m.DrivenID(th.NativeID) != th.ID {
		t.Fatal("native index not rebuilt")
	}
	if _, err := e.m.Send(th.ID, "hello again"); err != nil {
		t.Fatal(err)
	}
	if run := e.waitRunEnd(); field(run, "run", "status") != "completed" {
		t.Fatalf("run %v", run)
	}
	if strings.Count(e.fakeLog(), "--resume "+th.NativeID) != 1 {
		t.Fatalf("log:\n%s", e.fakeLog())
	}
}

func TestOrchContinue(t *testing.T) {
	e := newOrchEnv(t)
	watched := map[string]*orchWatched{
		"claude:live-1":  {Agent: "claude", NativeID: "live-1", Cwd: e.cwd, Title: "busy", Live: true},
		"claude:old-1":   {Agent: "claude", NativeID: "11111111-2222-4333-8444-555555555555", Cwd: e.cwd, Title: "old work"},
		"codex:thr_old":  {Agent: "codex", NativeID: "thr_old", Cwd: e.cwd, Title: "codex work"},
		"claude:missing": {Agent: "claude", NativeID: "x", Cwd: "/nope/gone"},
	}
	e.m.watched = func(id string) (*orchWatched, error) {
		if w, ok := watched[id]; ok {
			return w, nil
		}
		return nil, errOrchNotFound
	}
	native := watched["claude:old-1"].NativeID
	_ = os.MkdirAll(filepath.Join(e.claudeDir(), "projects", "fake"), 0o755)
	_ = os.WriteFile(filepath.Join(e.claudeDir(), "projects", "fake", native+".jsonl"), []byte("{}\n"), 0o644)

	mux := http.NewServeMux()
	e.m.routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	post := func(body string) (int, map[string]any) {
		resp, err := http.Post(srv.URL+"/api/orch/continue", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if code, out := post(`{"sessionId":"claude:live-1"}`); code != 409 || out["error"] != "live" {
		t.Fatalf("live: %d %v", code, out)
	}
	if code, _ := post(`{"sessionId":"claude:nope"}`); code != 404 {
		t.Fatalf("unknown: %d", code)
	}
	if code, _ := post(`{"sessionId":"claude:missing"}`); code != 400 {
		t.Fatalf("missing cwd: %d", code)
	}
	code, out := post(`{"sessionId":"claude:old-1"}`)
	if code != 201 || field(out, "thread", "nativeId") != native || field(out, "thread", "title") != "old work" ||
		field(out, "thread", "status") != "idle" || field(out, "thread", "agent") != "claude" {
		t.Fatalf("continue: %d %v", code, out)
	}
	id := field(out, "thread", "id")
	code, out = post(`{"sessionId":"claude:old-1","prompt":"hello"}`)
	if code != 200 || field(out, "thread", "id") != id {
		t.Fatalf("idempotent: %d %v", code, out)
	}
	if run := e.waitRunEnd(); field(run, "run", "status") != "completed" {
		t.Fatalf("run %v", run)
	}
	if !strings.Contains(e.fakeLog(), "--resume "+native) {
		t.Fatalf("no resume:\n%s", e.fakeLog())
	}
	old := orchMgr
	orchMgr = e.m
	defer func() { orchMgr = old }()
	if orchDrivenFor([]string{"claude:" + native}) != id {
		t.Fatal("watched claude session not marked driven")
	}

	code, out = post(`{"sessionId":"codex:thr_old","prompt":"hello"}`)
	if code != 201 || field(out, "thread", "agent") != "codex" || field(out, "thread", "nativeId") != "thr_old" {
		t.Fatalf("codex continue: %d %v", code, out)
	}
	e.waitRunEnd()
	if !strings.Contains(e.fakeLog(), `"threadId":"thr_old"`) || !strings.Contains(e.fakeLog(), "thread/resume") {
		t.Fatalf("codex did not resume:\n%s", e.fakeLog())
	}
}

func TestOrchClaudeHookSkip(t *testing.T) {
	e := newOrchEnv(t)
	th := e.createClaude("hang", "")
	e.hanging(th.ID)
	defer func() { _ = e.m.Interrupt(th.ID) }()
	env := newApprovalEnv(t)
	oldFn := drivenSessionFn
	drivenSessionFn = e.m.DrivenLive
	defer func() { drivenSessionFn = oldFn }()
	for _, hook := range []string{"claude-permission", "claude-pretool"} {
		resp, err := http.Post(env.srv.URL+"/api/approvals/hook/"+hook, "application/json",
			strings.NewReader(`{"session_id":"`+th.NativeID+`","cwd":"/tmp","hook_event_name":"PermissionRequest","tool_name":"Bash","tool_input":{"command":"rm -rf x"}}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent || len(env.s.list()["pending"].([]*Approval)) != 0 {
			t.Fatalf("%s: driven hook must pass through: %d", hook, resp.StatusCode)
		}
	}
}

func TestClaudeArgsAskRulesBeatUserAllow(t *testing.T) {
	cfg, cwd := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	os.WriteFile(filepath.Join(cfg, "settings.json"), []byte(`{"permissions":{"allow":["Bash","Write","mcp__agent-monitor","mcp__grafana","mcp__*","mcp__db__query"]}}`), 0o600)
	os.MkdirAll(filepath.Join(cwd, ".claude"), 0o700)
	os.WriteFile(filepath.Join(cwd, ".claude", "settings.local.json"), []byte(`{"permissions":{"allow":["mcp__grafana","mcp__gh__*"]}}`), 0o600)
	settingsOf := func(args []string) map[string][]string {
		var out map[string][]string
		for i, a := range args {
			if a == "--settings" {
				if out != nil {
					t.Fatalf("more than one --settings: %v", args)
				}
				var s struct{ Permissions map[string][]string }
				if err := json.Unmarshal([]byte(args[i+1]), &s); err != nil {
					t.Fatal(err)
				}
				out = s.Permissions
			}
		}
		return out
	}
	cases := []struct {
		mode, ws string
		ask      []string
	}{
		{"manual", "", []string{"Bash", "Write", "Edit", "MultiEdit", "NotebookEdit", "WebFetch", "WebSearch", "mcp__grafana", "mcp__db__query", "mcp__gh__*"}},
		{"", "", []string{"Bash", "Write", "Edit", "MultiEdit", "NotebookEdit", "WebFetch", "WebSearch", "mcp__grafana", "mcp__db__query", "mcp__gh__*"}},
		{"acceptEdits", "", []string{"Bash", "WebFetch", "WebSearch", "mcp__grafana", "mcp__db__query", "mcp__gh__*"}},
		{"manual", "read-only", []string{"Bash"}},
		{"auto", "", nil},
		{"plan", "", nil},
	}
	for _, c := range cases {
		args := claudeArgs(&OThread{NativeID: "n", Cwd: cwd, PermissionMode: c.mode, Workspace: c.ws}, false, nil)
		got := settingsOf(args)["ask"]
		if strings.Join(got, ",") != strings.Join(c.ask, ",") {
			t.Fatalf("mode %q ws %q: ask %v, want %v", c.mode, c.ws, got, c.ask)
		}
	}
}
