package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// syscallKill is a var so tests can stub it.
var syscallKill = syscall.Kill

// PaneRegistration links a session to its tmux pane; only the SessionStart hook or the run wrapper creates one.
type PaneRegistration struct {
	AgentID      string `json:"agentId"`              // store-internal id, e.g. "claude:abc123"
	Tool         Tool   `json:"tool"`                 // matches Session.Tool
	SessionID    string `json:"sessionId"`            // tool-native session id
	PaneID       string `json:"paneId"`               // tmux pane id, e.g. "%3"
	TmuxSocket   string `json:"tmuxSocket,omitempty"` // raw $TMUX value (path,pid,session)
	Cwd          string `json:"cwd"`                  // cwd when registered (sanity / display)
	Pid          int    `json:"pid,omitempty"`        // CLI pid at launch (best-effort)
	Alias        string `json:"alias,omitempty"`      // human-friendly name set via /api/pane/name
	RegisteredAt int64  `json:"registeredAt"`         // unix millis
	LastSeenAt   int64  `json:"lastSeenAt"`           // bumped on every send/keys/view
	Source       string `json:"source"`               // "hook" | "wrapper" | "manual"
}

// pendingWrapper holds a wrapper-launched announcement until the matching
// session shows up in the store. TTL'd so a typo'd wrapper doesn't linger.
type pendingWrapper struct {
	Tool       Tool
	PaneID     string
	TmuxSocket string
	Cwd        string
	Pid        int
	CreatedAt  time.Time
}

const pendingTTL = 60 * time.Second

type PaneRegistry struct {
	mu        sync.Mutex
	byAgentID map[string]*PaneRegistration // agentId -> reg
	byAlias   map[string]string            // alias  -> agentId
	pending   []pendingWrapper             // wrapper announcements awaiting pairing
	listeners map[int]func(PaneEvent)
	nextLid   int
	path      string // ~/.agent-monitor/panes.json
}

type PaneEvent struct {
	Kind         string            `json:"kind"` // "pane-register" | "pane-forget" | "pane-name"
	Registration *PaneRegistration `json:"registration,omitempty"`
	AgentID      string            `json:"agentId,omitempty"`
}

func NewPaneRegistry() *PaneRegistry {
	dir := stateDir()
	_ = os.MkdirAll(dir, 0o755)
	pr := &PaneRegistry{
		byAgentID: map[string]*PaneRegistration{},
		byAlias:   map[string]string{},
		listeners: map[int]func(PaneEvent){},
		path:      filepath.Join(dir, "panes.json"),
	}
	pr.loadFromDisk()
	return pr
}

// Subscribe sends every registration change to l, after replaying the current ones.
func (pr *PaneRegistry) Subscribe(l func(PaneEvent)) func() {
	pr.mu.Lock()
	id := pr.nextLid
	pr.nextLid++
	pr.listeners[id] = l
	snapshot := make([]*PaneRegistration, 0, len(pr.byAgentID))
	for _, r := range pr.byAgentID {
		snapshot = append(snapshot, r)
	}
	pr.mu.Unlock()
	for _, r := range snapshot {
		l(PaneEvent{Kind: "pane-register", Registration: r})
	}
	return func() {
		pr.mu.Lock()
		delete(pr.listeners, id)
		pr.mu.Unlock()
	}
}

func (pr *PaneRegistry) broadcast(e PaneEvent) {
	pr.mu.Lock()
	ls := make([]func(PaneEvent), 0, len(pr.listeners))
	for _, l := range pr.listeners {
		ls = append(ls, l)
	}
	pr.mu.Unlock()
	for _, l := range ls {
		func() { defer func() { _ = recover() }(); l(e) }()
	}
}

