package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestOrchStore(t *testing.T) *OrchStore {
	t.Helper()
	s, err := openOrchStore(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func putTestThread(t *testing.T, s *OrchStore, th *OThread) {
	t.Helper()
	if err := s.write(func(tx *orchTx) error {
		if err := tx.putThread(th); err != nil {
			return err
		}
		tx.emit(th.ID, "thread.updated", map[string]any{"thread": th})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOrchStoreSeqAndSnapshot(t *testing.T) {
	s := newTestOrchStore(t)
	putTestThread(t, s, &OThread{ID: "t1", Kind: "driven", Agent: "codex", Status: "idle", UpdatedAt: 1})
	putTestThread(t, s, &OThread{ID: "t2", Kind: "driven", Agent: "codex", Status: "idle", UpdatedAt: 2})
	if err := s.write(func(tx *orchTx) error {
		run := &ORun{ID: "r1", ThreadID: "t1", Status: "running"}
		it := &OItem{ID: "r1.a", ThreadID: "t1", RunID: "r1", Kind: "agent", Status: "started"}
		if err := tx.putRun(run); err != nil {
			return err
		}
		if err := tx.putItem(it); err != nil {
			return err
		}
		tx.emit("t1", "run.started", map[string]any{"run": run})
		tx.emit("t1", "item.started", map[string]any{"item": it})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	threads, seq, err := s.Threads()
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 2 || threads[0].ID != "t2" || seq != 4 {
		t.Fatalf("threads=%v seq=%d", threads, seq)
	}
	d, err := s.ThreadDetail("t1")
	if err != nil {
		t.Fatal(err)
	}
	if d.Seq != 4 || len(d.Runs) != 1 || len(d.Items) != 1 || d.Requests == nil {
		t.Fatalf("detail %+v", d)
	}
	if _, err := s.ThreadDetail("nope"); err != errOrchNotFound {
		t.Fatalf("want not found, got %v", err)
	}
	evs, _ := s.EventsAfter(2, 100)
	if len(evs) != 2 || evs[0].Seq != 3 || evs[0].Type != "run.started" || evs[1].Type != "item.started" {
		t.Fatalf("events %+v", evs)
	}
}

func TestOrchStoreDeltaCoalescing(t *testing.T) {
	s := newTestOrchStore(t)
	putTestThread(t, s, &OThread{ID: "t1", Status: "running"})
	_ = s.write(func(tx *orchTx) error {
		return tx.putItem(&OItem{ID: "i1", ThreadID: "t1", RunID: "r1", Kind: "agent", Status: "started"})
	})
	for _, p := range []string{"he", "ll", "o"} {
		s.AppendDelta("t1", "i1", "text", p)
	}
	s.AppendDelta("t1", "i1", "output", "out")
	var evs []OEvent
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		s.dmu.Lock()
		buffered := len(s.deltas)
		s.dmu.Unlock()
		evs, _ = s.EventsAfter(1, 100)
		if buffered == 0 && len(evs) >= 2 || time.Now().After(deadline) {
			break
		}
	}
	if len(evs) != 2 {
		t.Fatalf("want 2 coalesced deltas, got %d: %+v", len(evs), evs)
	}
	var d0 map[string]any
	_ = json.Unmarshal(evs[0].Data, &d0)
	if evs[0].Type != "item.delta" || d0["text"] != "hello" || d0["itemId"] != "i1" {
		t.Fatalf("delta %s", evs[0].Data)
	}
	d, _ := s.ThreadDetail("t1")
	if d.Items[0].Text != "hello" || d.Items[0].Output != "out" {
		t.Fatalf("snapshot text %q output %q", d.Items[0].Text, d.Items[0].Output)
	}
	// An explicit flush writes only that item's pending text.
	s.AppendDelta("t1", "i1", "text", "!")
	s.flushDeltas("i1")
	d, _ = s.ThreadDetail("t1")
	if d.Items[0].Text != "hello!" {
		t.Fatalf("after flush %q", d.Items[0].Text)
	}
}

func TestOrchStorePruneKeepsSeq(t *testing.T) {
	s := newTestOrchStore(t)
	putTestThread(t, s, &OThread{ID: "t1"})
	putTestThread(t, s, &OThread{ID: "t1"})
	if n, err := s.Prune(time.Now().Add(8 * 24 * time.Hour)); err != nil || n != 2 {
		t.Fatalf("prune n=%d err=%v", n, err)
	}
	putTestThread(t, s, &OThread{ID: "t1"})
	evs, _ := s.EventsAfter(0, 10)
	if len(evs) != 1 || evs[0].Seq != 3 {
		t.Fatalf("seq must keep growing after a prune: %+v", evs)
	}
	if n, _ := s.Prune(time.Now()); n != 0 {
		t.Fatalf("fresh events pruned: %d", n)
	}
}

func TestOrchStoreRecoverAfterRestart(t *testing.T) {
	s := newTestOrchStore(t)
	putTestThread(t, s, &OThread{ID: "t1", Status: "running", NativeID: "n1"})
	putTestThread(t, s, &OThread{ID: "t2", Status: "closed"})
	_ = s.write(func(tx *orchTx) error {
		_ = tx.putRun(&ORun{ID: "r1", ThreadID: "t1", Status: "running"})
		_ = tx.putItem(&OItem{ID: "r1.a", ThreadID: "t1", RunID: "r1", Kind: "command", Status: "started"})
		return tx.putRequest(&ORequest{ID: "q1", ThreadID: "t1", RunID: "r1", Kind: "command", ApprovalID: "ap_x", State: "open"})
	})
	if err := s.RecoverAfterRestart(); err != nil {
		t.Fatal(err)
	}
	d, _ := s.ThreadDetail("t1")
	if d.Thread.Status != "idle" || d.Thread.NativeID != "n1" || d.Runs[0].Status != "interrupted" ||
		d.Items[0].Status != "failed" || d.Requests[0].State != "closed" {
		t.Fatalf("recover: %+v %+v %+v %+v", d.Thread, d.Runs, d.Items, d.Requests)
	}
	if th, _ := s.Thread("t2"); th.Status != "closed" {
		t.Fatalf("closed thread changed: %s", th.Status)
	}
}

func TestOrchStoreRecoverListsTurnsToContinue(t *testing.T) {
	s := newTestOrchStore(t)
	now := time.Now().UnixMilli()
	for _, id := range []string{"fresh", "old", "bang"} {
		putTestThread(t, s, &OThread{ID: id, Status: "running"})
	}
	_ = s.write(func(tx *orchTx) error {
		_ = tx.putRun(&ORun{ID: "r1", ThreadID: "fresh", Status: "running", StartedAt: now - 60_000})
		_ = tx.putRun(&ORun{ID: "r2", ThreadID: "old", Status: "running", StartedAt: now - (7 * time.Hour).Milliseconds()})
		return tx.putRun(&ORun{ID: "ob_1", ThreadID: "bang", Status: "running", StartedAt: now})
	})
	if err := s.RecoverAfterRestart(); err != nil {
		t.Fatal(err)
	}
	if len(s.interrupted) != 1 || s.interrupted[0] != "fresh" {
		t.Fatalf("interrupted = %v, want [fresh]", s.interrupted)
	}
}

// readSSE returns a function that reads the next data event from a stream.
func readSSE(t *testing.T, r *bufio.Reader) func() OEvent {
	return func() OEvent {
		t.Helper()
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				t.Fatalf("stream read: %v", err)
			}
			if data, ok := strings.CutPrefix(strings.TrimRight(line, "\n"), "data: "); ok {
				var ev OEvent
				if err := json.Unmarshal([]byte(data), &ev); err != nil {
					t.Fatalf("bad event %q: %v", data, err)
				}
				return ev
			}
		}
	}
}

func TestOrchStreamReplayThenLive(t *testing.T) {
	s := newTestOrchStore(t)
	for i := 0; i < 3; i++ {
		putTestThread(t, s, &OThread{ID: "t1", UpdatedAt: int64(i)})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serveOrchStream(s, w, r) }))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "?after=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}
	next := readSSE(t, bufio.NewReader(resp.Body))
	if e := next(); e.Seq != 2 {
		t.Fatalf("first replayed seq %d", e.Seq)
	}
	if e := next(); e.Seq != 3 {
		t.Fatalf("second replayed seq %d", e.Seq)
	}
	putTestThread(t, s, &OThread{ID: "t1", Status: "idle"})
	e := next()
	if e.Seq != 4 || e.Type != "thread.updated" || e.ThreadID != "t1" {
		t.Fatalf("live event %+v", e)
	}
}

func TestOrchStreamHeartbeat(t *testing.T) {
	s := newTestOrchStore(t)
	old := orchHeartbeat
	orchHeartbeat = 50 * time.Millisecond
	defer func() { orchHeartbeat = old }()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { serveOrchStream(s, w, r) }))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == ": hb\n" {
			return
		}
	}
	t.Fatal("no heartbeat")
}
