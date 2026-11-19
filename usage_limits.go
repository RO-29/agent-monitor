package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"hash/fnv"
	"io"
	"io/fs"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// GET /api/usage/limits: plan usage windows per agent plus token totals measured from local transcripts.

const (
	usageCacheTTL   = 30 * time.Second
	usageWeek       = 7 * 24 * time.Hour
	usageFiveHour   = 5 * time.Hour
	usageHistoryMax = 400
)

type usagePoint struct {
	At  int64   `json:"at"`
	Pct float64 `json:"pct"`
}

type usageWindow struct {
	Key           string       `json:"key"` // "5h" or "weekly"
	Label         string       `json:"label"`
	WindowMinutes int          `json:"windowMinutes"`
	UsedPercent   *float64     `json:"usedPercent"`
	LastPercent   *float64     `json:"lastPercent,omitempty"` // set when the reading is from a window that has since reset
	ResetsAt      int64        `json:"resetsAt,omitempty"`    // ms
	Expired       bool         `json:"expired,omitempty"`
	History       []usagePoint `json:"history"`
}

type usageLocal struct {
	Key           string     `json:"key"`
	Label         string     `json:"label"`
	From          int64      `json:"from"`    // ms
	Aligned       bool       `json:"aligned"` // From is the plan window start, not now minus the window
	Tokens        TokenUsage `json:"tokens"`
	CostUSD       float64    `json:"costUsd"`
	CostEstimated bool       `json:"costEstimated"`
	Messages      int        `json:"messages"`
}

type agentUsage struct {
	Agent       string        `json:"agent"`
	Installed   bool          `json:"installed"`
	Source      string        `json:"source"`
	SourcePath  string        `json:"sourcePath,omitempty"`
	MeasuredAt  int64         `json:"measuredAt,omitempty"` // ms of the plan reading
	Plan        string        `json:"plan,omitempty"`
	Note        string        `json:"note,omitempty"`
	Windows     []usageWindow `json:"windows"`
	Local       []usageLocal  `json:"local"`
	LocalSource string        `json:"localSource"`
	Models      []usageModel  `json:"models"`
	Credits     *usageCredits `json:"credits,omitempty"`
}

// usageModel is one model's local totals in one window ("5h" or "weekly").
type usageModel struct {
	Window        string  `json:"window"`
	Model         string  `json:"model"`
	InTokens      int64   `json:"inTokens"`
	OutTokens     int64   `json:"outTokens"`
	CacheRead     int64   `json:"cacheRead"`
	CacheWrite    int64   `json:"cacheWrite"`
	CostUSD       float64 `json:"costUsd"`
	CostEstimated bool    `json:"costEstimated"`
	Messages      int     `json:"messages"`
}

// usageCredits is what an agent records locally about credits or spend; fields it does not record are omitted.
type usageCredits struct {
	Source     string          `json:"source"`
	MeasuredAt int64           `json:"measuredAt,omitempty"`
	HasCredits *bool           `json:"hasCredits,omitempty"`
	Unlimited  *bool           `json:"unlimited,omitempty"`
	Balance    json.RawMessage `json:"balance,omitempty"` // number or string, as recorded
	SpendUSD   *float64        `json:"spendUsd,omitempty"`
}

type usageLimitsResp struct {
	GeneratedAt int64        `json:"generatedAt"`
	Agents      []agentUsage `json:"agents"`
}

// usageBucketMs is the granularity of local token totals: a window edge can be off by up to this much.
const usageBucketMs = 60_000

// usageBucket sums the billed replies of one model in one minute of one transcript.
type usageBucket struct {
	min   int64 // ms / usageBucketMs
	model string
	tok   TokenUsage
	cost  float64
	n     int
}

// codexReading is one rate_limits snapshot from a Codex token_count event.
type codexReading struct {
	At        int64
	Plan      string
	Primary   *codexWin
	Secondary *codexWin
}

type codexWin struct {
	UsedPercent     *float64 `json:"used_percent"`
	WindowMinutes   int      `json:"window_minutes"`
	ResetsInSeconds *int64   `json:"resets_in_seconds"`
	ResetsAt        *int64   `json:"resets_at"`
}

// codexLatest is the newest reading of one window key.
type codexLatest struct {
	w  *codexWin
	at int64
}

