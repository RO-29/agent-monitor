package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// until polls cond for up to 10 s.
func (e *orchEnv) until(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (e *orchEnv) thread(id string) *OThread {
	e.t.Helper()
	th, err := e.store.Thread(id)
	if err != nil {
		e.t.Fatal(err)
	}
	return th
}

func (e *orchEnv) idle(id string) {
	e.t.Helper()
	e.until(id+" idle", func() bool { return e.thread(id).Status == "idle" })
}

func (e *orchEnv) group(id, want string) {
	e.t.Helper()
	e.until(id+" group "+want, func() bool { return e.thread(id).Group == want })
}

// hanging waits until a "hang" turn has started and can be interrupted.
func (e *orchEnv) hanging(id string) {
	e.t.Helper()
	e.until(id+" hanging", func() bool { return lastAgentText(e.detail(id)) == "working" && e.thread(id).Status == "running" })
}

func (e *orchEnv) runs(id string) []ORun { return e.detail(id).Runs }

func (e *orchEnv) delegate(parent string, spec orchChildSpec) *OThread {
	e.t.Helper()
	th, err := e.m.Delegate(parent, spec)
	if err != nil {
		e.t.Fatal(err)
	}
	return th
}

func (e *orchEnv) server() *httptest.Server {
	mux := http.NewServeMux()
	e.m.routes(mux)
	srv := httptest.NewServer(mux)
	e.t.Cleanup(srv.Close)
	return srv
}

func postJSON(t *testing.T, url, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func notifyRuns(runs []ORun) int {
	n := 0
	for _, r := range runs {
		if strings.HasPrefix(r.Prompt, "Subagents finished:") {
			n++
		}
	}
	return n
}

// createWritable starts a Codex root that may write, so its children may too.
func (e *orchEnv) createWritable(prompt string) *OThread {
	e.t.Helper()
	th, err := e.m.CreateThread(orchCreate{Agent: "codex", Cwd: e.cwd, Prompt: prompt, Sandbox: "workspace-write"})
	if err != nil {
		e.t.Fatal(err)
	}
	return th
}

func TestOrchDelegateSharedAndNotifyOnce(t *testing.T) {
	e := newOrchEnv(t)
	parent := e.createWritable("hello")
	e.idle(parent.ID)
	child := e.delegate(parent.ID, orchChildSpec{Title: "Child one", Prompt: "hello", Agent: "codex"})
	if child.ParentID != parent.ID || child.Depth != 1 || child.Cwd != parent.Cwd || child.Workspace != "shared" ||
		child.Sandbox != "workspace-write" || !child.notifies() || child.Title != "Child one" {
		t.Fatalf("child %+v", child)
	}
	if p := e.thread(parent.ID); len(p.Children) != 1 || p.Children[0] != child.ID {
		t.Fatalf("parent children %v", p.Children)
	}
	e.until("notify run", func() bool { return notifyRuns(e.runs(parent.ID)) == 1 })
	e.idle(parent.ID)
	runs := e.runs(parent.ID)
	last := runs[len(runs)-1]
	if !strings.Contains(last.Prompt, "Child one ("+child.ID+"): completed") || !strings.Contains(last.Prompt, "pong") {
		t.Fatalf("notify prompt %q", last.Prompt)
	}
	time.Sleep(400 * time.Millisecond)
	if n := notifyRuns(e.runs(parent.ID)); n != 1 {
		t.Fatalf("parent notified %d times", n)
	}
	if c := e.thread(child.ID); c.ReportedAt == 0 {
		t.Fatalf("child not marked reported: %+v", c)
	}
	log := e.fakeLog()
	for _, want := range []string{
		`mcp_servers.agent-monitor.env.AM_ORCH_THREAD=\"` + child.ID + `\"`,
		`mcp_servers.agent-monitor.command=\"/fake/agent-monitor\"`,
		`mcp_servers.agent-monitor.tools.delegate_task.approval_mode=\"approve\"`,
		"env AM_ORCH_THREAD=" + child.ID,
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("fake codex log lacks %s:\n%s", want, log)
		}
	}
	d := e.detail(parent.ID)
	if len(d.Children) != 1 || d.Children[0].ID != child.ID {
		t.Fatalf("detail children %+v", d.Children)
	}
}

func TestOrchNotifyParentOff(t *testing.T) {
	e := newOrchEnv(t)
	parent := e.create("hello")
	e.idle(parent.ID)
	child := e.delegate(parent.ID, orchChildSpec{Prompt: "hello", Agent: "codex", NotifyParent: boolPtr(false)})
	e.idle(child.ID)
	time.Sleep(400 * time.Millisecond)
	if n := len(e.runs(parent.ID)); n != 1 {
		t.Fatalf("parent got %d runs with notifyParent false", n)
	}
}

func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644)
	run("add", ".")
	run("commit", "-q", "-m", "init")
	return dir
}

