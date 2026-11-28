package main

import (
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ── child links: Claude subagents, Codex subagents, Codex runs started by a Bash call ──

type ChildLink struct {
	Parent string `json:"parent"` // parent store id
	Child  string `json:"child"`  // child store id
	Kind   string `json:"kind"`   // subagent | delegated
	SpanID string `json:"spanId,omitempty"`
	Ts     int64  `json:"ts"`
}

var reCodexExec = regexp.MustCompile(`(^|[\s/;&|(])codex\s+(exec|e)(\s|$)`)

var childCache = struct {
	sync.Mutex
	at    int64
	links []ChildLink
}{}

const childLookback = 3 * 24 * time.Hour

// childLinks returns every parent→child link among recent sessions, cached for 30 s.
func childLinks() []ChildLink {
	childCache.Lock()
	defer childCache.Unlock()
	now := time.Now().UnixMilli()
	if childCache.links != nil && now-childCache.at < 30_000 {
		return childCache.links
	}
	childCache.links = computeChildLinks(store.All(), now)
	childCache.at = now
	return childCache.links
}

func computeChildLinks(sessions []*Session, now int64) []ChildLink {
	links := []ChildLink{}
	used := map[string]bool{}
	var codex []*Session
	for _, s := range sessions {
		if s.Tool == ToolCodex {
			codex = append(codex, s)
			if p := codexParentOf(s.SessionID); p != "" {
				links = append(links, ChildLink{Parent: "codex:" + p, Child: s.ID, Kind: "subagent", Ts: s.StartedAt})
				used[s.ID] = true
			}
		}
	}
	sort.Slice(codex, func(i, j int) bool { return codex[i].StartedAt < codex[j].StartedAt })
	cutoff := now - childLookback.Milliseconds()
	for _, s := range sessions {
		if s.Tool != ToolClaude || s.LastActivityAt < cutoff {
			continue
		}
		for _, call := range codexCallsOf(s) {
			var c *Session
			if d := cmdDir(call.cmd, s.Cwd); d != "" {
				c = matchCodexRun(codex, used, d, call.ts)
			} else {
				c = matchCodexRun(codex, used, s.Cwd, call.ts)
			}
			if c == nil {
				c = matchCodexByTime(codex, used, call.ts)
			}
			if c != nil {
				used[c.ID] = true
				links = append(links, ChildLink{Parent: s.ID, Child: c.ID, Kind: "delegated", SpanID: call.spanID, Ts: call.ts})
			}
		}
	}
	return links
}

// codexCall is a Bash call in a Claude trace that runs codex exec.
type codexCall struct {
	spanID string
	ts     int64
	cmd    string
}

// claudeFiles fingerprints a Claude transcript and its subagent folder, which together decide its trace.
type claudeFiles struct {
	path              string
	size, mtime       int64
	subN              int
	subSize, subMtime int64
}

func claudeFilesOf(s *Session) (claudeFiles, bool) {
	st, err := os.Stat(s.TranscriptPath)
	if err != nil {
		return claudeFiles{}, false
	}
	fp := claudeFiles{path: s.TranscriptPath, size: st.Size(), mtime: st.ModTime().UnixNano()}
	ents, _ := os.ReadDir(filepath.Join(filepath.Dir(s.TranscriptPath), s.SessionID, "subagents"))
	for _, e := range ents {
		if info, err := e.Info(); err == nil {
			fp.subN++
			fp.subSize += info.Size()
			fp.subMtime = max(fp.subMtime, info.ModTime().UnixNano())
		}
	}
	return fp, true
}

// codexCalls caches each session's codexCall list, so child links need no trace while its files are unchanged.
var codexCalls = struct {
	sync.Mutex
	m map[string]codexCallsEnt
}{m: map[string]codexCallsEnt{}}

type codexCallsEnt struct {
	fp    claudeFiles
	calls []codexCall
	at    int64
}

func codexCallsOf(s *Session) []codexCall {
	if s.TranscriptPath == "" {
		return nil
	}
	fp, ok := claudeFilesOf(s)
	if !ok {
		return nil
	}
	now := time.Now().UnixMilli()
	codexCalls.Lock()
	e, hit := codexCalls.m[s.ID]
	if hit && e.fp == fp {
		e.at = now
		codexCalls.m[s.ID] = e
		codexCalls.Unlock()
		return e.calls
	}
	codexCalls.Unlock()
	tr := buildTrace(s)
	if tr == nil {
		return nil
	}
	var calls []codexCall
	for _, sp := range tr.Spans {
		if sp.Kind != "tool" || sp.Fam != "bash" {
			continue
		}
		cmd := sp.Res
		if !reCodexExec.MatchString(cmd) {
			full := tr.io.args(sp.ID)
			if !strings.Contains(full, "codex") || !reCodexExec.MatchString(full) {
				continue
			}
			cmd = full
		}
		calls = append(calls, codexCall{spanID: sp.ID, ts: sp.Ts, cmd: strings.Clone(cmd)})
	}
	codexCalls.Lock()
	codexCalls.m[s.ID] = codexCallsEnt{fp: fp, calls: calls, at: now}
	for id, e := range codexCalls.m {
		if now-e.at > childLookback.Milliseconds() {
			delete(codexCalls.m, id)
		}
	}
	codexCalls.Unlock()
	return calls
}

var reCdDir = regexp.MustCompile(`(?:^|&&|;)\s*cd\s+("[^"]+"|'[^']+'|\S+)|\s(?:-C|--cd)\s+("[^"]+"|'[^']+'|\S+)`)

// cmdDir returns the directory a command runs Codex in (cd <dir> or -C <dir>), resolved against cwd.
func cmdDir(cmd, cwd string) string {
	m := reCdDir.FindStringSubmatch(cmd)
	if m == nil {
		return ""
	}
	d := strings.Trim(firstOf(m[1], m[2]), `"'`)
	switch {
	case strings.HasPrefix(d, "~"):
		d = homeDir() + d[1:]
	case !strings.HasPrefix(d, "/"):
		d = strings.TrimRight(cwd, "/") + "/" + d
	}
	return filepath.Clean(d)
}

// matchCodexByTime accepts a Codex run in any folder only when exactly one started 0 to 10 s after ts.
func matchCodexByTime(codex []*Session, used map[string]bool, ts int64) *Session {
	var hit *Session
	for _, c := range codex {
		if d := c.StartedAt - ts; !used[c.ID] && d >= 0 && d <= 10_000 {
			if hit != nil {
				return nil
			}
			hit = c
		}
	}
	return hit
}

// matchCodexRun picks the Codex session in the same cwd that started closest after ts (within 20 s).
func matchCodexRun(codex []*Session, used map[string]bool, cwd string, ts int64) *Session {
	cwd = strings.TrimRight(cwd, "/")
	var best *Session
	for _, c := range codex {
		if used[c.ID] || strings.TrimRight(c.Cwd, "/") != cwd {
			continue
		}
		d := c.StartedAt - ts
		if d < -2000 || d > 20_000 {
			continue
		}
		if best == nil || abs64(d) < abs64(best.StartedAt-ts) {
			best = c
		}
	}
	return best
}

// ── thread helpers ──

type threadView struct {
	threads  []Thread
	threadOf map[string]string
	byID     map[string]*Session
	idx      map[string]int // thread id → index
}

func loadThreads() *threadView {
	sessions := store.All()
	kept := sessions[:0:0]
	for _, s := range sessions {
		// Codex memories sessions, and the Claude-hook twin of a Cursor CLI chat, are not threads of their own.
		if !codexInternalCwd(s.Tool, s.Cwd) && !(s.Tool == ToolClaude && s.TranscriptPath == "" && cursorChatExists(s.SessionID)) && !cursorEcho(s) {
			kept = append(kept, s)
		}
	}
	sessions = dropCursorProcessTwins(kept)
	threads, threadOf := computeThreads(sessions, panesBySession())
	byID := map[string]*Session{}
	for _, s := range sessions {
		byID[s.ID] = s
	}
	attachTraceSummaries(threads, byID)
	idx := map[string]int{}
	for i, t := range threads {
		idx[t.ID] = i
	}
	return &threadView{threads: threads, threadOf: threadOf, byID: byID, idx: idx}
}

func (v *threadView) threadFor(sessionID string) string {
	if t := v.threadOf[sessionID]; t != "" {
		return t
	}
	return sessionID
}

func (v *threadView) thread(id string) *Thread {
	if i, ok := v.idx[v.threadFor(id)]; ok {
		return &v.threads[i]
	}
	return nil
}

type ThreadV3 struct {
	Thread
	ParentThread   string `json:"parentThread,omitempty"`
	ChildKind      string `json:"childKind,omitempty"`
	ChildCount     int    `json:"childCount"`
	ChildAttention int    `json:"childAttention"`
	ChildErrors    int    `json:"childErrors"`
	Approvals      int    `json:"approvals"`
	LiveActivity   string `json:"liveActivity,omitempty"`
	DrivenID       string `json:"drivenId,omitempty"`
	// Handover on a driven row with a native session: "driven" or "terminal"; TerminalSession names the session then.
	Handover        string `json:"handover,omitempty"`
	TerminalSession string `json:"terminalSession,omitempty"`
	// Kind "driven" marks an orchestrator thread row (id = drivenId); watched rows leave it empty.
	Kind     string      `json:"kind,omitempty"`
	Status   string      `json:"status,omitempty"`
	Group    string      `json:"group,omitempty"` // a driven thread's server group: need, working, ready or settled
	Phase    string      `json:"phase,omitempty"` // needs-you, working, ready or done, from threadPhase
	Mark     *threadMark `json:"mark,omitempty"`  // pin, done and snooze of a watched thread
	SpendUSD float64     `json:"spendUsd,omitempty"`
	V4Fields
}

// parentage maps child thread → (parent thread, kind) for links whose threads differ.
func (v *threadView) parentage(links []ChildLink) map[string][2]string {
	out := map[string][2]string{}
	for _, l := range links {
		pt, ct := v.threadFor(l.Parent), v.threadFor(l.Child)
		if pt == ct || v.thread(pt) == nil || v.thread(ct) == nil {
			continue
		}
		if _, ok := out[ct]; !ok {
			out[ct] = [2]string{pt, l.Kind}
		}
	}
	return out
}

func liveActivity(s *Session) string {
	if s == nil {
		return ""
	}
	switch s.State {
	case StateAwaitingPermission:
		return firstOf(s.PermissionMessage, "waiting for permission")
	case StateAwaitingInput:
		return firstOf(clipString(firstLine(s.LastMessage), 140), "waiting for input")
	case StateRunning:
		if n := len(s.RecentEvents); n > 0 {
			return clipString(firstLine(s.RecentEvents[n-1].Text), 140)
		}
	}
	return clipString(firstLine(s.LastMessage), 140)
}

func pendingBySession() map[string]int {
	if pendingApprovalsFn == nil {
		return map[string]int{}
	}
	return pendingApprovalsFn()
}

// pendingApprovalsFn is set by approvals.go (store id → pending count).
var pendingApprovalsFn func() map[string]int

// threadsV3All returns every thread, children included, with parentage and approval counts.
func threadsV3All(v *threadView) []ThreadV3 {
	par := v.parentage(childLinks())
	pend := pendingBySession()
	need, today := needSet(), startOfDay(time.Now())
	out := make([]ThreadV3, 0, len(v.threads))
	pos := map[string]int{}
	for _, t := range v.threads {
		tv := ThreadV3{Thread: t, DrivenID: orchDrivenFor(t.Sessions)}
		for _, sid := range t.Sessions {
			tv.Approvals += pend[sid]
		}
		if tv.Approvals > 0 {
			tv.Attention = true
		}
		if n := len(t.Sessions); n > 0 {
			tv.LiveActivity = liveActivity(v.byID[t.Sessions[n-1]])
		}
		if p, ok := par[t.ID]; ok {
			tv.ParentThread, tv.ChildKind = p[0], p[1]
		}
		tv.Phase = threadPhase(&tv, v.byID, need, today)
		if m := markOf(t.ID); m != (threadMark{}) {
			tv.Mark = &m
		}
		pos[t.ID] = len(out)
		out = append(out, tv)
	}
	for i := range out {
		p := out[i].ParentThread
		if p == "" {
			continue
		}
		if j, ok := pos[p]; ok {
			out[j].ChildCount++
			if out[i].Attention {
				out[j].ChildAttention++
			}
			out[j].ChildErrors += out[i].Errors
		}
	}
	return out
}

type ChildInfo struct {
	ThreadID  string `json:"threadId"`
	SessionID string `json:"sessionId"`
	Kind      string `json:"kind"`
	SpanID    string `json:"spanId,omitempty"`
	Ts        int64  `json:"ts"`
	Title     string `json:"title"`
	State     State  `json:"state"`
	Tool      string `json:"tool"`
	Errors    int    `json:"errors"`
	Attention bool   `json:"attention"`
	Outcome   string `json:"outcome,omitempty"`
}

type ParentInfo struct {
	ThreadID  string `json:"threadId"`
	SessionID string `json:"sessionId"`
	SpanID    string `json:"spanId,omitempty"`
	Ts        int64  `json:"ts"`
	Title     string `json:"title"`
	Kind      string `json:"kind"`
}

func childrenOf(threadID string) (*ParentInfo, []ChildInfo) {
	v := loadThreads()
	th := v.thread(threadID)
	if th == nil {
		return nil, nil
	}
	members := map[string]bool{}
	for _, s := range th.Sessions {
		members[s] = true
	}
	pend := pendingBySession()
	var parent *ParentInfo
	kids := []ChildInfo{}
	seen := map[string]bool{}
	for _, l := range childLinks() {
		switch {
		case members[l.Parent] && !members[l.Child]:
			ct := v.thread(l.Child)
			c := v.byID[l.Child]
			if ct == nil || c == nil || seen[l.Child] {
				continue
			}
			seen[l.Child] = true
			kids = append(kids, ChildInfo{ThreadID: ct.ID, SessionID: l.Child, Kind: l.Kind, SpanID: l.SpanID, Ts: l.Ts,
				Title: ct.Title, State: c.State, Tool: string(c.Tool), Errors: ct.Errors,
				Attention: ct.Attention || pend[l.Child] > 0, Outcome: ct.LastOutcome})
		case members[l.Child] && !members[l.Parent] && parent == nil:
			pt := v.thread(l.Parent)
			if pt == nil {
				continue
			}
			parent = &ParentInfo{ThreadID: pt.ID, SessionID: l.Parent, SpanID: l.SpanID, Ts: l.Ts, Title: pt.Title, Kind: l.Kind}
		}
	}
	sort.Slice(kids, func(i, j int) bool { return kids[i].Ts < kids[j].Ts })
	return parent, kids
}

// ── cross-agent trace for one turn ──

type TraceRow struct {
	ID      string `json:"id"`
	Depth   int    `json:"depth"`
	Kind    string `json:"kind"`  // turn | tool | agent | child | wait
	Agent   string `json:"agent"` // claude | codex | subagent
	Name    string `json:"name"`
	Res     string `json:"res,omitempty"`
	Ts      int64  `json:"ts"`
	Dur     int64  `json:"dur"`
	Err     bool   `json:"err,omitempty"`
	Wait    bool   `json:"wait,omitempty"`
	Fam     string `json:"fam,omitempty"`
	Child   string `json:"child,omitempty"`
	Thread  string `json:"thread,omitempty"`
	Prompt  string `json:"prompt,omitempty"`
	Session string `json:"session"` // owning store id, for /api/trace/<session>/span/<id>
	By      string `json:"by,omitempty"`
}

type CrossTrace struct {
	SessionID string           `json:"sessionId"`
	ThreadID  string           `json:"threadId"`
	Turn      int              `json:"turn"`
	Turns     int              `json:"turns"`
	Prompt    string           `json:"prompt"`
	Rows      []TraceRow       `json:"rows"`
	ByAgent   map[string]int64 `json:"byAgent"`
	WallMs    int64            `json:"wallMs"`
	WorkMs    int64            `json:"workMs"`
	WaitMs    int64            `json:"waitMs"`
	Errors    int              `json:"errors"`
	Agents    int              `json:"agents"`
	From      int64            `json:"from"`
	To        int64            `json:"to"`
}

// waitIntervalsFn is set by approvals.go: decided approval intervals for a store id.
var waitIntervalsFn func(sessionID string, from, to int64) [][2]int64

func agentOf(s *Session) string {
	if s == nil {
		return "claude"
	}
	return string(s.Tool)
}

// turnBounds returns the start of every turn in a trace (user prompt time, else turn span time).
func turnBounds(tr *SessionTrace) ([]int64, []string) {
	var starts []int64
	var prompts []string
	pending := ""
	for _, sp := range tr.Spans {
		if sp.Depth != 0 {
			continue
		}
		switch sp.Kind {
		case "user":
			pending = sp.Text
		case "turn":
			starts = append(starts, sp.Ts)
			prompts = append(prompts, pending)
			pending = ""
		}
	}
	return starts, prompts
}

func buildCrossTrace(sessionID string, turn int) *CrossTrace {
	s := store.Get(sessionID)
	if s == nil {
		return nil
	}
	tr := buildTrace(s)
	if tr == nil {
		return nil
	}
	starts, prompts := turnBounds(tr)
	if len(starts) == 0 {
		return &CrossTrace{SessionID: sessionID, Rows: []TraceRow{}, ByAgent: map[string]int64{}}
	}
	if turn < 0 || turn >= len(starts) {
		turn = len(starts) - 1
	}
	from := starts[turn]
	to := tr.LastTs
	if turn+1 < len(starts) {
		to = starts[turn+1]
	}
	v := loadThreads()
	ct := &CrossTrace{SessionID: sessionID, ThreadID: v.threadFor(sessionID), Turn: turn, Turns: len(starts), Prompt: prompts[turn], ByAgent: map[string]int64{}, From: from}
	agent := agentOf(s)
	childBySpan := map[string]ChildLink{}
	for _, l := range childLinks() {
		if l.Parent == sessionID && l.SpanID != "" {
			childBySpan[l.SpanID] = l
		}
	}
	agents := map[string]bool{agent: true}
	end := from
	ct.Rows = append(ct.Rows, TraceRow{ID: "root", Depth: 0, Kind: "turn", Agent: agent, Name: "turn " + strconv.Itoa(turn+1), Ts: from, Prompt: prompts[turn], Session: sessionID})
	for _, sp := range tr.Spans {
		if sp.Depth == 0 || sp.Ts < from || sp.Ts >= to {
			continue
		}
		a := agent
		if sp.Kind == "agent" || sp.Fam == "agent" {
			a = "subagent"
		}
		if sp.Depth > 1 && strings.HasPrefix(sp.Parent, "a") {
			a = "subagent"
		}
		row := TraceRow{ID: sp.ID, Depth: sp.Depth, Kind: sp.Kind, Agent: a, Name: sp.Name, Res: sp.Res, Ts: sp.Ts, Dur: sp.Dur, Err: sp.Err, Fam: sp.Fam, Child: sp.Child, Session: sessionID, By: sp.By}
		ct.Rows = append(ct.Rows, row)
		if sp.Err {
			ct.Errors++
		}
		if sp.Depth == 1 {
			ct.ByAgent[a] += sp.Dur
			agents[a] = true
		}
		if e := sp.Ts + sp.Dur; e > end {
			end = e
		}
		if l, ok := childBySpan[sp.ID]; ok {
			if c := store.Get(l.Child); c != nil {
				if ctr := buildTrace(c); ctr != nil {
					ca := agentOf(c)
					agents[ca] = true
					ct.Rows = append(ct.Rows, TraceRow{ID: "child:" + c.ID, Depth: sp.Depth + 1, Kind: "child", Agent: ca, Name: firstOf(v.thread(c.ID).titleOr(), firstOf(c.Title, "child run")), Ts: ctr.FirstTs, Dur: ctr.LastTs - ctr.FirstTs, Child: c.ID, Thread: v.threadFor(c.ID), Session: c.ID})
					for _, csp := range ctr.Spans {
						if csp.Depth == 0 || csp.Depth > 2 {
							continue
						}
						ct.Rows = append(ct.Rows, TraceRow{ID: csp.ID, Depth: sp.Depth + 1 + csp.Depth, Kind: csp.Kind, Agent: ca, Name: csp.Name, Res: csp.Res, Ts: csp.Ts, Dur: csp.Dur, Err: csp.Err, Fam: csp.Fam, Session: c.ID})
						if csp.Err {
							ct.Errors++
						}
						if csp.Depth == 1 {
							ct.ByAgent[ca] += csp.Dur
						}
						if e := csp.Ts + csp.Dur; e > end {
							end = e
						}
					}
				}
			}
		}
	}
	if waitIntervalsFn != nil {
		for _, iv := range waitIntervalsFn(sessionID, from, to) {
			ct.Rows = append(ct.Rows, TraceRow{ID: "wait:" + strconv.FormatInt(iv[0], 10), Depth: 1, Kind: "wait", Agent: agent, Name: "waiting for you", Ts: iv[0], Dur: iv[1] - iv[0], Wait: true, Session: sessionID})
			ct.WaitMs += iv[1] - iv[0]
		}
		ct.ByAgent["wait"] = ct.WaitMs
	}
	ct.To = end
	ct.WallMs = end - from
	for k, ms := range ct.ByAgent {
		if k != "wait" {
			ct.WorkMs += ms
		}
	}
	ct.Rows[0].Dur = ct.WallMs
	ct.Agents = len(agents)
	return ct
}

func (t *Thread) titleOr() string {
	if t == nil {
		return ""
	}
	return t.Title
}

// ── timeline (Gantt) ──

type TimelineBar struct {
	From  int64  `json:"from"`
	To    int64  `json:"to"`
	Kind  string `json:"kind"` // work | wait | idle | error
	Agent string `json:"agent"`
}

type TimelineLane struct {
	ThreadID    string          `json:"threadId"`
	Title       string          `json:"title"`
	Agent       string          `json:"agent"`
	State       State           `json:"state"`
	Parent      string          `json:"parent,omitempty"`
	ChildKind   string          `json:"childKind,omitempty"`
	StartTurnTs int64           `json:"startTurnTs,omitempty"`
	Errors      int             `json:"errors"`
	Outcome     string          `json:"outcome,omitempty"`
	Bars        []TimelineBar   `json:"bars"`
	Display     string          `json:"display,omitempty"`
	Density     []DensityBucket `json:"density,omitempty"`
	Kind        string          `json:"kind,omitempty"` // "stack" for a collapsed stack lane
	Stack       *LaneStack      `json:"stack,omitempty"`
	Members     []string        `json:"members,omitempty"`
	DrivenID    string          `json:"drivenId,omitempty"`
}

const idleFoldMs = 20 * 60_000

func buildTimeline(dayStart, dayEnd int64, stacks bool) map[string]any {
	v := loadThreads()
	links := childLinks()
	par := v.parentage(links)
	startOf := map[string]int64{}
	for _, l := range links {
		ct := v.threadFor(l.Child)
		if _, ok := startOf[ct]; !ok {
			startOf[ct] = l.Ts
		}
	}
	lanes := []TimelineLane{}
	var waitTotal int64
	for _, t := range v.threads {
		if t.LastAt < dayStart || t.StartedAt > dayEnd || orchDrivenFor(t.Sessions) != "" {
			continue
		}
		lane := TimelineLane{ThreadID: t.ID, Title: t.Title, State: t.State, Errors: t.Errors, Outcome: t.LastOutcome,
			Display: displayTitle(t.Title, repoBase(t.Cwd), firstPromptOf(v, t.Sessions))}
		if p, ok := par[t.ID]; ok {
			lane.Parent, lane.ChildKind, lane.StartTurnTs = p[0], p[1], startOf[t.ID]
		}
		for _, sid := range t.Sessions {
			s := v.byID[sid]
			if s == nil || s.LastActivityAt < dayStart || s.StartedAt > dayEnd {
				continue
			}
			agent := agentOf(s)
			if lane.Agent == "" {
				lane.Agent = agent
			}
			tr := buildTrace(s)
			if tr == nil {
				continue
			}
			lane.Bars = append(lane.Bars, workBars(tr, agent, dayStart, dayEnd)...)
			lane.Density = append(lane.Density, densityOf(tr.Spans, dayStart, dayEnd)...)
			if waitIntervalsFn != nil {
				for _, iv := range waitIntervalsFn(sid, dayStart, dayEnd) {
					lane.Bars = append(lane.Bars, TimelineBar{From: iv[0], To: iv[1], Kind: "wait", Agent: agent})
					waitTotal += iv[1] - iv[0]
				}
			}
		}
		if len(lane.Bars) == 0 {
			continue
		}
		sort.Slice(lane.Bars, func(i, j int) bool { return lane.Bars[i].From < lane.Bars[j].From })
		if len(t.Sessions) > 1 {
			lane.Density = sumDensity(lane.Density)
		}
		lanes = append(lanes, lane)
	}
	now := time.Now().UnixMilli()
	dl, dw := drivenLanes(dayStart, dayEnd, now)
	lanes = append(lanes, dl...)
	waitTotal += dw
	sort.Slice(lanes, func(i, j int) bool { return lanes[i].Bars[0].From < lanes[j].Bars[0].From })
	if stacks {
		lanes = collapseStacks(lanes, enrichV4(append(withoutDriven(threadsV3All(v)), drivenRowsV3(now)...), v, now))
	}
	return map[string]any{"from": dayStart, "to": dayEnd, "lanes": orderLanes(lanes), "waitMs": waitTotal}
}

// workBars merges turn and tool spans into work intervals; gaps over idleFoldMs become idle bars.
func workBars(tr *SessionTrace, agent string, from, to int64) []TimelineBar {
	type iv struct{ a, b int64 }
	var ivs []iv
	var errs []TimelineBar
	for _, sp := range tr.Spans {
		if sp.Kind == "user" {
			continue
		}
		a, b := sp.Ts, sp.Ts+max(sp.Dur, 1000)
		if b < from || a > to {
			continue
		}
		ivs = append(ivs, iv{max(a, from), min(b, to)})
		if sp.Err {
			errs = append(errs, TimelineBar{From: sp.Ts, To: sp.Ts + max(sp.Dur, 30_000), Kind: "error", Agent: agent})
		}
	}
	sort.Slice(ivs, func(i, j int) bool { return ivs[i].a < ivs[j].a })
	var out []TimelineBar
	for _, x := range ivs {
		if n := len(out); n > 0 && x.a-out[n-1].To <= 2*60_000 {
			if x.b > out[n-1].To {
				out[n-1].To = x.b
			}
			continue
		} else if n > 0 && x.a-out[n-1].To > idleFoldMs {
			out = append(out, TimelineBar{From: out[n-1].To, To: x.a, Kind: "idle", Agent: agent})
		}
		out = append(out, TimelineBar{From: x.a, To: x.b, Kind: "work", Agent: agent})
	}
	return append(out, errs...)
}

// orderLanes puts each child lane right under its parent.
func orderLanes(lanes []TimelineLane) []TimelineLane {
	kids := map[string][]TimelineLane{}
	present := map[string]bool{}
	for _, l := range lanes {
		present[l.ThreadID] = true
	}
	var tops []TimelineLane
	for _, l := range lanes {
		if l.Parent != "" && present[l.Parent] {
			kids[l.Parent] = append(kids[l.Parent], l)
		} else {
			tops = append(tops, l)
		}
	}
	out := make([]TimelineLane, 0, len(lanes))
	var add func(l TimelineLane)
	add = func(l TimelineLane) {
		out = append(out, l)
		for _, k := range kids[l.ThreadID] {
			add(k)
		}
	}
	for _, t := range tops {
		add(t)
	}
	return out
}

// ── monitors ──

type Monitor struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Condition string `json:"condition"`
	State     string `json:"state"` // ok | warn | alert
	Detail    string `json:"detail"`
	Link      string `json:"link,omitempty"`
	Since     int64  `json:"since,omitempty"`
}

