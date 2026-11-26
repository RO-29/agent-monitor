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

// TestOrchFakeACPProcess is not a test: it is the fake ACP agent.
func TestOrchFakeACPProcess(t *testing.T) {
	if os.Getenv("ORCH_FAKE_ACP") != "1" {
		t.Skip("helper process")
	}
	runFakeACP()
	os.Exit(0)
}

type fakeACP struct {
	in      chan rpcMsg
	out     *json.Encoder
	log     *os.File
	dir     string
	sid     string
	nextID  int
	pending []rpcMsg
}

func runFakeACP() {
	f := &fakeACP{in: make(chan rpcMsg, 64), out: json.NewEncoder(os.Stdout), dir: os.Getenv("ORCH_FAKE_ACP_DIR")}
	if p := os.Getenv("ORCH_FAKE_LOG"); p != "" {
		f.log, _ = os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	}
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var m rpcMsg
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

func (f *fakeACP) logf(format string, a ...any) {
	if f.log != nil {
		fmt.Fprintf(f.log, format+"\n", a...)
	}
}

func (f *fakeACP) next() (rpcMsg, bool) {
	if len(f.pending) > 0 {
		m := f.pending[0]
		f.pending = f.pending[1:]
		return m, true
	}
	m, ok := <-f.in
	return m, ok
}

func (f *fakeACP) send(v map[string]any) {
	v["jsonrpc"] = "2.0"
	_ = f.out.Encode(v)
}

func (f *fakeACP) update(u map[string]any) {
	f.send(map[string]any{"method": "session/update", "params": map[string]any{"sessionId": f.sid, "update": u}})
}

func (f *fakeACP) say(kind, text string) {
	f.update(map[string]any{"sessionUpdate": kind, "content": map[string]any{"type": "text", "text": text}})
}

// call sends a request to the client and waits for its response, keeping other input for later.
func (f *fakeACP) call(method string, params map[string]any) rpcMsg {
	f.nextID++
	id := f.nextID
	f.send(map[string]any{"id": id, "method": method, "params": params})
	return f.waitFor(func(m rpcMsg) bool { return m.Method == "" && string(m.ID) == fmt.Sprint(id) })
}

func (f *fakeACP) waitFor(match func(rpcMsg) bool) rpcMsg {
	for {
		m, ok := <-f.in
		if !ok {
			os.Exit(0)
		}
		if match(m) {
			return m
		}
		f.pending = append(f.pending, m)
	}
}

func (f *fakeACP) sessions() string {
	b, _ := os.ReadFile(filepath.Join(f.dir, "sessions"))
	return string(b)
}

func (f *fakeACP) modes() map[string]any {
	modes := []any{map[string]any{"id": "build"}}
	if os.Getenv("ORCH_FAKE_ACP_NOPLAN") != "1" {
		modes = append(modes, map[string]any{"id": "plan"})
	}
	return map[string]any{"currentModeId": "build", "availableModes": modes}
}

func (f *fakeACP) handle(m rpcMsg) {
	var p map[string]any
	_ = json.Unmarshal(m.Params, &p)
	reply := func(res any) { f.send(map[string]any{"id": m.ID, "result": res}) }
	switch m.Method {
	case "initialize":
		reply(map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{"loadSession": os.Getenv("ORCH_FAKE_ACP_LOAD") != "0",
			"promptCapabilities": map[string]any{"image": os.Getenv("ORCH_FAKE_ACP_IMAGE") == "1"}}})
	case "session/new":
		f.sid = fmt.Sprintf("fs_%d", time.Now().UnixNano())
		f2, _ := os.OpenFile(filepath.Join(f.dir, "sessions"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		fmt.Fprintln(f2, f.sid)
		f2.Close()
		reply(map[string]any{"sessionId": f.sid, "models": map[string]any{"currentModelId": "m1", "availableModels": []any{
			map[string]any{"modelId": "m1", "name": "Model One"}, map[string]any{"modelId": "m2", "name": "Model Two"}}},
			"modes": f.modes()})
	case "session/load":
		sid, _ := p["sessionId"].(string)
		if !strings.Contains(f.sessions(), sid+"\n") {
			f.send(map[string]any{"id": m.ID, "error": map[string]any{"code": -32002, "message": "session not found"}})
			return
		}
		f.sid = sid
		f.say("user_message_chunk", "replayed old prompt")
		f.say("agent_message_chunk", "replayed old answer")
		reply(map[string]any{})
	case "session/set_model", "session/set_mode":
		reply(map[string]any{})
	case "session/prompt":
		prompt, _ := p["prompt"].([]any)
		text := ""
		for _, b := range prompt {
			if bm, _ := b.(map[string]any); bm["type"] == "text" {
				text, _ = bm["text"].(string)
			}
		}
		f.scenario(m, text)
	case "session/cancel":
	default:
		if len(m.ID) > 0 && m.Method != "" {
			f.send(map[string]any{"id": m.ID, "error": map[string]any{"code": -32601, "message": "no " + m.Method}})
		}
	}
}

func (f *fakeACP) scenario(m rpcMsg, text string) {
	done := func(reason string, extra map[string]any) {
		res := map[string]any{"stopReason": reason}
		for k, v := range extra {
			res[k] = v
		}
		f.send(map[string]any{"id": m.ID, "result": res})
	}
	verb, arg, _ := strings.Cut(text, " ")
	switch verb {
	case "chunks":
		f.say("agent_thought_chunk", "thinking ")
		f.say("agent_thought_chunk", "hard")
		f.say("agent_message_chunk", "Hel")
		f.say("agent_message_chunk", "lo")
		f.update(map[string]any{"sessionUpdate": "tool_call", "toolCallId": "c1", "title": "ls", "kind": "execute", "status": "pending", "rawInput": map[string]any{"command": "ls -la"}})
		f.update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "c1", "status": "completed",
			"content": []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": "a.txt\nb.txt"}}}, "rawOutput": map[string]any{"exitCode": 0}})
		f.update(map[string]any{"sessionUpdate": "tool_call", "toolCallId": "c2", "title": "Edit a.txt", "kind": "edit", "status": "completed",
			"content": []any{map[string]any{"type": "diff", "path": "/w/a.txt", "oldText": "one\ntwo\n", "newText": "one\n2\nthree\n"},
				map[string]any{"type": "diff", "path": "/w/new.txt", "newText": "x\ny\n"}}})
		f.update(map[string]any{"sessionUpdate": "tool_call", "toolCallId": "c3", "title": "https://example.com", "kind": "fetch", "status": "completed", "rawInput": map[string]any{"url": "https://example.com"}})
		f.update(map[string]any{"sessionUpdate": "plan", "entries": []any{map[string]any{"content": "step one", "status": "completed", "priority": "high"}, map[string]any{"content": "step two", "status": "pending", "priority": "low"}}})
		f.update(map[string]any{"sessionUpdate": "usage_update", "used": 100, "size": 1000, "cost": map[string]any{"amount": 0.25, "currency": "USD"}})
		f.say("agent_message_chunk", "done")
		done("end_turn", map[string]any{"usage": map[string]any{"inputTokens": 10, "outputTokens": 5, "cachedReadTokens": 2, "totalTokens": 17}})
	case "perm", "permedit":
		kind := "execute"
		if verb == "permedit" {
			kind = "edit"
		}
		f.update(map[string]any{"sessionUpdate": "tool_call", "toolCallId": "p1", "title": "touch x", "kind": kind, "status": "pending", "rawInput": map[string]any{"command": "touch x", "filePath": firstOf(arg, "/w/x")}})
		r := f.call("session/request_permission", map[string]any{"sessionId": f.sid,
			"toolCall": map[string]any{"toolCallId": "p1", "title": "touch x", "kind": kind},
			"options": []any{
				map[string]any{"optionId": "a1", "name": "Allow once", "kind": "allow_once"},
				map[string]any{"optionId": "a2", "name": "Always", "kind": "allow_always"},
				map[string]any{"optionId": "r1", "name": "Reject", "kind": "reject_once"}}})
		var res struct {
			Outcome struct {
				Outcome  string `json:"outcome"`
				OptionID string `json:"optionId"`
			} `json:"outcome"`
		}
		_ = json.Unmarshal(r.Result, &res)
		f.say("agent_message_chunk", "chose "+firstOf(res.Outcome.OptionID, res.Outcome.Outcome))
		if res.Outcome.Outcome == "cancelled" {
			done("cancelled", nil)
			return
		}
		f.update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "p1", "status": "completed"})
		done("end_turn", nil)
	case "read":
		r := f.call("fs/read_text_file", map[string]any{"sessionId": f.sid, "path": arg, "line": 2, "limit": 1})
		if r.Error != nil {
			f.say("agent_message_chunk", "error: "+r.Error.Message)
		} else {
			var res struct {
				Content string `json:"content"`
			}
			_ = json.Unmarshal(r.Result, &res)
			f.say("agent_message_chunk", "read: "+res.Content)
		}
		done("end_turn", nil)
	case "write":
		r := f.call("fs/write_text_file", map[string]any{"sessionId": f.sid, "path": arg, "content": "hi\n"})
		if r.Error != nil {
			f.say("agent_message_chunk", "error: "+r.Error.Message)
		} else {
			f.say("agent_message_chunk", "wrote")
		}
		done("end_turn", nil)
	case "hang":
		f.say("agent_message_chunk", "working")
		f.waitFor(func(m rpcMsg) bool { return m.Method == "session/cancel" })
		done("cancelled", nil)
	case "deaf":
		f.say("agent_message_chunk", "not listening")
	case "crash":
		fmt.Fprintln(os.Stderr, "fake agent crashed")
		os.Exit(3)
	case "quiet":
		done("end_turn", nil)
	default:
		f.say("agent_message_chunk", "echo "+text)
		done("end_turn", nil)
	}
}

