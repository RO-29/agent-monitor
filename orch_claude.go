package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ── claude -p stream-json child process ──

var errClaudeExited = errors.New("claude exited")

type orchClaudeConfig struct {
	Command func(args []string) *exec.Cmd
	// ExtraArgs is appended to every spawn (tests and probes turn user settings off here).
	ExtraArgs []string
	// SessionExists reports whether a transcript exists, so a spawn resumes instead of creating.
	SessionExists func(nativeID string) bool
}

func claudeBinary() string {
	if v := os.Getenv("AGENT_MONITOR_CLAUDE_BIN"); v != "" {
		return v
	}
	if p := filepath.Join(homeDir(), ".local", "bin", "claude"); pathExists(p) {
		return p
	}
	if p, err := exec.LookPath("claude"); err == nil {
		return p
	}
	return "claude"
}

func claudeConfigDir() string {
	if v := os.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
		return v
	}
	return filepath.Join(homeDir(), ".claude")
}

func claudeSessionExists(native string) bool {
	if native == "" {
		return false
	}
	m, _ := filepath.Glob(filepath.Join(claudeConfigDir(), "projects", "*", native+".jsonl"))
	return len(m) > 0
}

func defaultClaudeConfig() orchClaudeConfig {
	return orchClaudeConfig{
		Command:       func(args []string) *exec.Cmd { return exec.Command(claudeBinary(), args...) },
		SessionExists: claudeSessionExists,
	}
}

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

type claudeProc struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	wmu     sync.Mutex
	next    atomic.Int64
	pmu     sync.Mutex
	pending map[string]chan claudeCtlResp
	done    chan struct{}
	exitErr error
	stderr  *tailBuffer
}

type claudeCtlResp struct {
	Subtype   string          `json:"subtype"`
	RequestID string          `json:"request_id"`
	Response  json.RawMessage `json:"response"`
	Error     string          `json:"error"`
}

func startClaudeProc(cmd *exec.Cmd, onLine func(p *claudeProc, line []byte), onExit func(p *claudeProc), trace func([]byte)) (*claudeProc, error) {
	p := &claudeProc{cmd: cmd, pending: map[string]chan claudeCtlResp{}, done: make(chan struct{}), stderr: &tailBuffer{max: 16 << 10}}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = p.stderr
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	// A grandchild that keeps stderr open must not keep Wait (and done) from returning.
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p.stdin = stdin
	go func() {
		r := bufio.NewReaderSize(stdout, 1<<20)
		for {
			line, err := r.ReadBytes('\n')
			if len(strings.TrimSpace(string(line))) > 0 {
				if trace != nil {
					trace(line)
				}
				if !p.resolve(line) {
					onLine(p, line)
				}
			}
			if err != nil {
				break
			}
		}
		p.exitErr = cmd.Wait()
		close(p.done)
		onExit(p)
	}()
	return p, nil
}

// resolve hands a control_response to the waiting caller; false for every other line.
func (p *claudeProc) resolve(line []byte) bool {
	if !bytes.Contains(line, []byte(`"control_response"`)) {
		return false
	}
	var m struct {
		Type     string        `json:"type"`
		Response claudeCtlResp `json:"response"`
	}
	if json.Unmarshal(line, &m) != nil || m.Type != "control_response" {
		return false
	}
	p.pmu.Lock()
	ch := p.pending[m.Response.RequestID]
	delete(p.pending, m.Response.RequestID)
	p.pmu.Unlock()
	if ch != nil {
		ch <- m.Response
	}
	return true
}

func (p *claudeProc) alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

func (p *claudeProc) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	p.wmu.Lock()
	defer p.wmu.Unlock()
	if !p.alive() {
		return errClaudeExited
	}
	_, err = p.stdin.Write(b)
	return err
}

// writeUser starts or steers a turn; content is a string or a list of content blocks.
func (p *claudeProc) writeUser(content any) error {
	return p.write(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": content}, "parent_tool_use_id": nil, "session_id": ""})
}

// control sends a control_request to the CLI and waits for its control_response.
func (p *claudeProc) control(ctx context.Context, req map[string]any) (json.RawMessage, error) {
	id := fmt.Sprintf("am_%d", p.next.Add(1))
	ch := make(chan claudeCtlResp, 1)
	p.pmu.Lock()
	p.pending[id] = ch
	p.pmu.Unlock()
	defer func() {
		p.pmu.Lock()
		delete(p.pending, id)
		p.pmu.Unlock()
	}()
	if err := p.write(map[string]any{"type": "control_request", "request_id": id, "request": req}); err != nil {
		return nil, errClaudeExited
	}
	select {
	case r := <-ch:
		if r.Subtype == "error" {
			return nil, fmt.Errorf("claude %v: %s", req["subtype"], r.Error)
		}
		return r.Response, nil
	case <-p.done:
		return nil, errClaudeExited
	case <-ctx.Done():
		return nil, fmt.Errorf("claude %v: %w", req["subtype"], ctx.Err())
	}
}

func (p *claudeProc) respond(id string, resp any) error {
	return p.write(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id, "response": resp}})
}

