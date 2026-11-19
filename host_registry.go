package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// HostInfo is one live session host as the daemon knows it.
type HostInfo struct {
	HostID       string `json:"hostId"`
	Pid          int    `json:"pid"`
	AgentPid     int    `json:"agentPid"`
	Agent        string `json:"agent"`
	Cwd          string `json:"cwd"`
	Tty          string `json:"tty,omitempty"`
	TmuxPane     string `json:"tmuxPane,omitempty"`
	Socket       string `json:"socket,omitempty"`
	Tool         Tool   `json:"tool,omitempty"`
	SessionID    string `json:"sessionId,omitempty"`
	AgentID      string `json:"agentId,omitempty"`
	RegisteredAt int64  `json:"registeredAt"`
	LastSeenAt   int64  `json:"lastSeenAt"`
	// Set on a copy while the host has no session yet; never persisted.
	Prompt     *hostPrompt `json:"prompt,omitempty"`
	ScreenTail []string    `json:"screenTail,omitempty"`
}

type hostLink struct {
	Tool      Tool
	SessionID string
}

type HostRegistry struct {
	mu      sync.Mutex
	hosts   map[string]*HostInfo
	pending map[string]hostLink // links that arrived before their host registered
	path    string
}

var hostRegistry *HostRegistry

// procParentComm returns a pid's parent and command base name; tests swap it.
var procParentComm = func(pid int) (int, string, bool) {
	out, err := exec.Command("ps", "-o", "ppid=,comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, "", false
	}
	f := strings.Fields(strings.TrimSpace(string(out)))
	if len(f) < 2 {
		return 0, "", false
	}
	ppid, err := strconv.Atoi(f[0])
	if err != nil {
		return 0, "", false
	}
	return ppid, filepath.Base(strings.Join(f[1:], " ")), true
}

func NewHostRegistry(path string) *HostRegistry {
	hr := &HostRegistry{hosts: map[string]*HostInfo{}, pending: map[string]hostLink{}, path: path}
	if body, err := os.ReadFile(path); err == nil {
		var saved []*HostInfo
		if json.Unmarshal(body, &saved) == nil {
			for _, h := range saved {
				if h != nil && hostIDPattern.MatchString(h.HostID) && pidAlive(h.Pid) {
					hr.hosts[h.HostID] = h
				}
			}
		}
	}
	return hr
}

func (hr *HostRegistry) persistLocked() {
	if hr.path == "" {
		return
	}
	list := make([]*HostInfo, 0, len(hr.hosts))
	for _, h := range hr.hosts {
		list = append(list, h)
	}
	body, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return
	}
	tmp := hr.path + ".tmp"
	if os.WriteFile(tmp, body, 0o600) == nil {
		_ = os.Rename(tmp, hr.path)
	}
}

// validHostSocket accepts only <dir>/<hostId>.sock that is a unix socket owned by this user.
func validHostSocket(id, sock string) bool {
	if !filepath.IsAbs(sock) || filepath.Base(sock) != id+".sock" {
		return false
	}
	st, err := os.Lstat(sock)
	if err != nil || st.Mode()&os.ModeSocket == 0 {
		return false
	}
	return fileOwnedBySelf(st)
}

func (hr *HostRegistry) Register(in hostRegistration) (*HostInfo, error) {
	if !hostIDPattern.MatchString(in.HostID) {
		return nil, errors.New("bad hostId")
	}
	if in.Pid <= 0 {
		return nil, errors.New("pid required")
	}
	if !validHostSocket(in.HostID, in.Socket) {
		return nil, errors.New("socket must be <dir>/<hostId>.sock, a unix socket owned by this user")
	}
	now := time.Now().UnixMilli()
	hr.mu.Lock()
	defer hr.mu.Unlock()
	h := hr.hosts[in.HostID]
	if h == nil {
		h = &HostInfo{HostID: in.HostID, RegisteredAt: now}
		hr.hosts[in.HostID] = h
	}
	h.Pid, h.AgentPid, h.Agent, h.Cwd, h.Tty, h.TmuxPane, h.Socket = in.Pid, in.AgentPid, in.Agent, in.Cwd, in.Tty, in.TmuxPane, in.Socket
	if h.Tool == "" {
		h.Tool = toolFromAgentName(in.Agent)
	}
	h.LastSeenAt = now
	if l, ok := hr.pending[in.HostID]; ok {
		delete(hr.pending, in.HostID)
		hr.linkLocked(h, l.Tool, l.SessionID)
	}
	hr.persistLocked()
	c := *h
	return &c, nil
}

