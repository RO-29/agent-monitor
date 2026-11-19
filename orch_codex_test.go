package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestOrchFakeCodexProcess is not a test: it is the fake app-server the adapter tests re-exec.
func TestOrchFakeCodexProcess(t *testing.T) {
	if os.Getenv("ORCH_FAKE_CODEX") != "1" {
		t.Skip("helper process")
	}
	runFakeCodex()
	os.Exit(0)
}

type fakeCodex struct {
	in     chan rpcMsg
	out    *json.Encoder
	log    *os.File
	thread string
	turns  int
}

func runFakeCodex() {
	f := &fakeCodex{in: make(chan rpcMsg, 64), out: json.NewEncoder(os.Stdout), thread: "thr_fake_1"}
	if p := os.Getenv("ORCH_FAKE_LOG"); p != "" {
		f.log, _ = os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		fmt.Fprintf(f.log, "codex args %s env AM_ORCH_THREAD=%s\n", os.Getenv("ORCH_FAKE_ARGS"), os.Getenv("AM_ORCH_THREAD"))
	}
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var m rpcMsg
			if json.Unmarshal(sc.Bytes(), &m) == nil {
				if f.log != nil {
					fmt.Fprintf(f.log, "%s %s %s\n", m.Method, m.ID, m.Params)
				}
				f.in <- m
			}
		}
		close(f.in)
	}()
	for m := range f.in {
		f.handle(m)
	}
}

func (f *fakeCodex) send(v map[string]any) { _ = f.out.Encode(v) }

func (f *fakeCodex) notify(method string, params map[string]any) {
	params["threadId"] = f.thread
	f.send(map[string]any{"method": method, "params": params})
}

// waitFor reads messages until one matches; others are handled normally.
func (f *fakeCodex) waitFor(match func(rpcMsg) bool) rpcMsg {
	for m := range f.in {
		if match(m) {
			return m
		}
		f.handle(m)
	}
	os.Exit(0)
	return rpcMsg{}
}

func (f *fakeCodex) handle(m rpcMsg) {
	var p map[string]any
	_ = json.Unmarshal(m.Params, &p)
	reply := func(res any) { f.send(map[string]any{"id": m.ID, "result": res}) }
	switch m.Method {
	case "initialize":
		reply(map[string]any{"codexHome": "/tmp/fake", "platformFamily": "unix", "platformOs": "macos", "userAgent": "fake/0"})
	case "initialized":
	case "thread/start":
		reply(map[string]any{"thread": map[string]any{"id": f.thread}, "model": "fake-model", "cwd": p["cwd"], "sandbox": map[string]any{"type": "readOnly"}})
	case "thread/resume":
		f.thread, _ = p["threadId"].(string)
		reply(map[string]any{"thread": map[string]any{"id": f.thread}, "model": "fake-model"})
	case "turn/start":
		f.turns++
		turn := fmt.Sprintf("turn_%d", f.turns)
		reply(map[string]any{"turn": map[string]any{"id": turn, "items": []any{}, "status": "inProgress"}})
		text := p["input"].([]any)[0].(map[string]any)["text"].(string)
		f.scenario(turn, text)
	case "turn/interrupt", "turn/steer":
		f.send(map[string]any{"id": m.ID, "error": map[string]any{"code": -32600, "message": "no active turn"}})
	default:
		if m.Method != "" && len(m.ID) > 0 {
			reply(map[string]any{})
		}
	}
}

func (f *fakeCodex) agent(turn, id, text string, deltas ...string) {
	f.notify("item/started", map[string]any{"turnId": turn, "startedAtMs": 1, "item": map[string]any{"type": "agentMessage", "id": id, "text": ""}})
	for _, d := range deltas {
		f.notify("item/agentMessage/delta", map[string]any{"turnId": turn, "itemId": id, "delta": d})
	}
	f.notify("item/completed", map[string]any{"turnId": turn, "completedAtMs": 2, "item": map[string]any{"type": "agentMessage", "id": id, "text": text}})
}