func TestOrchDelegateWorktree(t *testing.T) {
	e := newOrchEnv(t)
	plain := e.createWritable("hello")
	e.idle(plain.ID)
	if _, err := e.m.Delegate(plain.ID, orchChildSpec{Prompt: "x", Agent: "codex", Workspace: "worktree"}); err == nil || !strings.Contains(err.Error(), "git repository") {
		t.Fatalf("worktree outside git: %v", err)
	}
	e.cwd = gitRepo(t)
	parent := e.createWritable("hello")
	e.idle(parent.ID)
	child := e.delegate(parent.ID, orchChildSpec{Title: "Fix the Parser!", Prompt: "hello", Agent: "codex", Workspace: "worktree"})
	wt := child.Worktree
	if wt == nil || wt.Path != filepath.Join(e.dir, "wt", child.ID) || child.Cwd != wt.Path ||
		wt.Branch != "am/fix-the-parser-"+child.ID[len(child.ID)-6:] || child.Branch != wt.Branch {
		t.Fatalf("worktree child %+v %+v", child, wt)
	}
	if child.Repo != filepath.Base(e.cwd) {
		t.Fatalf("worktree child repo %q, want %q", child.Repo, filepath.Base(e.cwd))
	}
	out, err := exec.Command("git", "-C", wt.Path, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil || strings.TrimSpace(string(out)) != wt.Branch {
		t.Fatalf("worktree branch %q %v", out, err)
	}
	if !pathExists(filepath.Join(wt.Path, "a.txt")) {
		t.Fatal("worktree lacks the parent HEAD files")
	}
	e.idle(child.ID)
	e.m.Close(child.ID)
	if !pathExists(wt.Path) {
		t.Fatal("worktree removed on close")
	}
}

func TestOrchDelegateReadOnly(t *testing.T) {
	e := newOrchEnv(t)
	parent := e.create("hang")
	e.hanging(parent.ID)
	cx := e.delegate(parent.ID, orchChildSpec{Prompt: "hello", Agent: "codex", Workspace: "read-only", Sandbox: "workspace-write"})
	if cx.Sandbox != "read-only" || cx.Workspace != "read-only" {
		t.Fatalf("codex read-only child %+v", cx)
	}
	cl := e.delegate(parent.ID, orchChildSpec{Prompt: "hello", Agent: "claude", Workspace: "read-only", Model: "haiku"})
	e.idle(cx.ID)
	e.idle(cl.ID)
	log := e.fakeLog()
	if !strings.Contains(log, `"sandbox":"read-only"`) {
		t.Fatalf("codex thread/start without read-only sandbox:\n%s", log)
	}
	var args string
	for _, l := range strings.Split(log, "\n") {
		if strings.HasPrefix(l, "args ") && strings.Contains(l, cl.NativeID) {
			args = l
		}
	}
	for _, want := range []string{"--disallowedTools Edit,Write,MultiEdit,NotebookEdit", "--mcp-config ", `"AM_ORCH_THREAD":"` + cl.ID + `"`,
		`"AM_ORCH_DEPTH":"1"`, "--allowedTools mcp__agent-monitor__delegate_task,mcp__agent-monitor__wait_tasks,mcp__agent-monitor__task_status,mcp__agent-monitor__search_sessions,mcp__agent-monitor__session_context", "--model haiku"} {
		if !strings.Contains(args, want) {
			t.Fatalf("claude args lack %q: %s", want, args)
		}
	}
}

func TestOrchDelegateLimits(t *testing.T) {
	e := newOrchEnv(t)
	now := time.Now().UnixMilli()
	put := func(id, parent string, depth int, status string) {
		if err := e.m.insertThread(&OThread{ID: id, Kind: "driven", Agent: "codex", Cwd: e.cwd, Status: status, ParentID: parent, Depth: depth, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	put("root", "", 0, "idle")
	put("mid", "root", 1, "idle")
	put("leaf", "mid", 2, "idle")
	if _, err := e.m.Delegate("leaf", orchChildSpec{Prompt: "x", Agent: "codex"}); err == nil || !strings.Contains(err.Error(), "max depth 2") {
		t.Fatalf("depth limit: %v", err)
	}
	for i := 0; i < 7; i++ {
		put("run"+itoa(i), "root", 1, "running")
	}
	c := e.delegate("root", orchChildSpec{Prompt: "hang", Agent: "codex"})
	if _, err := e.m.Delegate("root", orchChildSpec{Prompt: "x", Agent: "codex"}); err == nil || !strings.Contains(err.Error(), "max 8 running") {
		t.Fatalf("running limit: %v", err)
	}
	srv := e.server()
	code, out := postJSON(t, srv.URL+"/api/orch/threads/root/delegate", `{"prompt":"x","agent":"codex"}`)
	if code != 409 || !strings.Contains(out["error"].(string), "max 8 running") {
		t.Fatalf("http limit %d %v", code, out)
	}
	code, out = postJSON(t, srv.URL+"/api/orch/threads/root/delegate", `{"prompt":"x","agent":"gpt"}`)
	if code != 400 {
		t.Fatalf("bad agent %d %v", code, out)
	}
	if code, _ = postJSON(t, srv.URL+"/api/orch/threads/nope/delegate", `{"prompt":"x","agent":"codex"}`); code != 404 {
		t.Fatalf("unknown parent %d", code)
	}
	_ = e.m.Interrupt(c.ID)
}

func TestOrchWaitTasks(t *testing.T) {
	e := newOrchEnv(t)
	parent := e.create("hang")
	e.hanging(parent.ID)
	slow := e.delegate(parent.ID, orchChildSpec{Title: "slow", Prompt: "hang", Agent: "codex"})
	e.hanging(slow.ID)
	res, timedOut, err := e.m.Tasks(context.Background(), parent.ID, nil, 200*time.Millisecond)
	if err != nil || !timedOut || len(res) != 1 || res[0].Status != "running" || res[0].ThreadID != slow.ID {
		t.Fatalf("timeout wait %+v %v %v", res, timedOut, err)
	}
	srv := e.server()
	got := make(chan map[string]any, 1)
	go func() {
		resp, err := http.Get(srv.URL + "/api/orch/threads/" + parent.ID + "/tasks?wait=10")
		if err != nil {
			got <- nil
			return
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		got <- out
	}()
	time.Sleep(200 * time.Millisecond)
	if err := e.m.Interrupt(slow.ID); err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	select {
	case out = <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("wait_tasks did not return")
	}
	tasks, _ := out["tasks"].([]any)
	if out["timedOut"] != false || len(tasks) != 1 {
		t.Fatalf("wait result %v", out)
	}
	if r := tasks[0].(map[string]any); r["status"] != "interrupted" || r["result"] != "working" || r["title"] != "slow" {
		t.Fatalf("task %v", r)
	}
	fast := e.delegate(parent.ID, orchChildSpec{Prompt: "hello", Agent: "codex"})
	res, timedOut, err = e.m.Tasks(context.Background(), parent.ID, []string{fast.ID}, 10*time.Second)
	if err != nil || timedOut || len(res) != 1 || res[0].Status != "completed" || res[0].Result != "pong" {
		t.Fatalf("fast wait %+v %v %v", res, timedOut, err)
	}
	if _, _, err := e.m.Tasks(context.Background(), slow.ID, []string{fast.ID}, 0); err == nil {
		t.Fatal("a sibling is not a subagent of the caller")
	}
	res, _, _ = e.m.Tasks(context.Background(), parent.ID, nil, 0)
	if len(res) != 2 {
		t.Fatalf("task_status with nothing running lists all children: %+v", res)
	}
	_ = e.m.Interrupt(parent.ID)
	e.idle(parent.ID)
	time.Sleep(300 * time.Millisecond)
	if n := notifyRuns(e.runs(parent.ID)); n != 0 {
		t.Fatalf("children reported through wait_tasks must not notify again: %d", n)
	}
}

func TestOrchFanOut(t *testing.T) {
	e := newOrchEnv(t)
	srv := e.server()
	body, _ := json.Marshal(map[string]any{"agent": "codex", "cwd": e.cwd, "prompt": "lead", "children": []any{
		map[string]any{"title": "A", "prompt": "hello", "agent": "codex", "workspace": "read-only"},
		map[string]any{"title": "B", "prompt": "hello", "agent": "claude", "workspace": "shared"},
	}})
	code, out := postJSON(t, srv.URL+"/api/orch/threads", string(body))
	if code != 200 {
		t.Fatalf("create %d %v", code, out)
	}
	pid := field(out, "thread", "id")
	p := e.thread(pid)
	if len(p.Children) != 2 {
		t.Fatalf("parent children %+v", p)
	}
	first := e.runs(pid)[0]
	for _, id := range p.Children {
		c := e.thread(id)
		if c.Depth != 1 || c.ParentID != pid || !strings.Contains(first.Prompt, id+": "+c.Title) {
			t.Fatalf("child %+v prompt %q", c, first.Prompt)
		}
	}
	e.until("notify run", func() bool { return notifyRuns(e.runs(pid)) == 1 })
	e.idle(pid)
	time.Sleep(400 * time.Millisecond)
	if n := notifyRuns(e.runs(pid)); n != 1 {
		t.Fatalf("notified %d times", n)
	}
	bad, _ := json.Marshal(map[string]any{"agent": "codex", "cwd": e.cwd, "prompt": "lead", "children": []any{
		map[string]any{"prompt": "x", "agent": "codex", "workspace": "worktree"}}})
	if code, _ := postJSON(t, srv.URL+"/api/orch/threads", string(bad)); code != 400 {
		t.Fatalf("worktree fan-out outside git: %d", code)
	}
}

func TestOrchGroups(t *testing.T) {
	e := newOrchEnv(t)
	srv := e.server()
	parent := e.create("hello")
	e.idle(parent.ID)
	e.group(parent.ID, "ready")
	if th := e.thread(parent.ID); th.ReadyAt == 0 {
		t.Fatalf("readyAt %+v", th)
	}

	// A running child makes its parent working.
	slow := e.delegate(parent.ID, orchChildSpec{Prompt: "hang", Agent: "codex", NotifyParent: boolPtr(false)})
	e.group(slow.ID, "working")
	e.group(parent.ID, "working")
	e.until("interrupt slow", func() bool { return e.m.Interrupt(slow.ID) == nil })
	e.group(parent.ID, "ready")

	// A child request makes its parent need you; the approval names the root.
	asker := e.delegate(parent.ID, orchChildSpec{Prompt: "approve", Agent: "codex", NotifyParent: boolPtr(false)})
	opened := e.wait("request.opened", nil)
	e.group(parent.ID, "need")
	e.group(asker.ID, "need")
	a := e.pending(field(opened, "request", "approvalId"))
	if a == nil || a.DrivenID != asker.ID || a.RootID != parent.ID {
		t.Fatalf("child approval %+v", a)
	}
	e.decide(a.ID, approvalDecision{Behavior: "allow"})
	e.idle(asker.ID)
	e.group(parent.ID, "ready")

	// settle, pin, snooze and the 24 h rule.
	if code, out := postJSON(t, srv.URL+"/api/orch/threads/"+parent.ID+"/settle", `{"settled":true}`); code != 200 || field(out, "thread", "group") != "settled" {
		t.Fatalf("settle %d %v", code, out)
	}
	postJSON(t, srv.URL+"/api/orch/threads/"+parent.ID+"/settle", `{"settled":false}`)
	e.group(parent.ID, "ready")
	if code, out := postJSON(t, srv.URL+"/api/orch/threads/"+parent.ID+"/pin", `{"pinned":true}`); code != 200 || out["thread"].(map[string]any)["pinned"] != true {
		t.Fatalf("pin %d %v", code, out)
	}
	until := time.Now().Add(300 * time.Millisecond).UnixMilli()
	code, out := postJSON(t, srv.URL+"/api/orch/threads/"+parent.ID+"/snooze", `{"until":`+itoa(int(until))+`}`)
	if code != 200 || field(out, "thread", "group") != "settled" {
		t.Fatalf("snooze %d %v", code, out)
	}
	time.Sleep(400 * time.Millisecond)
	if err := e.store.SweepGroups(); err != nil {
		t.Fatal(err)
	}
	if th := e.thread(parent.ID); th.Group != "ready" || th.SnoozedUntil != 0 || !th.Pinned {
		t.Fatalf("after snooze expiry %+v", th)
	}
	_ = e.store.write(func(tx *orchTx) error {
		th, _ := tx.thread(parent.ID)
		th.ReadyAt = time.Now().Add(-25 * time.Hour).UnixMilli()
		return tx.storeThread(th)
	})
	if err := e.store.SweepGroups(); err != nil {
		t.Fatal(err)
	}
	e.group(parent.ID, "settled")

	// A new message brings a settled thread back.
	postJSON(t, srv.URL+"/api/orch/threads/"+parent.ID+"/settle", `{"settled":true}`)
	if _, err := e.m.Send(parent.ID, "hello"); err != nil {
		t.Fatal(err)
	}
	e.idle(parent.ID)
	if th := e.thread(parent.ID); th.Group != "ready" || th.SettledAt != 0 {
		t.Fatalf("after new run %+v", th)
	}

	// An errored thread needs you; closed is settled.
	e.m.Close(asker.ID)
	e.group(asker.ID, "settled")
}

func TestOrchCascadeInterrupt(t *testing.T) {
	e := newOrchEnv(t)
	srv := e.server()
	parent := e.create("hang")
	e.hanging(parent.ID)
	child := e.delegate(parent.ID, orchChildSpec{Prompt: "hang", Agent: "codex", NotifyParent: boolPtr(false)})
	e.hanging(child.ID)
	if code, out := postJSON(t, srv.URL+"/api/orch/threads/"+parent.ID+"/interrupt", `{"cascade":false}`); code != 200 {
		t.Fatalf("interrupt %d %v", code, out)
	}
	e.idle(parent.ID)
	if st := e.thread(child.ID).Status; st != "running" {
		t.Fatalf("cascade false stopped the child: %s", st)
	}
	// No body is phase 2 behaviour: the idle parent has no turn to stop.
	if code, _ := postJSON(t, srv.URL+"/api/orch/threads/"+parent.ID+"/interrupt", ``); code != 409 {
		t.Fatalf("interrupt without body on an idle parent: %d", code)
	}
	// The parent is idle now; a cascade still reaches the running child.
	if code, out := postJSON(t, srv.URL+"/api/orch/threads/"+parent.ID+"/interrupt", `{}`); code != 200 {
		t.Fatalf("cascade interrupt %d %v", code, out)
	}
	e.idle(child.ID)
	if r := e.runs(child.ID); r[len(r)-1].Status != "interrupted" {
		t.Fatalf("child run %+v", r)
	}
	if r := e.runs(parent.ID); r[0].Status != "interrupted" {
		t.Fatalf("parent run %+v", r)
	}
}

func TestOrchMigratePhase2DB(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "orch.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE threads(id TEXT PRIMARY KEY, created_at INTEGER, json TEXT)`,
		`CREATE TABLE runs(id TEXT PRIMARY KEY, thread_id TEXT, idx INTEGER, json TEXT)`,
		`CREATE TABLE items(id TEXT PRIMARY KEY, thread_id TEXT, run_id TEXT, ord INTEGER, json TEXT)`,
		`CREATE TABLE requests(id TEXT PRIMARY KEY, thread_id TEXT, approval_id TEXT, json TEXT)`,
		`CREATE TABLE events(seq INTEGER PRIMARY KEY AUTOINCREMENT, thread_id TEXT, at INTEGER, type TEXT, data TEXT)`,
		`INSERT INTO threads VALUES('p', 1, '{"id":"p","kind":"driven","agent":"codex","status":"idle","createdAt":1,"updatedAt":1}')`,
		`INSERT INTO threads VALUES('c', 2, '{"id":"c","kind":"driven","agent":"codex","status":"running","parentId":"p","createdAt":2,"updatedAt":2}')`,
		`INSERT INTO events(thread_id, at, type, data) VALUES('p', 1, 'thread.updated', '{}')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	store, err := openOrchStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	m := newOrchManager(store, nil)
	defer m.Shutdown()
	kids, err := store.Children("p")
	if err != nil || len(kids) != 1 || kids[0].ID != "c" {
		t.Fatalf("children after migration %+v %v", kids, err)
	}
	p, _ := store.Thread("p")
	c, _ := store.Thread("c")
	if p.Group != "ready" || c.Group != "ready" || c.Status != "idle" || !p.notifies() {
		t.Fatalf("groups after migration p=%+v c=%+v", p, c)
	}
	evs, _ := store.EventsAfter(0, 100)
	if len(evs) == 0 || evs[0].Seq != 1 {
		t.Fatalf("old events lost %+v", evs)
	}
	store.Close()
	if _, err := openOrchStore(dir, nil); err != nil {
		t.Fatalf("second open: %v", err)
	}
}

// mcpSession runs serveMCP in process and returns a request function.
func mcpSession(t *testing.T, daemonURL string, orch mcpOrch) func(method string, params map[string]any) map[string]any {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	go func() {
		serveMCP(inR, outW, daemonURL, orch, func(string, ...any) {})
		outW.Close()
	}()
	t.Cleanup(func() { inW.Close() })
	rd := bufio.NewReader(outR)
	n := 0
	return func(method string, params map[string]any) map[string]any {
		n++
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": n, "method": method, "params": params})
		if _, err := inW.Write(append(b, '\n')); err != nil {
			t.Fatal(err)
		}
		line, err := rd.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var resp map[string]any
		_ = json.Unmarshal(line, &resp)
		return resp
	}
}

func toolNames(resp map[string]any) []string {
	var out []string
	tools, _ := resp["result"].(map[string]any)["tools"].([]any)
	for _, tl := range tools {
		out = append(out, tl.(map[string]any)["name"].(string))
	}
	return out
}

func TestOrchMCPToolsOnlyWhenDriven(t *testing.T) {
	has := func(names []string, n string) bool {
		for _, x := range names {
			if x == n {
				return true
			}
		}
		return false
	}
	watched := toolNames(mcpSession(t, "http://127.0.0.1:9", mcpOrch{})("tools/list", nil))
	if has(watched, "delegate_task") || has(watched, "wait_tasks") || !has(watched, "talk_to_agent") {
		t.Fatalf("watched tools %v", watched)
	}
	driven := toolNames(mcpSession(t, "http://127.0.0.1:9", mcpOrch{thread: "ot_x"})("tools/list", nil))
	for _, n := range []string{"delegate_task", "wait_tasks", "task_status", "permission_prompt"} {
		if !has(driven, n) {
			t.Fatalf("driven tools lack %s: %v", n, driven)
		}
	}
	deep := toolNames(mcpSession(t, "http://127.0.0.1:9", mcpOrch{thread: "ot_x", depth: 2})("tools/list", nil))
	if has(deep, "delegate_task") || !has(deep, "wait_tasks") {
		t.Fatalf("depth-2 tools %v", deep)
	}
	resp := mcpSession(t, "http://127.0.0.1:9", mcpOrch{})("tools/call", map[string]any{"name": "delegate_task", "arguments": map[string]any{}})
	if resp["error"] == nil {
		t.Fatalf("watched delegate_task must be unknown: %v", resp)
	}
}

func TestOrchMCPDelegateEndToEnd(t *testing.T) {
	e := newOrchEnv(t)
	srv := e.server()
	parent := e.create("hang")
	e.hanging(parent.ID)
	call := mcpSession(t, srv.URL, mcpOrch{thread: parent.ID})
	text := func(resp map[string]any) (string, bool) {
		r, _ := resp["result"].(map[string]any)
		c, _ := r["content"].([]any)
		if len(c) == 0 {
			t.Fatalf("no content %v", resp)
		}
		isErr, _ := r["isError"].(bool)
		return c[0].(map[string]any)["text"].(string), isErr
	}
	out, isErr := text(call("tools/call", map[string]any{"name": "delegate_task", "arguments": map[string]any{
		"title": "pong child", "prompt": "hello", "agent": "codex", "workspace": "read-only"}}))
	var del map[string]any
	if isErr || json.Unmarshal([]byte(out), &del) != nil || del["threadId"] == nil {
		t.Fatalf("delegate_task %s", out)
	}
	out, isErr = text(call("tools/call", map[string]any{"name": "wait_tasks", "arguments": map[string]any{"timeoutSec": 10}}))
	var waited struct {
		Tasks    []orchTaskResult `json:"tasks"`
		TimedOut bool             `json:"timedOut"`
	}
	if isErr || json.Unmarshal([]byte(out), &waited) != nil || len(waited.Tasks) != 1 || waited.Tasks[0].Result != "pong" ||
		waited.Tasks[0].ThreadID != del["threadId"] {
		t.Fatalf("wait_tasks %s", out)
	}
	out, isErr = text(call("tools/call", map[string]any{"name": "task_status", "arguments": map[string]any{"threadIds": []any{"ot_other"}}}))
	if !isErr || !strings.Contains(out, "not a subagent") {
		t.Fatalf("task_status foreign id %s", out)
	}
	_ = e.m.Interrupt(parent.ID)
}

func TestRepoNameFromCommonDir(t *testing.T) {
	for in, want := range map[string]string{"": "", "/src/app/.git": "app", "/src/app/.git/": "app", "/srv/app.git": "app", "/src/super/.git/modules/sub": "sub"} {
		if got := repoNameFromCommonDir(in); got != want {
			t.Fatalf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestOrchProcEnvPointsHooksAtDrivingDaemon(t *testing.T) {
	m := &OrchManager{daemonURL: "http://127.0.0.1:7790"}
	env := strings.Join(m.orchProcEnv(&OThread{ID: "ot_x"}), " ")
	for _, want := range []string{"AM_DAEMON_URL=http://127.0.0.1:7790", "AGENT_MONITOR_URL=http://127.0.0.1:7790", "AGENT_MONITOR_PORT=7790"} {
		if !strings.Contains(env, want) {
			t.Fatalf("env %q lacks %q", env, want)
		}
	}
}

func TestClaudeHookScriptUsesMonitorPort(t *testing.T) {
	var got []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Method+" "+r.URL.Path)
		mu.Unlock()
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	for _, ev := range []string{"SessionStart", "SessionEnd"} {
		cmd := exec.Command("bash", "bin/claude-hook.sh")
		cmd.Env = append(os.Environ(), "AGENT_MONITOR_URL=", "AGENT_MONITOR_PORT="+u.Port(), "TMUX_PANE=%9", "TMUX=/tmp/x")
		cmd.Stdin = strings.NewReader(`{"hook_event_name":"` + ev + `","session_id":"s1","cwd":"/tmp"}`)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v %s", ev, err, out)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	want := "POST /events/claude,POST /api/pane/register,POST /events/claude,DELETE /api/pane/registration/claude:s1"
	if strings.Join(got, ",") != want {
		t.Fatalf("requests %v", got)
	}
}
