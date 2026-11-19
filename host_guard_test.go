package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHostGuardBlocksRebinding(t *testing.T) {
	t.Setenv("AGENT_MONITOR_ALLOWED_HOSTS", "mac.tail1234.ts.net")
	ok := hostGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	for host, want := range map[string]int{
		"127.0.0.1:7777": 204, "localhost:7777": 204, "[::1]:7777": 204, "100.64.0.10:7777": 204,
		"mac.tail1234.ts.net": 204, "MAC.tail1234.ts.net:443": 204,
		"evil.example:7777": 421, "localhost.evil.example": 421, "": 421,
	} {
		r := httptest.NewRequest("POST", "/api/orch/threads", nil)
		r.Host = host
		w := httptest.NewRecorder()
		ok.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("Host %q: got %d, want %d", host, w.Code, want)
		}
	}
}

func TestAuthTreatsProxiedLoopbackAsRemote(t *testing.T) {
	old := authPassword
	authPassword = "pw"
	defer func() { authPassword = old }()
	h := authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	r := httptest.NewRequest("GET", "/api/orch/threads", nil)
	r.RemoteAddr = "127.0.0.1:5555"
	r.Header.Set("X-Forwarded-For", "100.1.2.3")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code == 204 {
		t.Fatal("proxied loopback request passed auth with no credential")
	}
	r = httptest.NewRequest("GET", "/api/orch/threads", nil)
	r.RemoteAddr = "127.0.0.1:5555"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 204 {
		t.Fatalf("plain loopback refused: %d", w.Code)
	}
}

func TestRemoteCannotMintDeviceOrFlipSwitches(t *testing.T) {
	ds, err := openDeviceStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()
	oldStore, oldPw := deviceStore, authPassword
	deviceStore, authPassword = ds, "pw"
	defer func() { deviceStore, authPassword = oldStore, oldPw }()
	mux := http.NewServeMux()
	registerDeviceRoutes(mux)
	r := httptest.NewRequest("POST", "/api/devices", nil)
	r.RemoteAddr = "100.1.2.3:5555"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("remote password-only device create: %d, want 403", w.Code)
	}
}
