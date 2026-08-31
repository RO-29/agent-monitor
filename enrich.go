package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ── settings ──────────────────────────────────────────────────────────────

type Settings struct {
	EnrichEnabled bool    `json:"enrichEnabled"` // run the LLM chapter pass automatically
	EnrichModel   string  `json:"enrichModel"`   // claude -p --model
	DailyCapUSD   float64 `json:"dailyCapUsd"`   // stop auto-enrichment past this estimate
	SpentTodayUSD float64 `json:"spentTodayUsd"`
	SpentDay      string  `json:"spentDay"`
}

var settings = struct {
	mu sync.Mutex
	v  Settings
}{v: Settings{EnrichModel: "haiku", DailyCapUSD: 2.0}}

func settingsPath() string { return filepath.Join(stateDir(), "settings.json") }

func loadSettings() {
	settings.mu.Lock()
	defer settings.mu.Unlock()
	if raw, err := os.ReadFile(settingsPath()); err == nil {
		var s Settings
		if json.Unmarshal(raw, &s) == nil {
			if s.EnrichModel == "" {
				s.EnrichModel = "haiku"
			}
			if s.DailyCapUSD == 0 {
				s.DailyCapUSD = 2.0
			}
			settings.v = s
		}
	}
	if os.Getenv("AGENT_MONITOR_ENRICH") == "1" {
		settings.v.EnrichEnabled = true
	}
}

func getSettings() Settings {
	settings.mu.Lock()
	defer settings.mu.Unlock()
	return settings.v
}

func saveSettings(s Settings) {
	settings.mu.Lock()
	settings.v = s
	raw, _ := json.MarshalIndent(s, "", "  ")
	settings.mu.Unlock()
	_ = os.MkdirAll(filepath.Dir(settingsPath()), 0o755)
	_ = os.WriteFile(settingsPath(), raw, 0o600)
}

func addSpent(usd float64) {
	settings.mu.Lock()
	day := time.Now().Format("2006-01-02")
	if settings.v.SpentDay != day {
		settings.v.SpentDay = day
		settings.v.SpentTodayUSD = 0
	}
	settings.v.SpentTodayUSD += usd
	raw, _ := json.MarshalIndent(settings.v, "", "  ")
	settings.mu.Unlock()
	_ = os.WriteFile(settingsPath(), raw, 0o600)
}

// ── summary cache (SQLite-backed) ─────────────────────────────────────────

var summaryCache = struct {
	mu sync.Mutex
	m  map[string]*summaryEntry
	db *SessionDB
}{m: map[string]*summaryEntry{}}

type summaryEntry struct {
	size, mtime int64
	sum         *TraceSummary
}

func initTraceTables(db *SessionDB) {
	summaryCache.mu.Lock()
	summaryCache.db = db
	summaryCache.mu.Unlock()
	if db == nil {
		return
	}
	_, _ = db.db.Exec(`
		CREATE TABLE IF NOT EXISTS trace_summaries (
			id TEXT PRIMARY KEY, size INTEGER, mtime INTEGER, data TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS chapters (
			segment_id TEXT PRIMARY KEY, data TEXT NOT NULL, enriched_at INTEGER, model TEXT
		);`)
	rows, err := db.db.Query(`SELECT id, size, mtime, data FROM trace_summaries`)
	if err != nil {
		return
	}
	defer rows.Close()
	n := 0
	summaryCache.mu.Lock()
	for rows.Next() {
		var id, data string
		var size, mtime int64
		if rows.Scan(&id, &size, &mtime, &data) != nil {
			continue
		}
		var s TraceSummary
		if json.Unmarshal([]byte(data), &s) == nil {
			summaryCache.m[id] = &summaryEntry{size: size, mtime: mtime, sum: &s}
			n++
		}
	}
	summaryCache.mu.Unlock()
	log.Printf("trace: restored %d cached summaries", n)
}

// cachedSummary returns the stored summary when the transcript is unchanged.
// A stale or missing summary is recomputed in the background; the caller gets
// the stale value (or nil) immediately so list views never block.
func cachedSummary(sess *Session) *TraceSummary {
	if sess == nil || sess.TranscriptPath == "" {
		return nil
	}
	st, err := os.Stat(sess.TranscriptPath)
	if err != nil {
		return nil
	}
	summaryCache.mu.Lock()
	e := summaryCache.m[sess.ID]
	summaryCache.mu.Unlock()
	if e != nil && e.size == st.Size() && e.mtime == st.ModTime().UnixMilli() {
		return e.sum
	}
	scheduleSummary(sess)
	if e != nil {
		return e.sum
	}
	return nil
}

