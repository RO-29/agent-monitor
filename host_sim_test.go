// A Go session host, kept only as a test fixture for the daemon's host registry; am-helper is the real host.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// The session host runs an agent on a pty, relays the user's terminal to it, and serves a control socket.

const (
	hostRingSize     = 256 << 10
	hostScreenLines  = 50
	hostMaxLineBytes = 1 << 20
)

var (
	hostKeyGap        = 25 * time.Millisecond
	hostRegisterEvery = 10 * time.Second
)

// ringBuf keeps the newest max bytes of agent output.
type ringBuf struct {
	mu    sync.Mutex
	buf   []byte
	max   int
	total int64
	last  time.Time
}

func newRingBuf(max int) *ringBuf { return &ringBuf{max: max} }

func (r *ringBuf) Write(p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.max {
		r.buf = append([]byte(nil), r.buf[len(r.buf)-r.max:]...)
	}
	r.total += int64(len(p))
	r.last = time.Now()
}

func (r *ringBuf) Snapshot() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]byte(nil), r.buf...)
}

func (r *ringBuf) Stats() (int64, time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.total, r.last
}

type hostOptions struct {
	Argv     []string
	Env      []string
	HostID   string
	SockDir  string
	In       io.Reader
	Out      io.Writer
	TTY      *os.File // the user's terminal: raw mode and size; nil when there is none
	Signals  bool     // install process signal handlers; tests drive winch by hand
	Register func(hostRegistration) error
	Remove   func(hostID string)
	Cwd      string
	TtyName  string
	TmuxPane string
	// Leader is the pty session leader command (agent-monitor __pty-leader); nil runs the agent directly.
	Leader []string
	// Suspend stops the host for job control; nil sends SIGTSTP to itself when a shell can resume it.
	Suspend func()
}

type sessionHost struct {
	opt      hostOptions
	cmd      *exec.Cmd
	ptmx     *os.File
	ring     *ringBuf
	wmu      sync.Mutex
	ln       net.Listener
	sock     string
	restore  func()
	rawMu    sync.Mutex
	agentPid atomic.Int64
	winch    chan os.Signal
	exited   chan struct{}
}

func newSessionHost(opt hostOptions) *sessionHost {
	if opt.HostID == "" {
		opt.HostID = newHostID()
	}
	if opt.SockDir == "" {
		opt.SockDir = filepath.Join(stateDir(), "hosts")
	}
	return &sessionHost{opt: opt, ring: newRingBuf(hostRingSize), winch: make(chan os.Signal, 4), exited: make(chan struct{}), restore: func() {}}
}

