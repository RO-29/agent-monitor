package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestTerminalUseRule(t *testing.T) {
	const mark = 1_000_000
	dead := 1 << 30
	cases := []struct {
		name        string
		s           *Session
		reg         *PaneRegistration
		after, live bool
	}{
		{"no session", nil, nil, false, false},
		{"driven writes inside the grace", &Session{LastActivityAt: mark + handoverGrace, State: StateAwaitingInput}, nil, false, false},
		{"terminal resumed then ended", &Session{LastActivityAt: mark + 60_000, State: StateIdle}, nil, true, false},
		{"terminal running", &Session{LastActivityAt: mark + 60_000, State: StateRunning}, nil, true, true},
		{"stale active state before mark", &Session{LastActivityAt: mark - 1, State: StateAwaitingInput}, nil, false, false},
		{"live pid", &Session{LastActivityAt: mark - 1, Pid: os.Getpid()}, nil, true, true},
		{"pane registered after mark", &Session{LastActivityAt: mark}, &PaneRegistration{PaneID: "%1", RegisteredAt: mark + 1}, true, true},
		{"old pane", &Session{LastActivityAt: mark}, &PaneRegistration{PaneID: "%1", RegisteredAt: mark - 1}, false, false},
		{"pane with dead pid", nil, &PaneRegistration{PaneID: "%1", RegisteredAt: mark + 1, Pid: dead}, false, false},
	}
	for _, c := range cases {
		after, live := terminalUse(mark, c.s, c.reg)
		if after != c.after || live != c.live {
			t.Errorf("%s: after=%v live=%v, want %v %v", c.name, after, live, c.after, c.live)
		}
	}
}

// continueEnv continues a fake ended codex session with no message and returns the new thread.
func continueEnv(t *testing.T) (*orchEnv, *OThread, *httptest.Server) {
	e := drivenViewEnv(t)
	e.m.watched = func(id string) (*orchWatched, error) {
		if id == "codex:thr_cont" {
			return &orchWatched{Agent: "codex", NativeID: "thr_cont", Cwd: e.cwd, Title: "old work"}, nil
		}
		return nil, errOrchNotFound
	}
	mux := http.NewServeMux()
	e.m.routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	code, out := postJSON(t, srv.URL+"/api/orch/continue", `{"sessionId":"codex:thr_cont"}`)
	if code != 201 || field(out, "thread", "status") != "idle" || field(out, "thread", "nativeId") != "thr_cont" {
		t.Fatalf("continue: %d %v", code, out)
	}
	th := e.thread(field(out, "thread", "id"))
	if len(e.runs(th.ID)) != 0 || e.m.procAlive(th.ID) {
		t.Fatal("continue without a message must not start a run or a process")
	}
	return e, th, srv
}

func listed(id string) (watched bool, driven *ThreadV3) {
	for _, r := range threadsResponse(true, false)["threads"].([]ThreadV3) {
		if r.ID == id {
			watched = true
		}
		if r.Kind == "driven" {
			r := r
			driven = &r
		}
	}
	return
}

func TestHandoverMerge(t *testing.T) {
	_, th, _ := continueEnv(t)
	sid := "codex:thr_cont"
	// The ended session, last touched before Continue: the driven thread holds it.
	store.sessions[sid] = &Session{ID: sid, Tool: ToolCodex, SessionID: "thr_cont", Cwd: "/x", StartedAt: 1, LastActivityAt: th.CreatedAt - 1000, State: StateCompleted}
	if orchDrivenFor([]string{sid}) != th.ID {
		t.Fatal("empty continued thread must hold its session")
	}
	if w, d := listed(sid); w || d == nil || d.Handover != "driven" || d.TerminalSession != "" {
		t.Fatalf("held: watched=%v driven=%+v", w, d)
	}
	// The user resumes it in a terminal: the watched copy shows and the driven row points at it.
	store.sessions[sid].LastActivityAt = th.CreatedAt + 60_000
	store.sessions[sid].State = StateIdle
	if orchDrivenFor([]string{sid}) != "" {
		t.Fatal("terminal activity after the driven thread must release the session")
	}
	if w, d := listed(sid); !w || d == nil || d.Handover != "terminal" || d.TerminalSession != sid {
		t.Fatalf("handed over: watched=%v driven=%+v", w, d)
	}
}

func TestSendRefusedWhileTerminalLive(t *testing.T) {
	e, th, srv := continueEnv(t)
	sid := "codex:thr_cont"
	store.sessions[sid] = &Session{ID: sid, Tool: ToolCodex, SessionID: "thr_cont", Cwd: "/x", StartedAt: 1, LastActivityAt: th.CreatedAt + 60_000, State: StateRunning}
	code, out := postJSON(t, srv.URL+"/api/orch/threads/"+th.ID+"/messages", `{"text":"hello"}`)
	if code != 409 || out["error"] != "live" {
		t.Fatalf("send while the terminal runs: %d %v", code, out)
	}
	if code, out := postJSON(t, srv.URL+"/api/orch/continue", `{"sessionId":"codex:thr_cont","prompt":"hi"}`); code != 409 || out["error"] != "live" {
		t.Fatalf("continue with a prompt while the terminal runs: %d %v", code, out)
	}
	if len(e.runs(th.ID)) != 0 {
		t.Fatal("a refused send started a run")
	}
	// The terminal ended: the driven thread may take the session back.
	store.sessions[sid].State = StateIdle
	if code, out := postJSON(t, srv.URL+"/api/orch/threads/"+th.ID+"/messages", `{"text":"hello"}`); code != 200 {
		t.Fatalf("send after the terminal ended: %d %v", code, out)
	}
	e.idle(th.ID)
	// Driven turns write to the same transcript; that activity is not the terminal's.
	store.sessions[sid].LastActivityAt = e.m.store.LastRunAt(th.ID)
	if orchDrivenFor([]string{sid}) != th.ID {
		t.Fatal("a driven turn after the terminal must take the session back")
	}
}
