package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A device with a Face ID key or a passkey must sign approvals and admin writes.
// Phone: X-AM-Bio "<unix ms>.<base64 DER>" over bioMessage. Browser: X-AM-Passkey, a WebAuthn assertion whose challenge is sha256(bioMessage).

const (
	bioHeader     = "X-AM-Bio"
	passkeyHeader = "X-AM-Passkey"
	passkeyPrefix = "pk:" // bio_key holds "pk:" + JSON for a browser passkey
	bioMaxAge     = 60 * time.Second
)

// bioProof is the evidence kept with a decision, enough to check the signature again later.
type bioProof struct {
	Kind       string `json:"kind"` // faceid, passkey or trusted (inside the trust window, no new signature)
	DeviceID   string `json:"deviceId"`
	Device     string `json:"device"`
	At         int64  `json:"at"`
	Method     string `json:"method,omitempty"`
	Path       string `json:"path,omitempty"`
	BodyHash   string `json:"bodyHash,omitempty"`
	Key        string `json:"key,omitempty"`
	Sig        string `json:"sig,omitempty"`
	AuthData   string `json:"authData,omitempty"`
	ClientData string `json:"clientData,omitempty"`
	Rp         string `json:"rp,omitempty"`
}

type passkeyKey struct {
	ID   string `json:"id"`   // base64url credential id
	SPKI string `json:"spki"` // base64 DER SubjectPublicKeyInfo from PublicKeyCredential.getPublicKey()
	Rp   string `json:"rp"`
}

func parsePasskey(stored string) (*passkeyKey, *ecdsa.PublicKey, error) {
	var pk passkeyKey
	if err := json.Unmarshal([]byte(strings.TrimPrefix(stored, passkeyPrefix)), &pk); err != nil {
		return nil, nil, errors.New("bad passkey record")
	}
	der, err := base64.StdEncoding.DecodeString(pk.SPKI)
	if err != nil {
		return nil, nil, errors.New("the passkey is not base64")
	}
	k, err := x509.ParsePKIXPublicKey(der)
	ec, ok := k.(*ecdsa.PublicKey)
	if err != nil || !ok || ec.Curve != elliptic.P256() {
		return nil, nil, errors.New("only P-256 (ES256) passkeys work")
	}
	if pk.ID == "" || pk.Rp == "" {
		return nil, nil, errors.New("the passkey needs an id and a host")
	}
	return &pk, ec, nil
}

func bioMessage(method, path string, ts int64, body []byte) []byte {
	h := sha256.Sum256(body)
	return bioMessageHash(method, path, ts, h[:])
}

func bioMessageHash(method, path string, ts int64, bodyHash []byte) []byte {
	return []byte("agent-monitor-bio\n" + method + "\n" + path + "\n" + strconv.FormatInt(ts, 10) + "\n" + hex.EncodeToString(bodyHash))
}

func parseBioKey(b64 string) (*ecdsa.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, errors.New("the key is not base64")
	}
	return ecdsa.ParseUncompressedPublicKey(elliptic.P256(), raw)
}

// bioSeen stops a signature from being used twice inside its time window.
var bioSeen = struct {
	sync.Mutex
	m map[string]time.Time
}{m: map[string]time.Time{}}

func bioFresh(sig string, now time.Time) bool {
	bioSeen.Lock()
	defer bioSeen.Unlock()
	for k, t := range bioSeen.m {
		if now.Sub(t) > 2*bioMaxAge {
			delete(bioSeen.m, k)
		}
	}
	if _, used := bioSeen.m[sig]; used {
		return false
	}
	bioSeen.m[sig] = now
	return true
}

func readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}

func freshTime(ms int64, now time.Time) error {
	if d := now.Sub(time.UnixMilli(ms)); d > bioMaxAge || d < -bioMaxAge {
		return errors.New("the signature is too old; check the device clock")
	}
	return nil
}

// verifyBio checks the request's signature against the stored key (Face ID or passkey); it reads the body and puts it back.
func verifyBio(r *http.Request, key string, now time.Time) error {
	_, err := verifyProof(r, key, now)
	return err
}