func (p *claudeProc) respondError(id, msg string) error {
	return p.write(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": id, "error": msg}})
}

func (p *claudeProc) kill() {
	killProc(p.stdin, &p.wmu, p.cmd, p.done, 1500*time.Millisecond, 2*time.Second)
}

func (p *claudeProc) killFast() {
	killProc(p.stdin, &p.wmu, p.cmd, p.done, 400*time.Millisecond, 400*time.Millisecond)
}

func (p *claudeProc) exitSummary() string {
	msg := "claude exited"
	if p.exitErr != nil {
		msg += ": " + p.exitErr.Error()
	}
	if tail := p.stderr.lastLines(8); tail != "" {
		msg += "\n" + tail
	}
	return msg
}

// ── driver ──

type claudeBlock struct {
	index    int
	kind     string
	itemID   string
	consumed bool
}

type claudeTool struct {
	itemID, name string
}

type claudeDriver struct {
	orchBase
	opMu sync.Mutex

	mu           sync.Mutex
	proc         *claudeProc
	starting     *claudeProc // process in its handshake; close kills it
	nativeID     string
	runID        string
	turnLive     bool
	closing      bool
	interrupting bool
	steer        string
	orphan       string
	costBase     float64
	cur          string
	blocks       map[string][]*claudeBlock
	tools        map[string]claudeTool
	denied       map[string]bool
	waits        map[string]*orchWait
}

func newClaudeDriver(base orchBase, th *OThread) *claudeDriver {
	return &claudeDriver{orchBase: base, nativeID: th.NativeID, closing: th.Status == "closed",
		blocks: map[string][]*claudeBlock{}, tools: map[string]claudeTool{}, denied: map[string]bool{}, waits: map[string]*orchWait{}}
}

func (d *claudeDriver) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d.m.rpcTimeout)
}

// send starts a turn, or writes the text into the running turn (the CLI folds it in).
func (d *claudeDriver) send(text string, imgs []orchImage) (string, error) {
	content, err := claudeUserContent(text, imgs)
	if err != nil {
		return "", orchBadRequest(err.Error())
	}
	d.mu.Lock()
	if d.closing {
		d.mu.Unlock()
		return "", orchConflict("thread is closed")
	}
	if d.runID != "" {
		proc, runID, live := d.proc, d.runID, d.turnLive
		if proc == nil || !live {
			d.mu.Unlock()
			return "", orchConflict("turn is starting; try again in a moment")
		}
		d.steer = text
		d.mu.Unlock()
		if err := proc.writeUser(content); err != nil {
			return "", orchConflict("steer failed: " + err.Error())
		}
		d.addUserItem(runID, text, imgs)
		return "steer", nil
	}
	runID := orchID("or_")
	d.runID, d.turnLive, d.interrupting, d.steer, d.orphan = runID, false, false, "", ""
	d.mu.Unlock()
	if err := d.beginRun(runID, text, imgs); err != nil {
		d.mu.Lock()
		d.runID = ""
		d.mu.Unlock()
		return "", err
	}
	go d.runTurn(runID, content)
	return "turn", nil
}

func (d *claudeDriver) runTurn(runID string, content any) {
	proc, err := d.ensureProcess()
	if err != nil {
		d.failRun(runID, err.Error())
		return
	}
	d.mu.Lock()
	mine := d.runID == runID && !d.closing
	d.mu.Unlock()
	if !mine {
		return // closed or ended while the process started: never send the prompt
	}
	if err := proc.writeUser(content); err != nil {
		if proc.alive() {
			d.failRun(runID, "write to claude failed: "+err.Error())
		}
		return // otherwise the exit handler records the failure with stderr
	}
	d.mu.Lock()
	if d.runID == runID {
		d.turnLive = true
	}
	d.mu.Unlock()
}

func (d *claudeDriver) failRun(runID, msg string) {
	d.mu.Lock()
	if d.runID != runID {
		d.mu.Unlock()
		return
	}
	d.runID, d.turnLive = "", false
	d.mu.Unlock()
	d.writeRunFailure(runID, msg, true)
}

