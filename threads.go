package main

import (
	"sort"
	"strings"
)

// Thread = sessions linked by an EXPLICIT continuation. Edges (DESIGN.md §7):
//   1. clear   — the later session began with /clear, same cwd, and either the
//                same registered tmux pane or a start within 30 min of the
//                previous session's last activity in that cwd
//   2. resume  — Codex parent_thread_id (plain continuation, not a subagent)
//   3. handoff — both first prompts name the same *PROMPT|HANDOFF|RESUME|PLAN|BRIEF*.md
//                in the same cwd (the existing chainRef rule)
//   4. spawn   — Agent/Task children are NOT thread members; they render as lanes.
// The old "every session in a cwd is one chain" rule is gone.

type Thread struct {
	ID        string   `json:"id"` // root session id (oldest member)
	Title     string   `json:"title"`
	Cwd       string   `json:"cwd"`
	Ref       string   `json:"ref,omitempty"` // shared handoff file, when that is the link
	Tools     []string `json:"tools"`
	Sessions  []string `json:"sessions"` // oldest → newest
	State     State    `json:"state"`
	StartedAt int64    `json:"startedAt"`
	LastAt    int64    `json:"lastAt"`
	Tokens    TokenUsage `json:"tokens"`
	Turns     int      `json:"turns"`
	Attention bool     `json:"attention"`
	Model     string   `json:"model,omitempty"`
	Edges     []ThreadEdge `json:"edges"`
	// Filled from trace summaries when available (cheap, cached per session).
	Segments  int      `json:"segments"`
	Compacts  int      `json:"compacts"`
	Clears    int      `json:"clears"`
	Errors    int      `json:"errors"`
	CostUSD   float64  `json:"costUsd"`
	CostEstimated bool `json:"costEstimated"`
	Learnings int      `json:"learnings"`
	Outputs   int      `json:"outputs"`
	LastPoint string   `json:"lastPoint,omitempty"`
	LastOutcome string `json:"lastOutcome,omitempty"`
	SegWeights []int64 `json:"segWeights"`
	SegKinds   []string `json:"segKinds"`
	SessionSegs []int  `json:"sessionSegs"` // number of segments per session, same order as Sessions
}

type ThreadEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"` // clear | resume | handoff
}

const clearContinuationWindowMs = 30 * 60_000

// computeThreads groups sessions into threads. panes maps session id → tmux
// pane id for the currently registered panes.
func computeThreads(sessions []*Session, panes map[string]string) ([]Thread, map[string]string) {
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
	var edges []ThreadEdge
	union := func(a, b, kind string) {
		if a == b {
			return
		}
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[ra] = rb
		}
		edges = append(edges, ThreadEdge{From: a, To: b, Kind: kind})
	}

	byID := map[string]*Session{}
	byCwd := map[string][]*Session{}
	for _, s := range sessions {
		byID[s.ID] = s
		byCwd[s.Cwd] = append(byCwd[s.Cwd], s)
	}

	// (1) /clear continuation.
	for _, g := range byCwd {
		sort.Slice(g, func(i, j int) bool { return g[i].StartedAt < g[j].StartedAt })
		for i := 1; i < len(g); i++ {
			cur := g[i]
			if !startedWithClear(cur.ID) {
				continue
			}
			// candidate predecessor: the latest earlier session in the cwd
			// (same tool) that was still active when this one started
			var best *Session
			for j := i - 1; j >= 0; j-- {
				prev := g[j]
				if prev.Tool != cur.Tool {
					continue
				}
				samePane := panes[prev.ID] != "" && panes[prev.ID] == panes[cur.ID]
				gap := cur.StartedAt - prev.LastActivityAt
				if samePane || (gap > -60_000 && gap < clearContinuationWindowMs) {
					best = prev
					break
				}
				if cur.StartedAt-prev.LastActivityAt > 6*60*60_000 {
					break
				}
			}
			if best != nil {
				union(best.ID, cur.ID, "clear")
			}
		}
	}

	// (2) Codex resume / continuation via parent_thread_id.
	for _, s := range sessions {
		if s.Tool != ToolCodex {
			continue
		}
		if p := codexParentOf(s.SessionID); p != "" {
			if ps, ok := byID["codex:"+p]; ok {
				union(ps.ID, s.ID, "resume")
			}
		}
	}

	// (3) shared handoff reference within a cwd.
	byRef := map[string][]*Session{}
	for _, s := range sessions {
		if r := chainRef(s); r != "" {
			byRef[s.Cwd+"::"+r] = append(byRef[s.Cwd+"::"+r], s)
		}
	}
	for _, g := range byRef {
		sort.Slice(g, func(i, j int) bool { return g[i].StartedAt < g[j].StartedAt })
		for i := 1; i < len(g); i++ {
			union(g[i-1].ID, g[i].ID, "handoff")
		}
	}

	// Collect components.
	comp := map[string][]*Session{}
	for _, s := range sessions {
		comp[find(s.ID)] = append(comp[find(s.ID)], s)
	}
	edgesByRoot := map[string][]ThreadEdge{}
	for _, e := range edges {
		edgesByRoot[find(e.From)] = append(edgesByRoot[find(e.From)], e)
	}

	var threads []Thread
	threadOf := map[string]string{}
	for root, members := range comp {
		sort.Slice(members, func(i, j int) bool { return members[i].StartedAt < members[j].StartedAt })
		t := Thread{ID: members[0].ID, Cwd: members[0].Cwd, Edges: edgesByRoot[root]}
		if t.Edges == nil {
			t.Edges = []ThreadEdge{}
		}
		tools := map[string]bool{}
		var newest *Session
		for _, m := range members {
			t.Sessions = append(t.Sessions, m.ID)
			threadOf[m.ID] = t.ID
			tools[string(m.Tool)] = true
			if t.StartedAt == 0 || m.StartedAt < t.StartedAt {
				t.StartedAt = m.StartedAt
			}
			if m.LastActivityAt > t.LastAt {
				t.LastAt = m.LastActivityAt
				newest = m
			}
			t.Tokens.Input += m.Tokens.Input
			t.Tokens.Output += m.Tokens.Output
			t.Tokens.CacheRead += m.Tokens.CacheRead
			t.Tokens.CacheCreate += m.Tokens.CacheCreate
			t.Turns += m.MessageCount
			if m.State == StateAwaitingPermission {
				t.Attention = true
			}
		}
		for k := range tools {
			t.Tools = append(t.Tools, k)
		}
		sort.Strings(t.Tools)
		if newest != nil {
			t.State = newest.State
			t.Model = newest.Model
			// the most active member decides the state: running beats idle beats the rest
			for _, m := range members {
				if stateRank(m.State) < stateRank(t.State) {
					t.State = m.State
				}
			}
		}
		t.Ref = sharedRef(members)
		t.Title = threadTitle(members, t.Ref)
		threads = append(threads, t)
	}
	sort.Slice(threads, func(i, j int) bool { return threads[i].LastAt > threads[j].LastAt })
	return threads, threadOf
}

