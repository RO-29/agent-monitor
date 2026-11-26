package main

import (
	"database/sql"
	"fmt"
	"html"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type searchChip struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Source string `json:"source"` // typed | inferred
}

type searchTerm struct {
	Text   string
	Phrase bool // came from "double quotes"
}

type parsedQuery struct {
	Text  string       `json:"text"`
	Chips []searchChip `json:"chips"`
	terms []searchTerm
}

var searchChipKeys = map[string]bool{"agent": true, "repo": true, "cwd": true, "kind": true, "tool": true, "file": true,
	"is": true, "since": true, "until": true, "session": true, "thread": true}

var (
	reFileExt   = regexp.MustCompile(`^[^\s/]*[A-Za-z_][^\s/]*\.[A-Za-z][A-Za-z0-9]{0,5}$`)
	reWeekday   = map[string]time.Weekday{"mon": time.Monday, "monday": time.Monday, "tue": time.Tuesday, "tuesday": time.Tuesday, "wed": time.Wednesday, "wednesday": time.Wednesday, "thu": time.Thursday, "thursday": time.Thursday, "fri": time.Friday, "friday": time.Friday, "sat": time.Saturday, "saturday": time.Saturday, "sun": time.Sunday, "sunday": time.Sunday}
	errorWords  = map[string]bool{"failed": true, "failure": true, "errors": true, "broken": true}
	kindWords   = map[string]string{"prompt": "prompt", "prompts": "prompt", "command": "command", "commands": "command", "reply": "reply", "replies": "reply"}
	reRelative  = regexp.MustCompile(`^(\d+)([hdw])$`)
	reTrailPunc = regexp.MustCompile(`[,;:!?]+$`)
)

