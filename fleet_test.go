package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFleetThreadsTagsPeerRowsAndKeepsLastGood(t *testing.T) {
	url, code := peerPair(t, "view")
	if _, err := claimPeer(url, code, "mac2"); err != nil {
		t.Fatal(err)
	}
	prev := fleetGetFn
	t.Cleanup(func() { fleetGetFn = prev })
	fail := false
	fleetGetFn = func(p Peer, route string) (json.RawMessage, error) {
		if fail {
			return nil, errors.New("down")
		}
		if !strings.HasPrefix(route, "v3/threads") {
			t.Fatalf("route %q", route)
		}
		return json.RawMessage(`{"threads":[{"id":"t1","title":"Fix the build"},null]}`), nil
	}
	get := func() (out struct {
		Machines []fleetPeer      `json:"machines"`
		Threads  []map[string]any `json:"threads"`
	}) {
		mux := http.NewServeMux()
		registerFleetRoutes(mux)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/fleet/threads", nil))
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		return out
	}
	out := get()
	if len(out.Machines) != 2 || !out.Machines[0].Self || out.Machines[0].Level != "full" || out.Machines[1].Level != "view" || out.Machines[1].Status != "online" {
		t.Fatalf("machines %+v", out.Machines)
	}
	if len(out.Threads) != 1 || out.Threads[0]["machine"] != "mac2" || out.Threads[0]["level"] != "view" || out.Threads[0]["id"] != "t1" {
		t.Fatalf("threads %+v", out.Threads)
	}
	fail = true
	if again := get(); len(again.Threads) != 1 {
		t.Fatalf("the last good answer must stay for a minute: %+v", again.Threads)
	}
}

func TestFleetRoutesNeedAdmin(t *testing.T) {
	for _, p := range []string{"/api/fleet/threads", "/api/fleet/usage", "/api/fleet/machines"} {
		if l := routeLevel("GET", p); l != LevelAdmin {
			t.Fatalf("%s needs %v, want admin (no chaining through a machine grant)", p, l)
		}
	}
}

func TestMachineNameSetAndReset(t *testing.T) {
	t.Setenv("AGENT_MONITOR_HOME", t.TempDir())
	t.Cleanup(func() { _ = setMachineName("") })
	if err := setMachineName("Work Mac"); err != nil || selfMachineName() != "Work Mac" {
		t.Fatalf("set: %v %q", err, selfMachineName())
	}
	if setMachineName("a/b") == nil || setMachineName(strings.Repeat("x", 41)) == nil {
		t.Fatal("a bad name was kept")
	}
	if err := setMachineName(""); err != nil || selfMachineName() == "Work Mac" || selfMachineName() == "" {
		t.Fatalf("reset: %v %q", err, selfMachineName())
	}
}

func TestPeerRoutesForTheChatScreen(t *testing.T) {
	for _, c := range []struct {
		m, r string
		ok   bool
	}{
		{"GET", "session/claude:abc/items", true},
		{"GET", "session/claude:abc/children/a1/items", true},
		{"GET", "session/claude:abc/question", true},
		{"GET", "orch/threads/th_1/messages", true},
		{"POST", "session/claude:abc/answer", true},
		{"POST", "orch/continue", true},
		{"GET", "session/claude:abc/pane", false},
		{"GET", "orch/git", false},
		{"POST", "orch/terminal", false},
		{"GET", "fleet/threads", false},
	} {
		if peerRouteAllowed(c.m, c.r) != c.ok {
			t.Errorf("%s %s: want %v", c.m, c.r, c.ok)
		}
	}
}
