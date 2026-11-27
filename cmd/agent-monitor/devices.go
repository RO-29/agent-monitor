package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Per-device tokens: "amd_" + 32 random bytes (base64url). Only the sha256 is stored.

const (
	deviceTokenPrefix = "amd_"
	deviceCookieName  = "am_device"
	deviceTouchEvery  = time.Minute
)

type Device struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	CreatedAt  int64  `json:"createdAt"`
	LastUsedAt int64  `json:"lastUsedAt,omitempty"`
	LastIP     string `json:"lastIp,omitempty"`
	RevokedAt  int64  `json:"revokedAt,omitempty"`
	Current    bool   `json:"current"`
	Kind       string `json:"kind"`              // device (a person's app) or machine (another daemon)
	Level      string `json:"level"`             // view, control or admin; a machine is never admin
	BoundIP    string `json:"boundIp,omitempty"` // a machine grant works only from the address that paired
	Bio        bool   `json:"bio"`               // approvals and admin writes need a Face ID or passkey signature
	Passkey    bool   `json:"passkey"`           // the key is a browser passkey, not the phone's Face ID key
	bioKey     string // base64 of the uncompressed P-256 public key from the phone's Secure Enclave
	hash       []byte
}

type DeviceStore struct {
	mu    sync.Mutex
	db    *sql.DB
	devs  []*Device
	now   func() time.Time
	codes map[string]pairCode
}

// pairCode is a single-use code in the pair link; it maps to the token for pairCodeTTL.
type pairCode struct {
	token string
	exp   time.Time
}

const pairCodeTTL = 5 * time.Minute

// NewPairCode returns a random single-use code for tok.
func (s *DeviceStore) NewPairCode(tok string) string {
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	code := hex.EncodeToString(raw)
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.codes == nil {
		s.codes = map[string]pairCode{}
	}
	for k, c := range s.codes {
		if now.After(c.exp) {
			delete(s.codes, k)
		}
	}
	s.codes[code] = pairCode{token: tok, exp: now.Add(pairCodeTTL)}
	return code
}

// TakePairCode returns the token for a live code and removes the code.
func (s *DeviceStore) TakePairCode(code string) string {
	s.mu.Lock()
	c, ok := s.codes[code]
	delete(s.codes, code)
	s.mu.Unlock()
	if !ok || s.now().After(c.exp) {
		return ""
	}
	return c.token
}

var deviceStore *DeviceStore

