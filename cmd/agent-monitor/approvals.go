package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type Approval struct {
	ID          string           `json:"id"`
	Agent       string           `json:"agent"`
	SessionID   string           `json:"sessionId"`
	Cwd         string           `json:"cwd"`
	Kind        string           `json:"kind"`
	ToolName    string           `json:"toolName"`
	Input       map[string]any   `json:"input"`
	Summary     string           `json:"summary"`
	Risk        string           `json:"risk"`
	RiskReason  string           `json:"riskReason"`
	Suggestions []any            `json:"suggestions,omitempty"`
	Questions   []map[string]any `json:"questions,omitempty"`
	Plan        string           `json:"plan,omitempty"`
	CreatedAt   int64            `json:"createdAt"`
	HoldUntil   int64            `json:"holdUntil"`
	// Passable: a held hook of a session with a terminal, so "Answer in terminal" can release it there.
	Passable  bool            `json:"passable,omitempty"`
	State     string          `json:"state"`
	DecidedAt int64           `json:"decidedAt,omitempty"`
	DecidedBy string          `json:"decidedBy,omitempty"`
	Message   string          `json:"message,omitempty"`
	RuleID    string          `json:"ruleId,omitempty"`
	AlwaysOK  bool            `json:"alwaysOk"`
	DrivenID  string          `json:"drivenId,omitempty"`
	RootID    string          `json:"rootId,omitempty"`
	Options   []acpPermOption `json:"options,omitempty"`
	// Source "terminal-shown": the CLI shows its own prompt too, and a card decision is typed into it.
	Source       string    `json:"source,omitempty"`
	SentAt       int64     `json:"sentAt,omitempty"`
	Choice       string    `json:"choice,omitempty"`
	ToolUseID    string    `json:"toolUseId,omitempty"`
	Group        string    `json:"group,omitempty"`
	DeviceID     string    `json:"deviceId,omitempty"`
	DeviceName   string    `json:"deviceName,omitempty"`
	Proof        *bioProof `json:"proof,omitempty"`
	GroupPattern string    `json:"groupPattern,omitempty"`
	fullText     string
	// noAutoAllow keeps allow rules from answering (read-only workspaces); deny rules still apply.
	noAutoAllow bool
}

type Rule struct {
	ID        string `json:"id"`
	Action    string `json:"action"`
	Agent     string `json:"agent,omitempty"`
	Tool      string `json:"tool,omitempty"`
	Pattern   string `json:"pattern,omitempty"`
	CwdPrefix string `json:"cwdPrefix,omitempty"`
	Note      string `json:"note,omitempty"`
	CreatedAt int64  `json:"createdAt"`
	Hits      int    `json:"hits"`
}

type ApprovalSettings struct {
	HoldSeconds         int  `json:"holdSeconds"`
	QuestionHoldSeconds int  `json:"questionHoldSeconds"`
	SkipWhenFocused     bool `json:"skipWhenFocused"`
	// DrivenApprovalTimeoutSec declines a driven thread's approval after this long; 0 waits for the user.
	DrivenApprovalTimeoutSec int `json:"drivenApprovalTimeoutSec"`
	// ApprovalsMode "hold" (default) holds the hook and answers it directly; "both" shows the agent's own prompt
	// and types a remote answer into the terminal.
	ApprovalsMode string `json:"approvalsMode"`
	// ModeSet: the person chose ApprovalsMode; until then hold is the default, and Cursor keeps its own lists.
	ModeSet bool `json:"modeSet,omitempty"`
	// BioMinRisk: the lowest risk (low, medium, high) whose approval needs Face ID or a passkey; empty = medium.
	BioMinRisk string `json:"bioMinRisk,omitempty"`
	// BioTrustMin: minutes one signature covers the same session's low and medium approvals; nil = 10, 0 = off.
	BioTrustMin *int `json:"bioTrustMin,omitempty"`
}

type approvalDecision struct {
	Behavior     string            `json:"behavior"`
	Message      string            `json:"message,omitempty"`
	Interrupt    bool              `json:"interrupt,omitempty"`
	UpdatedInput map[string]any    `json:"updatedInput,omitempty"`
	Always       bool              `json:"always,omitempty"`
	Answers      map[string]string `json:"answers,omitempty"`
	Surface      string            `json:"surface,omitempty"`
	// OptionID picks one of Approval.Options (ACP agents).
	OptionID string `json:"optionId,omitempty"`
	device   *Device
	proof    *bioProof
	// limited: the sender is below admin, so it may not add a lasting rule ("always").
	limited bool
	// AlwaysScope "claude" returns the CLI's permission_suggestions instead of adding an agent-monitor rule.
	AlwaysScope string `json:"alwaysScope,omitempty"`
	// ApplySuggestions is the web alias for always + alwaysScope "claude".
	ApplySuggestions bool `json:"applySuggestions,omitempty"`
}

// Hook endpoint flavours; each answers in its own JSON shape.
const (
	hookClaudePermission = "claude-permission"
	hookClaudePretool    = "claude-pretool"
	hookCodex            = "codex"
	hookCursor           = "cursor" // beforeShellExecution and beforeMCPExecution
)

type pendingApproval struct {
	a     *Approval
	hook  string
	reply chan approvalDecision
	term  *termWatch
}

