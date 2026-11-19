package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ACP (Agent Client Protocol): JSON-RPC 2.0 over the agent's stdio, one session per driven thread.

const (
	acpProtocolVersion = 1
	acpReadCap         = 8 << 20
)

type acpPermOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

// acpToolCall is a ToolCall or a partial ToolCallUpdate; nil fields leave the old value.
type acpToolCall struct {
	ToolCallID string            `json:"toolCallId"`
	Title      *string           `json:"title"`
	Kind       *string           `json:"kind"`
	Status     *string           `json:"status"`
	Content    []json.RawMessage `json:"content"`
	Locations  []struct {
		Path string `json:"path"`
	} `json:"locations"`
	RawInput  json.RawMessage `json:"rawInput"`
	RawOutput json.RawMessage `json:"rawOutput"`
}

type acpTool struct {
	itemID    string
	title     string
	kind      string
	status    string
	content   []json.RawMessage
	locations []string
	rawInput  map[string]any
	rawOutput json.RawMessage
}

func (t *acpTool) merge(u *acpToolCall) {
	if u.Title != nil {
		t.title = *u.Title
	}
	if u.Kind != nil {
		t.kind = *u.Kind
	}
	if u.Status != nil {
		t.status = *u.Status
	}
	if u.Content != nil {
		t.content = u.Content
	}
	if u.Locations != nil {
		t.locations = t.locations[:0]
		for _, l := range u.Locations {
			t.locations = append(t.locations, l.Path)
		}
	}
	if len(u.RawInput) > 0 && string(u.RawInput) != "null" {
		var m map[string]any
		if json.Unmarshal(u.RawInput, &m) == nil {
			t.rawInput = m
		}
	}
	if len(u.RawOutput) > 0 && string(u.RawOutput) != "null" {
		t.rawOutput = u.RawOutput
	}
}

type acpDriver struct {
	orchBase
	opMu sync.Mutex

	mu          sync.Mutex
	conn        *codexConn
	sessionID   string
	runID       string
	closing     bool
	loading     bool
	stopPrompt  context.CancelFunc
	cancelTimer *time.Timer
	tools       map[string]*acpTool
	chunkID     string // open agent or reasoning item
	chunkKind   string
	chunkN      int
	planItem    string
	sawOutput   bool
	costBase    float64
	costLast    float64
	hasCost     bool
	waits       map[string]*orchWait
	lastActive  time.Time
}

func newACPDriver(base orchBase, th *OThread) *acpDriver {
	return &acpDriver{orchBase: base, sessionID: th.NativeID, closing: th.Status == "closed",
		tools: map[string]*acpTool{}, waits: map[string]*orchWait{}}
}

// acpThreadOptions validates the create fields an ACP thread uses.
func acpThreadOptions(th *OThread, in orchCreate) error {
	switch in.PermissionMode {
	case "":
		in.PermissionMode = "manual"
	case "manual", "acceptEdits", "plan":
	default:
		return orchBadRequest("permissionMode must be manual, acceptEdits or plan for an ACP agent")
	}
	th.PermissionMode = in.PermissionMode
	return nil
}

func (d *acpDriver) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d.m.rpcTimeout)
}

func (d *acpDriver) send(text string, imgs []orchImage) (string, error) {
	if pics, _ := splitUploads(imgs); len(pics) > 0 {
		if err := d.checkImages(); err != nil {
			return "", err
		}
	}
	d.mu.Lock()
	if d.closing {
		d.mu.Unlock()
		return "", orchConflict("thread is closed")
	}
	if d.runID != "" {
		d.mu.Unlock()
		return "", orchConflict("this agent cannot take a message while a turn runs: interrupt it first")
	}
	runID := orchID("or_")
	d.runID, d.chunkID, d.chunkKind, d.planItem, d.sawOutput = runID, "", "", "", false
	d.tools = map[string]*acpTool{}
	d.costBase = d.costLast
	d.mu.Unlock()
	if err := d.beginRun(runID, text, imgs); err != nil {
		d.mu.Lock()
		d.runID = ""
		d.mu.Unlock()
		return "", err
	}
	go d.runTurn(runID, text, imgs)
	return "turn", nil
}