func openDeviceStore(dir string) (*DeviceStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "devices.db")+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS devices (id TEXT PRIMARY KEY, name TEXT NOT NULL, token_hash TEXT NOT NULL UNIQUE,
		created_at INTEGER NOT NULL, last_used_at INTEGER NOT NULL DEFAULT 0, last_ip TEXT NOT NULL DEFAULT '', revoked_at INTEGER NOT NULL DEFAULT 0)`); err != nil {
		db.Close()
		return nil, err
	}
	// Devices paired before levels existed are the user's own: they stay admin.
	for _, col := range []string{`kind TEXT NOT NULL DEFAULT 'device'`, `level TEXT NOT NULL DEFAULT 'admin'`, `bound_ip TEXT NOT NULL DEFAULT ''`, `bio_key TEXT NOT NULL DEFAULT ''`} {
		if _, err := db.Exec(`ALTER TABLE devices ADD COLUMN ` + col); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			db.Close()
			return nil, err
		}
	}
	s := &DeviceStore{db: db, now: time.Now}
	rows, err := db.Query(`SELECT id, name, token_hash, created_at, last_used_at, last_ip, revoked_at, kind, level, bound_ip, bio_key FROM devices ORDER BY created_at`)
	if err != nil {
		db.Close()
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		d := &Device{}
		var h string
		if err := rows.Scan(&d.ID, &d.Name, &h, &d.CreatedAt, &d.LastUsedAt, &d.LastIP, &d.RevokedAt, &d.Kind, &d.Level, &d.BoundIP, &d.bioKey); err != nil {
			return nil, err
		}
		d.hash, _ = hex.DecodeString(h)
		d.Bio, d.Passkey = d.bioKey != "", strings.HasPrefix(d.bioKey, passkeyPrefix)
		s.devs = append(s.devs, d)
	}
	return s, rows.Err()
}

func (s *DeviceStore) Close() error { return s.db.Close() }

func hashToken(tok string) []byte {
	h := sha256.Sum256([]byte(tok))
	return h[:]
}

// Create adds a device of kind device at level admin (the user's own app) and returns its token.
func (s *DeviceStore) Create(name string) (*Device, string, error) {
	return s.CreateGrant(name, "device", "")
}

// CreateGrant adds a device or machine grant; the token is never stored or logged. A machine starts at
// view and can never be admin; a device defaults to admin.
func (s *DeviceStore) CreateGrant(name, kind, level string) (*Device, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 80 {
		return nil, "", errors.New("name is required (at most 80 characters)")
	}
	if kind != "machine" {
		kind = "device"
	}
	if level == "" {
		level = map[string]string{"device": "admin", "machine": "view"}[kind]
	}
	if parseLevel(level) == LevelNone || (kind == "machine" && level == "admin") {
		return nil, "", errors.New("level must be view or control for a machine, and view, control or admin for a device")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, "", err
	}
	tok := deviceTokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	id := make([]byte, 6)
	_, _ = rand.Read(id)
	d := &Device{ID: "dv_" + hex.EncodeToString(id), Name: name, CreatedAt: s.now().UnixMilli(), hash: hashToken(tok), Kind: kind, Level: level}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(`INSERT INTO devices (id, name, token_hash, created_at, kind, level) VALUES (?,?,?,?,?,?)`, d.ID, d.Name, hex.EncodeToString(d.hash), d.CreatedAt, d.Kind, d.Level); err != nil {
		return nil, "", err
	}
	s.devs = append(s.devs, d)
	c := *d
	return &c, tok, nil
}

// Lookup returns the active device for a token; every active hash is compared in constant time.
func (s *DeviceStore) Lookup(tok string) *Device {
	if s == nil || !strings.HasPrefix(tok, deviceTokenPrefix) {
		return nil
	}
	h := hashToken(tok)
	s.mu.Lock()
	defer s.mu.Unlock()
	var found *Device
	for _, d := range s.devs {
		if subtle.ConstantTimeCompare(h, d.hash) == 1 && d.RevokedAt == 0 {
			found = d
		}
	}
	if found == nil {
		return nil
	}
	c := *found
	return &c
}

// Touch records use at most once a minute per device.
func (s *DeviceStore) Touch(id, ip string) {
	now := s.now().UnixMilli()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.devs {
		if d.ID != id {
			continue
		}
		if now-d.LastUsedAt < deviceTouchEvery.Milliseconds() {
			return
		}
		d.LastUsedAt, d.LastIP = now, ip
		if _, err := s.db.Exec(`UPDATE devices SET last_used_at=?, last_ip=? WHERE id=?`, now, ip, id); err != nil {
			log.Printf("devices: touch %s: %v", id, err)
		}
		return
	}
}

func (s *DeviceStore) List(currentID string) []Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Device, 0, len(s.devs))
	for _, d := range s.devs {
		c := *d
		c.Current = c.ID == currentID
		out = append(out, c)
	}
	return out
}

// SetLevel changes a grant's level; a machine cannot become admin. It returns false for an unknown id.
func (s *DeviceStore) SetLevel(id, level string) (*Device, bool, error) {
	if parseLevel(level) == LevelNone {
		return nil, true, errors.New("level must be view, control or admin")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.devs {
		if d.ID != id {
			continue
		}
		if d.Kind == "machine" && level == "admin" {
			return nil, true, errors.New("a machine grant cannot be admin")
		}
		if _, err := s.db.Exec(`UPDATE devices SET level=? WHERE id=?`, level, id); err != nil {
			return nil, true, err
		}
		d.Level = level
		c := *d
		return &c, true, nil
	}
	return nil, false, nil
}

// BindIP records the address a machine grant paired from, once.
func (s *DeviceStore) BindIP(id, ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.devs {
		if d.ID == id && d.Kind == "machine" && d.BoundIP == "" && ip != "" {
			if _, err := s.db.Exec(`UPDATE devices SET bound_ip=? WHERE id=?`, ip, id); err == nil {
				d.BoundIP = ip
			}
		}
	}
}

// SetBioKey stores or clears (key "") a device's Face ID public key; it returns false for an unknown id.
func (s *DeviceStore) SetBioKey(id, key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.devs {
		if d.ID == id {
			if _, err := s.db.Exec(`UPDATE devices SET bio_key=? WHERE id=?`, key, id); err != nil {
				return true, err
			}
			d.bioKey, d.Bio, d.Passkey = key, key != "", strings.HasPrefix(key, passkeyPrefix)
			return true, nil
		}
	}
	return false, nil
}

// Revoke marks a device revoked; it returns false for an unknown id.
func (s *DeviceStore) Revoke(id string) (*Device, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.devs {
		if d.ID != id {
			continue
		}
		if d.RevokedAt == 0 {
			now := s.now().UnixMilli()
			if _, err := s.db.Exec(`UPDATE devices SET revoked_at=? WHERE id=?`, now, id); err != nil {
				return nil, true, err
			}
			d.RevokedAt = now
		}
		c := *d
		return &c, true, nil
	}
	return nil, false, nil
}

// ActiveExcept counts devices that are not revoked, leaving out exceptID.
func (s *DeviceStore) ActiveExcept(exceptID string) int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, d := range s.devs {
		if d.RevokedAt == 0 && d.ID != exceptID {
			n++
		}
	}
	return n
}

// presentedDeviceToken reads a device token from the Bearer header or the am_device cookie (never the URL).
func presentedDeviceToken(r *http.Request) []string {
	var out []string
	if h := r.Header.Get("Authorization"); len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		if t := strings.TrimSpace(h[7:]); strings.HasPrefix(t, deviceTokenPrefix) {
			out = append(out, t)
		}
	}
	if c, err := r.Cookie(deviceCookieName); err == nil && strings.HasPrefix(c.Value, deviceTokenPrefix) {
		out = append(out, c.Value)
	}
	return out
}

// requestDevice returns the active device a request authenticates as, if any.
func requestDevice(r *http.Request) *Device {
	if deviceStore == nil {
		return nil
	}
	for _, tok := range presentedDeviceToken(r) {
		if d := deviceStore.Lookup(tok); d != nil {
			return d
		}
	}
	return nil
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func requireDeviceToken() bool { return getSettings().RequireDeviceToken }

// authPublicPath is reachable without credentials: the pairing page, its static assets and the pairing call.
func authPublicPath(r *http.Request) bool {
	p := r.URL.Path
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		return p == "/pair" || p == "/pair/" || (strings.HasPrefix(p, "/assets/") && !strings.Contains(p, "..")) ||
			otaRequest(p) != "" // iOS fetches the install files with no credentials; the token is the check
	case http.MethodPost:
		return p == "/api/devices/session"
	}
	return false
}

var errLockout = errors.New("lockout")

// checkRequireDeviceToken refuses to turn the requirement on when it would lock the caller out.
func checkRequireDeviceToken(r *http.Request, enable bool) error {
	if !enable {
		return nil
	}
	if deviceStore == nil {
		return fmt.Errorf("%w: device tokens are unavailable", errLockout)
	}
	caller := requestDevice(r)
	if caller == nil && !localCaller(r) {
		return fmt.Errorf("%w: this browser signed in with the password, which would stop working. Pair this device first, or turn this on from the Mac itself", errLockout)
	}
	callerID := ""
	if caller != nil {
		callerID = caller.ID
	}
	if deviceStore.ActiveExcept(callerID) == 0 {
		return fmt.Errorf("%w: pair at least one other device first, so a lost device does not lock you out", errLockout)
	}
	return nil
}

// pairBase picks the address a phone reaches from config only, never from the request Host:
// AGENT_MONITOR_PAIR_BASE, the bind address, the first AGENT_MONITOR_ALLOWED_HOSTS name, else loopback.
func pairBase(_ *http.Request, bind string, port int, addrs func() []net.IP) string {
	if v := strings.TrimRight(strings.TrimSpace(os.Getenv("AGENT_MONITOR_PAIR_BASE")), "/"); v != "" {
		return v
	}
	ip := net.ParseIP(strings.Trim(bind, "[]"))
	if ip != nil && isTailscaleIP(ip) {
		return fmt.Sprintf("http://%s:%d", ip, port)
	}
	if ip != nil && ip.IsUnspecified() {
		for _, a := range addrs() {
			if isTailscaleIP(a) {
				return fmt.Sprintf("http://%s:%d", a, port)
			}
		}
	} else if ip != nil && !ip.IsLoopback() {
		return "http://" + net.JoinHostPort(ip.String(), fmt.Sprint(port))
	}
	for _, h := range strings.Split(os.Getenv("AGENT_MONITOR_ALLOWED_HOSTS"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			return "https://" + h
		}
	}
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

var tailscaleNet = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

func isTailscaleIP(ip net.IP) bool { return ip.To4() != nil && tailscaleNet.Contains(ip) }

func interfaceIPs() []net.IP {
	var out []net.IP
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ip, _, err := net.ParseCIDR(a.String()); err == nil {
			out = append(out, ip)
		}
	}
	return out
}

// secureRequest trusts X-Forwarded-Proto only from a local proxy such as tailscale serve.
func secureRequest(r *http.Request) bool {
	return r.TLS != nil || (isLoopbackRemote(r.RemoteAddr) && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"))
}

func registerDeviceRoutes(mux *http.ServeMux) {
	if deviceStore == nil {
		s, err := openDeviceStore(stateDir())
		if err != nil {
			log.Printf("devices: disabled: %v", err)
			return
		}
		deviceStore = s
	}
	deviceRoutes(mux, deviceStore)
	bioRoutes(mux, deviceStore)
}

func deviceRoutes(mux *http.ServeMux, s *DeviceStore) {
	mux.HandleFunc("GET /api/devices", func(w http.ResponseWriter, r *http.Request) {
		cur := ""
		if d := requestDevice(r); d != nil {
			cur = d.ID
		}
		writeJSON(w, s.List(cur))
	})
	mux.HandleFunc("POST /api/devices", func(w http.ResponseWriter, r *http.Request) {
		if !localCaller(r) {
			writeJSONStatus(w, http.StatusForbidden, map[string]any{"error": "add devices on the Mac itself"})
			return
		}
		var in struct {
			Name  string `json:"name"`
			Kind  string `json:"kind"`
			Level string `json:"level"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "bad json"})
			return
		}
		d, tok, err := s.CreateGrant(in.Name, in.Kind, in.Level)
		if err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		log.Printf("devices: created %s (%s)", d.ID, d.Name)
		base := pairBase(r, os.Getenv("AGENT_MONITOR_BIND"), daemonPort(), interfaceIPs)
		writeJSONStatus(w, http.StatusCreated, map[string]any{"device": d, "token": tok, "pairUrl": base + "/pair#" + s.NewPairCode(tok)})
	})
	mux.HandleFunc("PATCH /api/devices/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !localCaller(r) {
			writeJSONStatus(w, http.StatusForbidden, map[string]any{"error": "change access on the Mac itself"})
			return
		}
		var in struct {
			Level string `json:"level"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "bad json"})
			return
		}
		d, ok, err := s.SetLevel(r.PathValue("id"), in.Level)
		switch {
		case !ok:
			writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "device not found"})
		case err != nil:
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		default:
			log.Printf("devices: %s (%s) is now %s", d.ID, d.Name, d.Level)
			liveConns.closeDevice(r, d.ID) // open streams were authorised at the old level
			writeJSON(w, map[string]any{"ok": true, "device": d})
		}
	})
	mux.HandleFunc("DELETE /api/devices/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !localCaller(r) {
			if d := requestDevice(r); d == nil || d.ID != id {
				writeJSONStatus(w, http.StatusForbidden, map[string]any{"error": "a device may revoke only itself; revoke other devices on the Mac"})
				return
			}
		}
		// A remote caller may not revoke the last active device while tokens are required.
		if requireDeviceToken() && !localCaller(r) && s.ActiveExcept(id) == 0 {
			writeJSONStatus(w, http.StatusConflict, map[string]any{"error": "this is the last active device and device tokens are required: revoking it would lock out every remote device. Do it from the Mac, or turn the requirement off first"})
			return
		}
		d, ok, err := s.Revoke(id)
		switch {
		case err != nil:
			writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		case !ok:
			writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "device not found"})
		default:
			log.Printf("devices: revoked %s (%s)", d.ID, d.Name)
			onDeviceRevoked(r, d.ID)
			writeJSON(w, map[string]any{"ok": true, "device": d})
		}
	})
	mux.HandleFunc("POST /api/devices/session", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Code  string `json:"code"`
			Token string `json:"token"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in)
		// The {token} body stays for AgentBar; a non-token value there is a code from an older Pair page.
		tok, code := strings.TrimSpace(in.Token), strings.TrimSpace(in.Code)
		if code == "" && tok != "" && !strings.HasPrefix(tok, deviceTokenPrefix) {
			code = tok
		}
		if code != "" {
			tok = s.TakePairCode(code)
		}
		d := s.Lookup(tok)
		if d == nil {
			writeJSONStatus(w, http.StatusUnauthorized, map[string]any{"error": "invalid, used or expired pairing code, or a revoked device"})
			return
		}
		http.SetCookie(w, &http.Cookie{Name: deviceCookieName, Value: tok, Path: "/", HttpOnly: true,
			SameSite: http.SameSiteLaxMode, Secure: secureRequest(r), MaxAge: 400 * 24 * 3600})
		s.Touch(d.ID, remoteIP(r))
		s.BindIP(d.ID, remoteIP(r))
		d.Current = true
		if code != "" {
			// The one-time code is the secret: its claimant gets the token (a daemon or an app keeps it; a browser uses the cookie).
			writeJSON(w, map[string]any{"ok": true, "device": d, "token": tok})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "device": d})
	})
}
