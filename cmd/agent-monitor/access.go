package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Level is what a paired device or machine may do on this daemon. This Mac itself is always LevelAdmin.
type Level int

const (
	LevelNone Level = iota
	LevelView
	LevelControl
	LevelAdmin
	levelLocal // a route only this Mac may call (hooks, host and agent registration)
)

func (l Level) String() string {
	switch l {
	case LevelView:
		return "view"
	case LevelControl:
		return "control"
	case LevelAdmin:
		return "admin"
	case levelLocal:
		return "local"
	}
	return "none"
}

func parseLevel(s string) Level {
	switch s {
	case "view":
		return LevelView
	case "control":
		return LevelControl
	case "admin":
		return LevelAdmin
	}
	return LevelNone
}

type accessRule struct {
	methods string // "" any; else space-separated, e.g. "GET HEAD"
	prefix  string // path prefix; a trailing "$" means an exact path
	suffix  string // optional path suffix after the prefix
	level   Level
}

// accessRules: first match wins. A route with no rule needs admin, so a new route is closed to view and control.
var accessRules = []accessRule{
	// Inputs that only processes on this Mac send: hooks, agent and host registration, permission servers.
	{"", "/events/", "", levelLocal},
	{"", "/api/approvals/hook/", "", levelLocal},
	{"", "/api/pane/announce$", "", levelLocal},
	{"", "/api/session/owner/", "", levelLocal},
	{"POST", "/api/hosts$", "", levelLocal},
	{"DELETE", "/api/hosts/", "", levelLocal},
	{"", "/api/permission/request$", "", levelLocal},
	{"", "/api/permission/wait/", "", levelLocal},
	{"", "/api/talk/request$", "", levelLocal},
	{"", "/debug/", "", levelLocal},
	{"", "/api/notify/install$", "", levelLocal},

	// Wiring edits agent config files: reading the plan needs control, approving it needs admin (named, not the default).
	{"POST", "/api/agents/wiring/confirm$", "", LevelAdmin},
	{"GET HEAD", "/api/agents/wiring$", "", LevelControl},
	{"GET HEAD", "/api/agents/registry$", "", LevelView},

	// Terminals are a shell: admin only.
	{"", "/api/term$", "", LevelAdmin},
	{"", "/api/session/", "/term", LevelAdmin},
	{"", "/api/orch/threads/", "/term", LevelAdmin},
	// Wide reads that can carry secrets: admin only.
	{"", "/api/session/", "/pane", LevelAdmin},
	{"", "/api/session/pane-view/", "", LevelAdmin},
	{"", "/api/session/pane/", "", LevelAdmin},
	{"", "/api/session/", "/commands", LevelAdmin},
	{"", "/api/search", "", LevelAdmin},
	{"", "/api/prompts$", "", LevelAdmin},
	{"", "/api/commands/", "", LevelAdmin},
	{"", "/api/orch/git", "", LevelAdmin},
	{"", "/api/orch/threads/", "/git", LevelAdmin},
	{"", "/api/orch/threads/", "/diff", LevelAdmin},
	{"POST", "/api/orch/threads/", "/restore", LevelAdmin},
	{"POST", "/api/orch/threads/", "/commit", LevelAdmin},
	{"POST", "/api/orch/threads/", "/push", LevelAdmin},
	{"POST", "/api/orch/threads/", "/pr", LevelAdmin},
	{"POST", "/api/orch/threads/", "/merge", LevelAdmin},
	{"POST", "/api/orch/threads/", "/worktree/remove", LevelAdmin},
	{"", "/api/orch/files$", "", LevelAdmin},

	// Driving the agents.
	{"POST", "/api/approvals/", "/decide", LevelControl},
	{"POST", "/api/session/prompt/", "", LevelControl},
	{"POST", "/api/session/end/", "", LevelControl},
	{"POST", "/api/session/resume/", "", LevelControl},
	{"POST", "/api/session/send/", "", LevelControl},
	{"POST", "/api/session/keys/", "", LevelControl},
	{"POST", "/api/session/cancel/", "", LevelControl},
	{"POST", "/api/session/", "/cli-queue/", LevelControl},
	{"POST", "/api/session/", "/answer", LevelControl},
	{"POST", "/api/permission/", "/respond", LevelControl},
	{"", "/api/queue/", "", LevelControl},
	{"POST", "/api/hosts/", "/answer", LevelControl},
	{"POST", "/api/orch/continue$", "", LevelControl},
	{"POST", "/api/orch/terminal$", "", LevelControl},
	{"POST", "/api/orch/uploads$", "", LevelControl},
	{"GET", "/api/orch/uploads/", "", LevelControl},
	{"POST", "/api/orch/threads", "", LevelControl},
	{"POST", "/api/session-title$", "", LevelControl},
	{"POST DELETE", "/api/devices/me/bio-key$", "", LevelView}, // a device sets its own Face ID key; a replacement needs the old one
	{"POST DELETE", "/api/devices/me/passkey$", "", LevelView},
	{"POST", "/api/ios/renew$", "", LevelView}, // starts ios/resign.sh at most once per 30 min; view lets a peer pass the phone's ask on

	// Reading what agents do and ask.
	{"GET HEAD", "/ws$", "", LevelView},
	{"GET HEAD", "/api/needs-you$", "", LevelView},
	{"GET HEAD", "/api/sessions$", "", LevelView},
	{"GET HEAD", "/api/threads$", "", LevelView},
	{"GET HEAD", "/api/thread/", "", LevelView},
	{"GET HEAD", "/api/v3/", "", LevelView},
	{"GET HEAD", "/api/approvals$", "", LevelView},
	{"GET HEAD", "/api/errors$", "", LevelView},
	{"GET HEAD", "/api/health$", "", LevelView},
	{"GET HEAD", "/api/usage/limits$", "", LevelView},
	{"GET HEAD", "/api/orch/agents$", "", LevelView},
	{"GET HEAD", "/api/orch/stream$", "", LevelView},
	{"GET HEAD", "/api/orch/threads", "", LevelView},
	{"GET HEAD", "/api/orch/costs$", "", LevelView},
	{"GET HEAD", "/api/queue/", "", LevelView},
	{"GET HEAD", "/api/notify", "", LevelView},
	{"POST", "/api/notify/read$", "", LevelView},   // read marks only: a paired phone clears its own unread count
	{"POST", "/api/push/presence$", "", LevelView}, // only delays lock-screen alerts while the app shows them
	{"GET HEAD", "/api/session/", "", LevelView},
}