type usageFile struct {
	off       int64
	mod       time.Time
	buckets   []usageBucket // sorted by min
	prevTotal int64
	model     string
	plan      string // codex: newest plan_type and its time
	planAt    int64
	latest    [2]codexLatest // codex: newest reading per window, 5h then weekly
	credits   *codexCredits
	creditsAt int64
}

// codexCredits is the rate_limits.credits object of a Codex token_count event.
type codexCredits struct {
	HasCredits *bool           `json:"has_credits"`
	Unlimited  *bool           `json:"unlimited"`
	Balance    json.RawMessage `json:"balance"`
}

// usageSeen is where a Claude message (by id and request id) was counted, so a repeat counts once.
type usageSeen struct {
	f   *usageFile
	min int64
	out int64
}

type usageScanner struct {
	root    string
	codex   bool
	files   map[string]*usageFile
	seen    map[uint64]usageSeen
	pending []codexReading // codex readings read since the last compute, not yet in the history
	nowFn   func() time.Time
}

func newUsageScanner(root string, codex bool, now func() time.Time) *usageScanner {
	return &usageScanner{root: root, codex: codex, files: map[string]*usageFile{}, seen: map[uint64]usageSeen{}, nowFn: now}
}

// scan reads only the bytes appended since the last scan of each recent .jsonl file.
func (s *usageScanner) scan() {
	cut := s.nowFn().Add(-usageWeek - time.Hour)
	keep := cut.UnixMilli()
	keepMin := keep / usageBucketMs
	seen := map[string]bool{}
	rebuild := false
	_ = filepath.WalkDir(s.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".jsonl") {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.ModTime().Before(cut) {
			return nil
		}
		seen[p] = true
		f := s.files[p]
		if f == nil {
			f = &usageFile{}
			s.files[p] = f
		}
		if info.Size() < f.off {
			rebuild = s.forget(f, keepMin) || rebuild
			*f = usageFile{}
		}
		if info.Size() == f.off && info.ModTime().Equal(f.mod) {
			return nil
		}
		f.mod = info.ModTime()
		s.readFile(p, f)
		return nil
	})
	for p, f := range s.files {
		if !seen[p] {
			rebuild = s.forget(f, keepMin) || rebuild
			delete(s.files, p)
		}
	}
	if rebuild {
		// A message counted in a file that is gone may still be in another file: count again from scratch.
		s.files, s.seen = map[string]*usageFile{}, map[uint64]usageSeen{}
		s.scan()
		return
	}
	for _, f := range s.files {
		if len(f.buckets) > 0 && (f.buckets[0].min+1)*usageBucketMs <= keep {
			i := sort.Search(len(f.buckets), func(i int) bool { return (f.buckets[i].min+1)*usageBucketMs > keep })
			f.buckets = append([]usageBucket(nil), f.buckets[i:]...)
		}
		if f.planAt < keep {
			f.plan, f.planAt = "", 0
		}
		if f.creditsAt < keep {
			f.credits, f.creditsAt = nil, 0
		}
		for i := range f.latest {
			if f.latest[i].at < keep {
				f.latest[i] = codexLatest{}
			}
		}
	}
	for h, v := range s.seen {
		if v.min < keepMin {
			delete(s.seen, h)
		}
	}
	if len(s.pending) > 0 {
		out := s.pending[:0]
		for _, rd := range s.pending {
			if rd.At >= keep {
				out = append(out, rd)
			}
		}
		s.pending = out
	}
}

// forget drops the dedup marks that point into f and reports whether any is still inside the window.
func (s *usageScanner) forget(f *usageFile, keepMin int64) bool {
	had := false
	for h, v := range s.seen {
		if v.f == f {
			delete(s.seen, h)
			had = had || v.min >= keepMin
		}
	}
	return had
}

func (f *usageFile) bucket(min int64, model string) *usageBucket {
	for i := len(f.buckets) - 1; i >= 0 && f.buckets[i].min >= min; i-- {
		if f.buckets[i].min == min && f.buckets[i].model == model {
			return &f.buckets[i]
		}
	}
	i := sort.Search(len(f.buckets), func(i int) bool { return f.buckets[i].min > min })
	f.buckets = append(f.buckets, usageBucket{})
	copy(f.buckets[i+1:], f.buckets[i:])
	f.buckets[i] = usageBucket{min: min, model: model}
	return &f.buckets[i]
}

func (f *usageFile) add(at int64, model string, tok TokenUsage, cost float64, n int) {
	b := f.bucket(at/usageBucketMs, model)
	b.tok = addTok(b.tok, tok)
	b.cost += cost
	b.n += n
}