func (hr *HostRegistry) Remove(id string) {
	hr.mu.Lock()
	defer hr.mu.Unlock()
	delete(hr.hosts, id)
	delete(hr.pending, id)
	hr.persistLocked()
}

// aliveLocked drops a host whose process is gone.
func (hr *HostRegistry) aliveLocked(h *HostInfo) bool {
	if pidAlive(h.Pid) && (h.AgentPid <= 0 || pidAlive(h.AgentPid)) {
		return true
	}
	delete(hr.hosts, h.HostID)
	hr.persistLocked()
	return false
}

func (hr *HostRegistry) Get(id string) *HostInfo {
	hr.mu.Lock()
	defer hr.mu.Unlock()
	h := hr.hosts[id]
	if h == nil || !hr.aliveLocked(h) {
		return nil
	}
	c := *h
	return &c
}

func (hr *HostRegistry) List() []HostInfo {
	hr.mu.Lock()
	defer hr.mu.Unlock()
	out := make([]HostInfo, 0, len(hr.hosts))
	for _, h := range hr.hosts {
		if hr.aliveLocked(h) {
			out = append(out, *h)
		}
	}
	return out
}

// ForSession returns the live host whose current session is agentID (any tool prefix).
func (hr *HostRegistry) ForSession(agentID string) *HostInfo {
	_, raw := splitAgentID(agentID)
	if raw == "" {
		return nil
	}
	hr.mu.Lock()
	defer hr.mu.Unlock()
	var best *HostInfo
	for _, h := range hr.hosts {
		if h.SessionID == raw && (best == nil || h.LastSeenAt > best.LastSeenAt) {
			best = h
		}
	}
	if best == nil || !hr.aliveLocked(best) {
		return nil
	}
	c := *best
	return &c
}

func (hr *HostRegistry) linkLocked(h *HostInfo, tool Tool, sessionID string) {
	h.Tool, h.SessionID, h.AgentID = tool, sessionID, string(tool)+":"+sessionID
	if o, ok := hostOrigins.Load(h.HostID); ok && store != nil {
		go store.SetOrigin(tool, sessionID, o.(string))
	}
	for _, o := range hr.hosts {
		if o != h && o.SessionID == sessionID {
			o.SessionID, o.AgentID = "", ""
		}
	}
	c := *h
	c.Socket = ""
	go publishV3("host", "host", c)
}

// LinkFromHook links a hook's session to the host named by AM_HOST_ID. ppid is the process that ran
// the hook; a nested agent that inherited AM_HOST_ID is refused.
func (hr *HostRegistry) LinkFromHook(hostID string, tool Tool, sessionID string, ppid int) bool {
	if !hostIDPattern.MatchString(hostID) || sessionID == "" {
		return false
	}
	tool = canonicalPaneTool(tool, sessionID)
	hr.mu.Lock()
	h := hr.hosts[hostID]
	if h == nil {
		hr.pending[hostID] = hostLink{Tool: tool, SessionID: sessionID}
		hr.mu.Unlock()
		return false
	}
	agentPid, current := h.AgentPid, h.SessionID
	hr.mu.Unlock()
	if current != sessionID {
		if ppid > 0 && agentPid > 0 {
			if !hookFromAgent(agentPid, ppid) {
				return false
			}
		} else if current != "" {
			return false
		}
	}
	hr.mu.Lock()
	defer hr.mu.Unlock()
	if h = hr.hosts[hostID]; h == nil {
		return false
	}
	h.LastSeenAt = time.Now().UnixMilli()
	if h.SessionID != sessionID || h.Tool != tool {
		hr.linkLocked(h, tool, sessionID)
		hr.persistLocked()
	}
	return true
}

// hookFromAgent is true when agentPid is within four parents of ppid and no process between them runs the agent's program.
func hookFromAgent(agentPid, ppid int) bool {
	_, agentComm, _ := procParentComm(agentPid)
	p := ppid
	for i := 0; i < 5; i++ {
		if p == agentPid {
			return true
		}
		if p <= 1 {
			return false
		}
		parent, comm, ok := procParentComm(p)
		if !ok {
			return false
		}
		if agentComm != "" && comm == agentComm {
			return false
		}
		p = parent
	}
	return false
}

// ── host client ──

var (
	errHostUnreachable = errors.New("session host not reachable")
	hostDialTimeout    = 2 * time.Second
	hostEnterDelay     = 60 * time.Millisecond
)