// Register adds or replaces a registration (manual > wrapper > hook) and evicts others on the same pane.
func (pr *PaneRegistry) Register(reg *PaneRegistration) {
	if t := canonicalPaneTool(reg.Tool, reg.SessionID); t != reg.Tool {
		reg.Tool, reg.AgentID = t, sid(t, reg.SessionID)
	}
	if reg.RegisteredAt == 0 {
		reg.RegisteredAt = time.Now().UnixMilli()
	}
	reg.LastSeenAt = reg.RegisteredAt
	pr.mu.Lock()
	if existing, ok := pr.byAgentID[reg.AgentID]; ok {
		if reg.Alias == "" {
			reg.Alias = existing.Alias
		}
		if sourceRank(reg.Source) < sourceRank(existing.Source) {
			pr.mu.Unlock()
			return
		}
	}
	// Evict every OTHER registration on this pane, and any older entry for the same session under another prefix.
	var evicted []string
	for id, other := range pr.byAgentID {
		samePane := other.PaneID == reg.PaneID
		sameSession := reg.SessionID != "" && other.SessionID == reg.SessionID
		if id == reg.AgentID || !samePane && !sameSession {
			continue
		}
		evicted = append(evicted, id)
		delete(pr.byAgentID, id)
		if other.Alias != "" {
			delete(pr.byAlias, strings.ToLower(other.Alias))
		}
	}
	pr.byAgentID[reg.AgentID] = reg
	if reg.Alias != "" {
		pr.byAlias[strings.ToLower(reg.Alias)] = reg.AgentID
	}
	pr.persistLocked()
	pr.mu.Unlock()
	for _, id := range evicted {
		pr.broadcast(PaneEvent{Kind: "pane-forget", AgentID: id})
	}
	pr.broadcast(PaneEvent{Kind: "pane-register", Registration: reg})
}

// DedupeByPane keeps only the newest registration per pane.
func (pr *PaneRegistry) DedupeByPane() []string {
	pr.mu.Lock()
	// group ids by paneId
	byPane := map[string][]string{}
	for id, r := range pr.byAgentID {
		byPane[r.PaneID] = append(byPane[r.PaneID], id)
	}
	var dropped []string
	for _, ids := range byPane {
		if len(ids) <= 1 {
			continue
		}
		// keep the newest
		newest := ids[0]
		for _, id := range ids[1:] {
			if pr.byAgentID[id].RegisteredAt > pr.byAgentID[newest].RegisteredAt {
				newest = id
			}
		}
		for _, id := range ids {
			if id == newest {
				continue
			}
			r := pr.byAgentID[id]
			delete(pr.byAgentID, id)
			if r.Alias != "" {
				delete(pr.byAlias, strings.ToLower(r.Alias))
			}
			dropped = append(dropped, id)
		}
	}
	if len(dropped) > 0 {
		pr.persistLocked()
	}
	pr.mu.Unlock()
	for _, id := range dropped {
		pr.broadcast(PaneEvent{Kind: "pane-forget", AgentID: id})
	}
	return dropped
}

func sourceRank(s string) int {
	switch s {
	case "manual":
		return 3
	case "wrapper":
		return 2
	case "hook":
		return 1
	}
	return 0
}

// Get returns the registration for an agent id (or nil). A miss falls back
// to the raw session id, so "claude:<id>" finds an entry stored as "codex:<id>".
func (pr *PaneRegistry) Get(agentID string) *PaneRegistration {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	return pr.lookupLocked(agentID)
}

func (pr *PaneRegistry) lookupLocked(id string) *PaneRegistration {
	if r, ok := pr.byAgentID[id]; ok {
		return r
	}
	tool, raw := splitAgentID(id)
	if raw == "" {
		return nil
	}
	var best *PaneRegistration
	for _, r := range pr.byAgentID {
		if r.SessionID != raw {
			continue
		}
		if best == nil || r.Tool == tool && best.Tool != tool || (r.Tool == tool) == (best.Tool == tool) && r.RegisteredAt > best.RegisteredAt {
			best = r
		}
	}
	return best
}

// splitAgentID splits "claude:<id>" into its tool and raw session id; a bare id has no tool.
func splitAgentID(id string) (Tool, string) {
	if i := strings.IndexByte(id, ':'); i > 0 {
		return Tool(id[:i]), id[i+1:]
	}
	return "", id
}

// Touch bumps LastSeenAt. Call before every send/keys/view so a "stale"
// registration can be detected by comparing LastSeen vs current time.
func (pr *PaneRegistry) Touch(agentID string) {
	pr.mu.Lock()
	if r := pr.lookupLocked(agentID); r != nil {
		r.LastSeenAt = time.Now().UnixMilli()
		pr.persistLocked()
	}
	pr.mu.Unlock()
}

// List returns a snapshot of every registration.
func (pr *PaneRegistry) List() []*PaneRegistration {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	out := make([]*PaneRegistration, 0, len(pr.byAgentID))
	for _, r := range pr.byAgentID {
		out = append(out, r)
	}
	return out
}