type ApprovalStore struct {
	mu       sync.Mutex
	dir      string
	pending  map[string]*pendingApproval
	recent   []*Approval
	rules    []Rule
	hitsSave *time.Timer // a pending write of the rule hit counts
	settings ApprovalSettings
	db       *sql.DB
	// focused reports whether the user is looking at the session's tmux pane right now.
	focused func(storeID string) bool
	// reachable reports whether the session has a live host or tmux pane to type into.
	reachable func(storeID string) bool
}

var (
	approvalStore     *ApprovalStore
	approvalStoreOnce sync.Once
)

func registerApprovalRoutes(mux *http.ServeMux) {
	approvalStoreOnce.Do(func() { approvalStore = newApprovalStore(stateDir()) })
	approvalStore.routes(mux)
}

// pendingApprovalsBySession counts pending approvals per store id.
func pendingApprovalsBySession() map[string]int {
	out := map[string]int{}
	if approvalStore == nil {
		return out
	}
	approvalStore.mu.Lock()
	defer approvalStore.mu.Unlock()
	for _, p := range approvalStore.pending {
		out[p.a.SessionID]++
	}
	return out
}

func newApprovalStore(dir string) *ApprovalStore {
	_ = os.MkdirAll(dir, 0o755)
	s := &ApprovalStore{
		dir:       dir,
		pending:   map[string]*pendingApproval{},
		settings:  ApprovalSettings{HoldSeconds: 120, QuestionHoldSeconds: 600, SkipWhenFocused: true},
		focused:   newAttentionProbe().focused,
		reachable: terminalReachable,
	}
	if b, err := os.ReadFile(filepath.Join(dir, "approval-settings.json")); err == nil {
		_ = json.Unmarshal(b, &s.settings)
	}
	s.normalizeSettings()
	if b, err := os.ReadFile(filepath.Join(dir, "approval-rules.json")); err == nil {
		if err := json.Unmarshal(b, &s.rules); err != nil {
			log.Printf("approvals: bad approval-rules.json: %v", err)
		}
	} else {
		now := time.Now().UnixMilli()
		for i, t := range []string{"Read", "Grep", "Glob", "LS"} {
			s.rules = append(s.rules, Rule{ID: fmt.Sprintf("seed-allow-%d", i+1), Action: "allow", Tool: t, Note: "read-only tool", CreatedAt: now})
		}
		for i, p := range []string{"brew install*", "sudo *", "launchctl *"} {
			s.rules = append(s.rules, Rule{ID: fmt.Sprintf("seed-deny-%d", i+1), Action: "deny", Pattern: p, Note: "system change: run it yourself", CreatedAt: now})
		}
		s.saveRulesLocked()
	}
	s.openDB()
	return s
}

func (s *ApprovalStore) normalizeSettings() {
	if s.settings.HoldSeconds <= 0 {
		s.settings.HoldSeconds = 120
	}
	if s.settings.QuestionHoldSeconds <= 0 {
		s.settings.QuestionHoldSeconds = 600
	}
	if !s.settings.ModeSet {
		s.settings.ApprovalsMode = approvalsModeHold
	} else if s.settings.ApprovalsMode != approvalsModeHold {
		s.settings.ApprovalsMode = approvalsModeBoth
	}
}

func (s *ApprovalStore) openDB() {
	db, err := sql.Open("sqlite", filepath.Join(s.dir, "approvals.db")+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err == nil {
		_, err = db.Exec(`CREATE TABLE IF NOT EXISTS approvals(
			id TEXT PRIMARY KEY, created_at INTEGER, decided_at INTEGER, state TEXT,
			agent TEXT, session TEXT, tool TEXT, summary TEXT, json TEXT)`)
		if err == nil {
			_, err = db.Exec(`CREATE INDEX IF NOT EXISTS approvals_decided ON approvals(decided_at)`)
		}
	}
	if err != nil {
		log.Printf("approvals: audit db unavailable: %v", err)
		return
	}
	s.db = db
	if list, err := s.audit(0, 100); err == nil {
		for i := len(list) - 1; i >= 0; i-- {
			s.recent = append(s.recent, list[i])
		}
	}
}

func (s *ApprovalStore) audit(since int64, limit int) ([]*Approval, error) {
	out := []*Approval{}
	if s.db == nil {
		return out, nil
	}
	rows, err := s.db.Query(`SELECT json FROM approvals WHERE decided_at >= ? ORDER BY decided_at DESC LIMIT ?`, since, limit)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var js string
		if rows.Scan(&js) != nil {
			continue
		}
		a := &Approval{}
		if json.Unmarshal([]byte(js), a) == nil {
			out = append(out, a)
		}
	}
	return out, rows.Err()
}

func (s *ApprovalStore) auditOne(id string) *Approval {
	if s.db == nil {
		return nil
	}
	var raw string
	if s.db.QueryRow(`SELECT json FROM approvals WHERE id = ?`, id).Scan(&raw) != nil {
		return nil
	}
	var a Approval
	if json.Unmarshal([]byte(raw), &a) != nil {
		return nil
	}
	return &a
}

// proofCheck checks a stored decision's signature again, against the key it was made with.
func proofCheck(a *Approval) map[string]any {
	out := map[string]any{"id": a.ID, "device": a.DeviceName, "decidedAt": a.DecidedAt, "state": a.State}
	switch {
	case a.Proof == nil:
		out["ok"], out["detail"] = false, "no signature: decided on the Mac, by a rule, or by a device with no Face ID or passkey"
	case a.Proof.Kind == "trusted":
		out["ok"], out["kind"], out["detail"] = true, "trusted", "inside the trust window of an earlier signature from this device"
	default:
		err := checkProof(a.Proof)
		out["ok"], out["kind"] = err == nil, a.Proof.Kind
		if err != nil {
			out["detail"] = err.Error()
		} else {
			out["detail"] = "the signature matches the request and the device key"
		}
	}
	return out
}