func verifyProof(r *http.Request, key string, now time.Time) (*bioProof, error) {
	body, err := readBody(r)
	if err != nil {
		return nil, err
	}
	bh := sha256.Sum256(body)
	var p *bioProof
	if strings.HasPrefix(key, passkeyPrefix) {
		p, err = passkeyProof(r.Header.Get(passkeyHeader), key)
	} else {
		p, err = faceIDProof(r.Header.Get(bioHeader), key)
	}
	if err != nil {
		return nil, err
	}
	p.Method, p.Path, p.BodyHash = r.Method, r.URL.Path, hex.EncodeToString(bh[:])
	if err := freshTime(p.At, now); err != nil {
		return nil, err
	}
	if err := checkProof(p); err != nil {
		return nil, err
	}
	// The signed message, not the signature bytes, is the replay key: an ECDSA signature has a second valid form.
	if !bioFresh(key+"\n"+hex.EncodeToString(bioMessageHash(p.Method, p.Path, p.At, bh[:])), now) {
		return nil, errors.New("this signature was already used")
	}
	return p, nil
}

func faceIDProof(h, key string) (*bioProof, error) {
	ts, sig, ok := strings.Cut(h, ".")
	if !ok {
		return nil, errors.New("no Face ID signature")
	}
	ms, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return nil, errors.New("bad signature time")
	}
	return &bioProof{Kind: "faceid", At: ms, Key: key, Sig: sig}, nil
}

func passkeyProof(h, key string) (*bioProof, error) {
	if h == "" {
		return nil, errors.New("no passkey signature")
	}
	raw, err := base64.RawURLEncoding.DecodeString(h)
	if err != nil {
		return nil, errors.New("bad passkey header")
	}
	var in struct {
		TS         int64  `json:"ts"`
		ID         string `json:"id"`
		AuthData   string `json:"authData"`
		ClientData string `json:"clientData"`
		Sig        string `json:"sig"`
	}
	if json.Unmarshal(raw, &in) != nil {
		return nil, errors.New("bad passkey header")
	}
	pk, _, err := parsePasskey(key)
	if err != nil {
		return nil, err
	}
	if in.ID != pk.ID {
		return nil, errors.New("a different passkey signed this")
	}
	return &bioProof{Kind: "passkey", At: in.TS, Key: key, Sig: in.Sig, AuthData: in.AuthData, ClientData: in.ClientData, Rp: pk.Rp}, nil
}

// checkProof checks a stored or fresh proof against its own key and message; it does not check the time.
func checkProof(p *bioProof) error {
	bh, err := hex.DecodeString(p.BodyHash)
	if err != nil || len(bh) != sha256.Size {
		return errors.New("bad body hash")
	}
	msg := bioMessageHash(p.Method, p.Path, p.At, bh)
	switch p.Kind {
	case "faceid":
		pub, err := parseBioKey(p.Key)
		if err != nil {
			return err
		}
		der, err := base64.StdEncoding.DecodeString(p.Sig)
		if err != nil {
			return errors.New("bad signature")
		}
		h := sha256.Sum256(msg)
		if !ecdsa.VerifyASN1(pub, h[:], der) {
			return errors.New("the Face ID signature does not match")
		}
		return nil
	case "passkey":
		_, pub, err := parsePasskey(p.Key)
		if err != nil {
			return err
		}
		return checkAssertion(pub, p.Rp, msg, p.AuthData, p.ClientData, p.Sig)
	}
	return errors.New("no signature to check")
}