func (f *fakeCodex) done(turn, status string) {
	f.notify("turn/completed", map[string]any{"turn": map[string]any{"id": turn, "items": []any{}, "status": status}})
}

func (f *fakeCodex) scenario(turn, text string) {
	f.notify("turn/started", map[string]any{"turn": map[string]any{"id": turn, "items": []any{}, "status": "inProgress"}})
	f.notify("item/started", map[string]any{"turnId": turn, "item": map[string]any{"type": "userMessage", "id": "u" + turn, "content": []any{}}})
	if strings.HasPrefix(text, orchReviewMarker) {
		if f.log != nil {
			fmt.Fprintf(f.log, "review prompt %q\n", text)
		}
		reply := os.Getenv("ORCH_FAKE_REVIEW")
		if reply == "" {
			reply = "Looks mostly fine.\n\n```json\n{\"summary\": \"One real bug.\", \"findings\": [{\"severity\": \"major\", \"file\": \"b.txt\", \"line\": \"3\", \"title\": \"Off by one\", \"body\": \"Loop ends early.\"},]}\n```"
		}
		f.agent(turn, "rv", reply)
		f.done(turn, "completed")
		return
	}
	switch text {
	case "approve":
		cmd := map[string]any{"type": "commandExecution", "id": "c2", "command": "touch x", "cwd": "/tmp", "commandActions": []any{}, "status": "inProgress"}
		f.notify("item/started", map[string]any{"turnId": turn, "item": cmd})
		f.send(map[string]any{"id": "srv-1", "method": "item/commandExecution/requestApproval", "params": map[string]any{
			"threadId": f.thread, "turnId": turn, "itemId": "c2", "command": "touch x", "cwd": "/tmp", "startedAtMs": 1}})
		resp := f.waitFor(func(m rpcMsg) bool { return m.Method == "" && string(m.ID) == `"srv-1"` })
		var r struct {
			Decision string `json:"decision"`
		}
		_ = json.Unmarshal(resp.Result, &r)
		cmd["status"] = "completed"
		if r.Decision != "accept" {
			cmd["status"] = "declined"
		}
		f.notify("serverRequest/resolved", map[string]any{"requestId": "srv-1"})
		f.notify("item/completed", map[string]any{"turnId": turn, "item": cmd})
		f.agent(turn, "m2", "decision="+r.Decision)
		f.done(turn, "completed")
	case "hang", "steer":
		f.agent(turn, "m1", "working")
		m := f.waitFor(func(m rpcMsg) bool { return m.Method == "turn/interrupt" || m.Method == "turn/steer" })
		var p map[string]any
		_ = json.Unmarshal(m.Params, &p)
		if m.Method == "turn/interrupt" {
			f.send(map[string]any{"id": m.ID, "result": map[string]any{}})
			f.done(turn, "interrupted")
			return
		}
		f.send(map[string]any{"id": m.ID, "result": map[string]any{"turnId": turn}})
		steer := p["input"].([]any)[0].(map[string]any)["text"].(string)
		f.agent(turn, "m3", "steered: "+steer)
		f.done(turn, "completed")
	case "crash":
		fmt.Fprintln(os.Stderr, "starting turn")
		fmt.Fprintln(os.Stderr, "boom: fatal error in fake")
		os.Exit(3)
	case "fail":
		f.notify("error", map[string]any{"turnId": turn, "willRetry": false, "error": map[string]any{"message": "model refused"}})
		f.notify("turn/completed", map[string]any{"turn": map[string]any{"id": turn, "items": []any{}, "status": "failed", "error": map[string]any{"message": "model refused"}}})
	default:
		f.agent(turn, "m1", "pong", "po", "ng")
		cmd := map[string]any{"type": "commandExecution", "id": "c1", "command": "ls", "cwd": "/tmp", "commandActions": []any{}, "status": "inProgress"}
		f.notify("item/started", map[string]any{"turnId": turn, "item": cmd})
		f.notify("item/commandExecution/outputDelta", map[string]any{"turnId": turn, "itemId": "c1", "delta": "file1\n"})
		cmd["status"], cmd["exitCode"], cmd["aggregatedOutput"] = "completed", 0, "file1\n"
		f.notify("item/completed", map[string]any{"turnId": turn, "item": cmd})
		f.notify("item/started", map[string]any{"turnId": turn, "item": map[string]any{"type": "fileChange", "id": "f1", "status": "inProgress",
			"changes": []any{map[string]any{"path": "/tmp/a.go", "kind": map[string]any{"type": "update"}, "diff": "@@\n-old\n+new\n+more\n"}}}})
		f.notify("item/completed", map[string]any{"turnId": turn, "item": map[string]any{"type": "fileChange", "id": "f1", "status": "completed",
			"changes": []any{map[string]any{"path": "/tmp/a.go", "kind": map[string]any{"type": "update"}, "diff": "@@\n-old\n+new\n+more\n"}}}})
		f.notify("thread/tokenUsage/updated", map[string]any{"turnId": turn, "tokenUsage": map[string]any{
			"total": map[string]any{"inputTokens": 120, "cachedInputTokens": 100, "outputTokens": 30},
			"last":  map[string]any{"inputTokens": 120, "cachedInputTokens": 100, "outputTokens": 30}}})
		f.done(turn, "completed")
	}
}