func (s *ApprovalStore) writeAudit(a *Approval) {
	if s.db == nil {
		return
	}
	b, _ := json.Marshal(a)
	if _, err := s.db.Exec(`INSERT OR REPLACE INTO approvals(id, created_at, decided_at, state, agent, session, tool, summary, json)
		VALUES(?,?,?,?,?,?,?,?,?)`, a.ID, a.CreatedAt, a.DecidedAt, a.State, a.Agent, a.SessionID, a.ToolName, a.Summary, string(b)); err != nil {
		log.Printf("approvals: audit write: %v", err)
	}
}

func writeFileAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (s *ApprovalStore) saveRulesLocked() {
	if err := writeFileAtomic(filepath.Join(s.dir, "approval-rules.json"), s.rules); err != nil {
		log.Printf("approvals: save rules: %v", err)
	}
}

func newApprovalID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "ap_" + hex.EncodeToString(b)
}

func copyApproval(a *Approval) *Approval {
	c := *a
	return &c
}

// hookPayload is the union of the Claude and Codex hook inputs we read.
type hookPayload struct {
	SessionID             string `json:"session_id"`
	TranscriptPath        string `json:"transcript_path"`
	Cwd                   string `json:"cwd"`
	HookEventName         string `json:"hook_event_name"`
	ToolName              string `json:"tool_name"`
	ToolInput             any    `json:"tool_input"`
	PermissionSuggestions []any  `json:"permission_suggestions"`
	TurnID                string `json:"turn_id"`
	ToolUseID             string `json:"tool_use_id"`
	// Cursor hook fields
	ConversationID string   `json:"conversation_id"`
	Command        string   `json:"command"`
	WorkspaceRoots []string `json:"workspace_roots"`
	MCPServer      string   `json:"mcp_server_name"`
	CursorVersion  string   `json:"cursor_version"`
}

// cursorPayload maps a Cursor permission hook onto the fields Claude's hook uses.
func cursorPayload(p hookPayload) hookPayload {
	p.SessionID = firstOf(p.ConversationID, p.SessionID)
	if p.Cwd == "" && len(p.WorkspaceRoots) > 0 {
		p.Cwd = p.WorkspaceRoots[0]
	}
	if p.HookEventName == "beforeMCPExecution" {
		p.ToolName = "mcp__" + p.MCPServer + "__" + p.ToolName
	} else {
		p.ToolName, p.ToolInput = "Bash", map[string]any{"command": p.Command}
	}
	return p
}

func buildApproval(hook string, p hookPayload) *Approval {
	agent := "claude"
	if hook == hookCodex {
		agent = "codex"
	} else if hook == hookCursor {
		agent = "cursor"
	}
	input, ok := p.ToolInput.(map[string]any)
	if !ok {
		input = map[string]any{}
		if p.ToolInput != nil {
			input["value"] = p.ToolInput
		}
	}
	a := &Approval{
		ID: newApprovalID(), Agent: agent, SessionID: agent + ":" + p.SessionID, Cwd: p.Cwd,
		Kind: "permission", ToolName: p.ToolName, Input: input, CreatedAt: time.Now().UnixMilli(), State: "pending",
	}
	if p.ToolName == "" {
		a.ToolName = "tool"
	}
	switch p.ToolName {
	case "AskUserQuestion":
		a.Kind = "question"
		if qs, ok := input["questions"].([]any); ok {
			for _, q := range qs {
				if m, ok := q.(map[string]any); ok {
					a.Questions = append(a.Questions, m)
				}
			}
		}
	case "ExitPlanMode":
		a.Kind = "plan"
		a.Plan = str(input["plan"])
	}
	if hook == hookClaudePermission {
		a.Suggestions = p.PermissionSuggestions
	}
	a.fullText = approvalText(a.Kind, a.ToolName, input)
	a.Summary = clipString(strings.Join(strings.Fields(a.fullText), " "), 300)
	a.Risk, a.RiskReason = shownRisk(a.Kind, a.ToolName, strings.Join(strings.Fields(a.fullText), " "))
	if isShellTool(a.ToolName) && isCompoundShell(a.fullText) && a.Risk == "low" {
		a.Risk, a.RiskReason = "medium", "chained, piped or redirected command"
	}
	a.AlwaysOK = a.Kind == "permission" && a.Risk != "high" && !(isShellTool(a.ToolName) && isCompoundShell(a.fullText)) && alwaysPattern(a) != ""
	if a.AlwaysOK && a.Cwd != "" {
		// Requests with one group are what one Always rule would cover: "Allow these N" answers them together.
		a.GroupPattern = alwaysPattern(a)
		a.Group = suggestKey(a.Agent, a.ToolName, a.GroupPattern, a.Cwd)
	}
	return a
}

// matchRule applies deny before allow before hold; it bumps the hit count.
func (s *ApprovalStore) matchRule(a *Approval) *Rule {
	return s.matchRuleOf(a, "deny", "allow", "hold")
}

// saveHitsSoonLocked writes the rule file once for a burst of matches, not once per match.
func (s *ApprovalStore) saveHitsSoonLocked() {
	if s.hitsSave != nil {
		return
	}
	s.hitsSave = time.AfterFunc(10*time.Second, func() {
		s.mu.Lock()
		s.hitsSave = nil
		s.saveRulesLocked()
		s.mu.Unlock()
	})
}