func claudeArgs(th *OThread, resume bool, extra []string) []string {
	args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--include-partial-messages", "--permission-prompt-tool", "stdio"}
	if resume {
		args = append(args, "--resume", th.NativeID)
	} else {
		args = append(args, "--session-id", th.NativeID)
	}
	if th.Model != "" {
		args = append(args, "--model", th.Model)
	}
	readOnly := threadReadOnly(th)
	// An explicit mode, so the user's permissions.defaultMode never loosens a manual or read-only thread.
	mode := "default"
	if th.PermissionMode != "" && th.PermissionMode != "manual" && (!readOnly || th.PermissionMode == "plan") {
		mode = th.PermissionMode
	}
	args = append(args, "--permission-mode", mode)
	if readOnly {
		args = append(args, "--disallowedTools", "Edit,Write,MultiEdit,NotebookEdit")
	}
	if ask := claudeAskRules(th, mode, readOnly); len(ask) > 0 {
		// Ask rules beat user allow rules, so these calls reach agent-monitor's approval store.
		b, _ := json.Marshal(map[string]any{"permissions": map[string]any{"ask": ask}})
		args = append(args, "--settings", string(b))
	}
	if readOnly {
		args = append(args, "--strict-mcp-config")
	}
	return append(args, extra...)
}

// claudeAskRules lists the tools a manual or acceptEdits thread must ask for, whatever the user's allow rules say.
func claudeAskRules(th *OThread, mode string, readOnly bool) []string {
	if readOnly {
		return []string{"Bash"}
	}
	var ask []string
	switch mode {
	case "default":
		ask = []string{"Bash", "Write", "Edit", "MultiEdit", "NotebookEdit", "WebFetch", "WebSearch"}
	case "acceptEdits":
		ask = []string{"Bash", "WebFetch", "WebSearch"}
	default:
		return nil
	}
	// Permission rules accept no mcp__* wildcard, so mirror the user's MCP allow rules instead.
	return append(ask, userMCPAllowRules(th.Cwd)...)
}

var claudeMCPRuleRe = regexp.MustCompile(`^mcp__[\w-]+(?:__(?:[\w.-]+|\*))?$`)

// userMCPAllowRules reads the MCP allow rules from the user and project settings files.
func userMCPAllowRules(cwd string) []string {
	files := []string{filepath.Join(claudeConfigDir(), "settings.json"), filepath.Join(claudeConfigDir(), "settings.local.json")}
	if cwd != "" {
		files = append(files, filepath.Join(cwd, ".claude", "settings.json"), filepath.Join(cwd, ".claude", "settings.local.json"))
	}
	seen := map[string]bool{}
	var out []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var s struct {
			Permissions struct {
				Allow []string `json:"allow"`
			} `json:"permissions"`
		}
		if json.Unmarshal(b, &s) != nil {
			continue
		}
		for _, r := range s.Permissions.Allow {
			// agent-monitor's own server is replaced by the orchestrator's, whose delegate tools stay pre-allowed.
			if !claudeMCPRuleRe.MatchString(r) || r == "mcp__agent-monitor" || strings.HasPrefix(r, "mcp__agent-monitor__") || seen[r] {
				continue
			}
			seen[r] = true
			out = append(out, r)
		}
	}
	return out
}

