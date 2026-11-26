package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ── JSON-RPC over stdio (newline-delimited, no "jsonrpc" field, as codex app-server speaks it) ──

type rpcMsg struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int64           `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("codex: %s (%d)", e.Message, e.Code) }

var (
	errCodexExited = errors.New("codex app-server exited")
	errOrchClosed  = errors.New("thread is closed")
)

type codexConn struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdoutR io.Closer
	wmu     sync.Mutex
	nextID  atomic.Int64
	pmu     sync.Mutex
	pending map[int64]chan rpcMsg
	done    chan struct{}
	exitErr error
	stderr  *tailBuffer
	// name labels exit messages; rpc2 adds "jsonrpc":"2.0" to every message (ACP).
	name string
	rpc2 bool
}

type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

// lastLines returns up to n trailing non-empty lines.
func (t *tailBuffer) lastLines(n int) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var keep []string
	lines := strings.Split(string(t.buf), "\n")
	for i := len(lines) - 1; i >= 0 && len(keep) < n; i-- {
		if s := strings.TrimSpace(lines[i]); s != "" {
			keep = append([]string{s}, keep...)
		}
	}
	return strings.Join(keep, "\n")
}

type codexHandlers struct {
	notify  func(c *codexConn, method string, params json.RawMessage)
	request func(c *codexConn, id json.RawMessage, method string, params json.RawMessage)
	exit    func(c *codexConn)
	trace   func(line []byte)
}

func startCodexConn(cmd *exec.Cmd, h codexHandlers) (*codexConn, error) {
	return startRPCConn(cmd, h, "codex app-server", false)
}

func startRPCConn(cmd *exec.Cmd, h codexHandlers, name string, rpc2 bool) (*codexConn, error) {
	c := &codexConn{cmd: cmd, pending: map[int64]chan rpcMsg{}, done: make(chan struct{}), stderr: &tailBuffer{max: 16 << 10}, name: name, rpc2: rpc2}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = c.stderr
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	// A grandchild that keeps stderr open must not keep Wait (and done) from returning.
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c.stdin, c.stdoutR = stdin, stdout
	go func() {
		r := bufio.NewReaderSize(stdout, 1<<20)
		for {
			line, err := r.ReadBytes('\n')
			if len(strings.TrimSpace(string(line))) > 0 {
				c.dispatch(line, h)
			}
			if err != nil {
				break
			}
		}
		c.exitErr = cmd.Wait()
		close(c.done)
		if h.exit != nil {
			h.exit(c)
		}
	}()
	return c, nil
}

func (c *codexConn) dispatch(line []byte, h codexHandlers) {
	if h.trace != nil {
		h.trace(line)
	}
	var m rpcMsg
	if err := json.Unmarshal(line, &m); err != nil {
		log.Printf("orch: bad line from %s: %.200s", c.name, line)
		return
	}
	hasID := len(m.ID) > 0 && string(m.ID) != "null"
	switch {
	case m.Method != "" && hasID:
		if h.request != nil {
			h.request(c, m.ID, m.Method, m.Params)
		}
	case m.Method != "":
		if h.notify != nil {
			h.notify(c, m.Method, m.Params)
		}
	case hasID:
		var id int64
		if json.Unmarshal(m.ID, &id) != nil {
			return
		}
		c.pmu.Lock()
		ch := c.pending[id]
		delete(c.pending, id)
		c.pmu.Unlock()
		if ch != nil {
			ch <- m
		}
	}
}

func (c *codexConn) alive() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

func (c *codexConn) write(v any) error {
	if m, ok := v.(map[string]any); ok && c.rpc2 {
		m["jsonrpc"] = "2.0"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if !c.alive() {
		return errCodexExited
	}
	_, err = c.stdin.Write(b)
	return err
}

func (c *codexConn) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	ch := make(chan rpcMsg, 1)
	c.pmu.Lock()
	c.pending[id] = ch
	c.pmu.Unlock()
	defer func() {
		c.pmu.Lock()
		delete(c.pending, id)
		c.pmu.Unlock()
	}()
	if err := c.write(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return nil, errCodexExited
	}
	select {
	case m := <-ch:
		if m.Error != nil {
			return nil, m.Error
		}
		return m.Result, nil
	case <-c.done:
		return nil, errCodexExited
	case <-ctx.Done():
		return nil, fmt.Errorf("codex %s: %w", method, ctx.Err())
	}
}

func (c *codexConn) notify(method string, params any) error {
	m := map[string]any{"method": method}
	if params != nil {
		m["params"] = params
	}
	return c.write(m)
}

func (c *codexConn) respond(id json.RawMessage, result any) error {
	return c.write(map[string]any{"id": id, "result": result})
}

func (c *codexConn) respondError(id json.RawMessage, code int64, msg string) error {
	return c.write(map[string]any{"id": id, "error": map[string]any{"code": code, "message": msg}})
}

// kill closes stdin, then terminates the process group if it does not leave on its own.
func (c *codexConn) kill() {
	killProc(c.stdin, &c.wmu, c.cmd, c.done, 1500*time.Millisecond, 2*time.Second)
}

// killFast is kill with short grace periods (daemon shutdown).
func (c *codexConn) killFast() {
	killProc(c.stdin, &c.wmu, c.cmd, c.done, 400*time.Millisecond, 400*time.Millisecond)
}

// killProc closes stdin, waits, sends SIGTERM to the process group, waits, then SIGKILL.
func killProc(stdin io.Closer, wmu *sync.Mutex, cmd *exec.Cmd, done <-chan struct{}, grace, term time.Duration) {
	wmu.Lock()
	_ = stdin.Close()
	wmu.Unlock()
	select {
	case <-done:
		return
	case <-time.After(grace):
	}
	if p := cmd.Process; p != nil {
		_ = syscall.Kill(-p.Pid, syscall.SIGTERM)
		select {
		case <-done:
			return
		case <-time.After(term):
		}
		_ = syscall.Kill(-p.Pid, syscall.SIGKILL)
	}
}

// reap stops the process without trusting stdout to reach EOF: a grandchild that escaped the process group
// can hold the pipe open. After capD it closes our end, so the reader and Wait finish (Wait has WaitDelay).
func (c *codexConn) reap(capD time.Duration) {
	go c.kill()
	select {
	case <-c.done:
		return
	case <-time.After(capD):
	}
	_ = c.stdoutR.Close()
	select {
	case <-c.done:
	case <-time.After(3 * time.Second):
	}
}

func (c *codexConn) exitSummary() string {
	msg := c.name + " exited"
	if c.exitErr != nil {
		msg += ": " + c.exitErr.Error()
	}
	if tail := c.stderr.lastLines(8); tail != "" {
		msg += "\n" + tail
	}
	return msg
}

// ── manager ──

type orchHTTPError struct {
	code int
	msg  string
}

func (e *orchHTTPError) Error() string { return e.msg }

func orchConflict(msg string) error   { return &orchHTTPError{409, msg} }
func orchBadRequest(msg string) error { return &orchHTTPError{400, msg} }

// OrchManager owns the driven threads and one codex app-server child per live thread.
type OrchManager struct {
	store     *OrchStore
	approvals *ApprovalStore
	// command builds the codex app-server process; extra holds the -c overrides.
	command    func(th *OThread, extra []string) *exec.Cmd
	rpcTimeout time.Duration
	// holdFor is how long a driven approval waits before it is declined; 0 waits for the user.
	holdFor func() time.Duration
	trace   func(line []byte)

	mu       sync.Mutex
	stopping atomic.Bool // daemon shutdown: child exits are not recorded as failures
	drivers  map[string]orchDriver
	natives  map[string]string
	// procEnd is when each thread's process last exited (ms), for the handover rule.
	procEnd map[string]int64

	claude orchClaudeConfig
	// mcp is agent-monitor's MCP server for driven processes (nil: not offered).
	mcp       func() *orchMCPServer
	daemonURL string
	stateDir  string
	dmu       sync.Mutex // serializes child limit checks with inserts
	nmu       sync.Mutex // serializes parent notifications
	// watched resolves a watched v3 session for Continue here.
	watched func(sessionID string) (*orchWatched, error)
	git     *orchGit
	agents  *orchAgents
	// queue holds messages sent while a thread was busy (nil: off).
	queue *QueueManager
	// acpCommand builds the process for an ACP registry entry.
	acpCommand func(a acpAgent, th *OThread) (*exec.Cmd, error)
	// acpCancelGrace is how long an ACP agent may take to end a cancelled turn.
	acpCancelGrace time.Duration
	// acpTurnTimeout overrides the acpTurnTimeoutMin setting (tests).
	acpTurnTimeout func() time.Duration
	// spendCfg overrides the spend settings (tests).
	spendCfg     func() SpendSettings
	smu          sync.Mutex
	spendStopped map[string]bool
	// notifier receives needs-you, finished, error and spend events.
	notifier func(notifyEvent)
	// groupNotified holds, per parent, the newest child update already announced (guarded by nmu).
	groupNotified map[string]int64
	// reportHeld marks parents whose subagent report waits on a spend limit (guarded by nmu).
	reportHeld map[string]bool
	// spendGranted marks threads whose current turn an Allow once let through (guarded by smu).
	spendGranted map[string]bool
}

func codexBinary() string {
	if v := os.Getenv("AGENT_MONITOR_CODEX_BIN"); v != "" {
		return v
	}
	if p := filepath.Join(homeDir(), ".local", "bin", "codex"); pathExists(p) {
		return p
	}
	if p, err := exec.LookPath("codex"); err == nil {
		return p
	}
	return "codex"
}

// newCodexAppServerCmd builds the codex app-server process; every launch uses this one.
func newCodexAppServerCmd(extra ...string) *exec.Cmd {
	return exec.Command(codexBinary(), append([]string{"app-server"}, extra...)...)
}

func newOrchManager(store *OrchStore, approvals *ApprovalStore) *OrchManager {
	m := &OrchManager{
		store: store, approvals: approvals, rpcTimeout: 120 * time.Second,
		drivers: map[string]orchDriver{}, natives: map[string]string{}, procEnd: map[string]int64{}, spendStopped: map[string]bool{},
		reportHeld: map[string]bool{}, spendGranted: map[string]bool{},
		command: func(th *OThread, extra []string) *exec.Cmd {
			return newCodexAppServerCmd(extra...)
		},
		claude:  defaultClaudeConfig(),
		watched: lookupWatched,
		git:     newOrchGit(), agents: newOrchAgents(),
		acpCommand: func(a acpAgent, th *OThread) (*exec.Cmd, error) { return acpCommandFor(a) },
		mcp:        defaultOrchMCP, daemonURL: defaultDaemonURL(), stateDir: stateDir(),
	}
	m.holdFor = func() time.Duration {
		if approvals == nil {
			return 0
		}
		approvals.mu.Lock()
		defer approvals.mu.Unlock()
		return time.Duration(approvals.settings.DrivenApprovalTimeoutSec) * time.Second
	}
	if err := store.RecoverAfterRestart(); err != nil {
		log.Printf("orch: recover: %v", err)
	}
	if err := store.SweepGroups(); err != nil {
		log.Printf("orch: groups: %v", err)
	}
	if threads, _, err := store.Threads(); err == nil {
		for _, th := range threads {
			if th.NativeID != "" {
				m.natives[th.NativeID] = th.ID
			}
		}
	}
	return m
}

// DrivenID returns the driven thread id for a Codex native session id.
func (m *OrchManager) DrivenID(native string) string {
	if m == nil || native == "" {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.natives[native]
}

// DrivenLive is true when a native session id belongs to a driven thread whose process runs a turn now.
// Only then does the user's hook step aside; a closed or idle thread's session gets normal hook handling.
func (m *OrchManager) DrivenLive(native string) bool {
	if m == nil || native == "" {
		return false
	}
	m.mu.Lock()
	d := m.drivers[m.natives[native]]
	m.mu.Unlock()
	return d != nil && d.live()
}

func (m *OrchManager) setNative(threadID, native string) {
	m.mu.Lock()
	m.natives[native] = threadID
	m.mu.Unlock()
}

func (m *OrchManager) driver(id string) (orchDriver, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if d, ok := m.drivers[id]; ok {
		return d, nil
	}
	th, err := m.store.Thread(id)
	if err != nil {
		return nil, err
	}
	var d orchDriver
	base := orchBase{m: m, id: id}
	switch th.Agent {
	case "claude":
		d = newClaudeDriver(base, th)
	case "codex", "":
		d = &codexDriver{orchBase: base, nativeID: th.NativeID, closing: th.Status == "closed",
			turnRuns: map[string]string{}, waits: map[string]*orchWait{}}
	default:
		d = newACPDriver(base, th)
	}
	m.drivers[id] = d
	return d, nil
}

type orchCreate struct {
	Agent          string `json:"agent"`
	Cwd            string `json:"cwd"`
	Prompt         string `json:"prompt"`
	Model          string `json:"model,omitempty"`
	Sandbox        string `json:"sandbox,omitempty"`
	ApprovalPolicy string `json:"approvalPolicy,omitempty"`
	PermissionMode string `json:"permissionMode,omitempty"`
	// Children are started with the thread (fan-out).
	Children []orchChildSpec `json:"children,omitempty"`
	// Workspace is "here" (default), "worktree" or "read-only" for a root thread.
	Workspace string `json:"workspace,omitempty"`
	// Attachments are upload ids sent with the first prompt.
	Attachments []string `json:"attachments,omitempty"`
	// Set by template runs and schedules.
	TemplateID string `json:"-"`
	ScheduleID string `json:"-"`
	CatchUp    bool   `json:"-"`
	AutoReview string `json:"-"`
	Where      string `json:"-"` // "terminal" starts a tmux session instead of a thread
}

func orchID(prefix string) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

func gitInfo(cwd string) (repo, branch string) {
	run := func(args ...string) string {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", cwd}, args...)...)
		cmd.Env = gitEnv()
		out, err := cmd.Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	if top := run("rev-parse", "--show-toplevel"); top != "" {
		repo = filepath.Base(top)
		if name := repoNameFromCommonDir(run("rev-parse", "--path-format=absolute", "--git-common-dir")); name != "" {
			repo = name
		}
		branch = run("rev-parse", "--abbrev-ref", "HEAD")
	}
	return repo, branch
}

// repoNameFromCommonDir names the main repository, so a linked worktree reports its origin repo, not its folder.
func repoNameFromCommonDir(common string) string {
	if common == "" {
		return ""
	}
	common = filepath.Clean(common)
	if filepath.Base(common) == ".git" {
		return filepath.Base(filepath.Dir(common))
	}
	return strings.TrimSuffix(filepath.Base(common), ".git")
}

func orchTitle(prompt string) string {
	t := strings.TrimSpace(firstLine(strings.TrimSpace(prompt)))
	if r := []rune(t); len(r) > 80 {
		t = string(r[:80])
	}
	return t
}

func (m *OrchManager) CreateThread(in orchCreate) (*OThread, error) {
	if strings.TrimSpace(in.Prompt) == "" {
		return nil, orchBadRequest("prompt is required")
	}
	imgs, err := m.resolveUploads(in.Attachments)
	if err != nil {
		return nil, err
	}
	if err := m.spendGate(""); err != nil {
		return nil, err
	}
	if in.Workspace == "read-only" {
		in.Sandbox = "read-only"
	}
	th, err := m.newThread(in, "")
	if err != nil {
		return nil, err
	}
	th.Title, th.Status = orchTitle(in.Prompt), "starting"
	if len(in.Children) > orchMaxRunning {
		return nil, orchBadRequest(fmt.Sprintf("at most %d children", orchMaxRunning))
	}
	for i := range in.Children {
		c := &in.Children[i]
		if c.Agent == "" {
			c.Agent = th.Agent
		}
		if err := m.checkChild(c, th.Depth, th.Cwd); err != nil {
			return nil, err
		}
	}
	switch in.Workspace {
	case "", "here":
	case "worktree":
		wt, cwd, err := m.addWorktree(th.Cwd, th.ID, th.Title)
		if err != nil {
			return nil, err
		}
		th.Workspace, th.Worktree, th.Cwd = "worktree", wt, cwd
		th.Repo, th.Branch = gitInfo(cwd)
	case "read-only":
		th.Workspace = "read-only"
	default:
		return nil, orchBadRequest("workspace must be here, worktree or read-only")
	}
	th.TemplateID, th.ScheduleID, th.CatchUp, th.AutoReview = in.TemplateID, in.ScheduleID, in.CatchUp, in.AutoReview
	if err := m.insertOrDrop(th); err != nil {
		return nil, err
	}
	prompt := in.Prompt
	if len(in.Children) > 0 {
		kids, err := m.fanOut(th, in.Children)
		if err != nil {
			m.rollbackFanOut(th.ID, kids)
			return nil, &orchHTTPError{422, "fan-out rolled back: " + err.Error()}
		}
		prompt = fanOutPrompt(prompt, kids)
	}
	return m.startThread(th.ID, prompt, imgs...)
}

// rollbackFanOut interrupts and closes the children a failed fan-out created, then the parent.
func (m *OrchManager) rollbackFanOut(parentID string, kids []*OThread) {
	for _, k := range kids {
		_ = m.Interrupt(k.ID)
		_ = m.Close(k.ID)
	}
	_ = m.Close(parentID)
}

// fanOut creates and starts a new thread's children; it returns those it created.
func (m *OrchManager) fanOut(parent *OThread, specs []orchChildSpec) ([]*OThread, error) {
	var kids []*OThread
	m.dmu.Lock()
	for _, spec := range specs {
		child, err := m.buildChild(parent, spec, 0) // earlier children are inserted, so they count already
		if err == nil {
			err = m.insertOrDrop(child)
		}
		if err != nil {
			m.dmu.Unlock()
			return kids, err
		}
		kids = append(kids, child)
	}
	m.dmu.Unlock()
	for i, k := range kids {
		if _, err := m.startThread(k.ID, specs[i].Prompt); err != nil {
			return kids, err
		}
	}
	return kids, nil
}

// newThread validates a create request and builds the thread row; nativeID is set when it continues a session.
func (m *OrchManager) newThread(in orchCreate, nativeID string) (*OThread, error) {
	if in.Agent == "" {
		in.Agent = "codex"
	}
	if !m.validAgent(in.Agent) {
		return nil, m.agentError()
	}
	if strings.HasPrefix(in.Cwd, "~/") {
		in.Cwd = filepath.Join(homeDir(), in.Cwd[2:])
	}
	if !filepath.IsAbs(in.Cwd) {
		return nil, orchBadRequest("cwd must be an absolute path")
	}
	if st, err := os.Stat(in.Cwd); err != nil || !st.IsDir() {
		return nil, orchBadRequest("cwd does not exist")
	}
	repo, branch := gitInfo(in.Cwd)
	now := time.Now().UnixMilli()
	th := &OThread{
		ID: orchID("ot_"), Kind: "driven", Agent: in.Agent, Cwd: in.Cwd, Repo: repo, Branch: branch,
		Model: in.Model, Status: "idle", NativeID: nativeID, CreatedAt: now, UpdatedAt: now, NotifyParent: boolPtr(true),
	}
	if in.Agent != "claude" && in.Agent != "codex" {
		if a, _ := m.acpAgent(in.Agent); resolveBinary(a.Command) == "" {
			return nil, orchBadRequest(a.Name + " is not installed: " + a.Command + " was not found")
		}
		if err := acpThreadOptions(th, in); err != nil {
			return nil, err
		}
		return th, nil
	}
	if in.Agent == "claude" {
		switch in.PermissionMode {
		case "":
			in.PermissionMode = "manual"
		case "manual", "acceptEdits", "plan", "auto":
		default:
			return nil, orchBadRequest("permissionMode must be manual, acceptEdits, plan or auto")
		}
		th.PermissionMode = in.PermissionMode
		if th.NativeID == "" {
			th.NativeID = newUUID()
		}
		return th, nil
	}
	switch in.Sandbox {
	case "":
		in.Sandbox = "workspace-write"
	case "read-only", "workspace-write":
	default:
		return nil, orchBadRequest("sandbox must be read-only or workspace-write")
	}
	switch in.ApprovalPolicy {
	case "":
		in.ApprovalPolicy = "on-request"
	case "untrusted", "on-request", "never":
	default:
		return nil, orchBadRequest("approvalPolicy must be untrusted, on-request or never")
	}
	th.Sandbox, th.ApprovalPolicy = in.Sandbox, in.ApprovalPolicy
	return th, nil
}

// insertThread stores a new thread and indexes its native id.
func (m *OrchManager) insertThread(th *OThread) error {
	if err := m.store.write(func(t *orchTx) error {
		if err := t.putThread(th); err != nil {
			return err
		}
		t.emit(th.ID, "thread.updated", map[string]any{"thread": th})
		if th.ParentID == "" {
			return nil
		}
		parent, err := t.thread(th.ParentID)
		if err != nil {
			return err
		}
		parent.Children = append(parent.Children, th.ID)
		if err := t.putThread(parent); err != nil {
			return err
		}
		t.emit(parent.ID, "thread.updated", map[string]any{"thread": parent})
		return nil
	}); err != nil {
		return err
	}
	if th.NativeID != "" {
		m.setNative(th.ID, th.NativeID)
	}
	return nil
}

func (m *OrchManager) Send(id, text string) (string, error) {
	return m.SendWith(id, text, nil)
}

// SendWith sends a message with optional image attachments (upload ids).
func (m *OrchManager) SendWith(id, text string, attachments []string) (string, error) {
	if strings.TrimSpace(text) == "" && len(attachments) == 0 {
		return "", orchBadRequest("text is required")
	}
	imgs, err := m.resolveUploads(attachments)
	if err != nil {
		return "", err
	}
	if _, err := m.store.Thread(id); err != nil {
		return "", err
	}
	if m.handover(id).Live {
		return "", orchConflict("live")
	}
	if cmd := bangCommand(text); cmd != "" && len(attachments) == 0 {
		return m.runBang(id, cmd)
	}
	if err := m.spendGate(id); err != nil {
		return "", err
	}
	d, err := m.driver(id)
	if err != nil {
		return "", err
	}
	mode, err := d.send(takeBangContext(id)+text, imgs)
	return mode, err
}

func (m *OrchManager) Interrupt(id string) error {
	d, err := m.driver(id)
	if err != nil {
		return err
	}
	return d.interrupt()
}

func (m *OrchManager) Close(id string) error {
	d, err := m.driver(id)
	if err != nil {
		return err
	}
	d.close()
	return nil
}

// Shutdown stops every child process; the threads stay as they are in the DB.
func (m *OrchManager) Shutdown() {
	m.stopping.Store(true)
	m.mu.Lock()
	ds := make([]orchDriver, 0, len(m.drivers))
	for _, d := range m.drivers {
		ds = append(ds, d)
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, d := range ds {
		wg.Add(1)
		go func() { defer wg.Done(); d.shutdown() }()
	}
	wg.Wait()
}

// ── driver: one driven thread ──

type orchWait struct {
	cancel context.CancelFunc
	runID  string
}

type codexDriver struct {
	orchBase

	opMu sync.Mutex // serializes process start

	mu        sync.Mutex
	conn      *codexConn
	starting  *codexConn // process in its handshake; close kills it
	nativeID  string
	runID     string
	turnID    string
	turnRuns  map[string]string
	closing   bool
	lastTotal *OUsage
	runBase   *OUsage
	runErr    bool
	waits     map[string]*orchWait
}

func (d *codexDriver) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d.m.rpcTimeout)
}

// send starts a turn, or steers the running one.
func (d *codexDriver) send(text string, imgs []orchImage) (string, error) {
	d.mu.Lock()
	if d.closing {
		d.mu.Unlock()
		return "", orchConflict("thread is closed")
	}
	if d.runID != "" {
		runID, turnID, conn, native := d.runID, d.turnID, d.conn, d.nativeID
		d.mu.Unlock()
		if turnID == "" || conn == nil {
			return "", orchConflict("turn is starting; try again in a moment")
		}
		ctx, cancel := d.ctx()
		defer cancel()
		if _, err := conn.call(ctx, "turn/steer", map[string]any{"threadId": native, "expectedTurnId": turnID, "input": codexInput(text, imgs)}); err != nil {
			return "", orchConflict("steer failed: " + err.Error())
		}
		d.addUserItem(runID, text, imgs)
		return "steer", nil
	}
	runID := orchID("or_")
	d.runID, d.turnID, d.runErr, d.runBase = runID, "", false, d.lastTotal
	d.mu.Unlock()

	err := d.beginRun(runID, text, imgs)
	if err != nil {
		d.mu.Lock()
		d.runID = ""
		d.mu.Unlock()
		return "", err
	}
	go d.runTurn(runID, text, imgs)
	return "turn", nil
}

func (d *codexDriver) runTurn(runID, text string, imgs []orchImage) {
	conn, native, err := d.ensureProcess()
	if err != nil {
		d.failRun(runID, err.Error(), true)
		return
	}
	d.mu.Lock()
	mine := d.runID == runID && !d.closing
	d.mu.Unlock()
	if !mine {
		return // closed or ended while the process started: never send the prompt
	}
	ctx, cancel := d.ctx()
	defer cancel()
	res, err := conn.call(ctx, "turn/start", map[string]any{"threadId": native, "input": codexInput(text, imgs)})
	if errors.Is(err, errCodexExited) {
		return // the exit handler records the failure with stderr
	}
	if err != nil {
		d.failRun(runID, "turn/start failed: "+err.Error(), false)
		return
	}
	var r struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	_ = json.Unmarshal(res, &r)
	if r.Turn.ID == "" {
		return
	}
	d.mu.Lock()
	d.turnRuns[r.Turn.ID] = runID
	if d.runID == runID && d.turnID == "" {
		d.turnID = r.Turn.ID
	}
	d.mu.Unlock()
	_ = d.m.store.write(func(t *orchTx) error {
		run, err := t.run(runID)
		if err != nil || run.TurnID != "" {
			return nil
		}
		run.TurnID = r.Turn.ID
		return t.putRun(run)
	})
}

// failRun ends a run that never got going; threadErr also puts the thread in error.
func (d *codexDriver) failRun(runID, msg string, threadErr bool) {
	d.mu.Lock()
	if d.runID != runID {
		d.mu.Unlock()
		return
	}
	d.runID, d.turnID = "", ""
	d.mu.Unlock()
	d.writeRunFailure(runID, msg, threadErr)
}

// ensureProcess spawns codex app-server, runs the handshake and starts or resumes the native thread.
func (d *codexDriver) ensureProcess() (*codexConn, string, error) {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	d.mu.Lock()
	if d.closing {
		d.mu.Unlock()
		return nil, "", errOrchClosed
	}
	if d.conn != nil && d.conn.alive() {
		c, n := d.conn, d.nativeID
		d.mu.Unlock()
		return c, n, nil
	}
	d.mu.Unlock()

	th, err := d.m.store.Thread(d.id)
	if err != nil {
		return nil, "", err
	}
	cmd := d.m.command(th, d.m.codexMCPArgs(th))
	cmd.Dir = th.Cwd
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = append(cmd.Env, "AGENT_MONITOR_DRIVEN="+d.id)
	cmd.Env = append(cmd.Env, d.m.orchProcEnv(th)...)
	conn, err := startCodexConn(cmd, codexHandlers{notify: d.onNotify, request: d.onRequest, exit: d.onExit, trace: d.m.trace})
	if err != nil {
		return nil, "", fmt.Errorf("start codex app-server: %w", err)
	}
	fail := func(err error) (*codexConn, string, error) {
		d.mu.Lock()
		if d.starting == conn {
			d.starting = nil
		}
		d.mu.Unlock()
		go conn.kill()
		<-conn.done
		if errors.Is(err, errCodexExited) {
			return nil, "", errors.New(conn.exitSummary())
		}
		return nil, "", err
	}
	d.mu.Lock()
	closing := d.closing
	d.starting = conn
	d.mu.Unlock()
	if closing {
		return fail(errOrchClosed)
	}
	ctx, cancel := d.ctx()
	defer cancel()
	if _, err := conn.call(ctx, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "agent-monitor", "title": "agent-monitor", "version": "0.1.0"},
		"capabilities": map[string]any{"experimentalApi": false},
	}); err != nil {
		return fail(err)
	}
	if err := conn.notify("initialized", nil); err != nil {
		return fail(err)
	}
	params := map[string]any{"cwd": th.Cwd, "sandbox": th.Sandbox, "approvalPolicy": firstOf(th.ApprovalPolicy, "on-request")}
	if th.Model != "" {
		params["model"] = th.Model
	}
	method := "thread/start"
	if th.NativeID != "" {
		method = "thread/resume"
		params["threadId"] = th.NativeID
		params["excludeTurns"] = true
	}
	res, err := conn.call(ctx, method, params)
	if err != nil {
		return fail(err)
	}
	var r struct {
		Model  string `json:"model"`
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	_ = json.Unmarshal(res, &r)
	native := firstOf(r.Thread.ID, th.NativeID)
	if native == "" {
		return fail(fmt.Errorf("%s returned no thread id", method))
	}
	d.mu.Lock()
	if d.closing {
		d.mu.Unlock()
		return fail(errOrchClosed)
	}
	d.starting = nil
	d.conn, d.nativeID = conn, native
	d.turnRuns = map[string]string{}
	d.mu.Unlock()
	d.m.setNative(d.id, native)
	_ = d.m.store.write(func(t *orchTx) error {
		th, err := t.thread(d.id)
		if err != nil {
			return err
		}
		th.NativeID = native
		if r.Model != "" {
			th.Model = r.Model
		}
		if th.Status == "starting" || th.Status == "error" || th.Status == "idle" {
			th.Status = "running"
		}
		th.UpdatedAt = t.now
		if err := t.putThread(th); err != nil {
			return err
		}
		t.emit(d.id, "thread.updated", map[string]any{"thread": th})
		return nil
	})
	return conn, native, nil
}

func (d *codexDriver) interrupt() error {
	d.mu.Lock()
	conn, turnID, native := d.conn, d.turnID, d.nativeID
	d.mu.Unlock()
	if conn == nil || turnID == "" {
		return orchConflict("no running turn")
	}
	ctx, cancel := d.ctx()
	defer cancel()
	if _, err := conn.call(ctx, "turn/interrupt", map[string]any{"threadId": native, "turnId": turnID}); err != nil {
		return orchConflict("interrupt failed: " + err.Error())
	}
	return nil
}

func (d *codexDriver) close() {
	d.mu.Lock()
	d.closing = true
	conn, starting, runID := d.conn, d.starting, d.runID
	d.runID, d.turnID = "", ""
	waits := d.waits
	d.waits = map[string]*orchWait{}
	d.mu.Unlock()
	for _, w := range waits {
		w.cancel()
	}
	d.writeClosed(runID)
	for _, c := range []*codexConn{conn, starting} {
		if c != nil {
			c.kill()
		}
	}
}

func (d *codexDriver) shutdown() {
	d.mu.Lock()
	c, st := d.conn, d.starting
	d.mu.Unlock()
	for _, x := range []*codexConn{c, st} {
		if x != nil {
			x.killFast()
		}
	}
}

// live is true while the process runs a turn for an open thread.
func (d *codexDriver) live() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return !d.closing && d.runID != "" && d.conn != nil && d.conn.alive()
}

func (d *codexDriver) procAlive() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.conn != nil && d.conn.alive()
}

// endRunTx sets a run's final status and fails items it left open.
func endRunTx(t *orchTx, threadID, runID, status string) {
	run, err := t.run(runID)
	if err != nil {
		return
	}
	if run.Status == "running" {
		run.Status = status
	}
	run.EndedAt = t.now
	_ = t.putRun(run)
	items, _ := scanJSONRows[OItem](t.tx.Query(`SELECT json FROM items WHERE run_id=? AND json_extract(json,'$.status')='started'`, runID))
	for i := range items {
		it := &items[i]
		it.Status, it.EndedAt = "failed", t.now
		_ = t.putItem(it)
		t.emit(threadID, "item.completed", map[string]any{"item": it})
	}
	t.emit(threadID, "run.ended", map[string]any{"run": run})
}

func (d *codexDriver) onExit(c *codexConn) {
	d.mu.Lock()
	if d.conn != c {
		d.mu.Unlock()
		return
	}
	defer d.m.noteProcEnd(d.id)
	d.conn = nil
	closing, runID := d.closing, d.runID
	d.runID, d.turnID = "", ""
	waits := d.waits
	d.waits = map[string]*orchWait{}
	d.mu.Unlock()
	for _, w := range waits {
		w.cancel()
	}
	if closing || d.m.stopping.Load() {
		return
	}
	d.writeExit(runID, c.exitSummary(), c.exitErr != nil)
}

// runFor maps a codex turn id to its run, adopting an unknown turn into the active run.
func (d *codexDriver) runFor(turnID string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if r, ok := d.turnRuns[turnID]; ok {
		return r
	}
	if d.runID == "" {
		return ""
	}
	if turnID != "" {
		d.turnRuns[turnID] = d.runID
		if d.turnID == "" {
			d.turnID = turnID
		}
	}
	return d.runID
}

// ── notifications ──

type codexItem struct {
	Type             string          `json:"type"`
	ID               string          `json:"id"`
	Text             string          `json:"text"`
	Command          string          `json:"command"`
	Cwd              string          `json:"cwd"`
	ExitCode         *int            `json:"exitCode"`
	AggregatedOutput *string         `json:"aggregatedOutput"`
	Status           string          `json:"status"`
	Summary          []string        `json:"summary"`
	Content          json.RawMessage `json:"content"`
	Changes          []struct {
		Path string `json:"path"`
		Kind struct {
			Type string `json:"type"`
		} `json:"kind"`
		Diff string `json:"diff"`
	} `json:"changes"`
	Server string          `json:"server"`
	Tool   string          `json:"tool"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
	Query  string          `json:"query"`
	Path   string          `json:"path"`
}