// Run starts the agent and relays until it exits; it returns the agent's exit code.
func (h *sessionHost) Run() (code int, err error) {
	defer func() {
		if r := recover(); r != nil {
			h.cooked()
			panic(r)
		}
	}()
	bin, err := exec.LookPath(h.opt.Argv[0])
	if err != nil {
		return 127, fmt.Errorf("%s not found in PATH (%v)", h.opt.Argv[0], err)
	}
	var pidR *os.File
	if len(h.opt.Leader) > 0 {
		args := append(append(append([]string{}, h.opt.Leader[1:]...), "--", bin), h.opt.Argv[1:]...)
		h.cmd = exec.Command(h.opt.Leader[0], args...)
		var pidW *os.File
		if pidR, pidW, err = os.Pipe(); err != nil {
			return 126, err
		}
		h.cmd.ExtraFiles = []*os.File{pidW}
		defer pidW.Close()
	} else {
		h.cmd = exec.Command(bin, h.opt.Argv[1:]...)
	}
	h.cmd.Env = append(hostChildEnv(h.opt.Env), "AM_HOST_ID="+h.opt.HostID)
	if h.opt.Cwd != "" {
		h.cmd.Dir = h.opt.Cwd
	}
	size := &pty.Winsize{Cols: 80, Rows: 24}
	if h.opt.TTY != nil {
		if ws, err := pty.GetsizeFull(h.opt.TTY); err == nil && ws.Cols > 0 && ws.Rows > 0 {
			size = ws
		}
	}
	h.ptmx, err = pty.StartWithSize(h.cmd, size)
	if err != nil {
		return 126, err
	}
	h.agentPid.Store(int64(h.cmd.Process.Pid))
	if pidR != nil {
		for _, f := range h.cmd.ExtraFiles {
			_ = f.Close()
		}
		h.readAgentPid(pidR)
	}
	h.raw()
	defer h.cooked()
	if err := h.listen(); err != nil {
		fmt.Fprintf(os.Stderr, "agent-monitor run: control socket: %v\r\n", err)
	}
	defer h.closeSocket()

	outDone := make(chan struct{})
	h.goSafe(func() { h.relayOut(outDone) })
	h.goSafe(h.relayIn)
	h.goSafe(h.watchResize)
	stopSig := make(chan struct{})
	if h.opt.Signals {
		signal.Notify(h.winch, syscall.SIGWINCH)
		sigs := make(chan os.Signal, 4)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
		defer signal.Stop(sigs)
		defer signal.Stop(h.winch)
		h.goSafe(func() { h.forwardSignals(sigs, stopSig) })
	}
	defer close(stopSig)
	if h.opt.Register != nil {
		h.goSafe(h.registerLoop)
	}

	code = h.waitAgent()
	close(h.exited)
	// Grandchildren can hold the pty open, so the drain wait is bounded.
	select {
	case <-outDone:
	case <-time.After(500 * time.Millisecond):
	}
	_ = h.ptmx.Close()
	if h.opt.Remove != nil {
		h.opt.Remove(h.opt.HostID)
	}
	return code, nil
}

// readAgentPid takes the agent's pid from the leader; it waits at most 2 s.
func (h *sessionHost) readAgentPid(r *os.File) {
	got := make(chan int, 1)
	go func() {
		defer r.Close()
		var pid int
		if _, err := fmt.Fscan(r, &pid); err == nil && pid > 0 {
			got <- pid
		}
		close(got)
	}()
	select {
	case pid, ok := <-got:
		if ok {
			h.agentPid.Store(int64(pid))
		}
	case <-time.After(2 * time.Second):
		_ = r.Close()
	}
}

func (h *sessionHost) raw() {
	h.rawMu.Lock()
	defer h.rawMu.Unlock()
	if h.opt.TTY == nil {
		return
	}
	if restore, err := makeRaw(h.opt.TTY); err == nil {
		h.restore = restore
	}
}

func (h *sessionHost) cooked() {
	h.rawMu.Lock()
	defer h.rawMu.Unlock()
	h.restore()
	h.restore = func() {}
}

// waitAgent waits for the child to exit; a stop (the agent's Ctrl-Z) suspends the host as a shell job.
func (h *sessionHost) waitAgent() int {
	pid := h.cmd.Process.Pid
	for {
		var ws syscall.WaitStatus
		_, err := syscall.Wait4(pid, &ws, syscall.WUNTRACED, nil)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return 1
		}
		switch {
		case waitStopped(ws):
			h.suspend()
		case ws.Signaled():
			return 128 + int(ws.Signal())
		case ws.Exited():
			return ws.ExitStatus()
		}
	}
}

// suspend gives the terminal back to the shell, stops the host, and on resume restores raw mode, size and the agent.
func (h *sessionHost) suspend() {
	if h.opt.TTY != nil && (h.opt.Suspend != nil || shellJobControl()) {
		h.cooked()
		if h.opt.Suspend != nil {
			h.opt.Suspend()
		} else {
			_ = syscall.Kill(os.Getpid(), syscall.SIGTSTP)
		}
		h.raw()
		if ws, err := pty.GetsizeFull(h.opt.TTY); err == nil {
			_ = ptySetSize(h.ptmx, ws.Cols, ws.Rows)
		}
	}
	// The leader continues the agent's process group itself.
	if len(h.opt.Leader) > 0 {
		_ = syscall.Kill(h.cmd.Process.Pid, syscall.SIGCONT)
	} else {
		h.signalAgent(syscall.SIGCONT)
	}
}