func (d *acpDriver) runTurn(runID, text string, imgs []orchImage) {
	conn, sid, err := d.ensureProcess(runID)
	if err != nil {
		d.mu.Lock()
		mine := d.runID == runID
		if mine {
			d.runID = ""
		}
		d.mu.Unlock()
		if mine {
			d.writeRunFailure(runID, err.Error(), true)
		}
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.mu.Lock()
	if d.runID != runID {
		d.mu.Unlock()
		cancel()
		return
	}
	d.stopPrompt = cancel
	d.lastActive = time.Now()
	d.mu.Unlock()
	defer cancel()
	go d.watchSilence(runID, ctx)
	prompt, err := acpPrompt(text, imgs)
	if err != nil {
		d.endRun(runID, "failed", err.Error(), nil)
		return
	}
	res, err := conn.call(ctx, "session/prompt", map[string]any{"sessionId": sid, "prompt": prompt})
	if errors.Is(err, errCodexExited) || ctx.Err() != nil {
		return // the exit handler or the cancel timer ends the run
	}
	if err != nil {
		d.endRun(runID, "failed", "session/prompt failed: "+acpErrText(err), nil)
		return
	}
	var r struct {
		StopReason string `json:"stopReason"`
		Usage      *struct {
			InputTokens       int64 `json:"inputTokens"`
			OutputTokens      int64 `json:"outputTokens"`
			ThoughtTokens     int64 `json:"thoughtTokens"`
			CachedReadTokens  int64 `json:"cachedReadTokens"`
			CachedWriteTokens int64 `json:"cachedWriteTokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(res, &r)
	var usage *OUsage
	if r.Usage != nil {
		usage = &OUsage{InTokens: r.Usage.InputTokens + r.Usage.CachedReadTokens + r.Usage.CachedWriteTokens, OutTokens: r.Usage.OutputTokens + r.Usage.ThoughtTokens}
	}
	status, note := "completed", ""
	switch r.StopReason {
	case "cancelled":
		status = "interrupted"
	case "max_tokens":
		note = "The agent stopped: max tokens reached."
	case "max_turn_requests":
		note = "The agent stopped: max model requests in one turn reached."
	case "refusal":
		note = "The agent refused to continue."
	}
	d.mu.Lock()
	quiet := !d.sawOutput && len(d.tools) == 0
	d.mu.Unlock()
	if status == "completed" && note == "" && quiet {
		note = "The agent ended the turn without any output. Check that it is logged in and that its model works."
	}
	d.endRun(runID, status, note, usage)
}

func (d *acpDriver) touch() {
	d.mu.Lock()
	d.lastActive = time.Now()
	d.mu.Unlock()
}

// watchSilence interrupts the run when the agent sends nothing for acpTurnTimeoutMin; open approvals pause it.
func (d *acpDriver) watchSilence(runID string, ctx context.Context) {
	limit := d.m.acpTurnTimeoutOr()
	tick := time.NewTicker(min(max(limit/4, 10*time.Millisecond), 30*time.Second))
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		d.mu.Lock()
		mine, waiting := d.runID == runID, len(d.waits) > 0
		if waiting {
			d.lastActive = time.Now()
		}
		silent := time.Since(d.lastActive)
		d.mu.Unlock()
		if !mine {
			return
		}
		if silent >= limit {
			d.note(runID, fmt.Sprintf("No output from the agent for %s: the turn was interrupted (setting acpTurnTimeoutMin).", limit.Round(time.Second)))
			_ = d.interrupt()
			return
		}
	}
}

func acpErrText(err error) string {
	var re *rpcError
	if errors.As(err, &re) {
		return fmt.Sprintf("%s (%d)", re.Message, re.Code)
	}
	return err.Error()
}

// endRun closes the run once: open text items complete, tool items left open fail.
func (d *acpDriver) endRun(runID, status, errText string, usage *OUsage) {
	d.mu.Lock()
	if d.runID != runID {
		d.mu.Unlock()
		return
	}
	d.runID = ""
	if d.cancelTimer != nil {
		d.cancelTimer.Stop()
		d.cancelTimer = nil
	}
	d.stopPrompt = nil
	chunk := d.chunkID
	d.chunkID, d.chunkKind = "", ""
	waits := d.takeWaits(runID)
	if d.hasCost {
		if usage == nil {
			usage = &OUsage{}
		}
		usage.CostUSD = max(0, d.costLast-d.costBase)
	}
	d.mu.Unlock()
	for _, w := range waits {
		w.cancel()
	}
	d.completeItem(chunk)
	d.writeRunEnd(runID, status, errText, usage)
}

func (d *acpDriver) takeWaits(runID string) []*orchWait {
	var out []*orchWait
	for k, w := range d.waits {
		if runID == "" || w.runID == runID {
			out = append(out, w)
			delete(d.waits, k)
		}
	}
	return out
}

func (d *acpDriver) completeItem(id string) {
	if id == "" {
		return
	}
	d.m.store.flushDeltas(id)
	_ = d.m.store.write(func(t *orchTx) error {
		it, err := t.item(id)
		if err != nil || it.Status != "started" {
			return nil
		}
		it.Status, it.EndedAt = "completed", t.now
		if err := t.putItem(it); err != nil {
			return err
		}
		t.emit(d.id, "item.completed", map[string]any{"item": it})
		return nil
	})
}

// note records a short "other" item in a run.
func (d *acpDriver) note(runID, text string) {
	if runID == "" {
		return
	}
	now := time.Now().UnixMilli()
	it := &OItem{ID: orchID(runID + ".n"), ThreadID: d.id, RunID: runID, Kind: "other", Status: "completed", Text: text, StartedAt: now, EndedAt: now}
	_ = d.m.store.write(func(t *orchTx) error { return d.emitItem(t, it) })
}

// acpMCPServers passes agent-monitor's MCP server (delegate tools) to session/new and session/load.
func (m *OrchManager) acpMCPServers(th *OThread) []any {
	srv := m.mcpServer()
	if srv == nil {
		return []any{}
	}
	env := []any{}
	for _, kv := range append(append([]string{}, srv.Env...), m.orchProcEnv(th)...) {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env = append(env, map[string]any{"name": k, "value": v})
		}
	}
	args := srv.Args
	if args == nil {
		args = []string{}
	}
	return []any{map[string]any{"name": "agent-monitor", "command": srv.Command, "args": args, "env": env}}
}

type acpSessionResult struct {
	SessionID string `json:"sessionId"`
	Models    *struct {
		CurrentModelID  string `json:"currentModelId"`
		AvailableModels []struct {
			ModelID string `json:"modelId"`
			Name    string `json:"name"`
		} `json:"availableModels"`
	} `json:"models"`
	Modes *struct {
		CurrentModeID  string `json:"currentModeId"`
		AvailableModes []struct {
			ID string `json:"id"`
		} `json:"availableModes"`
	} `json:"modes"`
}

// ensureProcess spawns the agent, runs initialize and loads, resumes or creates the session.
func (d *acpDriver) ensureProcess(runID string) (*codexConn, string, error) {
	d.opMu.Lock()
	defer d.opMu.Unlock()
	d.mu.Lock()
	if d.closing {
		d.mu.Unlock()
		return nil, "", errOrchClosed
	}
	if d.conn != nil && d.conn.alive() && d.sessionID != "" {
		c, s := d.conn, d.sessionID
		d.mu.Unlock()
		return c, s, nil
	}
	d.mu.Unlock()

	th, err := d.m.store.Thread(d.id)
	if err != nil {
		return nil, "", err
	}
	agent, ok := d.m.acpAgent(th.Agent)
	if !ok {
		return nil, "", fmt.Errorf("agent %q is not in the registry", th.Agent)
	}
	cmd, err := d.m.acpCommand(agent, th)
	if err != nil {
		return nil, "", err
	}
	cmd.Dir = th.Cwd
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	for k, v := range agent.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Env = append(cmd.Env, "AGENT_MONITOR_DRIVEN="+d.id)
	cmd.Env = append(cmd.Env, d.m.orchProcEnv(th)...)
	conn, err := startRPCConn(cmd, codexHandlers{notify: d.onNotify, request: d.onRequest, exit: d.onExit, trace: d.m.trace}, agent.Name, true)
	if err != nil {
		return nil, "", fmt.Errorf("start %s: %w", agent.Name, err)
	}
	d.mu.Lock()
	closing := d.closing
	if !closing {
		d.conn = conn
	}
	d.mu.Unlock()
	fail := func(err error) (*codexConn, string, error) {
		d.mu.Lock()
		if d.conn == conn {
			d.conn = nil
		}
		d.mu.Unlock()
		go conn.kill()
		<-conn.done
		if errors.Is(err, errCodexExited) {
			return nil, "", errors.New(conn.exitSummary())
		}
		return nil, "", err
	}
	if closing {
		return fail(errOrchClosed)
	}
	ctx, cancel := d.ctx()
	defer cancel()
	res, err := conn.call(ctx, "initialize", map[string]any{
		"protocolVersion":    acpProtocolVersion,
		"clientCapabilities": map[string]any{"fs": map[string]any{"readTextFile": true, "writeTextFile": true}, "terminal": false},
		"clientInfo":         map[string]any{"name": "agent-monitor", "title": "agent-monitor", "version": "0.1.0"},
	})
	if err != nil {
		return fail(fmt.Errorf("initialize: %s", acpErrText(err)))
	}
	var init struct {
		ProtocolVersion   int `json:"protocolVersion"`
		AgentCapabilities struct {
			LoadSession         bool                       `json:"loadSession"`
			SessionCapabilities map[string]json.RawMessage `json:"sessionCapabilities"`
			PromptCapabilities  struct {
				Image bool `json:"image"`
			} `json:"promptCapabilities"`
		} `json:"agentCapabilities"`
	}
	_ = json.Unmarshal(res, &init)
	d.m.agents.setImages(agent.ID, init.AgentCapabilities.PromptCapabilities.Image)
	if init.ProtocolVersion != 0 && init.ProtocolVersion != acpProtocolVersion {
		return fail(fmt.Errorf("%s speaks ACP protocol version %d; agent-monitor speaks %d", agent.Name, init.ProtocolVersion, acpProtocolVersion))
	}
	mcp := d.m.acpMCPServers(th)
	var sr acpSessionResult
	sid := ""
	if th.NativeID != "" {
		method := ""
		switch {
		case init.AgentCapabilities.LoadSession:
			method = "session/load"
		case init.AgentCapabilities.SessionCapabilities["resume"] != nil:
			method = "session/resume"
		}
		if method != "" {
			d.mu.Lock()
			d.loading = true
			d.mu.Unlock()
			res, err := conn.call(ctx, method, map[string]any{"sessionId": th.NativeID, "cwd": th.Cwd, "mcpServers": mcp})
			d.mu.Lock()
			d.loading = false
			d.mu.Unlock()
			if errors.Is(err, errCodexExited) {
				return fail(err)
			}
			if err == nil {
				_ = json.Unmarshal(res, &sr)
				sid = th.NativeID
			} else {
				d.note(runID, fmt.Sprintf("%s could not load the earlier session (%s): this run starts a new session, and the agent does not see the earlier messages.", agent.Name, acpErrText(err)))
			}
		} else {
			d.note(runID, agent.Name+" cannot load an earlier session: this run starts a new session, and the agent does not see the earlier messages.")
		}
	}
	if sid == "" {
		res, err := conn.call(ctx, "session/new", map[string]any{"cwd": th.Cwd, "mcpServers": mcp})
		if err != nil {
			return fail(fmt.Errorf("session/new: %s", acpErrText(err)))
		}
		_ = json.Unmarshal(res, &sr)
		if sr.SessionID == "" {
			return fail(errors.New("session/new returned no sessionId"))
		}
		sid = sr.SessionID
	}
	model := ""
	if sr.Models != nil {
		var ms []any
		for _, mm := range sr.Models.AvailableModels {
			ms = append(ms, map[string]any{"id": mm.ModelID, "name": mm.Name})
		}
		d.m.agents.setModels(agent.ID, ms)
		model = sr.Models.CurrentModelID
	}
	if th.Model != "" && th.Model != model {
		if _, err := conn.call(ctx, "session/set_model", map[string]any{"sessionId": sid, "modelId": th.Model}); err != nil {
			d.note(runID, "Model "+th.Model+" not set: "+acpErrText(err))
		} else {
			model = th.Model
		}
	}
	if th.PermissionMode == "plan" || threadReadOnly(th) {
		hasPlan := false
		if sr.Modes != nil {
			for _, md := range sr.Modes.AvailableModes {
				hasPlan = hasPlan || md.ID == "plan"
			}
		}
		var planErr error
		switch {
		case !hasPlan:
			planErr = fmt.Errorf("%s offers no plan mode", agent.Name)
		case sr.Modes.CurrentModeID != "plan":
			if _, err := conn.call(ctx, "session/set_mode", map[string]any{"sessionId": sid, "modeId": "plan"}); err != nil {
				planErr = fmt.Errorf("plan mode not set: %s", acpErrText(err))
			}
		}
		// A read-only thread never runs a turn whose agent may edit without asking.
		if planErr != nil && threadReadOnly(th) {
			return fail(fmt.Errorf("read-only thread not started: %v", planErr))
		}
		if planErr != nil {
			d.note(runID, "Plan mode not set: "+planErr.Error())
		}
	}
	d.mu.Lock()
	if d.closing {
		d.mu.Unlock()
		return fail(errOrchClosed)
	}
	d.sessionID = sid
	d.mu.Unlock()
	d.m.setNative(d.id, sid)
	_ = d.m.store.write(func(t *orchTx) error {
		th, err := t.thread(d.id)
		if err != nil {
			return err
		}
		th.NativeID = sid
		if model != "" {
			th.Model = model
		}
		if th.Status == "starting" || th.Status == "error" || th.Status == "idle" {
			th.Status = "running"
		}
		th.Error = ""
		d.putThreadTx(t, th)
		return nil
	})
	return conn, sid, nil
}

// interrupt sends session/cancel; a run the agent does not end within the grace period ends here.
func (d *acpDriver) interrupt() error {
	d.mu.Lock()
	conn, sid, runID := d.conn, d.sessionID, d.runID
	if conn == nil || runID == "" || sid == "" {
		d.mu.Unlock()
		return orchConflict("no running turn")
	}
	waits := d.takeWaits(runID)
	if d.cancelTimer == nil {
		d.cancelTimer = time.AfterFunc(d.m.acpCancelGraceOr(), func() {
			d.mu.Lock()
			stop := d.stopPrompt
			mine := d.runID == runID
			d.mu.Unlock()
			if !mine {
				return
			}
			if stop != nil {
				stop()
			}
			d.endRun(runID, "interrupted", "", nil)
		})
	}
	d.mu.Unlock()
	for _, w := range waits {
		w.cancel()
	}
	if err := conn.notify("session/cancel", map[string]any{"sessionId": sid}); err != nil {
		return orchConflict("interrupt failed: " + err.Error())
	}
	return nil
}

func (m *OrchManager) acpCancelGraceOr() time.Duration {
	if m.acpCancelGrace > 0 {
		return m.acpCancelGrace
	}
	return 10 * time.Second
}

func (d *acpDriver) close() {
	d.mu.Lock()
	d.closing = true
	conn, runID := d.conn, d.runID
	d.runID = ""
	if d.cancelTimer != nil {
		d.cancelTimer.Stop()
		d.cancelTimer = nil
	}
	if d.stopPrompt != nil {
		d.stopPrompt()
		d.stopPrompt = nil
	}
	waits := d.takeWaits("")
	d.mu.Unlock()
	for _, w := range waits {
		w.cancel()
	}
	d.writeClosed(runID)
	if conn != nil {
		conn.kill()
	}
}

func (d *acpDriver) shutdown() {
	d.mu.Lock()
	c := d.conn
	d.mu.Unlock()
	if c != nil {
		c.killFast()
	}
}

func (d *acpDriver) live() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return !d.closing && d.runID != "" && d.conn != nil && d.conn.alive()
}

func (d *acpDriver) procAlive() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.conn != nil && d.conn.alive()
}

func (d *acpDriver) onExit(c *codexConn) {
	d.mu.Lock()
	if d.conn != c {
		d.mu.Unlock()
		return
	}
	defer d.m.noteProcEnd(d.id)
	d.conn = nil
	closing, runID := d.closing, d.runID
	d.runID = ""
	if d.cancelTimer != nil {
		d.cancelTimer.Stop()
		d.cancelTimer = nil
	}
	waits := d.takeWaits("")
	d.mu.Unlock()
	for _, w := range waits {
		w.cancel()
	}
	if !closing && !d.m.stopping.Load() {
		d.writeExit(runID, c.exitSummary(), c.exitErr != nil)
	}
}

// ── session/update ──

func (d *acpDriver) onNotify(c *codexConn, method string, params json.RawMessage) {
	if method != "session/update" {
		return
	}
	d.touch()
	var p struct {
		SessionID string          `json:"sessionId"`
		Update    json.RawMessage `json:"update"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	var u struct {
		SessionUpdate string          `json:"sessionUpdate"`
		Content       json.RawMessage `json:"content"`
		Entries       []struct {
			Content string `json:"content"`
			Status  string `json:"status"`
		} `json:"entries"`
		Cost *struct {
			Amount   float64 `json:"amount"`
			Currency string  `json:"currency"`
		} `json:"cost"`
	}
	if json.Unmarshal(p.Update, &u) != nil {
		return
	}
	d.mu.Lock()
	if d.loading || (d.sessionID != "" && p.SessionID != "" && p.SessionID != d.sessionID) {
		d.mu.Unlock()
		return
	}
	runID := d.runID
	if u.SessionUpdate == "usage_update" && u.Cost != nil && (u.Cost.Currency == "" || strings.EqualFold(u.Cost.Currency, "USD")) {
		if !d.hasCost && runID != "" {
			d.costBase = 0
		}
		d.costLast, d.hasCost = u.Cost.Amount, true
	}
	d.mu.Unlock()
	if runID == "" {
		return
	}
	switch u.SessionUpdate {
	case "agent_message_chunk", "agent_thought_chunk":
		kind := "agent"
		if u.SessionUpdate == "agent_thought_chunk" {
			kind = "reasoning"
		}
		var c struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		_ = json.Unmarshal(u.Content, &c)
		text := c.Text
		if c.Type != "" && c.Type != "text" {
			text = "[" + c.Type + "]"
		}
		d.chunk(runID, kind, text)
	case "tool_call", "tool_call_update":
		var tc acpToolCall
		if json.Unmarshal(p.Update, &tc) != nil || tc.ToolCallID == "" {
			return
		}
		d.toolUpdate(runID, &tc)
	case "plan":
		var b strings.Builder
		for _, e := range u.Entries {
			mark := " "
			switch e.Status {
			case "completed":
				mark = "x"
			case "in_progress":
				mark = "~"
			}
			fmt.Fprintf(&b, "- [%s] %s\n", mark, e.Content)
		}
		d.closeChunk()
		d.mu.Lock()
		id, first := d.planItem, d.planItem == ""
		if first {
			id = runID + ".plan"
			d.planItem = id
		}
		d.mu.Unlock()
		_ = d.m.store.write(func(t *orchTx) error {
			it := &OItem{ID: id, ThreadID: d.id, RunID: runID, Kind: "plan", Status: "completed", Text: strings.TrimRight(b.String(), "\n"), StartedAt: t.now, EndedAt: t.now}
			if prev, err := t.item(id); err == nil {
				it.StartedAt = prev.StartedAt
			}
			if err := t.putItem(it); err != nil {
				return err
			}
			if first {
				t.emit(d.id, "item.started", map[string]any{"item": it})
			}
			t.emit(d.id, "item.completed", map[string]any{"item": it})
			return nil
		})
	}
}

// chunk appends text to the open item of kind, or starts a new item.
func (d *acpDriver) chunk(runID, kind, text string) {
	d.mu.Lock()
	d.sawOutput = true
	if d.chunkID != "" && d.chunkKind == kind {
		id := d.chunkID
		d.mu.Unlock()
		d.m.store.AppendDelta(d.id, id, "text", text)
		return
	}
	prev := d.chunkID
	d.chunkN++
	id := runID + ".c" + strconv.Itoa(d.chunkN)
	d.chunkID, d.chunkKind = id, kind
	d.mu.Unlock()
	d.completeItem(prev)
	_ = d.m.store.write(func(t *orchTx) error {
		it := &OItem{ID: id, ThreadID: d.id, RunID: runID, Kind: kind, Status: "started", Text: text, StartedAt: t.now}
		if err := t.putItem(it); err != nil {
			return err
		}
		t.emit(d.id, "item.started", map[string]any{"item": it})
		return nil
	})
}

func (d *acpDriver) closeChunk() {
	d.mu.Lock()
	id := d.chunkID
	d.chunkID, d.chunkKind = "", ""
	d.mu.Unlock()
	d.completeItem(id)
}

// toolUpdate merges a tool call into its state and writes the item snapshot.
func (d *acpDriver) toolUpdate(runID string, tc *acpToolCall) *acpTool {
	d.closeChunk()
	d.mu.Lock()
	st, ok := d.tools[tc.ToolCallID]
	if !ok {
		st = &acpTool{itemID: runID + ".t." + tc.ToolCallID, status: "pending"}
		d.tools[tc.ToolCallID] = st
	}
	st.merge(tc)
	snap := *st
	d.mu.Unlock()
	th, _ := d.m.store.Thread(d.id)
	cwd := ""
	if th != nil {
		cwd = th.Cwd
	}
	it := acpToolItem(&snap, d.id, runID, cwd)
	_ = d.m.store.write(func(t *orchTx) error {
		prev, err := t.item(it.ID)
		isNew := err != nil
		if isNew {
			it.StartedAt = t.now
		} else {
			it.StartedAt = prev.StartedAt
		}
		if it.Status != "started" {
			it.EndedAt = t.now
		}
		if err := t.putItem(it); err != nil {
			return err
		}
		if isNew {
			t.emit(d.id, "item.started", map[string]any{"item": it})
		}
		if it.Status != "started" {
			t.emit(d.id, "item.completed", map[string]any{"item": it})
		}
		return nil
	})
	return &snap
}

func acpStr(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []any:
		parts := make([]string, 0, len(x))
		for _, p := range x {
			parts = append(parts, fmt.Sprint(p))
		}
		return strings.Join(parts, " ")
	case nil:
		return ""
	}
	return fmt.Sprint(v)
}

// lineDiffCounts counts added and removed lines after trimming the common head and tail.
func lineDiffCounts(oldText, newText string) (add, del int) {
	split := func(s string) []string {
		if s == "" {
			return nil
		}
		return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	}
	a, b := split(oldText), split(newText)
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	j := 0
	for j < len(a)-i && j < len(b)-i && a[len(a)-1-j] == b[len(b)-1-j] {
		j++
	}
	return len(b) - i - j, len(a) - i - j
}

func acpStatus(s string) string {
	switch s {
	case "completed":
		return "completed"
	case "failed":
		return "failed"
	}
	return "started"
}

// acpToolItem maps a tool call to an OItem by its ToolKind.
func acpToolItem(st *acpTool, threadID, runID, cwd string) *OItem {
	it := &OItem{ID: st.itemID, ThreadID: threadID, RunID: runID, Status: acpStatus(st.status), Text: st.title}
	var texts []string
	type diff struct {
		Path    string  `json:"path"`
		OldText *string `json:"oldText"`
		NewText string  `json:"newText"`
	}
	var diffs []diff
	for _, raw := range st.content {
		var c struct {
			Type    string `json:"type"`
			Content struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			diff
		}
		if json.Unmarshal(raw, &c) != nil {
			continue
		}
		switch c.Type {
		case "content":
			if c.Content.Text != "" {
				texts = append(texts, c.Content.Text)
			}
		case "diff":
			diffs = append(diffs, c.diff)
		case "terminal":
			texts = append(texts, "[terminal output]")
		}
	}
	output := strings.Join(texts, "\n")
	if output == "" && len(st.rawOutput) > 0 {
		var ro map[string]any
		if json.Unmarshal(st.rawOutput, &ro) == nil {
			output = firstOf(acpStr(ro["output"]), acpStr(ro["stdout"]), acpStr(ro["content"]))
		}
		if output == "" {
			output = clipJSON(st.rawOutput)
		}
	}
	if len(output) > orchOutputCap {
		output = output[len(output)-orchOutputCap:]
	}
	in := st.rawInput
	switch st.kind {
	case "execute":
		it.Kind = "command"
		it.Command = firstOf(acpStr(in["command"]), acpStr(in["cmd"]), st.title)
		it.Cwd = firstOf(acpStr(in["cwd"]), acpStr(in["workdir"]), cwd)
		it.Output = output
		var ro map[string]any
		if json.Unmarshal(st.rawOutput, &ro) == nil {
			for _, k := range []string{"exitCode", "exit_code", "exit"} {
				if f, ok := ro[k].(float64); ok {
					n := int(f)
					it.ExitCode = &n
					break
				}
			}
			if meta, ok := ro["metadata"].(map[string]any); ok && it.ExitCode == nil {
				if f, ok := meta["exit"].(float64); ok {
					n := int(f)
					it.ExitCode = &n
				}
			}
		}
	case "edit", "delete", "move":
		it.Kind = "fileChange"
		var paths []string
		for _, df := range diffs {
			kind := "update"
			if df.OldText == nil {
				kind = "add"
			}
			old := ""
			if df.OldText != nil {
				old = *df.OldText
			}
			a, dl := lineDiffCounts(old, df.NewText)
			it.Files = append(it.Files, OFile{Path: df.Path, Kind: kind, Add: a, Del: dl})
			paths = append(paths, df.Path)
		}
		if len(it.Files) == 0 {
			kind := map[string]string{"edit": "update", "delete": "delete", "move": "move"}[st.kind]
			locs := st.locations
			if len(locs) == 0 {
				if p := firstOf(acpStr(in["filePath"]), acpStr(in["file_path"]), acpStr(in["path"])); p != "" {
					locs = []string{p}
				}
			}
			for _, p := range locs {
				it.Files = append(it.Files, OFile{Path: p, Kind: kind})
				paths = append(paths, p)
			}
		}
		if len(paths) > 0 {
			it.Text = strings.Join(paths, "\n")
		}
		if it.Status == "failed" {
			it.Output = output
		}
	case "fetch":
		it.Kind = "web"
		it.Text = firstOf(acpStr(in["url"]), acpStr(in["query"]), st.title)
		it.Output = output
	case "search":
		it.Kind = "other"
		if strings.Contains(strings.ToLower(st.title), "web") {
			it.Kind = "web"
		}
		it.Command = st.kind
		it.Output = output
	default:
		it.Kind = "other"
		it.Command = firstOf(st.kind, "tool")
		it.Output = output
	}
	if it.Text == "" {
		it.Text = firstOf(st.title, st.kind, "tool call")
	}
	return it
}

// ── agent requests ──

func (d *acpDriver) onRequest(c *codexConn, id json.RawMessage, method string, params json.RawMessage) {
	d.touch()
	switch method {
	case "session/request_permission":
		go d.handlePermission(c, id, params)
	case "fs/read_text_file":
		go d.handleRead(c, id, params)
	case "fs/write_text_file":
		go d.handleWrite(c, id, params)
	default:
		_ = c.respondError(id, -32601, "agent-monitor does not support "+method)
	}
}

// workspaceRoots are the folders a driven ACP agent may read and write, each also in its resolved form.
func workspaceRoots(th *OThread) []string {
	roots := []string{th.Cwd}
	if th.Worktree != nil && !th.Worktree.Removed && th.Worktree.Path != "" {
		roots = append(roots, th.Worktree.Path)
	}
	var out []string
	for _, r := range roots {
		out = append(out, filepath.Clean(r))
		if real, err := filepath.EvalSymlinks(r); err == nil && filepath.Clean(real) != filepath.Clean(r) {
			out = append(out, filepath.Clean(real))
		}
	}
	return out
}

// openWorkspace opens the workspace root that lexically holds path, with path relative to it.
// Every read and write goes through the *os.Root, which refuses symlinks and races that leave it.
func openWorkspace(th *OThread, path string) (*os.Root, string, error) {
	if !filepath.IsAbs(path) {
		return nil, "", errOutsideWorkspace
	}
	p := filepath.Clean(path)
	for _, root := range workspaceRoots(th) {
		rel, err := filepath.Rel(root, p)
		if err != nil || rel == "." || !filepath.IsLocal(rel) {
			continue
		}
		r, err := os.OpenRoot(root)
		if err != nil {
			return nil, "", err
		}
		return r, rel, nil
	}
	return nil, "", errOutsideWorkspace
}

var errOutsideWorkspace = errors.New("outside the thread workspace")

// insideWorkspace reports whether path stays inside the thread cwd or worktree, symlinks included.
func insideWorkspace(th *OThread, path string) (string, bool) {
	r, rel, err := openWorkspace(th, path)
	if err != nil {
		return "", false
	}
	defer r.Close()
	if _, err := r.Stat(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", false
	}
	if dir := filepath.Dir(rel); dir != "." {
		if _, err := r.Stat(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", false
		}
	}
	return filepath.Join(r.Name(), rel), true
}

const (
	acpErrDenied   = -32000
	acpErrNotFound = -32002
	acpErrParams   = -32602
)

func (d *acpDriver) handleRead(c *codexConn, id json.RawMessage, params json.RawMessage) {
	var p struct {
		Path  string `json:"path"`
		Line  *int   `json:"line"`
		Limit *int   `json:"limit"`
	}
	if json.Unmarshal(params, &p) != nil || p.Path == "" {
		_ = c.respondError(id, acpErrParams, "path is required")
		return
	}
	th, err := d.m.store.Thread(d.id)
	if err != nil {
		_ = c.respondError(id, acpErrDenied, "thread not found")
		return
	}
	root, rel, err := openWorkspace(th, p.Path)
	if err != nil {
		_ = c.respondError(id, acpErrDenied, "agent-monitor: "+p.Path+" is outside the thread workspace "+th.Cwd)
		return
	}
	defer root.Close()
	// O_NONBLOCK keeps a FIFO from blocking the open; only regular files are read.
	f, err := root.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		code := int64(acpErrDenied)
		if errors.Is(err, fs.ErrNotExist) {
			code = acpErrNotFound
		}
		_ = c.respondError(id, code, err.Error())
		return
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		_ = c.respondError(id, acpErrDenied, "agent-monitor: "+p.Path+" is not a regular file")
		return
	}
	buf := make([]byte, acpReadCap+1)
	n, _ := readFull(f, buf)
	if n > acpReadCap {
		_ = c.respondError(id, acpErrDenied, fmt.Sprintf("file is larger than %d bytes", acpReadCap))
		return
	}
	text := string(buf[:n])
	if p.Line != nil || p.Limit != nil {
		lines := strings.SplitAfter(text, "\n")
		start := 0
		if p.Line != nil && *p.Line > 1 {
			start = min(*p.Line-1, len(lines))
		}
		end := len(lines)
		if p.Limit != nil && *p.Limit >= 0 {
			end = min(start+*p.Limit, len(lines))
		}
		text = strings.Join(lines[start:end], "")
	}
	_ = c.respond(id, map[string]any{"content": text})
}

func readFull(f *os.File, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := f.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func (d *acpDriver) newApproval(th *OThread, tool string, input map[string]any) *Approval {
	a := buildApproval(hookCodex, hookPayload{SessionID: th.NativeID, Cwd: th.Cwd, ToolName: tool, ToolInput: input})
	a.Agent, a.SessionID = th.Agent, th.Agent+":"+th.NativeID
	return a
}

// autoAllowed records an approval that acceptEdits answered without the user.
func (d *acpDriver) autoAllowed(a *Approval) {
	if d.m.approvals == nil {
		return
	}
	a.DrivenID, a.RootID = d.id, d.m.store.RootOf(d.id)
	a.State, a.DecidedBy = "auto-allowed", "acceptEdits"
	d.m.approvals.recordFinal(a)
}

// await registers the approval as a wait of the active run; canceled means the run ended first.
func (d *acpDriver) await(a *Approval, itemID, kind string) (approvalDecision, string) {
	d.mu.Lock()
	runID := d.runID
	ctx, cancel := context.WithCancel(context.Background())
	key := a.ID
	d.waits[key] = &orchWait{cancel: cancel, runID: runID}
	d.mu.Unlock()
	defer func() {
		cancel()
		d.mu.Lock()
		delete(d.waits, key)
		d.mu.Unlock()
	}()
	return d.awaitApproval(ctx, a, runID, itemID, kind, func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.runID != ""
	})
}

func (d *acpDriver) handleWrite(c *codexConn, id json.RawMessage, params json.RawMessage) {
	var p struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if json.Unmarshal(params, &p) != nil || p.Path == "" {
		_ = c.respondError(id, acpErrParams, "path is required")
		return
	}
	th, err := d.m.store.Thread(d.id)
	if err != nil {
		_ = c.respondError(id, acpErrDenied, "thread not found")
		return
	}
	if _, ok := insideWorkspace(th, p.Path); !ok {
		_ = c.respondError(id, acpErrDenied, "agent-monitor: "+p.Path+" is outside the thread workspace "+th.Cwd)
		return
	}
	if threadReadOnly(th) {
		_ = c.respondError(id, acpErrDenied, "agent-monitor: this thread is read-only")
		return
	}
	root, rel, err := openWorkspace(th, p.Path)
	if err != nil {
		_ = c.respondError(id, acpErrDenied, "agent-monitor: "+p.Path+" is outside the thread workspace "+th.Cwd)
		return
	}
	defer root.Close()
	old, _ := root.ReadFile(rel)
	a, dl := lineDiffCounts(string(old), p.Content)
	input := map[string]any{"file_path": p.Path, "content": clipString(p.Content, 4000), "add": a, "del": dl}
	appr := d.newApproval(th, "Write", input)
	if th.PermissionMode == "acceptEdits" {
		if d.m.approvals != nil {
			if dec := d.m.approvals.denyDecision(appr); dec != nil {
				_ = c.respondError(id, acpErrDenied, "agent-monitor: write denied: "+dec.Message)
				return
			}
		}
		d.autoAllowed(appr)
	} else {
		dec, outcome := d.await(appr, "", "fileChange")
		if outcome == "canceled" {
			_ = c.respondError(id, acpErrDenied, "agent-monitor: the turn was cancelled")
			return
		}
		if dec.Behavior != "allow" {
			_ = c.respondError(id, acpErrDenied, "agent-monitor: write denied: "+firstOf(dec.Message, "denied by the user"))
			return
		}
	}
	// The write resolves rel inside the root again, so a symlink swapped in during the approval cannot leave it.
	mode := os.FileMode(0o644)
	if st, err := root.Stat(rel); err == nil {
		mode = st.Mode().Perm()
	}
	if dir := filepath.Dir(rel); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			_ = c.respondError(id, acpErrDenied, err.Error())
			return
		}
	}
	f, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NONBLOCK, mode)
	if err == nil {
		if st, serr := f.Stat(); serr != nil || !st.Mode().IsRegular() {
			err = errors.New(p.Path + " is not a regular file")
		} else {
			_, err = f.WriteString(p.Content)
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		_ = c.respondError(id, acpErrDenied, err.Error())
		return
	}
	_ = c.respond(id, map[string]any{})
}

