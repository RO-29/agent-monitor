package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Web Push with no third-party service: VAPID (RFC 8292, ES256 JWT) and aes128gcm payloads (RFC 8291).

var b64u = base64.RawURLEncoding

// decodeB64 accepts url-safe or standard base64, padded or not.
func decodeB64(s string) ([]byte, error) {
	s = strings.TrimRight(strings.TrimSpace(s), "=")
	s = strings.NewReplacer("+", "-", "/", "_").Replace(s)
	return b64u.DecodeString(s)
}

type vapidKeys struct {
	priv   *ecdsa.PrivateKey
	public string // uncompressed P-256 point, base64url
}

type vapidFile struct {
	PublicKey  string `json:"publicKey"`
	PrivateKey string `json:"privateKey"` // PKCS#8, base64url
}

// loadVAPID reads stateDir/vapid.json, generating the key pair once (mode 0600).
func loadVAPID(dir string) (*vapidKeys, error) {
	path := filepath.Join(dir, "vapid.json")
	if b, err := os.ReadFile(path); err == nil {
		var f vapidFile
		if err := json.Unmarshal(b, &f); err != nil {
			return nil, fmt.Errorf("vapid.json: %w", err)
		}
		der, err := b64u.DecodeString(f.PrivateKey)
		if err != nil {
			return nil, errors.New("vapid.json: bad private key encoding")
		}
		k, err := x509.ParsePKCS8PrivateKey(der)
		if err != nil {
			return nil, errors.New("vapid.json: bad private key")
		}
		priv, ok := k.(*ecdsa.PrivateKey)
		if !ok {
			return nil, errors.New("vapid.json: not an ECDSA key")
		}
		return vapidFromPriv(priv)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	v, err := vapidFromPriv(priv)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	b, _ := json.MarshalIndent(vapidFile{PublicKey: v.public, PrivateKey: b64u.EncodeToString(der)}, "", "  ")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return nil, err
	}
	return v, nil
}

func vapidFromPriv(priv *ecdsa.PrivateKey) (*vapidKeys, error) {
	pub, err := priv.PublicKey.Bytes()
	if err != nil {
		return nil, err
	}
	return &vapidKeys{priv: priv, public: b64u.EncodeToString(pub)}, nil
}

// vapidJWT signs {aud, exp, sub} for the push service origin.
func (v *vapidKeys) jwt(endpoint, sub string, now time.Time) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return "", errors.New("bad push endpoint")
	}
	head := b64u.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims, _ := json.Marshal(map[string]any{"aud": u.Scheme + "://" + u.Host, "exp": now.Add(12 * time.Hour).Unix(), "sub": sub})
	signing := head + "." + b64u.EncodeToString(claims)
	h := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, v.priv, h[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signing + "." + b64u.EncodeToString(sig), nil
}

// pushSub is a browser PushSubscription as PushSubscription.toJSON() sends it.
type pushSub struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// pushServiceSuffixes are the Web Push services browsers use; any other host is refused.
var pushServiceSuffixes = []string{"push.apple.com", "fcm.googleapis.com", "updates.push.services.mozilla.com",
	"push.services.mozilla.com", "notify.windows.com"}

// pushHostAllowed is swapped in tests that run a local push server.
var pushHostAllowed = func(u *url.URL) bool {
	if p := u.Port(); p != "" && p != "443" {
		return false
	}
	h := strings.ToLower(u.Hostname())
	for _, s := range pushServiceSuffixes {
		if h == s || strings.HasSuffix(h, "."+s) {
			return true
		}
	}
	return false
}

func (s *pushSub) validate() error {
	u, err := url.Parse(s.Endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return errors.New("endpoint must be an https URL")
	}
	if !pushHostAllowed(u) {
		return errors.New("endpoint is not a known Web Push service")
	}
	if p, err := decodeB64(s.Keys.P256dh); err != nil || len(p) != 65 || p[0] != 4 {
		return errors.New("keys.p256dh must be an uncompressed P-256 key")
	}
	if a, err := decodeB64(s.Keys.Auth); err != nil || len(a) != 16 {
		return errors.New("keys.auth must be 16 bytes")
	}
	return nil
}

