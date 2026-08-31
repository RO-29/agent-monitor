package main

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// ── Trace model ───────────────────────────────────────────────────────────
//
// Vocabulary (see DESIGN.md §3):
//   Session  = one transcript file.
//   Segment  = the part of a session between two boundaries
//              (start · compact · clear · resume).
//   Chapter  = the intelligence card for one segment.
//   Span     = one row on the waterfall: a user prompt, a turn, a tool call,
//              or a subagent run.
//   Learning = one ledger item with a source: memory | correction | summary | output.

type Boundary struct {
	Kind              string            `json:"kind"` // start | compact | clear | resume
	At                int64             `json:"at"`
	Trigger           string            `json:"trigger,omitempty"` // auto | manual
	PreTokens         int64             `json:"preTokens,omitempty"`
	PostTokens        int64             `json:"postTokens,omitempty"`
	DroppedTokens     int64             `json:"droppedTokens,omitempty"`
	CumulativeDropped int64             `json:"cumulativeDropped,omitempty"`
	DurationMs        int64             `json:"durationMs,omitempty"`
	Summary           string            `json:"summary,omitempty"`  // compaction summary text (clipped)
	Sections          map[string]string `json:"sections,omitempty"` // parsed summary sections
}

type Segment struct {
	ID       string     `json:"id"` // "<storeId>#<index>"
	Index    int        `json:"index"`
	Boundary Boundary   `json:"boundary"`
	FromTs   int64      `json:"fromTs"`
	ToTs     int64      `json:"toTs"`
	Turns    int        `json:"turns"`
	Spans    int        `json:"spans"`
	Errors   int        `json:"errors"`
	Tokens   TokenUsage `json:"tokens"`
	UsdEst   float64    `json:"usdEst"`
	Chapter  *Chapter   `json:"chapter,omitempty"`
}

type Span struct {
	ID     string      `json:"id"`
	Kind   string      `json:"kind"` // user | turn | tool | agent
	Name   string      `json:"name"`
	Res    string      `json:"res,omitempty"`
	Ts     int64       `json:"ts"`
	Dur    int64       `json:"dur"` // ms
	Parent string      `json:"parent,omitempty"`
	Depth  int         `json:"depth"`
	Seg    int         `json:"seg"`
	Err    bool        `json:"err,omitempty"`
	Fam    string      `json:"fam"` // bash | read | edit | agent | mcp | web | other | model | user
	Tokens *TokenUsage `json:"tokens,omitempty"`
	Model  string      `json:"model,omitempty"`
	Flag   string      `json:"flag,omitempty"` // "correction" on a user span, "aborted" on a turn
	Text   string      `json:"text,omitempty"` // prompt / prose head, clipped
	Child  string      `json:"child,omitempty"` // linked child session id (subagent run)
}

// SpanIO is the heavy part of a span, served on demand by /api/span/:id.
type SpanIO struct {
	Args   string `json:"args"`
	Result string `json:"result"`
}

type Learning struct {
	ID        string `json:"id"`
	Source    string `json:"source"` // memory | correction | summary | output
	Text      string `json:"text"`
	Evidence  string `json:"evidence"`
	Ts        int64  `json:"ts"`
	Seg       int    `json:"seg"`
	Heuristic bool   `json:"heuristic,omitempty"`
	Ref       string `json:"ref,omitempty"` // file path, url, span id
}

type Output struct {
	Kind  string `json:"kind"` // pr | artifact | doc | commit
	Label string `json:"label"`
	Ref   string `json:"ref"`
	Ts    int64  `json:"ts"`
	Seg   int    `json:"seg"`
}

type IntentChange struct {
	Ts   int64  `json:"ts"`
	Text string `json:"text"`
}

type Chapter struct {
	Point         string         `json:"point"`
	IntentChanges []IntentChange `json:"intentChanges"`
	Outcome       string         `json:"outcome"`
	Learnings     []Learning     `json:"learnings"`
	Open          []string       `json:"open"`
	Outputs       []Output       `json:"outputs"`
	Source        string         `json:"source"` // deterministic | enriched
	EnrichedAt    int64          `json:"enrichedAt,omitempty"`
	Model         string         `json:"model,omitempty"`
}

