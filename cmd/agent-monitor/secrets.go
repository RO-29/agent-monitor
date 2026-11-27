package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"
)

// An agent asks for a secret with the request_secret MCP tool; the person answers on a private card.
// The value goes to a named pipe that gives it out once, so it never reaches the chat, a transcript or the disk.

const (
	secretWait = 10 * time.Minute // how long the card waits for an answer
	secretTTL  = 10 * time.Minute // how long the pipe waits for its one read
)

type SecretRequest struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Reason     string    `json:"reason,omitempty"`
	URL        string    `json:"url,omitempty"`
	Agent      string    `json:"agent,omitempty"`
	Cwd        string    `json:"cwd,omitempty"`
	CreatedAt  int64     `json:"createdAt"`
	State      string    `json:"state"` // pending, saved, declined, timed_out
	DecidedAt  int64     `json:"decidedAt,omitempty"`
	DeviceName string    `json:"deviceName,omitempty"`
	Proof      *bioProof `json:"proof,omitempty"`
	file       string
	done       chan struct{}
}

type secretStore struct {
	mu   sync.Mutex
	reqs map[string]*SecretRequest
	dir  func() string
}

var secrets = &secretStore{reqs: map[string]*SecretRequest{}, dir: func() string { return filepath.Join(stateDir(), "secrets") }}

func (s *secretStore) add(name, reason, url, agent, cwd string) *SecretRequest {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	r := &SecretRequest{ID: "sec_" + hex.EncodeToString(b), Name: name, Reason: reason, URL: url, Agent: agent, Cwd: cwd,
		CreatedAt: time.Now().UnixMilli(), State: "pending", done: make(chan struct{})}
	s.mu.Lock()
	for id, old := range s.reqs {
		if old.State != "pending" && time.Since(time.UnixMilli(old.CreatedAt)) > time.Hour {
			delete(s.reqs, id)
		}
	}
	s.reqs[r.ID] = r
	s.mu.Unlock()
	return r
}

func (s *secretStore) snapshot(r *SecretRequest) SecretRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := *r
	c.done = nil
	return c
}

func (s *secretStore) pending() []SecretRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []SecretRequest{}
	for _, r := range s.reqs {
		if r.State == "pending" {
			c := *r
			c.done = nil
			out = append(out, c)
		}
	}
	return out
}

func (s *secretStore) get(id string) *SecretRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reqs[id]
}

// finish ends a pending request once; a second answer gets an error.
func (s *secretStore) finish(id, state, file string, d *Device, p *bioProof) (*SecretRequest, error) {
	s.mu.Lock()
	r := s.reqs[id]
	if r == nil {
		s.mu.Unlock()
		return nil, errors.New("no such secret request")
	}
	if r.State != "pending" {
		s.mu.Unlock()
		return nil, errors.New("this secret request was already answered")
	}
	r.State, r.DecidedAt, r.file, r.Proof = state, time.Now().UnixMilli(), file, p
	if d != nil {
		r.DeviceName = d.Name
	}
	close(r.done)
	s.mu.Unlock()
	if notifier != nil {
		notifier.Resolve("secret:" + id)
	}
	publishV3("secret", "secret", s.snapshot(r))
	return r, nil
}

// serveOnce gives val to the first reader of a new named pipe, then removes it; it also goes away after ttl.
func serveOnce(dir string, val []byte, ttl time.Duration) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	_ = os.Chmod(dir, 0o700)
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	path := filepath.Join(dir, hex.EncodeToString(b))
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		return "", err
	}
	var expired atomic.Bool
	t := time.AfterFunc(ttl, func() {
		expired.Store(true)
		if f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0); err == nil {
			f.Close() // lets the waiting writer return
		}
		os.Remove(path)
	})
	go func() {
		defer clear(val)
		f, err := os.OpenFile(path, os.O_WRONLY, 0) // waits for the reader
		if err != nil {
			return
		}
		if !expired.Load() {
			_, _ = f.Write(val)
		}
		f.Close()
		t.Stop()
		os.Remove(path)
	}()
	return path, nil
}