func hostCall(h *HostInfo, req hostRequest) (hostResponse, error) {
	return hostCallWithin(h, req, hostDialTimeout, 10*time.Second)
}

// hostCallWithin is hostCall with its own dial timeout and total deadline.
func hostCallWithin(h *HostInfo, req hostRequest, dial, total time.Duration) (hostResponse, error) {
	var resp hostResponse
	if h == nil || h.Socket == "" {
		return resp, errHostUnreachable
	}
	c, err := net.DialTimeout("unix", h.Socket, dial)
	if err != nil {
		return resp, fmt.Errorf("%w: %v", errHostUnreachable, err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(total))
	body, _ := json.Marshal(req)
	if _, err := c.Write(append(body, '\n')); err != nil {
		return resp, fmt.Errorf("%w: %v", errHostUnreachable, err)
	}
	r := bufio.NewReaderSize(c, 64<<10)
	line, err := r.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return resp, err
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return resp, err
	}
	if !resp.OK {
		return resp, errors.New(resp.Error)
	}
	return resp, nil
}

func hostSendText(h *HostInfo, text string, enter bool) error {
	paste := asPaste(text)
	if paste && h.Agent == "claude" {
		paste = false
		err := typeAsKeys(text, func(t string) error { _, err := hostCall(h, hostRequest{Op: "input", Data: t}); return err },
			func(k []string) error { return hostSendKeys(h, k) })
		if err != nil {
			return err
		}
		text = ""
		time.Sleep(hostEnterDelay)
	}
	if paste {
		text = "\x1b[200~" + text + "\x1b[201~" // one bracketed paste, as a terminal sends it
	}
	if text != "" {
		if _, err := hostCall(h, hostRequest{Op: "input", Data: text}); err != nil {
			return err
		}
	}
	if enter {
		if text != "" {
			time.Sleep(hostEnterDelay)
		}
		if paste {
			time.Sleep(pasteSettle)
		}
		_, err := hostCall(h, hostRequest{Op: "keys", Keys: []string{"Enter"}})
		return err
	}
	return nil
}

func hostSendKeys(h *HostInfo, keys []string) error {
	_, err := hostCall(h, hostRequest{Op: "keys", Keys: keys})
	return err
}

// hostScreen returns the last lines of the host's rendered output; 0 means the visible rows.
func hostScreen(h *HostInfo, lines int) (string, error) {
	if lines <= 0 {
		if r, err := hostCall(h, hostRequest{Op: "size"}); err == nil && r.Rows > 0 {
			lines = r.Rows
		}
	}
	r, err := hostCall(h, hostRequest{Op: "screen", Lines: lines})
	if err != nil {
		return "", err
	}
	return r.Text, nil
}

// ── terminal targets: a host first, then a tmux pane ──

const hostTargetPrefix = "host:"

var (
	tmuxSendTextFn = SendToTmuxPane
	tmuxSendKeysFn = SendKeysToTmuxPane
	tmuxCaptureFn  = CapturePane
)

func hostForSession(agentID string) *HostInfo {
	if hostRegistry == nil || agentID == "" {
		return nil
	}
	return hostRegistry.ForSession(agentID)
}

func targetHost(target string) (*HostInfo, bool, error) {
	id, ok := strings.CutPrefix(target, hostTargetPrefix)
	if !ok {
		return nil, false, nil
	}
	if hostRegistry == nil {
		return nil, true, errHostUnreachable
	}
	h := hostRegistry.Get(id)
	if h == nil {
		return nil, true, errHostUnreachable
	}
	return h, true, nil
}

// targetText types text into a target ("host:<id>" or a tmux pane id), then Enter when asked.
func targetText(target, text string, enter bool) error {
	if h, isHost, err := targetHost(target); isHost {
		if err != nil {
			return err
		}
		return hostSendText(h, text, enter)
	}
	return tmuxSendTextFn(target, text, enter)
}

func targetKeys(target string, keys []string) error {
	if h, isHost, err := targetHost(target); isHost {
		if err != nil {
			return err
		}
		return hostSendKeys(h, keys)
	}
	return tmuxSendKeysFn(target, keys)
}

// targetCapture reads the screen; a host always answers plain text.
func targetCapture(target string, lines int, plain bool) (string, error) {
	if h, isHost, err := targetHost(target); isHost {
		if err != nil {
			return "", err
		}
		// The helper's screen copy handles a subset of escapes and can lose rows; tmux, when the host runs in it, does not.
		if h.TmuxPane != "" {
			if s, err := tmuxCaptureFn(h.TmuxPane, lines, plain); err == nil && strings.TrimSpace(s) != "" {
				return s, nil
			}
		}
		return hostScreen(h, lines)
	}
	return tmuxCaptureFn(target, lines, plain)
}