// acpSetup registers the fake agent "fake" and points the manager at the helper process.
func (e *orchEnv) acpSetup(m *OrchManager, extraEnv ...string) {
	e.t.Helper()
	reg := []acpAgent{{ID: "fake", Name: "Fake ACP", Command: os.Args[0], Args: []string{"-test.run=^TestOrchFakeACPProcess$"}, Env: map[string]string{"FAKE_FROM_REGISTRY": "1"}}}
	b, _ := json.Marshal(reg)
	if err := os.WriteFile(filepath.Join(e.dir, "acp-agents.json"), b, 0o644); err != nil {
		e.t.Fatal(err)
	}
	m.acpCommand = func(a acpAgent, th *OThread) (*exec.Cmd, error) {
		cmd := exec.Command(a.Command, a.Args...)
		cmd.Env = append(os.Environ(), append([]string{"ORCH_FAKE_ACP=1", "ORCH_FAKE_LOG=" + e.log, "ORCH_FAKE_ACP_DIR=" + e.dir}, extraEnv...)...)
		return cmd, nil
	}
	m.acpCancelGrace = 300 * time.Millisecond
	m.agents.version = func(string) string { return "fake 1.0" }
}

func (e *orchEnv) createACP(prompt, mode string) *OThread {
	e.t.Helper()
	th, err := e.m.CreateThread(orchCreate{Agent: "fake", Cwd: e.cwd, Prompt: prompt, PermissionMode: mode})
	if err != nil {
		e.t.Fatal(err)
	}
	return th
}

