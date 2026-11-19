package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeQDriven struct {
	mu       sync.Mutex
	st       string
	term     bool
	sent     []string
	sendErr  error
	missing  bool
	lastMode string
}

func (f *fakeQDriven) exists(string) bool { f.mu.Lock(); defer f.mu.Unlock(); return !f.missing }
func (f *fakeQDriven) status(string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st
}
func (f *fakeQDriven) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}
func (f *fakeQDriven) terminal(string) bool { f.mu.Lock(); defer f.mu.Unlock(); return f.term }
func (f *fakeQDriven) send(_ string, text string, _ []string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return "", f.sendErr
	}
	f.sent = append(f.sent, text)
	if f.st == "running" {
		return "steer", nil
	}
	return "turn", nil
}

type fakeQWatched struct {
	mu   sync.Mutex
	ph   string
	pane string
	sent []string
}

func (f *fakeQWatched) set(ph string) { f.mu.Lock(); f.ph = ph; f.mu.Unlock() }
func (f *fakeQWatched) phase(string) (string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ph, f.pane
}
func (f *fakeQWatched) send(_ string, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, text)
	return nil
}
func (f *fakeQWatched) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

// testQueue builds a manager with a counting clock, so every step is a later millisecond.
func testQueue(t *testing.T, s *OrchStore, d queueDriven, w queueWatched) *QueueManager {
	t.Helper()
	q := newQueueManager(s, d, w)
	var clock atomic.Int64
	clock.Store(time.Now().UnixMilli())
	q.now = func() int64 { return clock.Add(1) }
	return q
}

func texts(items []QItem) string {
	var b []string
	for _, it := range items {
		b = append(b, it.Text+"/"+it.State)
	}
	return strings.Join(b, ",")
}

func mustAdd(t *testing.T, q *QueueManager, thread string, in queueAdd) *QItem {
	t.Helper()
	it, err := q.Add(thread, in)
	if err != nil {
		t.Fatal(err)
	}
	return it
}

func TestQueueWatchedDispatchOnIdle(t *testing.T) {
	s := newTestOrchStore(t)
	w := &fakeQWatched{ph: "busy"}
	q := testQueue(t, s, &fakeQDriven{}, w)
	const th = "claude:abc"
	mustAdd(t, q, th, queueAdd{Text: "one", Session: th})
	mustAdd(t, q, th, queueAdd{Text: "two", Session: th})
	q.kicks.Wait()
	q.sweep()
	if len(w.got()) != 0 {
		t.Fatalf("busy session got %v", w.got())
	}
	w.set("prompt")
	q.sweep()
	if len(w.got()) != 0 {
		t.Fatal("a session at a question must not get a queued message")
	}
	w.set("ready")
	q.sweep()
	q.sweep()
	if got := w.got(); len(got) != 1 || got[0] != "one" {
		t.Fatalf("first idle: sent %v", got)
	}
	// Still idle with no turn in between: the next message waits for the next idle transition.
	q.sweep()
	if len(w.got()) != 1 {
		t.Fatalf("sent twice in one idle: %v", w.got())
	}
	w.set("busy")
	q.sweep()
	w.set("ready")
	q.sweep()
	if got := w.got(); len(got) != 2 || got[1] != "two" {
		t.Fatalf("second idle: sent %v", got)
	}
	if items, _ := q.List(th); len(items) != 0 {
		t.Fatalf("queue not empty: %s", texts(items))
	}
}

func TestQueueWatchedHookBusyCountsAsTurn(t *testing.T) {
	s := newTestOrchStore(t)
	w := &fakeQWatched{ph: "ready"}
	q := testQueue(t, s, &fakeQDriven{}, w)
	const th = "codex:x"
	mustAdd(t, q, th, queueAdd{Text: "a", Session: th})
	mustAdd(t, q, th, queueAdd{Text: "b", Session: th})
	q.kicks.Wait() // Add kicks in the background
	q.sweep()
	if got := w.got(); len(got) != 1 {
		t.Fatalf("idle on add: sent %v", got)
	}
	q.NoteBusy(th)
	q.sweep()
	if got := w.got(); len(got) != 2 || got[1] != "b" {
		t.Fatalf("after a hook busy event: sent %v", got)
	}
}