// termRoute is how a session's terminal is reached.
type termRoute struct {
	Target      string
	HostID      string
	PaneID      string
	SessionName string
	Command     string
	Alive       bool // the agent still runs in the pane; only routes that type refuse a dead one
}

// resolveTmuxRoute finds the registered live tmux pane of a session.
func resolveTmuxRoute(agentID string) (*termRoute, int, error) {
	var reg *PaneRegistration
	if paneRegistry != nil {
		reg = paneRegistry.Get(agentID)
	}
	if reg == nil {
		return nil, http.StatusNotFound, errors.New("no session host or tmux pane registered: launch the agent via `agent-monitor run` or the SessionStart hook")
	}
	panes, err := listPanesFn()
	if err != nil {
		return nil, http.StatusServiceUnavailable, err
	}
	for _, p := range panes {
		if p.PaneID == reg.PaneID {
			// The same rule as WatchedPane.Sendable: typing into a pane whose agent ended runs the text in the shell.
			alive := p.CurrentCommand != "" && !shellCommands[p.CurrentCommand]
			if reg.Pid > 0 {
				alive = pidAlive(reg.Pid)
			}
			paneRegistry.Touch(agentID)
			return &termRoute{Target: p.PaneID, PaneID: p.PaneID, SessionName: p.SessionName, Command: p.CurrentCommand, Alive: alive}, 0, nil
		}
	}
	return nil, http.StatusGone, errors.New("registered pane no longer exists in tmux: re-launch the agent or DELETE /api/pane/registration to forget")
}

// withTerminal runs fn on the session's host, or on its tmux pane when it has no host or the host is unreachable.
var errDeadPane = errors.New("the agent in this pane has ended; agent-monitor will not type into the shell")

// withTerminal runs fn on a session's terminal to type into it; a pane whose agent ended is refused.
func withTerminal(agentID string, fn func(target string) error) (*termRoute, int, error) {
	return withTerminalAt(agentID, true, fn)
}

// readTerminal runs fn to read the terminal, also after its agent ended.
func readTerminal(agentID string, fn func(target string) error) (*termRoute, int, error) {
	return withTerminalAt(agentID, false, fn)
}

func withTerminalAt(agentID string, write bool, fn func(target string) error) (*termRoute, int, error) {
	if h := hostForSession(agentID); h != nil {
		rt := &termRoute{Target: hostTargetPrefix + h.HostID, HostID: h.HostID, PaneID: h.TmuxPane, Command: h.Agent}
		err := fn(rt.Target)
		if err == nil {
			return rt, 0, nil
		}
		if !errors.Is(err, errHostUnreachable) {
			return rt, http.StatusBadGateway, err
		}
		if prt, _, perr := resolveTmuxRoute(agentID); perr == nil && (prt.Alive || !write) {
			if err := fn(prt.Target); err != nil {
				return prt, http.StatusInternalServerError, err
			}
			return prt, 0, nil
		}
		return rt, http.StatusBadGateway, err
	}
	rt, code, err := resolveTmuxRoute(agentID)
	if err != nil {
		return nil, code, err
	}
	if write && !rt.Alive {
		return rt, statusRefused, errDeadPane
	}
	if err := fn(rt.Target); err != nil {
		return rt, http.StatusInternalServerError, err
	}
	return rt, 0, nil
}

// ── routes ──

func registerHostRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/hosts", func(w http.ResponseWriter, r *http.Request) {
		list := []HostInfo{}
		if hostRegistry != nil {
			list = hostRegistry.List()
			pruneHostPromptState(list)
		}
		withStartStates(list)
		local := localCaller(r)
		for i := range list {
			if !local {
				list[i].Socket = ""
			}
		}
		writeJSON(w, map[string]any{"hosts": list})
	})
	mux.HandleFunc("GET /api/hosts/{id}", func(w http.ResponseWriter, r *http.Request) {
		var h *HostInfo
		if hostRegistry != nil {
			h = hostRegistry.Get(r.PathValue("id"))
		}
		if h == nil {
			writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "host not found"})
			return
		}
		withStartState(h)
		if !localCaller(r) {
			h.Socket = ""
		}
		writeJSON(w, h)
	})
	mux.HandleFunc("POST /api/hosts/{id}/answer", serveHostAnswer)
	mux.HandleFunc("POST /api/hosts", func(w http.ResponseWriter, r *http.Request) {
		if !localCaller(r) || hostRegistry == nil {
			writeJSONStatus(w, http.StatusForbidden, map[string]any{"error": "hosts register from this machine only"})
			return
		}
		var in hostRegistration
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		h, err := hostRegistry.Register(in)
		if err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "hostId": h.HostID})
	})
	mux.HandleFunc("DELETE /api/hosts/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !localCaller(r) || hostRegistry == nil {
			writeJSONStatus(w, http.StatusForbidden, map[string]any{"error": "local callers only"})
			return
		}
		hostRegistry.Remove(r.PathValue("id"))
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/orch/terminal", serveOrchTerminal)
	registerTargetRoutes(mux)
}