type SessionTrace struct {
	SessionID     string     `json:"sessionId"` // store id, e.g. claude:<uuid>
	Tool          Tool       `json:"tool"`
	Segments      []Segment  `json:"segments"`
	Spans         []Span     `json:"spans"`
	Learnings     []Learning `json:"learnings"`
	Outputs       []Output   `json:"outputs"`
	CostUSD       float64    `json:"costUsd"`
	CostEstimated bool       `json:"costEstimated"`
	ContextUsed   int64      `json:"contextUsed"`
	ContextWindow int64      `json:"contextWindow"`
	Model         string     `json:"model,omitempty"`
	GeneratedAt   int64      `json:"generatedAt"`
	FirstTs       int64      `json:"firstTs"`
	LastTs        int64      `json:"lastTs"`

	io map[string]SpanIO // span id → args/result (not serialised)
}

// TraceSummary is the cheap per-session roll-up used by list views.
type TraceSummary struct {
	Segments      int     `json:"segments"`
	Compacts      int     `json:"compacts"`
	Clears        int     `json:"clears"`
	Spans         int     `json:"spans"`
	Errors        int     `json:"errors"`
	CostUSD       float64 `json:"costUsd"`
	CostEstimated bool    `json:"costEstimated"`
	ContextUsed   int64   `json:"contextUsed"`
	ContextWindow int64   `json:"contextWindow"`
	LastPoint     string  `json:"lastPoint,omitempty"`
	LastOutcome   string  `json:"lastOutcome,omitempty"`
	Learnings     int     `json:"learnings"`
	Outputs       int     `json:"outputs"`
	SegWeights    []int64 `json:"segWeights"` // duration ms per segment, for the strip
	SegKinds      []string `json:"segKinds"`
}

// ── cache ─────────────────────────────────────────────────────────────────

type traceCacheEntry struct {
	size  int64
	mtime int64
	trace *SessionTrace
}

var traceCache = struct {
	mu sync.Mutex
	m  map[string]*traceCacheEntry
}{m: map[string]*traceCacheEntry{}}

// buildTrace returns the trace for a session, reusing the cached copy while
// the transcript file has not changed. Returns nil when the tool has no parser.
func buildTrace(sess *Session) *SessionTrace {
	if sess == nil || sess.TranscriptPath == "" {
		return nil
	}
	st, err := os.Stat(sess.TranscriptPath)
	if err != nil {
		return nil
	}
	traceCache.mu.Lock()
	if e, ok := traceCache.m[sess.ID]; ok && e.size == st.Size() && e.mtime == st.ModTime().UnixMilli() {
		traceCache.mu.Unlock()
		return e.trace
	}
	traceCache.mu.Unlock()

	var tr *SessionTrace
	switch sess.Tool {
	case ToolClaude:
		tr = buildClaudeTrace(sess)
	case ToolCodex:
		tr = buildCodexTrace(sess)
	default:
		return nil
	}
	if tr == nil {
		return nil
	}
	tr.GeneratedAt = time.Now().UnixMilli()
	traceCache.mu.Lock()
	traceCache.m[sess.ID] = &traceCacheEntry{size: st.Size(), mtime: st.ModTime().UnixMilli(), trace: tr}
	traceCache.mu.Unlock()
	return tr
}

func summarizeTrace(tr *SessionTrace) *TraceSummary {
	if tr == nil {
		return nil
	}
	s := &TraceSummary{Segments: len(tr.Segments), Spans: len(tr.Spans), CostUSD: tr.CostUSD, CostEstimated: tr.CostEstimated,
		ContextUsed: tr.ContextUsed, ContextWindow: tr.ContextWindow, Learnings: len(tr.Learnings), Outputs: len(tr.Outputs)}
	for _, seg := range tr.Segments {
		switch seg.Boundary.Kind {
		case "compact":
			s.Compacts++
		case "clear":
			s.Clears++
		}
		s.Errors += seg.Errors
		w := seg.ToTs - seg.FromTs
		if w < 60_000 {
			w = 60_000
		}
		s.SegWeights = append(s.SegWeights, w)
		s.SegKinds = append(s.SegKinds, seg.Boundary.Kind)
	}
	if n := len(tr.Segments); n > 0 && tr.Segments[n-1].Chapter != nil {
		s.LastPoint = tr.Segments[n-1].Chapter.Point
		s.LastOutcome = tr.Segments[n-1].Chapter.Outcome
	}
	return s
}