// ── harness ──

type orchEnv struct {
	t      *testing.T
	dir    string
	store  *OrchStore
	appr   *ApprovalStore
	m      *OrchManager
	events <-chan OEvent
	log    string
	cwd    string
}

func newOrchEnv(t *testing.T) *orchEnv {
	t.Helper()
	dir := t.TempDir()
	store, err := openOrchStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	appr := newApprovalStore(dir)
	appr.focused = func(string) bool { return false }
	e := &orchEnv{t: t, dir: dir, store: store, appr: appr, log: dir + "/fake.log", cwd: t.TempDir()}
	e.events, _ = store.hub.subscribe()
	e.m = e.manager()
	t.Cleanup(func() {
		e.m.Shutdown()
		store.Close()
		if appr.db != nil {
			appr.db.Close()
		}
	})
	return e
}

func (e *orchEnv) manager() *OrchManager {
	m := newOrchManager(e.store, e.appr)
	m.rpcTimeout = 10 * time.Second
	m.command = func(th *OThread, extra []string) *exec.Cmd {
		b, _ := json.Marshal(extra)
		cmd := exec.Command(os.Args[0], "-test.run=^TestOrchFakeCodexProcess$")
		cmd.Env = append(os.Environ(), "ORCH_FAKE_CODEX=1", "ORCH_FAKE_LOG="+e.log, "ORCH_FAKE_ARGS="+string(b))
		return cmd
	}
	m.mcp = func() *orchMCPServer {
		return &orchMCPServer{Command: "/fake/agent-monitor", Args: []string{"mcp-perm-server"}}
	}
	m.daemonURL = "http://127.0.0.1:9"
	m.stateDir = e.dir
	m.claude = e.fakeClaudeConfig()
	m.watched = func(string) (*orchWatched, error) { return nil, errOrchNotFound }
	return m
}

// wait returns the first event matching typ and pred.
func (e *orchEnv) wait(typ string, pred func(map[string]any) bool) map[string]any {
	e.t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-e.events:
			if !ok {
				e.t.Fatal("event channel closed")
			}
			if ev.Type != typ {
				continue
			}
			var data map[string]any
			_ = json.Unmarshal(ev.Data, &data)
			if pred == nil || pred(data) {
				return data
			}
		case <-timeout:
			e.t.Fatalf("timed out waiting for %s", typ)
		}
	}
}

func field(data map[string]any, obj, key string) string {
	if o, ok := data[obj].(map[string]any); ok {
		s, _ := o[key].(string)
		return s
	}
	return ""
}

func (e *orchEnv) create(prompt string) *OThread {
	e.t.Helper()
	th, err := e.m.CreateThread(orchCreate{Agent: "codex", Cwd: e.cwd, Prompt: prompt, Sandbox: "read-only"})
	if err != nil {
		e.t.Fatal(err)
	}
	return th
}