func stateRank(s State) int {
	switch s {
	case StateAwaitingPermission:
		return 0
	case StateAwaitingInput:
		return 1
	case StateRunning:
		return 2
	case StateIdle:
		return 3
	case StateCompleted:
		return 4
	}
	return 5
}

func sharedRef(members []*Session) string {
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
	return ""
}

// threadTitle: the newest member with an AI title wins; else the first prompt.
func threadTitle(members []*Session, ref string) string {
	for i := len(members) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(members[i].Title); t != "" && !strings.HasPrefix(t, "<") && !claudeLooksLikeMeta(t) {
			return t
		}
	}
	for i := len(members) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(firstNonEmptyLine(members[i].FirstMessage)); t != "" && !claudeLooksLikeMeta(t) {
			return clipString(t, 100)
		}
	}
	if ref != "" {
		return ref
	}
	cwd := strings.TrimRight(members[0].Cwd, "/")
	if i := strings.LastIndex(cwd, "/"); i >= 0 && i+1 < len(cwd) {
		return cwd[i+1:]
	}
	return "thread"
}

// attachTraceSummaries fills the per-thread trace roll-ups from the summary
// cache. Sessions without a computed summary contribute zero (the list is
// never blocked on parsing a transcript).
func attachTraceSummaries(threads []Thread, byID map[string]*Session) {
	for i := range threads {
		t := &threads[i]
		t.SegWeights = []int64{}
		t.SegKinds = []string{}
		t.SessionSegs = []int{}
		var newestSummary *TraceSummary
		var newestAt int64
		for _, sid := range t.Sessions {
			s := byID[sid]
			if s == nil {
				t.SessionSegs = append(t.SessionSegs, 0)
				continue
			}
			sum := cachedSummary(s)
			if sum == nil {
				t.SessionSegs = append(t.SessionSegs, 0)
				continue
			}
			t.Segments += sum.Segments
			t.Compacts += sum.Compacts
			t.Clears += sum.Clears
			t.Errors += sum.Errors
			t.CostUSD += sum.CostUSD
			if sum.CostEstimated {
				t.CostEstimated = true
			}
			t.Learnings += sum.Learnings
			t.Outputs += sum.Outputs
			t.SegWeights = append(t.SegWeights, sum.SegWeights...)
			t.SegKinds = append(t.SegKinds, sum.SegKinds...)
			t.SessionSegs = append(t.SessionSegs, sum.Segments)
			if s.LastActivityAt >= newestAt {
				newestAt = s.LastActivityAt
				newestSummary = sum
			}
		}
		if newestSummary != nil {
			t.LastPoint = newestSummary.LastPoint
			t.LastOutcome = newestSummary.LastOutcome
		}
	}
}

// chainsFromThreads keeps the legacy /api/chains shape (iOS + old clients).
func chainsFromThreads(threads []Thread, sessions []*Session) ([]Chain, map[string]string) {
	var chains []Chain
	chainOf := map[string]string{}
	for _, t := range threads {
		if len(t.Sessions) < 2 {
			continue
		}
		c := Chain{ID: t.ID, Cwd: t.Cwd, Ref: firstNonEmpty(t.Ref, t.Title), Sessions: t.Sessions}
		chains = append(chains, c)
		for _, sid := range t.Sessions {
			chainOf[sid] = t.ID
		}
	}
	return chains, chainOf
}