func (s *ApprovalStore) matchRuleOf(a *Approval, actions ...string) *Rule {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, action := range actions {
		if a.Kind != "permission" && action != "hold" {
			continue
		}
		if action == "allow" && !a.AlwaysOK {
			continue // high risk or compound shell: rules may deny or hold, never allow
		}
		for i := range s.rules {
			if s.rules[i].Action == action && ruleMatches(s.rules[i], a) {
				s.rules[i].Hits++
				s.saveHitsSoonLocked()
				r := s.rules[i]
				return &r
			}
		}
	}
	return nil
}

// hookResponse renders a decision in the shape the calling agent expects.
func hookResponse(hook string, a *Approval, d approvalDecision) map[string]any {
	msg := d.Message
	if msg == "" && d.Behavior == "deny" {
		msg = "Denied in agent-monitor"
	}
	switch hook {
	case hookCursor:
		if d.Behavior == "allow" {
			return map[string]any{"permission": "allow"}
		}
		return map[string]any{"permission": "deny", "user_message": msg, "agent_message": msg}
	case hookClaudePretool:
		out := map[string]any{"hookEventName": "PreToolUse"}
		if d.Behavior != "allow" {
			out["permissionDecision"] = "deny"
			out["permissionDecisionReason"] = msg
			return map[string]any{"hookSpecificOutput": out}
		}
		out["permissionDecision"] = "allow"
		upd := d.UpdatedInput
		if upd == nil {
			upd = map[string]any{}
			for k, v := range a.Input {
				upd[k] = v
			}
		}
		if a.Kind == "question" {
			if _, ok := upd["questions"]; !ok {
				upd["questions"] = a.Input["questions"]
			}
			upd["answers"] = d.Answers
		}
		out["updatedInput"] = upd
		return map[string]any{"hookSpecificOutput": out}
	case hookCodex:
		dec := map[string]any{"behavior": d.Behavior}
		if d.Behavior == "deny" {
			dec["message"] = msg
		}
		return map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PermissionRequest", "decision": dec}}
	default:
		dec := map[string]any{"behavior": d.Behavior}
		if d.Behavior == "allow" {
			if d.UpdatedInput != nil {
				dec["updatedInput"] = d.UpdatedInput
			}
			if d.Always && len(a.Suggestions) > 0 {
				dec["updatedPermissions"] = a.Suggestions
			}
		} else {
			dec["message"] = msg
			dec["interrupt"] = d.Interrupt
		}
		return map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PermissionRequest", "decision": dec}}
	}
}

// finish moves a pending approval to a final state; false when it already left pending.
func (s *ApprovalStore) finish(id, state, by, msg string, d *approvalDecision) (*Approval, bool) {
	return s.finishChoice(id, state, by, msg, "", d)
}

func (s *ApprovalStore) finishChoice(id, state, by, msg, choice string, d *approvalDecision) (*Approval, bool) {
	s.mu.Lock()
	p, ok := s.pending[id]
	if !ok {
		s.mu.Unlock()
		return nil, false
	}
	delete(s.pending, id)
	p.a.State, p.a.DecidedAt, p.a.DecidedBy, p.a.Message = state, time.Now().UnixMilli(), by, msg
	if choice != "" {
		p.a.Choice = choice
	}
	if d != nil && d.device != nil {
		p.a.DeviceID, p.a.DeviceName, p.a.Proof = d.device.ID, d.device.Name, d.proof
	}
	if d != nil {
		p.reply <- *d
	}
	snap := copyApproval(p.a)
	s.pushRecentLocked(snap)
	s.mu.Unlock()
	s.writeAudit(snap)
	publishV3("approval", "approval", snap)
	if notifier != nil {
		notifier.Resolve(snap.ID)
	}
	publishV3("approval-done", "approval", snap)
	return snap, true
}

func (s *ApprovalStore) pushRecentLocked(a *Approval) {
	s.recent = append(s.recent, a)
	if len(s.recent) > 100 {
		s.recent = s.recent[len(s.recent)-100:]
	}
}

// recordFinal stores an approval that never waited (rule or focus).
func (s *ApprovalStore) recordFinal(a *Approval) {
	a.DecidedAt = time.Now().UnixMilli()
	snap := copyApproval(a)
	s.mu.Lock()
	s.pushRecentLocked(snap)
	s.mu.Unlock()
	s.writeAudit(snap)
	publishV3("approval", "approval", snap)
	publishV3("approval-done", "approval", snap)
}

// drivenSessionFn reports whether a native session id belongs to a live orchestrator-driven turn.
var drivenSessionFn func(sessionID string) bool

// ruleDecision applies an allow or deny rule and records the result; nil means the approval must wait.
func (s *ApprovalStore) ruleDecision(a *Approval) *approvalDecision {
	rule := s.matchRule(a)
	if rule == nil || (a.noAutoAllow && rule.Action == "allow") {
		return nil
	}
	a.RuleID = rule.ID
	if rule.Action == "hold" {
		return nil
	}
	a.DecidedBy = "rule:" + rule.ID
	d := approvalDecision{Behavior: rule.Action}
	if rule.Action == "allow" {
		a.State = "auto-allowed"
	} else {
		a.State = "auto-denied"
		d.Message = "Blocked by agent-monitor rule"
		if rule.Note != "" {
			d.Message += ": " + rule.Note
		}
		a.Message = d.Message
	}
	s.recordFinal(a)
	return &d
}

