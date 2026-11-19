package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// BarkConfig sends alerts to the Bark iOS app, so a locked phone gets them; the payload is AES-256-CBC encrypted.
type BarkConfig struct {
	Enabled   bool   `json:"enabled"`
	Server    string `json:"server"`
	DeviceKey string `json:"deviceKey"`
	Key       string `json:"key"`   // 32 ASCII characters, typed into Bark's encryption settings
	IV        string `json:"iv"`    // 16 characters for Bark's settings form; each push sends its own IV
	Sound     string `json:"sound"` // Bark sound name; "" = the Bark default
}

const barkDefaultServer = "https://api.day.app"

var bark = struct {
	mu          sync.Mutex
	cfg         BarkConfig
	loaded      bool
	phoneActive time.Time // the iPhone app is in the foreground until then, and alerts there itself
	client      *http.Client
}{client: &http.Client{Timeout: 15 * time.Second}}

func barkPath() string { return filepath.Join(stateDir(), "push-bark.json") }

const barkAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnpqrstuvwxyz23456789"

func barkRandom(n int) string {
	b := make([]byte, n)
	for i := range b {
		k, err := rand.Int(rand.Reader, big.NewInt(int64(len(barkAlphabet))))
		if err != nil {
			panic("crypto/rand: " + err.Error())
		}
		b[i] = barkAlphabet[k.Int64()]
	}
	return string(b)
}

// barkConfig loads the config once and makes a key and IV when there are none.
func barkConfig() BarkConfig {
	bark.mu.Lock()
	defer bark.mu.Unlock()
	if !bark.loaded {
		bark.loaded = true
		if raw, err := os.ReadFile(barkPath()); err == nil {
			if err := json.Unmarshal(raw, &bark.cfg); err != nil {
				log.Printf("push: bark config unreadable, keeping what parsed: %v", err)
			}
		}
	}
	changed := false
	if len(bark.cfg.Key) != 32 {
		bark.cfg.Key, changed = barkRandom(32), true
	}
	if len(bark.cfg.IV) != 16 {
		bark.cfg.IV, changed = barkRandom(16), true
	}
	if changed {
		barkSaveLocked()
	}
	if bark.cfg.Server == "" {
		bark.cfg.Server = barkDefaultServer
	}
	return bark.cfg
}

func barkSaveLocked() {
	raw, _ := json.MarshalIndent(bark.cfg, "", "  ")
	_ = os.MkdirAll(filepath.Dir(barkPath()), 0o700)
	_ = os.WriteFile(barkPath(), raw, 0o600)
}

// barkSeal encrypts a payload the way Bark's AES256 / CBC / pkcs7 setting decrypts it (its documented example).
func barkSeal(key, iv string, payload any) (string, error) {
	plain, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return barkSealBytes(key, iv, plain)
}

func barkSealBytes(key, iv string, plain []byte) (string, error) {
	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		return "", err
	}
	if len(iv) != aes.BlockSize {
		return "", errors.New("the IV must be 16 characters")
	}
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	plain = append(append([]byte{}, plain...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	out := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, []byte(iv)).CryptBlocks(out, plain)
	return base64.StdEncoding.EncodeToString(out), nil
}

// barkLink is the agent-monitor URL a tapped push opens.
func barkLink(no ONotification) string {
	switch {
	case strings.HasPrefix(no.URL, "https://"):
		return no.URL // the iPhone install page opens in Safari
	case no.ThreadID != "":
		return "agentmonitor://thread/" + url.PathEscape(no.ThreadID)
	case no.Kind == "needs-you":
		return "agentmonitor://approvals"
	default:
		return "agentmonitor://alerts"
	}
}

func barkPost(cfg BarkConfig, body map[string]any) error {
	body["device_key"] = cfg.DeviceKey
	b, _ := json.Marshal(body)
	resp, err := bark.client.Post(strings.TrimRight(cfg.Server, "/")+"/push", "application/json; charset=utf-8", bytes.NewReader(b))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var r struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(out, &r)
	if resp.StatusCode != http.StatusOK || (r.Code != 0 && r.Code != http.StatusOK) {
		return fmt.Errorf("bark answered %d: %s", resp.StatusCode, clipString(strings.TrimSpace(r.Message+" "+string(out)), 200))
	}
	return nil
}

// barkPushID is the Bark notification id: the same id replaces, and delete removes it.
func barkPushID(id string) string { return "am-" + id }

// barkSend pushes one alert unless push is off, quiet, or the iPhone app is open and alerts itself.
func barkSend(no ONotification, force bool) error {
	cfg := barkConfig()
	if !force {
		bark.mu.Lock()
		active := time.Now().Before(bark.phoneActive)
		bark.mu.Unlock()
		if !cfg.Enabled || active || no.Quiet || no.Resolved {
			return nil
		}
	}
	if cfg.DeviceKey == "" {
		return errors.New("no Bark device key is set")
	}
	level := "active"
	if no.Kind == "needs-you" || no.Kind == "error" {
		level = "timeSensitive"
	}
	payload := map[string]string{"title": no.Title, "body": clipString(no.Body, 300), "url": barkLink(no),
		"group": "agent-monitor", "level": level, "isArchive": "0"}
	if no.ThreadID != "" {
		payload["group"] = "am-" + no.ThreadID
	}
	if cfg.Sound != "" {
		payload["sound"] = cfg.Sound
	}
	iv := barkRandom(16)
	ct, err := barkSeal(cfg.Key, iv, payload)
	if err != nil {
		return err
	}
	return barkPost(cfg, map[string]any{"ciphertext": ct, "iv": iv, "id": barkPushID(no.ID)})
}