// checkAssertion checks a WebAuthn assertion: challenge, origin, rp id hash, user verification and signature.
func checkAssertion(pub *ecdsa.PublicKey, rp string, msg []byte, authB64, clientB64, sigB64 string) error {
	auth, err1 := base64.RawURLEncoding.DecodeString(authB64)
	client, err2 := base64.RawURLEncoding.DecodeString(clientB64)
	sig, err3 := base64.RawURLEncoding.DecodeString(sigB64)
	if err1 != nil || err2 != nil || err3 != nil {
		return errors.New("bad passkey data")
	}
	var cd struct {
		Type      string `json:"type"`
		Challenge string `json:"challenge"`
		Origin    string `json:"origin"`
	}
	if json.Unmarshal(client, &cd) != nil || cd.Type != "webauthn.get" {
		return errors.New("bad passkey client data")
	}
	want := sha256.Sum256(msg)
	if cd.Challenge != base64.RawURLEncoding.EncodeToString(want[:]) {
		return errors.New("the passkey signed a different request")
	}
	u, err := url.Parse(cd.Origin)
	if err != nil || u.Hostname() != rp || !(u.Scheme == "https" || (u.Scheme == "http" && rp == "localhost")) {
		return errors.New("the passkey came from a different site")
	}
	rph := sha256.Sum256([]byte(rp))
	if len(auth) < 37 || !bytes.Equal(auth[:32], rph[:]) {
		return errors.New("the passkey is for a different site")
	}
	if auth[32]&0x01 == 0 || auth[32]&0x04 == 0 {
		return errors.New("the passkey did not check your face, finger or password")
	}
	ch := sha256.Sum256(client)
	h := sha256.Sum256(append(append([]byte{}, auth...), ch[:]...))
	if !ecdsa.VerifyASN1(pub, h[:], sig) {
		return errors.New("the passkey signature does not match")
	}
	return nil
}

// bioRoute is true when a Face ID device must sign this request.
func bioRoute(r *http.Request, need Level) bool {
	ok, _, _ := bioPolicy(r, need)
	return ok
}

// bioPolicy says whether the request needs a signature, and gives the risk and session of an approval it decides.
func bioPolicy(r *http.Request, need Level) (bool, string, string) {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return false, "", ""
	}
	p := r.URL.Path
	if strings.HasPrefix(p, "/api/devices/me/bio-key") || strings.HasPrefix(p, "/api/devices/me/passkey") {
		return false, "", "" // the handler checks the old key itself
	}
	if need >= LevelAdmin {
		return true, "high", "" // rules, settings and devices could undo the check
	}
	switch {
	case strings.HasPrefix(p, "/api/approvals/") && strings.HasSuffix(p, "/decide"):
		risk, sess := approvalRiskOf(strings.TrimSuffix(strings.TrimPrefix(p, "/api/approvals/"), "/decide"))
		return riskNeedsBio(risk), risk, sess
	case strings.HasPrefix(p, "/api/permission/") && strings.HasSuffix(p, "/respond"):
		risk, sess := permRiskOf(strings.TrimSuffix(strings.TrimPrefix(p, "/api/permission/"), "/respond"))
		return riskNeedsBio(risk), risk, sess
	case strings.HasPrefix(p, "/api/hosts/") && strings.HasSuffix(p, "/answer"),
		strings.HasPrefix(p, "/api/session/") && strings.HasSuffix(p, "/answer"),
		strings.HasPrefix(p, "/api/session/prompt/"),
		p == "/api/orch/terminal",
		strings.HasPrefix(p, "/api/orch/threads/") && strings.HasSuffix(p, "/allow-once"),
		strings.HasPrefix(p, "/api/secrets/") && strings.HasSuffix(p, "/answer"):
		return true, "high", ""
	case strings.HasPrefix(p, "/api/session/keys/"):
		return sessionWaitsForPermission(strings.TrimPrefix(p, "/api/session/keys/")), "high", ""
	case strings.HasPrefix(p, "/api/session/send/"):
		return sessionWaitsForPermission(strings.TrimPrefix(p, "/api/session/send/")), "high", ""
	case strings.HasPrefix(p, "/api/session/") && strings.Contains(p, "/cli-queue/"):
		id, _, _ := strings.Cut(strings.TrimPrefix(p, "/api/session/"), "/")
		return sessionWaitsForPermission(id), "high", ""
	}
	return false, "", ""
}