func (f *usageFile) sub(min int64, model string, tok TokenUsage, cost float64) {
	for i := range f.buckets {
		if b := &f.buckets[i]; b.min == min && b.model == model {
			b.tok = addTok(b.tok, TokenUsage{Input: -tok.Input, Output: -tok.Output, CacheRead: -tok.CacheRead, CacheCreate: -tok.CacheCreate})
			b.cost -= cost
			if b.n--; b.n <= 0 {
				f.buckets = append(f.buckets[:i], f.buckets[i+1:]...)
			}
			return
		}
	}
}

func (s *usageScanner) readFile(p string, f *usageFile) {
	fh, err := os.Open(p)
	if err != nil {
		return
	}
	defer fh.Close()
	if _, err := fh.Seek(f.off, io.SeekStart); err != nil {
		return
	}
	r := bufio.NewReaderSize(fh, 256<<10)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return // a partial last line is read again next scan
		}
		f.off += int64(len(line))
		if s.codex {
			s.codexLine(f, line)
		} else {
			s.claudeLine(f, line)
		}
	}
}

var (
	bAssistant  = []byte(`"assistant"`)
	bUsage      = []byte(`"usage"`)
	bTokenCount = []byte(`"token_count"`)
	bTurnCtx    = []byte(`"turn_context"`)
)

