package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Watched items: index turn offsets once, parse only the turns a client asks for, resume as the file grows.

const (
	watchedPageTurns = 40
	watchedTailMax   = 160 // trim the live tail back to watchedPageTurns*2 past this
	watchedCacheMax  = 16
	watchedIdleTTL   = 5 * time.Minute
	codexDupWindowMs = 30_000
	watchedHeadLen   = 4096
)

// watchedLineMax skips a transcript line longer than this; the offset still moves past it.
var watchedLineMax = 8 << 20

// skippedLine is what forEachLine passes for a line over watchedLineMax.
var skippedLine = []byte{}

type wItem struct {
	it  *OItem
	mod int64 // offset of the line that last changed it
	bg  *wBg  // a ! command Claude Code moved to the background
}

type wTurn struct {
	off    int64
	run    *ORun
	runMod int64
	items  []*wItem
	tok    TokenUsage
	msgTok map[string]TokenUsage // claude: message id -> usage already counted
	model  string
	lastTs int64
	user   *wItem
	cmd    bool // opened by a prompt slash command whose expanded prompt may follow
	bang   bool // opened by a ! command; the agent may answer it
}

type wIndex struct {
	off     int64
	starts  []int64
	lastKey string
	lastTs  int64
	cmd     bool // the last start is a prompt slash command; its expansion is not a new turn
	idle    bool // claude: the last turn ended, so a ! command opens a turn of its own
}

type wParser struct {
	tool    Tool
	thread  string
	from    int64
	off     int64
	ix      *wIndex
	si      int
	turns   []*wTurn
	cur     *wTurn
	pending map[string]*wItem
	bang    *wItem // last ! command whose output may come on a later line
	slash   *wItem // last local slash command, whose output comes on a later line
	review  *wItem // codex /review waiting for its result
	bgs     []*wItem
	bgByID  map[string]*wItem
	errs    map[string]*wItem // error text to its first item; repeats count there
	cwd     string
	model   string
	dup     map[string]int64 // agent/reasoning text key -> ts, for codex's doubled records
	prevTot *TokenUsage
	lastCmp int64
	repl    replLinks
}

type wSession struct {
	mu      sync.Mutex
	ref     watchedRef
	fi      os.FileInfo
	ix      *wIndex
	tail    *wParser
	lastUse time.Time
	head    [sha256.Size]byte // hash of the first headLen bytes, to see a rewrite in place
	headLen int
	mark    [sha256.Size]byte // hash of the bytes just before markOff, the indexed end
	markOff int64
}

var watchedCache = struct {
	mu sync.Mutex
	m  map[string]*wSession
}{m: map[string]*wSession{}}

// WatchedItems is the GET /api/session/{id}/items answer.
type WatchedItems struct {
	ID      string   `json:"id"`
	Agent   Tool     `json:"agent"`
	Items   []*OItem `json:"items"`
	Runs    []*ORun  `json:"runs"`
	Cursor  int64    `json:"cursor"`
	More    bool     `json:"more"`
	Earlier *int64   `json:"earlier,omitempty"`
	Reset   bool     `json:"reset,omitempty"`
}

func serveWatchedItems(w http.ResponseWriter, r *http.Request, id string) {
	q := r.URL.Query()
	after, before := int64(-1), int64(-1)
	for k, dst := range map[string]*int64{"after": &after, "before": &before} {
		if v := q.Get(k); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": k + " must be a byte offset"})
				return
			}
			*dst = n
		}
	}
	ref := resolveWatched(id)
	if !ref.Resolved || ref.Path == "" {
		writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "no transcript for session"})
		return
	}
	if ref.Tool == ToolCursor {
		read := func() ([]*OItem, []*ORun, error) { return cursorItemsFor(ref.ID, ref.Path) }
		if id, ok := strings.CutPrefix(ref.Path, cursorDeskPrefix); ok {
			read = func() ([]*OItem, []*ORun, error) { return cursorDesktopItems(ref.ID, id) }
		} else if isCursorTranscript(ref.Path) {
			read = func() ([]*OItem, []*ORun, error) { return cursorTranscriptItems(ref.ID, ref.Path) }
		}
		items, runs, err := read()
		if err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		res := &WatchedItems{ID: ref.ID, Agent: ToolCursor, Items: items, Runs: runs, Cursor: cursorChatCursor(items)}
		if after >= 0 && after == res.Cursor {
			res.Items, res.Runs = []*OItem{}, []*ORun{}
		}
		if res.Items == nil {
			res.Items = []*OItem{}
		}
		if res.Runs == nil {
			res.Runs = []*ORun{}
		}
		writeJSON(w, res)
		return
	}
	if ref.Tool != ToolClaude && ref.Tool != ToolCodex {
		writeJSONStatus(w, http.StatusNotImplemented, map[string]any{"error": "chat items are not read for " + string(ref.Tool) + " yet"})
		return
	}
	res, err := watchedItemsFor(ref, after, before)
	if errors.Is(err, fs.ErrNotExist) { // a new session writes its transcript with the first prompt
		writeJSON(w, &WatchedItems{ID: ref.ID, Agent: ref.Tool, Items: []*OItem{}, Runs: []*ORun{}})
		return
	}
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, res)
}

func watchedSessionFor(ref watchedRef) *wSession {
	watchedCache.mu.Lock()
	defer watchedCache.mu.Unlock()
	s := watchedCache.m[ref.ID]
	if s == nil || s.ref.Path != ref.Path {
		s = &wSession{ref: ref}
		watchedCache.m[ref.ID] = s
	}
	s.lastUse = time.Now()
	if len(watchedCache.m) > watchedCacheMax {
		var oldK string
		var old time.Time
		for k, v := range watchedCache.m {
			if v != s && (oldK == "" || v.lastUse.Before(old)) {
				oldK, old = k, v.lastUse
			}
		}
		delete(watchedCache.m, oldK)
	}
	return s
}

// evictIdleWatched drops watched sessions and subagent parses not read for watchedIdleTTL; a reader rebuilds them.
func evictIdleWatched(now time.Time) int {
	n := 0
	watchedCache.mu.Lock()
	for k, v := range watchedCache.m {
		if now.Sub(v.lastUse) > watchedIdleTTL {
			delete(watchedCache.m, k)
			n++
		}
	}
	watchedCache.mu.Unlock()
	subTranscripts.mu.Lock()
	for k, v := range subTranscripts.m {
		if now.Sub(v.lastUse) > watchedIdleTTL {
			delete(subTranscripts.m, k)
			n++
		}
	}
	subTranscripts.mu.Unlock()
	return n
}

