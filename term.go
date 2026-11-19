package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
)

const (
	termScrollback = 256 << 10
	termIdleKill   = 30 * time.Minute
	termMaxShells  = 6
)

// termManager owns the browser terminals: one login shell per thread (or per cwd for watched threads).
type termManager struct {
	mu       sync.Mutex
	sessions map[string]*termSession
	idle     time.Duration
	max      int
}

type termSession struct {
	m       *termManager
	key     string
	cwd     string
	cmd     *exec.Cmd
	pty     *os.File
	mu      sync.Mutex
	buf     []byte
	clients map[*termClient]struct{}
	timer   *time.Timer
	exited  bool
	cols    int
	rows    int
}

type termClient struct {
	conn *websocket.Conn
	out  chan termFrame
	once sync.Once
	done chan struct{}
}

type termFrame struct {
	binary bool
	data   []byte
}

var termMgr = newTermManager()

// termThreadCwd resolves a driven thread id to its cwd (swapped in tests).
var termThreadCwd = func(id string) (string, error) {
	if orchMgr == nil {
		return "", errOrchNotFound
	}
	th, err := orchMgr.store.Thread(id)
	if err != nil {
		return "", err
	}
	return th.Cwd, nil
}

func newTermManager() *termManager {
	return &termManager{sessions: map[string]*termSession{}, idle: termIdleKill, max: termMaxShells}
}

func registerTermRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/orch/threads/{id}/term", func(w http.ResponseWriter, r *http.Request) {
		cwd, err := termThreadCwd(r.PathValue("id"))
		if errors.Is(err, errOrchNotFound) {
			writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "thread not found"})
			return
		} else if err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		termMgr.serve(w, r, "thread:"+r.PathValue("id"), cwd)
	})
	mux.HandleFunc("GET /api/term", func(w http.ResponseWriter, r *http.Request) {
		cwd := r.URL.Query().Get("cwd")
		if !filepath.IsAbs(cwd) {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "cwd must be an absolute path"})
			return
		}
		cwd = filepath.Clean(cwd)
		if !knownCwd(cwd) {
			writeJSONStatus(w, http.StatusForbidden, map[string]any{"error": "cwd is not the folder of a watched session or a driven thread"})
			return
		}
		termMgr.serve(w, r, "cwd:"+cwd, cwd)
	})
}

// termAllowed: loopback always; remote only with remoteTerminal on AND a device token (never the password alone).
func termAllowed(r *http.Request) (bool, string) {
	if localCaller(r) {
		return true, ""
	}
	if !getSettings().RemoteTerminal {
		return false, "remote terminal is off (setting remoteTerminal)"
	}
	if requestDevice(r) == nil {
		return false, "remote terminal needs a paired device token; the password is not enough"
	}
	return true, ""
}

func (m *termManager) serve(w http.ResponseWriter, r *http.Request, key, cwd string) {
	if ok, why := termAllowed(r); !ok {
		writeJSONStatus(w, http.StatusForbidden, map[string]any{"error": why})
		return
	}
	if st, err := os.Stat(cwd); cwd == "" || err != nil || !st.IsDir() {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "cwd does not exist or is not a directory"})
		return
	}
	// Start a shell only for a same-origin WebSocket upgrade; a plain GET just learns that all checks pass.
	if !websocket.IsWebSocketUpgrade(r) || !sameOriginRequest(r) {
		writeJSONStatus(w, http.StatusUpgradeRequired, map[string]any{"error": "websocket upgrade from this origin required"})
		return
	}
	cols, rows := termSize(r.URL.Query().Get("cols"), 80), termSize(r.URL.Query().Get("rows"), 24)
	s, created, err := m.get(key, cwd, cols, rows)
	if err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, errTermLimit) {
			code = http.StatusTooManyRequests
		}
		writeJSONStatus(w, code, map[string]any{"error": err.Error()})
		return
	}
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c.SetReadLimit(1 << 20)
	cl := &termClient{conn: c, out: make(chan termFrame, 512), done: make(chan struct{})}
	go cl.writeLoop()
	if !s.attach(cl, created) {
		cl.close()
		return
	}
	defer s.detach(cl)
	for {
		mt, data, err := c.ReadMessage()
		if err != nil {
			return
		}
		if mt == websocket.BinaryMessage {
			s.input(data)
			continue
		}
		var msg struct {
			Type string `json:"type"`
			Data string `json:"data"`
			Cols int    `json:"cols"`
			Rows int    `json:"rows"`
		}
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		switch msg.Type {
		case "input":
			s.input([]byte(msg.Data))
		case "resize":
			s.resize(msg.Cols, msg.Rows)
		}
	}
}

func termSize(v string, def int) int {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 1000 {
		return def
	}
	return n
}

var errTermLimit = errors.New("too many terminals: close one first")

func (m *termManager) get(key, cwd string, cols, rows int) (*termSession, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s := m.sessions[key]; s != nil {
		return s, false, nil
	}
	if len(m.sessions) >= m.max {
		return nil, false, errTermLimit
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/zsh"
	}
	cmd := exec.Command(shell, "-l")
	cmd.Dir = cwd
	cmd.Env = termEnv(os.Environ())
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return nil, false, err
	}
	s := &termSession{m: m, key: key, cwd: cwd, cmd: cmd, pty: f, clients: map[*termClient]struct{}{}, cols: cols, rows: rows}
	s.timer = time.AfterFunc(m.idle, s.kill)
	m.sessions[key] = s
	go s.readLoop()
	return s, true, nil
}

