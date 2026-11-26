package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func newBioKey(t *testing.T) (*ecdsa.PrivateKey, string) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := k.PublicKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return k, base64.StdEncoding.EncodeToString(raw)
}

// signBio sets X-AM-Bio the way the iPhone app does.
func signBio(t *testing.T, r *http.Request, k *ecdsa.PrivateKey, body string, at time.Time) {
	t.Helper()
	ms := at.UnixMilli()
	h := sha256.Sum256(bioMessage(r.Method, r.URL.Path, ms, []byte(body)))
	der, err := ecdsa.SignASN1(rand.Reader, k, h[:])
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set(bioHeader, strconv.FormatInt(ms, 10)+"."+base64.StdEncoding.EncodeToString(der))
}

func bioReq(method, path, body, token string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "100.64.0.9:40000"
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

func TestBioGateNeedsAFreshSignature(t *testing.T) {
	h, tok := accessHarness(t)
	k, pub := newBioKey(t)
	d := deviceStore.Lookup(tok["phone"])
	if _, err := deviceStore.SetBioKey(d.ID, pub); err != nil {
		t.Fatal(err)
	}
	const path, body = "/api/approvals/ap_1/decide", `{"decision":"allow"}`
	run := func(r *http.Request) int {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if c := run(bioReq("POST", path, body, tok["phone"])); c != http.StatusPreconditionRequired {
		t.Fatalf("unsigned decide = %d, want 428", c)
	}
	good := bioReq("POST", path, body, tok["phone"])
	signBio(t, good, k, body, time.Now())
	if c := run(good); c != http.StatusOK {
		t.Fatalf("signed decide = %d, want 200", c)
	}
	again := bioReq("POST", path, body, tok["phone"])
	again.Header.Set(bioHeader, good.Header.Get(bioHeader))
	if c := run(again); c != http.StatusPreconditionRequired {
		t.Fatalf("replayed signature = %d, want 428", c)
	}
	other := bioReq("POST", path, `{"decision":"deny"}`, tok["phone"])
	signBio(t, other, k, body, time.Now())
	if c := run(other); c != http.StatusPreconditionRequired {
		t.Fatalf("signature over another body = %d, want 428", c)
	}
	old := bioReq("POST", path, body, tok["phone"])
	signBio(t, old, k, body, time.Now().Add(-2*time.Minute))
	if c := run(old); c != http.StatusPreconditionRequired {
		t.Fatalf("old signature = %d, want 428", c)
	}
	if c := run(bioReq("POST", path, `{"behavior":"deny"}`, tok["phone"])); c != http.StatusOK {
		t.Fatalf("a plain deny = %d, want 200 with no Face ID", c)
	}
	if c := run(bioReq("POST", path, `{"behavior":"deny","always":true}`, tok["phone"])); c != http.StatusPreconditionRequired {
		t.Fatalf("an always-deny rule = %d, want 428", c)
	}
	if c := run(bioReq("POST", "/api/settings", "{}", tok["phone"])); c != http.StatusPreconditionRequired {
		t.Fatalf("unsigned admin write = %d, want 428", c)
	}
	if c := run(bioReq("GET", "/api/needs-you", "", tok["phone"])); c != http.StatusOK {
		t.Fatalf("read = %d, want 200", c)
	}
	if c := run(bioReq("POST", "/api/session/prompt/claude:x", `{"text":"hi"}`, tok["phone"])); c != http.StatusOK {
		t.Fatalf("a prompt = %d, want 200 with no Face ID", c)
	}
	if c := run(bioReq("POST", path, body, tok["driver"])); c != http.StatusOK {
		t.Fatalf("a device with no key = %d, want 200", c)
	}
}

func TestBioKeyReplacementNeedsTheOldKey(t *testing.T) {
	_, tok := accessHarness(t)
	mux := http.NewServeMux()
	bioRoutes(mux, deviceStore)
	h := authMiddleware(mux)
	k1, pub1 := newBioKey(t)
	_, pub2 := newBioKey(t)
	run := func(r *http.Request) int {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	set := func(pub string) *http.Request {
		return bioReq("POST", "/api/devices/me/bio-key", `{"publicKey":"`+pub+`"}`, tok["phone"])
	}
	if c := run(set(pub1)); c != http.StatusOK {
		t.Fatalf("first key = %d", c)
	}
	if c := run(set(pub2)); c != http.StatusPreconditionRequired {
		t.Fatalf("unsigned replacement = %d, want 428", c)
	}
	r := set(pub2)
	signBio(t, r, k1, `{"publicKey":"`+pub2+`"}`, time.Now())
	if c := run(r); c != http.StatusOK {
		t.Fatalf("signed replacement = %d", c)
	}
	if d := deviceStore.Lookup(tok["phone"]); d.bioKey != pub2 || !d.Bio {
		t.Fatal("the new key was not stored")
	}
	if c := run(set("bm90IGEga2V5")); c != http.StatusPreconditionRequired {
		t.Fatalf("garbage key without a signature = %d, want 428", c)
	}
}

func TestBioRouteCoversTerminalAnswers(t *testing.T) {
	for _, c := range []struct {
		method, path string
		need         Level
		want         bool
	}{
		{"POST", "/api/approvals/ap_1/decide", LevelControl, true},
		{"POST", "/api/permission/p1/respond", LevelControl, true},
		{"POST", "/api/hosts/h1/answer", LevelControl, true},
		{"POST", "/api/session/claude:x/answer", LevelControl, true},
		{"POST", "/api/orch/terminal", LevelControl, true},
		{"POST", "/api/session/keys/claude:none", LevelControl, false},
		{"POST", "/api/orch/threads/ot_1/messages", LevelControl, false},
		{"POST", "/api/settings", LevelAdmin, true},
		{"GET", "/api/search", LevelAdmin, false},
	} {
		r := httptest.NewRequest(c.method, c.path, nil)
		if got := bioRoute(r, c.need); got != c.want {
			t.Errorf("%s %s = %v, want %v", c.method, c.path, got, c.want)
		}
	}
}