func TestQueueSendNowRemovePatch(t *testing.T) {
	s := newTestOrchStore(t)
	w := &fakeQWatched{ph: "busy"}
	q := testQueue(t, s, &fakeQDriven{}, w)
	const th = "claude:s"
	a := mustAdd(t, q, th, queueAdd{Text: "a", Session: th})
	b := mustAdd(t, q, th, queueAdd{Text: "b", Session: th})
	c := mustAdd(t, q, th, queueAdd{Text: "c", Session: th})
	if _, err := q.Add(th, queueAdd{Text: "img", Session: th, Attachments: []string{"up_1"}}); err == nil {
		t.Fatal("a watched session must refuse attachments")
	}
	two := 0
	if err := q.Patch(th, c.ID, queuePatch{Index: &two}); err != nil {
		t.Fatal(err)
	}
	first := 0
	edited := "c2"
	if err := q.Patch(th, c.ID, queuePatch{Index: &first, Text: &edited}); err != nil {
		t.Fatal(err)
	}
	q.kicks.Wait()
	items, _ := q.List(th)
	if texts(items) != "c2/queued,a/queued,b/queued" {
		t.Fatalf("after reorder: %s", texts(items))
	}
	if err := q.Remove(th, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := q.Remove(th, a.ID); err == nil {
		t.Fatal("removing twice must fail")
	}
	// Send now ignores busy: the agent's own TUI queues it.
	it, mode, err := q.SendNow(th, b.ID)
	if err != nil || mode != "pane" || it.State != "sent" {
		t.Fatalf("send now: %v %q %+v", err, mode, it)
	}
	items, _ = q.List(th)
	if texts(items) != "c2/queued" || strings.Join(w.got(), ",") != "b" {
		t.Fatalf("after send now: %s sent %v", texts(items), w.got())
	}
}

func TestQueueDrivenPausesForTerminal(t *testing.T) {
	s := newTestOrchStore(t)
	d := &fakeQDriven{st: "idle", term: true}
	q := testQueue(t, s, d, &fakeQWatched{})
	mustAdd(t, q, "ot_1", queueAdd{Text: "x"})
	q.kicks.Wait()
	q.sweep()
	v, _ := q.View("ot_1", "")
	if len(d.got()) != 0 || v.Paused != "terminal" {
		t.Fatalf("sent %v paused %q", d.got(), v.Paused)
	}
	d.mu.Lock()
	d.term = false
	d.mu.Unlock()
	q.sweep()
	if len(d.got()) != 1 {
		t.Fatalf("after the terminal left: sent %v", d.got())
	}
	d.mu.Lock()
	d.missing = true
	d.mu.Unlock()
	if _, err := q.Add("ot_gone", queueAdd{Text: "x"}); err == nil {
		t.Fatal("unknown driven thread must 404")
	}
}

func TestQueueFailureSkipsAndStops(t *testing.T) {
	s := newTestOrchStore(t)
	d := &fakeQDriven{st: "idle", sendErr: &orchSpendError{Scope: "day"}}
	q := testQueue(t, s, d, &fakeQWatched{})
	mustAdd(t, q, "ot_1", queueAdd{Text: "a"})
	mustAdd(t, q, "ot_1", queueAdd{Text: "b"})
	q.kicks.Wait()
	q.sweep()
	q.sweep()
	items, _ := q.List("ot_1")
	if texts(items) != "a/failed,b/queued" || items[0].Error == "" {
		t.Fatalf("one failure must not burn the queue: %s", texts(items))
	}
	// A cannot-steer refusal keeps the message queued.
	d.mu.Lock()
	d.sendErr = orchConflict("cannot steer")
	d.mu.Unlock()
	if _, _, err := q.SendNow("ot_1", items[1].ID); err == nil {
		t.Fatal("want the refusal")
	}
	items, _ = q.List("ot_1")
	if texts(items) != "a/failed,b/queued" || items[1].Error != "cannot steer" {
		t.Fatalf("refused send now: %s %q", texts(items), items[1].Error)
	}
}

func TestQueuePersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := openOrchStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeQDriven{st: "running"}
	q := testQueue(t, s, d, &fakeQWatched{})
	mustAdd(t, q, "ot_1", queueAdd{Text: "first", Attachments: []string{"up_a"}})
	b := mustAdd(t, q, "ot_1", queueAdd{Text: "second"})
	mustAdd(t, q, "claude:w", queueAdd{Text: "watched", Session: "claude:w"})
	q.kicks.Wait()
	if _, err := q.claim("ot_1", b.ID); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := openOrchStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s2.Close() })
	q2 := testQueue(t, s2, d, &fakeQWatched{})
	items, _ := q2.List("ot_1")
	if texts(items) != "first/queued,second/failed" || len(items[0].Attachments) != 1 || !strings.Contains(items[1].Error, "restarted") {
		t.Fatalf("after reopen: %s %+v", texts(items), items)
	}
	if w, _ := q2.List("claude:w"); len(w) != 1 || w[0].Session != "claude:w" {
		t.Fatalf("watched queue lost: %+v", w)
	}
}