// routeLevel is the least level a request needs; the web app's own files need nothing beyond sign-in.
func routeLevel(method, path string) Level {
	if !strings.HasPrefix(path, "/api/") && !strings.HasPrefix(path, "/events/") && !strings.HasPrefix(path, "/debug/") && path != "/ws" {
		return LevelView // the page and its assets
	}
	for _, r := range accessRules {
		if r.methods != "" && !strings.Contains(" "+r.methods+" ", " "+method+" ") {
			continue
		}
		p, exact := strings.CutSuffix(r.prefix, "$")
		if exact {
			if path != p {
				continue
			}
		} else if !strings.HasPrefix(path, p) {
			continue
		}
		if r.suffix != "" {
			rest := strings.TrimPrefix(path, p)
			if strings.HasSuffix(r.suffix, "/") {
				if !strings.Contains(rest, r.suffix) {
					continue
				}
			} else if !strings.HasSuffix(rest, r.suffix) {
				continue
			}
		}
		return r.level
	}
	return LevelAdmin
}

type callerKey struct{}

type caller struct {
	level  Level
	device *Device
}

func withCaller(r *http.Request, c caller) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), callerKey{}, c))
}

// callerLevel is the level of the request's sender; this Mac and the password are admin.
// belowAdmin: the access layer marked this caller below admin (a direct call in a test has no mark).
func belowAdmin(r *http.Request) bool {
	c, ok := r.Context().Value(callerKey{}).(caller)
	return ok && c.level < LevelAdmin
}

func callerLevel(r *http.Request) Level {
	if c, ok := r.Context().Value(callerKey{}).(caller); ok {
		return c.level
	}
	if localCaller(r) {
		return LevelAdmin
	}
	return LevelNone
}

// statusRecorder keeps the response code for the access log; hijacking (websockets) passes through.
type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(c int)           { s.code = c; s.ResponseWriter.WriteHeader(c) }
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
func (s *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := s.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("hijack not supported")
}
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

var accessLog = struct {
	sync.Mutex
	f *os.File
}{}

const accessLogMax = 10 << 20

// logAccess appends one JSON line per remote request to <state>/access.log (rotated at 10 MB).
func logAccess(r *http.Request, d *Device, need Level, code int) {
	accessLog.Lock()
	defer accessLog.Unlock()
	path := filepath.Join(stateDir(), "access.log")
	if accessLog.f == nil {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		accessLog.f = f
	}
	if st, err := accessLog.f.Stat(); err == nil && st.Size() > accessLogMax {
		accessLog.f.Close()
		_ = os.Rename(path, path+".1")
		accessLog.f = nil
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		accessLog.f = f
	}
	e := map[string]any{"ts": time.Now().UTC().Format(time.RFC3339), "ip": remoteIP(r), "method": r.Method, "path": r.URL.Path, "need": need.String(), "status": code}
	if d != nil {
		e["device"], e["name"], e["kind"], e["level"] = d.ID, d.Name, d.Kind, d.Level
	}
	b, _ := json.Marshal(e)
	_, _ = accessLog.f.Write(append(b, '\n'))
}

func callerDevice(r *http.Request) *Device {
	c, _ := r.Context().Value(callerKey{}).(caller)
	return c.device
}