// shellJobControl is false when the host leads its own session (tmux ran it with exec): no shell could resume it.
func shellJobControl() bool {
	sid, err := unix.Getsid(0)
	return err == nil && sid != unix.Getpgrp()
}

// goSafe restores the terminal before a goroutine panic kills the process.
func (h *sessionHost) goSafe(fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				h.cooked()
				panic(r)
			}
		}()
		fn()
	}()
}

// hostChildEnv drops host markers inherited from an outer host.
func hostChildEnv(env []string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if strings.HasPrefix(kv, "AM_HOST_ID=") || strings.HasPrefix(kv, "AM_HOST_ID_ASSIGN=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func (h *sessionHost) write(p []byte) error {
	h.wmu.Lock()
	defer h.wmu.Unlock()
	_, err := h.ptmx.Write(p)
	return err
}

func (h *sessionHost) relayIn() {
	if h.opt.In == nil {
		return
	}
	b := make([]byte, 32<<10)
	for {
		n, err := h.opt.In.Read(b)
		if n > 0 {
			if h.write(b[:n]) != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (h *sessionHost) relayOut(done chan struct{}) {
	defer close(done)
	b := make([]byte, 32<<10)
	for {
		n, err := h.ptmx.Read(b)
		if n > 0 {
			h.ring.Write(b[:n])
			if h.opt.Out != nil {
				// A dead terminal must not stall the agent, so write errors are ignored.
				_, _ = h.opt.Out.Write(b[:n])
			}
		}
		if err != nil {
			return
		}
	}
}

func (h *sessionHost) watchResize() {
	for {
		select {
		case <-h.exited:
			return
		case <-h.winch:
			if h.opt.TTY == nil {
				continue
			}
			if ws, err := pty.GetsizeFull(h.opt.TTY); err == nil {
				_ = ptySetSize(h.ptmx, ws.Cols, ws.Rows)
			}
		}
	}
}

// forwardSignals passes INT/TERM/HUP to the agent's process group; a hung-up agent is killed after 10 s.
func (h *sessionHost) forwardSignals(sigs chan os.Signal, stop chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case s := <-sigs:
			sig := s.(syscall.Signal)
			h.signalAgent(sig)
			if sig == syscall.SIGHUP {
				go func() {
					select {
					case <-h.exited:
					case <-time.After(10 * time.Second):
						h.signalAgent(syscall.SIGKILL)
					}
				}()
			}
		}
	}
}

// signalAgent signals the agent's process group; under a leader a kill also takes the leader.
func (h *sessionHost) signalAgent(sig syscall.Signal) {
	if h.cmd == nil || h.cmd.Process == nil {
		return
	}
	pid := int(h.agentPid.Load())
	if syscall.Kill(-pid, sig) != nil {
		_ = syscall.Kill(pid, sig)
	}
	if lp := h.cmd.Process.Pid; lp != pid && sig == syscall.SIGKILL {
		_ = syscall.Kill(lp, sig)
	}
}

func (h *sessionHost) registration() hostRegistration {
	agentPid := int(h.agentPid.Load())
	return hostRegistration{
		HostID: h.opt.HostID, Pid: os.Getpid(), AgentPid: agentPid, Agent: filepath.Base(h.opt.Argv[0]),
		Cwd: h.opt.Cwd, Tty: h.opt.TtyName, TmuxPane: h.opt.TmuxPane, Socket: h.sock,
	}
}

// registerLoop retries quickly at start, then re-registers on a heartbeat so a restarted daemon learns the host.
func (h *sessionHost) registerLoop() {
	delay := 500 * time.Millisecond
	for {
		err := h.opt.Register(h.registration())
		wait := hostRegisterEvery
		if err != nil && delay < hostRegisterEvery {
			wait = delay
			delay *= 2
		}
		select {
		case <-h.exited:
			return
		case <-time.After(wait):
		}
	}
}

// ── control socket ──

func (h *sessionHost) listen() error {
	if err := os.MkdirAll(h.opt.SockDir, 0o700); err != nil {
		return err
	}
	_ = os.Chmod(h.opt.SockDir, 0o700)
	h.sock = filepath.Join(h.opt.SockDir, h.opt.HostID+".sock")
	_ = os.Remove(h.sock)
	ln, err := net.Listen("unix", h.sock)
	if err != nil {
		h.sock = ""
		return err
	}
	_ = os.Chmod(h.sock, 0o600)
	h.ln = ln
	h.goSafe(func() {
		// A transient accept error (EMFILE, ECONNABORTED) backs off; only a closed listener ends the loop.
		delay := 5 * time.Millisecond
		for {
			c, err := ln.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					return
				}
				time.Sleep(delay)
				delay = min(delay*2, time.Second)
				continue
			}
			delay = 5 * time.Millisecond
			h.goSafe(func() { h.serveConn(c) })
		}
	})
	return nil
}