func TestQueueDrivenAutoDispatchOnRunEnd(t *testing.T) {
	e := newOrchEnv(t)
	e.m.queue = newQueueManager(e.store, orchQueueDriven{e.m}, &fakeQWatched{})
	th := e.create("hang")
	e.wait("item.completed", func(d map[string]any) bool { return field(d, "item", "kind") == "agent" })
	mustAdd(t, e.m.queue, th.ID, queueAdd{Text: "next one"})
	mustAdd(t, e.m.queue, th.ID, queueAdd{Text: "last one"})
	e.m.queue.sweep()
	if len(e.runs(th.ID)) != 1 {
		t.Fatal("a running thread must not take a queued message")
	}
	if err := e.m.Interrupt(th.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		runs := e.runs(th.ID)
		items, _ := e.m.queue.List(th.ID)
		if len(runs) == 3 && runs[2].Status != "running" && len(items) == 0 {
			if runs[1].Prompt != "next one" || runs[2].Prompt != "last one" {
				t.Fatalf("prompts %q %q", runs[1].Prompt, runs[2].Prompt)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	items, _ := e.m.queue.List(th.ID)
	t.Fatalf("runs %d, queue %s", len(e.runs(th.ID)), texts(items))
}

func TestQueueDrivenSendNowSteers(t *testing.T) {
	e := newOrchEnv(t)
	e.m.queue = newQueueManager(e.store, orchQueueDriven{e.m}, &fakeQWatched{})
	th := e.create("steer")
	e.wait("item.completed", func(d map[string]any) bool { return field(d, "item", "kind") == "agent" })
	it := mustAdd(t, e.m.queue, th.ID, queueAdd{Text: "also do y"})
	_, mode, err := e.m.queue.SendNow(th.ID, it.ID)
	if err != nil || mode != "steer" {
		t.Fatalf("send now: %q %v", mode, err)
	}
	e.waitRunEnd()
	if got := lastAgentText(e.detail(th.ID)); got != "steered: also do y" {
		t.Fatalf("agent saw %q", got)
	}
}

func TestQueueAPI(t *testing.T) {
	e := newOrchEnv(t)
	w := &fakeQWatched{ph: "busy", pane: "✻ Working… (esc to interrupt)\n\n> fix the tests too\n> then push\n────────\n❯ \n────────\n  Press up to edit queued messages\n"}
	e.m.queue = testQueue(t, e.store, &fakeQDriven{st: "running"}, w)
	mux := http.NewServeMux()
	e.m.routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	base := srv.URL + "/api/queue/"
	th := "claude:abc"
	code, out := postJSON(t, base+th, `{"text":"one","session":"claude:abc"}`)
	if code != 201 || field(out, "item", "state") != "queued" {
		t.Fatalf("add: %d %v", code, out)
	}
	id := field(out, "item", "id")
	postJSON(t, base+th, `{"text":"two","session":"claude:abc"}`)
	if code, _ := postJSON(t, base+th, `{"text":"  "}`); code != 400 {
		t.Fatalf("empty text: %d", code)
	}
	if code, _ := postJSON(t, base+"ot_missing", `{"text":"x"}`); code != 201 {
		t.Fatalf("fake driven accepts: %d", code)
	}
	var v QueueView
	getJSONT(t, base+th, &v)
	if len(v.Items) != 2 || strings.Join(v.Terminal, "|") != "fix the tests too|then push" {
		t.Fatalf("get: %+v", v)
	}
	req, _ := http.NewRequest("PATCH", base+th+"/"+id, strings.NewReader(`{"index":1,"text":"one edited"}`))
	if r, err := http.DefaultClient.Do(req); err != nil || r.StatusCode != 200 {
		t.Fatalf("patch: %v %v", err, r)
	}
	getJSONT(t, base+th, &v)
	if texts(v.Items) != "two/queued,one edited/queued" {
		t.Fatalf("after patch: %s", texts(v.Items))
	}
	code, out = postJSON(t, base+th+"/"+id+"/send-now", `{}`)
	if code != 200 || out["mode"] != "pane" || len(w.got()) != 1 {
		t.Fatalf("send-now: %d %v", code, out)
	}
	if code, _ := postJSON(t, base+th+"/"+id+"/send-now", `{}`); code != 404 {
		t.Fatalf("sent item again: %d", code)
	}
	req, _ = http.NewRequest("DELETE", base+th, nil)
	if r, err := http.DefaultClient.Do(req); err != nil || r.StatusCode != 200 {
		t.Fatalf("clear: %v %v", err, r)
	}
	getJSONT(t, base+th, &v)
	if len(v.Items) != 0 {
		t.Fatalf("after clear: %s", texts(v.Items))
	}
}

func TestWatchedPhase(t *testing.T) {
	ev := func(kinds ...string) *Session {
		s := &Session{State: StateRunning}
		for _, k := range kinds {
			s.RecentEvents = append(s.RecentEvents, RecentEvent{Kind: k})
		}
		return s
	}
	cases := []struct {
		name string
		s    *Session
		ok   bool
		pane string
		want string
	}{
		{"no pane", ev("Stop"), false, "", "off"},
		{"stopped", ev("UserPromptSubmit", "Stop", "state:running"), true, "❯ ", "ready"},
		{"codex done", ev("task_started", "task_complete"), true, "", "ready"},
		{"tool running", ev("UserPromptSubmit", "PreToolUse"), true, "", "busy"},
		{"spinner wins", ev("Stop"), true, "✻ Thinking… (esc to interrupt)", "busy"},
		{"question open", ev("Stop"), true, "Do you want to proceed?\n❯ 1. Yes\n  2. No", "prompt"},
		{"unknown session", nil, true, "", "busy"},
	}
	for _, c := range cases {
		if got := watchedPhaseOf(c.s, c.ok, c.pane); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
	if got := tuiQueued("✻ Working (esc to interrupt)\n❯ "); got != nil {
		t.Errorf("no hint: %v", got)
	}
}

// A message that ends between two ticks gives no busy reading; the queue goes on after queueStall.
func TestQueueWatchedGoesOnAfterStall(t *testing.T) {
	s := newTestOrchStore(t)
	w := &fakeQWatched{ph: "ready"}
	q := testQueue(t, s, &fakeQDriven{}, w)
	clock := time.Now().UnixMilli()
	q.now = func() int64 { return clock }
	const th = "claude:abc"
	mustAdd(t, q, th, queueAdd{Text: "! ls", Session: th})
	mustAdd(t, q, th, queueAdd{Text: "two", Session: th})
	q.kicks.Wait()
	q.sweep()
	if got := w.got(); len(got) != 1 {
		t.Fatalf("first: %v", got)
	}
	clock += (queueStall - time.Second).Milliseconds()
	q.sweep()
	if len(w.got()) != 1 {
		t.Fatalf("sent before the stall limit: %v", w.got())
	}
	clock += (2 * time.Second).Milliseconds()
	q.sweep()
	if got := w.got(); len(got) != 2 || got[1] != "two" {
		t.Fatalf("after the stall limit: %v", got)
	}
}

// One non-ready reading in the window restarts the clock: the stall rule needs queueStall of ready in a row.
func TestQueueStallNeedsUnbrokenReady(t *testing.T) {
	s := newTestOrchStore(t)
	w := &fakeQWatched{ph: "ready"}
	q := testQueue(t, s, &fakeQDriven{}, w)
	clock := time.Now().UnixMilli()
	q.now = func() int64 { return clock }
	const th = "claude:abc"
	mustAdd(t, q, th, queueAdd{Text: "one", Session: th})
	mustAdd(t, q, th, queueAdd{Text: "two", Session: th})
	q.kicks.Wait()
	q.sweep()
	step := func(d time.Duration, ph string) { clock += d.Milliseconds(); w.set(ph); q.sweep() }
	step(15*time.Second, "ready")
	step(time.Second, "prompt") // a prompt opened and closed: not a busy reading, but not ready either
	step(9*time.Second, "ready")
	if got := w.got(); len(got) != 1 {
		t.Fatalf("sent across a broken ready window: %v", got)
	}
	step(12*time.Second, "ready") // 12 s since the clock restarted at the first ready reading after the break
	if got := w.got(); len(got) != 1 {
		t.Fatalf("sent before %v of unbroken ready: %v", queueStall, got)
	}
	step(9*time.Second, "ready")
	if got := w.got(); len(got) != 2 || got[1] != "two" {
		t.Fatalf("after %v of unbroken ready: %v", queueStall, got)
	}
}
