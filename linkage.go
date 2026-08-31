package main

import (
	"regexp"
	"strings"
	"sync"
)

// SpawnRef records an Agent/Task/Workflow tool call that launched a child
// agent, captured during the transcript scan so linkage can match the child
// session it spawned.
type SpawnRef struct {
	Name   string `json:"name"`   // Agent | Task | Workflow
	Prompt string `json:"prompt"` // first line of the prompt/description passed
	Ts     int64  `json:"ts"`
}

// spawnStore holds each session's spawn calls (agentID → []SpawnRef), populated
// during the scan. Kept out of the Session/Upsert pipeline to avoid plumbing.
var spawnStore = struct {
	mu sync.RWMutex
	m  map[string][]SpawnRef
}{m: map[string][]SpawnRef{}}

func setSpawns(agentID string, spawns []SpawnRef) {
	spawnStore.mu.Lock()
	defer spawnStore.mu.Unlock()
	if len(spawns) == 0 {
		delete(spawnStore.m, agentID)
		return
	}
	spawnStore.m[agentID] = spawns
}

// Sessions whose transcript began with /clear — i.e. a continuation of the
// prior session in the same terminal. Captured during the scan.
var clearStore = struct {
	mu sync.RWMutex
	m  map[string]bool
}{m: map[string]bool{}}

func setStartedWithClear(agentID string, v bool) {
	clearStore.mu.Lock()
	defer clearStore.mu.Unlock()
	if v {
		clearStore.m[agentID] = true
	} else {
		delete(clearStore.m, agentID)
	}
}
func startedWithClear(agentID string) bool {
	clearStore.mu.RLock()
	defer clearStore.mu.RUnlock()
	return clearStore.m[agentID]
}

func normPrompt(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.Join(strings.Fields(s), " ") // collapse whitespace
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

type SpawnChild struct {
	ID     string `json:"id"`
	Prompt string `json:"prompt"`
	Name   string `json:"name"`
}

// matchSpawns links each spawn call to the child session it launched — by
// prompt-prefix match within a time window after the call. Returns child→parent
// and parent→children maps.
func matchSpawns(sessions []*Session) (map[string]string, map[string][]SpawnChild) {
	parentOf := map[string]string{}
	childrenOf := map[string][]SpawnChild{}

	spawnStore.mu.RLock()
	snapshot := make(map[string][]SpawnRef, len(spawnStore.m))
	for k, v := range spawnStore.m {
		snapshot[k] = v
	}
	spawnStore.mu.RUnlock()

	for _, parent := range sessions {
		spawns := snapshot[parent.ID]
		if len(spawns) == 0 {
			continue
		}
		for _, sp := range spawns {
			np := normPrompt(sp.Prompt)
			if len(np) < 8 {
				continue // too short to match reliably
			}
			var best *Session
			var bestDelta int64 = 1<<62 - 1
			for _, c := range sessions {
				if c.ID == parent.ID || parentOf[c.ID] != "" {
					continue
				}
				// child must start at/after the spawn (allow small skew) & within 30m
				d := c.StartedAt - sp.Ts
				if d < -60_000 || d > 30*60_000 {
					continue
				}
				nc := normPrompt(firstNonEmpty(c.FirstMessage, c.Title))
				if nc == "" || !(strings.HasPrefix(nc, np[:min(len(np), len(nc))]) || strings.HasPrefix(np, nc[:min(len(nc), len(np))])) {
					continue
				}
				ad := d
				if ad < 0 {
					ad = -ad
				}
				if ad < bestDelta {
					bestDelta, best = ad, c
				}
			}
			if best != nil {
				parentOf[best.ID] = parent.ID
				childrenOf[parent.ID] = append(childrenOf[parent.ID], SpawnChild{ID: best.ID, Prompt: sp.Prompt, Name: sp.Name})
			}
		}
	}
	return parentOf, childrenOf
}

// Session linkage — groups related sessions into "chains" so the UI can show
// that work continued across /clear boundaries (and later, spawned children).
//
// Chosen strategy is PRECISE: link only on strong signals —
//   1. a shared handoff / resume "source of truth" plan file, and
//   2. same-pane consecutive runs (a /clear continuation).
// Same-folder-and-time alone is deliberately NOT enough (avoids false links).

// A plan/prompt file the session's first prompt points at, e.g.
// "handoff_session_plan/RESUME_PROMPT.md" or a bare "E2E_QA_PROMPT.md".
var reHandoff = regexp.MustCompile(`(?i)(?:handoff_session_plan/|source of truth[:\s]+)?([A-Za-z0-9_-]*(?:PROMPT|HANDOFF|RESUME|PLAN|BRIEF)[A-Za-z0-9_-]*\.md)`)

// chainRef returns the normalized plan-file reference a session continues from,
// or "" if none — the primary precise linkage key.
func chainRef(s *Session) string {
	hay := s.FirstMessage
	if hay == "" {
		hay = s.Title
	}
	if m := reHandoff.FindStringSubmatch(hay); m != nil {
		return strings.ToLower(m[1])
	}
	return ""
}

type Chain struct {
	ID       string   `json:"id"`       // stable key: "<cwd>::<ref>"
	Cwd      string   `json:"cwd"`      //
	Ref      string   `json:"ref"`      // the plan file the chain follows
	Sessions []string `json:"sessions"` // member ids, oldest → newest
}