func (h *sessionHost) closeSocket() {
	if h.ln != nil {
		_ = h.ln.Close()
	}
	if h.sock != "" {
		_ = os.Remove(h.sock)
	}
}

func (h *sessionHost) serveConn(c net.Conn) {
	defer c.Close()
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 64<<10), hostMaxLineBytes)
	enc := json.NewEncoder(c)
	for {
		_ = c.SetDeadline(time.Now().Add(30 * time.Second))
		if !sc.Scan() {
			return
		}
		var req hostRequest
		var resp hostResponse
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			resp = hostResponse{Error: "bad json"}
		} else {
			resp = h.handle(req)
		}
		if enc.Encode(resp) != nil {
			return
		}
	}
}

func (h *sessionHost) handle(req hostRequest) hostResponse {
	select {
	case <-h.exited:
		return hostResponse{Error: "agent exited"}
	default:
	}
	switch req.Op {
	case "ping":
		total, last := h.ring.Stats()
		r := h.registration()
		idle := int64(0)
		if !last.IsZero() {
			idle = time.Since(last).Milliseconds()
		}
		return hostResponse{OK: true, HostID: r.HostID, Agent: r.Agent, AgentPid: r.AgentPid, OutBytes: total, IdleMs: idle}
	case "input":
		if req.Data == "" {
			return hostResponse{OK: true}
		}
		if err := h.write([]byte(req.Data)); err != nil {
			return hostResponse{Error: err.Error()}
		}
		return hostResponse{OK: true}
	case "keys":
		seqs, err := hostKeyBytes(req.Keys)
		if err != nil {
			return hostResponse{Error: err.Error()}
		}
		for i, s := range seqs {
			if i > 0 && hostKeyGap > 0 {
				time.Sleep(hostKeyGap)
			}
			if err := h.write(s); err != nil {
				return hostResponse{Error: err.Error()}
			}
		}
		return hostResponse{OK: true}
	case "screen":
		cols, rows := h.size()
		n := req.Lines
		if n <= 0 {
			n = hostScreenLines
		}
		if n > 2000 {
			n = 2000
		}
		return hostResponse{OK: true, Text: screenText(h.ring.Snapshot(), cols, rows, n), Cols: cols, Rows: rows}
	case "size":
		cols, rows := h.size()
		return hostResponse{OK: true, Cols: cols, Rows: rows}
	}
	return hostResponse{Error: "unknown op " + req.Op}
}

func (h *sessionHost) size() (int, int) {
	if cols, rows, err := ptyGetSize(h.ptmx); err == nil && cols > 0 {
		return cols, rows
	}
	return 80, 24
}