func (e *orchEnv) waitRunEnd() map[string]any {
	return e.wait("run.ended", nil)
}

func (e *orchEnv) detail(id string) *OThreadDetail {
	e.t.Helper()
	d, err := e.store.ThreadDetail(id)
	if err != nil {
		e.t.Fatal(err)
	}
	return d
}

func itemOf(d *OThreadDetail, kind string) *OItem {
	for i := range d.Items {
		if d.Items[i].Kind == kind {
			return &d.Items[i]
		}
	}
	return nil
}

func (e *orchEnv) fakeLog() string {
	b, _ := os.ReadFile(e.log)
	return string(b)
}

// ── adapter tests ──

func TestOrchCodexTurnWithDeltasAndCommand(t *testing.T) {
	e := newOrchEnv(t)
	th := e.create("hello")
	if th.Status != "starting" || th.Title != "hello" {
		t.Fatalf("created thread %+v", th)
	}
	delta := e.wait("item.delta", func(d map[string]any) bool { return d["field"] == nil })
	if !strings.HasSuffix(delta["itemId"].(string), ".m1") {
		t.Fatalf("delta %v", delta)
	}
	run := e.waitRunEnd()
	if field(run, "run", "status") != "completed" {
		t.Fatalf("run %v", run)
	}
	e.wait("thread.updated", func(d map[string]any) bool { return field(d, "thread", "status") == "idle" })
	d := e.detail(th.ID)
	if d.Thread.NativeID != "thr_fake_1" || d.Thread.Model != "fake-model" || d.Thread.Status != "idle" {
		t.Fatalf("thread %+v", d.Thread)
	}
	if u := itemOf(d, "user"); u == nil || u.Text != "hello" {
		t.Fatalf("user item %+v", u)
	}
	if a := itemOf(d, "agent"); a == nil || a.Text != "pong" || a.Status != "completed" {
		t.Fatalf("agent item %+v", a)
	}
	c := itemOf(d, "command")
	if c == nil || c.Command != "ls" || c.Output != "file1\n" || c.ExitCode == nil || *c.ExitCode != 0 || c.Status != "completed" {
		t.Fatalf("command item %+v", c)
	}
	f := itemOf(d, "fileChange")
	if f == nil || len(f.Files) != 1 || f.Files[0].Add != 2 || f.Files[0].Del != 1 || f.Files[0].Kind != "update" {
		t.Fatalf("file item %+v", f)
	}
	if len(d.Runs) != 1 || d.Runs[0].Usage == nil || d.Runs[0].Usage.InTokens != 120 || d.Runs[0].TurnID != "turn_1" {
		t.Fatalf("run %+v", d.Runs)
	}
	// Built-in codex price: 20 fresh in at 1.25, 100 cached at 0.125, 30 out at 10 per 1M.
	if u := d.Runs[0].Usage; u.CachedIn != 100 || !u.Estimated || math.Abs(u.CostUSD-337.5e-6) > 1e-12 {
		t.Fatalf("codex usage cost %+v", u)
	}
	for _, it := range d.Items {
		if strings.HasSuffix(it.ID, ".uturn_1") {
			t.Fatal("codex userMessage must not duplicate the user item")
		}
	}
	lg := e.fakeLog()
	for _, want := range []string{"initialize 1", "initialized", `thread/start 2 {"approvalPolicy":"on-request","cwd":"` + e.cwd + `","sandbox":"read-only"}`, "turn/start 3"} {
		if !strings.Contains(lg, want) {
			t.Fatalf("fake log lacks %q:\n%s", want, lg)
		}
	}
	if e.m.DrivenID("thr_fake_1") != th.ID {
		t.Fatal("native id not indexed")
	}
}