// ── shared helpers ────────────────────────────────────────────────────────

func spanID(prefix, key string) string {
	h := sha1.Sum([]byte(prefix + "|" + key))
	return prefix + "-" + hex.EncodeToString(h[:6])
}

func toolFamily(name string) string {
	switch {
	case name == "Bash", name == "shell", name == "exec_command", name == "local_shell":
		return "bash"
	case name == "Read", name == "Grep", name == "Glob", name == "LS", name == "NotebookRead", name == "read_file", name == "list_dir", name == "grep_search":
		return "read"
	case name == "Edit", name == "Write", name == "MultiEdit", name == "NotebookEdit", name == "apply_patch", name == "write_file":
		return "edit"
	case name == "Agent", name == "Task", name == "Workflow", name == "spawn_agent":
		return "agent"
	case strings.HasPrefix(name, "mcp__"):
		return "mcp"
	case name == "WebFetch", name == "WebSearch", name == "web_search":
		return "web"
	}
	return "other"
}

func shortHome(p string) string {
	h := homeDir()
	if h != "" && strings.HasPrefix(p, h) {
		return "~" + p[len(h):]
	}
	return p
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// claudeToolResource derives the short "resource" label for a tool span.
func claudeToolResource(name string, input map[string]any) string {
	str := func(k string) string { v, _ := input[k].(string); return v }
	switch name {
	case "Bash":
		if d := str("description"); d != "" && len(d) < 80 {
			return clipString(firstLine(str("command")), 140)
		}
		return clipString(firstLine(str("command")), 140)
	case "Read", "Edit", "Write", "MultiEdit", "NotebookEdit", "NotebookRead":
		return shortHome(str("file_path"))
	case "Grep":
		return clipString(str("pattern"), 100)
	case "Glob":
		return clipString(str("pattern"), 100)
	case "Agent", "Task":
		if d := str("description"); d != "" {
			return clipString(d, 120)
		}
		return clipString(firstLine(str("prompt")), 120)
	case "Skill":
		return str("skill")
	case "WebFetch":
		return clipString(str("url"), 100)
	case "WebSearch":
		return clipString(str("query"), 100)
	}
	if strings.HasPrefix(name, "mcp__") {
		parts := strings.SplitN(name, "__", 3)
		if len(parts) == 3 {
			return parts[1] + " · " + parts[2]
		}
	}
	// generic: first short string value
	keys := make([]string, 0, len(input))
	for k := range input {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v, ok := input[k].(string); ok && v != "" {
			return clipString(firstLine(v), 100)
		}
	}
	return ""
}

var reCorrection = regexp.MustCompile(`(?i)^\s*(no\b|nope\b|wrong\b|don'?t\b|do not\b|stop\b|not that\b|never\b|instead\b|that'?s not\b|actually,|undo\b|revert\b|why did you\b|this is not\b|not working\b|still (broken|failing|wrong|not)\b)`)
var reCorrectionBody = regexp.MustCompile(`(?i)\b(that'?s wrong|you broke|is broken|doesn'?t work|does not work|not what i asked|i said|i told you|rethink|go back to)\b`)

// isCorrection flags a user prompt that reverses or corrects the previous
// assistant turn. Heuristic: a leading negation, or an explicit correction
// phrase in a short prompt sent soon after an Edit/Write.
func isCorrection(text string, ts, lastEditTs int64) bool {
	if reCorrection.MatchString(text) {
		return true
	}
	if reCorrectionBody.MatchString(text) && len(text) < 400 {
		return true
	}
	_ = ts
	_ = lastEditTs
	return false
}

var reHandoffDoc = regexp.MustCompile(`(?i)(HANDOFF|PLAN|RESUME|BRIEF|PROMPT|RCA|AUDIT|SUMMARY)[^/]*\.md$`)
var reGitCommit = regexp.MustCompile(`git\s+commit\b`)

// memoryLearning reads the memory file the session wrote and turns it into a
// ledger item (type + description from the frontmatter, else the first line).
func memoryLearning(path string, ts int64, seg int, verb string) Learning {
	text := filepath.Base(path)
	kind := ""
	if raw, err := os.ReadFile(path); err == nil {
		s := string(raw)
		if m := regexp.MustCompile(`(?m)^description:\s*(.+)$`).FindStringSubmatch(s); m != nil {
			text = strings.TrimSpace(m[1])
		}
		if m := regexp.MustCompile(`(?m)^\s*type:\s*(\w+)`).FindStringSubmatch(s); m != nil {
			kind = m[1]
		}
	}
	if kind != "" {
		text = kind + ": " + text
	}
	return Learning{ID: spanID("lrn", path+fmt.Sprint(ts)), Source: "memory", Text: clipString(text, 240),
		Evidence: verb + " " + shortHome(path), Ts: ts, Seg: seg, Ref: path}
}

// ── compact summary parsing ───────────────────────────────────────────────

// Headings appear as "1. **Title:**", "**Title**" or "## 1. Title" depending on
// the Claude Code version that wrote the summary.
var reSummarySection = regexp.MustCompile(`(?m)^\s*(?:\d+\.\s*)?\*\*([^*\n]{3,80}?)\*\*:?\s*$|^\s*\d+\.\s*\*\*([^*\n]{3,80}?):?\*\*:?|^\s*#{1,4}\s*(?:\d+\.\s*)?([^\n#*]{3,80}?):?\s*$`)

// parseSummarySections splits Claude's compaction summary into its numbered
// bold sections ("Primary Request and Intent", "Errors and fixes", "Pending
// Tasks", …). Keys are lower-cased titles.
func parseSummarySections(s string) map[string]string {
	out := map[string]string{}
	idx := reSummarySection.FindAllStringSubmatchIndex(s, -1)
	if len(idx) == 0 {
		return out
	}
	for i, m := range idx {
		title := ""
		switch {
		case m[2] >= 0:
			title = s[m[2]:m[3]]
		case m[4] >= 0:
			title = s[m[4]:m[5]]
		case len(m) > 7 && m[6] >= 0:
			title = s[m[6]:m[7]]
		}
		title = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(title, ":")))
		end := len(s)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		body := strings.TrimSpace(s[m[1]:end])
		if title != "" && body != "" {
			out[title] = body
		}
	}
	return out
}

