package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A device with a Face ID key must sign approvals and admin writes with its Secure Enclave key.
// Header X-AM-Bio: "<unix ms>.<base64 DER signature>" over bioMessage.

const (
	bioHeader = "X-AM-Bio"
	bioMaxAge = 60 * time.Second
)

func bioMessage(method, path string, ts int64, body []byte) []byte {
	h := sha256.Sum256(body)
	return []byte("agent-monitor-bio\n" + method + "\n" + path + "\n" + strconv.FormatInt(ts, 10) + "\n" + hex.EncodeToString(h[:]))
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

// verifyBio checks the request's signature against key; it reads the body and puts it back.
func verifyBio(r *http.Request, key string, now time.Time) error {
	pub, err := parseBioKey(key)
	if err != nil {
		return err
	}
	ts, sig, ok := strings.Cut(r.Header.Get(bioHeader), ".")
	if !ok {
		return errors.New("no Face ID signature")
	}
	ms, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return errors.New("bad signature time")
	}
	if d := now.Sub(time.UnixMilli(ms)); d > bioMaxAge || d < -bioMaxAge {
		return errors.New("the signature is too old; check the phone's clock")
	}
	der, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		return errors.New("bad signature")
	}
	var body []byte
	if r.Body != nil {
		body, err = io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			return err
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	h := sha256.Sum256(bioMessage(r.Method, r.URL.Path, ms, body))
	if !ecdsa.VerifyASN1(pub, h[:], der) {
		return errors.New("the Face ID signature does not match")
	}
	if !bioFresh(sig, now) {
		return errors.New("this signature was already used")
	}
	return nil
}

// bioRoute is true when a Face ID device must sign this request.
func bioRoute(r *http.Request, need Level) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return false
	}
	p := r.URL.Path
	if strings.HasPrefix(p, "/api/devices/me/bio-key") {
		return false // the handler checks the old key itself
	}
	if need >= LevelAdmin {
		return true // rules, settings and devices could undo the check
	}
	switch {
	case strings.HasPrefix(p, "/api/approvals/") && strings.HasSuffix(p, "/decide"),
		strings.HasPrefix(p, "/api/permission/") && strings.HasSuffix(p, "/respond"),
		strings.HasPrefix(p, "/api/hosts/") && strings.HasSuffix(p, "/answer"),
		strings.HasPrefix(p, "/api/session/") && strings.HasSuffix(p, "/answer"),
		p == "/api/orch/terminal":
		return true
	case strings.HasPrefix(p, "/api/session/keys/"):
		return sessionWaitsForPermission(strings.TrimPrefix(p, "/api/session/keys/"))
	case strings.HasPrefix(p, "/api/session/send/"):
		return sessionWaitsForPermission(strings.TrimPrefix(p, "/api/session/send/"))
	case strings.HasPrefix(p, "/api/session/") && strings.Contains(p, "/cli-queue/"):
		id, _, _ := strings.Cut(strings.TrimPrefix(p, "/api/session/"), "/")
		return sessionWaitsForPermission(id)
	}
	return false
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

// bioGate answers 428 when a Face ID device sends an unsigned or bad request; false means it wrote the answer.
func bioGate(w http.ResponseWriter, r *http.Request, d *Device, need Level) bool {
	if d == nil || d.bioKey == "" || !bioRoute(r, need) || plainDeny(r) {
		return true
	}
	if err := verifyBio(r, d.bioKey, time.Now()); err != nil {
		writeJSONStatus(w, http.StatusPreconditionRequired, map[string]any{"error": "Face ID is needed for this on " + d.Name + ": " + err.Error(), "bio": true})
		return false
	}
	return true
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
				writeJSONStatus(w, http.StatusPreconditionRequired, map[string]any{"error": "Face ID with the current key is needed: " + err.Error() + ". If the key is lost, reset it on the Mac in Settings, Devices.", "bio": true})
				return
			}
		}
		key := ""
		if r.Method == http.MethodPost {
			var in struct {
				PublicKey string `json:"publicKey"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&in); err != nil {
				writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "bad json"})
				return
			}
			if _, err := parseBioKey(in.PublicKey); err != nil {
				writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "not a P-256 public key: " + err.Error()})
				return
			}
			key = in.PublicKey
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