// ensureProcess spawns claude for the thread and runs the initialize handshake.
func (d *claudeDriver) ensureProcess() (*claudeProc, error) {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	d.mu.Lock()
	if d.closing {
		d.mu.Unlock()
		return nil, errOrchClosed
	}
	if d.proc != nil && d.proc.alive() {
		p := d.proc
		d.mu.Unlock()
		return p, nil
	}
	d.mu.Unlock()

	th, err := d.m.store.Thread(d.id)
	if err != nil {
		return nil, err
	}
	if th.NativeID == "" {
		return nil, errors.New("thread has no claude session id")
	}
	cfg := d.m.claude
	resume := cfg.SessionExists != nil && cfg.SessionExists(th.NativeID)
	mcpArgs := d.m.claudeMCPArgs(th)
	cmd := cfg.Command(claudeArgs(th, resume, append(mcpArgs, cfg.ExtraArgs...)))
	cmd.Dir = th.Cwd
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = append(cmd.Env, "AGENT_MONITOR_DRIVEN="+d.id)
	cmd.Env = append(cmd.Env, d.m.orchProcEnv(th)...)
	if len(mcpArgs) > 0 {
		cmd.Env = append(cmd.Env, "MCP_TOOL_TIMEOUT="+strconv.Itoa(orchMCPToolTimeout*1000))
	}
	proc, err := startClaudeProc(cmd, d.onLine, d.onExit, d.m.trace)
	if err != nil {
		return nil, fmt.Errorf("start claude: %w", err)
	}
	fail := func(err error) (*claudeProc, error) {
		d.mu.Lock()
		if d.starting == proc {
			d.starting = nil
		}
		d.mu.Unlock()
		go proc.kill()
		<-proc.done
		if errors.Is(err, errClaudeExited) {
			return nil, errors.New(proc.exitSummary())
		}
		return nil, err
	}
	d.mu.Lock()
	closing := d.closing
	d.starting = proc
	d.mu.Unlock()
	if closing {
		return fail(errOrchClosed)
	}
	ctx, cancel := d.ctx()
	defer cancel()
	if _, err := proc.control(ctx, map[string]any{"subtype": "initialize"}); err != nil {
		return fail(err)
	}
	d.mu.Lock()
	if d.closing {
		d.mu.Unlock()
		return fail(errOrchClosed)
	}
	d.starting = nil
	d.proc, d.nativeID, d.costBase = proc, th.NativeID, 0
	d.blocks, d.tools, d.denied = map[string][]*claudeBlock{}, map[string]claudeTool{}, map[string]bool{}
	d.mu.Unlock()
	d.m.setNative(d.id, th.NativeID)
	_ = d.m.store.write(func(t *orchTx) error {
		th, err := t.thread(d.id)
		if err != nil {
			return err
		}
		if th.Status == "starting" || th.Status == "error" || th.Status == "idle" {
			th.Status = "running"
		}
		th.Error = ""
		d.putThreadTx(t, th)
		return nil
	})
	return proc, nil
}

func (d *claudeDriver) interrupt() error {
	d.mu.Lock()
	proc, runID := d.proc, d.runID
	if proc == nil || runID == "" {
		d.mu.Unlock()
		return orchConflict("no running turn")
	}
	d.interrupting = true
	d.mu.Unlock()
	ctx, cancel := d.ctx()
	defer cancel()
	if _, err := proc.control(ctx, map[string]any{"subtype": "interrupt"}); err != nil {
		return orchConflict("interrupt failed: " + err.Error())
	}
	return nil
}

func (d *claudeDriver) takeWaits(runID string) []*orchWait {
	var out []*orchWait
	for k, w := range d.waits {
		if runID == "" || w.runID == runID {
			out = append(out, w)
			delete(d.waits, k)
		}
	}
	return out
}

func (d *claudeDriver) close() {
	d.mu.Lock()
	d.closing = true
	proc, starting, runID := d.proc, d.starting, d.runID
	d.runID, d.turnLive = "", false
	waits := d.takeWaits("")
	d.mu.Unlock()
	for _, w := range waits {
		w.cancel()
	}
	d.writeClosed(runID)
	for _, p := range []*claudeProc{proc, starting} {
		if p != nil {
			p.kill()
		}
	}
}

func (d *claudeDriver) shutdown() {
	d.mu.Lock()
	p, st := d.proc, d.starting
	d.mu.Unlock()
	for _, x := range []*claudeProc{p, st} {
		if x != nil {
			x.killFast()
		}
	}
}

func (d *claudeDriver) live() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return !d.closing && d.runID != "" && d.proc != nil && d.proc.alive()
}

func (d *claudeDriver) procAlive() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.proc != nil && d.proc.alive()
}

func (d *claudeDriver) onExit(p *claudeProc) {
	d.mu.Lock()
	if d.proc != p {
		d.mu.Unlock()
		return
	}
	defer d.m.noteProcEnd(d.id)
	d.proc = nil
	closing, runID := d.closing, d.runID
	d.runID, d.turnLive = "", false
	waits := d.takeWaits("")
	d.mu.Unlock()
	for _, w := range waits {
		w.cancel()
	}
	if !closing && !d.m.stopping.Load() {
		d.writeExit(runID, p.exitSummary(), p.exitErr != nil)
	}
}

// activeRun returns the running run; output after a run ended (a steer the CLI queued) opens a new one.
func (d *claudeDriver) activeRun() string {
	d.mu.Lock()
	if d.runID != "" || d.orphan == "" || d.closing {
		r := d.runID
		d.mu.Unlock()
		return r
	}
	text := d.orphan
	runID := orchID("or_")
	d.runID, d.turnLive, d.orphan, d.steer = runID, true, "", ""
	d.mu.Unlock()
	if err := d.beginRun(runID, text, nil); err != nil {
		log.Printf("orch: claude adopt run: %v", err)
	}
	return runID
}

// ── stream mapping ──

