package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// peerPair runs an owner daemon (device routes and stubs behind authMiddleware) that sees every request
// as coming from another machine, and returns its URL and a machine pairing code at level.
func peerPair(t *testing.T, level string) (string, string) {
	t.Helper()
	prevStore, prevPw, prevOK := deviceStore, authPassword, peerLoopbackOK
	t.Setenv("AGENT_MONITOR_HOME", t.TempDir())
	ds, err := openDeviceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	deviceStore, authPassword, peerLoopbackOK = ds, "", true
	peerStore.Lock()
	peerStore.peers, peerStore.read = nil, false
	peerStore.Unlock()
	t.Cleanup(func() { ds.Close(); deviceStore, authPassword, peerLoopbackOK = prevStore, prevPw, prevOK })
	mux := http.NewServeMux()
	registerDeviceRoutes(mux)
	stub := func(w http.ResponseWriter, r *http.Request) { writeJSON(w, map[string]any{"ok": true, "level": callerLevel(r).String()}) }
	mux.HandleFunc("GET /api/needs-you", stub)
	mux.HandleFunc("GET /api/health", stub)
	mux.HandleFunc("GET /api/term", stub)
	mux.HandleFunc("GET /api/peers", stub)
	mux.HandleFunc("POST /api/approvals/{id}/decide", stub)
	remote := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = "100.64.0.50:40000" // the viewer is another machine
		authMiddleware(mux).ServeHTTP(w, r)
	})
	srv := httptest.NewServer(remote)
	t.Cleanup(srv.Close)
	_, tok, err := ds.CreateGrant("viewer-mac", "machine", level)
	if err != nil {
		t.Fatal(err)
	}
	return srv.URL, ds.NewPairCode(tok)
}

func viewerReq(method, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, path, strings.NewReader("{}"))
	servePeerProxy(w, r)
	return w
}

func TestPeerClaimAndViewThroughProxy(t *testing.T) {
	url, code := peerPair(t, "view")
	p, err := claimPeer(url, code, "mac2")
	if err != nil || p.Level != "view" || !strings.HasPrefix(p.Token, deviceTokenPrefix) {
		t.Fatalf("claim: %+v %v", p, err)
	}
	if _, err := claimPeer(url, code, "mac2"); err == nil {
		t.Fatal("a pairing code was claimed twice")
	}
	if w := viewerReq("GET", "/api/m/mac2/api/needs-you"); w.Code != 200 || !strings.Contains(w.Body.String(), `"level":"view"`) {
		t.Fatalf("needs-you through the proxy: %d %s", w.Code, w.Body)
	}
	if w := viewerReq("GET", "/api/m/mac2/api/term"); w.Code != 403 {
		t.Fatalf("a terminal is not a peer route: %d", w.Code)
	}
	if w := viewerReq("POST", "/api/m/mac2/api/approvals/ap_1/decide"); w.Code != 403 {
		t.Fatalf("view on the owner cannot decide: %d %s", w.Code, w.Body)
	}
	if w := viewerReq("GET", "/api/m/nobody/api/needs-you"); w.Code != 404 {
		t.Fatalf("unknown peer: %d", w.Code)
	}
}

func TestPeerControlAndNoChaining(t *testing.T) {
	url, code := peerPair(t, "control")
	p, err := claimPeer(url, code, "mac2")
	if err != nil {
		t.Fatal(err)
	}
	if w := viewerReq("POST", "/api/m/mac2/api/approvals/ap_1/decide"); w.Code != 200 {
		t.Fatalf("control decides: %d %s", w.Code, w.Body)
	}
	// The grant token itself never reaches the owner's own peer list: a machine is never admin.
	r, _ := http.NewRequest("GET", url+"/api/peers", nil)
	r.Header.Set("Authorization", "Bearer "+p.Token)
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("machine grant reached /api/peers: %d", resp.StatusCode)
	}
}

func TestPeerURLRefusesLocal(t *testing.T) {
	prev := peerLoopbackOK
	peerLoopbackOK = false
	t.Cleanup(func() { peerLoopbackOK = prev })
	for _, u := range []string{"http://127.0.0.1:7777", "http://localhost:7777", "http://0.0.0.0:7777", "ftp://100.64.0.9", "http://100.64.0.9:7777/api", "http://user@100.64.0.9:7777"} {
		if _, err := peerURLAllowed(u); err == nil {
			t.Errorf("%s accepted", u)
		}
	}
	if _, err := peerURLAllowed("http://100.64.0.9:7777"); err != nil {
		t.Errorf("a tailnet address was refused: %v", err)
	}
}