var summaryQueue = struct {
	mu      sync.Mutex
	pending map[string]bool
	ch      chan *Session
}{pending: map[string]bool{}, ch: make(chan *Session, 4096)}

func scheduleSummary(sess *Session) {
	summaryQueue.mu.Lock()
	if summaryQueue.pending[sess.ID] {
		summaryQueue.mu.Unlock()
		return
	}
	summaryQueue.pending[sess.ID] = true
	summaryQueue.mu.Unlock()
	select {
	case summaryQueue.ch <- sess:
	default:
		summaryQueue.mu.Lock()
		delete(summaryQueue.pending, sess.ID)
		summaryQueue.mu.Unlock()
	}
}

// startSummaryWorkers computes trace summaries off the request path (two
// workers so a 50 MB transcript never starves the rest).
func startSummaryWorkers(s *Store) {
	for i := 0; i < 2; i++ {
		go func() {
			for sess := range summaryQueue.ch {
				refreshSummary(sess)
				summaryQueue.mu.Lock()
				delete(summaryQueue.pending, sess.ID)
				summaryQueue.mu.Unlock()
			}
		}()
	}
	// Warm the cache for every known session shortly after startup.
	go func() {
		time.Sleep(3 * time.Second)
		for _, sess := range s.All() {
			cachedSummary(sess)
		}
	}()
}

func refreshSummary(sess *Session) {
	st, err := os.Stat(sess.TranscriptPath)
	if err != nil {
		return
	}
	tr := buildTrace(sess)
	if tr == nil {
		return
	}
	sum := summarizeTrace(tr)
	summaryCache.mu.Lock()
	prev := summaryCache.m[sess.ID]
	summaryCache.m[sess.ID] = &summaryEntry{size: st.Size(), mtime: st.ModTime().UnixMilli(), sum: sum}
	db := summaryCache.db
	summaryCache.mu.Unlock()
	if db != nil {
		raw, _ := json.Marshal(sum)
		_, _ = db.db.Exec(`INSERT INTO trace_summaries(id,size,mtime,data) VALUES(?,?,?,?)
			ON CONFLICT(id) DO UPDATE SET size=excluded.size, mtime=excluded.mtime, data=excluded.data`,
			sess.ID, st.Size(), st.ModTime().UnixMilli(), string(raw))
	}
	// A new boundary appeared → tell clients, and enrich the closed segment.
	if prev != nil && prev.sum != nil && sum.Segments > prev.sum.Segments {
		traceBus.publish(TraceEvent{Kind: "segment", SessionID: sess.ID, Segments: sum.Segments})
		if getSettings().EnrichEnabled && len(tr.Segments) >= 2 {
			go enrichSegment(sess, tr, len(tr.Segments)-2, false)
		}
	}
	if prev == nil {
		traceBus.publish(TraceEvent{Kind: "segment", SessionID: sess.ID, Segments: sum.Segments})
	}
}

// ── trace event bus (WS) ──────────────────────────────────────────────────

type TraceEvent struct {
	Kind      string `json:"kind"` // segment | chapter | context
	SessionID string `json:"sessionId"`
	Segments  int    `json:"segments,omitempty"`
	Segment   int    `json:"segment,omitempty"`
}

type traceBusT struct {
	mu        sync.Mutex
	listeners map[int]func(TraceEvent)
	next      int
}

var traceBus = &traceBusT{listeners: map[int]func(TraceEvent){}}

func (b *traceBusT) subscribe(l func(TraceEvent)) func() {
	b.mu.Lock()
	id := b.next
	b.next++
	b.listeners[id] = l
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		delete(b.listeners, id)
		b.mu.Unlock()
	}
}

func (b *traceBusT) publish(e TraceEvent) {
	b.mu.Lock()
	ls := make([]func(TraceEvent), 0, len(b.listeners))
	for _, l := range b.listeners {
		ls = append(ls, l)
	}
	b.mu.Unlock()
	for _, l := range ls {
		func() { defer func() { _ = recover() }(); l(e) }()
	}
}

// ── enrichment (claude -p, Haiku) ─────────────────────────────────────────

var enrichRunning = struct {
	mu sync.Mutex
	m  map[string]bool
}{m: map[string]bool{}}

func chapterKey(sessionID string, seg int) string { return fmt.Sprintf("%s#%d", sessionID, seg) }

