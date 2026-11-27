package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// signPasskey sets X-AM-Passkey the way the web app does after navigator.credentials.get.
func signPasskey(t *testing.T, r *http.Request, k *ecdsa.PrivateKey, id, rp, origin, body string, at time.Time, flags byte) {
	t.Helper()
	ms := at.UnixMilli()
	ch := sha256.Sum256(bioMessage(r.Method, r.URL.Path, ms, []byte(body)))
	client, _ := json.Marshal(map[string]string{"type": "webauthn.get", "challenge": base64.RawURLEncoding.EncodeToString(ch[:]), "origin": origin})
	rph := sha256.Sum256([]byte(rp))
	auth := append(rph[:], flags, 0, 0, 0, 1)
	ch2 := sha256.Sum256(client)
	h := sha256.Sum256(append(append([]byte{}, auth...), ch2[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, k, h[:])
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	hdr, _ := json.Marshal(map[string]any{"ts": ms, "id": id, "authData": enc(auth), "clientData": enc(client), "sig": enc(sig)})
	r.Header.Set(passkeyHeader, enc(hdr))
}

func newPasskey(t *testing.T, rp string) (*ecdsa.PrivateKey, string) {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(passkeyKey{ID: "cred1", SPKI: base64.StdEncoding.EncodeToString(der), Rp: rp})
	return k, passkeyPrefix + string(b)
}

func TestPasskeyGateChecksTheAssertion(t *testing.T) {
	h, tok := accessHarness(t)
	const rp = "mac.tail1.ts.net"
	k, key := newPasskey(t, rp)
	d := deviceStore.Lookup(tok["phone"])
	if _, err := deviceStore.SetBioKey(d.ID, key); err != nil {
		t.Fatal(err)
	}
	const path, body = "/api/approvals/ap_1/decide", `{"behavior":"allow"}`
	run := func(mut func(r *http.Request)) int {
		r := bioReq("POST", path, body, tok["phone"])
		mut(r)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	now := time.Now()
	if c := run(func(*http.Request) {}); c != http.StatusPreconditionRequired {
		t.Fatalf("unsigned = %d, want 428", c)
	}
	if c := run(func(r *http.Request) { signPasskey(t, r, k, "cred1", rp, "https://"+rp, body, now, 0x05) }); c != http.StatusOK {
		t.Fatalf("signed = %d, want 200", c)
	}
	for name, mut := range map[string]func(r *http.Request){
		"other site":    func(r *http.Request) { signPasskey(t, r, k, "cred1", rp, "https://evil.example", body, now, 0x05) },
		"no user check": func(r *http.Request) { signPasskey(t, r, k, "cred1", rp, "https://"+rp, body, now, 0x01) },
		"other body":    func(r *http.Request) { signPasskey(t, r, k, "cred1", rp, "https://"+rp, `{"behavior":"x"}`, now, 0x05) },
		"other key id":  func(r *http.Request) { signPasskey(t, r, k, "cred2", rp, "https://"+rp, body, now, 0x05) },
		"old signature": func(r *http.Request) {
			signPasskey(t, r, k, "cred1", rp, "https://"+rp, body, now.Add(-2*time.Minute), 0x05)
		},
		"plain http rp": func(r *http.Request) { signPasskey(t, r, k, "cred1", rp, "http://"+rp, body, now, 0x05) },
	} {
		if c := run(mut); c != http.StatusPreconditionRequired {
			t.Errorf("%s = %d, want 428", name, c)
		}
	}
	if !deviceStore.Lookup(tok["phone"]).Passkey {
		t.Error("the device does not show a passkey")
	}
}

func TestBioMinRiskAndTrustWindow(t *testing.T) {
	prev := approvalStore
	t.Cleanup(func() { approvalStore = prev })
	trust := 10
	approvalStore = &ApprovalStore{pending: map[string]*pendingApproval{
		"low":  {a: &Approval{ID: "low", Risk: "low", SessionID: "s1"}},
		"med":  {a: &Approval{ID: "med", Risk: "medium", SessionID: "s1"}},
		"high": {a: &Approval{ID: "high", Risk: "high", SessionID: "s1"}},
	}, settings: ApprovalSettings{BioTrustMin: &trust}}
	need := func(id string) bool {
		return bioRoute(httptest.NewRequest("POST", "/api/approvals/"+id+"/decide", nil), LevelControl)
	}
	if need("low") || !need("med") || !need("high") || !need("gone") {
		t.Fatalf("default medium: low=%v med=%v high=%v gone=%v", need("low"), need("med"), need("high"), need("gone"))
	}
	approvalStore.settings.BioMinRisk = "high"
	if need("med") || !need("high") {
		t.Fatal("high: a medium approval still needs a signature")
	}
	now := time.Now()
	if trusted("d1", "s1", now) {
		t.Fatal("trusted before any signature")
	}
	grantTrust("d1", "s1", now)
	if !trusted("d1", "s1", now.Add(9*time.Minute)) || trusted("d1", "s1", now.Add(11*time.Minute)) || trusted("d2", "s1", now) || trusted("d1", "s2", now) {
		t.Fatal("the trust window covers the wrong device, session or time")
	}
}

func TestStoredProofChecksAgain(t *testing.T) {
	k, pub := newBioKey(t)
	body := `{"behavior":"allow"}`
	r2 := httptest.NewRequest("POST", "/api/approvals/ap_9/decide", strings.NewReader(body))
	signBio(t, r2, k, body, time.Now())
	p, err := verifyProof(r2, pub, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if out := proofCheck(&Approval{Proof: p}); out["ok"] != true {
		t.Fatalf("stored proof = %v", out)
	}
	p.BodyHash = p.BodyHash[:62] + "00"
	if out := proofCheck(&Approval{Proof: p}); out["ok"] != false {
		t.Fatal("a changed body hash still checks")
	}
}

func TestBioPolicyGatesTerminalPromptAnswers(t *testing.T) {
	post := func(path, body string) *http.Request {
		return httptest.NewRequest("POST", path, strings.NewReader(body))
	}
	if needs, risk, _ := bioPolicy(post("/api/session/prompt/s1", `{"answer":"yes"}`), LevelControl); !needs || risk != "high" {
		t.Fatalf("a yes to a terminal prompt must need a signature: %v %q", needs, risk)
	}
	if !plainDeny(post("/api/session/prompt/s1", `{"answer":"no"}`)) {
		t.Fatal("no runs nothing, so it must pass without a signature")
	}
	if plainDeny(post("/api/session/prompt/s1", `{"answer":"dismiss"}`)) {
		t.Fatal("dismiss lifts the gate on keys and send, so it must be signed")
	}
	if plainDeny(post("/api/session/prompt/s1", `{"answer":"yes"}`)) {
		t.Fatal("yes is not a deny")
	}
	if needs, _, _ := bioPolicy(post("/api/orch/threads/t1/allow-once", ""), LevelControl); !needs {
		t.Fatal("allow-once lifts a spend limit, so it must need a signature")
	}
}