// Extra signals set by other parts; nil means not available.
var (
	pendingApprovalAgesFn func() []struct {
		SessionID string
		Since     int64
	}
	repeatErrorsFn func(since int64) []struct {
		Sig, Sample, SessionID string
		Count                  int
	}
	repeatDeniesFn func(since int64) []struct {
		SessionID, Summary string
		Count              int
	}
)

var monState = struct {
	sync.Mutex
	list    []Monitor
	cost    []costSample
	started bool
}{}

type costSample struct {
	at    int64
	total float64
}

func evalMonitors(now int64) []Monitor {
	sessions := store.All()
	v := loadThreads()
	link := func(sid string) string { return "/t/" + v.threadFor(sid) }
	var out []Monitor

	wait := Monitor{ID: "waiting", Name: "Waiting on you", Condition: "a prompt waits for you longer than 5 min", State: "ok"}
	var oldest int64
	var oldestSID string
	consider := func(sid string, since int64) {
		if now-since > 5*60_000 && (oldest == 0 || since < oldest) {
			oldest, oldestSID = since, sid
		}
	}
	if pendingApprovalAgesFn != nil {
		for _, p := range pendingApprovalAgesFn() {
			consider(p.SessionID, p.Since)
		}
	}
	for _, s := range sessions {
		if s.State == StateAwaitingPermission || s.State == StateAwaitingInput {
			consider(s.ID, s.LastActivityAt)
		}
	}
	if oldest > 0 {
		wait.State, wait.Detail, wait.Link, wait.Since = "alert", titleOf(v, oldestSID)+" · "+humanDur(now-oldest), link(oldestSID), oldest
	}
	out = append(out, wait)

	stuck := Monitor{ID: "stuck", Name: "Stuck thread", Condition: "running, no new activity for 10 min", State: "ok"}
	for _, s := range sessions {
		if s.State == StateRunning && now-s.LastActivityAt > 10*60_000 && now-s.LastActivityAt < 6*3600_000 {
			stuck.State, stuck.Detail, stuck.Link = "warn", titleOf(v, s.ID)+" · quiet "+humanDur(now-s.LastActivityAt), link(s.ID)
			break
		}
	}
	out = append(out, stuck)

	ctx := Monitor{ID: "context", Name: "Context pressure", Condition: "a live session above 90% of its context window", State: "ok"}
	for _, s := range sessions {
		if s.State != StateRunning && s.State != StateIdle && s.State != StateAwaitingInput && s.State != StateAwaitingPermission {
			continue
		}
		if sum := cachedSummary(s); sum != nil && sum.ContextWindow > 0 && float64(sum.ContextUsed) > 0.9*float64(sum.ContextWindow) {
			ctx.State, ctx.Detail, ctx.Link = "warn", titleOf(v, s.ID)+" · "+strconv.Itoa(int(100*sum.ContextUsed/sum.ContextWindow))+"%", link(s.ID)
			break
		}
	}
	out = append(out, ctx)

	spend := Monitor{ID: "spend", Name: "Spend rate", Condition: "more than $25 per hour across agents", State: "ok"}
	var total float64
	for _, s := range sessions {
		if now-s.LastActivityAt > 26*3600_000 {
			continue
		}
		if sum := cachedSummary(s); sum != nil {
			total += sum.CostUSD
		}
	}
	monState.cost = append(monState.cost, costSample{now, total})
	for len(monState.cost) > 1 && now-monState.cost[0].at > 3600_000 {
		monState.cost = monState.cost[1:]
	}
	if first := monState.cost[0]; now-first.at > 10*60_000 {
		rate := (total - first.total) * 3600_000 / float64(now-first.at)
		if rate < 0 {
			rate = 0
		}
		spend.Detail = "$" + strconv.FormatFloat(rate, 'f', 2, 64) + " per hour"
		switch {
		case rate > 25:
			spend.State = "alert"
		case rate > 15:
			spend.State = "warn"
		}
	} else {
		spend.Detail = "measuring"
	}
	out = append(out, spend)

	rep := Monitor{ID: "repeat-error", Name: "Same failure again", Condition: "one error signature 3 times in 1 hour", State: "ok"}
	if repeatErrorsFn != nil {
		if errs := repeatErrorsFn(now - 3600_000); len(errs) > 0 && errs[0].Count >= 3 {
			rep.State, rep.Detail, rep.Link = "alert", clipString(errs[0].Sample, 90)+" · "+strconv.Itoa(errs[0].Count)+"×", link(errs[0].SessionID)
		}
	} else {
		rep.Detail = "needs the search index"
	}
	out = append(out, rep)

	den := Monitor{ID: "repeat-deny", Name: "Repeat denies", Condition: "the same request denied twice in 1 hour", State: "ok"}
	if repeatDeniesFn != nil {
		if ds := repeatDeniesFn(now - 3600_000); len(ds) > 0 && ds[0].Count >= 2 {
			den.State, den.Detail, den.Link = "warn", titleOf(v, ds[0].SessionID)+" · "+clipString(ds[0].Summary, 60), link(ds[0].SessionID)
		}
	} else {
		den.Detail = "needs approvals"
	}
	out = append(out, den)
	return out
}

