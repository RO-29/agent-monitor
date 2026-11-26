package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPprofOnlyFromLoopback(t *testing.T) {
	mux := http.NewServeMux()
	registerLocalPprof(mux)
	for addr, want := range map[string]int{"127.0.0.1:5000": 200, "[::1]:5000": 200, "100.64.0.10:5000": 404, "192.168.1.9:5000": 404} {
		r := httptest.NewRequest("GET", "/debug/pprof/", nil)
		r.RemoteAddr = addr
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("%s: got %d, want %d", addr, w.Code, want)
		}
	}
}