// loadEnrichedChapter returns the stored LLM chapter for a segment, if any.
func loadEnrichedChapter(sessionID string, seg int) *Chapter {
	summaryCache.mu.Lock()
	db := summaryCache.db
	summaryCache.mu.Unlock()
	if db == nil {
		return nil
	}
	var data string
	if err := db.db.QueryRow(`SELECT data FROM chapters WHERE segment_id=?`, chapterKey(sessionID, seg)).Scan(&data); err != nil {
		return nil
	}
	var c Chapter
	if json.Unmarshal([]byte(data), &c) != nil {
		return nil
	}
	return &c
}

// enrichSegment asks a small model for the chapter card and stores it. force
// re-runs even when a card exists. Returns the chapter or an error string.
func enrichSegment(sess *Session, tr *SessionTrace, seg int, force bool) (*Chapter, string) {
	if tr == nil || seg < 0 || seg >= len(tr.Segments) {
		return nil, "segment out of range"
	}
	key := chapterKey(sess.ID, seg)
	enrichRunning.mu.Lock()
	if enrichRunning.m[key] {
		enrichRunning.mu.Unlock()
		return nil, "already running"
	}
	enrichRunning.m[key] = true
	enrichRunning.mu.Unlock()
	defer func() {
		enrichRunning.mu.Lock()
		delete(enrichRunning.m, key)
		enrichRunning.mu.Unlock()
	}()
	if !force {
		if c := loadEnrichedChapter(sess.ID, seg); c != nil {
			return c, ""
		}
	}
	st := getSettings()
	if !force && st.SpentDay == time.Now().Format("2006-01-02") && st.SpentTodayUSD >= st.DailyCapUSD {
		return nil, "daily cap reached"
	}
	if _, err := exec.LookPath("claude"); err != nil {
		return nil, "claude CLI not on PATH"
	}

	segment := tr.Segments[seg]
	det := segment.Chapter
	var b strings.Builder
	fmt.Fprintf(&b, "You summarise one segment of a coding-agent session for an observability dashboard.\n")
	fmt.Fprintf(&b, "Return ONLY a JSON object with keys: point (string, <=60 words: what the user wanted in this segment), intentChanges (array of {ts:number,text:string} — moments the goal changed, reuse the given ts), outcome (string, <=60 words: what was achieved), learnings (array of {source:\"correction\"|\"summary\",text:string,evidence:string} — facts that changed the plan, user corrections, rules stated by the user), open (array of strings — unfinished items).\n")
	fmt.Fprintf(&b, "Write in plain, short sentences. Do not invent facts. Do not include code.\n\n")
	fmt.Fprintf(&b, "SEGMENT %d of %d. Boundary: %s", seg+1, len(tr.Segments), segment.Boundary.Kind)
	if segment.Boundary.Summary != "" {
		fmt.Fprintf(&b, "\nCarried summary (from the agent's own compaction):\n%s\n", clipString(segment.Boundary.Summary, 6000))
	}
	if det != nil {
		fmt.Fprintf(&b, "\nDeterministic draft: %s\n", mustJSON(det))
	}
	fmt.Fprintf(&b, "\nTranscript of this segment (user prompts, assistant prose heads, tool names):\n")
	budget := 28000
	for _, sp := range tr.Spans {
		if sp.Seg != seg || budget <= 0 {
			continue
		}
		var line string
		switch sp.Kind {
		case "user":
			line = fmt.Sprintf("[%d] USER: %s\n", sp.Ts, sp.Text)
		case "turn":
			line = fmt.Sprintf("[%d] ASSISTANT (%s): %s\n", sp.Ts, fmtDurShort(sp.Dur), sp.Text)
		case "tool", "agent":
			e := ""
			if sp.Err {
				e = " ERROR"
			}
			line = fmt.Sprintf("  - %s %s%s\n", sp.Name, clipString(sp.Res, 100), e)
		}
		budget -= len(line)
		b.WriteString(line)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "claude", "-p", "--model", st.EnrichModel, "--output-format", "json", "--max-turns", "1")
	cmd.Stdin = strings.NewReader(b.String())
	// Strip the nested-session marker so `claude -p` runs even when the daemon
	// was started from inside a Claude Code shell; skip hooks for speed.
	procEnv := []string{"CLAUDE_CODE_DISABLE_HOOKS=1"}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "CLAUDECODE=") || strings.HasPrefix(kv, "CLAUDE_CODE_ENTRYPOINT=") {
			continue
		}
		procEnv = append(procEnv, kv)
	}
	cmd.Env = procEnv
	cmd.Dir = homeDir()
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Sprintf("claude -p failed: %v: %s", err, clipString(errb.String(), 400))
	}
	var env struct {
		Result       string  `json:"result"`
		TotalCostUSD float64 `json:"total_cost_usd"`
	}
	if json.Unmarshal(out.Bytes(), &env) != nil || env.Result == "" {
		return nil, "unexpected claude output: " + clipString(out.String(), 400)
	}
	addSpent(env.TotalCostUSD)
	txt := env.Result
	if m := regexp.MustCompile(`(?s)\{.*\}`).FindString(txt); m != "" {
		txt = m
	}
	var c Chapter
	if err := json.Unmarshal([]byte(txt), &c); err != nil {
		return nil, "model did not return JSON: " + clipString(txt, 300)
	}
	c.Source = "enriched"
	c.EnrichedAt = time.Now().UnixMilli()
	c.Model = st.EnrichModel
	if c.IntentChanges == nil {
		c.IntentChanges = []IntentChange{}
	}
	if c.Open == nil {
		c.Open = []string{}
	}
	// keep deterministic facts the model cannot know: memory writes, outputs
	if det != nil {
		c.Outputs = det.Outputs
		for _, l := range det.Learnings {
			if l.Source == "memory" || l.Source == "output" {
				c.Learnings = append(c.Learnings, l)
			}
		}
	}
	for i := range c.Learnings {
		if c.Learnings[i].ID == "" {
			c.Learnings[i].ID = spanID("lrn", c.Learnings[i].Text)
			c.Learnings[i].Ts = segment.FromTs
			c.Learnings[i].Seg = seg
		}
	}
	if c.Learnings == nil {
		c.Learnings = []Learning{}
	}
	summaryCache.mu.Lock()
	db := summaryCache.db
	summaryCache.mu.Unlock()
	if db != nil {
		raw, _ := json.Marshal(c)
		_, _ = db.db.Exec(`INSERT INTO chapters(segment_id,data,enriched_at,model) VALUES(?,?,?,?)
			ON CONFLICT(segment_id) DO UPDATE SET data=excluded.data, enriched_at=excluded.enriched_at, model=excluded.model`,
			key, string(raw), c.EnrichedAt, c.Model)
	}
	traceBus.publish(TraceEvent{Kind: "chapter", SessionID: sess.ID, Segment: seg})
	return &c, ""
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func fmtDurShort(ms int64) string {
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	if ms < 60_000 {
		return fmt.Sprintf("%ds", ms/1000)
	}
	return fmt.Sprintf("%dm%02ds", ms/60_000, (ms%60_000)/1000)
}