// GarbageCollect drops registrations whose pane is gone or whose pid is dead, and returns their ids.
func (pr *PaneRegistry) GarbageCollect(panes []TmuxPane) []string {
	// Zero panes means tmux output did not parse; never wipe the registry on it.
	if len(panes) == 0 {
		return nil
	}
	live := make(map[string]bool, len(panes))
	for _, p := range panes {
		live[p.PaneID] = true
	}
	var dropped []string
	pr.mu.Lock()
	for id, r := range pr.byAgentID {
		gone := !live[r.PaneID]
		dead := r.Pid > 0 && !pidAlive(r.Pid)
		if gone || dead {
			dropped = append(dropped, id)
			delete(pr.byAgentID, id)
			if r.Alias != "" {
				delete(pr.byAlias, strings.ToLower(r.Alias))
			}
		}
	}
	if len(dropped) > 0 {
		pr.persistLocked()
	}
	pr.mu.Unlock()
	for _, id := range dropped {
		pr.broadcast(PaneEvent{Kind: "pane-forget", AgentID: id})
	}
	return dropped
}

// pidAlive probes the pid with signal 0.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	// FindProcess always succeeds on Unix, so use raw syscall.
	if err := syscallKill(pid, 0); err != nil {
		return false
	}
	return true
}

// Forget drops a registration entirely (e.g. tmux restarted, agent died).
func (pr *PaneRegistry) Forget(agentID string) {
	pr.mu.Lock()
	r := pr.lookupLocked(agentID)
	if r != nil {
		agentID = r.AgentID
	}
	delete(pr.byAgentID, agentID)
	if r != nil && r.Alias != "" {
		delete(pr.byAlias, strings.ToLower(r.Alias))
	}
	pr.persistLocked()
	pr.mu.Unlock()
	if r != nil {
		pr.broadcast(PaneEvent{Kind: "pane-forget", AgentID: agentID})
	}
}

// SetAlias names an agent; lookup ignores case, and an alias held by another agent is an error.
func (pr *PaneRegistry) SetAlias(agentID, alias string) error {
	alias = strings.TrimSpace(alias)
	pr.mu.Lock()
	defer pr.mu.Unlock()
	r := pr.lookupLocked(agentID)
	if r == nil {
		return errors.New("no registration for agent")
	}
	agentID = r.AgentID
	key := strings.ToLower(alias)
	if alias == "" {
		// Clearing the alias.
		if r.Alias != "" {
			delete(pr.byAlias, strings.ToLower(r.Alias))
		}
		r.Alias = ""
	} else {
		if other, ok := pr.byAlias[key]; ok && other != agentID {
			return errors.New("alias already in use")
		}
		if r.Alias != "" {
			delete(pr.byAlias, strings.ToLower(r.Alias))
		}
		r.Alias = alias
		pr.byAlias[key] = agentID
	}
	pr.persistLocked()
	go pr.broadcast(PaneEvent{Kind: "pane-name", Registration: r})
	return nil
}

// ResolveRecipient matches an alias, agent id, pane id or a 6+ character session id prefix.
func (pr *PaneRegistry) ResolveRecipient(query string) *PaneRegistration {
	if query == "" {
		return nil
	}
	q := strings.TrimSpace(query)
	pr.mu.Lock()
	defer pr.mu.Unlock()
	// alias
	if id, ok := pr.byAlias[strings.ToLower(q)]; ok {
		return pr.byAgentID[id]
	}
	// exact agent id, or the same session under another prefix
	if r := pr.lookupLocked(q); r != nil {
		return r
	}
	// pane id
	if strings.HasPrefix(q, "%") {
		for _, r := range pr.byAgentID {
			if r.PaneID == q {
				return r
			}
		}
	}
	// session-id prefix (>=6 chars, unambiguous)
	if len(q) >= 6 {
		var match *PaneRegistration
		for _, r := range pr.byAgentID {
			if strings.HasPrefix(r.SessionID, q) {
				if match != nil {
					return nil // ambiguous
				}
				match = r
			}
		}
		if match != nil {
			return match
		}
	}
	return nil
}

// AnnouncePending records a wrapper launch to pair with the next session of its tool.
func (pr *PaneRegistry) AnnouncePending(p pendingWrapper) {
	p.CreatedAt = time.Now()
	pr.mu.Lock()
	pr.pending = append(pr.pending, p)
	// GC expired entries while we're here.
	pr.gcPendingLocked()
	pr.mu.Unlock()
}

func (pr *PaneRegistry) gcPendingLocked() {
	cutoff := time.Now().Add(-pendingTTL)
	kept := pr.pending[:0]
	for _, p := range pr.pending {
		if p.CreatedAt.After(cutoff) {
			kept = append(kept, p)
		}
	}
	pr.pending = kept
}

