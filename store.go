package main

import (
	"sync"
	"time"
)

const (
	// Decay only applies to sessions we've seen live. Old transcripts are
	// classified by age once at seed time and never re-decay.
	idleAfter      = 5 * time.Minute
	completedAfter = 60 * time.Minute
	maxRecent      = 80
)

type Listener func(ServerEvent)

type Store struct {
	mu        sync.Mutex
	sessions  map[string]*Session
	listeners map[int]Listener
	nextID    int
}

func NewStore() *Store {
	return &Store{
		sessions:  map[string]*Session{},
		listeners: map[int]Listener{},
	}
}

func sid(tool Tool, sessionID string) string {
	return string(tool) + ":" + sessionID
}

func (s *Store) Subscribe(l Listener) func() {
	s.mu.Lock()
	id := s.nextID
	s.nextID++
	s.listeners[id] = l
	snap := make([]*Session, 0, len(s.sessions))
	for _, v := range s.sessions {
		snap = append(snap, v)
	}
	s.mu.Unlock()
	l(ServerEvent{Kind: "snapshot", Sessions: snap})
	return func() {
		s.mu.Lock()
		delete(s.listeners, id)
		s.mu.Unlock()
	}
}

func (s *Store) Get(id string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.sessions[id]; ok {
		// Return a shallow copy so the caller can't mutate live state.
		cp := *v
		return &cp
	}
	return nil
}

func (s *Store) All() []*Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Session, 0, len(s.sessions))
	for _, v := range s.sessions {
		out = append(out, v)
	}
	return out
}

// LoadHistorical merges persisted sessions into the store, skipping any id that
// is already present so a live scan/hook always wins over stale disk state.
// Returns how many were newly added. Used once at startup from SQLite.
func (s *Store) LoadHistorical(sessions []*Session) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	added := 0
	for _, sess := range sessions {
		if sess == nil || sess.ID == "" {
			continue
		}
		if _, exists := s.sessions[sess.ID]; exists {
			continue
		}
		cp := *sess
		s.sessions[sess.ID] = &cp
		added++
	}
	return added
}

func (s *Store) broadcastLocked(e ServerEvent) {
	ls := make([]Listener, 0, len(s.listeners))
	for _, l := range s.listeners {
		ls = append(ls, l)
	}
	// Release lock during dispatch so a slow listener can't block the store.
	s.mu.Unlock()
	for _, l := range ls {
		func() {
			defer func() { _ = recover() }()
			l(e)
		}()
	}
	s.mu.Lock()
}

type UpsertInput struct {
	Tool              Tool
	SessionID         string
	Cwd               string
	HasCwd            bool
	Pid               int
	HasPid            bool
	State             State
	HasState          bool
	Message           string
	HasMessage        bool
	PermissionMessage string
	HasPerm           bool
	EventKind         string
	EventText         string
	Title             string
	HasTitle          bool
	FirstMessage      string
	HasFirst          bool
	Model             string
	HasModel          bool
	Branch            string
	HasBranch         bool
	Mode              string
	HasMode           bool
	StartedAtOverride      int64 // 0 = use existing or now
	LastActivityAtOverride int64 // 0 = use existing or now (anchored to a file mtime when seeding from disk)
	IncrementMsg           bool
	MessageCountSet        int  // absolute count to set (overrides existing)
	HasMessageCount        bool
	TranscriptPath    string
	HasTranscript     bool
	Tokens            *TokenUsage // if set, replaces session tokens
	ToolUsageDelta    map[string]int
	SubagentDelta     int
	FilesTouchedSet   int // absolute count to set
	HasFiles          bool
	BgTasksSet        int
	HasBgTasks        bool
}

