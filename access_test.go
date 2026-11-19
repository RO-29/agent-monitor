package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRouteLevel(t *testing.T) {
	cases := []struct {
		method, path string
		want         Level
	}{
		{"POST", "/events/claude", levelLocal},
		{"POST", "/api/approvals/hook/claude-permission", levelLocal},
		{"POST", "/api/pane/announce", levelLocal},
		{"POST", "/api/hosts", levelLocal},
		{"GET", "/debug/pprof/heap", levelLocal},
		{"GET", "/api/needs-you", LevelView},
		{"GET", "/ws", LevelView},
		{"GET", "/api/session/claude:x/items", LevelView},
		{"GET", "/api/session/claude:x/place", LevelView},
		{"GET", "/api/orch/threads/ot_1", LevelView},
		{"GET", "/w/ot_1", LevelView}, // the app page
		{"POST", "/api/approvals/ap_1/decide", LevelControl},
		{"POST", "/api/session/prompt/claude:x", LevelControl},
		{"POST", "/api/session/claude:x/cli-queue/send", LevelControl},
		{"POST", "/api/orch/threads/ot_1/send", LevelControl},
		{"POST", "/api/hosts/h1/answer", LevelControl},
		{"POST", "/api/permission/p1/respond", LevelControl},
		{"POST", "/api/push/presence", LevelView},
		{"GET", "/api/session/claude:x/term", LevelAdmin},
		{"GET", "/api/term", LevelAdmin},
		{"GET", "/api/session/claude:x/pane", LevelAdmin},
		{"GET", "/api/search", LevelAdmin},
		{"GET", "/api/orch/threads/ot_1/diff", LevelAdmin},
		{"POST", "/api/approvals/rules", LevelAdmin},
		{"POST", "/api/settings", LevelAdmin},
		{"POST", "/api/push/bark", LevelAdmin},
		{"GET", "/api/devices", LevelAdmin},
		{"GET", "/api/some-new-route", LevelAdmin}, // a route with no rule is closed to view and control
	}
	for _, c := range cases {
		if got := routeLevel(c.method, c.path); got != c.want {
			t.Errorf("%s %s: got %s want %s", c.method, c.path, got, c.want)
		}
	}
}

// accessHarness serves a stub for every path through authMiddleware, with devices at each level.
func accessHarness(t *testing.T) (http.Handler, map[string]string) {
	t.Helper()
	prevStore, prevPw := deviceStore, authPassword
	t.Setenv("AGENT_MONITOR_HOME", t.TempDir())
	ds, err := openDeviceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	deviceStore, authPassword = ds, ""
	t.Cleanup(func() { ds.Close(); deviceStore, authPassword = prevStore, prevPw })
	tok := map[string]string{}
	for _, g := range []struct{ name, kind, level string }{
		{"phone", "device", "admin"}, {"viewer", "device", "view"}, {"driver", "device", "control"}, {"mac2", "machine", "control"},
	} {
		_, tk, err := ds.CreateGrant(g.name, g.kind, g.level)
		if err != nil {
			t.Fatal(err)
		}
		tok[g.name] = tk
	}
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Level", callerLevel(r).String())
		w.WriteHeader(http.StatusOK)
	})
	return authMiddleware(ok), tok
}

func remoteReq(method, path, token, ip string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader("{}"))
	r.RemoteAddr = ip + ":40000"
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func TestAccessLevelsFromAnotherMachine(t *testing.T) {
	h, tok := accessHarness(t)
	const ip = "100.64.0.9"
	cases := []struct {
		who, method, path string
		want              int
	}{
		{"", "GET", "/api/needs-you", 401}, // no token, no password: refused even with the requirement off
		{"viewer", "GET", "/api/needs-you", 200},
		{"viewer", "POST", "/api/approvals/ap_1/decide", 403},
		{"viewer", "GET", "/api/session/claude:x/term", 403},
		{"viewer", "GET", "/api/search", 403},
		{"viewer", "POST", "/events/claude", 403},
		{"driver", "POST", "/api/approvals/ap_1/decide", 200},
		{"driver", "POST", "/api/session/prompt/claude:x", 200},
		{"driver", "GET", "/api/term", 403},
		{"driver", "POST", "/api/approvals/rules", 403},
		{"phone", "GET", "/api/term", 200},
		{"phone", "POST", "/api/approvals/rules", 200},
		{"phone", "POST", "/events/claude", 403}, // hooks come from this Mac only, whatever the level
		{"phone", "POST", "/api/approvals/hook/claude-permission", 403},
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, remoteReq(c.method, c.path, tok[c.who], ip))
		if w.Code != c.want {
			t.Errorf("%s %s %s: got %d want %d", c.who, c.method, c.path, w.Code, c.want)
		}
	}
}