func sectionLike(sections map[string]string, needles ...string) string {
	for k, v := range sections {
		for _, n := range needles {
			if strings.Contains(k, n) {
				return v
			}
		}
	}
	return ""
}

var reListItem = regexp.MustCompile(`^\s*(?:[-*•·]|\d+[.)])\s+`)

// bullets returns the list items of a markdown block (max n, clipped). Prose
// paragraphs are not items; a block with no list markers yields nothing.
func bullets(block string, n, clip int) []string {
	var out []string
	for _, ln := range strings.Split(block, "\n") {
		if !reListItem.MatchString(ln) {
			continue
		}
		t := strings.TrimSpace(ln)
		t = strings.TrimLeft(t, "-*•· ")
		t = regexp.MustCompile(`^\d+[.)]\s*`).ReplaceAllString(t, "")
		t = strings.ReplaceAll(t, "**", "")
		t = strings.Trim(strings.TrimSpace(t), "*`")
		if len(t) < 4 {
			continue
		}
		out = append(out, clipString(t, clip))
		if len(out) >= n {
			break
		}
	}
	return out
}

func firstParagraph(block string, clip int) string {
	block = strings.TrimSpace(block)
	if block == "" {
		return ""
	}
	parts := strings.SplitN(block, "\n\n", 2)
	p := strings.TrimSpace(parts[0])
	p = strings.ReplaceAll(p, "**", "")
	p = strings.Join(strings.Fields(p), " ")
	return clipString(p, clip)
}