func (s *Store) Upsert(in UpsertInput) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := sid(in.Tool, in.SessionID)
	now := time.Now().UnixMilli()
	prev := s.sessions[id]

	var events []RecentEvent
	if prev != nil {
		events = prev.RecentEvents
	}
	if in.EventKind != "" {
		txt := in.EventText
		if len(txt) > 400 {
			txt = txt[:400]
		}
		events = append(events, RecentEvent{Ts: now, Kind: in.EventKind, Text: txt})
		if len(events) > maxRecent {
			events = events[len(events)-maxRecent:]
		}
	}

	next := &Session{ID: id, Tool: in.Tool, SessionID: in.SessionID, RecentEvents: events}

	switch {
	case in.HasCwd:
		next.Cwd = in.Cwd
	case prev != nil:
		next.Cwd = prev.Cwd
	}
	switch {
	case in.HasPid:
		next.Pid = in.Pid
	case prev != nil:
		next.Pid = prev.Pid
	}
	switch {
	case in.HasState:
		next.State = in.State
	case prev != nil:
		next.State = prev.State
	default:
		next.State = StateRunning
	}
	switch {
	case in.StartedAtOverride > 0:
		next.StartedAt = in.StartedAtOverride
	case prev != nil:
		next.StartedAt = prev.StartedAt
	default:
		next.StartedAt = now
	}
	switch {
	case in.LastActivityAtOverride > 0:
		next.LastActivityAt = in.LastActivityAtOverride
	case in.HasState && (in.State == StateCompleted || in.State == StateAbandoned):
		if prev != nil {
			next.LastActivityAt = prev.LastActivityAt
		} else {
			next.LastActivityAt = now
		}
	default:
		next.LastActivityAt = now
	}
	switch {
	case in.HasMessage:
		next.LastMessage = in.Message
	case prev != nil:
		next.LastMessage = prev.LastMessage
	}
	// Permission message lifecycle:
	//   - entering awaiting-permission: take provided perm/message, fall back to prev
	//   - any other state transition: clear it
	//   - no state change: keep prev
	if in.HasState && in.State == StateAwaitingPermission {
		switch {
		case in.HasPerm:
			next.PermissionMessage = in.PermissionMessage
		case in.HasMessage:
			next.PermissionMessage = in.Message
		case prev != nil:
			next.PermissionMessage = prev.PermissionMessage
		}
	} else if in.HasState {
		next.PermissionMessage = ""
	} else if prev != nil {
		next.PermissionMessage = prev.PermissionMessage
	}

	switch {
	case in.HasTitle:
		next.Title = in.Title
	case prev != nil:
		next.Title = prev.Title
	}
	switch {
	case in.HasFirst:
		next.FirstMessage = in.FirstMessage
	case prev != nil:
		next.FirstMessage = prev.FirstMessage
	}
	switch {
	case in.HasModel:
		next.Model = in.Model
	case prev != nil:
		next.Model = prev.Model
	}
	switch {
	case in.HasBranch:
		next.Branch = in.Branch
	case prev != nil:
		next.Branch = prev.Branch
	}
	switch {
	case in.HasMode:
		next.Mode = in.Mode
	case prev != nil:
		next.Mode = prev.Mode
	}
	if prev != nil {
		next.MessageCount = prev.MessageCount
		next.Tokens = prev.Tokens
		next.ToolUsage = prev.ToolUsage
		next.SubagentCount = prev.SubagentCount
		next.FilesTouched = prev.FilesTouched
		next.BgTasksCount = prev.BgTasksCount
		next.TranscriptPath = prev.TranscriptPath
	}
	if in.IncrementMsg {
		next.MessageCount++
	}
	if in.HasMessageCount {
		next.MessageCount = in.MessageCountSet
	}
	if in.HasTranscript {
		next.TranscriptPath = in.TranscriptPath
	}
	if in.Tokens != nil {
		next.Tokens = *in.Tokens
	}
	if len(in.ToolUsageDelta) > 0 {
		if next.ToolUsage == nil {
			next.ToolUsage = map[string]int{}
		}
		for k, v := range in.ToolUsageDelta {
			next.ToolUsage[k] += v
		}
	}
	if in.SubagentDelta > 0 {
		next.SubagentCount += in.SubagentDelta
	}
	if in.HasFiles {
		next.FilesTouched = in.FilesTouchedSet
	}
	if in.HasBgTasks {
		next.BgTasksCount = in.BgTasksSet
	}

	s.sessions[id] = next
	s.broadcastLocked(ServerEvent{Kind: "upsert", Session: next})
	return next
}

func (s *Store) Remove(tool Tool, sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := sid(tool, sessionID)
	if _, ok := s.sessions[id]; !ok {
		return
	}
	delete(s.sessions, id)
	s.broadcastLocked(ServerEvent{Kind: "remove", ID: id})
}

func (s *Store) SetState(tool Tool, sessionID string, state State, msg string) {
	s.mu.Lock()
	prev, ok := s.sessions[sid(tool, sessionID)]
	s.mu.Unlock()
	if !ok {
		return
	}
	if prev.State == state && msg == "" {
		return
	}
	in := UpsertInput{
		Tool: tool, SessionID: sessionID,
		State: state, HasState: true,
		EventKind: "state:" + string(state), EventText: msg,
	}
	if msg != "" {
		in.Message = msg
		in.HasMessage = true
	}
	s.Upsert(in)
}

// StartDecayLoop transitions running -> idle -> completed for sessions we've
// observed live. Awaiting/completed/abandoned are sticky terminal states.
// Sessions seeded from old transcripts are classified once at seed time
// (their state never decays further — being old isn't the same as being stuck).
func (s *Store) StartDecayLoop() {
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for range t.C {
			s.tick()
		}
	}()
}

func (s *Store) tick() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UnixMilli()
	for _, v := range s.sessions {
		dt := time.Duration(now-v.LastActivityAt) * time.Millisecond
		var next State
		switch {
		case v.State == StateRunning && dt > idleAfter:
			next = StateIdle
		case v.State == StateIdle && dt > completedAfter:
			next = StateCompleted
		case (v.State == StateAwaitingPermission || v.State == StateAwaitingInput) && dt > completedAfter:
			// Safety net: an awaiting session with no activity for an hour is
			// stale (answered elsewhere, agent gone). Don't let it linger in the
			// Approvals tab forever. Paned prompts still on screen get re-flagged
			// by the pane watcher, so only truly-abandoned asks fall through here.
			next = StateCompleted
		default:
			continue
		}
		v.State = next
		s.broadcastLocked(ServerEvent{Kind: "upsert", Session: v})
	}
}

// ClassifyAge returns the appropriate state for a session of the given age,
// based on time-since-last-activity. Used at seed time so old transcripts get
// a sensible state (idle / completed / abandoned) rather than transient ones
// like running / stuck.
func ClassifyAge(ageMs int64) State {
	switch {
	case ageMs < 30_000:
		return StateRunning
	case ageMs < int64(idleAfter/time.Millisecond):
		return StateIdle
	case ageMs < 24*60*60*1000:
		return StateCompleted
	default:
		return StateAbandoned
	}
}