// tokenizeQuery splits on spaces and keeps "quoted phrases" (also as key:"value").
func tokenizeQuery(q string) []searchTerm {
	var out []searchTerm
	var cur strings.Builder
	inQ, quoted := false, false
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, searchTerm{Text: cur.String(), Phrase: quoted})
		}
		cur.Reset()
		quoted = false
	}
	for _, r := range q {
		switch {
		case r == '"':
			inQ = !inQ
			quoted = true
		case unicode.IsSpace(r) && !inQ:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// parseSearchQuery turns typed chips and natural words into chips plus FTS terms.
func parseSearchQuery(q string, repos map[string]bool) parsedQuery {
	pq := parsedQuery{Chips: []searchChip{}}
	toks := tokenizeQuery(q)
	add := func(k, v, src string) {
		for _, c := range pq.Chips {
			if c.Key == k && c.Value == v {
				return
			}
		}
		pq.Chips = append(pq.Chips, searchChip{Key: k, Value: v, Source: src})
	}
	var rest []searchTerm
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if k, v, ok := strings.Cut(t.Text, ":"); ok && searchChipKeys[strings.ToLower(k)] && v != "" {
			k = strings.ToLower(k)
			if k == "is" && strings.ToLower(v) != "error" {
				rest = append(rest, t)
				continue
			}
			add(k, v, "typed")
			continue
		}
		if t.Phrase {
			rest = append(rest, t)
			continue
		}
		w := strings.ToLower(reTrailPunc.ReplaceAllString(t.Text, ""))
		if i+1 < len(toks) && !toks[i+1].Phrase {
			next := strings.ToLower(toks[i+1].Text)
			if (w == "last" || w == "this" || w == "past") && (next == "week" || next == "month") {
				v := map[string]string{"last week": "7d", "past week": "7d", "this week": "mon", "last month": "30d", "past month": "30d", "this month": "30d"}[w+" "+next]
				add("since", v, "inferred")
				i++
				continue
			}
		}
		switch {
		case w == "claude" || w == "codex":
			add("agent", w, "inferred")
		case errorWords[w]:
			add("is", "error", "inferred")
		case w == "today" || w == "yesterday":
			add("since", w, "inferred")
		case reWeekday[w] != 0 || w == "sunday" || w == "sun":
			add("since", w, "inferred")
		case kindWords[w] != "":
			add("kind", kindWords[w], "inferred")
		case len(w) >= 3 && repos[w]:
			add("repo", w, "inferred")
		case strings.Contains(t.Text, "/") && !strings.HasPrefix(t.Text, "http"):
			add("file", t.Text, "inferred")
		case reFileExt.MatchString(t.Text):
			add("file", t.Text, "inferred")
		default:
			rest = append(rest, t)
		}
	}
	pq.terms = rest
	parts := make([]string, len(rest))
	for i, t := range rest {
		if t.Phrase {
			parts[i] = `"` + t.Text + `"`
		} else {
			parts[i] = t.Text
		}
	}
	pq.Text = strings.Join(parts, " ")
	return pq
}

// ftsMatch builds a quote-safe FTS5 query: every term quoted, last bare term as prefix.
func ftsMatch(terms []searchTerm) string {
	var b strings.Builder
	for i, t := range terms {
		s := strings.TrimSpace(t.Text)
		if s == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(`"` + strings.ReplaceAll(s, `"`, `""`) + `"`)
		if i == len(terms)-1 && !t.Phrase {
			b.WriteByte('*')
		}
	}
	return b.String()
}

// triMatch is the trigram query: only terms of 3+ chars, no prefix marker.
func triMatch(terms []searchTerm) string {
	var parts []string
	for _, t := range terms {
		s := strings.TrimSpace(t.Text)
		if len([]rune(s)) < 3 {
			continue
		}
		parts = append(parts, `"`+strings.ReplaceAll(s, `"`, `""`)+`"`)
	}
	return strings.Join(parts, " ")
}

func identifierLike(terms []searchTerm) bool {
	for _, t := range terms {
		s := t.Text
		if strings.ContainsAny(s, "-_./:") {
			return true
		}
		hasLower, hasUpperAfter, hasDigit, hasLetter := false, false, false, false
		for i, r := range s {
			switch {
			case unicode.IsLower(r):
				hasLower = true
				hasLetter = true
			case unicode.IsUpper(r):
				hasLetter = true
				if i > 0 && hasLower {
					hasUpperAfter = true
				}
			case unicode.IsDigit(r):
				hasDigit = true
			}
		}
		if hasUpperAfter || (hasDigit && hasLetter) {
			return true
		}
	}
	return false
}

// resolveSince maps a since/until value to epoch ms.
func resolveSince(v string, now time.Time) (int64, bool) {
	v = strings.ToLower(strings.TrimSpace(v))
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	switch v {
	case "today":
		return day.UnixMilli(), true
	case "yesterday":
		return day.AddDate(0, 0, -1).UnixMilli(), true
	}
	if m := reRelative.FindStringSubmatch(v); m != nil {
		n, _ := strconv.Atoi(m[1])
		unit := map[string]time.Duration{"h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour}[m[2]]
		return now.Add(-time.Duration(n) * unit).UnixMilli(), true
	}
	if wd, ok := reWeekday[v]; ok {
		back := (int(day.Weekday()) - int(wd) + 7) % 7
		return day.AddDate(0, 0, -back).UnixMilli(), true
	}
	if t, err := time.ParseInLocation("2006-01-02", v, now.Location()); err == nil {
		return t.UnixMilli(), true
	}
	return 0, false
}

// ── SQL filters ───────────────────────────────────────────────────────────

type sqlFilter struct {
	where []string
	args  []any
}

func (f *sqlFilter) add(cond string, args ...any) {
	f.where = append(f.where, cond)
	f.args = append(f.args, args...)
}

func (f *sqlFilter) sql() string {
	if len(f.where) == 0 {
		return "1=1"
	}
	return strings.Join(f.where, " AND ")
}

// docFilter turns chips into WHERE clauses over docs alias d.
func (ix *searchIndex) docFilter(chips []searchChip, kindParam string, now time.Time) sqlFilter {
	var f sqlFilter
	group := map[string][]string{}
	for _, c := range chips {
		group[c.Key] = append(group[c.Key], c.Value)
	}
	orEq := func(col string, vals []string, nocase bool) {
		var parts []string
		for _, v := range vals {
			if nocase {
				parts = append(parts, col+" = ? COLLATE NOCASE")
			} else {
				parts = append(parts, col+" = ?")
			}
			f.args = append(f.args, v)
		}
		f.where = append(f.where, "("+strings.Join(parts, " OR ")+")")
	}
	if v := group["agent"]; len(v) > 0 {
		orEq("d.agent", lowerAll(v), false)
	}
	if v := group["repo"]; len(v) > 0 {
		orEq("d.repo", v, true)
	}
	kinds := append([]string(nil), group["kind"]...)
	if kindParam != "" {
		kinds = append(kinds, strings.Split(kindParam, ",")...)
	}
	if len(group["is"]) > 0 {
		kinds = []string{"error"}
	}
	if len(kinds) > 0 {
		orEq("d.kind", lowerAll(kinds), false)
	}
	for _, v := range group["cwd"] {
		f.add("d.cwd LIKE ?", "%"+v+"%")
	}
	for _, v := range group["tool"] {
		f.add("(d.tool = ? COLLATE NOCASE OR d.tool LIKE ?)", v, v+"%")
	}
	for _, v := range group["file"] {
		if ix.fts5 && len([]rune(v)) >= 3 {
			f.add("((d.id IN (SELECT rowid FROM docs_tri WHERE docs_tri MATCH ?) AND (d.path LIKE ? OR d.kind IN ('command','file','tool','error','output'))) OR (d.kind IN ('error','output') AND d.text LIKE ?))", `"`+strings.ReplaceAll(v, `"`, `""`)+`"`, "%"+v+"%", "%"+v+"%")
		} else {
			f.add("(d.path LIKE ? OR (d.kind IN ('command','file','tool','error','output') AND d.text LIKE ?))", "%"+v+"%", "%"+v+"%")
		}
	}
	for _, v := range group["since"] {
		if ms, ok := resolveSince(v, now); ok {
			f.add("d.ts >= ?", ms)
		}
	}
	for _, v := range group["until"] {
		if ms, ok := resolveSince(v, now); ok {
			f.add("d.ts < ?", ms)
		}
	}
	for _, v := range group["session"] {
		f.add("(d.session = ? OR d.session LIKE ?)", v, "%:"+strings.TrimPrefix(strings.TrimPrefix(v, "claude:"), "codex:"))
	}
	for _, v := range group["thread"] {
		members := threadMembers(v)
		if len(members) == 0 {
			members = []string{v}
		}
		ph := strings.TrimSuffix(strings.Repeat("?,", len(members)), ",")
		args := make([]any, len(members))
		for i, m := range members {
			args[i] = m
		}
		f.add("d.session IN ("+ph+")", args...)
	}
	return f
}

func lowerAll(v []string) []string {
	out := make([]string, len(v))
	for i, s := range v {
		out[i] = strings.ToLower(s)
	}
	return out
}

// ── search ────────────────────────────────────────────────────────────────

type searchHit struct {
	ID       int64  `json:"id"`
	Session  string `json:"session"`
	Agent    string `json:"agent"`
	Cwd      string `json:"cwd"`
	Repo     string `json:"repo"`
	Ts       int64  `json:"ts"`
	Kind     string `json:"kind"`
	Tool     string `json:"tool"`
	Turn     int    `json:"turn"`
	Span     string `json:"span"`
	Path     string `json:"path"`
	Snippet  string `json:"snippet"`
	ThreadID string `json:"threadId,omitempty"`
	DrivenID string `json:"drivenId,omitempty"`
	score    float64
}

type searchResult struct {
	Query  parsedQuery               `json:"query"`
	Total  int                       `json:"total"`
	Facets map[string]map[string]int `json:"facets"`
	Hits   []searchHit               `json:"hits"`
	TookMs int64                     `json:"tookMs"`
	Mode   string                    `json:"mode"` // fts | trigram+fts | like | filter
}

const halfLifeMs = 14 * 24 * 3600 * 1000.0

const hitCols = `d.id, d.session, d.agent, d.cwd, d.repo, d.ts, d.kind, d.tool, d.turn, d.span, d.path`

func scanHit(rows *sql.Rows, extra ...any) (searchHit, error) {
	var h searchHit
	var cwd, repo, tool, span, path sql.NullString
	dest := append([]any{&h.ID, &h.Session, &h.Agent, &cwd, &repo, &h.Ts, &h.Kind, &tool, &h.Turn, &span, &path}, extra...)
	err := rows.Scan(dest...)
	h.Cwd, h.Repo, h.Tool, h.Span, h.Path = cwd.String, repo.String, tool.String, span.String, path.String
	return h, err
}

func (ix *searchIndex) search(pq parsedQuery, scope, kind string, limit, offset int, now time.Time) (*searchResult, error) {
	start := time.Now()
	res := &searchResult{Query: pq, Facets: map[string]map[string]int{"kind": {}, "agent": {}, "repo": {}, "tool": {}}, Hits: []searchHit{}}
	f := ix.docFilter(pq.Chips, kind, now)
	hasText := strings.TrimSpace(ftsMatch(pq.terms)) != ""
	if !hasText && len(f.where) == 0 {
		res.Mode = "empty"
		return res, nil
	}
	var err error
	switch {
	case !hasText:
		res.Mode = "filter"
		err = ix.searchFilter(res, f, limit, offset)
	case !ix.fts5:
		res.Mode = "like"
		err = ix.searchLike(res, pq, f, limit, offset)
	default:
		res.Mode = "fts"
		var a, b []searchHit
		var ta, tb int
		var fa, fb map[string]map[string]int
		a, ta, fa, err = ix.searchFTS("docs_fts", ftsMatch(pq.terms), f, scope, limit+offset, now)
		if err == nil && identifierLike(pq.terms) && triMatch(pq.terms) != "" {
			res.Mode = "trigram+fts"
			b, tb, fb, err = ix.searchFTS("docs_tri", triMatch(pq.terms), f, scope, limit+offset, now)
		}
		if err != nil {
			return nil, err
		}
		res.Total, res.Facets = ta, fa
		if tb > ta {
			res.Total, res.Facets = tb, fb
		}
		merged := mergeHits(a, b)
		if b != nil && len(a) == ta && len(b) == tb {
			res.Total = len(merged) // both lists are complete, so the union is exact
		}
		if offset < len(merged) {
			merged = merged[offset:min(len(merged), offset+limit)]
		} else {
			merged = nil
		}
		res.Hits = append(res.Hits, merged...)
	}
	if err != nil {
		return nil, err
	}
	threadOf := cachedThreadOf()
	for i := range res.Hits {
		h := &res.Hits[i]
		h.ThreadID = threadOf[h.Session]
		if h.DrivenID = drivenHitThread(h.Session); h.ThreadID == "" {
			h.ThreadID = h.DrivenID
		}
	}
	res.TookMs = time.Since(start).Milliseconds()
	return res, nil
}

// mergeHits normalizes each list's scores to its best hit, then merges by id.
func mergeHits(lists ...[]searchHit) []searchHit {
	best := map[int64]searchHit{}
	for _, l := range lists {
		top := 0.0
		for _, h := range l {
			if h.score > top {
				top = h.score
			}
		}
		for _, h := range l {
			if top > 0 {
				h.score /= top
			}
			if old, ok := best[h.ID]; !ok || h.score > old.score {
				best[h.ID] = h
			}
		}
	}
	out := make([]searchHit, 0, len(best))
	for _, h := range best {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		return out[i].Ts > out[j].Ts
	})
	return out
}

func (ix *searchIndex) searchFTS(table, match string, f sqlFilter, scope string, n int, now time.Time) ([]searchHit, int, map[string]map[string]int, error) {
	where := f.sql()
	from := fmt.Sprintf("FROM %s JOIN docs d ON d.id = %s.rowid WHERE %s MATCH ? AND %s", table, table, table, where)
	args := append([]any{match}, f.args...)
	total, facets, err := ix.facets(from, args)
	if err != nil {
		return nil, 0, nil, err
	}
	recency := "1.0"
	if ix.mathFns {
		recency = fmt.Sprintf("(1.0 + exp(-0.6931471805599453 * MAX(0, %d - d.ts) / %f))", now.UnixMilli(), halfLifeMs)
	}
	q := fmt.Sprintf(`SELECT %s, snippet(%s, -1, char(1), char(2), '…', 16), (-bm25(%s, 1.0, 0.5)) * %s * (CASE WHEN d.repo = ? COLLATE NOCASE AND ? <> '' THEN 1.5 ELSE 1.0 END) AS score
		%s ORDER BY score DESC LIMIT ?`, hitCols, table, table, recency, from)
	qargs := append([]any{scope, scope}, args...)
	qargs = append(qargs, n)
	rows, err := ix.r.Query(q, qargs...)
	if err != nil {
		return nil, 0, nil, err
	}
	defer rows.Close()
	var hits []searchHit
	for rows.Next() {
		var snip sql.NullString
		var score float64
		h, err := scanHit(rows, &snip, &score)
		if err != nil {
			return nil, 0, nil, err
		}
		h.Snippet, h.score = markSnippet(snip.String), score
		hits = append(hits, h)
	}
	return hits, total, facets, rows.Err()
}

func (ix *searchIndex) facets(from string, args []any) (int, map[string]map[string]int, error) {
	out := map[string]map[string]int{"kind": {}, "agent": {}, "repo": {}, "tool": {}}
	rows, err := ix.r.Query("SELECT d.kind, d.agent, COALESCE(d.repo,''), COALESCE(d.tool,''), COUNT(*) "+from+" GROUP BY 1,2,3,4", args...)
	if err != nil {
		return 0, out, err
	}
	defer rows.Close()
	total := 0
	for rows.Next() {
		var k, a, r, tl string
		var n int
		if rows.Scan(&k, &a, &r, &tl, &n) != nil {
			continue
		}
		total += n
		out["kind"][k] += n
		out["agent"][a] += n
		if r != "" {
			out["repo"][r] += n
		}
		if tl != "" {
			out["tool"][tl] += n
		}
	}
	return total, out, rows.Err()
}

func (ix *searchIndex) searchFilter(res *searchResult, f sqlFilter, limit, offset int) error {
	from := "FROM docs d WHERE " + f.sql()
	total, facets, err := ix.facets(from, f.args)
	if err != nil {
		return err
	}
	res.Total, res.Facets = total, facets
	rows, err := ix.r.Query("SELECT "+hitCols+", substr(d.text, 1, 400) "+from+" ORDER BY d.ts DESC LIMIT ? OFFSET ?", append(f.args, limit, offset)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var text sql.NullString
		h, err := scanHit(rows, &text)
		if err != nil {
			return err
		}
		h.Snippet = html.EscapeString(clipString(strings.Join(strings.Fields(text.String), " "), 240))
		res.Hits = append(res.Hits, h)
	}
	return rows.Err()
}

func (ix *searchIndex) searchLike(res *searchResult, pq parsedQuery, f sqlFilter, limit, offset int) error {
	var words []string
	for _, t := range pq.terms {
		f.add("(d.text LIKE ? OR d.path LIKE ?)", "%"+t.Text+"%", "%"+t.Text+"%")
		words = append(words, t.Text)
	}
	from := "FROM docs d WHERE " + f.sql()
	total, facets, err := ix.facets(from, f.args)
	if err != nil {
		return err
	}
	res.Total, res.Facets = total, facets
	rows, err := ix.r.Query("SELECT "+hitCols+", d.text "+from+" ORDER BY d.ts DESC LIMIT ? OFFSET ?", append(f.args, limit, offset)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var text sql.NullString
		h, err := scanHit(rows, &text)
		if err != nil {
			return err
		}
		h.Snippet = makeSnippet(text.String, words, 80)
		res.Hits = append(res.Hits, h)
	}
	return rows.Err()
}

// markSnippet escapes HTML and turns the \x01 \x02 sentinels into <mark> tags.
func markSnippet(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	s = html.EscapeString(s)
	s = strings.ReplaceAll(s, "\x01", "<mark>")
	return strings.ReplaceAll(s, "\x02", "</mark>")
}

// makeSnippet cuts a window around the first matched word and marks every match.
func makeSnippet(text string, words []string, radius int) string {
	text = strings.Join(strings.Fields(text), " ")
	lower := strings.ToLower(text)
	first := -1
	for _, w := range words {
		if i := strings.Index(lower, strings.ToLower(w)); i >= 0 && (first < 0 || i < first) {
			first = i
		}
	}
	if first < 0 {
		return html.EscapeString(clipString(text, 2*radius))
	}
	a, b := max(0, first-radius), min(len(text), first+radius*2)
	for a > 0 && text[a]&0xC0 == 0x80 {
		a--
	}
	for b < len(text) && text[b]&0xC0 == 0x80 {
		b++
	}
	win := text[a:b]
	lw := strings.ToLower(win)
	type rng struct{ s, e int }
	var rs []rng
	for _, w := range words {
		lwq := strings.ToLower(w)
		if lwq == "" {
			continue
		}
		for i := 0; ; {
			j := strings.Index(lw[i:], lwq)
			if j < 0 {
				break
			}
			rs = append(rs, rng{i + j, i + j + len(lwq)})
			i += j + len(lwq)
		}
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].s < rs[j].s })
	var bld strings.Builder
	if a > 0 {
		bld.WriteString("…")
	}
	pos := 0
	for _, r := range rs {
		if r.s < pos {
			continue
		}
		bld.WriteString(html.EscapeString(win[pos:r.s]))
		bld.WriteString("<mark>" + html.EscapeString(win[r.s:r.e]) + "</mark>")
		pos = r.e
	}
	bld.WriteString(html.EscapeString(win[pos:]))
	if b < len(text) {
		bld.WriteString("…")
	}
	return bld.String()
}
