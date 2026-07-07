package main

import (
	"regexp"
	"sort"
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

// computeChains groups related sessions with a union-find over three precise
// edge types:
//   1. /clear continuation — a session that began with /clear links to the
//      previous session in the same cwd (the terminal it was cleared in).
//   2. shared handoff/resume plan reference within a cwd.
//   3. same tmux pane (when the registry knows it).
// Returns multi-member chains (oldest→newest) + a sessionID→chainID lookup.
func computeChains(sessions []*Session, panes map[string]string) ([]Chain, map[string]string) {
	parent := map[string]string{}
	for _, s := range sessions {
		parent[s.ID] = s.ID
	}
	var find func(string) string
	find = func(x string) string {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	union := func(a, b string) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[ra] = rb
		}
	}

	byCwd := map[string][]*Session{}
	for _, s := range sessions {
		byCwd[s.Cwd] = append(byCwd[s.Cwd], s)
	}
	home := homeDir()
	for cwd, g := range byCwd {
		sort.Slice(g, func(i, j int) bool { return g[i].StartedAt < g[j].StartedAt })
		if cwd != "" && cwd != home {
			// (1a) Project directory: every session in it is one project chain —
			//      this is what "the project has been running for a week" means.
			for i := 1; i < len(g); i++ {
				union(g[0].ID, g[i].ID)
			}
		} else {
			// (1b) Bare home dir: too generic to chain wholesale (unrelated
			//      one-offs), so link only true /clear continuations there.
			for i := 1; i < len(g); i++ {
				if startedWithClear(g[i].ID) {
					union(g[i-1].ID, g[i].ID)
				}
			}
		}
	}

	// (2) shared plan reference within a cwd.
	byRef := map[string][]*Session{}
	for _, s := range sessions {
		if r := chainRef(s); r != "" {
			byRef[s.Cwd+"::"+r] = append(byRef[s.Cwd+"::"+r], s)
		}
	}
	for _, g := range byRef {
		for i := 1; i < len(g); i++ {
			union(g[0].ID, g[i].ID)
		}
	}

	// (3) same tmux pane.
	byPane := map[string][]*Session{}
	for _, s := range sessions {
		if p := panes[s.ID]; p != "" {
			byPane[p] = append(byPane[p], s)
		}
	}
	for _, g := range byPane {
		for i := 1; i < len(g); i++ {
			union(g[0].ID, g[i].ID)
		}
	}

	// Collect connected components.
	comp := map[string][]*Session{}
	for _, s := range sessions {
		r := find(s.ID)
		comp[r] = append(comp[r], s)
	}
	byID := map[string]*Session{}
	for _, s := range sessions {
		byID[s.ID] = s
	}

	var chains []Chain
	chainOf := map[string]string{}
	for root, members := range comp {
		if len(members) < 2 {
			continue
		}
		sort.Slice(members, func(i, j int) bool { return members[i].StartedAt < members[j].StartedAt })
		ids := make([]string, len(members))
		for i, m := range members {
			ids[i] = m.ID
			chainOf[m.ID] = root
		}
		chains = append(chains, Chain{ID: root, Cwd: members[0].Cwd, Ref: chainLabel(members), Sessions: ids})
	}
	sort.Slice(chains, func(i, j int) bool {
		return lastActivityOfChain(chains[i], sessions) > lastActivityOfChain(chains[j], sessions)
	})
	return chains, chainOf
}

// chainLabel names a chain: if every member shares one plan reference, use it;
// otherwise the project folder name (a whole-cwd chain); else the first title.
func chainLabel(members []*Session) string {
	refs := map[string]bool{}
	for _, m := range members {
		if r := chainRef(m); r != "" {
			refs[r] = true
		}
	}
	if len(refs) == 1 {
		for r := range refs {
			return r
		}
	}
	cwd := strings.TrimRight(members[0].Cwd, "/")
	if i := strings.LastIndex(cwd, "/"); i >= 0 && i+1 < len(cwd) {
		return cwd[i+1:]
	}
	if t := strings.TrimSpace(members[0].Title); t != "" {
		return t
	}
	return "chain"
}

func lastActivityOfChain(c Chain, sessions []*Session) int64 {
	byID := map[string]*Session{}
	for _, s := range sessions {
		byID[s.ID] = s
	}
	var mx int64
	for _, id := range c.Sessions {
		if s := byID[id]; s != nil && s.LastActivityAt > mx {
			mx = s.LastActivityAt
		}
	}
	return mx
}