func (e *orchEnv) approvalRun(decide func(approvalID string)) (*OThreadDetail, map[string]any) {
	th := e.create("approve")
	var opened map[string]any
	if decide != nil {
		opened = e.wait("request.opened", nil)
		if field(opened, "request", "kind") != "command" || field(opened, "request", "state") != "open" {
			e.t.Fatalf("request %v", opened)
		}
		e.wait("thread.updated", func(d map[string]any) bool { return field(d, "thread", "status") == "waiting" })
		decide(field(opened, "request", "approvalId"))
		e.wait("request.closed", nil)
	}
	e.waitRunEnd()
	return e.detail(th.ID), opened
}

func lastAgentText(d *OThreadDetail) string {
	for i := len(d.Items) - 1; i >= 0; i-- {
		if d.Items[i].Kind == "agent" {
			return d.Items[i].Text
		}
	}
	return ""
}

func TestOrchCodexApprovalAllowed(t *testing.T) {
	e := newOrchEnv(t)
	d, opened := e.approvalRun(func(id string) {
		e.appr.mu.Lock()
		p := e.appr.pending[id]
		e.appr.mu.Unlock()
		if p == nil || p.a.Agent != "codex" || p.a.ToolName != "shell" || p.a.Summary != "touch x" || p.a.DrivenID == "" || p.a.Risk == "" {
			t.Fatalf("pending approval %+v", p)
		}
		if _, code, err := e.appr.decide(id, approvalDecision{Behavior: "allow"}); err != nil {
			t.Fatalf("decide %d %v", code, err)
		}
	})
	if got := lastAgentText(d); got != "decision=accept" {
		t.Fatalf("agent saw %q", got)
	}
	if c := itemOf(d, "command"); c.Status != "completed" {
		t.Fatalf("command %+v", c)
	}
	if len(d.Requests) != 1 || d.Requests[0].State != "closed" || d.Requests[0].ApprovalID != field(opened, "request", "approvalId") ||
		!strings.HasSuffix(d.Requests[0].ItemID, ".c2") {
		t.Fatalf("requests %+v", d.Requests)
	}
	if d.Thread.Status != "idle" {
		t.Fatalf("thread %s", d.Thread.Status)
	}
}

func TestOrchCodexApprovalDenied(t *testing.T) {
	e := newOrchEnv(t)
	d, _ := e.approvalRun(func(id string) {
		if _, _, err := e.appr.decide(id, approvalDecision{Behavior: "deny", Message: "no"}); err != nil {
			t.Fatal(err)
		}
	})
	if got := lastAgentText(d); got != "decision=decline" {
		t.Fatalf("agent saw %q", got)
	}
	if c := itemOf(d, "command"); c.Status != "declined" {
		t.Fatalf("command %+v", c)
	}
	recent := e.appr.list()["recent"].([]*Approval)
	if len(recent) == 0 || recent[0].State != "denied" {
		t.Fatalf("audit %+v", recent)
	}
}

func TestOrchCodexApprovalRuleAndTimeout(t *testing.T) {
	e := newOrchEnv(t)
	if _, err := e.appr.upsertRule(Rule{Action: "allow", Agent: "codex", Tool: "shell", Pattern: "touch *"}); err != nil {
		t.Fatal(err)
	}
	d, _ := e.approvalRun(nil)
	if got := lastAgentText(d); got != "decision=accept" || len(d.Requests) != 0 {
		t.Fatalf("rule: agent saw %q, requests %+v", got, d.Requests)
	}
	recent := e.appr.list()["recent"].([]*Approval)
	if recent[0].State != "auto-allowed" {
		t.Fatalf("audit %+v", recent[0])
	}

	e2 := newOrchEnv(t)
	e2.m.holdFor = func() time.Duration { return 200 * time.Millisecond }
	d2, _ := e2.approvalRun(nil)
	if got := lastAgentText(d2); got != "decision=decline" || len(d2.Requests) != 1 || d2.Requests[0].State != "closed" {
		t.Fatalf("timeout: agent saw %q, requests %+v", got, d2.Requests)
	}
}

