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

// syscallKill is wrapped in its own var so the helper above stays Linux
// build-portable and tests can stub it. syscall.Kill(pid, 0) is the
// standard "is this process alive?" probe.
var syscallKill = syscall.Kill

// PaneRegistration is the canonical link between an agent-monitor session and
// the tmux pane that hosts its CLI. Heuristic matching is gone — every pane
// shown in the bridge MUST have a registration created via the SessionStart
// hook (Claude) or the `agent-monitor run` wrapper (everything else).
type PaneRegistration struct {
	AgentID      string `json:"agentId"`               // store-internal id, e.g. "claude:abc123"
	Tool         Tool   `json:"tool"`                  // matches Session.Tool
	SessionID    string `json:"sessionId"`             // tool-native session id
	PaneID       string `json:"paneId"`                // tmux pane id, e.g. "%3"
	TmuxSocket   string `json:"tmuxSocket,omitempty"`  // raw $TMUX value (path,pid,session)
	Cwd          string `json:"cwd"`                   // cwd when registered (sanity / display)
	Pid          int    `json:"pid,omitempty"`         // CLI pid at launch (best-effort)
	Alias        string `json:"alias,omitempty"`       // human-friendly name set via /api/pane/name
	RegisteredAt int64  `json:"registeredAt"`          // unix millis
	LastSeenAt   int64  `json:"lastSeenAt"`            // bumped on every send/keys/view
	Source       string `json:"source"`                // "hook" | "wrapper" | "manual"
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
	dir := filepath.Join(homeDir(), ".agent-monitor")
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

// Subscribe broadcasts every registration change to the listener. Replays
// current registrations on subscribe so a freshly-connected web client gets
// the full picture.
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

// Register creates or replaces a registration. Source ("hook"/"wrapper"/"manual")
// is used for diagnostics and to decide whether to overwrite an existing entry
// for the same agent id (manual > wrapper > hook so a user pin can't be
// silently clobbered).
//
// Critically, this also EVICTS any other registration that points at the same
// paneId — a tmux pane only ever runs one agent at a time, so a fresh
// announce/hook registration means the previous occupant is gone. Without
// this the registry accumulates ghosts: every time you `agent-monitor run
// claude` in the same pane, the previous claude's row stuck around.
func (pr *PaneRegistry) Register(reg *PaneRegistration) {
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
	// Evict every OTHER registration that's currently bound to this pane.
	var evicted []string
	for id, other := range pr.byAgentID {
		if id == reg.AgentID || other.PaneID != reg.PaneID {
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

// DedupeByPane is a one-shot sweep: for each tmux pane, keep only the
// most-recently-registered entry. Older entries on the same pane get dropped.
// Used at daemon startup to clean up registries built up before the
// register-time eviction logic existed.
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

// Get returns the registration for an agent id (or nil).
func (pr *PaneRegistry) Get(agentID string) *PaneRegistration {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	return pr.byAgentID[agentID]
}

// Touch bumps LastSeenAt. Call before every send/keys/view so a "stale"
// registration can be detected by comparing LastSeen vs current time.
func (pr *PaneRegistry) Touch(agentID string) {
	pr.mu.Lock()
	if r, ok := pr.byAgentID[agentID]; ok {
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

// GarbageCollect drops registrations that are no longer plausible. Two checks:
//
//  1. pane_id is gone from tmux (tmux restart, pane closed)
//  2. the agent's pid is no longer alive (the CLI exited; the pane was
//     reused by a new shell or a different agent)
//
// Without (2) we accumulate orphan rows: every time you `agent-monitor run
// claude` in the same pane, the previous claude's registration sticks
// around even though that agent is dead. (2) catches that.
//
// Returns the agent ids it removed so the caller can broadcast pane-forget
// events. Cheap: ~5ms tmux fork plus an O(N) syscall per registration.
func (pr *PaneRegistry) GarbageCollect(panes []TmuxPane) []string {
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

// pidAlive returns true if the given pid is still running (or, more
// accurately, signalable by us). Uses syscall.Kill(pid, 0) which sends no
// signal — just probes the existence/permissions of the process.
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
	r := pr.byAgentID[agentID]
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

// SetAlias attaches a human-friendly name to an agent id. Aliases are
// case-insensitive on lookup but stored as the user typed them. Returns an
// error if the alias is already used by a different agent.
func (pr *PaneRegistry) SetAlias(agentID, alias string) error {
	alias = strings.TrimSpace(alias)
	pr.mu.Lock()
	defer pr.mu.Unlock()
	r := pr.byAgentID[agentID]
	if r == nil {
		return errors.New("no registration for agent")
	}
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

// ResolveRecipient maps any of: alias, exact agent id, pane id (%3), or the
// first 6+ chars of a tool-native session id back to the registered agent.
// Returns nil if nothing matches.
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
	// exact agent id
	if r, ok := pr.byAgentID[q]; ok {
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

// AnnouncePending records a wrapper-launched agent waiting to be paired with
// the next session of its tool. Returns a token the wrapper doesn't actually
// need to remember — the pairing is automatic when the session arrives.
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

// PairWithSession is called by the store-listener whenever a NEW session is
// upserted. It looks for a pending wrapper announcement matching the session's
// tool/cwd and registers the pane if found.
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
		pr.byAgentID[r.AgentID] = r
		if r.Alias != "" {
			pr.byAlias[strings.ToLower(r.Alias)] = r.AgentID
		}
	}
}
