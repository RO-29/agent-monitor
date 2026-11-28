package main

import (
	"strings"
	"time"
)

// Phases: the one status every client shows for a session or a thread.
const (
	phaseNeedsYou = "needs-you"
	phaseWorking  = "working"
	phaseReady    = "ready"
	phaseDone     = "done"
)

// needSet lists the store ids that wait for the person: held approvals, MCP prompts, open questions.
// It reads caches only, so the WebSocket path can call it for each event.
func needSet() map[string]bool {
	out := map[string]bool{}
	for id, n := range pendingBySession() {
		if n > 0 {
			out[id] = true
		}
	}
	if permStore != nil {
		for _, p := range permStore.List() {
			if id := p.SessionID; id != "" {
				if !strings.Contains(id, ":") {
					id = "claude:" + id
				}
				out[id] = true
			}
		}
	}
	tqCache.mu.Lock()
	for path, c := range tqCache.m {
		if q := c.q; q != nil && q.Pending && !q.Held && q.SentAt == 0 {
			out["path:"+path] = true
		}
	}
	tqCache.mu.Unlock()
	return out
}

func startOfDay(now time.Time) int64 {
	y, m, d := now.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, now.Location()).UnixMilli()
}

// sessionPhase is the single rule: a request waits → needs-you; a turn runs → working; it ended today → ready; else done.
func sessionPhase(s *Session, need map[string]bool, today int64) string {
	switch {
	case need[s.ID] || (s.TranscriptPath != "" && need["path:"+s.TranscriptPath]):
		return phaseNeedsYou
	case s.State == StateAwaitingPermission && !cursorEcho(s):
		return phaseNeedsYou
	case s.State == StateRunning:
		return phaseWorking
	case markOf(s.ID).hides(s.LastActivityAt, time.Now().UnixMilli()):
		return phaseDone
	case (s.State == StateIdle || s.State == StateAwaitingInput) && s.LastActivityAt >= today:
		return phaseReady
	}
	return phaseDone
}

// withPhase returns copies of the sessions with Phase set; the store's own copies stay unchanged.
func withPhase(in []*Session) []*Session {
	need, today := needSet(), startOfDay(time.Now())
	out := make([]*Session, len(in))
	for i, s := range in {
		cp := *s
		cp.Phase = sessionPhase(s, need, today)
		if s.Tool == ToolClaude && s.State != StateCompleted && s.State != StateAbandoned {
			cp.BgRunning = runningBg(s.TranscriptPath)
		}
		out[i] = &cp
	}
	return out
}

func eventWithPhase(e ServerEvent) ServerEvent {
	if e.Session != nil {
		e.Session = withPhase([]*Session{e.Session})[0]
	}
	if len(e.Sessions) > 0 {
		e.Sessions = withPhase(e.Sessions)
	}
	return e
}

// threadPhase: the most urgent member wins; a driven thread maps its server group.
func threadPhase(t *ThreadV3, byID map[string]*Session, need map[string]bool, today int64) string {
	if t.Kind == "driven" {
		switch t.Group {
		case "need":
			return phaseNeedsYou
		case "working":
			return phaseWorking
		case "ready":
			return phaseReady
		}
		return phaseDone
	}
	best := phaseDone
	if t.Approvals > 0 {
		return phaseNeedsYou
	}
	for _, id := range t.Sessions {
		s := byID[id]
		if s == nil {
			continue
		}
		if p := sessionPhase(s, need, today); phaseRank(p) < phaseRank(best) {
			best = p
		}
	}
	return best
}

func phaseRank(p string) int {
	switch p {
	case phaseNeedsYou:
		return 0
	case phaseWorking:
		return 1
	case phaseReady:
		return 2
	}
	return 3
}

// watchPhases re-sends a session when its waiting state changes, so every client sees the new phase.
func watchPhases(st *Store) {
	go func() {
		prev := map[string]bool{}
		day := startOfDay(time.Now())
		last := time.Now().UnixMilli()
		for range time.Tick(2 * time.Second) {
			now := time.Now()
			// At midnight Ready becomes Done; a snooze that ends brings a thread back. Neither has an event of its own.
			if d := startOfDay(now); d != day {
				for _, s := range st.All() {
					if s.LastActivityAt >= day && s.LastActivityAt < d {
						st.Touch(s.ID)
					}
				}
				day = d
			}
			for _, id := range snoozesEnded(last, now.UnixMilli()) {
				st.Touch(id)
			}
			last = now.UnixMilli()
			cur := needSet()
			for id := range cur {
				if !prev[id] {
					st.Touch(id)
				}
			}
			for id := range prev {
				if !cur[id] {
					st.Touch(id)
				}
			}
			prev = cur
		}
	}()
}