// denyDecision applies deny rules only, for calls a mode allows without asking; nil means not denied.
func (s *ApprovalStore) denyDecision(a *Approval) *approvalDecision {
	rule := s.matchRuleOf(a, "deny")
	if rule == nil {
		return nil
	}
	a.RuleID, a.DecidedBy, a.State = rule.ID, "rule:"+rule.ID, "auto-denied"
	a.Message = "Blocked by agent-monitor rule"
	if rule.Note != "" {
		a.Message += ": " + rule.Note
	}
	s.recordFinal(a)
	return &approvalDecision{Behavior: "deny", Message: a.Message}
}

func (s *ApprovalStore) handleHook(hook string, w http.ResponseWriter, r *http.Request) {
	var p hookPayload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&p); err != nil {
		passHook(w, hook)
		return
	}
	if hook == hookCursor {
		p = cursorPayload(p)
	}
	// A driven thread answers through its own channel; the user's hook passes with no opinion.
	if drivenSessionFn != nil && p.SessionID != "" && drivenSessionFn(p.SessionID) {
		passHook(w, hook)
		return
	}
	a := buildApproval(hook, p)

	s.mu.Lock()
	holdAll := s.settings.ApprovalsMode == approvalsModeHold && s.settings.ModeSet
	s.mu.Unlock()
	// Cursor runs this hook for every command: what its own lists already decide passes through unchanged.
	// The CLI and the desktop app keep separate lists; the desktop ignores an "allow" for a call it would ask about.
	if hook == hookCursor && !holdAll {
		pol := ""
		if cursorFromCLI(p.CursorVersion) {
			pol = cursorPolicy(a.ToolName, a.Input)
		} else if pol = cursorDesktopPolicy(a.ToolName, a.Input); pol == "" && s.settingsSkipWhenFocused() && cursorDesktopInFront() {
			passHook(w, hook)
			return
		}
		if pol != "" {
			out := map[string]any{"permission": pol}
			if pol == "deny" {
				out["user_message"], out["agent_message"] = "Blocked by your Cursor deny list", "Blocked by your Cursor deny list"
			}
			writeJSON(w, out)
			return
		}
	}

	if d := s.ruleDecision(a); d != nil {
		writeJSON(w, hookResponse(hook, a, *d))
		return
	}

	s.mu.Lock()
	settings := s.settings
	s.mu.Unlock()
	// The CLI shows its own prompt now; the card shows everywhere and a decision is typed into the terminal.
	// agent-monitor cannot type into Cursor's prompt, so Cursor holds for a remote decision unless its terminal has focus.
	if hook != hookCursor && settings.ApprovalsMode != approvalsModeHold && s.reachable != nil && s.reachable(a.SessionID) {
		if s.showInTerminal(hook, a, p) {
			notifyWatchedApproval(a)
		}
		passHook(w, hook)
		return
	}
	if a.RuleID == "" && settings.SkipWhenFocused && s.focused != nil && s.focused(a.SessionID) {
		a.State, a.DecidedBy = "passed", "terminal"
		s.recordFinal(a)
		passHook(w, hook)
		return
	}

	hold := settings.HoldSeconds
	if a.Kind != "permission" {
		hold = settings.QuestionHoldSeconds
	}
	a.HoldUntil = a.CreatedAt + int64(hold)*1000
	a.Passable = hook != hookCursor && s.reachable != nil && s.reachable(a.SessionID)
	pa := &pendingApproval{a: a, hook: hook, reply: make(chan approvalDecision, 1)}
	s.mu.Lock()
	s.pending[a.ID] = pa
	snap := copyApproval(a)
	s.mu.Unlock()
	publishV3("approval", "approval", snap)
	notifyWatchedApproval(snap)

	timer := time.NewTimer(time.Duration(hold) * time.Second)
	defer timer.Stop()
	// Focusing the agent's terminal hands the request back to it, so a person at the Mac answers there.
	look := time.NewTicker(2 * time.Second)
	defer look.Stop()
	for {
		select {
		case d := <-pa.reply:
			s.answerHook(w, hook, a, d)
			return
		case <-look.C:
			if hook == hookCursor || s.focused == nil || !settings.SkipWhenFocused || !s.focused(a.SessionID) {
				continue
			}
			if _, ok := s.finish(a.ID, "passed", "terminal", "", nil); ok {
				passHook(w, hook)
				return
			}
		case <-timer.C:
			if _, ok := s.finish(a.ID, "expired", "", "", nil); ok {
				passHook(w, hook)
				return
			}
		case <-r.Context().Done():
			if _, ok := s.finish(a.ID, "passed", "terminal", "", nil); ok {
				return
			}
		}
		break
	}
	// A decision landed at the same moment; a finish with no decision sends nothing, so wait only briefly.
	select {
	case d := <-pa.reply:
		s.answerHook(w, hook, a, d)
	case <-time.After(2 * time.Second):
		passHook(w, hook)
	}
}

func (s *ApprovalStore) answerHook(w http.ResponseWriter, hook string, a *Approval, d approvalDecision) {
	if d.Behavior == "pass" {
		passHook(w, hook)
		return
	}
	writeJSON(w, hookResponse(hook, a, d))
}

func (s *ApprovalStore) settingsSkipWhenFocused() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settings.SkipWhenFocused
}