func agentText(d *OThreadDetail) string {
	var s []string
	for _, it := range d.Items {
		if it.Kind == "agent" {
			s = append(s, it.Text)
		}
	}
	return strings.Join(s, "|")
}

func TestOrchACPTurn(t *testing.T) {
	e := newOrchEnv(t)
	e.acpSetup(e.m)
	th := e.createACP("chunks", "")
	run := e.waitRunEnd()
	if field(run, "run", "status") != "completed" {
		t.Fatalf("run %v", run)
	}
	d := e.detail(th.ID)
	if got := agentText(d); got != "Hello|done" {
		t.Fatalf("agent items %q", got)
	}
	if r := itemOf(d, "reasoning"); r == nil || r.Text != "thinking hard" || r.Status != "completed" {
		t.Fatalf("reasoning %+v", r)
	}
	c := itemOf(d, "command")
	if c == nil || c.Command != "ls -la" || c.Output != "a.txt\nb.txt" || c.ExitCode == nil || *c.ExitCode != 0 || c.Status != "completed" || c.Cwd != e.cwd {
		t.Fatalf("command %+v", c)
	}
	fc := itemOf(d, "fileChange")
	if fc == nil || len(fc.Files) != 2 || fc.Files[0] != (OFile{Path: "/w/a.txt", Kind: "update", Add: 2, Del: 1}) || fc.Files[1] != (OFile{Path: "/w/new.txt", Kind: "add", Add: 2}) {
		t.Fatalf("fileChange %+v", fc)
	}
	if w := itemOf(d, "web"); w == nil || w.Text != "https://example.com" {
		t.Fatalf("web %+v", w)
	}
	if p := itemOf(d, "plan"); p == nil || p.Text != "- [x] step one\n- [ ] step two" {
		t.Fatalf("plan %+v", p)
	}
	u := d.Runs[0].Usage
	if u == nil || u.InTokens != 12 || u.OutTokens != 5 || u.CostUSD != 0.25 {
		t.Fatalf("usage %+v", u)
	}
	if !strings.HasPrefix(d.Thread.NativeID, "fs_") || d.Thread.Status != "idle" || d.Thread.Agent != "fake" {
		t.Fatalf("thread %+v", d.Thread)
	}
	log := e.fakeLog()
	for _, want := range []string{`"readTextFile":true`, `"writeTextFile":true`, `"terminal":false`, `"name":"agent-monitor"`, `"command":"/fake/agent-monitor"`, `{"name":"AM_ORCH_THREAD","value":"` + th.ID + `"}`, `"jsonrpc":"2.0"`} {
		if !strings.Contains(log, want) {
			t.Fatalf("fake log lacks %s:\n%s", want, log)
		}
	}
	// A second message reuses the process and the session.
	if _, err := e.m.Send(th.ID, "again"); err != nil {
		t.Fatal(err)
	}
	e.waitRunEnd()
	if got := agentText(e.detail(th.ID)); !strings.HasSuffix(got, "echo again") {
		t.Fatalf("second run %q", got)
	}
	if strings.Count(e.fakeLog(), `"method":"session/new"`) != 1 {
		t.Fatal("second turn opened another session")
	}

	// The registry lists the fake agent with the models its session reported.
	var fake *orchAgentInfo
	for _, a := range e.m.Agents() {
		if a.ID == "fake" {
			fake = &a
		}
	}
	if fake == nil || fake.Kind != "acp" || !fake.Installed || fake.Version != "fake 1.0" || len(fake.Models) != 2 {
		t.Fatalf("agents row %+v", fake)
	}
	// delegate_task accepts a registry id.
	child, err := e.m.Delegate(th.ID, orchChildSpec{Title: "kid", Prompt: "hello", Agent: "fake"})
	if err != nil || child.Agent != "fake" || child.ParentID != th.ID {
		t.Fatalf("delegate %+v %v", child, err)
	}
	e.wait("run.ended", func(d map[string]any) bool { return field(d, "run", "threadId") == child.ID })
}