// ── chapters (deterministic) ──────────────────────────────────────────────

// finalizeTrace assigns spans to segments, computes segment counters, and
// builds the deterministic chapter for each segment.
func finalizeTrace(tr *SessionTrace, awaySummaries []IntentChange) {
	// Safety net for the UI's tree walk: ids must be unique and parent chains
	// acyclic. Duplicates are replays (drop them); a cyclic parent becomes a root.
	seenID := map[string]bool{}
	uniq := make([]Span, 0, len(tr.Spans))
	for _, sp := range tr.Spans {
		if seenID[sp.ID] {
			continue
		}
		seenID[sp.ID] = true
		uniq = append(uniq, sp)
	}
	tr.Spans = uniq
	byID := map[string]int{}
	for i, sp := range tr.Spans {
		byID[sp.ID] = i
	}
	for i := range tr.Spans {
		visited := map[string]bool{tr.Spans[i].ID: true}
		cur := tr.Spans[i].Parent
		for cur != "" {
			if visited[cur] {
				tr.Spans[i].Parent = ""
				break
			}
			visited[cur] = true
			j, ok := byID[cur]
			if !ok {
				tr.Spans[i].Parent = ""
				break
			}
			cur = tr.Spans[j].Parent
		}
	}
	sort.SliceStable(tr.Spans, func(i, j int) bool { return tr.Spans[i].Ts < tr.Spans[j].Ts })
	if len(tr.Segments) == 0 {
		tr.Segments = append(tr.Segments, Segment{Index: 0, Boundary: Boundary{Kind: "start", At: tr.FirstTs}})
	}
	segAt := func(ts int64) int {
		idx := 0
		for i, s := range tr.Segments {
			if s.Boundary.At <= ts {
				idx = i
			}
		}
		return idx
	}
	for i := range tr.Segments {
		tr.Segments[i].Index = i
		tr.Segments[i].ID = fmt.Sprintf("%s#%d", tr.SessionID, i)
		tr.Segments[i].FromTs = tr.Segments[i].Boundary.At
		if i+1 < len(tr.Segments) {
			tr.Segments[i].ToTs = tr.Segments[i+1].Boundary.At
		} else {
			tr.Segments[i].ToTs = tr.LastTs
		}
		tr.Segments[i].Turns, tr.Segments[i].Spans, tr.Segments[i].Errors = 0, 0, 0
		tr.Segments[i].Tokens = TokenUsage{}
	}
	for i := range tr.Spans {
		sp := &tr.Spans[i]
		sp.Seg = segAt(sp.Ts)
		seg := &tr.Segments[sp.Seg]
		seg.Spans++
		if sp.Kind == "turn" {
			seg.Turns++
			if sp.Tokens != nil {
				seg.Tokens.Input += sp.Tokens.Input
				seg.Tokens.Output += sp.Tokens.Output
				seg.Tokens.CacheRead += sp.Tokens.CacheRead
				seg.Tokens.CacheCreate += sp.Tokens.CacheCreate
			}
		}
		if sp.Err {
			seg.Errors++
		}
	}
	for i := range tr.Learnings {
		tr.Learnings[i].Seg = segAt(tr.Learnings[i].Ts)
	}
	// Outputs repeat in the transcript (pr-link is re-written every turn);
	// keep the first sighting of each (kind, ref).
	seenOut := map[string]bool{}
	dedup := make([]Output, 0, len(tr.Outputs))
	for _, o := range tr.Outputs {
		k := o.Kind + "|" + o.Ref
		if seenOut[k] {
			continue
		}
		seenOut[k] = true
		o.Seg = segAt(o.Ts)
		dedup = append(dedup, o)
	}
	tr.Outputs = dedup
	if tr.Learnings == nil {
		tr.Learnings = []Learning{}
	}
	for i := range tr.Segments {
		seg := &tr.Segments[i]
		seg.UsdEst = estimateCost(tr.Model, seg.Tokens)
		ch := &Chapter{Source: "deterministic", IntentChanges: []IntentChange{}, Learnings: []Learning{}, Open: []string{}, Outputs: []Output{}}
		// The point: the carried summary's intent, else the first prompt.
		if b := seg.Boundary; b.Kind == "compact" && len(b.Sections) > 0 {
			ch.Point = firstParagraph(sectionLike(b.Sections, "primary request", "intent"), 480)
		}
		var lastProse string
		for _, sp := range tr.Spans {
			if sp.Seg != i {
				continue
			}
			if sp.Kind == "user" && sp.Text != "" {
				// The point is the first substantive prompt; a bare "hi" or "ok"
				// yields to the next prompt when one exists.
				if ch.Point == "" || (len(ch.Point) < 12 && len(sp.Text) >= 12 && len(ch.IntentChanges) <= 1) {
					ch.Point = clipString(sp.Text, 480)
				}
				ch.IntentChanges = append(ch.IntentChanges, IntentChange{Ts: sp.Ts, Text: clipString(sp.Text, 160)})
			}
			if sp.Kind == "turn" && sp.Text != "" {
				lastProse = sp.Text
			}
		}
		if len(ch.IntentChanges) > 12 {
			ch.IntentChanges = append(ch.IntentChanges[:6], ch.IntentChanges[len(ch.IntentChanges)-6:]...)
		}
		// Outcome: an away summary inside the segment beats the last prose.
		for _, a := range awaySummaries {
			if a.Ts >= seg.FromTs && (a.Ts <= seg.ToTs || i == len(tr.Segments)-1) {
				ch.Outcome = clipString(a.Text, 480)
			}
		}
		if ch.Outcome == "" {
			ch.Outcome = clipString(lastProse, 480)
		}
		for _, l := range tr.Learnings {
			if l.Seg == i {
				ch.Learnings = append(ch.Learnings, l)
			}
		}
		for _, o := range tr.Outputs {
			if o.Seg == i {
				ch.Outputs = append(ch.Outputs, o)
			}
		}
		// Open threads: pending tasks named by the summary that CLOSES the
		// segment (next boundary), else by the one that opens it.
		var pend string
		if i+1 < len(tr.Segments) {
			pend = sectionLike(tr.Segments[i+1].Boundary.Sections, "pending", "next step")
		}
		if pend == "" {
			pend = sectionLike(seg.Boundary.Sections, "pending", "next step")
		}
		ch.Open = bullets(pend, 5, 200)
		if ch.Open == nil {
			ch.Open = []string{}
		}
		seg.Chapter = ch
	}
}