func titleOf(v *threadView, sid string) string {
	if t := v.thread(sid); t != nil {
		return clipString(t.Title, 60)
	}
	return sid
}

func humanDur(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d >= time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h " + strconv.Itoa(int(d.Minutes())%60) + "m"
	case d >= time.Minute:
		return strconv.Itoa(int(d.Minutes())) + "m"
	default:
		return strconv.Itoa(int(d.Seconds())) + "s"
	}
}

func startMonitorLoop() {
	monState.Lock()
	if monState.started {
		monState.Unlock()
		return
	}
	monState.started = true
	monState.Unlock()
	go func() {
		for {
			tick()
			time.Sleep(30 * time.Second)
		}
	}()
}

func tick() {
	monState.Lock()
	prev := map[string]string{}
	for _, m := range monState.list {
		prev[m.ID] = m.State
	}
	list := evalMonitors(time.Now().UnixMilli())
	monState.list = list
	monState.Unlock()
	for _, m := range list {
		if p, ok := prev[m.ID]; ok && p != m.State {
			publishV3("monitor", "monitor", m)
		}
	}
}

func currentMonitors() []Monitor {
	monState.Lock()
	defer monState.Unlock()
	if monState.list == nil {
		monState.list = evalMonitors(time.Now().UnixMilli())
	}
	return append([]Monitor(nil), monState.list...)
}

