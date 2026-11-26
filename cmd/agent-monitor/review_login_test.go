package main

import (
	"net/http"
	"strings"
	"testing"
)

// F11: the cookie holds a session id, ?token= is ignored, and login failures are limited.
func TestReviewF11PasswordSession(t *testing.T) {
	e := newDevEnv(t, "pw")
	resetLoginLimiter()
	w := e.do("POST", "/api/login", `{"password":"pw"}`, remoteAddr, nil)
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == authCookieName {
			cookie = c
		}
	}
	if w.Code != 200 || cookie == nil || cookie.Value == "pw" || strings.Contains(cookie.Value, "pw") && len(cookie.Value) < 20 {
		t.Fatalf("login cookie %d %+v", w.Code, cookie)
	}
	if w := e.do("GET", "/api/secret", "", remoteAddr, func(r *http.Request) { r.AddCookie(cookie) }); w.Code != 200 {
		t.Fatalf("session cookie %d", w.Code)
	}
	if w := e.do("GET", "/api/secret", "", remoteAddr, func(r *http.Request) { r.AddCookie(&http.Cookie{Name: authCookieName, Value: "pw"}) }); w.Code != 401 {
		t.Fatalf("password as cookie accepted: %d", w.Code)
	}
	if w := e.do("GET", "/api/secret?token=pw", "", remoteAddr, nil); w.Code != 401 {
		t.Fatalf("password in URL accepted: %d", w.Code)
	}
	for i := 0; i < loginMaxFailures; i++ {
		if w := e.do("POST", "/api/login", `{"password":"nope"}`, "100.9.9.9:1", nil); w.Code != 401 {
			t.Fatalf("failure %d: %d", i, w.Code)
		}
	}
	if w := e.do("POST", "/api/login", `{"password":"pw"}`, "100.9.9.9:1", nil); w.Code != http.StatusTooManyRequests {
		t.Fatalf("login after %d failures: %d", loginMaxFailures, w.Code)
	}
	if w := e.do("POST", "/api/login", `{"password":"pw"}`, "100.9.9.10:1", nil); w.Code != 200 {
		t.Fatalf("other IP limited: %d", w.Code)
	}
}