func pickOption(opts []acpPermOption, kinds ...string) string {
	for _, k := range kinds {
		for _, o := range opts {
			if o.Kind == k {
				return o.OptionID
			}
		}
	}
	return ""
}

// acpChoice maps a decision to an offered option: allow → allow_once, Always → allow_always, deny → reject_once.
func acpChoice(a *Approval, opts []acpPermOption, dec approvalDecision) string {
	if dec.OptionID != "" {
		for _, o := range opts {
			if o.OptionID == dec.OptionID {
				if o.Kind == "allow_always" && !a.AlwaysOK {
					if id := pickOption(opts, "allow_once"); id != "" {
						return id
					}
				}
				return o.OptionID
			}
		}
	}
	if dec.Behavior == "allow" {
		if dec.Always {
			return pickOption(opts, "allow_always", "allow_once")
		}
		return pickOption(opts, "allow_once", "allow_always")
	}
	return pickOption(opts, "reject_once", "reject_always")
}

func acpRequestKind(kind string) string {
	switch kind {
	case "execute":
		return "command"
	case "edit", "delete", "move":
		return "fileChange"
	}
	return "permissions"
}

func (d *acpDriver) handlePermission(c *codexConn, id json.RawMessage, params json.RawMessage) {
	var p struct {
		ToolCall acpToolCall     `json:"toolCall"`
		Options  []acpPermOption `json:"options"`
	}
	respond := func(optionID string) {
		if optionID == "" {
			_ = c.respond(id, map[string]any{"outcome": map[string]any{"outcome": "cancelled"}})
			return
		}
		_ = c.respond(id, map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": optionID}})
	}
	if json.Unmarshal(params, &p) != nil {
		respond("")
		return
	}
	th, err := d.m.store.Thread(d.id)
	if err != nil {
		respond("")
		return
	}
	d.mu.Lock()
	runID := d.runID
	d.mu.Unlock()
	if runID == "" {
		respond("")
		return
	}
	st := &acpTool{title: "tool", status: "pending"}
	itemID := ""
	if p.ToolCall.ToolCallID != "" {
		st = d.toolUpdate(runID, &p.ToolCall)
		itemID = st.itemID
	}
	edits := st.kind == "edit" || st.kind == "delete" || st.kind == "move"
	in := st.rawInput
	if edits && threadReadOnly(th) {
		respond(pickOption(p.Options, "reject_once", "reject_always"))
		return
	}
	if edits && th.PermissionMode == "acceptEdits" {
		// acceptEdits covers edits inside the workspace only, and deny rules still apply.
		paths := append([]string{}, st.locations...)
		if path := firstOf(acpStr(in["filePath"]), acpStr(in["file_path"]), acpStr(in["path"])); path != "" {
			paths = append(paths, path)
		}
		inside := len(paths) > 0
		for _, path := range paths {
			if _, ok := insideWorkspace(th, path); !ok {
				inside = false
			}
		}
		if inside {
			for _, path := range paths {
				a := d.newApproval(th, "Edit", map[string]any{"title": st.title, "file_path": path})
				if d.m.approvals != nil {
					if dec := d.m.approvals.denyDecision(a); dec != nil {
						respond(pickOption(p.Options, "reject_once", "reject_always"))
						return
					}
				}
			}
			d.autoAllowed(d.newApproval(th, "Edit", map[string]any{"title": st.title, "file_path": paths[0]}))
			respond(pickOption(p.Options, "allow_once", "allow_always"))
			return
		}
	}
	input := map[string]any{"title": st.title}
	tool := firstOf(st.title, "tool")
	switch st.kind {
	case "execute":
		tool = "Bash"
		input["command"] = firstOf(acpStr(in["command"]), acpStr(in["cmd"]), st.title)
		input["cwd"] = firstOf(acpStr(in["cwd"]), acpStr(in["workdir"]), th.Cwd)
	case "edit", "delete", "move":
		tool = "Edit"
		path := firstOf(acpStr(in["filePath"]), acpStr(in["file_path"]), acpStr(in["path"]))
		if path == "" && len(st.locations) > 0 {
			path = st.locations[0]
		}
		input["file_path"] = path
	case "fetch":
		tool = "WebFetch"
		input["url"] = firstOf(acpStr(in["url"]), st.title)
	default:
		for k, v := range in {
			input[k] = v
		}
	}
	a := d.newApproval(th, tool, input)
	a.Options = p.Options
	dec, outcome := d.await(a, itemID, acpRequestKind(st.kind))
	if outcome == "canceled" {
		respond("")
		return
	}
	respond(acpChoice(a, p.Options, dec))
}

// checkImages answers 400 unless the agent advertised image prompts; an unknown agent is started once to learn it.
func (d *acpDriver) checkImages() error {
	th, err := d.m.store.Thread(d.id)
	if err != nil {
		return err
	}
	ok, known := d.m.agents.imageSupport(th.Agent)
	if !known {
		d.mu.Lock()
		busy := d.runID != "" || d.closing
		d.mu.Unlock()
		if busy {
			return nil // send answers the 409
		}
		if _, _, err := d.ensureProcess(""); err != nil {
			return err
		}
		_ = d.m.store.write(func(t *orchTx) error {
			th, err := t.thread(d.id)
			if err == nil && th.Status == "running" {
				d.mu.Lock()
				idle := d.runID == ""
				d.mu.Unlock()
				if idle {
					th.Status = "idle"
					d.putThreadTx(t, th)
				}
			}
			return nil
		})
		ok, _ = d.m.agents.imageSupport(th.Agent)
	}
	if !ok {
		return orchBadRequest(th.Agent + " does not accept images (promptCapabilities.image is false)")
	}
	return nil
}