type claudeLine struct {
	Type            string          `json:"type"`
	Subtype         string          `json:"subtype"`
	RequestID       string          `json:"request_id"`
	Request         json.RawMessage `json:"request"`
	Event           json.RawMessage `json:"event"`
	Message         json.RawMessage `json:"message"`
	ParentToolUseID *string         `json:"parent_tool_use_id"`
	SessionID       string          `json:"session_id"`
	Model           string          `json:"model"`
	PermissionMode  string          `json:"permissionMode"`
	ToolUseResult   json.RawMessage `json:"tool_use_result"`
	IsError         bool            `json:"is_error"`
	Result          string          `json:"result"`
	TerminalReason  string          `json:"terminal_reason"`
	TotalCostUSD    float64         `json:"total_cost_usd"`
	Usage           *claudeUsage    `json:"usage"`
	ModelUsage      map[string]struct {
		CostUSD float64 `json:"costUSD"`
	} `json:"model_usage_since_last_result"`
	Errors []string `json:"errors"`
}

type claudeUsage struct {
	InputTokens         int64 `json:"input_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadTokens     int64 `json:"cache_read_input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
}

type claudeContent struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     map[string]any  `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

func (d *claudeDriver) onLine(p *claudeProc, line []byte) {
	var l claudeLine
	if err := json.Unmarshal(line, &l); err != nil {
		log.Printf("orch: bad line from claude: %.200s", line)
		return
	}
	switch l.Type {
	case "control_request":
		// The wait exists before any control_cancel_request for it can be read.
		ctx, cancel := context.WithCancel(context.Background())
		d.mu.Lock()
		d.waits[l.RequestID] = &orchWait{cancel: cancel, runID: d.runID}
		d.mu.Unlock()
		go d.onControl(ctx, cancel, p, l.RequestID, l.Request)
	case "control_cancel_request":
		d.mu.Lock()
		w := d.waits[l.RequestID]
		d.mu.Unlock()
		if w != nil {
			w.cancel()
		}
	case "system":
		d.onSystem(&l)
	case "stream_event":
		if l.ParentToolUseID == nil {
			d.onStream(l.Event)
		}
	case "assistant":
		if l.ParentToolUseID == nil {
			d.onAssistant(l.Message)
		}
	case "user":
		if l.ParentToolUseID == nil {
			d.onUser(l.Message, l.ToolUseResult)
		}
	case "result":
		d.onResult(&l)
	}
}

func (d *claudeDriver) onSystem(l *claudeLine) {
	model, mode := "", l.PermissionMode
	switch l.Subtype {
	case "init":
		model = l.Model
		mode = ""
	case "status":
	default:
		return
	}
	if model == "" && mode == "" {
		return
	}
	if mode == "default" {
		mode = "manual"
	}
	_ = d.m.store.write(func(t *orchTx) error {
		th, err := t.thread(d.id)
		if err != nil {
			return nil
		}
		if (model == "" || th.Model == model) && (mode == "" || th.PermissionMode == mode) {
			return nil
		}
		if model != "" && th.Model == "" {
			th.Model = model
		}
		if mode != "" {
			th.PermissionMode = mode
		}
		d.putThreadTx(t, th)
		return nil
	})
}

func (d *claudeDriver) block(msgID string, index int) *claudeBlock {
	for _, b := range d.blocks[msgID] {
		if b.index == index {
			return b
		}
	}
	return nil
}

func (d *claudeDriver) startItem(it *OItem) {
	it.Status, it.StartedAt = "started", time.Now().UnixMilli()
	_ = d.m.store.write(func(t *orchTx) error {
		if err := t.putItem(it); err != nil {
			return err
		}
		t.emit(d.id, "item.started", map[string]any{"item": it})
		return nil
	})
}

func (d *claudeDriver) onStream(raw json.RawMessage) {
	var e struct {
		Type    string `json:"type"`
		Index   int    `json:"index"`
		Message struct {
			ID string `json:"id"`
		} `json:"message"`
		ContentBlock struct {
			Type string `json:"type"`
		} `json:"content_block"`
		Delta struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"delta"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return
	}
	switch e.Type {
	case "message_start":
		d.mu.Lock()
		d.cur = e.Message.ID
		d.mu.Unlock()
	case "content_block_start":
		runID := d.activeRun()
		if runID == "" {
			return
		}
		d.mu.Lock()
		b := &claudeBlock{index: e.Index, kind: e.ContentBlock.Type}
		if b.kind == "text" || b.kind == "thinking" {
			b.itemID = fmt.Sprintf("%s.%s.%d", runID, d.cur, e.Index)
		}
		d.blocks[d.cur] = append(d.blocks[d.cur], b)
		d.mu.Unlock()
		if b.kind == "text" {
			d.startItem(&OItem{ID: b.itemID, ThreadID: d.id, RunID: runID, Kind: "agent"})
		}
	case "content_block_delta":
		runID := d.activeRun()
		d.mu.Lock()
		b := d.block(d.cur, e.Index)
		d.mu.Unlock()
		if b == nil || b.itemID == "" || runID == "" {
			return
		}
		switch e.Delta.Type {
		case "text_delta":
			d.m.store.AppendDelta(d.id, b.itemID, "text", e.Delta.Text)
		case "thinking_delta":
			if e.Delta.Thinking == "" {
				return
			}
			if _, err := d.itemSnapshot(b.itemID); err != nil {
				d.startItem(&OItem{ID: b.itemID, ThreadID: d.id, RunID: runID, Kind: "reasoning"})
			}
			d.m.store.AppendDelta(d.id, b.itemID, "text", e.Delta.Thinking)
		}
	}
}

// completeItem flushes an item's deltas and stores its final form; text replaces the streamed text when set.
func (d *claudeDriver) completeItem(it *OItem, status string) {
	d.m.store.flushDeltas(it.ID)
	_ = d.m.store.write(func(t *orchTx) error {
		if prev, err := t.item(it.ID); err == nil {
			it.StartedAt = prev.StartedAt
			if it.Text == "" {
				it.Text = prev.Text
			}
		} else {
			it.StartedAt = t.now
			t.emit(d.id, "item.started", map[string]any{"item": it})
		}
		it.Status, it.EndedAt = status, t.now
		if err := t.putItem(it); err != nil {
			return err
		}
		t.emit(d.id, "item.completed", map[string]any{"item": it})
		return nil
	})
}

func (d *claudeDriver) onAssistant(raw json.RawMessage) {
	var msg struct {
		ID      string          `json:"id"`
		Content []claudeContent `json:"content"`
	}
	if json.Unmarshal(raw, &msg) != nil {
		return
	}
	runID := d.activeRun()
	if runID == "" {
		return
	}
	th, _ := d.m.store.Thread(d.id)
	cwd := ""
	if th != nil {
		cwd = th.Cwd
	}
	for i := range msg.Content {
		c := &msg.Content[i]
		d.mu.Lock()
		var b *claudeBlock
		for _, x := range d.blocks[msg.ID] {
			if !x.consumed && x.kind == c.Type {
				b, x.consumed = x, true
				break
			}
		}
		d.mu.Unlock()
		itemID := ""
		if b != nil {
			itemID = b.itemID
		}
		if itemID == "" {
			itemID = fmt.Sprintf("%s.%s.c%d", runID, msg.ID, i)
		}
		switch c.Type {
		case "text":
			d.completeItem(&OItem{ID: itemID, ThreadID: d.id, RunID: runID, Kind: "agent", Text: c.Text}, "completed")
		case "thinking":
			if _, err := d.itemSnapshot(itemID); err == nil || c.Thinking != "" {
				d.completeItem(&OItem{ID: itemID, ThreadID: d.id, RunID: runID, Kind: "reasoning", Text: c.Thinking}, "completed")
			}
		case "tool_use":
			it := claudeToolItem(c.Name, c.Input, cwd)
			it.ID, it.ThreadID, it.RunID = runID+"."+c.ID, d.id, runID
			d.mu.Lock()
			d.tools[c.ID] = claudeTool{itemID: it.ID, name: c.Name}
			d.mu.Unlock()
			d.startItem(it)
		}
	}
}

var claudeExitCode = regexp.MustCompile(`^Exit code (\d+)`)

func claudeResultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &parts)
	var out []string
	for _, p := range parts {
		if p.Text != "" {
			out = append(out, p.Text)
		}
	}
	return strings.Join(out, "\n")
}

func (d *claudeDriver) onUser(raw, toolUseResult json.RawMessage) {
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &msg) != nil {
		return
	}
	var blocks []claudeContent
	if json.Unmarshal(msg.Content, &blocks) != nil {
		return
	}
	var res struct {
		Stdout      string `json:"stdout"`
		Stderr      string `json:"stderr"`
		Interrupted bool   `json:"interrupted"`
	}
	_ = json.Unmarshal(toolUseResult, &res)
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		d.mu.Lock()
		tool, ok := d.tools[b.ToolUseID]
		denied := d.denied[b.ToolUseID]
		d.mu.Unlock()
		if !ok {
			continue
		}
		prev, err := d.itemSnapshot(tool.itemID)
		if err != nil {
			continue
		}
		it := *prev
		it.Output = claudeResultText(b.Content)
		status := "completed"
		switch {
		case denied:
			status = "declined"
		case b.IsError || res.Interrupted:
			status = "failed"
		}
		if it.Kind == "command" && status != "declined" {
			code := 0
			if b.IsError {
				code = -1
				if m := claudeExitCode.FindStringSubmatch(it.Output); m != nil {
					code, _ = strconv.Atoi(m[1])
				}
			}
			if code >= 0 {
				it.ExitCode = &code
			}
		}
		d.completeItem(&it, status)
	}
}

func (d *claudeDriver) onResult(l *claudeLine) {
	d.mu.Lock()
	runID := d.runID
	if runID == "" {
		d.mu.Unlock()
		return
	}
	interrupting, steer := d.interrupting, d.steer
	d.runID, d.turnLive, d.interrupting, d.steer = "", false, false, ""
	if steer != "" {
		d.orphan = steer
	}
	cost := 0.0
	for _, u := range l.ModelUsage {
		cost += u.CostUSD
	}
	if len(l.ModelUsage) == 0 && l.TotalCostUSD > 0 {
		cost = max(0, l.TotalCostUSD-d.costBase)
	}
	if l.TotalCostUSD > 0 {
		d.costBase = l.TotalCostUSD
	}
	waits := d.takeWaits(runID)
	d.mu.Unlock()
	for _, w := range waits {
		w.cancel()
	}
	status, errText := "completed", ""
	switch {
	case l.Subtype == "success" && !l.IsError:
	case interrupting || strings.Contains(l.TerminalReason, "abort"):
		status = "interrupted"
	default:
		status = "failed"
		errText = firstOf(l.Result, strings.Join(l.Errors, "\n"), l.Subtype, "claude reported an error")
	}
	var usage *OUsage
	if l.Usage != nil {
		usage = &OUsage{InTokens: l.Usage.InputTokens + l.Usage.CacheCreationTokens + l.Usage.CacheReadTokens, OutTokens: l.Usage.OutputTokens, CostUSD: cost}
	}
	d.writeRunEnd(runID, status, errText, usage)
}

// ── tool mapping ──

func claudeLines(s string) int {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

func claudeStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// claudeToolItem maps a tool_use block to an item kind and its display fields.
func claudeToolItem(name string, in map[string]any, cwd string) *OItem {
	it := &OItem{Kind: "other", Command: name}
	switch {
	case name == "Bash":
		it.Kind, it.Command, it.Cwd, it.Text = "command", claudeStr(in, "command"), cwd, claudeStr(in, "description")
	case name == "Edit" || name == "Write" || name == "MultiEdit" || name == "NotebookEdit":
		it.Kind, it.Command = "fileChange", ""
		path := claudeStr(in, "file_path", "notebook_path")
		f := OFile{Path: path, Kind: "update"}
		switch name {
		case "Write":
			if _, err := os.Stat(path); err != nil {
				f.Kind = "add"
			}
			f.Add = claudeLines(claudeStr(in, "content"))
		case "Edit":
			f.Add, f.Del = claudeLines(claudeStr(in, "new_string")), claudeLines(claudeStr(in, "old_string"))
		case "MultiEdit":
			edits, _ := in["edits"].([]any)
			for _, e := range edits {
				if m, ok := e.(map[string]any); ok {
					f.Add += claudeLines(claudeStr(m, "new_string"))
					f.Del += claudeLines(claudeStr(m, "old_string"))
				}
			}
		case "NotebookEdit":
			f.Add = claudeLines(claudeStr(in, "new_source"))
		}
		it.Files, it.Text = []OFile{f}, path
	case strings.HasPrefix(name, "mcp__"):
		it.Kind, it.Text = "mcp", claudeSummary(in)
	case name == "WebFetch" || name == "WebSearch":
		it.Kind, it.Text = "web", claudeStr(in, "url", "query")
	case name == "TodoWrite":
		b, _ := json.Marshal(in)
		it.Kind, it.Command, it.Text = "plan", "", string(b)
	case name == "REPL": // a Code Mode script; its tool calls link to this item by ParentID
		it.Text = firstOf(claudeStr(in, "description"), firstLine(claudeStr(in, "code")))
	case name == "Task" || name == "Agent":
		it.Text = claudeStr(in, "description", "prompt")
	case name == "AskUserQuestion":
		if qs, ok := in["questions"].([]any); ok && len(qs) > 0 {
			if q, ok := qs[0].(map[string]any); ok {
				it.Text = claudeStr(q, "question")
			}
		}
	case name == "ExitPlanMode":
		it.Text = firstLine(claudeStr(in, "plan"))
	default:
		it.Text = claudeSummary(in)
	}
	it.Text = clipString(it.Text, 2000)
	return it
}

func claudeSummary(in map[string]any) string {
	if s := claudeStr(in, "file_path", "path", "pattern", "command", "url", "query", "description", "prompt"); s != "" {
		return s
	}
	for _, v := range in {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	if len(in) == 0 {
		return ""
	}
	b, _ := json.Marshal(in)
	return string(b)
}

// ── permission prompts ──

func claudeRequestKind(tool string) string {
	switch tool {
	case "Bash":
		return "command"
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		return "fileChange"
	case "AskUserQuestion":
		return "question"
	case "ExitPlanMode":
		return "plan"
	}
	return "permissions"
}

func (d *claudeDriver) onControl(ctx context.Context, cancel context.CancelFunc, p *claudeProc, reqID string, raw json.RawMessage) {
	defer func() {
		cancel()
		d.mu.Lock()
		delete(d.waits, reqID)
		d.mu.Unlock()
	}()
	var r struct {
		Subtype     string         `json:"subtype"`
		ToolName    string         `json:"tool_name"`
		Input       map[string]any `json:"input"`
		Suggestions []any          `json:"permission_suggestions"`
		ToolUseID   string         `json:"tool_use_id"`
	}
	_ = json.Unmarshal(raw, &r)
	if r.Subtype != "can_use_tool" {
		_ = p.respondError(reqID, "agent-monitor does not support "+r.Subtype)
		return
	}
	if r.Input == nil {
		r.Input = map[string]any{}
	}
	th, err := d.m.store.Thread(d.id)
	if err != nil {
		_ = p.respond(reqID, map[string]any{"behavior": "deny", "message": "thread not found"})
		return
	}
	d.mu.Lock()
	runID := d.runID
	itemID := d.tools[r.ToolUseID].itemID
	d.mu.Unlock()
	if ctx.Err() != nil {
		return
	}
	a := buildApproval(hookClaudePermission, hookPayload{SessionID: th.NativeID, Cwd: th.Cwd, ToolName: r.ToolName, ToolInput: r.Input, PermissionSuggestions: r.Suggestions})
	if threadReadOnly(th) && r.ToolName == "Bash" && readOnlyBash(claudeStr(r.Input, "command"), th.Cwd) {
		a.DrivenID, a.RootID = d.id, d.m.store.RootOf(d.id)
		a.State, a.DecidedBy, a.Message = "auto-allowed", "read-only allowlist", "read-only command"
		if d.m.approvals != nil {
			d.m.approvals.recordFinal(a)
		}
		_ = p.respond(reqID, map[string]any{"behavior": "allow", "updatedInput": r.Input})
		return
	}

	dec, outcome := d.awaitApproval(ctx, a, runID, itemID, claudeRequestKind(r.ToolName), func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.runID != ""
	})
	if outcome == "canceled" {
		return
	}
	resp := claudePermissionResponse(a, r.Input, dec)
	if resp["behavior"] == "deny" && r.ToolUseID != "" {
		d.mu.Lock()
		d.denied[r.ToolUseID] = true
		d.mu.Unlock()
	}
	_ = p.respond(reqID, resp)
}

// claudePermissionResponse renders a decision as the can_use_tool result the CLI reads.
func claudePermissionResponse(a *Approval, input map[string]any, dec approvalDecision) map[string]any {
	if dec.Behavior != "allow" {
		out := map[string]any{"behavior": "deny", "message": firstOf(dec.Message, "Denied in agent-monitor")}
		if dec.Interrupt {
			out["interrupt"] = true
		}
		return out
	}
	upd := dec.UpdatedInput
	if upd == nil {
		upd = map[string]any{}
		for k, v := range input {
			upd[k] = v
		}
	}
	if a.Kind == "question" {
		if _, ok := upd["questions"]; !ok {
			upd["questions"] = input["questions"]
		}
		upd["answers"] = claudeAnswers(a.Questions, dec.Answers)
	}
	out := map[string]any{"behavior": "allow", "updatedInput": upd}
	if dec.Always && dec.AlwaysScope == "claude" && len(a.Suggestions) > 0 {
		out["updatedPermissions"] = a.Suggestions
	}
	return out
}

// claudeAnswers keys each answer by its question text, accepting header or index keys too.
func claudeAnswers(qs []map[string]any, in map[string]string) map[string]string {
	out := map[string]string{}
	for i, q := range qs {
		text := claudeStr(q, "question")
		for _, k := range []string{text, claudeStr(q, "header"), strconv.Itoa(i)} {
			if v, ok := in[k]; ok && k != "" {
				out[text] = v
				break
			}
		}
	}
	if len(out) == 0 {
		for k, v := range in {
			out[k] = v
		}
	}
	return out
}
