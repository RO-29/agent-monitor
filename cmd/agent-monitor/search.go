package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// registerSearchRoutes wires search and span analytics and starts the indexer.
func registerSearchRoutes(mux *http.ServeMux) {
	if os.Getenv("AGENT_MONITOR_SEARCH") != "0" {
		startSearchIndexer()
	}
	mux.HandleFunc("/api/search/status", withIndex(func(w http.ResponseWriter, r *http.Request, ix *searchIndex) {
		writeJSON(w, ix.status())
	}))
	mux.HandleFunc("/api/search", withIndex(handleSearch))
	mux.HandleFunc("/api/search/context", withIndex(handleSearchContext))
	mux.HandleFunc("/api/search/ask", withIndex(handleSearchAsk))
	mux.HandleFunc("/api/spans", withIndex(handleSpans))
	mux.HandleFunc("/api/tools/latency", withIndex(handleToolLatency))
	mux.HandleFunc("/api/errors", withIndex(handleErrors))
	mux.HandleFunc("/api/graph", withIndex(handleGraph))
}

func withIndex(h func(http.ResponseWriter, *http.Request, *searchIndex)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if searchIx == nil {
			writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{"error": "search index not available"})
			return
		}
		h(w, r, searchIx)
	}
}

// ── thread + repo caches ──────────────────────────────────────────────────

var searchCache = struct {
	mu        sync.Mutex
	threadOf  map[string]string
	members   map[string][]string
	threadsAt time.Time
	repos     map[string]bool
	reposAt   time.Time
}{}

func refreshThreads() {
	searchCache.mu.Lock()
	defer searchCache.mu.Unlock()
	if time.Since(searchCache.threadsAt) < 30*time.Second && searchCache.threadOf != nil {
		return
	}
	searchCache.threadOf, searchCache.members = map[string]string{}, map[string][]string{}
	searchCache.threadsAt = time.Now()
	if store == nil {
		return
	}
	threads, threadOf := computeThreads(store.All(), panesBySession())
	searchCache.threadOf = threadOf
	for _, t := range threads {
		searchCache.members[t.ID] = t.Sessions
	}
}

func cachedThreadOf() map[string]string {
	refreshThreads()
	searchCache.mu.Lock()
	defer searchCache.mu.Unlock()
	return searchCache.threadOf
}

func threadMembers(id string) []string {
	refreshThreads()
	searchCache.mu.Lock()
	defer searchCache.mu.Unlock()
	if m := searchCache.members[id]; len(m) > 0 {
		return m
	}
	if root := searchCache.threadOf[id]; root != "" {
		return searchCache.members[root]
	}
	return nil
}

func (ix *searchIndex) knownRepos() map[string]bool {
	searchCache.mu.Lock()
	defer searchCache.mu.Unlock()
	if searchCache.repos != nil && time.Since(searchCache.reposAt) < 2*time.Minute {
		return searchCache.repos
	}
	m := map[string]bool{}
	if rows, err := ix.r.Query(`SELECT DISTINCT repo FROM spans WHERE repo <> ''`); err == nil {
		for rows.Next() {
			var s string
			if rows.Scan(&s) == nil {
				m[strings.ToLower(s)] = true
			}
		}
		rows.Close()
	}
	delete(m, strings.ToLower(baseName(homeDir())))
	searchCache.repos, searchCache.reposAt = m, time.Now()
	return m
}

func baseName(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// ── /api/search ───────────────────────────────────────────────────────────

func handleSearch(w http.ResponseWriter, r *http.Request, ix *searchIndex) {
	q := r.URL.Query()
	limit := clampInt(qInt(q.Get("limit"), 50), 1, 200)
	offset := clampInt(qInt(q.Get("offset"), 0), 0, 10000)
	pq := parseSearchQuery(q.Get("q"), ix.knownRepos())
	res, err := ix.search(pq, q.Get("scope"), q.Get("kind"), limit, offset, time.Now())
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, res)
}