// watchedItemsFor answers one request. after<0 and before<0 is the first page.
func watchedItemsFor(ref watchedRef, after, before int64) (*WatchedItems, error) {
	s := watchedSessionFor(ref)
	s.mu.Lock()
	defer s.mu.Unlock()
	if ref.Model != "" {
		s.ref.Model = ref.Model
	}
	f, err := os.Open(ref.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	reset := false
	head := make([]byte, min(st.Size(), watchedHeadLen))
	if _, err := io.ReadFull(io.NewSectionReader(f, 0, int64(len(head))), head); err != nil {
		return nil, err
	}
	rewritten := s.fi != nil && (st.ModTime().Before(s.fi.ModTime()) || len(head) < s.headLen ||
		sha256.Sum256(head[:s.headLen]) != s.head)
	if !rewritten && s.ix != nil && s.markOff > 0 && st.Size() >= s.markOff {
		if h, err := hashBefore(f, s.markOff); err != nil || h != s.mark {
			rewritten = true
		}
	}
	if s.ix == nil || s.fi == nil || !os.SameFile(s.fi, st) || st.Size() < s.ix.off || rewritten {
		reset = s.ix != nil
		s.ix, s.tail = &wIndex{}, nil
	}
	s.fi, s.head, s.headLen = st, sha256.Sum256(head), len(head)
	if err := s.ix.scan(f, ref.Tool, st.Size()); err != nil {
		return nil, err
	}
	if h, err := hashBefore(f, s.ix.off); err == nil {
		s.mark, s.markOff = h, s.ix.off
	}
	out := &WatchedItems{ID: ref.ID, Agent: ref.Tool, Items: []*OItem{}, Runs: []*ORun{}}
	starts := s.ix.starts

	if before >= 0 {
		k := sort.Search(len(starts), func(i int) bool { return starts[i] >= before })
		first := max(0, k-watchedPageTurns)
		out.Cursor = before
		if k == 0 {
			return out, nil
		}
		p := s.newParser(starts[first])
		if err := p.advance(f, before); err != nil {
			return nil, err
		}
		p.refreshBg()
		p.collect(out, 0, -1)
		if first > 0 {
			e := starts[first]
			out.More, out.Earlier = true, &e
		}
		return out, nil
	}

	if after > s.ix.off || reset {
		reset, after = true, -1
	}
	first := max(0, len(starts)-watchedPageTurns)
	from := int64(0)
	if len(starts) > 0 {
		from = starts[first]
	}
	if after >= 0 {
		// The turn holding the cursor must be materialised so its later changes are seen.
		k := sort.Search(len(starts), func(i int) bool { return starts[i] > after })
		if k > 0 {
			from = min(from, starts[k-1])
		}
	}
	if s.tail == nil || s.tail.from > from {
		s.tail = s.newParser(from)
	}
	if err := s.tail.advance(f, s.ix.off); err != nil {
		return nil, err
	}
	s.tail.refreshBg()
	out.Cursor, out.Reset = s.tail.off, reset
	if after >= 0 {
		s.tail.collect(out, after, -1)
	} else {
		s.tail.collect(out, -1, from)
		if first > 0 {
			e := starts[first]
			out.More, out.Earlier = true, &e
		}
	}
	s.tail.trim()
	return out, nil
}

func (s *wSession) newParser(from int64) *wParser {
	p := &wParser{tool: s.ref.Tool, thread: s.ref.ID, from: from, off: from, ix: s.ix, pending: map[string]*wItem{},
		cwd: s.ref.Cwd, model: s.ref.Model, dup: map[string]int64{}, bgByID: map[string]*wItem{}}
	return p
}

// hashBefore hashes up to watchedHeadLen bytes that end at off.
func hashBefore(f *os.File, off int64) ([sha256.Size]byte, error) {
	n := min(off, watchedHeadLen)
	b := make([]byte, n)
	if _, err := io.ReadFull(io.NewSectionReader(f, off-n, n), b); err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(b), nil
}

// forEachLine calls fn for every complete line in [from, to) and returns the offset after the last one.
func forEachLine(f *os.File, from, to int64, fn func(off int64, line []byte)) (int64, error) {
	if to <= from {
		return from, nil
	}
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return from, err
	}
	r := bufio.NewReaderSize(io.LimitReader(f, to-from), int(min(max(to-from, 4096), 1<<20)))
	off := from
	var big []byte
	long := 0 // bytes of a line over watchedLineMax, which is dropped as it is read
	for {
		chunk, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			if long > 0 || len(big)+len(chunk) > watchedLineMax {
				long += len(big) + len(chunk)
				big = nil
			} else {
				big = append(big, chunk...)
			}
			continue
		}
		if err != nil {
			return off, nil // a partial last line waits for its newline
		}
		if long > 0 || len(big)+len(chunk) > watchedLineMax {
			fn(off, skippedLine)
			off += int64(long + len(big) + len(chunk))
			long, big = 0, nil
			continue
		}
		line := chunk
		if big != nil {
			line = append(big, chunk...)
			big = nil
		}
		fn(off, line)
		off += int64(len(line))
	}
}

// ── index ──

func (ix *wIndex) scan(f *os.File, tool Tool, size int64) error {
	end, err := forEachLine(f, ix.off, size, func(off int64, line []byte) {
		if len(line) == 0 {
			return
		}
		var text string
		var ts int64
		if tool == ToolClaude {
			if bytes.Contains(line, bClaudeAsst) {
				ix.cmd = false
				if bytes.Contains(line, bEndTurn) {
					ix.idle = true
				}
			}
			if bytes.Contains(line, bTurnDuration) || bytes.Contains(line, bInterrupted) && claudeInterruptLine(line) {
				ix.idle = true
			}
			var raw string
			raw, ts = claudeUserLine(line)
			text = claudePromptText(raw)
			c, _, isCmd := claudeSlashLine(line)
			switch {
			case text == "" && (ix.idle || len(ix.starts) == 0) && strings.HasPrefix(strings.TrimSpace(raw), "<bash-input>"):
				text = "!" // a ! command after the turn ended: the agent answers it in a turn of its own
			case isCmd:
				ix.cmd = c.Prompt
				if !c.Prompt && len(ix.starts) > 0 {
					text = "" // a local command is an item of the current turn
				} else {
					text = firstOf(text, c.line())
				}
			case text != "" && ix.cmd:
				ix.cmd, text = false, ""
			}
		} else {
			text, ts = codexPromptLine(line)
			if text == "" && len(ix.starts) == 0 {
				if cmd, _, ok := codexUserShellLine(line); ok {
					text = "!" + cmd
				}
			}
			if text != "" {
				key := dupKey(text)
				if key == ix.lastKey && abs64(ts-ix.lastTs) < codexDupWindowMs {
					return
				}
				ix.lastKey, ix.lastTs = key, ts
			}
		}
		if text != "" {
			ix.starts = append(ix.starts, off)
			if text != "!" {
				ix.idle = false
			}
		}
	})
	ix.off = end
	return err
}

func dupKey(s string) string { return clipString(strings.Join(strings.Fields(s), " "), 200) }