// estimateCost applies a coarse public price table; the UI labels it as an
// estimate. Exact session cost comes from cost-state when present.
func estimateCost(model string, t TokenUsage) float64 {
	m := strings.ToLower(model)
	var in, out, cr, cc float64
	switch {
	case strings.Contains(m, "haiku"):
		in, out, cr, cc = 1, 5, 0.1, 1.25
	case strings.Contains(m, "sonnet"):
		in, out, cr, cc = 3, 15, 0.3, 3.75
	case strings.Contains(m, "opus"), strings.Contains(m, "fable"), strings.Contains(m, "mythos"):
		in, out, cr, cc = 15, 75, 1.5, 18.75
	case strings.Contains(m, "gpt"), strings.Contains(m, "codex"):
		in, out, cr, cc = 1.25, 10, 0.125, 0
	default:
		in, out, cr, cc = 3, 15, 0.3, 3.75
	}
	return (float64(t.Input)*in + float64(t.Output)*out + float64(t.CacheRead)*cr + float64(t.CacheCreate)*cc) / 1e6
}

func contextWindowFor(model string) int64 {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "fable"), strings.Contains(m, "mythos"), strings.Contains(m, "opus-5"), strings.Contains(m, "sonnet-5"), strings.Contains(m, "1m"):
		return 1_000_000
	case strings.Contains(m, "gpt"), strings.Contains(m, "codex"):
		return 258_400
	}
	return 200_000
}