// ── promote a learning to a memory file ───────────────────────────────────

// promoteLearning writes a memory file into the project's memory directory
// (the same layout Claude Code's auto-memory uses) and appends an index line.
func promoteLearning(sess *Session, text, kind string) (string, error) {
	if kind == "" {
		kind = "project"
	}
	cwd := sess.Cwd
	if cwd == "" {
		cwd = homeDir()
	}
	enc := "-" + strings.ReplaceAll(strings.TrimPrefix(cwd, "/"), "/", "-")
	dir := filepath.Join(homeDir(), ".claude", "projects", enc, "memory")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	slug := regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(strings.ToLower(clipString(text, 48)), "-")
	slug = strings.Trim(slug, "-")
	if slug == "" {
		slug = fmt.Sprintf("learning-%d", time.Now().Unix())
	}
	path := filepath.Join(dir, slug+".md")
	body := fmt.Sprintf("---\nname: %s\ndescription: %s\nmetadata:\n  type: %s\n---\n\n%s\n\n**Why:** promoted from agent-monitor session %s on %s.\n**How to apply:** treat as a standing rule for this project.\n",
		slug, strings.ReplaceAll(clipString(text, 200), "\n", " "), kind, text, sess.SessionID[:min(8, len(sess.SessionID))], time.Now().Format("2006-01-02"))
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return "", err
	}
	idx := filepath.Join(dir, "MEMORY.md")
	line := fmt.Sprintf("- [%s](%s.md) — %s\n", clipString(text, 60), slug, clipString(text, 120))
	f, err := os.OpenFile(idx, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err == nil {
		_, _ = f.WriteString(line)
		f.Close()
	}
	return path, nil
}