func diffCounts(kind, diff string) (add, del int) {
	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"):
		case strings.HasPrefix(l, "+"):
			add++
		case strings.HasPrefix(l, "-"):
			del++
		}
	}
	if kind == "add" && add == 0 && del == 0 && diff != "" {
		add = strings.Count(strings.TrimRight(diff, "\n"), "\n") + 1
	}
	return add, del
}

func codexStatus(s, def string) string {
	switch s {
	case "inProgress":
		return "started"
	case "completed", "failed", "declined":
		return s
	}
	return def
}

func clipJSON(raw json.RawMessage) string {
	s := string(raw)
	if s == "null" {
		return ""
	}
	if len(s) > orchOutputCap {
		s = s[:orchOutputCap]
	}
	return s
}

// mapCodexItem converts a codex ThreadItem; ok is false for items we do not show (user messages).
func mapCodexItem(ci *codexItem, threadID, runID, defStatus string) (*OItem, bool) {
	it := &OItem{ID: runID + "." + ci.ID, ThreadID: threadID, RunID: runID, Status: codexStatus(ci.Status, defStatus)}
	switch ci.Type {
	case "userMessage":
		return nil, false
	case "agentMessage":
		it.Kind, it.Text = "agent", ci.Text
	case "reasoning":
		it.Kind = "reasoning"
		var content []string
		_ = json.Unmarshal(ci.Content, &content)
		if len(ci.Summary) > 0 {
			it.Text = strings.Join(ci.Summary, "\n\n")
		} else {
			it.Text = strings.Join(content, "\n\n")
		}
	case "commandExecution":
		it.Kind, it.Command, it.Cwd, it.ExitCode = "command", ci.Command, ci.Cwd, ci.ExitCode
		if ci.AggregatedOutput != nil {
			it.Output = *ci.AggregatedOutput
		}
	case "fileChange":
		it.Kind = "fileChange"
		var paths []string
		for _, ch := range ci.Changes {
			a, dl := diffCounts(ch.Kind.Type, ch.Diff)
			it.Files = append(it.Files, OFile{Path: ch.Path, Kind: ch.Kind.Type, Add: a, Del: dl})
			paths = append(paths, ch.Path)
		}
		it.Text = strings.Join(paths, "\n")
	case "mcpToolCall":
		it.Kind, it.Text = "mcp", ci.Server+"."+ci.Tool
		it.Output = firstOf(clipJSON(ci.Error), clipJSON(ci.Result))
	case "webSearch":
		it.Kind, it.Text = "web", ci.Query
	case "plan":
		it.Kind, it.Text = "plan", ci.Text
	case "contextCompaction":
		it.Kind, it.Text = "other", "Context compacted"
	case "dynamicToolCall", "collabAgentToolCall":
		it.Kind, it.Text = "other", ci.Type+": "+ci.Tool
	case "imageView":
		it.Kind, it.Text = "other", "Viewed image "+ci.Path
	default:
		it.Kind, it.Text = "other", ci.Type
	}
	return it, true
}