// approvalRiskOf: an unknown approval counts as high, so it always needs a signature.
func approvalRiskOf(id string) (string, string) {
	if approvalStore == nil {
		return "high", ""
	}
	approvalStore.mu.Lock()
	defer approvalStore.mu.Unlock()
	if p, ok := approvalStore.pending[id]; ok {
		return firstOf(p.a.Risk, "medium"), p.a.SessionID
	}
	return "high", ""
}

func permRiskOf(id string) (string, string) {
	if permStore != nil {
		if q := permStore.Get(id); q != nil {
			return firstOf(q.Risk, "medium"), q.SessionID
		}
	}
	return "high", ""
}

func riskRank(r string) int {
	switch r {
	case "low":
		return 0
	case "high":
		return 2
	}
	return 1
}

// bioSettings: the lowest risk that needs a signature (default medium), and the trust window in minutes (default 10).
func bioSettings() (string, int) {
	if approvalStore == nil {
		return "medium", 10
	}
	approvalStore.mu.Lock()
	defer approvalStore.mu.Unlock()
	st := approvalStore.settings
	trust := 10
	if st.BioTrustMin != nil {
		trust = max(0, *st.BioTrustMin)
	}
	return firstOf(st.BioMinRisk, "medium"), trust
}

func riskNeedsBio(risk string) bool {
	min, _ := bioSettings()
	return riskRank(risk) >= riskRank(min)
}

// bioTrust: after one signature, the same device may decide the same session's low and medium approvals for a while.
var bioTrust = struct {
	sync.Mutex
	m map[string]time.Time
}{m: map[string]time.Time{}}

func trusted(dev, sess string, now time.Time) bool {
	if sess == "" {
		return false
	}
	bioTrust.Lock()
	defer bioTrust.Unlock()
	return now.Before(bioTrust.m[dev+"|"+sess])
}

func grantTrust(dev, sess string, now time.Time) {
	_, mins := bioSettings()
	if sess == "" || mins == 0 {
		return
	}
	bioTrust.Lock()
	defer bioTrust.Unlock()
	for k, t := range bioTrust.m {
		if now.After(t) {
			delete(bioTrust.m, k)
		}
	}
	bioTrust.m[dev+"|"+sess] = now.Add(time.Duration(mins) * time.Minute)
}

type proofKey struct{}

// requestProof is the signature that let this request through, if it needed one.
func requestProof(r *http.Request) *bioProof {
	p, _ := r.Context().Value(proofKey{}).(*bioProof)
	return p
}

// sessionWaitsForPermission: typed keys would answer the terminal's permission menu.
func sessionWaitsForPermission(id string) bool {
	if store != nil {
		if s := store.Get(id); s != nil && s.State == StateAwaitingPermission {
			return true
		}
	}
	if hostRegistry != nil {
		for _, h := range hostRegistry.List() {
			if (h.AgentID == id || h.HostID == id) && h.Prompt != nil {
				return true
			}
		}
	}
	return false
}