// PairWithSession registers the pane of a pending wrapper that matches a new session.
func (pr *PaneRegistry) PairWithSession(sess *Session) {
	if sess == nil {
		return
	}
	pr.mu.Lock()
	pr.gcPendingLocked()
	var matched *pendingWrapper
	idx := -1
	for i := range pr.pending {
		p := &pr.pending[i]
		if p.Tool != sess.Tool {
			continue
		}
		// cwd match preferred but tolerate when the wrapper had no cwd info.
		if p.Cwd != "" && sess.Cwd != "" && p.Cwd != sess.Cwd {
			continue
		}
		matched = p
		idx = i
		break
	}
	if matched == nil {
		pr.mu.Unlock()
		return
	}
	// Consume the pending entry.
	pr.pending = append(pr.pending[:idx], pr.pending[idx+1:]...)
	reg := &PaneRegistration{
		AgentID:    sess.ID,
		Tool:       sess.Tool,
		SessionID:  sess.SessionID,
		PaneID:     matched.PaneID,
		TmuxSocket: matched.TmuxSocket,
		Cwd:        sess.Cwd,
		Pid:        matched.Pid,
		Source:     "wrapper",
	}
	pr.mu.Unlock()
	pr.Register(reg)
}

// ─── persistence ──────────────────────────────────────────────────────────

type persistedRegistry struct {
	Registrations []*PaneRegistration `json:"registrations"`
}

func (pr *PaneRegistry) persistLocked() {
	tmp := pr.path + ".tmp"
	body, err := json.MarshalIndent(persistedRegistry{Registrations: regsLocked(pr)}, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, pr.path)
}

// regsLocked must be called with pr.mu held.
func regsLocked(pr *PaneRegistry) []*PaneRegistration {
	out := make([]*PaneRegistration, 0, len(pr.byAgentID))
	for _, r := range pr.byAgentID {
		out = append(out, r)
	}
	return out
}

func (pr *PaneRegistry) loadFromDisk() {
	body, err := os.ReadFile(pr.path)
	if err != nil {
		return
	}
	var p persistedRegistry
	if err := json.Unmarshal(body, &p); err != nil {
		return
	}
	pr.mu.Lock()
	defer pr.mu.Unlock()
	for _, r := range p.Registrations {
		if r == nil || r.AgentID == "" {
			continue
		}
		if t := canonicalPaneTool(r.Tool, r.SessionID); t != r.Tool {
			r.Tool, r.AgentID = t, sid(t, r.SessionID)
		}
		pr.byAgentID[r.AgentID] = r
		if r.Alias != "" {
			pr.byAlias[strings.ToLower(r.Alias)] = r.AgentID
		}
	}
}

// paneToolProbe reports which agent wrote a transcript for a session id ("" if none); tests stub it.
var paneToolProbe = func(sessionID string) Tool {
	if claudeSessionExists(sessionID) {
		return ToolClaude
	}
	if codexRolloutPath(sessionID) != "" {
		return ToolCodex
	}
	return ""
}

// canonicalPaneTool corrects the tool a hook claimed for a session. The shared
// tab-title hook posts tool "codex" from Claude sessions too.
func canonicalPaneTool(claimed Tool, sessionID string) Tool {
	if claimed != ToolClaude && claimed != ToolCodex || sessionID == "" || strings.HasPrefix(sessionID, "pane-") {
		return claimed
	}
	other := ToolClaude
	if claimed == ToolClaude {
		other = ToolCodex
	}
	if store != nil && store.Get(sid(claimed, sessionID)) == nil && store.Get(sid(other, sessionID)) != nil {
		return other
	}
	if t := paneToolProbe(sessionID); t != "" {
		return t
	}
	// Codex thread ids are UUIDv7; Claude session ids are UUIDv4.
	if claimed == ToolCodex && uuidVersion(sessionID) == '4' {
		return ToolClaude
	}
	return claimed
}

func uuidVersion(id string) byte {
	if len(id) == 36 && id[8] == '-' && id[13] == '-' {
		return id[14]
	}
	return 0
}

// codexRolloutPath finds the rollout file for a Codex thread id ("" if none).
func codexRolloutPath(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	m, _ := filepath.Glob(filepath.Join(homeDir(), ".codex", "sessions", "*", "*", "*", "rollout-*-"+sessionID+".jsonl"))
	if len(m) == 0 {
		return ""
	}
	return m[len(m)-1]
}
