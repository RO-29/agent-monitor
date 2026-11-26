package main

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// F8: push endpoints must be known push services, and the client follows no redirect.
func TestReviewF8PushEndpointAllowlist(t *testing.T) {
	keys := testPushKeys(t)
	for ep, ok := range map[string]bool{
		"https://web.push.apple.com/QGx":                       true,
		"https://fcm.googleapis.com/fcm/send/abc":              true,
		"https://updates.push.services.mozilla.com/wpush/v2/x": true,
		"https://wns2-par02p.notify.windows.com/w/?token=x":    true,
		"https://ci.internal.example/x":                        false,
		"https://127.0.0.1:7777/api/x":                         false,
		"https://evilpush.apple.com/x":                         false,
		"https://web.push.apple.com.evil.example/x":            false,
		"https://web.push.apple.com:8443/x":                    false,
	} {
		s := keys
		s.Endpoint = ep
		if err := s.validate(); (err == nil) != ok {
			t.Errorf("%s: err=%v want ok=%v", ep, err, ok)
		}
	}
	var hit atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit.Add(1) }))
	defer target.Close()
	redir := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/x", http.StatusTemporaryRedirect)
	}))
	defer redir.Close()
	c := newPushClient()
	c.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	resp, err := c.Post(redir.URL+"/push", "application/octet-stream", strings.NewReader("x"))
	if err == nil {
		resp.Body.Close()
	}
	if hit.Load() != 0 {
		t.Fatal("push client followed a redirect")
	}
}

// S1: the default VAPID subject is not a localhost address, and the env value wins.
func TestReviewS1VapidSubject(t *testing.T) {
	t.Setenv("AGENT_MONITOR_VAPID_SUB", "")
	if s := vapidSubject(); strings.Contains(s, "localhost") || !(strings.HasPrefix(s, "mailto:") || strings.HasPrefix(s, "https://")) {
		t.Fatalf("default subject %q", s)
	}
	t.Setenv("AGENT_MONITOR_VAPID_SUB", "mailto:me@example.org")
	if vapidSubject() != "mailto:me@example.org" {
		t.Fatal("env subject ignored")
	}
	t.Setenv("AGENT_MONITOR_VAPID_SUB", "agent@localhost")
	if strings.Contains(vapidSubject(), "localhost") {
		t.Fatal("invalid env subject used")
	}
}