var (
	bClaudeUser    = []byte(`"type":"user"`)
	bClaudeAsst    = []byte(`"type":"assistant"`)
	bToolResult    = []byte(`"type":"tool_result"`)
	bIsMeta        = []byte(`"isMeta":true`)
	bIsSidechain   = []byte(`"isSidechain":true`)
	bIsCompactSum  = []byte(`"isCompactSummary":true`)
	bCodexUserMsg  = []byte(`"user_message"`)
	bCodexUserItem = []byte(`"UserMessage"`)
	bCodexUserRole = []byte(`"role":"user"`)
	bCodexReview   = []byte(`"entered_review_mode"`)
	bEndTurn       = []byte(`"stop_reason":"end_turn"`)
	bTurnDuration  = []byte(`"subtype":"turn_duration"`)
	bInterrupted   = []byte(`[Request interrupted`)
)

// claudeInterruptLine reports a user line with a text part that starts "[Request interrupted", as the parser reads it.
func claudeInterruptLine(line []byte) bool {
	if !bytes.Contains(line, bClaudeUser) {
		return false
	}
	var j wClaudeLine
	if json.Unmarshal(line, &j) != nil || j.Type != "user" || j.Message == nil || j.IsMeta || j.IsSidechain || j.IsCompactSummary {
		return false
	}
	var parts []wClaudePart
	if c := j.Message.Content; len(c) > 0 && c[0] == '"' {
		var s string
		_ = json.Unmarshal(c, &s)
		parts = []wClaudePart{{Type: "text", Text: s}}
	} else {
		_ = json.Unmarshal(c, &parts)
	}
	for _, pt := range parts {
		if pt.Type == "text" && strings.HasPrefix(strings.TrimSpace(pt.Text), "[Request interrupted") {
			return true
		}
	}
	return false
}

// claudePromptLine returns the prompt a line opens a turn with, or "". Escaped
// quotes inside strings never match the structural byte patterns.
func claudePromptLine(line []byte) (string, int64) {
	raw, ts := claudeUserLine(line)
	return claudePromptText(raw), ts
}

// claudeUserLine returns the text a person typed on a user line, before prompt filtering.
func claudeUserLine(line []byte) (string, int64) {
	if !bytes.Contains(line, bClaudeUser) || bytes.Contains(line, bToolResult) || bytes.Contains(line, bIsMeta) ||
		bytes.Contains(line, bIsSidechain) || bytes.Contains(line, bIsCompactSum) {
		return "", 0
	}
	var j struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		Message   struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &j) != nil || j.Type != "user" || j.Message.Role != "user" {
		return "", 0
	}
	text, isResult := claudeUserText(j.Message.Content)
	if isResult {
		return "", 0
	}
	return text, parseTimestamp(j.Timestamp)
}