func (e *orchEnv) acpPermRun(prompt, mode string, decide func(a *Approval)) *OThreadDetail {
	e.t.Helper()
	th := e.createACP(prompt, mode)
	if decide != nil {
		opened := e.wait("request.opened", nil)
		a := e.pending(field(opened, "request", "approvalId"))
		if a == nil || a.Agent != "fake" || a.DrivenID != th.ID || a.SessionID != "fake:"+e.mustThread(th.ID).NativeID {
			e.t.Fatalf("pending approval %+v", a)
		}
		decide(a)
		e.wait("request.closed", nil)
	}
	e.waitRunEnd()
	return e.detail(th.ID)
}

func (e *orchEnv) mustThread(id string) *OThread {
	th, err := e.store.Thread(id)
	if err != nil {
		e.t.Fatal(err)
	}
	return th
}

func TestOrchACPPermission(t *testing.T) {
	cases := []struct {
		name string
		dec  approvalDecision
		want string
	}{
		{"allow", approvalDecision{Behavior: "allow"}, "chose a1"},
		{"always", approvalDecision{Behavior: "allow", Always: true}, "chose a2"},
		{"deny", approvalDecision{Behavior: "deny"}, "chose r1"},
		{"option", approvalDecision{OptionID: "r1"}, "chose r1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newOrchEnv(t)
			e.acpSetup(e.m)
			d := e.acpPermRun("perm", "", func(a *Approval) {
				if len(a.Options) != 3 || a.Options[1].Kind != "allow_always" || a.ToolName != "Bash" || a.Input["command"] != "touch x" {
					t.Fatalf("approval %+v", a)
				}
				e.decide(a.ID, c.dec)
			})
			if got := agentText(d); got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
			if len(d.Requests) != 1 || d.Requests[0].Kind != "command" || d.Requests[0].ItemID == "" {
				t.Fatalf("requests %+v", d.Requests)
			}
		})
	}
	t.Run("acceptEdits", func(t *testing.T) {
		e := newOrchEnv(t)
		e.acpSetup(e.m)
		if got := agentText(e.acpPermRun("permedit "+filepath.Join(e.cwd, "x"), "acceptEdits", nil)); got != "chose a1" {
			t.Fatalf("got %q", got)
		}
	})
}