func (s *usageScanner) claudeLine(f *usageFile, line []byte) {
	if !bytes.Contains(line, bUsage) || !bytes.Contains(line, bAssistant) {
		return
	}
	var rec struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		RequestID string `json:"requestId"`
		Message   struct {
			ID    string `json:"id"`
			Model string `json:"model"`
			Usage *struct {
				In  int64 `json:"input_tokens"`
				Out int64 `json:"output_tokens"`
				CR  int64 `json:"cache_read_input_tokens"`
				CC  int64 `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &rec) != nil || rec.Type != "assistant" || rec.Message.Usage == nil || rec.Message.Model == "<synthetic>" {
		return
	}
	ts, err := time.Parse(time.RFC3339Nano, rec.Timestamp)
	if err != nil {
		return
	}
	u := rec.Message.Usage
	tok := TokenUsage{Input: u.In, Output: u.Out, CacheRead: u.CR, CacheCreate: u.CC}
	at := ts.UnixMilli()
	model := rec.Message.Model
	if rec.Message.ID == "" {
		f.add(at, model, tok, estimateCost(model, tok), 1)
		return
	}
	h := fnv.New64a()
	h.Write([]byte(rec.Message.ID))
	h.Write([]byte{':'})
	h.Write([]byte(rec.RequestID))
	key := h.Sum64()
	min := at / usageBucketMs
	was, ok := s.seen[key]
	if !ok {
		f.add(at, model, tok, estimateCost(model, tok), 1)
		s.seen[key] = usageSeen{f: f, min: min, out: tok.Output}
		return
	}
	// A repeat counts once: in the latest minute it appears, with its largest output.
	if min <= was.min && tok.Output <= was.out {
		return
	}
	old := tok
	old.Output = was.out
	was.f.sub(was.min, model, old, estimateCost(model, old))
	now := usageSeen{f: was.f, min: was.min, out: max(was.out, tok.Output)}
	if min > was.min {
		now.f, now.min = f, min
	}
	tok.Output = now.out
	now.f.add(now.min*usageBucketMs, model, tok, estimateCost(model, tok), 1)
	s.seen[key] = now
}

func (s *usageScanner) codexLine(f *usageFile, line []byte) {
	if bytes.Contains(line, bTurnCtx) {
		var tc struct {
			Type    string `json:"type"`
			Payload struct {
				Model string `json:"model"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &tc) == nil && tc.Type == "turn_context" && tc.Payload.Model != "" {
			f.model = tc.Payload.Model
		}
		return
	}
	if !bytes.Contains(line, bTokenCount) {
		return
	}
	var rec struct {
		Timestamp string `json:"timestamp"`
		Payload   struct {
			Type string `json:"type"`
			Info *struct {
				Total *struct {
					In     int64 `json:"input_tokens"`
					Cached int64 `json:"cached_input_tokens"`
					Out    int64 `json:"output_tokens"`
					Total  int64 `json:"total_tokens"`
				} `json:"total_token_usage"`
				Last *struct {
					In     int64 `json:"input_tokens"`
					Cached int64 `json:"cached_input_tokens"`
					Out    int64 `json:"output_tokens"`
				} `json:"last_token_usage"`
			} `json:"info"`
			RateLimits *struct {
				Primary   *codexWin     `json:"primary"`
				Secondary *codexWin     `json:"secondary"`
				PlanType  string        `json:"plan_type"`
				Credits   *codexCredits `json:"credits"`
			} `json:"rate_limits"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &rec) != nil || rec.Payload.Type != "token_count" {
		return
	}
	ts, err := time.Parse(time.RFC3339Nano, rec.Timestamp)
	if err != nil {
		return
	}
	at := ts.UnixMilli()
	if rl := rec.Payload.RateLimits; rl != nil {
		rd := codexReading{At: at, Plan: rl.PlanType}
		if rl.Primary != nil && rl.Primary.UsedPercent != nil {
			rd.Primary = rl.Primary
		}
		if rl.Secondary != nil && rl.Secondary.UsedPercent != nil {
			rd.Secondary = rl.Secondary
		}
		if rd.Primary != nil || rd.Secondary != nil || rd.Plan != "" {
			s.note(f, rd)
		}
		if rl.Credits != nil && at >= f.creditsAt {
			f.credits, f.creditsAt = rl.Credits, at
		}
	}
	// Totals repeat when Codex re-emits an event, so count only the growth of total_token_usage.
	if info := rec.Payload.Info; info != nil && info.Total != nil {
		t := info.Total
		if t.Total <= f.prevTotal && f.prevTotal > 0 {
			return
		}
		var in, cached, out int64
		if f.prevTotal > 0 && info.Last != nil {
			in, cached, out = info.Last.In, info.Last.Cached, info.Last.Out
		} else {
			in, cached, out = t.In, t.Cached, t.Out
		}
		f.prevTotal = t.Total
		tok := TokenUsage{Input: max(0, in-cached), Output: out, CacheRead: cached}
		model, priced := f.model, f.model
		if model == "" {
			model, priced = "unknown", "gpt"
		}
		f.add(at, model, tok, estimateCost(priced, tok), 1)
	}
}

// note keeps the newest plan and window readings of f, and queues rd for the history.
func (s *usageScanner) note(f *usageFile, rd codexReading) {
	s.pending = append(s.pending, rd)
	if rd.Plan != "" && rd.At >= f.planAt {
		f.plan, f.planAt = rd.Plan, rd.At
	}
	for i, w := range []*codexWin{rd.Primary, rd.Secondary} {
		if w == nil {
			continue
		}
		k := 0
		if codexWinKey(w, i == 0) == "weekly" {
			k = 1
		}
		if rd.At >= f.latest[k].at {
			f.latest[k] = codexLatest{w, rd.At}
		}
	}
}

// totals sums the buckets that overlap [from, ...), counting each Claude message once.
func (s *usageScanner) totals(from int64) (TokenUsage, float64, int) {
	var tok TokenUsage
	var cost float64
	n := 0
	for _, f := range s.files {
		i := sort.Search(len(f.buckets), func(i int) bool { return (f.buckets[i].min+1)*usageBucketMs > from })
		for _, b := range f.buckets[i:] {
			tok = addTok(tok, b.tok)
			cost += b.cost
			n += b.n
		}
	}
	return tok, cost, n
}

// modelTotals sums the buckets that overlap [from, ...) per model.
func (s *usageScanner) modelTotals(from int64) map[string]*usageModel {
	out := map[string]*usageModel{}
	for _, f := range s.files {
		i := sort.Search(len(f.buckets), func(i int) bool { return (f.buckets[i].min+1)*usageBucketMs > from })
		for _, b := range f.buckets[i:] {
			m := out[b.model]
			if m == nil {
				m = &usageModel{Model: b.model}
				out[b.model] = m
			}
			m.InTokens += b.tok.Input
			m.OutTokens += b.tok.Output
			m.CacheRead += b.tok.CacheRead
			m.CacheWrite += b.tok.CacheCreate
			m.CostUSD += b.cost
			m.Messages += b.n
		}
	}
	return out
}

func addTok(a, b TokenUsage) TokenUsage {
	a.Input += b.Input
	a.Output += b.Output
	a.CacheRead += b.CacheRead
	a.CacheCreate += b.CacheCreate
	return a
}

// takePending returns the readings queued since the last call, oldest first.
func (s *usageScanner) takePending() []codexReading {
	out := s.pending
	s.pending = nil
	sort.SliceStable(out, func(i, j int) bool { return out[i].At < out[j].At })
	return out
}

// usageHistory keeps plan percent readings per agent window, saved as a small JSON file.
type usageHistory struct {
	path   string
	Points map[string][]usagePoint `json:"points"`
	dirty  bool
}

func loadUsageHistory(path string) *usageHistory {
	h := &usageHistory{path: path, Points: map[string][]usagePoint{}}
	if path == "" {
		return h
	}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, h)
		if h.Points == nil {
			h.Points = map[string][]usagePoint{}
		}
	}
	return h
}

// add records a reading when it is newer and the percent moved or 15 minutes passed.
func (h *usageHistory) add(key string, at int64, pct float64, now time.Time) {
	pts := h.Points[key]
	if n := len(pts); n > 0 {
		last := pts[n-1]
		if at <= last.At || (last.Pct == pct && at-last.At < int64(15*time.Minute/time.Millisecond)) {
			return
		}
	}
	pts = append(pts, usagePoint{At: at, Pct: pct})
	keep := now.Add(-usageWeek).UnixMilli()
	i := sort.Search(len(pts), func(i int) bool { return pts[i].At >= keep })
	pts = pts[i:]
	if len(pts) > usageHistoryMax {
		pts = pts[len(pts)-usageHistoryMax:]
	}
	h.Points[key] = pts
	h.dirty = true
}

func (h *usageHistory) get(key string, since int64) []usagePoint {
	out := []usagePoint{}
	for _, p := range h.Points[key] {
		if p.At >= since {
			out = append(out, p)
		}
	}
	return out
}

func (h *usageHistory) save() {
	if !h.dirty || h.path == "" {
		return
	}
	if b, err := json.Marshal(h); err == nil {
		_ = os.MkdirAll(filepath.Dir(h.path), 0o755)
		tmp := h.path + ".tmp"
		if os.WriteFile(tmp, b, 0o644) == nil && os.Rename(tmp, h.path) == nil {
			h.dirty = false
		}
	}
}

type usageLimits struct {
	mu          sync.Mutex
	claudeState string // dietclaude's copy of the statusLine rate_limits
	claude      *usageScanner
	codex       *usageScanner
	opencode    *opencodeUsageScanner
	cursorDB    string
	claudeCfg   string // ~/.claude.json: only oauthAccount plan fields are decoded
	claudePlan  cachedPlan
	home        string
	binDirs     []string
	lookPath    func(string) (string, error)
	hist        *usageHistory
	now         func() time.Time
	cached      *usageLimitsResp
	cachedAt    time.Time
	peak        map[string]usagePeak // per window: highest reading seen for one reset time
	statePath   string               // saved scan positions, loaded by the first compute
	loaded      bool
	savedAt     time.Time
}

// usagePeak exists because every Claude session saves its own last reading; within one window usage only rises.
type usagePeak struct {
	resetsAt int64
	pct      float64
}

// monotonic returns the highest reading for this window and reset time; caller holds u.mu.
func (u *usageLimits) monotonic(key string, pct *float64, resetsAt int64) *float64 {
	if pct == nil || resetsAt == 0 {
		return pct
	}
	if u.peak == nil {
		u.peak = map[string]usagePeak{}
	}
	p, ok := u.peak[key]
	if ok && p.resetsAt == resetsAt && p.pct >= *pct {
		v := p.pct
		return &v
	}
	u.peak[key] = usagePeak{resetsAt: resetsAt, pct: *pct}
	return pct
}

func newUsageLimits(home, histPath string, now func() time.Time) *usageLimits {
	if now == nil {
		now = time.Now
	}
	return &usageLimits{
		claudeState: filepath.Join(home, ".dietclaude", "state.json"),
		claude:      newUsageScanner(filepath.Join(home, ".claude", "projects"), false, now),
		codex:       newUsageScanner(filepath.Join(home, ".codex", "sessions"), true, now),
		opencode:    newOpencodeUsageScanner(filepath.Join(home, ".local", "share", "opencode", "storage", "message"), now),
		cursorDB:    filepath.Join(home, ".cursor", "ai-tracking", "ai-code-tracking.db"),
		claudeCfg:   filepath.Join(home, ".claude.json"),
		home:        home,
		binDirs:     defaultBinDirs(home),
		lookPath:    exec.LookPath,
		hist:        loadUsageHistory(histPath),
		now:         now,
		statePath:   usageStatePath(histPath),
	}
}

func (u *usageLimits) get() *usageLimitsResp {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.cached != nil && u.now().Sub(u.cachedAt) < usageCacheTTL {
		return u.cached
	}
	u.cached = u.compute()
	u.cachedAt = u.now()
	return u.cached
}

func (u *usageLimits) compute() *usageLimitsResp {
	now := u.now()
	if !u.loaded {
		u.loadState()
		u.loaded = true
	}
	u.claude.scan()
	u.codex.scan()
	u.opencode.scan()
	out := &usageLimitsResp{GeneratedAt: now.UnixMilli(), Agents: []agentUsage{
		u.claudeUsage(now), u.codexUsage(now), u.opencodeUsage(now), u.cursorUsage(now), u.piUsage()}}
	u.hist.save()
	if now.Sub(u.savedAt) >= usageSaveEvery {
		u.saveState()
	}
	return out
}

func pctPtr(v float64) *float64 {
	v = math.Max(0, math.Min(100, v))
	return &v
}

// window builds one plan window and records its reading in the history.
func (u *usageLimits) window(agent, key string, minutes int, pct *float64, resetsAt, measuredAt int64, now time.Time) usageWindow {
	w := usageWindow{Key: key, Label: map[string]string{"5h": "5-hour", "weekly": "Weekly"}[key], WindowMinutes: minutes, ResetsAt: resetsAt}
	hk := agent + ":" + key
	if pct != nil {
		p := pctPtr(*pct)
		u.hist.add(hk, measuredAt, *p, now)
		if resetsAt > 0 && resetsAt <= now.UnixMilli() {
			w.Expired = true
			w.LastPercent = p
		} else {
			w.UsedPercent = p
		}
	}
	since := now.Add(-usageWeek).UnixMilli()
	if key == "5h" {
		since = now.Add(-24 * time.Hour).UnixMilli()
	}
	w.History = u.hist.get(hk, since)
	return w
}

// local returns the window totals and the per-model rows; totals(from) gives the models seen since from.
func (u *usageLimits) local(totals func(from int64) map[string]*usageModel, wins []usageWindow, now time.Time, estimated bool) ([]usageLocal, []usageModel) {
	var out []usageLocal
	models := []usageModel{}
	for _, spec := range []struct {
		key, label string
		d          time.Duration
	}{{"5h", "5 hours", usageFiveHour}, {"weekly", "7 days", usageWeek}} {
		from := now.Add(-spec.d).UnixMilli()
		aligned := false
		for _, w := range wins {
			if w.Key == spec.key && w.ResetsAt > now.UnixMilli() {
				if start := w.ResetsAt - int64(w.WindowMinutes)*60_000; w.WindowMinutes > 0 && start <= now.UnixMilli() {
					from, aligned = start, true
				}
			}
		}
		l := usageLocal{Key: spec.key, Label: spec.label, From: from, Aligned: aligned, CostEstimated: estimated}
		ms := totals(from)
		keys := make([]string, 0, len(ms))
		for k := range ms {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			m := ms[k]
			m.Window, m.CostEstimated = spec.key, estimated
			l.Tokens = addTok(l.Tokens, TokenUsage{Input: m.InTokens, Output: m.OutTokens, CacheRead: m.CacheRead, CacheCreate: m.CacheWrite})
			l.CostUSD += m.CostUSD
			l.Messages += m.Messages
			models = append(models, *m)
		}
		out = append(out, l)
	}
	return out, models
}

func (u *usageLimits) claudeUsage(now time.Time) agentUsage {
	a := agentUsage{Agent: "claude", Installed: u.installed("claude"), Source: "Claude Code statusLine (dietclaude copy)", SourcePath: u.claudeState,
		LocalSource: "Claude Code transcripts in ~/.claude/projects, cost at public list prices", Plan: u.claudePlanName(now)}
	var st struct {
		UpdatedAt  float64 `json:"updated_at"`
		RateLimits map[string]struct {
			UsedPercentage *float64 `json:"used_percentage"`
			ResetsAt       float64  `json:"resets_at"`
		} `json:"rate_limits"`
	}
	b, err := os.ReadFile(u.claudeState)
	if err == nil {
		err = json.Unmarshal(b, &st)
	}
	if err != nil {
		a.Note = "No plan reading. Claude Code sends rate_limits only to a statusLine command, and no saved copy was found."
	}
	measured := int64(st.UpdatedAt * 1000)
	a.MeasuredAt = measured
	for _, spec := range []struct {
		name, key string
		min       int
	}{{"five_hour", "5h", 300}, {"seven_day", "weekly", 10080}} {
		rl := st.RateLimits[spec.name]
		resets := int64(rl.ResetsAt * 1000)
		pct := u.monotonic("claude/"+spec.key, rl.UsedPercentage, resets)
		a.Windows = append(a.Windows, u.window("claude", spec.key, spec.min, pct, resets, measured, now))
	}
	a.Local, a.Models = u.local(u.claude.modelTotals, a.Windows, now, true)
	return a
}

func codexWinKey(w *codexWin, primary bool) string {
	switch {
	case w.WindowMinutes > 0 && w.WindowMinutes <= 24*60:
		return "5h"
	case w.WindowMinutes > 24*60:
		return "weekly"
	case primary:
		return "5h"
	}
	return "weekly"
}

func codexResetMs(w *codexWin, at int64) int64 {
	switch {
	case w.ResetsAt != nil && *w.ResetsAt > 0:
		return *w.ResetsAt * 1000
	case w.ResetsInSeconds != nil:
		return at + *w.ResetsInSeconds*1000
	}
	return 0
}

func (u *usageLimits) codexUsage(now time.Time) agentUsage {
	a := agentUsage{Agent: "codex", Installed: u.installed("codex"), Source: "Codex rollout token_count.rate_limits", SourcePath: u.codex.root,
		LocalSource: "Codex rollouts in ~/.codex/sessions, cost estimated from tokens"}
	for _, rd := range u.codex.takePending() {
		for i, w := range []*codexWin{rd.Primary, rd.Secondary} {
			if w != nil {
				u.hist.add("codex:"+codexWinKey(w, i == 0), rd.At, *pctPtr(*w.UsedPercent), now)
			}
		}
	}
	got := map[string]codexLatest{}
	var planAt, creditsAt int64
	for _, f := range u.codex.files {
		if f.plan != "" && f.planAt >= planAt {
			a.Plan, planAt = f.plan, f.planAt
		}
		if c := f.credits; c != nil && f.creditsAt >= creditsAt {
			creditsAt = f.creditsAt
			a.Credits = &usageCredits{Source: "Codex rollout token_count.rate_limits.credits", MeasuredAt: creditsAt,
				HasCredits: c.HasCredits, Unlimited: c.Unlimited}
			if b := bytes.TrimSpace(c.Balance); len(b) > 0 && string(b) != "null" {
				a.Credits.Balance = b
			}
		}
		for i, l := range f.latest {
			if l.w == nil {
				continue
			}
			k := [2]string{"5h", "weekly"}[i]
			if g, ok := got[k]; !ok || l.at >= g.at {
				got[k] = l
			}
			a.MeasuredAt = max(a.MeasuredAt, l.at)
		}
	}
	for _, spec := range []struct {
		key string
		min int
	}{{"5h", 300}, {"weekly", 10080}} {
		l, ok := got[spec.key]
		if !ok {
			a.Windows = append(a.Windows, u.window("codex", spec.key, spec.min, nil, 0, 0, now))
			continue
		}
		m := spec.min
		if l.w.WindowMinutes > 0 {
			m = l.w.WindowMinutes
		}
		a.Windows = append(a.Windows, u.window("codex", spec.key, m, l.w.UsedPercent, codexResetMs(l.w, l.at), l.at, now))
	}
	if len(got) == 0 {
		a.Note = "No plan reading. Recent Codex rollouts report rate_limits with no primary or secondary window"
		if a.Plan != "" {
			a.Note += " (plan " + a.Plan + ")"
		}
		a.Note += "."
	}
	a.Local, a.Models = u.local(u.codex.modelTotals, a.Windows, now, true)
	return a
}

var usageLimitsSvc *usageLimits

func (u *usageLimits) shutdown() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.loaded {
		u.saveState()
	}
}

func registerUsageRoutes(mux *http.ServeMux) {
	usageLimitsSvc = newUsageLimits(homeDir(), filepath.Join(stateDir(), "usage-history.json"), nil)
	go usageLimitsSvc.get() // the first transcript scan takes seconds
	onShutdown(usageLimitsSvc.shutdown)
	mux.HandleFunc("GET /api/usage/limits", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, usageLimitsSvc.get())
	})
}