func (d *codexDriver) onNotify(c *codexConn, method string, params json.RawMessage) {
	var p struct {
		ThreadID string          `json:"threadId"`
		TurnID   string          `json:"turnId"`
		ItemID   string          `json:"itemId"`
		Delta    string          `json:"delta"`
		Item     json.RawMessage `json:"item"`
		Turn     json.RawMessage `json:"turn"`
		Status   json.RawMessage `json:"status"`
		Error    *struct {
			Message string `json:"message"`
		} `json:"error"`
		WillRetry  bool            `json:"willRetry"`
		TokenUsage json.RawMessage `json:"tokenUsage"`
		RequestID  json.RawMessage `json:"requestId"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	store := d.m.store
	switch method {
	case "turn/started":
		var turn struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(p.Turn, &turn)
		d.runFor(turn.ID)
	case "item/started", "item/completed":
		var ci codexItem
		if json.Unmarshal(p.Item, &ci) != nil {
			return
		}
		runID := d.runFor(p.TurnID)
		if runID == "" {
			return
		}
		def := "started"
		if method == "item/completed" {
			def = "completed"
		}
		it, ok := mapCodexItem(&ci, d.id, runID, def)
		if !ok {
			return
		}
		if method == "item/started" {
			it.StartedAt = time.Now().UnixMilli()
			_ = store.write(func(t *orchTx) error {
				if err := t.putItem(it); err != nil {
					return err
				}
				t.emit(d.id, "item.started", map[string]any{"item": it})
				return nil
			})
			return
		}
		store.flushDeltas(it.ID)
		_ = store.write(func(t *orchTx) error {
			now := t.now
			prev, err := t.item(it.ID)
			if err == nil {
				it.StartedAt = prev.StartedAt
				if it.Text == "" {
					it.Text = prev.Text
				}
				if it.Output == "" {
					it.Output = prev.Output
				}
			} else {
				it.StartedAt = now
				t.emit(d.id, "item.started", map[string]any{"item": it})
			}
			if it.Status == "started" {
				it.Status = "completed"
			}
			it.EndedAt = now
			if err := t.putItem(it); err != nil {
				return err
			}
			t.emit(d.id, "item.completed", map[string]any{"item": it})
			return nil
		})
	case "item/agentMessage/delta", "item/plan/delta", "item/reasoning/summaryTextDelta", "item/reasoning/textDelta":
		if runID := d.runFor(p.TurnID); runID != "" {
			store.AppendDelta(d.id, runID+"."+p.ItemID, "text", p.Delta)
		}
	case "item/commandExecution/outputDelta":
		if runID := d.runFor(p.TurnID); runID != "" {
			store.AppendDelta(d.id, runID+"."+p.ItemID, "output", p.Delta)
		}
	case "thread/tokenUsage/updated":
		d.onTokenUsage(p.TurnID, p.TokenUsage)
	case "error":
		if p.WillRetry || p.Error == nil {
			return
		}
		runID := d.runFor(p.TurnID)
		if runID == "" {
			return
		}
		d.mu.Lock()
		d.runErr = true
		d.mu.Unlock()
		_ = store.write(func(t *orchTx) error {
			it := &OItem{ID: orchID(runID + ".e"), ThreadID: d.id, RunID: runID, Kind: "error", Status: "failed", Text: p.Error.Message, StartedAt: t.now, EndedAt: t.now}
			if err := t.putItem(it); err != nil {
				return err
			}
			t.emit(d.id, "item.started", map[string]any{"item": it})
			t.emit(d.id, "item.completed", map[string]any{"item": it})
			return nil
		})
	case "turn/completed":
		d.onTurnCompleted(p.Turn)
	case "serverRequest/resolved":
		d.mu.Lock()
		w := d.waits[string(p.RequestID)]
		d.mu.Unlock()
		if w != nil {
			w.cancel()
		}
	case "thread/status/changed":
		var st struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(p.Status, &st) == nil && st.Type == "systemError" {
			_ = store.write(func(t *orchTx) error {
				th, err := t.thread(d.id)
				if err != nil || th.Status == "closed" {
					return nil
				}
				th.Status, th.Error, th.UpdatedAt = "error", "codex reported a system error", t.now
				_ = t.putThread(th)
				t.emit(d.id, "thread.updated", map[string]any{"thread": th})
				return nil
			})
		}
	}
}

func (d *codexDriver) onTokenUsage(turnID string, raw json.RawMessage) {
	var u struct {
		Total struct {
			InputTokens       int64 `json:"inputTokens"`
			CachedInputTokens int64 `json:"cachedInputTokens"`
			OutputTokens      int64 `json:"outputTokens"`
		} `json:"total"`
		Last struct {
			InputTokens       int64 `json:"inputTokens"`
			CachedInputTokens int64 `json:"cachedInputTokens"`
			OutputTokens      int64 `json:"outputTokens"`
		} `json:"last"`
	}
	if json.Unmarshal(raw, &u) != nil {
		return
	}
	total := &OUsage{InTokens: u.Total.InputTokens, OutTokens: u.Total.OutputTokens, CachedIn: u.Total.CachedInputTokens}
	runID := d.runFor(turnID)
	d.mu.Lock()
	base := d.runBase
	if base == nil && runID != "" {
		base = &OUsage{InTokens: total.InTokens - u.Last.InputTokens, OutTokens: total.OutTokens - u.Last.OutputTokens,
			CachedIn: total.CachedIn - u.Last.CachedInputTokens}
		d.runBase = base
	}
	d.lastTotal = total
	d.mu.Unlock()
	if runID == "" {
		return
	}
	_ = d.m.store.write(func(t *orchTx) error {
		run, err := t.run(runID)
		if err != nil {
			return nil
		}
		usage := &OUsage{InTokens: max(0, total.InTokens-base.InTokens), OutTokens: max(0, total.OutTokens-base.OutTokens),
			CachedIn: max(0, total.CachedIn-base.CachedIn)}
		// Codex reports no price, so spend limits need a computed one.
		var model string
		if th, err := t.thread(d.id); err == nil {
			model = th.Model
		}
		usage.CostUSD, usage.Estimated = codexRunCost(model, usage, d.m.spendSettings())
		run.Usage = usage
		return t.putRun(run)
	})
}

func (d *codexDriver) onTurnCompleted(raw json.RawMessage) {
	var turn struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &turn) != nil {
		return
	}
	runID := d.runFor(turn.ID)
	if runID == "" {
		return
	}
	d.mu.Lock()
	sawErr := d.runErr
	if d.runID == runID {
		d.runID, d.turnID = "", ""
	}
	var waits []*orchWait
	for k, w := range d.waits {
		if w.runID == runID {
			waits = append(waits, w)
			delete(d.waits, k)
		}
	}
	d.mu.Unlock()
	for _, w := range waits {
		w.cancel()
	}
	status := "completed"
	switch turn.Status {
	case "interrupted", "failed":
		status = turn.Status
	}
	errText := ""
	if status == "failed" && turn.Error != nil && !sawErr {
		errText = turn.Error.Message
	}
	d.writeRunEnd(runID, status, errText, nil)
}

// ── approvals: server requests become Approvals in the shared ApprovalStore ──

type codexQuestion struct {
	ID       string `json:"id"`
	Header   string `json:"header"`
	Question string `json:"question"`
	Options  []struct {
		Label       string `json:"label"`
		Description string `json:"description"`
	} `json:"options"`
}

func (d *codexDriver) onRequest(c *codexConn, id json.RawMessage, method string, params json.RawMessage) {
	switch method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval",
		"item/permissions/requestApproval", "item/tool/requestUserInput":
		// The wait exists before a serverRequest/resolved for it can be read.
		var t struct {
			TurnID string `json:"turnId"`
		}
		_ = json.Unmarshal(params, &t)
		runID := d.runFor(t.TurnID)
		ctx, cancel := context.WithCancel(context.Background())
		d.mu.Lock()
		d.waits[string(id)] = &orchWait{cancel: cancel, runID: runID}
		d.mu.Unlock()
		go d.handleApproval(ctx, cancel, runID, c, id, method, params)
	case "mcpServer/elicitation/request":
		_ = c.respond(id, map[string]any{"action": "decline"})
	default:
		_ = c.respondError(id, -32601, "agent-monitor does not support "+method)
	}
}

func (d *codexDriver) handleApproval(ctx context.Context, cancel context.CancelFunc, runID string, c *codexConn, rpcID json.RawMessage, method string, params json.RawMessage) {
	key := string(rpcID)
	defer func() {
		cancel()
		d.mu.Lock()
		delete(d.waits, key)
		d.mu.Unlock()
	}()
	var p struct {
		ThreadID    string          `json:"threadId"`
		TurnID      string          `json:"turnId"`
		ItemID      string          `json:"itemId"`
		Command     string          `json:"command"`
		Cwd         string          `json:"cwd"`
		Reason      string          `json:"reason"`
		GrantRoot   string          `json:"grantRoot"`
		Permissions json.RawMessage `json:"permissions"`
		Questions   []codexQuestion `json:"questions"`
	}
	_ = json.Unmarshal(params, &p)
	itemID := ""
	if runID != "" && p.ItemID != "" {
		itemID = runID + "." + p.ItemID
	}
	th, err := d.m.store.Thread(d.id)
	if err != nil {
		_ = c.respondError(rpcID, -32603, "thread not found")
		return
	}
	if ctx.Err() != nil {
		return
	}
	if threadReadOnly(th) && (method == "item/fileChange/requestApproval" || method == "item/permissions/requestApproval") {
		_ = c.respond(rpcID, codexApprovalResponse(method, approvalDecision{Behavior: "deny"}, p.Permissions, p.Questions))
		return
	}
	cwd := firstOf(p.Cwd, th.Cwd)
	input := map[string]any{}
	if p.Reason != "" {
		input["reason"] = p.Reason
	}
	var kind, tool string
	switch method {
	case "item/commandExecution/requestApproval":
		kind, tool = "command", "shell"
		input["command"], input["cwd"] = p.Command, cwd
	case "item/fileChange/requestApproval":
		kind, tool = "fileChange", "apply_patch"
		var patch strings.Builder
		if it, err := d.itemSnapshot(itemID); err == nil {
			for _, f := range it.Files {
				verb := "Update"
				switch f.Kind {
				case "add":
					verb = "Add"
				case "delete":
					verb = "Delete"
				}
				fmt.Fprintf(&patch, "*** %s File: %s\n", verb, f.Path)
			}
		}
		input["patch"] = patch.String()
		if p.GrantRoot != "" {
			input["grantRoot"] = p.GrantRoot
		}
	case "item/permissions/requestApproval":
		kind, tool = "permissions", "request_permissions"
		var perms any
		_ = json.Unmarshal(p.Permissions, &perms)
		input["permissions"], input["cwd"] = perms, cwd
	case "item/tool/requestUserInput":
		kind, tool = "question", "AskUserQuestion"
		qs := []any{}
		for _, q := range p.Questions {
			opts := []any{}
			for _, o := range q.Options {
				opts = append(opts, map[string]any{"label": o.Label, "description": o.Description})
			}
			qs = append(qs, map[string]any{"id": q.ID, "header": q.Header, "question": q.Question, "options": opts, "multiSelect": false})
		}
		input["questions"] = qs
	}
	a := buildApproval(hookCodex, hookPayload{SessionID: th.NativeID, Cwd: cwd, ToolName: tool, ToolInput: input})
	dec, outcome := d.awaitApproval(ctx, a, runID, itemID, kind, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.runID != ""
	})
	if outcome != "canceled" {
		_ = c.respond(rpcID, codexApprovalResponse(method, dec, p.Permissions, p.Questions))
	}
}

// codexApprovalResponse renders a decision in the response shape of each server request.
func codexApprovalResponse(method string, dec approvalDecision, perms json.RawMessage, qs []codexQuestion) any {
	allow := dec.Behavior == "allow"
	switch method {
	case "item/permissions/requestApproval":
		granted := json.RawMessage(`{}`)
		if allow && len(perms) > 0 && string(perms) != "null" {
			granted = perms
		}
		return map[string]any{"permissions": granted, "scope": "turn"}
	case "item/tool/requestUserInput":
		answers := map[string]any{}
		if allow {
			for _, q := range qs {
				ans, ok := dec.Answers[q.ID]
				if !ok {
					ans, ok = dec.Answers[q.Question]
				}
				if ok {
					answers[q.ID] = map[string]any{"answers": []string{ans}}
				}
			}
		}
		return map[string]any{"answers": answers}
	default:
		switch {
		case allow:
			return map[string]any{"decision": "accept"}
		case dec.Interrupt:
			return map[string]any{"decision": "cancel"}
		default:
			return map[string]any{"decision": "decline"}
		}
	}
}

// awaitDriven runs an approval through rules, then waits for a decision.
// outcome is rule, decided, timeout (declined) or canceled (no answer should be sent).
func (s *ApprovalStore) awaitDriven(ctx context.Context, a *Approval, hold time.Duration, onPending func(*Approval)) (approvalDecision, string) {
	if s == nil {
		return approvalDecision{Behavior: "deny", Message: "approvals unavailable"}, "rule"
	}
	if d := s.ruleDecision(a); d != nil {
		return *d, "rule"
	}
	if hold > 0 {
		a.HoldUntil = a.CreatedAt + hold.Milliseconds()
	}
	pa := &pendingApproval{a: a, hook: hookCodex, reply: make(chan approvalDecision, 1)}
	s.mu.Lock()
	s.pending[a.ID] = pa
	snap := copyApproval(a)
	s.mu.Unlock()
	publishV3("approval", "approval", snap)
	if onPending != nil {
		onPending(snap)
	}
	var expire <-chan time.Time
	if hold > 0 {
		timer := time.NewTimer(hold)
		defer timer.Stop()
		expire = timer.C
	}
	select {
	case d := <-pa.reply:
		return d, "decided"
	case <-expire:
		if _, ok := s.finish(a.ID, "expired", "", "no decision in time", nil); ok {
			return approvalDecision{Behavior: "deny", Message: "no decision in time"}, "timeout"
		}
	case <-ctx.Done():
		if _, ok := s.finish(a.ID, "expired", "agent", "resolved by the agent", nil); ok {
			return approvalDecision{Behavior: "deny"}, "canceled"
		}
	}
	return <-pa.reply, "decided"
}