func TestOrchACPFileSystem(t *testing.T) {
	e := newOrchEnv(t)
	e.acpSetup(e.m)
	inside := filepath.Join(e.cwd, "in.txt")
	_ = os.WriteFile(inside, []byte("l1\nl2\nl3\n"), 0o644)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	_ = os.WriteFile(outside, []byte("secret\n"), 0o644)

	if got := agentText(e.acpPermRun("read "+inside, "", nil)); got != "read: l2\n" {
		t.Fatalf("read inside %q", got)
	}
	if got := agentText(e.acpPermRun("read "+outside, "", nil)); !strings.Contains(got, "outside the thread workspace") {
		t.Fatalf("read outside %q", got)
	}
	if got := agentText(e.acpPermRun("read "+filepath.Join(e.cwd, "..", filepath.Base(filepath.Dir(outside)), "secret.txt"), "", nil)); !strings.Contains(got, "error") {
		t.Fatalf("read via .. %q", got)
	}
	if got := agentText(e.acpPermRun("write "+outside, "", nil)); !strings.Contains(got, "outside the thread workspace") {
		t.Fatalf("write outside %q", got)
	}
	target := filepath.Join(e.cwd, "sub", "w.txt")
	d := e.acpPermRun("write "+target, "", func(a *Approval) {
		if a.ToolName != "Write" || a.Input["file_path"] != target {
			t.Fatalf("write approval %+v", a)
		}
		e.decide(a.ID, approvalDecision{Behavior: "deny"})
	})
	if got := agentText(d); !strings.Contains(got, "write denied") || pathExists(target) {
		t.Fatalf("denied write %q", got)
	}
	if d.Requests[0].Kind != "fileChange" {
		t.Fatalf("request %+v", d.Requests[0])
	}
	d = e.acpPermRun("write "+target, "", func(a *Approval) { e.decide(a.ID, approvalDecision{Behavior: "allow"}) })
	if b, _ := os.ReadFile(target); agentText(d) != "wrote" || string(b) != "hi\n" {
		t.Fatalf("allowed write %q %q", agentText(d), b)
	}
	other := filepath.Join(e.cwd, "auto.txt")
	if got := agentText(e.acpPermRun("write "+other, "acceptEdits", nil)); got != "wrote" || !pathExists(other) {
		t.Fatalf("acceptEdits write %q", got)
	}
}

func TestOrchACPCancel(t *testing.T) {
	e := newOrchEnv(t)
	e.acpSetup(e.m)
	th := e.createACP("hang", "")
	e.wait("item.started", func(d map[string]any) bool { return field(d, "item", "kind") == "agent" })
	if err := e.m.Interrupt(th.ID); err != nil {
		t.Fatal(err)
	}
	if run := e.waitRunEnd(); field(run, "run", "status") != "interrupted" {
		t.Fatalf("run %v", run)
	}
	if !strings.Contains(e.fakeLog(), `"method":"session/cancel"`) {
		t.Fatal("no session/cancel sent")
	}
	// An agent that ignores session/cancel is ended after the grace period.
	if _, err := e.m.Send(th.ID, "deaf"); err != nil {
		t.Fatal(err)
	}
	e.wait("item.started", func(d map[string]any) bool { return field(d, "item", "kind") == "agent" })
	if _, err := e.m.Send(th.ID, "busy"); err == nil {
		t.Fatal("send during a turn should be refused")
	}
	if err := e.m.Interrupt(th.ID); err != nil {
		t.Fatal(err)
	}
	if run := e.waitRunEnd(); field(run, "run", "status") != "interrupted" {
		t.Fatalf("deaf run %v", run)
	}
	// A pending permission is answered cancelled when the turn is interrupted.
	if _, err := e.m.Send(th.ID, "perm"); err != nil {
		t.Fatal(err)
	}
	e.wait("request.opened", nil)
	if err := e.m.Interrupt(th.ID); err != nil {
		t.Fatal(err)
	}
	e.waitRunEnd()
	if d := e.detail(th.ID); !strings.Contains(agentText(d), "chose cancelled") {
		t.Fatalf("perm cancel %q", agentText(d))
	}
}