func TestOrchCodexInterrupt(t *testing.T) {
	e := newOrchEnv(t)
	th := e.create("hang")
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

func TestOrchCodexSteer(t *testing.T) {
	e := newOrchEnv(t)
	th := e.create("steer")
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
	users := 0
	for _, it := range d.Items {
		if it.Kind == "user" {
			users++
		}
	}
	if users != 2 {
		t.Fatalf("want 2 user items, got %d", users)
	}
}

func TestOrchCodexCrash(t *testing.T) {
	e := newOrchEnv(t)
	th := e.create("crash")
	if run := e.waitRunEnd(); field(run, "run", "status") != "failed" {
		t.Fatalf("run %v", run)
	}
	up := e.wait("thread.updated", func(d map[string]any) bool { return field(d, "thread", "status") == "error" })
	if msg := field(up, "thread", "error"); !strings.Contains(msg, "boom: fatal error in fake") || !strings.Contains(msg, "exit status 3") {
		t.Fatalf("error %q", msg)
	}
	// The next message respawns and resumes the same native thread.
	if _, err := e.m.Send(th.ID, "hello"); err != nil {
		t.Fatal(err)
	}
	if run := e.waitRunEnd(); field(run, "run", "status") != "completed" {
		t.Fatalf("run after crash %v", run)
	}
	if !strings.Contains(e.fakeLog(), `thread/resume 2 {"approvalPolicy":"on-request","cwd":"`+e.cwd+`","excludeTurns":true,"model":"fake-model","sandbox":"read-only","threadId":"thr_fake_1"}`) {
		t.Fatalf("no resume:\n%s", e.fakeLog())
	}
	if d := e.detail(th.ID); d.Thread.Status != "idle" || d.Thread.Error != "" {
		t.Fatalf("thread after recovery %+v", d.Thread)
	}
}

func TestOrchCodexFailedTurn(t *testing.T) {
	e := newOrchEnv(t)
	th := e.create("fail")
	if run := e.waitRunEnd(); field(run, "run", "status") != "failed" {
		t.Fatalf("run %v", run)
	}
	d := e.detail(th.ID)
	n := 0
	for _, it := range d.Items {
		if it.Kind == "error" {
			n++
			if it.Text != "model refused" {
				t.Fatalf("error item %+v", it)
			}
		}
	}
	if n != 1 {
		t.Fatalf("want one error item, got %d", n)
	}
}

func TestOrchCodexResumeAfterRestart(t *testing.T) {
	e := newOrchEnv(t)
	th := e.create("hello")
	e.waitRunEnd()
	e.wait("thread.updated", func(d map[string]any) bool { return field(d, "thread", "status") == "idle" })
	e.m.Shutdown()

	// Simulate a daemon restart mid-turn: the DB still says running.
	_ = e.store.write(func(t *orchTx) error {
		x, _ := t.thread(th.ID)
		x.Status = "running"
		return t.putThread(x)
	})
	e.m = e.manager()
	if got, _ := e.store.Thread(th.ID); got.Status != "idle" {
		t.Fatalf("after restart status %s", got.Status)
	}
	if e.m.DrivenID("thr_fake_1") != th.ID {
		t.Fatal("native index not rebuilt")
	}
	mode, err := e.m.Send(th.ID, "hello again")
	if err != nil || mode != "turn" {
		t.Fatalf("send mode=%q err=%v", mode, err)
	}
	e.waitRunEnd()
	if strings.Count(e.fakeLog(), "thread/resume") != 1 || !strings.Contains(e.fakeLog(), `"threadId":"thr_fake_1"`) {
		t.Fatalf("log:\n%s", e.fakeLog())
	}
	d := e.detail(th.ID)
	if len(d.Runs) != 2 || d.Runs[1].Index != 1 || d.Runs[1].Status != "completed" {
		t.Fatalf("runs %+v", d.Runs)
	}
}

func TestOrchCodexClose(t *testing.T) {
	e := newOrchEnv(t)
	th := e.create("hang")
	e.wait("item.completed", func(d map[string]any) bool { return field(d, "item", "kind") == "agent" })
	if err := e.m.Close(th.ID); err != nil {
		t.Fatal(err)
	}
	d := e.detail(th.ID)
	if d.Thread.Status != "closed" || d.Runs[0].Status != "interrupted" {
		t.Fatalf("closed thread %+v runs %+v", d.Thread, d.Runs)
	}
	if _, err := e.m.Send(th.ID, "more"); err == nil {
		t.Fatal("send to a closed thread must fail")
	}
}

func TestOrchAPIRoutes(t *testing.T) {
	e := newOrchEnv(t)
	mux := http.NewServeMux()
	e.m.routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	post := func(path, body string) (int, map[string]any) {
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if code, _ := post("/api/orch/threads", `{"agent":"codex","cwd":"/nope/missing","prompt":"x"}`); code != 400 {
		t.Fatalf("missing cwd: %d", code)
	}
	if code, _ := post("/api/orch/threads", `{"agent":"claude","cwd":"`+e.cwd+`","prompt":"x","permissionMode":"yolo"}`); code != 400 {
		t.Fatalf("bad permissionMode: %d", code)
	}
	if code, _ := post("/api/orch/threads", `{"agent":"nosuch","cwd":"`+e.cwd+`","prompt":"x"}`); code != 400 {
		t.Fatalf("unknown agent: %d", code)
	}
	code, out := post("/api/orch/threads", `{"agent":"codex","cwd":"`+e.cwd+`","prompt":"hello"}`)
	if code != 200 {
		t.Fatalf("create %d %v", code, out)
	}
	id := field(out, "thread", "id")
	e.waitRunEnd()
	resp, _ := http.Get(srv.URL + "/api/orch/threads/" + id)
	var d OThreadDetail
	_ = json.NewDecoder(resp.Body).Decode(&d)
	resp.Body.Close()
	if d.Thread == nil || d.Thread.Sandbox != "workspace-write" || len(d.Items) < 3 || d.Seq == 0 {
		t.Fatalf("detail %+v", d)
	}
	resp, _ = http.Get(srv.URL + "/api/orch/threads")
	var list struct {
		Threads []OThread `json:"threads"`
		Seq     int64     `json:"seq"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list.Threads) != 1 || list.Seq < d.Seq {
		t.Fatalf("list %+v", list)
	}
	if code, _ := post("/api/orch/threads/"+id+"/interrupt", ``); code != 409 {
		t.Fatalf("interrupt idle: %d", code)
	}
	if code, _ := post("/api/orch/threads/nope/messages", `{"text":"x"}`); code != 404 {
		t.Fatalf("unknown thread: %d", code)
	}
	if code, out := post("/api/orch/threads/"+id+"/close", ``); code != 200 || field(out, "thread", "status") != "closed" {
		t.Fatalf("close %d %v", code, out)
	}
}

func TestOrchDrivenMergeAndHookSkip(t *testing.T) {
	e := newOrchEnv(t)
	th := e.create("hang")
	e.hanging(th.ID)
	defer func() { _ = e.m.Interrupt(th.ID) }()
	old := orchMgr
	orchMgr = e.m
	defer func() { orchMgr = old }()
	id := orchDrivenFor([]string{"claude:abc", "codex:thr_fake_1"})
	if id == "" {
		t.Fatal("driven session not matched")
	}
	got := withoutDriven([]ThreadV3{{Thread: Thread{ID: "a"}}, {Thread: Thread{ID: "b"}, DrivenID: id}})
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("withoutDriven %+v", got)
	}

	env := newApprovalEnv(t)
	oldFn := drivenSessionFn
	drivenSessionFn = orchMgr.DrivenLive
	defer func() { drivenSessionFn = oldFn }()
	resp, err := http.Post(env.srv.URL+"/api/approvals/hook/codex", "application/json",
		strings.NewReader(`{"session_id":"thr_fake_1","cwd":"/tmp","hook_event_name":"PermissionRequest","tool_name":"shell","tool_input":{"command":"rm -rf x"}}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || len(env.s.list()["pending"].([]*Approval)) != 0 {
		t.Fatalf("driven hook must pass through: %d", resp.StatusCode)
	}
}