func TestMachineGrantBoundAndCapped(t *testing.T) {
	h, tok := accessHarness(t)
	d := deviceStore.Lookup(tok["mac2"])
	deviceStore.BindIP(d.ID, "100.64.0.20")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, remoteReq("GET", "/api/needs-you", tok["mac2"], "100.64.0.20"))
	if w.Code != 200 || w.Header().Get("X-Level") != "control" {
		t.Fatalf("bound address: %d level %q", w.Code, w.Header().Get("X-Level"))
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, remoteReq("GET", "/api/needs-you", tok["mac2"], "100.64.0.21"))
	if w.Code != 401 {
		t.Fatalf("another address: %d", w.Code)
	}
	if _, _, err := deviceStore.SetLevel(d.ID, "admin"); err == nil {
		t.Fatal("a machine grant became admin")
	}
	if _, _, err := deviceStore.CreateGrant("m3", "machine", "admin"); err == nil {
		t.Fatal("a machine grant was created as admin")
	}
}

func TestLevelChangeAndRevokeTakeEffect(t *testing.T) {
	h, tok := accessHarness(t)
	d := deviceStore.Lookup(tok["viewer"])
	if _, _, err := deviceStore.SetLevel(d.ID, "control"); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, remoteReq("POST", "/api/approvals/ap_1/decide", tok["viewer"], "100.64.0.9"))
	if w.Code != 200 {
		t.Fatalf("after raise to control: %d", w.Code)
	}
	if _, _, err := deviceStore.Revoke(d.ID); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, remoteReq("GET", "/api/needs-you", tok["viewer"], "100.64.0.9"))
	if w.Code != 401 {
		t.Fatalf("after revoke: %d", w.Code)
	}
}

func TestLocalCallerIsAdmin(t *testing.T) {
	h, _ := accessHarness(t)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, remoteReq("POST", "/events/claude", "", "127.0.0.1"))
	if w.Code != 200 || w.Header().Get("X-Level") != "admin" {
		t.Fatalf("local hook: %d %q", w.Code, w.Header().Get("X-Level"))
	}
	r := remoteReq("POST", "/events/claude", "", "127.0.0.1")
	r.Header.Set("X-Forwarded-For", "100.64.0.9") // a forwarded request is not this Mac
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("forwarded hook: %d", w.Code)
	}
}

// A sender below admin may allow once, but may not add a lasting rule with "always".
func TestControlCannotAddAlwaysRule(t *testing.T) {
	e := newApprovalEnv(t)
	ch := e.startHook(hookClaudePermission, claudeBash("npm test --watch=false"))
	a := e.waitPending()
	if _, code, err := e.s.decide(a.ID, approvalDecision{Behavior: "allow", Always: true, limited: true}); err == nil || code != http.StatusForbidden {
		t.Fatalf("always from control: code %d err %v", code, err)
	}
	if _, code, err := e.s.decide(a.ID, approvalDecision{Behavior: "allow", ApplySuggestions: true, limited: true}); err == nil || code != http.StatusForbidden {
		t.Fatalf("applySuggestions from control: code %d err %v", code, err)
	}
	if _, _, err := e.s.decide(a.ID, approvalDecision{Behavior: "allow", limited: true}); err != nil {
		t.Fatalf("allow once from control: %v", err)
	}
	waitResult(t, ch)
	for _, r := range e.s.rules {
		if r.Tool == "Bash" {
			t.Fatalf("a Bash rule was added: %+v", r)
		}
	}
}