func TestOrchACPCrashThenResume(t *testing.T) {
	e := newOrchEnv(t)
	e.acpSetup(e.m)
	th := e.createACP("hello", "")
	e.waitRunEnd()
	native := e.mustThread(th.ID).NativeID
	if _, err := e.m.Send(th.ID, "crash"); err != nil {
		t.Fatal(err)
	}
	run := e.waitRunEnd()
	if field(run, "run", "status") != "failed" {
		t.Fatalf("crash run %v", run)
	}
	got := e.wait("thread.updated", func(d map[string]any) bool { return field(d, "thread", "status") == "error" })
	if !strings.Contains(field(got, "thread", "error"), "fake agent crashed") {
		t.Fatalf("thread error %v", got)
	}
	if _, err := e.m.Send(th.ID, "after"); err != nil {
		t.Fatal(err)
	}
	if run := e.waitRunEnd(); field(run, "run", "status") != "completed" {
		t.Fatalf("resume run %v", run)
	}
	d := e.detail(th.ID)
	if d.Thread.NativeID != native || !strings.Contains(e.fakeLog(), `"method":"session/load"`) {
		t.Fatalf("not loaded: native %s -> %s", native, d.Thread.NativeID)
	}
	if txt := agentText(d); strings.Contains(txt, "replayed") || !strings.HasSuffix(txt, "echo after") {
		t.Fatalf("replay leaked into items: %q", txt)
	}
}

func TestOrchACPLoadUnsupported(t *testing.T) {
	e := newOrchEnv(t)
	e.acpSetup(e.m, "ORCH_FAKE_ACP_LOAD=0")
	th := e.createACP("hello", "")
	e.waitRunEnd()
	native := e.mustThread(th.ID).NativeID
	// A daemon restart drops the process; the next message needs a new session.
	e.m.Shutdown()
	e.m = e.manager()
	e.acpSetup(e.m, "ORCH_FAKE_ACP_LOAD=0")
	if _, err := e.m.Send(th.ID, "again"); err != nil {
		t.Fatal(err)
	}
	if run := e.waitRunEnd(); field(run, "run", "status") != "completed" {
		t.Fatalf("run %v", run)
	}
	d := e.detail(th.ID)
	if d.Thread.NativeID == native || d.Thread.NativeID == "" {
		t.Fatalf("native id not replaced: %s", d.Thread.NativeID)
	}
	note := ""
	for _, it := range d.Items {
		if it.Kind == "other" && strings.Contains(it.Text, "cannot load an earlier session") {
			note = it.Text
		}
	}
	if note == "" {
		t.Fatalf("no resume-as-new note: %+v", d.Items)
	}
}

func TestOrchACPQuietTurnAndCreateChecks(t *testing.T) {
	e := newOrchEnv(t)
	e.acpSetup(e.m)
	th := e.createACP("quiet", "")
	e.waitRunEnd()
	if it := itemOf(e.detail(th.ID), "error"); it == nil || !strings.Contains(it.Text, "without any output") {
		t.Fatalf("quiet note %+v", it)
	}
	if _, err := e.m.CreateThread(orchCreate{Agent: "fake", Cwd: e.cwd, Prompt: "x", PermissionMode: "auto"}); err == nil {
		t.Fatal("auto permission mode accepted for an ACP agent")
	}
	_ = os.WriteFile(filepath.Join(e.dir, "acp-agents.json"), []byte(`[{"id":"ghost","command":"/nope/ghost-agent","args":["acp"]}]`), 0o644)
	if _, err := e.m.CreateThread(orchCreate{Agent: "ghost", Cwd: e.cwd, Prompt: "x"}); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("missing binary: %v", err)
	}
	srv := httptest.NewServer(func() http.Handler { mux := http.NewServeMux(); e.m.routes(mux); return mux }())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/orch/agents")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var rows []orchAgentInfo
	_ = json.NewDecoder(resp.Body).Decode(&rows)
	ids := map[string]orchAgentInfo{}
	for _, r := range rows {
		ids[r.ID] = r
	}
	if ids["claude"].Kind != "claude" || ids["codex"].Kind != "codex" || ids["opencode"].Kind != "acp" || ids["ghost"].Installed || ids["ghost"].Kind != "acp" {
		t.Fatalf("agents %+v", rows)
	}
}