func clampInt(v, lo, hi int) int {
	return max(lo, min(hi, v))
}

type contextDoc struct {
	searchHit
	Text string `json:"text"`
}

func handleSearchContext(w http.ResponseWriter, r *http.Request, ix *searchIndex) {
	id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	n := clampInt(qInt(r.URL.Query().Get("n"), 2), 0, 20)
	doc, before, after, err := ix.context(id, n)
	if err != nil {
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"doc": doc, "before": before, "after": after})
}

func (ix *searchIndex) context(id int64, n int) (*contextDoc, []contextDoc, []contextDoc, error) {
	read := func(q string, args ...any) ([]contextDoc, error) {
		rows, err := ix.r.Query(q, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := []contextDoc{}
		for rows.Next() {
			var text sql.NullString
			h, err := scanHit(rows, &text)
			if err != nil {
				return nil, err
			}
			out = append(out, contextDoc{searchHit: h, Text: clipString(text.String, 8192)})
		}
		return out, rows.Err()
	}
	self, err := read(`SELECT `+hitCols+`, d.text FROM docs d WHERE d.id=?`, id)
	if err != nil || len(self) == 0 {
		return nil, nil, nil, fmt.Errorf("doc %d not found", id)
	}
	d := self[0]
	d.ThreadID = cachedThreadOf()[d.Session]
	before, err := read(`SELECT `+hitCols+`, d.text FROM docs d WHERE d.session=? AND (d.ts < ? OR (d.ts = ? AND d.id < ?)) ORDER BY d.ts DESC, d.id DESC LIMIT ?`, d.Session, d.Ts, d.Ts, d.ID, n)
	if err != nil {
		return nil, nil, nil, err
	}
	for i, j := 0, len(before)-1; i < j; i, j = i+1, j-1 {
		before[i], before[j] = before[j], before[i]
	}
	after, err := read(`SELECT `+hitCols+`, d.text FROM docs d WHERE d.session=? AND (d.ts > ? OR (d.ts = ? AND d.id > ?)) ORDER BY d.ts, d.id LIMIT ?`, d.Session, d.Ts, d.Ts, d.ID, n)
	if err != nil {
		return nil, nil, nil, err
	}
	return &d, before, after, nil
}

// promptCounts returns the number of prompt docs (turns) per session.
func (ix *searchIndex) promptCounts(sessions []string) map[string]int {
	out := map[string]int{}
	for _, s := range sessions {
		var n int
		if ix.r.QueryRow(`SELECT COUNT(*) FROM docs WHERE session=? AND kind='prompt'`, s).Scan(&n) == nil {
			out[s] = n
		}
	}
	return out
}

// ── /api/search/ask ───────────────────────────────────────────────────────

type askCitation struct {
	N       int    `json:"n"`
	HitID   int64  `json:"hitId"`
	Session string `json:"session"`
	Ts      int64  `json:"ts"`
}

var reCiteN = regexp.MustCompile(`\[(\d{1,2})\]`)

func handleSearchAsk(w http.ResponseWriter, r *http.Request, ix *searchIndex) {
	if r.Method != http.MethodPost {
		writeJSONStatus(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST only"})
		return
	}
	var body struct {
		Q string `json:"q"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil || strings.TrimSpace(body.Q) == "" {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "q is required"})
		return
	}
	st := getSettings()
	if !st.EnrichEnabled {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "enrichment is disabled in settings"})
		return
	}
	if st.SpentDay == time.Now().Format("2006-01-02") && st.SpentTodayUSD >= st.DailyCapUSD {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "daily cap reached"})
		return
	}
	pq := parseSearchQuery(body.Q, ix.knownRepos())
	res, err := ix.search(pq, "", "", 20, 0, time.Now())
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if len(res.Hits) == 0 {
		writeJSON(w, map[string]any{"answer": "No matching transcript lines.", "citations": []askCitation{}, "costUsd": 0})
		return
	}
	texts := ix.hitTexts(res.Hits)
	answer, cost, err := runClaudeAsk(buildAskPrompt(body.Q, res.Hits, texts), st.EnrichModel)
	if err != nil {
		writeJSONStatus(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	addSpent(cost)
	writeJSON(w, map[string]any{"answer": answer, "citations": askCitations(answer, res.Hits), "costUsd": cost})
}

func (ix *searchIndex) hitTexts(hits []searchHit) map[int64]string {
	out := map[int64]string{}
	for _, h := range hits {
		var t sql.NullString
		if ix.r.QueryRow(`SELECT text FROM docs WHERE id=?`, h.ID).Scan(&t) == nil {
			out[h.ID] = t.String
		}
	}
	return out
}

// buildAskPrompt numbers each hit so the answer can cite [n].
func buildAskPrompt(q string, hits []searchHit, texts map[int64]string) string {
	var b strings.Builder
	b.WriteString("You answer a question about the user's past coding-agent sessions (Claude Code and Codex).\n")
	b.WriteString("Use ONLY the numbered excerpts below. Cite every claim with [n]. If the excerpts do not answer the question, say so.\n")
	b.WriteString("Answer in at most 120 words, plain sentences, no code blocks.\n\n")
	fmt.Fprintf(&b, "QUESTION: %s\n\nEXCERPTS:\n", strings.TrimSpace(q))
	budget := 24000
	for i, h := range hits {
		t := texts[h.ID]
		if t == "" {
			t = h.Snippet
		}
		t = clipString(strings.Join(strings.Fields(t), " "), 1200)
		line := fmt.Sprintf("[%d] %s · %s · %s · %s%s\n%s\n\n", i+1, time.UnixMilli(h.Ts).Format("2006-01-02 15:04"), h.Agent, h.Repo, h.Kind, toolSuffix(h.Tool), t)
		if budget -= len(line); budget < 0 {
			break
		}
		b.WriteString(line)
	}
	return b.String()
}

func toolSuffix(t string) string {
	if t == "" {
		return ""
	}
	return " · " + t
}

func askCitations(answer string, hits []searchHit) []askCitation {
	out := []askCitation{}
	seen := map[int]bool{}
	for _, m := range reCiteN.FindAllStringSubmatch(answer, -1) {
		n, _ := strconv.Atoi(m[1])
		if n < 1 || n > len(hits) || seen[n] {
			continue
		}
		seen[n] = true
		h := hits[n-1]
		out = append(out, askCitation{N: n, HitID: h.ID, Session: h.Session, Ts: h.Ts})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].N < out[j].N })
	return out
}

func runClaudeAsk(prompt, model string) (string, float64, error) {
	if _, err := exec.LookPath("claude"); err != nil {
		return "", 0, fmt.Errorf("claude CLI not on PATH")
	}
	if model == "" {
		model = "haiku"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "claude", "-p", "--model", model, "--output-format", "json", "--max-turns", "1")
	cmd.Stdin = strings.NewReader(prompt)
	env := []string{"CLAUDE_CODE_DISABLE_HOOKS=1"}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "CLAUDECODE=") && !strings.HasPrefix(kv, "CLAUDE_CODE_ENTRYPOINT=") {
			env = append(env, kv)
		}
	}
	cmd.Env, cmd.Dir = env, homeDir()
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", 0, fmt.Errorf("claude -p failed: %v: %s", err, clipString(errb.String(), 300))
	}
	var envl struct {
		Result       string  `json:"result"`
		TotalCostUSD float64 `json:"total_cost_usd"`
	}
	if json.Unmarshal(out.Bytes(), &envl) != nil || envl.Result == "" {
		return "", 0, fmt.Errorf("unexpected claude output: %s", clipString(out.String(), 300))
	}
	return strings.TrimSpace(envl.Result), envl.TotalCostUSD, nil
}

// ── span analytics ────────────────────────────────────────────────────────

type spanRow struct {
	ID      string `json:"id"`
	Session string `json:"session"`
	Agent   string `json:"agent"`
	Cwd     string `json:"cwd"`
	Repo    string `json:"repo"`
	Ts      int64  `json:"ts"`
	Dur     int64  `json:"dur"`
	Tool    string `json:"tool"`
	Fam     string `json:"fam"`
	Cmd     string `json:"cmd"`
	CmdKey  string `json:"cmdKey"`
	Err     bool   `json:"err"`
	ErrSig  string `json:"errSig,omitempty"`
	ErrText string `json:"errText,omitempty"`
	Child   string `json:"child,omitempty"`
}

const spanCols = `s.id, s.session, s.agent, COALESCE(s.cwd,''), COALESCE(s.repo,''), s.ts, COALESCE(s.dur,0), s.tool, COALESCE(s.fam,''), COALESCE(s.cmd,''), COALESCE(s.cmdKey,''), COALESCE(s.err,0), COALESCE(s.errSig,''), COALESCE(s.errText,''), COALESCE(s.child,'')`

func scanSpan(rows *sql.Rows) (spanRow, error) {
	var s spanRow
	var e int
	err := rows.Scan(&s.ID, &s.Session, &s.Agent, &s.Cwd, &s.Repo, &s.Ts, &s.Dur, &s.Tool, &s.Fam, &s.Cmd, &s.CmdKey, &e, &s.ErrSig, &s.ErrText, &s.Child)
	s.Err = e != 0
	return s, err
}

// spanFilter reads since (default 7d), repo, agent, minDur and q (chips + words over cmd).
func (ix *searchIndex) spanFilter(r *http.Request, now time.Time) sqlFilter {
	var f sqlFilter
	q := r.URL.Query()
	pq := parseSearchQuery(q.Get("q"), ix.knownRepos())
	chips := pq.Chips
	hasSince := false
	for _, c := range chips {
		hasSince = hasSince || c.Key == "since"
	}
	if v := q.Get("since"); v != "" {
		chips = append(chips, searchChip{Key: "since", Value: v})
		hasSince = true
	}
	if !hasSince {
		chips = append(chips, searchChip{Key: "since", Value: "7d"})
	}
	if v := q.Get("repo"); v != "" {
		chips = append(chips, searchChip{Key: "repo", Value: v})
	}
	if v := q.Get("agent"); v != "" {
		chips = append(chips, searchChip{Key: "agent", Value: v})
	}
	for _, c := range chips {
		switch c.Key {
		case "agent":
			f.add("s.agent = ?", strings.ToLower(c.Value))
		case "repo":
			f.add("s.repo = ? COLLATE NOCASE", c.Value)
		case "tool":
			f.add("(s.tool = ? COLLATE NOCASE OR s.tool LIKE ?)", c.Value, c.Value+"%")
		case "is":
			f.add("s.err = 1")
		case "cwd":
			f.add("s.cwd LIKE ?", "%"+c.Value+"%")
		case "file":
			f.add("s.cmd LIKE ?", "%"+c.Value+"%")
		case "session":
			f.add("(s.session = ? OR s.session LIKE ?)", c.Value, "%:"+c.Value)
		case "since", "until":
			if ms, ok := resolveSince(c.Value, now); ok {
				op := ">="
				if c.Key == "until" {
					op = "<"
				}
				f.add("s.ts "+op+" ?", ms)
			}
		}
	}
	for _, t := range pq.terms {
		f.add("s.cmd LIKE ?", "%"+t.Text+"%")
	}
	if v := qInt(q.Get("minDur"), 0); v > 0 {
		f.add("s.dur >= ?", v)
	}
	return f
}

// percentile is nearest-rank over an ascending slice.
func percentile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	k := int(math.Ceil(p*float64(len(sorted)))) - 1
	return sorted[clampInt(k, 0, len(sorted)-1)]
}

var histEdges = []int64{0, 10, 30, 100, 300, 1000, 3000, 10_000, 30_000, 100_000, 300_000, 1_000_000, 3_600_000}

type histBucket struct {
	FromMs int64 `json:"fromMs"`
	ToMs   int64 `json:"toMs"` // -1 = open-ended
	N      int   `json:"n"`
	Failed int   `json:"failed"`
}

func histogram(rows []spanRow) []histBucket {
	bs := make([]histBucket, len(histEdges))
	for i := range histEdges {
		bs[i].FromMs = histEdges[i]
		bs[i].ToMs = -1
		if i+1 < len(histEdges) {
			bs[i].ToMs = histEdges[i+1]
		}
	}
	for _, s := range rows {
		i := sort.Search(len(histEdges), func(i int) bool { return histEdges[i] > s.Dur }) - 1
		i = clampInt(i, 0, len(bs)-1)
		bs[i].N++
		if s.Err {
			bs[i].Failed++
		}
	}
	return bs
}

func (ix *searchIndex) querySpans(f sqlFilter, order string, limit int) ([]spanRow, error) {
	q := "SELECT " + spanCols + " FROM spans s WHERE " + f.sql()
	if order != "" {
		q += " ORDER BY " + order
	}
	args := f.args
	if limit > 0 {
		q += " LIMIT ?"
		args = append(append([]any(nil), f.args...), limit)
	}
	rows, err := ix.r.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []spanRow
	for rows.Next() {
		s, err := scanSpan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

type spanGroup struct {
	Key    string   `json:"key"`
	Runs   int      `json:"runs"`
	Failed int      `json:"failed"`
	P50    int64    `json:"p50"`
	P95    int64    `json:"p95"`
	Repos  []string `json:"repos"`
	LastTs int64    `json:"lastTs"`
	durs   []int64
	repoM  map[string]bool
}

func groupKey(s spanRow, by string) string {
	switch by {
	case "tool":
		return s.Tool
	case "repo":
		return s.Repo
	case "errSig":
		return s.ErrSig
	}
	if s.CmdKey != "" {
		return s.CmdKey
	}
	return s.Tool
}

func groupSpans(rows []spanRow, by string) []*spanGroup {
	m := map[string]*spanGroup{}
	var order []*spanGroup
	for _, s := range rows {
		k := groupKey(s, by)
		g := m[k]
		if g == nil {
			g = &spanGroup{Key: k, repoM: map[string]bool{}}
			m[k] = g
			order = append(order, g)
		}
		g.Runs++
		if s.Err {
			g.Failed++
		}
		g.durs = append(g.durs, s.Dur)
		if s.Repo != "" {
			g.repoM[s.Repo] = true
		}
		g.LastTs = max(g.LastTs, s.Ts)
	}
	for _, g := range order {
		sort.Slice(g.durs, func(i, j int) bool { return g.durs[i] < g.durs[j] })
		g.P50, g.P95 = percentile(g.durs, 0.5), percentile(g.durs, 0.95)
		g.Repos = sortedKeys(g.repoM)
	}
	sort.SliceStable(order, func(i, j int) bool { return order[i].Runs > order[j].Runs })
	return order
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func handleSpans(w http.ResponseWriter, r *http.Request, ix *searchIndex) {
	now := time.Now()
	f := ix.spanFilter(r, now)
	by := r.URL.Query().Get("groupBy")
	if by == "" {
		by = "cmdKey"
	}
	if by == "errSig" {
		f.add("s.err = 1")
	}
	limit := clampInt(qInt(r.URL.Query().Get("limit"), 50), 1, 1000)
	rows, err := ix.querySpans(f, "s.ts DESC", 0)
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	failed := 0
	for _, s := range rows {
		if s.Err {
			failed++
		}
	}
	out := map[string]any{"total": len(rows), "failed": failed, "histogram": map[string]any{"buckets": histogram(rows)}, "groups": []*spanGroup{}}
	if by == "none" {
		out["spans"] = rows[:min(len(rows), limit)]
	} else {
		gs := groupSpans(rows, by)
		out["groups"] = gs[:min(len(gs), limit)]
	}
	writeJSON(w, out)
}

// dailyTrend counts rows per local day for the last 7 days (index 6 = today).
func dailyTrend(ts []int64, now time.Time) []int {
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	out := make([]int, 7)
	for _, t := range ts {
		tt := time.UnixMilli(t)
		tday := time.Date(tt.Year(), tt.Month(), tt.Day(), 0, 0, 0, 0, now.Location())
		if d := int(math.Round(day.Sub(tday).Hours() / 24)); d >= 0 && d < 7 {
			out[6-d]++
		}
	}
	return out
}

func handleToolLatency(w http.ResponseWriter, r *http.Request, ix *searchIndex) {
	now := time.Now()
	rows, err := ix.querySpans(ix.spanFilter(r, now), "", 0)
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	r7 := r.Clone(r.Context())
	q7 := r7.URL.Query()
	q7.Set("since", "7d")
	r7.URL.RawQuery = q7.Encode()
	trendRows, _ := ix.querySpans(ix.spanFilter(r7, now), "", 0)
	trendTs := map[string][]int64{}
	for _, s := range trendRows {
		trendTs[s.Tool] = append(trendTs[s.Tool], s.Ts)
	}
	type toolRow struct {
		Tool    string  `json:"tool"`
		Calls   int     `json:"calls"`
		P50     int64   `json:"p50"`
		P95     int64   `json:"p95"`
		ErrRate float64 `json:"errRate"`
		Trend   []int   `json:"trend"`
	}
	out := []toolRow{}
	for _, g := range groupSpans(rows, "tool") {
		out = append(out, toolRow{Tool: g.Key, Calls: g.Runs, P50: g.P50, P95: g.P95, ErrRate: round3(float64(g.Failed) / float64(g.Runs)), Trend: dailyTrend(trendTs[g.Key], now)})
	}
	writeJSON(w, map[string]any{"tools": out})
}

func round3(x float64) float64 { return math.Round(x*1000) / 1000 }

func handleErrors(w http.ResponseWriter, r *http.Request, ix *searchIndex) {
	now := time.Now()
	f := ix.spanFilter(r, now)
	f.add("s.err = 1")
	rows, err := ix.querySpans(f, "s.ts DESC", 0)
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"groups": errorGroups(rows, now, clampInt(qInt(r.URL.Query().Get("limit"), 100), 1, 1000))})
}

type errGroup struct {
	Sig      string   `json:"sig"`
	Sample   string   `json:"sample"`
	Count    int      `json:"count"`
	Sessions int      `json:"sessions"`
	Repos    []string `json:"repos"`
	FirstTs  int64    `json:"firstTs"`
	LastTs   int64    `json:"lastTs"`
	Tool     string   `json:"tool"`
	CmdKey   string   `json:"cmdKey"`
	Trend    []int    `json:"trend"`
}

// errorGroups expects rows newest first, so the sample is the latest occurrence.
func errorGroups(rows []spanRow, now time.Time, limit int) []errGroup {
	type acc struct {
		g              errGroup
		sess, repos    map[string]bool
		tools, cmdKeys map[string]int
		ts             []int64
	}
	m := map[string]*acc{}
	var order []*acc
	for _, s := range rows {
		k := s.ErrSig
		if k == "" {
			k = "(no signature) " + s.Tool
		}
		a := m[k]
		if a == nil {
			a = &acc{g: errGroup{Sig: k, Sample: s.ErrText, FirstTs: s.Ts}, sess: map[string]bool{}, repos: map[string]bool{}, tools: map[string]int{}, cmdKeys: map[string]int{}}
			m[k] = a
			order = append(order, a)
		}
		a.g.Count++
		a.sess[s.Session] = true
		if s.Repo != "" {
			a.repos[s.Repo] = true
		}
		a.tools[s.Tool]++
		a.cmdKeys[s.CmdKey]++
		a.ts = append(a.ts, s.Ts)
		a.g.FirstTs = min(a.g.FirstTs, s.Ts)
		a.g.LastTs = max(a.g.LastTs, s.Ts)
	}
	out := make([]errGroup, 0, len(order))
	for _, a := range order {
		a.g.Sessions, a.g.Repos = len(a.sess), sortedKeys(a.repos)
		a.g.Tool, a.g.CmdKey = topKey(a.tools), topKey(a.cmdKeys)
		a.g.Trend = dailyTrend(a.ts, now)
		out = append(out, a.g)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out[:min(len(out), limit)]
}

func topKey(m map[string]int) string {
	best, n := "", -1
	for k, v := range m {
		if v > n || (v == n && k < best) {
			best, n = k, v
		}
	}
	return best
}

var (
	reSearchCodexExec = regexp.MustCompile(`(^|[\s;&|(])codex\s+(exec|e|review)\b`)
	reSearchClaudeP   = regexp.MustCompile(`(^|[\s;&|(])claude\s+(-p|--print)\b`)
)

type graphNode struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}

type graphEdge struct {
	From     string  `json:"from"`
	To       string  `json:"to"`
	Calls    int     `json:"calls"`
	FailRate float64 `json:"failRate"`
	P95      int64   `json:"p95"`
}

func handleGraph(w http.ResponseWriter, r *http.Request, ix *searchIndex) {
	rows, err := ix.querySpans(ix.spanFilter(r, time.Now()), "", 0)
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	nodes, edges := buildGraph(rows)
	writeJSON(w, map[string]any{"nodes": nodes, "edges": edges})
}

// buildGraph links agents to subagents (Task / spawn_agent), delegated CLIs and MCP servers.
func buildGraph(rows []spanRow) ([]graphNode, []graphEdge) {
	nodes := map[string]*graphNode{}
	type eacc struct {
		calls, failed int
		durs          []int64
	}
	edges := map[[2]string]*eacc{}
	node := func(id, label, kind string) *graphNode {
		n := nodes[id]
		if n == nil {
			n = &graphNode{ID: id, Label: label, Kind: kind}
			nodes[id] = n
		}
		return n
	}
	for _, s := range rows {
		from := s.Agent
		node(from, from, "agent").Count++
		var to *graphNode
		switch {
		case s.Fam == "agent":
			to = node(s.Agent+"/subagent", s.Agent+" subagents", "subagent")
		case s.Fam == "bash" && reSearchCodexExec.MatchString(s.Cmd):
			to = node("delegated:codex", "codex exec", "delegated")
		case s.Fam == "bash" && reSearchClaudeP.MatchString(s.Cmd):
			to = node("delegated:claude", "claude -p", "delegated")
		case strings.HasPrefix(s.Tool, "mcp__"):
			srv := strings.SplitN(strings.TrimPrefix(s.Tool, "mcp__"), "__", 2)[0]
			to = node("mcp:"+srv, srv, "mcp")
		}
		if to == nil || to.ID == from {
			continue
		}
		to.Count++
		k := [2]string{from, to.ID}
		e := edges[k]
		if e == nil {
			e = &eacc{}
			edges[k] = e
		}
		e.calls++
		if s.Err {
			e.failed++
		}
		e.durs = append(e.durs, s.Dur)
	}
	ns := make([]graphNode, 0, len(nodes))
	for _, n := range nodes {
		ns = append(ns, *n)
	}
	sort.Slice(ns, func(i, j int) bool { return ns[i].Count > ns[j].Count })
	es := make([]graphEdge, 0, len(edges))
	for k, e := range edges {
		sort.Slice(e.durs, func(i, j int) bool { return e.durs[i] < e.durs[j] })
		es = append(es, graphEdge{From: k[0], To: k[1], Calls: e.calls, FailRate: round3(float64(e.failed) / float64(e.calls)), P95: percentile(e.durs, 0.95)})
	}
	sort.Slice(es, func(i, j int) bool { return es[i].Calls > es[j].Calls })
	return ns, es
}
