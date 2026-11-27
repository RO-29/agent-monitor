package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestServeOnceGivesTheValueOneTime(t *testing.T) {
	dir := t.TempDir()
	path, err := serveOnce(dir, []byte("hunter2"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "hunter2" {
		t.Fatalf("first read = %q, %v", got, err)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the pipe is still there after one read: %v", err)
	}
	p2, _ := serveOnce(dir, []byte("x"), 50*time.Millisecond)
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(p2); !os.IsNotExist(err) {
		t.Fatal("an unread pipe outlived its time")
	}
}

func TestSecretCardFlow(t *testing.T) {
	prev := secrets
	dir := t.TempDir()
	secrets = &secretStore{reqs: map[string]*SecretRequest{}, dir: func() string { return dir }}
	t.Cleanup(func() { secrets = prev })
	mux := http.NewServeMux()
	registerSecretRoutes(mux)
	call := func(method, path, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
		return w
	}
	w := call("POST", "/api/secrets/request", `{"name":"staging DB password","reason":"psql"}`)
	var req SecretRequest
	if err := json.Unmarshal(w.Body.Bytes(), &req); err != nil || req.ID == "" {
		t.Fatalf("request: %d %s", w.Code, w.Body)
	}
	if strings.Contains(call("GET", "/api/secrets", "").Body.String(), "hunter2") {
		t.Fatal("the list holds a value")
	}
	done := make(chan map[string]any, 1)
	go func() {
		var out map[string]any
		_ = json.Unmarshal(call("GET", "/api/secrets/"+req.ID+"/await?timeout=5", "").Body.Bytes(), &out)
		done <- out
	}()
	time.Sleep(50 * time.Millisecond)
	if w := call("POST", "/api/secrets/"+req.ID+"/answer", `{"type":"save","secret":"hunter2"}`); w.Code != 200 {
		t.Fatalf("answer: %d %s", w.Code, w.Body)
	}
	if w := call("POST", "/api/secrets/"+req.ID+"/answer", `{"type":"save","secret":"again"}`); w.Code != http.StatusConflict {
		t.Fatalf("second answer = %d, want 409", w.Code)
	}
	out := <-done
	file, _ := out["file"].(string)
	if out["state"] != "saved" || file == "" {
		t.Fatalf("await = %v", out)
	}
	if b, err := os.ReadFile(file); err != nil || string(b) != "hunter2" {
		t.Fatalf("pipe read = %q, %v", b, err)
	}
}

func TestSecretSaveNeedsASignatureButDeclineDoesNot(t *testing.T) {
	save := httptest.NewRequest("POST", "/api/secrets/sec_1/answer", strings.NewReader(`{"type":"save","secret":"x"}`))
	if !bioRoute(save, LevelControl) || plainDeny(save) {
		t.Fatal("a save must need Face ID or a passkey")
	}
	decline := httptest.NewRequest("POST", "/api/secrets/sec_1/answer", strings.NewReader(`{"type":"decline"}`))
	if !plainDeny(decline) {
		t.Fatal("a decline must not need a signature")
	}
	if routeLevel("POST", "/api/secrets/request") != levelLocal || routeLevel("GET", "/api/secrets/sec_1/await") != levelLocal {
		t.Fatal("only this Mac may open a card or wait for one")
	}
}