// tmuxNewSessionFn starts a detached tmux session; tests swap it.
var tmuxNewSessionFn = func(name, cwd, command string) error {
	out, err := exec.Command("tmux", "new-session", "-d", "-s", name, "-c", cwd, command).CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmux new-session: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

var (
	agentMonitorExe   = os.Executable
	hostReadyTimeout  = 90 * time.Second // Claude with many MCP servers can take a minute to start
	hostTypedWithin   = 5 * time.Second
	hostReadyPoll     = 250 * time.Millisecond
	hostReadyQuietFor = 800 * time.Millisecond
)

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// typeWhenReady waits for the host to register and its output to go quiet, then submits the prompt.
func typeWhenReady(hostID, prompt string) error {
	deadline := time.Now().Add(hostReadyTimeout)
	for time.Now().Before(deadline) {
		var h *HostInfo
		if hostRegistry != nil {
			h = hostRegistry.Get(hostID)
		}
		if h != nil {
			if p, err := hostCall(h, hostRequest{Op: "ping"}); err == nil && p.OutBytes > 0 && time.Duration(p.IdleMs)*time.Millisecond >= hostReadyQuietFor {
				if done, err := typeFirstPrompt(h, prompt); done || err != nil {
					return err
				}
			}
		}
		time.Sleep(hostReadyPoll)
	}
	return errors.New("the agent did not show its input box")
}

// typeFirstPrompt types once Claude draws its input box, and presses Enter only when the box shows the
// prompt; false means the box is not drawn yet. It never types twice: without proof it reports an error.
func typeFirstPrompt(h *HostInfo, prompt string) (bool, error) {
	box := func() (string, bool) {
		scr, err := hostScreen(h, 0)
		return cliInputBox(scr), err == nil
	}
	if h.Agent == "claude" {
		if b, ok := box(); !ok || b == "" {
			return false, nil
		}
	}
	if asPaste(prompt) && h.Agent == "claude" {
		if err := typeAsKeys(prompt, func(t string) error { _, err := hostCall(h, hostRequest{Op: "input", Data: t}); return err },
			func(k []string) error { return hostSendKeys(h, k) }); err != nil {
			return false, err
		}
	} else {
		data := prompt
		if asPaste(prompt) {
			data = "\x1b[200~" + prompt + "\x1b[201~"
		}
		if _, err := hostCall(h, hostRequest{Op: "input", Data: data}); err != nil {
			return false, err
		}
	}
	if h.Agent == "claude" {
		landed := false
		for end := time.Now().Add(hostTypedWithin); !landed && time.Now().Before(end); {
			time.Sleep(hostReadyPoll)
			b, ok := box()
			landed = ok && promptInBox(b, prompt)
		}
		if !landed {
			return true, errors.New("the prompt was typed but the input box does not show it; check the terminal before sending it again")
		}
	}
	time.Sleep(hostEnterDelay)
	return true, hostSendKeys(h, []string{"Enter"})
}

// promptInBox is true when Claude's input box shows the start or the end of the prompt (a long prompt scrolls
// its start above "↑ N more"), or its marker for pasted text.
func promptInBox(box, prompt string) bool {
	if strings.Contains(box, "[Pasted text") {
		return true
	}
	want := strings.Join(strings.Fields(prompt), " ")
	got := strings.Join(strings.Fields(box), " ")
	if want == "" {
		return false
	}
	r := []rune(want)
	return strings.Contains(got, clipRunes(want, 16)) || strings.Contains(got, string(r[max(0, len(r)-16):]))
}

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		r = r[:n]
	}
	return string(r)
}

func fileOwnedBySelf(st os.FileInfo) bool {
	s, ok := st.Sys().(*syscall.Stat_t)
	return ok && int(s.Uid) == os.Getuid()
}