// ── intelligence ──

func buildIntel(from, to int64) map[string]any {
	v := loadThreads()
	workByAgent := map[string]int64{}
	timeByFam := map[string]int64{}
	spendByRepo := map[string]float64{}
	estimated := false
	type shipped struct {
		Kind   string `json:"kind"`
		Label  string `json:"label"`
		Ref    string `json:"ref"`
		Thread string `json:"thread"`
		Ts     int64  `json:"ts"`
	}
	ships := []shipped{}
	threadsActive := 0
	var waitMs int64
	for _, t := range v.threads {
		if t.LastAt < from || t.StartedAt > to {
			continue
		}
		threadsActive++
		for _, sid := range t.Sessions {
			s := v.byID[sid]
			if s == nil || s.LastActivityAt < from || s.StartedAt > to {
				continue
			}
			tr := buildTrace(s)
			if tr == nil {
				continue
			}
			agent := agentOf(s)
			for _, sp := range tr.Spans {
				if sp.Ts < from || sp.Ts > to {
					continue
				}
				switch {
				case sp.Kind == "turn":
					workByAgent[agent] += sp.Dur
				case sp.Depth == 1 && sp.Kind == "tool":
					timeByFam[sp.Fam] += sp.Dur
				case sp.Depth == 1 && sp.Kind == "agent":
					timeByFam["subagent"] += sp.Dur
				}
			}
			for _, seg := range tr.Segments {
				span := seg.ToTs - seg.FromTs
				if span <= 0 || seg.ToTs < from || seg.FromTs > to {
					continue
				}
				overlap := min(seg.ToTs, to) - max(seg.FromTs, from)
				spendByRepo[repoName(s.Cwd)] += seg.UsdEst * float64(overlap) / float64(span)
			}
			if tr.CostEstimated {
				estimated = true
			}
			for _, o := range tr.Outputs {
				if o.Ts >= from && o.Ts <= to {
					ships = append(ships, shipped{o.Kind, o.Label, o.Ref, t.ID, o.Ts})
				}
			}
			if waitIntervalsFn != nil {
				for _, iv := range waitIntervalsFn(sid, from, to) {
					waitMs += iv[1] - iv[0]
				}
			}
		}
	}
	sort.Slice(ships, func(i, j int) bool { return ships[i].Ts > ships[j].Ts })
	if len(ships) > 40 {
		ships = ships[:40]
	}
	type repeat struct {
		Sig     string `json:"sig"`
		Sample  string `json:"sample"`
		Session string `json:"session"`
		Thread  string `json:"thread"`
		Count   int    `json:"count"`
	}
	errs := []repeat{}
	if repeatErrorsFn != nil {
		for _, e := range repeatErrorsFn(from) {
			errs = append(errs, repeat{e.Sig, e.Sample, e.SessionID, v.threadFor(e.SessionID), e.Count})
		}
	}
	return map[string]any{
		"from": from, "to": to, "threads": threadsActive,
		"workByAgent": workByAgent, "timeByFamily": timeByFam,
		"spendByRepo": spendByRepo, "spendEstimated": estimated,
		"waitMs": waitMs, "waitKnown": waitIntervalsFn != nil,
		"shipped": ships, "repeatFailures": errs,
	}
}