// One worker sends and deletes in order, so a delete never overtakes its send.
var barkWork = struct {
	once sync.Once
	ops  chan func()
	sent map[string]bool // alerts that reached Bark; only these get a delete
}{ops: make(chan func(), 128), sent: map[string]bool{}}

func barkDo(op func()) {
	barkWork.once.Do(func() {
		go func() {
			for op := range barkWork.ops {
				op()
			}
		}()
	})
	select {
	case barkWork.ops <- op:
	default:
		log.Printf("push: bark queue full; an alert was dropped")
	}
}

// barkQueue pushes a new alert; while the iPhone app is open it waits, then goes unless read or resolved.
func barkQueue(no ONotification, current func(id string) (ONotification, bool)) {
	barkDo(func() {
		bark.mu.Lock()
		wait := time.Until(bark.phoneActive)
		bark.mu.Unlock()
		if wait > 0 {
			time.AfterFunc(wait+time.Second, func() {
				if cur, ok := current(no.ID); ok && !cur.Read && !cur.Resolved {
					barkQueue(cur, current)
				}
			})
			return
		}
		if err := barkSend(no, false); err != nil {
			log.Printf("push: bark: %v", err)
			return
		}
		if barkConfig().Enabled && !no.Quiet && !no.Resolved {
			if len(barkWork.sent) > 2000 {
				barkWork.sent = map[string]bool{}
			}
			barkWork.sent[no.ID] = true
		}
	})
}

// barkDelete removes an alert from the lock screen once its approval ended elsewhere.
func barkDelete(id string) {
	barkDo(func() {
		if !barkWork.sent[id] {
			return
		}
		delete(barkWork.sent, id)
		cfg := barkConfig()
		if !cfg.Enabled || cfg.DeviceKey == "" {
			return
		}
		if err := barkPost(cfg, map[string]any{"id": barkPushID(id), "delete": "1"}); err != nil {
			log.Printf("push: bark delete: %v", err)
		}
	})
}

// barkView is what the settings pages show; the device key shows only its last 4 characters.
func barkView() map[string]any {
	cfg := barkConfig()
	bark.mu.Lock()
	active := time.Now().Before(bark.phoneActive)
	bark.mu.Unlock()
	hint := ""
	if n := len(cfg.DeviceKey); n > 4 {
		hint = "…" + cfg.DeviceKey[n-4:]
	}
	return map[string]any{"enabled": cfg.Enabled, "server": cfg.Server, "deviceKeySet": cfg.DeviceKey != "", "deviceKeyHint": hint,
		"key": cfg.Key, "iv": cfg.IV, "algorithm": "AES256", "mode": "CBC", "padding": "pkcs7", "sound": cfg.Sound, "phoneActive": active}
}

// barkDeviceKey takes a bare key or a Bark URL such as https://api.day.app/<key>/ and returns the key.
func barkDeviceKey(s string) (key, server string) {
	s = strings.TrimSpace(s)
	if u, err := url.Parse(s); err == nil && u.Scheme == "https" && u.Host != "" {
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		return parts[0], u.Scheme + "://" + u.Host
	}
	return s, ""
}

func registerPushRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/push/bark", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, barkView()) })
	mux.HandleFunc("POST /api/push/bark", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Enabled   *bool   `json:"enabled"`
			Server    *string `json:"server"`
			DeviceKey *string `json:"deviceKey"`
			Sound     *string `json:"sound"`
			NewKey    bool    `json:"newKey"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&body); err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		barkConfig()
		bark.mu.Lock()
		c := bark.cfg
		if body.DeviceKey != nil {
			k, srv := barkDeviceKey(*body.DeviceKey)
			if k != "" && !barkKeyRe(k) {
				bark.mu.Unlock()
				writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "that is not a Bark device key"})
				return
			}
			c.DeviceKey = k
			if srv != "" {
				c.Server = srv
			}
		}
		if body.Server != nil {
			s := strings.TrimRight(strings.TrimSpace(*body.Server), "/")
			if u, err := url.Parse(s); s != "" && (err != nil || u.Scheme != "https" || u.Host == "") {
				bark.mu.Unlock()
				writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "the server must be an https URL"})
				return
			}
			c.Server = s
		}
		if body.Sound != nil {
			c.Sound = strings.TrimSpace(*body.Sound)
		}
		if body.Enabled != nil {
			c.Enabled = *body.Enabled
		}
		if body.NewKey {
			c.Key, c.IV = barkRandom(32), barkRandom(16)
		}
		bark.cfg = c
		barkSaveLocked()
		bark.mu.Unlock()
		writeJSON(w, barkView())
	})
	mux.HandleFunc("POST /api/push/bark/test", func(w http.ResponseWriter, r *http.Request) {
		no := ONotification{ID: "test-" + barkRandom(6), Kind: "needs-you", Title: "agent-monitor test",
			Body: "Lock screen alerts work. A tap opens agent-monitor."}
		if err := barkSend(no, true); err != nil {
			writeJSONStatus(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})
	// The iPhone app says when it is in the foreground (and alerts by itself); a lapse of 90 s counts as background.
	mux.HandleFunc("POST /api/push/presence", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Active bool `json:"active"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&body)
		bark.mu.Lock()
		if body.Active {
			bark.phoneActive = time.Now().Add(90 * time.Second)
		} else {
			bark.phoneActive = time.Time{}
		}
		bark.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true})
	})
}

func barkKeyRe(k string) bool {
	if len(k) < 8 || len(k) > 64 {
		return false
	}
	for _, c := range k {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