// termEnv inherits the daemon env minus agent-monitor secrets and orchestrator markers.
func termEnv(env []string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		u := strings.ToUpper(k)
		switch {
		case k == "TERM", k == "AM_ORCH_THREAD", k == "AM_ORCH_DEPTH", k == "AM_HOST_ID", k == "AM_HOST_ID_ASSIGN":
			continue
		case strings.HasPrefix(u, "AGENT_MONITOR_") && (strings.Contains(u, "PASSWORD") || strings.Contains(u, "TOKEN") ||
			strings.Contains(u, "SECRET") || strings.Contains(u, "KEY") || strings.Contains(u, "VAPID")):
			continue
		}
		out = append(out, kv)
	}
	return append(out, "TERM=xterm-256color")
}

// Live reports the number of running shells.
func (m *termManager) Live() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

func (s *termSession) readLoop() {
	b := make([]byte, 32<<10)
	for {
		n, err := s.pty.Read(b)
		if n > 0 {
			chunk := append([]byte(nil), b[:n]...)
			s.mu.Lock()
			s.buf = append(s.buf, chunk...)
			if len(s.buf) > termScrollback {
				s.buf = append([]byte(nil), s.buf[len(s.buf)-termScrollback:]...)
			}
			for cl := range s.clients {
				cl.send(termFrame{binary: true, data: chunk})
			}
			s.mu.Unlock()
		}
		if err != nil {
			break
		}
	}
	code := 0
	if err := s.cmd.Wait(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	s.pty.Close()
	s.m.mu.Lock()
	if s.m.sessions[s.key] == s {
		delete(s.m.sessions, s.key)
	}
	s.m.mu.Unlock()
	msg, _ := json.Marshal(map[string]any{"type": "exit", "code": code})
	s.mu.Lock()
	s.exited = true
	s.timer.Stop()
	for cl := range s.clients {
		cl.send(termFrame{data: msg})
		cl.finish()
	}
	s.clients = map[*termClient]struct{}{}
	s.mu.Unlock()
}

// attach sends the hello and the scrollback before any live output, then registers the client.
func (s *termSession) attach(cl *termClient, created bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exited {
		return false
	}
	s.timer.Stop()
	hello, _ := json.Marshal(map[string]any{"type": "hello", "new": created, "cwd": s.cwd, "pid": s.cmd.Process.Pid,
		"cols": s.cols, "rows": s.rows, "scrollback": len(s.buf)})
	cl.send(termFrame{data: hello})
	if len(s.buf) > 0 {
		i := 0
		for i < len(s.buf) && i < 4 && s.buf[i]&0xC0 == 0x80 {
			i++
		}
		cl.send(termFrame{binary: true, data: append([]byte(nil), s.buf[i:]...)})
	}
	s.clients[cl] = struct{}{}
	return true
}

func (s *termSession) detach(cl *termClient) {
	cl.close()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.clients[cl]; !ok {
		return
	}
	delete(s.clients, cl)
	if len(s.clients) == 0 && !s.exited {
		s.timer.Reset(s.m.idle)
	}
}

func (s *termSession) input(b []byte) {
	if len(b) > 0 {
		_, _ = s.pty.Write(b)
	}
}

func (s *termSession) resize(cols, rows int) {
	if cols < 1 || rows < 1 || cols > 1000 || rows > 1000 {
		return
	}
	s.mu.Lock()
	s.cols, s.rows = cols, rows
	s.mu.Unlock()
	_ = pty.Setsize(s.pty, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

// kill ends the shell's process group: SIGHUP first, SIGKILL after 3 s.
func (s *termSession) kill() {
	s.mu.Lock()
	gone := s.exited
	s.mu.Unlock()
	if gone {
		return
	}
	pid := s.cmd.Process.Pid
	_ = syscall.Kill(-pid, syscall.SIGHUP)
	time.AfterFunc(3*time.Second, func() {
		s.mu.Lock()
		gone := s.exited
		s.mu.Unlock()
		if !gone {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
	})
}

// send queues a frame; a client that falls 512 frames behind is dropped.
func (cl *termClient) send(f termFrame) {
	select {
	case <-cl.done:
	case cl.out <- f:
	default:
		cl.close()
	}
}

// finish closes the client after its queued frames are written.
func (cl *termClient) finish() {
	select {
	case <-cl.done:
	case cl.out <- termFrame{data: nil}:
	default:
		cl.close()
	}
}

func (cl *termClient) close() {
	cl.once.Do(func() {
		close(cl.done)
		cl.conn.Close()
	})
}

func (cl *termClient) writeLoop() {
	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-cl.done:
			return
		case f := <-cl.out:
			if f.data == nil {
				_ = cl.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "shell exited"), time.Now().Add(time.Second))
				cl.close()
				return
			}
			mt := websocket.TextMessage
			if f.binary {
				mt = websocket.BinaryMessage
			}
			_ = cl.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if cl.conn.WriteMessage(mt, f.data) != nil {
				cl.close()
				return
			}
		case <-ping.C:
			if cl.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)) != nil {
				cl.close()
				return
			}
		}
	}
}