func repoName(cwd string) string {
	cwd = strings.TrimRight(cwd, "/")
	if cwd == "" {
		return "(none)"
	}
	if cwd == homeDir() {
		return "~"
	}
	if i := strings.LastIndex(cwd, "/"); i >= 0 {
		return cwd[i+1:]
	}
	return cwd
}

// ── health ──

func hooksInstalled() map[string]bool {
	read := func(p string) string {
		b, _ := os.ReadFile(p)
		return string(b)
	}
	cl := read(homeDir() + "/.claude/settings.json")
	cx := read(homeDir()+"/.codex/hooks.json") + read(homeDir()+"/.codex/config.toml")
	return map[string]bool{
		"claudeHttp":    strings.Contains(cl, "/api/approvals/hook/claude-permission"),
		"claudePreTool": strings.Contains(cl, "/api/approvals/hook/claude-pretool"),
		"codex":         strings.Contains(cx, "PermissionRequest") && strings.Contains(cx, "codex-hook.sh"),
	}
}

// searchStatusFn is set by search.go.
var searchStatusFn func() any

// ── routes ──

func registerOpsRoutes(mux *http.ServeMux) {
	startMonitorLoop()

	// ?all=1 children, ?driven=1 owned rows, ?compact=1 list fields only, ?since=<ms> lastAt floor, ?limit=<n> newest n.
	mux.HandleFunc("/api/v3/threads", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		writeJSON(w, shapeThreads(threadsResponse(q.Get("all") == "1", q.Get("driven") == "1"), q))
	})
	mux.HandleFunc("/api/v3/mark", handleMark)
	mux.HandleFunc("/api/v3/now", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, buildNow(time.Now().UnixMilli()))
	})
	mux.HandleFunc("/api/v3/thread/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/v3/thread/")
		id, sub, _ := strings.Cut(rest, "/")
		if sub != "children" {
			http.NotFound(w, r)
			return
		}
		parent, kids := childrenOf(id)
		if kids == nil && parent == nil && loadThreads().thread(id) == nil {
			writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "thread not found"})
			return
		}
		writeJSON(w, map[string]any{"parent": parent, "children": kids})
	})
	mux.HandleFunc("/api/v3/trace/", func(w http.ResponseWriter, r *http.Request) {
		sid := strings.TrimPrefix(r.URL.Path, "/api/v3/trace/")
		turn := -1
		if v := r.URL.Query().Get("turn"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				turn = n
			}
		}
		ct := buildCrossTrace(sid, turn)
		if ct == nil {
			writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "session not found"})
			return
		}
		writeJSON(w, ct)
	})
	mux.HandleFunc("/api/v3/timeline", func(w http.ResponseWriter, r *http.Request) {
		day := r.URL.Query().Get("day")
		t, err := time.ParseInLocation("2006-01-02", day, time.Local)
		if err != nil {
			t = time.Now()
			t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.Local)
		}
		span := 24 * time.Hour
		if r.URL.Query().Get("range") == "week" {
			t = t.AddDate(0, 0, -6)
			span = 7 * 24 * time.Hour
		}
		writeJSON(w, buildTimeline(t.UnixMilli(), t.Add(span).UnixMilli(), r.URL.Query().Get("stacks") == "1"))
	})
	mux.HandleFunc("/api/v3/monitors", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"monitors": currentMonitors()})
	})
	mux.HandleFunc("/api/v3/intel", func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		from := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
		if r.URL.Query().Get("range") == "7d" {
			from = from.AddDate(0, 0, -6)
		}
		writeJSON(w, buildIntel(from.UnixMilli(), now.UnixMilli()))
	})
	mux.HandleFunc("/api/v3/health", func(w http.ResponseWriter, r *http.Request) {
		pending := 0
		for _, n := range pendingBySession() {
			pending += n
		}
		var search any
		if searchStatusFn != nil {
			search = searchStatusFn()
		}
		writeJSON(w, map[string]any{"hooks": hooksInstalled(), "search": search, "approvals": map[string]any{"pending": pending}})
	})
}

// dropCursorProcessTwins hides a process-only cursor-agent session when a Cursor chat in its folder was active since it started.
func dropCursorProcessTwins(ss []*Session) []*Session {
	chatAt := map[string]int64{}
	for _, s := range ss {
		if s.Tool == ToolCursor && s.Cwd != "" && s.LastActivityAt > chatAt[s.Cwd] {
			chatAt[s.Cwd] = s.LastActivityAt
		}
	}
	out := ss[:0:0]
	for _, s := range ss {
		if s.Tool == ToolCursorAgent && strings.HasPrefix(s.SessionID, "pid-") && chatAt[s.Cwd] >= s.StartedAt && chatAt[s.Cwd] > 0 {
			continue
		}
		out = append(out, s)
	}
	return out
}