// clearSecretDir removes pipes a previous daemon left behind; nobody can read them now.
func clearSecretDir(dir string) {
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		os.Remove(filepath.Join(dir, e.Name()))
	}
}

func registerSecretRoutes(mux *http.ServeMux) {
	clearSecretDir(secrets.dir())
	bad := func(w http.ResponseWriter, code int, msg string) {
		writeJSONStatus(w, code, map[string]any{"error": msg})
	}
	// The MCP helper on this Mac opens a card (local only, see access.go).
	mux.HandleFunc("POST /api/secrets/request", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Name, Reason, URL, Agent, Cwd string }
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
			bad(w, http.StatusBadRequest, "bad json")
			return
		}
		in.Name = strings.TrimSpace(in.Name)
		if in.Name == "" || utf8.RuneCountInString(in.Name) > 80 || utf8.RuneCountInString(in.Reason) > 500 {
			bad(w, http.StatusBadRequest, "name is required (at most 80 characters); reason at most 500")
			return
		}
		if in.URL != "" && !strings.HasPrefix(in.URL, "https://") {
			in.URL = ""
		}
		req := secrets.add(in.Name, in.Reason, in.URL, in.Agent, in.Cwd)
		snap := secrets.snapshot(req)
		publishV3("secret", "secret", snap)
		if notifier != nil {
			who := firstOf(in.Agent, filepath.Base(in.Cwd), "An agent")
			notifier.Emit(notifyEvent{Kind: "needs-you", Title: who + " asks for a secret: " + in.Name, Body: in.Reason, Keys: []string{"secret:" + req.ID}})
		}
		writeJSON(w, snap)
	})
	mux.HandleFunc("GET /api/secrets/{id}/await", func(w http.ResponseWriter, r *http.Request) {
		req := secrets.get(r.PathValue("id"))
		if req == nil {
			bad(w, http.StatusNotFound, "no such secret request")
			return
		}
		wait := secretWait
		if v, err := strconv.Atoi(r.URL.Query().Get("timeout")); err == nil && v > 0 && v < 3600 {
			wait = time.Duration(v) * time.Second
		}
		select {
		case <-req.done:
		case <-time.After(wait):
			_, _ = secrets.finish(req.ID, "timed_out", "", nil, nil)
		case <-r.Context().Done():
			_, _ = secrets.finish(req.ID, "cancelled", "", nil, nil)
			return
		}
		snap := secrets.snapshot(req)
		writeJSON(w, map[string]any{"state": snap.State, "file": req.file, "ttlSec": int(secretTTL.Seconds())})
	})
	mux.HandleFunc("GET /api/secrets", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"secrets": secrets.pending()})
	})
	mux.HandleFunc("POST /api/secrets/{id}/answer", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Type   string `json:"type"`
			Secret string `json:"secret"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
			bad(w, http.StatusBadRequest, "bad json")
			return
		}
		id := r.PathValue("id")
		switch in.Type {
		case "decline":
			if _, err := secrets.finish(id, "declined", "", callerDevice(r), nil); err != nil {
				bad(w, http.StatusConflict, err.Error())
				return
			}
			writeJSON(w, map[string]any{"ok": true, "state": "declined"})
		case "save":
			val := []byte(in.Secret)
			in.Secret = ""
			if len(strings.TrimSpace(string(val))) == 0 {
				bad(w, http.StatusBadRequest, "the secret is empty")
				return
			}
			if req := secrets.get(id); req == nil || req.State != "pending" {
				bad(w, http.StatusConflict, "this secret request is not waiting")
				return
			}
			path, err := serveOnce(secrets.dir(), val, secretTTL)
			if err != nil {
				clear(val)
				bad(w, http.StatusInternalServerError, "could not make the one-read pipe: "+err.Error())
				return
			}
			if _, err := secrets.finish(id, "saved", path, callerDevice(r), requestProof(r)); err != nil {
				os.Remove(path)
				bad(w, http.StatusConflict, err.Error())
				return
			}
			writeJSON(w, map[string]any{"ok": true, "state": "saved"})
		default:
			bad(w, http.StatusBadRequest, "type must be save or decline")
		}
	})
}