const pushRecordSize = 4096

// encryptPush builds one aes128gcm record for the subscriber's key and auth secret (RFC 8291).
func encryptPush(uaPub, auth, payload []byte, random io.Reader) ([]byte, error) {
	if len(payload) > pushRecordSize-17-86 {
		return nil, errors.New("push payload too large")
	}
	curve := ecdh.P256()
	ua, err := curve.NewPublicKey(uaPub)
	if err != nil {
		return nil, err
	}
	as, err := curve.GenerateKey(random)
	if err != nil {
		return nil, err
	}
	asPub := as.PublicKey().Bytes()
	secret, err := as.ECDH(ua)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := io.ReadFull(random, salt); err != nil {
		return nil, err
	}
	cek, nonce, err := pushKeys(secret, auth, uaPub, asPub, salt)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plain := append(append([]byte{}, payload...), 0x02)
	var out bytes.Buffer
	out.Write(salt)
	_ = binary.Write(&out, binary.BigEndian, uint32(pushRecordSize))
	out.WriteByte(byte(len(asPub)))
	out.Write(asPub)
	out.Write(gcm.Seal(nil, nonce, plain, nil))
	return out.Bytes(), nil
}

// pushKeys derives the content key and nonce from the ECDH secret (RFC 8291 section 3.4).
func pushKeys(secret, auth, uaPub, asPub, salt []byte) (cek, nonce []byte, err error) {
	prkKey, err := hkdf.Extract(sha256.New, secret, auth)
	if err != nil {
		return nil, nil, err
	}
	info := "WebPush: info\x00" + string(uaPub) + string(asPub)
	ikm, err := hkdf.Expand(sha256.New, prkKey, info, 32)
	if err != nil {
		return nil, nil, err
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, nil, err
	}
	if cek, err = hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16); err != nil {
		return nil, nil, err
	}
	nonce, err = hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	return cek, nonce, err
}

// webPusher sends notifications to the stored subscriptions.
type webPusher struct {
	keys   *vapidKeys
	sub    string
	client *http.Client
}

// newPushClient follows no redirect, so a push service answer cannot move the POST to another host.
func newPushClient() *http.Client {
	return &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// defaultVapidSubject avoids a localhost address, which Apple's push service is reported to refuse.
const defaultVapidSubject = "mailto:agent-monitor@example.com"

// vapidSubject is AGENT_MONITOR_VAPID_SUB when it is a mailto: or https: URL, else the default.
func vapidSubject() string {
	v := strings.TrimSpace(os.Getenv("AGENT_MONITOR_VAPID_SUB"))
	if (strings.HasPrefix(v, "mailto:") || strings.HasPrefix(v, "https://")) && !strings.Contains(strings.ToLower(v), "localhost") {
		return v
	}
	if v != "" {
		log.Printf("notify: AGENT_MONITOR_VAPID_SUB must be a mailto: or https:// URL that is not localhost; using %s", defaultVapidSubject)
	}
	return defaultVapidSubject
}

// send posts one encrypted message; the status tells the caller to drop a gone subscription (404/410).
func (p *webPusher) send(s pushSub, payload []byte, urgency string) (int, error) {
	uaPub, err := decodeB64(s.Keys.P256dh)
	if err != nil {
		return 0, err
	}
	auth, err := decodeB64(s.Keys.Auth)
	if err != nil {
		return 0, err
	}
	body, err := encryptPush(uaPub, auth, payload, rand.Reader)
	if err != nil {
		return 0, err
	}
	if err := s.validate(); err != nil {
		return 0, err
	}
	tok, err := p.keys.jwt(s.Endpoint, p.sub, time.Now())
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequest(http.MethodPost, s.Endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("TTL", "86400")
	req.Header.Set("Urgency", urgency)
	req.Header.Set("Authorization", "vapid t="+tok+", k="+p.keys.public)
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("push service answered %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}