// passToTerminal releases a held hook with no opinion, so the agent shows its own prompt in the terminal.
func (s *ApprovalStore) passToTerminal(id string, d approvalDecision) (*Approval, int, error) {
	s.mu.Lock()
	p, ok := s.pending[id]
	held := ok && p.term == nil && p.a.Passable
	s.mu.Unlock()
	if !ok {
		return nil, http.StatusConflict, fmt.Errorf("approval is not pending")
	}
	if !held {
		return nil, http.StatusConflict, fmt.Errorf("this request has no terminal to answer in")
	}
	pass := approvalDecision{Behavior: "pass", device: d.device, proof: d.proof}
	done, ok := s.finish(id, "passed", "terminal", "", &pass)
	if !ok {
		return nil, http.StatusConflict, fmt.Errorf("approval is not pending")
	}
	return done, http.StatusOK, nil
}

// passHook answers with no opinion: an empty 204, or "ask" for Cursor, which blocks on an empty reply.
func passHook(w http.ResponseWriter, hook string) {
	if hook == hookCursor {
		writeJSON(w, map[string]any{"permission": "ask"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *ApprovalStore) decide(id string, d approvalDecision) (*Approval, int, error) {
	if d.OptionID != "" {
		s.mu.Lock()
		p, ok := s.pending[id]
		if !ok {
			s.mu.Unlock()
			return nil, http.StatusConflict, fmt.Errorf("approval is not pending") // the same answer as without an option
		}
		if err := applyOption(p.a, &d); err != nil {
			s.mu.Unlock()
			return nil, http.StatusBadRequest, err
		}
		s.mu.Unlock()
	}
	if d.Behavior == "terminal" {
		return s.passToTerminal(id, d)
	}
	if d.Behavior != "allow" && d.Behavior != "deny" {
		return nil, http.StatusBadRequest, fmt.Errorf("behavior must be allow, deny or terminal")
	}
	s.mu.Lock()
	p, ok := s.pending[id]
	var a *Approval
	terminal := false
	if ok {
		a = copyApproval(p.a)
		terminal = p.term != nil
	}
	s.mu.Unlock()
	if !ok {
		return nil, http.StatusConflict, fmt.Errorf("approval is not pending")
	}
	if d.ApplySuggestions && d.Behavior == "allow" {
		d.Always, d.AlwaysScope = true, "claude"
	}
	if d.limited && d.Always {
		return nil, http.StatusForbidden, fmt.Errorf("always adds a lasting rule and needs admin access; allow once instead")
	}
	if d.limited && d.UpdatedInput != nil && a.Kind == "permission" {
		return nil, http.StatusForbidden, fmt.Errorf("an edited command needs admin access")
	}
	if d.UpdatedInput != nil && a.Agent != "claude" {
		return nil, http.StatusBadRequest, fmt.Errorf("updatedInput is supported only for Claude approvals")
	}
	if d.Always && d.UpdatedInput != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("always cannot be combined with an edited input")
	}
	if d.Always && !a.AlwaysOK {
		return nil, http.StatusBadRequest, fmt.Errorf("always is not offered for high-risk or compound commands")
	}
	switch d.AlwaysScope {
	case "", "agent-monitor":
	case "claude":
		if !d.Always || a.Agent != "claude" || len(a.Suggestions) == 0 {
			return nil, http.StatusBadRequest, fmt.Errorf("alwaysScope claude needs always and a Claude approval with permission suggestions")
		}
	default:
		return nil, http.StatusBadRequest, fmt.Errorf("alwaysScope must be agent-monitor or claude")
	}
	if d.Behavior == "allow" && a.Kind == "question" && len(d.Answers) == 0 {
		return nil, http.StatusBadRequest, fmt.Errorf("answers required for a question")
	}
	by := d.Surface
	if by == "" {
		by = "web"
	}
	if terminal {
		return s.decideInTerminal(id, d, by)
	}
	state := "allowed"
	if d.Behavior == "deny" {
		state = "denied"
	}
	done, ok := s.finish(id, state, by, d.Message, &d)
	if !ok {
		return nil, http.StatusConflict, fmt.Errorf("approval is not pending")
	}
	if d.Always && d.Behavior == "allow" && a.Kind == "permission" && d.AlwaysScope != "claude" {
		s.addAlwaysRule(a)
	}
	return done, http.StatusOK, nil
}

func (s *ApprovalStore) addAlwaysRule(a *Approval) {
	r := Rule{Action: "allow", Agent: a.Agent, Tool: a.ToolName, Pattern: alwaysPattern(a), CwdPrefix: a.Cwd}
	if r.Pattern == "" || r.CwdPrefix == "" {
		return // no pattern or no folder: the rule would cover more than the user saw
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.rules {
		if x.Action == r.Action && x.Agent == r.Agent && x.Tool == r.Tool && x.Pattern == r.Pattern && x.CwdPrefix == r.CwdPrefix {
			return
		}
	}
	r.ID = "r_" + newApprovalID()[3:]
	r.CreatedAt = time.Now().UnixMilli()
	r.Note = "always, from " + a.ID
	s.rules = append(s.rules, r)
	s.saveRulesLocked()
}

func (s *ApprovalStore) upsertRule(r Rule) (Rule, error) {
	switch r.Action {
	case "allow", "deny", "hold":
	default:
		return r, fmt.Errorf("action must be allow, deny or hold")
	}
	if r.Agent == "cursor-agent" {
		r.Agent = "cursor" // Cursor approvals carry "cursor" for the editor and the CLI
	}
	if r.Agent != "" && !isRegistryID(r.Agent) && r.Agent != "cursor" {
		return r, fmt.Errorf("unknown agent %q", r.Agent)
	}
	if r.Action == "allow" && r.Tool == "" && r.Pattern == "" {
		return r, fmt.Errorf("an allow rule needs a tool or a pattern")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.ID != "" {
		for i := range s.rules {
			if s.rules[i].ID == r.ID {
				if r.CreatedAt == 0 {
					r.CreatedAt = s.rules[i].CreatedAt
				}
				if r.Hits == 0 {
					r.Hits = s.rules[i].Hits
				}
				s.rules[i] = r
				s.saveRulesLocked()
				return r, nil
			}
		}
	} else {
		r.ID = "r_" + newApprovalID()[3:]
	}
	if r.CreatedAt == 0 {
		r.CreatedAt = time.Now().UnixMilli()
	}
	s.rules = append(s.rules, r)
	s.saveRulesLocked()
	return r, nil
}

func (s *ApprovalStore) deleteRule(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.rules {
		if s.rules[i].ID == id {
			s.rules = append(s.rules[:i], s.rules[i+1:]...)
			s.saveRulesLocked()
			return true
		}
	}
	return false
}

func daemonPort() int {
	if v := os.Getenv("AGENT_MONITOR_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 7777
}

func codexHookScriptPath() string {
	var cands []string
	if exe, err := os.Executable(); err == nil {
		cands = append(cands, filepath.Join(filepath.Dir(exe), "bin", "codex-hook.sh"))
	}
	if wd, err := os.Getwd(); err == nil {
		cands = append(cands, filepath.Join(wd, "bin", "codex-hook.sh"))
	}
	for _, c := range cands {
		if pathExists(c) {
			return c
		}
	}
	return filepath.Join(homeDir(), "agent-monitor", "bin", "codex-hook.sh")
}

func (s *ApprovalStore) hooksSnippet() map[string]any {
	s.mu.Lock()
	st := s.settings
	s.mu.Unlock()
	base := fmt.Sprintf("http://127.0.0.1:%d", daemonPort())
	claude := map[string]any{
		"PermissionRequest": []any{map[string]any{"matcher": "", "hooks": []any{map[string]any{
			"type": "http", "url": base + "/api/approvals/hook/claude-permission", "timeout": st.HoldSeconds + 30}}}},
		"PreToolUse": []any{map[string]any{"matcher": "AskUserQuestion|ExitPlanMode", "hooks": []any{map[string]any{
			"type": "http", "url": base + "/api/approvals/hook/claude-pretool", "timeout": st.QuestionHoldSeconds + 30}}}},
	}
	codex := map[string]any{"hooks": map[string]any{
		"PermissionRequest": []any{map[string]any{"matcher": "", "hooks": []any{map[string]any{
			"type": "command", "command": "AGENT_MONITOR_URL=" + base + " " + codexHookScriptPath(), "timeout": st.HoldSeconds + 30}}}},
	}}
	return map[string]any{"claude": claude, "codex": codex}
}

func (s *ApprovalStore) list() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := make([]*Approval, 0, len(s.pending))
	for _, p := range s.pending {
		pending = append(pending, copyApproval(p.a))
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].CreatedAt < pending[j].CreatedAt })
	recent := make([]*Approval, 0, len(s.recent))
	for i := len(s.recent) - 1; i >= 0; i-- {
		recent = append(recent, s.recent[i])
	}
	return map[string]any{"pending": pending, "recent": recent}
}

func (s *ApprovalStore) routes(mux *http.ServeMux) {
	mux.HandleFunc("/api/approvals", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, s.list())
	})
	mux.HandleFunc("/api/approvals/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/approvals/"), "/"), "/")
		bad := func(code int, msg string) { writeJSONStatus(w, code, map[string]any{"error": msg}) }
		switch {
		case len(parts) == 2 && parts[0] == "hook" && r.Method == http.MethodPost:
			switch parts[1] {
			case hookClaudePermission, hookClaudePretool, hookCodex, hookCursor:
				s.handleHook(parts[1], w, r)
			default:
				bad(http.StatusNotFound, "unknown hook")
			}
		case len(parts) == 1 && parts[0] == "rules":
			switch r.Method {
			case http.MethodGet:
				s.mu.Lock()
				rules := append([]Rule{}, s.rules...)
				s.mu.Unlock()
				writeJSON(w, map[string]any{"rules": rules})
			case http.MethodPost:
				var rule Rule
				if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
					bad(http.StatusBadRequest, "bad json")
					return
				}
				out, err := s.upsertRule(rule)
				if err != nil {
					bad(http.StatusBadRequest, err.Error())
					return
				}
				writeJSON(w, out)
			default:
				bad(http.StatusMethodNotAllowed, "method not allowed")
			}
		case len(parts) == 2 && parts[0] == "rules" && parts[1] == "suggest" && r.Method == http.MethodGet:
			writeJSON(w, map[string]any{"suggestions": s.ruleSuggestions(time.Now())})
		case len(parts) == 4 && parts[0] == "rules" && parts[1] == "suggest" && parts[3] == "dismiss" && r.Method == http.MethodPost:
			if err := s.dismissSuggestion(parts[2]); err != nil {
				code := http.StatusInternalServerError
				if errors.Is(err, errBadSuggestKey) {
					code = http.StatusBadRequest
				}
				bad(code, err.Error())
				return
			}
			writeJSON(w, map[string]any{"ok": true})
		case len(parts) == 2 && parts[0] == "rules" && r.Method == http.MethodDelete:
			if !s.deleteRule(parts[1]) {
				bad(http.StatusNotFound, "rule not found")
				return
			}
			writeJSON(w, map[string]any{"ok": true})
		case len(parts) == 1 && parts[0] == "audit" && r.Method == http.MethodGet:
			since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			if limit <= 0 || limit > 1000 {
				limit = 100
			}
			list, err := s.audit(since, limit)
			if err != nil {
				bad(http.StatusInternalServerError, err.Error())
				return
			}
			writeJSON(w, map[string]any{"approvals": list})
		case len(parts) == 1 && parts[0] == "settings":
			switch r.Method {
			case http.MethodGet:
				s.mu.Lock()
				st := s.settings
				s.mu.Unlock()
				writeJSON(w, st)
			case http.MethodPost:
				var in struct {
					HoldSeconds              *int    `json:"holdSeconds"`
					QuestionHoldSeconds      *int    `json:"questionHoldSeconds"`
					SkipWhenFocused          *bool   `json:"skipWhenFocused"`
					DrivenApprovalTimeoutSec *int    `json:"drivenApprovalTimeoutSec"`
					ApprovalsMode            *string `json:"approvalsMode"`
					BioMinRisk               *string `json:"bioMinRisk"`
					BioTrustMin              *int    `json:"bioTrustMin"`
				}
				if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
					bad(http.StatusBadRequest, "bad json")
					return
				}
				for _, v := range []*int{in.HoldSeconds, in.QuestionHoldSeconds} {
					if v != nil && (*v < 1 || *v > 3600) {
						bad(http.StatusBadRequest, "hold seconds must be 1..3600")
						return
					}
				}
				if v := in.ApprovalsMode; v != nil && *v != approvalsModeBoth && *v != approvalsModeHold {
					bad(http.StatusBadRequest, "approvalsMode must be both or hold")
					return
				}
				if v := in.BioMinRisk; v != nil && *v != "low" && *v != "medium" && *v != "high" {
					bad(http.StatusBadRequest, "bioMinRisk must be low, medium or high")
					return
				}
				if v := in.BioTrustMin; v != nil && (*v < 0 || *v > 120) {
					bad(http.StatusBadRequest, "bioTrustMin must be 0..120")
					return
				}
				if v := in.DrivenApprovalTimeoutSec; v != nil && (*v < 0 || *v > 86400) {
					bad(http.StatusBadRequest, "drivenApprovalTimeoutSec must be 0..86400")
					return
				}
				s.mu.Lock()
				if in.DrivenApprovalTimeoutSec != nil {
					s.settings.DrivenApprovalTimeoutSec = *in.DrivenApprovalTimeoutSec
				}
				if in.HoldSeconds != nil {
					s.settings.HoldSeconds = *in.HoldSeconds
				}
				if in.QuestionHoldSeconds != nil {
					s.settings.QuestionHoldSeconds = *in.QuestionHoldSeconds
				}
				if in.SkipWhenFocused != nil {
					s.settings.SkipWhenFocused = *in.SkipWhenFocused
				}
				if in.ApprovalsMode != nil {
					s.settings.ModeSet = true
					s.settings.ApprovalsMode = *in.ApprovalsMode
				}
				if in.BioMinRisk != nil {
					s.settings.BioMinRisk = *in.BioMinRisk
				}
				if in.BioTrustMin != nil {
					s.settings.BioTrustMin = in.BioTrustMin
				}
				st := s.settings
				err := writeFileAtomic(filepath.Join(s.dir, "approval-settings.json"), st)
				s.mu.Unlock()
				if err != nil {
					bad(http.StatusInternalServerError, err.Error())
					return
				}
				writeJSON(w, st)
			default:
				bad(http.StatusMethodNotAllowed, "method not allowed")
			}
		case len(parts) == 1 && parts[0] == "hooks-snippet" && r.Method == http.MethodGet:
			writeJSON(w, s.hooksSnippet())
		case len(parts) == 2 && parts[1] == "proof" && r.Method == http.MethodGet:
			a := s.auditOne(parts[0])
			if a == nil {
				bad(http.StatusNotFound, "no decided approval with this id")
				return
			}
			writeJSON(w, proofCheck(a))
		case len(parts) == 2 && parts[1] == "decide" && r.Method == http.MethodPost:
			var d approvalDecision
			if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
				bad(http.StatusBadRequest, "bad json")
				return
			}
			d.limited = callerLevel(r) < LevelAdmin
			d.device, d.proof = callerDevice(r), requestProof(r)
			a, code, err := s.decide(parts[0], d)
			if err != nil {
				bad(code, err.Error())
				return
			}
			writeJSON(w, a)
		default:
			bad(http.StatusNotFound, "not found")
		}
	})
}

// applyOption derives behavior and always from a chosen ACP permission option.
func applyOption(a *Approval, d *approvalDecision) error {
	for _, o := range a.Options {
		if o.OptionID != d.OptionID {
			continue
		}
		switch o.Kind {
		case "allow_once", "allow_always":
			d.Behavior = "allow"
			d.Always = o.Kind == "allow_always" && a.AlwaysOK
		default:
			d.Behavior = "deny"
			d.Always = false
		}
		return nil
	}
	return fmt.Errorf("unknown optionId")
}