// plainDeny: a deny cannot run anything, so a lock-screen Deny works without Face ID.
func plainDeny(r *http.Request) bool {
	p := r.URL.Path
	if strings.HasPrefix(p, "/api/session/prompt/") {
		body, err := readBody(r)
		var in struct {
			Answer string `json:"answer"`
		}
		return err == nil && json.Unmarshal(body, &in) == nil && (in.Answer == "no" || in.Answer == "dismiss")
	}
	if strings.HasPrefix(p, "/api/secrets/") && strings.HasSuffix(p, "/answer") {
		body, err := readBody(r)
		var in struct {
			Type string `json:"type"`
		}
		return err == nil && json.Unmarshal(body, &in) == nil && in.Type == "decline"
	}
	if !(strings.HasPrefix(p, "/api/approvals/") && strings.HasSuffix(p, "/decide")) && !(strings.HasPrefix(p, "/api/permission/") && strings.HasSuffix(p, "/respond")) {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var in struct {
		Behavior     string          `json:"behavior"`
		Always       bool            `json:"always"`
		UpdatedInput json.RawMessage `json:"updatedInput"`
		Answers      json.RawMessage `json:"answers"`
		OptionID     string          `json:"optionId"`
	}
	return json.Unmarshal(body, &in) == nil && in.Behavior == "deny" && !in.Always && len(in.UpdatedInput) == 0 && len(in.Answers) == 0 && in.OptionID == ""
}

// bioGate answers 428 when a Face ID or passkey device sends an unsigned or bad request; false means it wrote the answer.
func bioGate(w http.ResponseWriter, r *http.Request, d *Device, need Level) (*http.Request, bool) {
	if d == nil || d.bioKey == "" {
		return r, true
	}
	needs, risk, sess := bioPolicy(r, need)
	if !needs || plainDeny(r) {
		return r, true
	}
	now := time.Now()
	if risk != "high" && trusted(d.ID, sess, now) {
		return r.WithContext(context.WithValue(r.Context(), proofKey{}, &bioProof{Kind: "trusted", DeviceID: d.ID, Device: d.Name, At: now.UnixMilli()})), true
	}
	p, err := verifyProof(r, d.bioKey, now)
	if err != nil {
		what := "Face ID"
		if strings.HasPrefix(d.bioKey, passkeyPrefix) {
			what = "A passkey"
		}
		writeJSONStatus(w, http.StatusPreconditionRequired, map[string]any{"error": what + " is needed for this on " + d.Name + ": " + err.Error(), "bio": true})
		return r, false
	}
	p.DeviceID, p.Device = d.ID, d.Name
	if risk != "high" {
		grantTrust(d.ID, sess, now)
	}
	return r.WithContext(context.WithValue(r.Context(), proofKey{}, p)), true
}

func bioRoutes(mux *http.ServeMux, s *DeviceStore) {
	// A device sets its own key; replacing or removing a key needs a signature from the old one.
	own := func(w http.ResponseWriter, r *http.Request) {
		d := requestDevice(r)
		if d == nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "only a paired device has a Face ID key"})
			return
		}
		if d.bioKey != "" {
			if err := verifyBio(r, d.bioKey, time.Now()); err != nil {
				writeJSONStatus(w, http.StatusPreconditionRequired, map[string]any{"error": "the current key must sign this: " + err.Error() + ". If the key is lost, reset it on the Mac in Settings, Devices.", "bio": true})
				return
			}
		}
		key := ""
		if r.Method == http.MethodPost {
			var in struct {
				PublicKey string     `json:"publicKey"`
				Passkey   passkeyKey `json:"passkey"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&in); err != nil {
				writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "bad json"})
				return
			}
			if strings.HasSuffix(r.URL.Path, "/passkey") {
				b, _ := json.Marshal(in.Passkey)
				key = passkeyPrefix + string(b)
				if _, _, err := parsePasskey(key); err != nil {
					writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
					return
				}
			} else {
				if _, err := parseBioKey(in.PublicKey); err != nil {
					writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "not a P-256 public key: " + err.Error()})
					return
				}
				key = in.PublicKey
			}
		}
		if _, err := s.SetBioKey(d.ID, key); err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		log.Printf("devices: %s Face ID key %s", d.Name, map[bool]string{true: "set", false: "removed"}[key != ""])
		writeJSON(w, map[string]any{"ok": true, "bio": key != ""})
	}
	mux.HandleFunc("POST /api/devices/me/bio-key", own)
	mux.HandleFunc("DELETE /api/devices/me/bio-key", own)
	mux.HandleFunc("POST /api/devices/me/passkey", own)
	mux.HandleFunc("DELETE /api/devices/me/passkey", own)
	// The Mac resets a device whose phone lost its key (a new face, a new install).
	mux.HandleFunc("DELETE /api/devices/{id}/bio-key", func(w http.ResponseWriter, r *http.Request) {
		if !localCaller(r) {
			writeJSONStatus(w, http.StatusForbidden, map[string]any{"error": "reset a Face ID key on the Mac itself"})
			return
		}
		ok, err := s.SetBioKey(r.PathValue("id"), "")
		if err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		if !ok {
			writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "no such device"})
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})
}