// claudeUserText joins the text blocks of a user message.
func claudeUserText(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	if raw[0] == '"' {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s, false
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(raw, &parts)
	var texts, real []string
	for _, p := range parts {
		switch p.Type {
		case "tool_result":
			return "", true
		case "text":
			texts = append(texts, p.Text)
			if !isReminderBlock(p.Text) {
				real = append(real, p.Text)
			}
		}
	}
	// Claude desktop puts a <system-reminder> part before the user's own text; keep the user's text.
	if len(real) > 0 {
		return strings.Join(real, "\n\n"), false
	}
	return strings.Join(texts, "\n\n"), false
}

func isReminderBlock(s string) bool {
	t := strings.TrimSpace(s)
	return strings.HasPrefix(t, "<system-reminder>") && strings.HasSuffix(t, "</system-reminder>")
}

type wCodexPayload struct {
	codexPayloadRec
	Model            string          `json:"model"`
	Info             json.RawMessage `json:"info"`
	Summary          json.RawMessage `json:"summary"`
	Text             string          `json:"text"`
	Reason           string          `json:"reason"`
	CompletedAtMs    int64           `json:"completed_at_ms"`
	Query            string          `json:"query"`
	Item             *wCodexItem     `json:"item"`
	LastAgentMessage string          `json:"last_agent_message"`
	UserFacingHint   string          `json:"user_facing_hint"`
	ReviewOutput     *struct {
		Findings    []json.RawMessage `json:"findings"`
		Correctness string            `json:"overall_correctness"`
		Explanation string            `json:"overall_explanation"`
	} `json:"review_output"`
	Goal *struct {
		Objective string `json:"objective"`
		Status    string `json:"status"`
		CreatedAt int64  `json:"createdAt"`
		UpdatedAt int64  `json:"updatedAt"`
	} `json:"goal"`
}

type wCodexItem struct {
	codexItemRec
	Status      string          `json:"status"`
	SummaryText []string        `json:"summary_text"`
	Kind        string          `json:"kind"`
	Query       string          `json:"query"`
	Action      json.RawMessage `json:"action"`
	AgentPath   string          `json:"agent_path"`
	Path        string          `json:"path"`
	Stderr      string          `json:"stderr"`
	Text        string          `json:"text"`
}

func codexPromptLine(line []byte) (string, int64) {
	if !bytes.Contains(line, bCodexUserMsg) && !bytes.Contains(line, bCodexUserItem) && !bytes.Contains(line, bCodexUserRole) && !bytes.Contains(line, bCodexReview) {
		return "", 0
	}
	var rec codexLineRec
	if json.Unmarshal(line, &rec) != nil || len(rec.Payload) == 0 || rec.Payload[0] != '{' {
		return "", 0
	}
	var p wCodexPayload
	if json.Unmarshal(rec.Payload, &p) != nil {
		return "", 0
	}
	return codexPromptOf(rec.Type, &p), parseTimestamp(rec.Timestamp)
}

func codexPromptOf(top string, p *wCodexPayload) string {
	var t string
	switch {
	case top == "event_msg" && p.Type == "user_message":
		t = p.Message
	case top == "event_msg" && p.Type == "item_completed" && p.Item != nil && p.Item.Type == "UserMessage":
		t = codexFlattenContent(rawToAny(p.Item.Content))
	case top == "response_item" && p.Type == "message" && p.Role == "user":
		t = codexFlattenContent(rawToAny(p.Content))
	case top == "event_msg" && p.Type == "entered_review_mode":
		return strings.TrimSpace("/review " + p.UserFacingHint)
	default:
		return ""
	}
	t = strings.TrimSpace(t)
	if codexLooksLikeSystemPrompt(t) || strings.HasPrefix(t, "<turn_aborted>") || strings.HasPrefix(t, "<subagent_notification>") {
		return ""
	}
	return t
}

// ── parser ──

func (p *wParser) advance(f *os.File, to int64) error {
	end, err := forEachLine(f, p.off, to, p.line)
	p.off = end
	return err
}

func (p *wParser) line(off int64, b []byte) {
	starts := p.ix.starts
	for p.si < len(starts) && starts[p.si] < off {
		p.si++
	}
	start := p.si < len(starts) && starts[p.si] == off
	if start {
		p.si++
	}
	if len(b) == 0 {
		p.add(off, 0, &OItem{Kind: "other", Status: "failed", Command: "transcript", Text: fmt.Sprintf("A transcript line over %d KB was skipped.", watchedLineMax>>10)})
		return
	}
	if p.tool == ToolClaude {
		p.claudeLine(off, b, start)
	} else {
		p.codexLine(off, b, start)
	}
}

func (p *wParser) startTurn(off, ts int64, prompt string) {
	if c := p.cur; c != nil && c.run.Status == "running" {
		c.run.Status, c.run.EndedAt, c.runMod = "completed", c.lastTs, off
		p.closeOrphans(c, off, c.lastTs, "completed")
	}
	if wi := p.bang; wi != nil { // a ! command whose output never came
		p.bang = nil
		if wi.it.Status == "started" {
			p.complete(off, wi, "completed", max(ts, wi.it.StartedAt), "", nil)
		}
	}
	runID := fmt.Sprintf("%s@%d", p.thread, off)
	t := &wTurn{off: off, model: p.model, lastTs: ts, runMod: off,
		run: &ORun{ID: runID, ThreadID: p.thread, Index: p.si, Prompt: prompt, Status: "running", StartedAt: ts}}
	p.turns = append(p.turns, t)
	p.cur = t
	t.user = p.add(off, 0, &OItem{Kind: "user", Status: "completed", Text: prompt, StartedAt: ts, EndedAt: ts})
}

// startCmdTurn opens a turn with a slash command: the command item comes first, then the prompt.
func (p *wParser) startCmdTurn(off, ts int64, c slashCmd, prompt string) {
	p.startTurn(off, ts, prompt)
	t := p.cur
	t.run.Prompt = firstOf(prompt, c.line())
	t.cmd = c.Prompt
	wi := p.add(off, 1, &OItem{Kind: "command", By: "user", Status: "completed", Text: c.Name, Command: c.line(), StartedAt: ts, EndedAt: ts})
	t.items = append([]*wItem{wi}, t.items[:len(t.items)-1]...)
	if !c.Prompt {
		p.slash = wi
	}
}

// startBangTurn opens a turn with a ! command, placed before the empty prompt like a leading slash command.
func (p *wParser) startBangTurn(off, ts int64, content json.RawMessage) {
	p.startTurn(off, ts, "")
	t := p.cur
	t.bang = true
	// No prompt bubble: it would be empty and share its id with the command item.
	t.items, t.user = t.items[:0], nil
	p.claudeBang(off, ts, content)
	if len(t.items) > 0 && t.items[0].it.Kind == "command" {
		t.run.Prompt = "!" + t.items[0].it.Command
	}
}

// claudeUserCmd handles slash command lines, their local output, a prompt command's
// expansion and background task notifications. It reports whether the line was used.
func (p *wParser) claudeUserCmd(off, ts int64, meta bool, content json.RawMessage) bool {
	text, isResult := claudeUserText(content)
	if isResult || p.cur == nil {
		return false
	}
	t := p.cur
	if c, ok := parseSlashCmd(text); ok && !meta {
		p.touch(off, ts)
		wi := p.add(off, 0, &OItem{Kind: "command", By: "user", Status: "completed", Text: c.Name, Command: c.line(), StartedAt: ts, EndedAt: ts})
		p.slash = nil
		if !c.Prompt {
			p.slash = wi
		}
		return true
	}
	if out, isErr, ok := localCmdOutput(text); ok {
		if wi := p.slash; wi != nil {
			wi.it.Output = clipString(strings.TrimSpace(strings.Join(nonEmpty(wi.it.Output, out), "\n")), orchOutputCap)
			if isErr {
				wi.it.Status = "failed"
			}
			wi.mod = off
		}
		return true
	}
	if p.claudeTaskNotification(off, ts, text) {
		return true
	}
	if t.cmd && !claudeLooksLikeMeta(text) {
		t.cmd = false
		if !strings.HasPrefix(strings.TrimSpace(text), skillBasePrefix) && t.user != nil {
			prompt := strings.TrimSpace(text)
			t.user.it.Text, t.user.mod = prompt, off
			t.run.Prompt, t.runMod = prompt, off
		}
		return true
	}
	return false
}

// add appends an item to the current turn; n separates items from one line.
func (p *wParser) add(off int64, n int, it *OItem) *wItem {
	t := p.cur
	if t == nil {
		return nil
	}
	if it.ID == "" {
		it.ID = fmt.Sprintf("%s@%d.%d", p.thread, off, n)
	}
	it.ThreadID, it.RunID = p.thread, t.run.ID
	wi := &wItem{it: it, mod: off}
	t.items = append(t.items, wi)
	return wi
}

func (p *wParser) touch(off, ts int64) {
	if t := p.cur; t != nil && ts > t.lastTs {
		t.lastTs = ts
	}
}

func (p *wParser) endRun(off, ts int64, status string) {
	t := p.cur
	if t == nil {
		return
	}
	t.run.Status, t.run.EndedAt, t.runMod = status, max(ts, t.run.StartedAt), off
	p.closeOrphans(t, off, ts, status)
}

// closeOrphans ends tool calls of an ended turn that never got a result, so no row keeps running.
func (p *wParser) closeOrphans(t *wTurn, off, ts int64, status string) {
	for id, wi := range p.pending {
		if wi.it.RunID != t.run.ID || wi.it.Status != "started" {
			continue
		}
		st := "completed"
		if status == "interrupted" || status == "failed" {
			st = "failed"
		}
		p.complete(off, wi, st, max(ts, wi.it.StartedAt), "", nil)
		delete(p.pending, id)
	}
}

func (p *wParser) reopen(off int64) {
	if t := p.cur; t != nil && t.run.Status == "completed" {
		t.run.Status, t.run.EndedAt, t.runMod = "running", 0, off
	}
}

func (p *wParser) setUsage(off int64) {
	t := p.cur
	u := &OUsage{InTokens: t.tok.Input + t.tok.CacheRead + t.tok.CacheCreate, OutTokens: t.tok.Output,
		CostUSD: estimateCost(firstOf(t.model, p.model), t.tok)}
	t.run.Usage, t.runMod = u, off
}

func (p *wParser) complete(off int64, wi *wItem, status string, endTs int64, output string, exit *int) {
	it := wi.it
	if status != "" {
		it.Status = status
	}
	if endTs > 0 {
		it.EndedAt = endTs
	}
	if output != "" {
		it.Output = clipString(output, orchOutputCap)
	}
	if exit != nil {
		it.ExitCode = exit
	}
	wi.mod = off
}

// replEvents completes the Code Mode scripts whose repl-eval events this text delivers.
func (p *wParser) replEvents(off int64, text string) {
	for _, ev := range parseReplEvents(text) {
		wi := p.repl.byEval[ev.eval]
		if wi == nil {
			continue
		}
		status := "completed"
		if ev.status != "settled" {
			status = "failed"
		}
		p.complete(off, wi, status, ev.at, ev.result, nil)
		p.repl.cur = wi
	}
}

// ── claude ──

// claudeErrorItem turns an error record into a thread item: hook failures, Stop hook errors, API error
// replies and system lines at level error. Other records give nil.
func claudeErrorItem(j *wClaudeLine, ts int64) *OItem {
	text := ""
	switch {
	case j.Attachment != nil && strings.HasPrefix(j.Attachment.Type, "hook_") && strings.Contains(j.Attachment.Type, "error"):
		text = firstOf(j.Attachment.HookName, "A hook") + " hook failed: " + firstOf(strings.TrimSpace(j.Attachment.Stderr), strings.TrimSpace(j.Attachment.Content), "no output")
	case j.Type == "system" && j.Subtype == "stop_hook_summary" && len(j.HookErrors) > 0:
		text = "Stop hook failed: " + strings.Join(j.HookErrors, "\n")
	case j.Type == "assistant" && j.IsAPIError && j.Message != nil:
		t, _ := claudeUserText(j.Message.Content)
		text = firstOf(strings.TrimSpace(t), "API error")
	case j.Type == "system" && j.Level == "error" && j.Subtype != "api_error":
		text = firstOf(strings.TrimSpace(j.Content), "error")
	default:
		return nil
	}
	return &OItem{Kind: "error", Status: "failed", Text: clipString(text, 2000), StartedAt: ts, EndedAt: ts}
}

type wClaudeLine struct {
	Type             string          `json:"type"`
	Subtype          string          `json:"subtype"`
	Timestamp        string          `json:"timestamp"`
	Cwd              string          `json:"cwd"`
	Content          string          `json:"content"`
	IsMeta           bool            `json:"isMeta"`
	WakeupSource     string          `json:"wakeupSource"`
	IsSidechain      bool            `json:"isSidechain"`
	IsCompactSummary bool            `json:"isCompactSummary"`
	Level            string          `json:"level"`
	IsAPIError       bool            `json:"isApiErrorMessage"`
	IsVirtual        bool            `json:"isVirtual"`
	HookErrors       []string        `json:"hookErrors"`
	ToolUseResult    json.RawMessage `json:"toolUseResult"`
	Attachment       *struct {
		Type      string   `json:"type"`
		Envelopes []string `json:"envelopes"`
		HookName  string   `json:"hookName"`
		Stderr    string   `json:"stderr"`
		Content   string   `json:"content"`
	} `json:"attachment"`
	Message *struct {
		ID         string          `json:"id"`
		Role       string          `json:"role"`
		Model      string          `json:"model"`
		StopReason string          `json:"stop_reason"`
		Content    json.RawMessage `json:"content"`
		Usage      *struct {
			Input       int64 `json:"input_tokens"`
			Output      int64 `json:"output_tokens"`
			CacheRead   int64 `json:"cache_read_input_tokens"`
			CacheCreate int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

type wClaudePart struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     map[string]any  `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

func (p *wParser) claudeLine(off int64, b []byte, start bool) {
	var j wClaudeLine
	if json.Unmarshal(b, &j) != nil {
		return
	}
	ts := parseTimestamp(j.Timestamp)
	if j.Cwd != "" {
		p.cwd = j.Cwd
	}
	if j.IsSidechain {
		return
	}
	if it := claudeErrorItem(&j, ts); it != nil {
		if prev := p.errs[it.Text]; prev != nil {
			prev.it.Repeat = max(prev.it.Repeat, 1) + 1
			prev.it.EndedAt, prev.mod = ts, off
			return
		}
		if wi := p.add(off, 0, it); wi != nil {
			if p.errs == nil {
				p.errs = map[string]*wItem{}
			}
			p.errs[it.Text] = wi
		}
		return
	}
	if start && j.Message != nil {
		text, _ := claudeUserText(j.Message.Content)
		if strings.HasPrefix(strings.TrimSpace(text), "<bash-input>") {
			p.startBangTurn(off, ts, j.Message.Content)
			return
		}
		if c, ok := parseSlashCmd(text); ok {
			prompt := ""
			if c.Prompt {
				prompt = claudePromptText(text)
			}
			p.startCmdTurn(off, ts, c, prompt)
			return
		}
		p.startTurn(off, ts, claudePromptText(text))
		return
	}
	if j.Type == "attachment" && j.Attachment != nil && j.Attachment.Type == "poll_events" {
		p.replEvents(off, strings.Join(j.Attachment.Envelopes, "\n"))
		return
	}
	switch j.Type {
	case "system":
		switch j.Subtype {
		case "turn_duration":
			if t := p.cur; t != nil && t.run.Status == "running" {
				p.endRun(off, ts, "completed")
			}
		case "compact_boundary":
			p.add(off, 0, &OItem{Kind: "other", Status: "completed", Text: "Context compacted", StartedAt: ts, EndedAt: ts})
		case "api_error":
			p.add(off, 0, &OItem{Kind: "error", Status: "failed", Text: firstOf(j.Content, "API error"), StartedAt: ts, EndedAt: ts})
		}
		return
	case "user", "assistant":
	default:
		return
	}
	// A scheduled task or wakeup prompts Claude by itself: it opens its own turn, so its reply never hides the one before.
	if j.Type == "user" && j.IsMeta && j.WakeupSource != "" && j.Message != nil {
		text, _ := claudeUserText(j.Message.Content)
		p.startTurn(off, ts, wakeupLabel(j.WakeupSource, text))
		p.cur.user.it.By = "schedule"
		return
	}
	if j.Type == "user" && j.Message != nil && !j.IsCompactSummary && (p.cur != nil && p.cur.cmd || bytes.Contains(b, bLocalCmd) || bytes.Contains(b, bTaskNote)) &&
		p.claudeUserCmd(off, ts, j.IsMeta, j.Message.Content) {
		return
	}
	if j.Type == "user" && !j.IsMeta && j.Message != nil && (bytes.Contains(b, []byte("bash-input")) || bytes.Contains(b, []byte("bash-stdout")) || bytes.Contains(b, []byte("bash-stderr"))) && p.claudeBang(off, ts, j.Message.Content) {
		return
	}
	if j.Message == nil || j.IsMeta || j.IsCompactSummary || p.cur == nil {
		return
	}
	m := j.Message
	if m.Model != "" && m.Model != "<synthetic>" {
		p.model, p.cur.model = m.Model, m.Model
	}
	var parts []wClaudePart
	if len(m.Content) > 0 && m.Content[0] == '"' {
		var s string
		_ = json.Unmarshal(m.Content, &s)
		parts = []wClaudePart{{Type: "text", Text: s}}
	} else {
		_ = json.Unmarshal(m.Content, &parts)
	}
	p.touch(off, ts)
	if j.Type == "user" {
		for _, part := range parts {
			switch part.Type {
			case "tool_result":
				wi := p.pending[part.ToolUseID]
				if wi == nil {
					continue
				}
				delete(p.pending, part.ToolUseID)
				res := claudeResultText(part.Content)
				if strings.Contains(res, `kind="repl-eval"`) {
					defer p.replEvents(off, res)
				}
				if wi.it.Command == "REPL" && p.repl.dispatched(wi, j.ToolUseResult) {
					continue // the script runs on; its repl-eval event completes the item
				}
				status := "completed"
				if part.IsError {
					status = "failed"
				}
				var exit *int
				if wi.it.Kind == "command" {
					if mm := reExitCode.FindStringSubmatch(res[:min(len(res), 200)]); mm != nil {
						code, _ := strconv.Atoi(mm[1])
						exit = &code
					} else if !part.IsError {
						code := 0
						exit = &code
					}
					if exit != nil && *exit != 0 {
						status = "failed"
					}
				}
				p.complete(off, wi, status, ts, res, exit)
			case "text":
				if strings.HasPrefix(strings.TrimSpace(part.Text), "[Request interrupted") {
					p.endRun(off, ts, "interrupted")
				}
			}
		}
		return
	}
	p.reopen(off)
	p.cur.cmd = false
	for i, part := range parts {
		switch part.Type {
		case "text":
			if strings.TrimSpace(part.Text) != "" {
				p.add(off, i, &OItem{Kind: "agent", Status: "completed", Text: part.Text, StartedAt: ts, EndedAt: ts})
			}
		case "thinking":
			if strings.TrimSpace(part.Thinking) != "" {
				p.add(off, i, &OItem{Kind: "reasoning", Status: "completed", Text: part.Thinking, StartedAt: ts, EndedAt: ts})
			}
		case "tool_use":
			if part.ID == "" || part.Name == "" {
				continue
			}
			if part.Input == nil {
				part.Input = map[string]any{}
			}
			it := claudeToolItem(part.Name, part.Input, p.cwd)
			it.ID, it.Status, it.StartedAt = p.thread+"/"+part.ID, "started", ts
			if j.IsVirtual && strings.HasPrefix(part.ID, "repl_") && p.repl.cur != nil {
				it.ParentID = p.repl.cur.it.ID
			}
			if wi := p.add(off, i, it); wi != nil {
				p.pending[part.ID] = wi
			}
		}
	}
	if u := m.Usage; u != nil {
		t := p.cur
		cur := TokenUsage{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheCreate: u.CacheCreate}
		if m.ID != "" {
			if t.msgTok == nil {
				t.msgTok = map[string]TokenUsage{}
			}
			prev := t.msgTok[m.ID]
			t.msgTok[m.ID] = cur
			cur = TokenUsage{Input: u.Input - prev.Input, Output: u.Output - prev.Output, CacheRead: u.CacheRead - prev.CacheRead, CacheCreate: u.CacheCreate - prev.CacheCreate}
		}
		t.tok.Input += cur.Input
		t.tok.Output += cur.Output
		t.tok.CacheRead += cur.CacheRead
		t.tok.CacheCreate += cur.CacheCreate
		p.setUsage(off)
	}
	if m.StopReason == "end_turn" {
		p.endRun(off, ts, "completed")
	}
}

// ── codex ──

var codexShellTools = map[string]bool{"exec_command": true, "shell": true, "local_shell": true, "shell_command": true, "container.exec": true}

func (p *wParser) codexDup(text string, ts int64) bool {
	k := dupKey(text)
	if prev, ok := p.dup[k]; ok && abs64(ts-prev) < codexDupWindowMs {
		return true
	}
	if len(p.dup) > 64 {
		p.dup = map[string]int64{}
	}
	p.dup[k] = ts
	return false
}

func (p *wParser) codexText(off int64, n int, kind, text string, ts int64) {
	if strings.TrimSpace(text) == "" || p.codexDup(kind+"|"+text, ts) {
		return
	}
	p.add(off, n, &OItem{Kind: kind, Status: "completed", Text: text, StartedAt: ts, EndedAt: ts})
}

func (p *wParser) codexLine(off int64, b []byte, start bool) {
	var rec codexLineRec
	if json.Unmarshal(b, &rec) != nil {
		return
	}
	if codexSkipTop[rec.Type] || len(rec.Payload) == 0 || rec.Payload[0] != '{' {
		return
	}
	var pl wCodexPayload
	if json.Unmarshal(rec.Payload, &pl) != nil {
		return
	}
	ts := parseTimestamp(rec.Timestamp)
	if start {
		if pl.Type == "entered_review_mode" {
			p.startCmdTurn(off, ts, slashCmd{Name: "/review", Args: strings.TrimSpace(pl.UserFacingHint)}, "")
			p.review, p.slash = p.slash, nil
			return
		}
		if cmd, ok := codexUserShell(rec.Type, &pl); ok {
			// A ! command before any prompt opens its own turn, as a leading Claude local command does.
			p.startTurn(off, ts, "")
			p.cur.run.Prompt = "!" + cmd
			p.touch(off, ts)
			p.codexEvent(off, ts, &pl)
			p.endRun(off, ts, "completed")
			return
		}
		p.startTurn(off, ts, codexPromptOf(rec.Type, &pl))
		return
	}
	switch rec.Type {
	case "turn_context":
		if pl.Model != "" {
			p.model = pl.Model
		}
		if pl.Cwd != "" {
			p.cwd = pl.Cwd
		}
		return
	case "compacted":
		p.codexCompacted(off, ts)
		return
	}
	if p.cur == nil {
		return
	}
	p.touch(off, ts)
	switch rec.Type {
	case "event_msg":
		p.codexEvent(off, ts, &pl)
	case "response_item":
		p.codexResponseItem(off, ts, &pl)
	}
}

func (p *wParser) codexCompacted(off, ts int64) {
	if p.cur == nil || abs64(ts-p.lastCmp) < 5_000 {
		return
	}
	p.lastCmp = ts
	p.add(off, 0, &OItem{Kind: "other", Status: "completed", Text: "Context compacted", StartedAt: ts, EndedAt: ts})
}

func (p *wParser) codexEvent(off, ts int64, pl *wCodexPayload) {
	switch pl.Type {
	case "agent_message":
		p.codexText(off, 0, "agent", pl.Message, ts)
	case "agent_reasoning":
		p.codexText(off, 0, "reasoning", pl.Text, ts)
	case "task_complete":
		if last := pl.LastAgentMessage; last != "" {
			p.codexText(off, 1, "agent", last, ts)
		}
		p.endRun(off, ts, "completed")
	case "turn_aborted":
		p.endRun(off, ts, "interrupted")
	case "context_compacted":
		p.codexCompacted(off, ts)
	case "error":
		p.add(off, 0, &OItem{Kind: "error", Status: "failed", Text: firstOf(pl.Message, "error"), StartedAt: ts, EndedAt: ts})
	case "token_count":
		var info map[string]any
		if json.Unmarshal(pl.Info, &info) != nil || info == nil {
			return
		}
		d := codexUsageDelta(info, &p.prevTot)
		t := p.cur
		t.tok.Input += d.Input
		t.tok.CacheRead += d.CacheRead
		t.tok.Output += d.Output
		p.setUsage(off)
	case "exec_command_end", "mcp_tool_call_end", "patch_apply_end", "web_search_end":
		wi := p.pending[pl.CallID]
		if wi == nil {
			return
		}
		delete(p.pending, pl.CallID)
		status, out := "completed", ""
		var exit *int
		switch pl.Type {
		case "exec_command_end":
			out = firstOf(pl.AggregatedOutput, pl.Stdout)
			if pl.Stderr != "" && pl.AggregatedOutput == "" {
				out += "\n" + pl.Stderr
			}
			code := 0
			if pl.ExitCode != nil {
				code = int(*pl.ExitCode)
			}
			exit = &code
			if code != 0 {
				status = "failed"
			}
		case "mcp_tool_call_end":
			var res map[string]any
			_ = json.Unmarshal(pl.Result, &res)
			out = codexExtractMCPResult(res)
			if _, isErr := res["Err"]; isErr {
				status = "failed"
			}
			wi.it.Kind = "mcp"
		case "patch_apply_end":
			out = pl.Stdout + pl.Stderr
			if pl.Success != nil && !*pl.Success {
				status = "failed"
			}
		}
		p.complete(off, wi, status, ts, out, exit)
	case "item_completed":
		if pl.Item != nil {
			p.codexItem(off, ts, pl)
		}
	case "exited_review_mode":
		if wi, r := p.review, pl.ReviewOutput; wi != nil && r != nil {
			out := strings.Join(nonEmpty(r.Correctness, r.Explanation), "\n")
			if n := len(r.Findings); n > 0 {
				out += fmt.Sprintf("\n%d findings", n)
			}
			p.complete(off, wi, "", ts, strings.TrimSpace(out), nil)
		}
		p.review = nil
	case "thread_goal_updated":
		// Only the record that creates a goal is the user's /goal; later ones are progress.
		if g := pl.Goal; g != nil && g.Objective != "" && g.CreatedAt == g.UpdatedAt {
			c := slashCmd{Name: "/goal", Args: g.Objective}
			p.add(off, 0, &OItem{Kind: "command", By: "user", Status: "completed", Text: c.Name, Command: c.line(), StartedAt: ts, EndedAt: ts})
		}
	}
}

func (p *wParser) codexResponseItem(off, ts int64, pl *wCodexPayload) {
	switch pl.Type {
	case "message":
		if pl.Role == "assistant" {
			p.codexText(off, 0, "agent", codexFlattenContent(rawToAny(pl.Content)), ts)
		}
	case "reasoning":
		var sum []struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(pl.Summary, &sum)
		var parts []string
		for _, s := range sum {
			parts = append(parts, s.Text)
		}
		p.codexText(off, 0, "reasoning", strings.Join(parts, "\n\n"), ts)
	case "function_call", "custom_tool_call", "web_search_call":
		name := pl.Name
		if pl.Type == "web_search_call" {
			name = "web_search"
		}
		if name == "" || name == "exec" { // "exec" wraps a script; its calls arrive as item_completed records
			return
		}
		args := pl.Input
		if args == "" && len(pl.Arguments) > 0 {
			if pl.Arguments[0] == '"' {
				_ = json.Unmarshal(pl.Arguments, &args)
			} else {
				args = string(pl.Arguments)
			}
		}
		it := codexToolOItem(name, args, p.cwd)
		it.Status, it.StartedAt = "started", ts
		if pl.CallID != "" {
			it.ID = p.thread + "/" + pl.CallID
		}
		if wi := p.add(off, 0, it); wi != nil && pl.CallID != "" {
			p.pending[pl.CallID] = wi
		}
	case "function_call_output", "custom_tool_call_output":
		wi := p.pending[pl.CallID]
		if wi == nil {
			return
		}
		out := ""
		if len(pl.Output) > 0 && pl.Output[0] == '"' {
			_ = json.Unmarshal(pl.Output, &out)
		} else {
			out = codexFlattenContent(rawToAny(pl.Output))
		}
		var exit *int
		status := "completed"
		if wi.it.Kind == "command" {
			var meta struct {
				Output   string `json:"output"`
				Metadata struct {
					ExitCode *int `json:"exit_code"`
				} `json:"metadata"`
			}
			if json.Unmarshal([]byte(out), &meta) == nil && meta.Metadata.ExitCode != nil {
				exit, out = meta.Metadata.ExitCode, meta.Output
			} else if mm := reExitCode.FindStringSubmatch(out[:min(len(out), 200)]); mm != nil {
				code, _ := strconv.Atoi(mm[1])
				exit = &code
			}
			if exit != nil && *exit != 0 {
				status = "failed"
			}
		}
		// exec_command_end may still follow with the authoritative exit code.
		p.complete(off, wi, status, ts, out, exit)
		if wi.it.Kind != "command" && wi.it.Kind != "mcp" && wi.it.Kind != "fileChange" {
			delete(p.pending, pl.CallID)
		}
	}
}

// codexToolOItem maps a pre-item_completed Codex tool call to an item.
func codexToolOItem(name, args, cwd string) *OItem {
	it := &OItem{Kind: "other", Command: name, Text: codexToolResource(name, args)}
	var in map[string]any
	_ = json.Unmarshal([]byte(args), &in)
	switch {
	case codexShellTools[name]:
		it.Kind, it.Command, it.Text = "command", shellCommandOf(in), ""
		it.Cwd = firstOf(claudeStr(in, "workdir"), cwd)
	case name == "apply_patch":
		it.Kind, it.Command = "fileChange", ""
		patch := args
		if s := claudeStr(in, "input", "patch"); s != "" {
			patch = s
		}
		it.Files = patchFileStats(patch)
		var paths []string
		for _, f := range it.Files {
			paths = append(paths, f.Path)
		}
		it.Text = strings.Join(paths, "\n")
	case strings.HasPrefix(name, "mcp__"):
		it.Kind = "mcp"
	case name == "web_search":
		it.Kind = "web"
	case name == "update_plan":
		it.Kind, it.Command, it.Text = "plan", "", args
	}
	return it
}

// patchFileStats counts added and removed lines per file of an apply_patch body.
func patchFileStats(patch string) []OFile {
	var out []OFile
	var cur *OFile
	for _, l := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(l, "*** Add File: "):
			out = append(out, OFile{Path: strings.TrimSpace(l[14:]), Kind: "add"})
			cur = &out[len(out)-1]
		case strings.HasPrefix(l, "*** Update File: "):
			out = append(out, OFile{Path: strings.TrimSpace(l[17:]), Kind: "update"})
			cur = &out[len(out)-1]
		case strings.HasPrefix(l, "*** Delete File: "):
			out = append(out, OFile{Path: strings.TrimSpace(l[17:]), Kind: "delete"})
			cur = nil
		case cur == nil || strings.HasPrefix(l, "***"):
		case strings.HasPrefix(l, "+"):
			cur.Add++
		case strings.HasPrefix(l, "-"):
			cur.Del++
		}
	}
	return out
}

func (p *wParser) codexItem(off, ts int64, pl *wCodexPayload) {
	ci := pl.Item
	startTs, endTs := pl.StartedAtMs, pl.CompletedAtMs
	if startTs == 0 {
		startTs = ts - ci.Duration.ms()
	}
	if endTs == 0 {
		endTs = startTs + ci.Duration.ms()
	}
	it := &OItem{Status: codexStatus(strings.ToLower(ci.Status), "completed"), StartedAt: startTs, EndedAt: endTs}
	if ci.ID != "" {
		it.ID = p.thread + "/" + ci.ID
	}
	switch ci.Type {
	case "UserMessage":
		return
	case "AgentMessage":
		p.codexText(off, 0, "agent", codexFlattenContent(rawToAny(ci.Content)), ts)
		return
	case "Reasoning":
		p.codexText(off, 0, "reasoning", strings.Join(ci.SummaryText, "\n\n"), ts)
		return
	case "ContextCompaction":
		p.codexCompacted(off, ts)
		return
	case "CommandExecution":
		s := codexCommandString(ci.Command)
		it.Kind, it.Command, it.Cwd = "command", s, strings.TrimPrefix(ci.Cwd, "file://")
		if ci.Source == "user_shell" {
			it.By, it.Text = "user", "Shell command"
		}
		it.Output = clipString(ci.AggregatedOutput, orchOutputCap)
		if ci.ExitCode != nil {
			code := int(*ci.ExitCode)
			it.ExitCode = &code
			if code != 0 {
				it.Status = "failed"
			}
		}
	case "FileChange":
		it.Kind = "fileChange"
		paths := make([]string, 0, len(ci.Changes))
		for path := range ci.Changes {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		for _, path := range paths {
			var ch struct {
				Type        string `json:"type"`
				Content     string `json:"content"`
				UnifiedDiff string `json:"unified_diff"`
			}
			_ = json.Unmarshal(ci.Changes[path], &ch)
			f := OFile{Path: path, Kind: ch.Type}
			if ch.UnifiedDiff != "" {
				f.Add, f.Del = diffCounts(ch.Type, ch.UnifiedDiff)
			} else if ch.Type == "add" {
				f.Add = claudeLines(ch.Content)
			} else if ch.Type == "delete" {
				f.Del = claudeLines(ch.Content)
			}
			it.Files = append(it.Files, f)
		}
		it.Text = strings.Join(paths, "\n")
		it.Output = clipString(ci.Stderr, orchOutputCap)
	case "McpToolCall":
		it.Kind, it.Text = "mcp", ci.Server+"."+ci.Tool
		var res map[string]any
		_ = json.Unmarshal(ci.Result, &res)
		if res != nil {
			it.Output = clipString(codexExtractMCPResult(map[string]any{"Ok": res}), orchOutputCap)
		}
		if len(ci.Error) > 0 && string(ci.Error) != "null" {
			it.Status = "failed"
			it.Output = clipString(string(ci.Error)+"\n"+it.Output, orchOutputCap)
		}
	case "Extension":
		it.Kind, it.Text = "other", ci.Kind
		if strings.HasPrefix(ci.Kind, "web.") {
			it.Kind = "web"
			var a struct {
				Queries []string `json:"queries"`
				URL     string   `json:"url"`
			}
			_ = json.Unmarshal(ci.Action, &a)
			it.Text = firstOf(ci.Query, firstOf(strings.Join(a.Queries, "\n"), a.URL))
		}
	case "ImageView":
		it.Kind, it.Text = "other", "Viewed image "+ci.Path
	case "SubAgentActivity":
		it.Kind, it.Text = "other", strings.TrimSpace("Subagent "+ci.Kind+" "+ci.AgentPath)
	case "Plan", "TodoList":
		it.Kind, it.Text = "plan", firstOf(ci.Text, codexFlattenContent(rawToAny(ci.Content)))
	default:
		it.Kind, it.Text = "other", ci.Type
	}
	p.add(off, 0, it)
}

// ── output ──

// collect appends items and runs: changed at or after `since` (since>=0), or
// every turn starting at or after `from` (from>=0), or all (both <0).
func (p *wParser) collect(out *WatchedItems, since, from int64) {
	for _, t := range p.turns {
		if from >= 0 && t.off < from {
			continue
		}
		if since < 0 || t.runMod >= since {
			r := *t.run
			if r.Usage != nil {
				u := *r.Usage
				r.Usage = &u
			}
			out.Runs = append(out.Runs, &r)
		}
		for _, wi := range t.items {
			if since < 0 || wi.mod >= since {
				it := *wi.it
				out.Items = append(out.Items, &it)
			}
		}
	}
}

// trim drops the oldest materialised turns of a long-lived tail.
func (p *wParser) trim() {
	if len(p.turns) <= watchedTailMax {
		return
	}
	drop := len(p.turns) - watchedPageTurns*2
	gone := map[*wItem]bool{}
	for _, t := range p.turns[:drop] {
		for _, wi := range t.items {
			gone[wi] = true
		}
	}
	for k, wi := range p.pending {
		if gone[wi] {
			delete(p.pending, k)
		}
	}
	for k, wi := range p.bgByID {
		if gone[wi] {
			delete(p.bgByID, k)
		}
	}
	bgs := p.bgs[:0]
	for _, wi := range p.bgs {
		if !gone[wi] {
			bgs = append(bgs, wi)
		}
	}
	p.bgs = bgs
	p.turns = append([]*wTurn(nil), p.turns[drop:]...)
	p.from = p.turns[0].off
}

// claudeBang turns <bash-input>/<bash-stdout>/<bash-stderr> user lines into a command item by the user.
func (p *wParser) claudeBang(off, ts int64, content json.RawMessage) bool {
	var text string
	if len(content) > 0 && content[0] == '"' {
		_ = json.Unmarshal(content, &text)
	} else {
		var parts []wClaudePart
		_ = json.Unmarshal(content, &parts)
		for _, pt := range parts {
			if pt.Type == "text" {
				text += pt.Text
			}
		}
	}
	cmd, in, out, hasOut := bangParts(text)
	if !in && !hasOut {
		return false
	}
	if in {
		if p.cur == nil {
			return true
		}
		p.touch(off, ts)
		p.bang = p.add(off, 0, &OItem{Kind: "command", By: "user", Status: "started", Text: "Shell command", Command: cmd, StartedAt: ts})
	}
	if wi := p.bang; wi != nil && hasOut {
		wi.mod = off
		p.bang = nil
		if m := reBgMoved.FindStringSubmatch(out); m != nil {
			wi.it.Bg, wi.bg = true, &wBg{id: m[1], path: m[2]}
			p.bgs = append(p.bgs, wi)
			p.bgByID[m[1]] = wi
		} else {
			p.complete(off, wi, "completed", ts, out, nil)
		}
	}
	if t := p.cur; hasOut && t != nil && t.bang && t.run.Status == "running" {
		p.endRun(off, ts, "completed") // reopened if the agent answers
	}
	return true
}

// wakeupLabel is the one line a turn the agent started by itself shows: its kind and the prompt's first line.
func wakeupLabel(source, prompt string) string {
	kind := "Scheduled task"
	if !strings.Contains(source, "schedule") {
		kind = "Wakeup"
	}
	line, _, _ := strings.Cut(strings.TrimSpace(prompt), "\n")
	if r := []rune(line); len(r) > 120 {
		line = string(r[:120]) + "…"
	}
	if line == "" {
		return kind
	}
	return kind + ": " + line
}