// ptySetSize and ptyGetSize go through SyscallConn: File.Fd races with Close and makes the fd blocking.
func ptySetSize(f *os.File, cols, rows uint16) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ierr error
	if err := rc.Control(func(fd uintptr) {
		ierr = unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, &unix.Winsize{Col: cols, Row: rows})
	}); err != nil {
		return err
	}
	return ierr
}

func ptyGetSize(f *os.File) (int, int, error) {
	rc, err := f.SyscallConn()
	if err != nil {
		return 0, 0, err
	}
	var ws *unix.Winsize
	var ierr error
	if err := rc.Control(func(fd uintptr) { ws, ierr = unix.IoctlGetWinsize(int(fd), unix.TIOCGWINSZ) }); err != nil {
		return 0, 0, err
	}
	if ierr != nil {
		return 0, 0, ierr
	}
	return int(ws.Col), int(ws.Row), nil
}

// hostKeyBytes maps tmux-style key names to the bytes a terminal sends; any unknown name fails the whole list.
func hostKeyBytes(keys []string) ([][]byte, error) {
	if len(keys) == 0 {
		return nil, errors.New("keys required")
	}
	named := map[string]string{
		"enter": "\r", "cr": "\r", "escape": "\x1b", "esc": "\x1b", "tab": "\t", "btab": "\x1b[Z",
		"up": "\x1b[A", "down": "\x1b[B", "right": "\x1b[C", "left": "\x1b[D",
		"bspace": "\x7f", "backspace": "\x7f", "space": " ", "home": "\x1b[H", "end": "\x1b[F",
		"pageup": "\x1b[5~", "ppage": "\x1b[5~", "pagedown": "\x1b[6~", "npage": "\x1b[6~", "dc": "\x1b[3~", "delete": "\x1b[3~",
	}
	out := make([][]byte, 0, len(keys))
	for _, k := range keys {
		if s, ok := named[strings.ToLower(k)]; ok {
			out = append(out, []byte(s))
			continue
		}
		if len(k) == 3 && (k[0] == 'C' || k[0] == 'c') && k[1] == '-' {
			c := k[2] | 0x20
			if c >= 'a' && c <= 'z' {
				out = append(out, []byte{c - 'a' + 1})
				continue
			}
		}
		if len([]rune(k)) == 1 {
			out = append(out, []byte(k))
			continue
		}
		return nil, fmt.Errorf("unknown key %q", k)
	}
	return out, nil
}

// ── CLI entry ──

// runHostMain runs `agent-monitor run` as a host on the current terminal and returns the exit code.
func runHostMain(argv []string, tmuxPane string) int {
	cwd, _ := os.Getwd()
	id := os.Getenv("AM_HOST_ID_ASSIGN")
	if !hostIDPattern.MatchString(id) {
		id = ""
	}
	var leader []string
	if exe, err := os.Executable(); err == nil {
		leader = []string{helperFor(exe), "__pty-leader"}
	}
	h := newSessionHost(hostOptions{
		Argv: argv, Env: os.Environ(), HostID: id, In: os.Stdin, Out: os.Stdout, TTY: os.Stdin, Signals: true, Leader: leader,
		Register: postHostRegistration, Remove: deleteHostRegistration, Cwd: cwd, TtyName: ttyName(), TmuxPane: tmuxPane,
	})
	code, err := h.Run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-monitor run: %v\n", err)
	}
	return code
}

func ttyName() string {
	cmd := exec.Command("tty")
	cmd.Stdin = os.Stdin
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

var hostHTTP = &http.Client{Timeout: time.Second}

func postHostRegistration(reg hostRegistration) error {
	body, _ := json.Marshal(reg)
	resp, err := hostHTTP.Post(daemonURL()+"/api/hosts", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("register: %s", resp.Status)
	}
	return nil
}

func deleteHostRegistration(id string) {
	req, _ := http.NewRequest(http.MethodDelete, daemonURL()+"/api/hosts/"+id, nil)
	c := &http.Client{Timeout: 300 * time.Millisecond}
	if resp, err := c.Do(req); err == nil {
		resp.Body.Close()
	}
}
