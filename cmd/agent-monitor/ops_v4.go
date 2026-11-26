package main

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
	"weak"
)

// ── v4 thread fields ──

type Shipped struct {
	Kind  string `json:"kind"` // commit | pr | push | page | merge
	Label string `json:"label"`
	Ref   string `json:"ref,omitempty"`
	Ts    int64  `json:"ts"`
}

type V4Fields struct {
	Display     string    `json:"display"`
	Repo        string    `json:"repo"`
	OutcomeLine string    `json:"outcomeLine,omitempty"`
	Shipped     []Shipped `json:"shipped,omitempty"`
	ErrorsOpen  int       `json:"errorsOpen"`
	Activity    []int     `json:"activity,omitempty"`
	ActivityErr []int     `json:"activityErr,omitempty"`
	StackID     string    `json:"stackId,omitempty"`
}

type Stack struct {
	ID        string   `json:"id"`
	Parent    string   `json:"parent"`
	Agent     string   `json:"agent"`
	Kind      string   `json:"kind"`
	Label     string   `json:"label"`
	Repo      string   `json:"repo"`
	Total     int      `json:"total"`
	Running   int      `json:"running"`
	Waiting   int      `json:"waiting"`
	Done      int      `json:"done"`
	Failed    int      `json:"failed"`
	Abandoned int      `json:"abandoned"`
	Findings  int      `json:"findings"`
	LastAt    int64    `json:"lastAt"`
	Members   []string `json:"members"`
}

const (
	bucketMs    = 5 * 60_000
	maxShipped  = 12
	activityLen = 12
)

func threadsResponse(all, driven bool) map[string]any {
	v := loadThreads()
	out := threadsV3All(v)
	if !driven {
		out = withoutDriven(out)
	}
	now := time.Now().UnixMilli()
	out = append(out, drivenRowsV3(now)...)
	stacks := enrichV4(out, v, now)
	if !all {
		top := out[:0:0]
		for _, t := range out {
			if t.ParentThread == "" {
				top = append(top, t)
			}
		}
		out = top
	}
	return map[string]any{"threads": out, "threadOf": v.threadOf, "stacks": stacks}
}

// enrichV4 fills the v4 fields on every thread and returns the stacks among them.
func enrichV4(out []ThreadV3, v *threadView, now int64) []Stack {
	hourAgo := alignUp(now) - activityLen*bucketMs
	for i := range out {
		t := &out[i]
		t.Repo = repoBase(t.Cwd)
		t.Display = memo("d\x00"+t.Title+"\x00"+t.Repo+"\x00"+firstPromptOf(v, t.Sessions), func(k string) string {
			f := strings.SplitN(k, "\x00", 4)
			return displayTitle(f[1], f[2], f[3])
		})
		t.OutcomeLine = memo("o\x00"+firstOf(t.LastOutcome, t.LastPoint), func(k string) string { return outcomeLine(k[2:]) })
		if t.Kind == "driven" {
			t.ErrorsOpen = t.Errors
			continue
		}
		var ships []Shipped
		traceErrs, open := 0, 0
		active := t.LastAt >= hourAgo
		if active {
			t.Activity, t.ActivityErr = make([]int, activityLen), make([]int, activityLen)
		}
		for _, sid := range t.Sessions {
			d := derivedFor(sid)
			if d == nil {
				continue
			}
			ships = append(ships, d.shipped...)
			traceErrs += d.errors
			open += d.errorsOpen
			if s := v.byID[sid]; active && s != nil && s.LastActivityAt >= hourAgo {
				addActivityMarks(d.calls, hourAgo, t.Activity, t.ActivityErr)
			}
		}
		// Errors from sessions without a cached trace count as open.
		t.ErrorsOpen = max(open+t.Errors-traceErrs, open, 0)
		t.Shipped = mergeShipped(ships)
	}
	items := make([]stackItem, 0)
	for _, t := range out {
		if t.ParentThread == "" {
			continue
		}
		agent := threadAgent(v, t.Sessions)
		if t.Kind == "driven" && len(t.Tools) > 0 {
			agent = t.Tools[0]
		}
		items = append(items, stackItem{ID: t.ID, Parent: t.ParentThread, Agent: agent, ChildKind: t.ChildKind,
			Display: t.Display, Repo: t.Repo, Outcome: firstOf(t.LastOutcome, t.LastPoint), State: t.State,
			Waiting: t.Attention, ErrorsOpen: t.ErrorsOpen, LastAt: t.LastAt, StartedAt: t.StartedAt})
	}
	stacks, of := computeStacks(items)
	for i := range out {
		out[i].StackID = of[out[i].ID]
	}
	return stacks
}

var memoCache = struct {
	sync.Mutex
	m map[string]string
}{m: map[string]string{}}

// memo caches pure string derivations; the map is dropped when it grows past 20k keys.
func memo(key string, f func(string) string) string {
	memoCache.Lock()
	v, ok := memoCache.m[key]
	memoCache.Unlock()
	if ok {
		return v
	}
	v = f(key)
	memoCache.Lock()
	if len(memoCache.m) > 20_000 {
		memoCache.m = map[string]string{}
	}
	memoCache.m[key] = v
	memoCache.Unlock()
	return v
}

func threadAgent(v *threadView, sessions []string) string {
	if len(sessions) == 0 {
		return "claude"
	}
	return agentOf(v.byID[sessions[0]])
}

func firstPromptOf(v *threadView, sessions []string) string {
	for _, sid := range sessions {
		if s := v.byID[sid]; s != nil {
			if p := claudePromptText(s.FirstMessage); p != "" {
				return p
			}
		}
	}
	return ""
}

func alignUp(ts int64) int64 {
	return (ts + bucketMs - 1) / bucketMs * bucketMs
}

func addActivity(spans []Span, from int64, calls, errs []int) {
	addActivityMarks(callMarks(spans), from, calls, errs)
}

func addActivityMarks(marks []callMark, from int64, calls, errs []int) {
	for _, m := range marks {
		if m.ts < from {
			continue
		}
		i := int((m.ts - from) / bucketMs)
		if i >= len(calls) {
			continue
		}
		calls[i]++
		if m.err {
			errs[i]++
		}
	}
}

// callMark is a tool or agent span reduced to what activity and error rates read.
type callMark struct {
	ts  int64
	err bool
}

func callMarks(spans []Span) []callMark {
	var out []callMark
	for _, sp := range spans {
		if sp.Kind == "tool" || sp.Kind == "agent" {
			out = append(out, callMark{ts: sp.Ts, err: sp.Err})
		}
	}
	return out
}

type segCost struct {
	from, to int64
	usd      float64
}

// ── per-session derived values, cached per trace snapshot ──

// sessDerived outlives an evicted trace, so list views keep their values without a reparse.
type sessDerived struct {
	src        weak.Pointer[SessionTrace] // the trace it was derived from
	shipped    []Shipped
	errors     int
	errorsOpen int
	calls      []callMark
	segs       []segCost
}

var derivedCache = struct {
	sync.Mutex
	m map[string]*sessDerived
}{m: map[string]*sessDerived{}}

// peekTrace returns the cached trace without touching the transcript file.
func peekTrace(id string) *SessionTrace {
	traceCache.mu.Lock()
	defer traceCache.mu.Unlock()
	if e := traceCache.m[id]; e != nil {
		return e.trace
	}
	return nil
}

// derivedFor returns values derived from the newest trace built for id, also after that trace left the cache.
func derivedFor(id string) *sessDerived {
	tr := peekTrace(id)
	derivedCache.Lock()
	d := derivedCache.m[id]
	derivedCache.Unlock()
	if tr == nil || d != nil && d.src == weak.Make(tr) {
		return d
	}
	return deriveTrace(id, tr)
}

func deriveTrace(id string, tr *SessionTrace) *sessDerived {
	d := &sessDerived{src: weak.Make(tr), shipped: traceShipped(tr), calls: callMarks(tr.Spans)}
	d.errors, d.errorsOpen = countErrors(tr.Spans)
	for _, seg := range tr.Segments {
		d.segs = append(d.segs, segCost{from: seg.FromTs, to: seg.ToTs, usd: seg.UsdEst})
	}
	derivedCache.Lock()
	derivedCache.m[id] = d
	derivedCache.Unlock()
	return d
}

// countErrors returns all failed tool calls and those not followed by a success of the same tool in the same turn.
func countErrors(spans []Span) (total, open int) {
	type key struct{ parent, name string }
	lastOK := map[key]int64{}
	for _, sp := range spans {
		if (sp.Kind == "tool" || sp.Kind == "agent") && !sp.Err {
			k := key{sp.Parent, sp.Name}
			if t, ok := lastOK[k]; !ok || sp.Ts > t {
				lastOK[k] = sp.Ts
			}
		}
	}
	for _, sp := range spans {
		if !sp.Err || sp.Kind != "tool" && sp.Kind != "agent" {
			continue
		}
		total++
		if t, ok := lastOK[key{sp.Parent, sp.Name}]; !ok || t <= sp.Ts {
			open++
		}
	}
	return total, open
}

var reShipHint = regexp.MustCompile(`git|gh\s|host\.sh|doc\.sh|publish\.sh`)

func traceShipped(tr *SessionTrace) []Shipped {
	var out []Shipped
	for _, sp := range tr.Spans {
		if sp.Fam != "bash" {
			continue
		}
		io := tr.io.get(sp.ID)
		if !reShipHint.MatchString(io.Args) && !reShipHint.MatchString(sp.Res) {
			continue
		}
		cmd := commandFromArgs(io.Args)
		if cmd == "" {
			cmd = sp.Res
		}
		out = append(out, extractShipped(cmd, io.Result, sp.Err, sp.Ts)...)
	}
	for _, o := range tr.Outputs {
		switch o.Kind {
		case "pr":
			out = append(out, Shipped{Kind: "pr", Label: o.Label, Ref: o.Ref, Ts: o.Ts})
		case "artifact":
			out = append(out, Shipped{Kind: "page", Label: o.Label, Ref: o.Ref, Ts: o.Ts})
		}
	}
	return out
}

// commandFromArgs reads the shell command from Claude ({"command":"..."}) or Codex ({"cmd"|"command": str|[]}) args.
func commandFromArgs(args string) string {
	var obj map[string]any
	if json.Unmarshal([]byte(args), &obj) != nil {
		return ""
	}
	v, ok := obj["command"]
	if !ok {
		v = obj["cmd"]
	}
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, p := range c {
			if s, ok := p.(string); ok {
				parts = append(parts, s)
			}
		}
		if n := len(parts); n >= 3 && (parts[n-2] == "-lc" || parts[n-2] == "-c") {
			return parts[n-1]
		}
		return strings.Join(parts, " ")
	}
	return ""
}

func shipKey(s Shipped) string {
	switch {
	case s.Kind == "pr" && s.Ref != "":
		return "pr|" + s.Ref
	case s.Kind == "commit" && s.Ref != "":
		return "commit|" + s.Ref
	}
	return s.Kind + "|" + s.Label
}

// mergeShipped dedupes (first sighting wins) and returns newest first, at most maxShipped.
func mergeShipped(in []Shipped) []Shipped {
	if len(in) == 0 {
		return nil
	}
	sort.SliceStable(in, func(i, j int) bool { return in[i].Ts < in[j].Ts })
	seen := map[string]bool{}
	out := make([]Shipped, 0, len(in))
	for _, s := range in {
		k := shipKey(s)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, s)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if len(out) > maxShipped {
		out = out[:maxShipped]
	}
	return out
}

// ── shipped extraction from one shell command ──

var (
	reCommitLine = regexp.MustCompile(`(?m)^\[([^\]\s]+)(?: \(root-commit\))? ([0-9a-f]{7,40})\] (.+)$`)
	rePushRef    = regexp.MustCompile(`(?m)^\s*(?:[+*!=-]\s+)?(?:\[[^\]]+\]|[0-9a-f]{7,40}\.\.\.?[0-9a-f]{7,40})\s+(\S+)\s+->\s+(\S+)`)
	rePRURL      = regexp.MustCompile(`https://\S+/pull/(\d+)`)
	reURL        = regexp.MustCompile(`https?://[^\s"'<>)]+`)
	rePRNum      = regexp.MustCompile(`^#?(\d+)$`)
)

// extractShipped finds commits, PRs, merges, pushes and published pages in one command.
func extractShipped(cmd, result string, failed bool, ts int64) []Shipped {
	var out []Shipped
	for _, words := range splitCommands(cmd) {
		words = stripPrefixWords(words)
		if len(words) == 0 {
			continue
		}
		head := filepath.Base(words[0])
		if (head == "bash" || head == "sh" || head == "zsh") && len(words) > 1 {
			words, head = words[1:], filepath.Base(words[1])
		}
		switch head {
		case "git":
			sub, rest := gitSub(words[1:])
			switch sub {
			case "commit":
				if s, ok := commitShip(rest, result, failed, ts); ok {
					out = append(out, s)
				}
			case "push":
				if s, ok := pushShip(rest, result, failed, ts); ok {
					out = append(out, s)
				}
			}
		case "gh":
			if s, ok := ghShip(words[1:], result, failed, ts); ok {
				out = append(out, s)
			}
		case "host.sh", "doc.sh", "publish.sh":
			if s, ok := pageShip(words[1:], result, failed, ts); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func stripPrefixWords(w []string) []string {
	for len(w) > 0 {
		switch {
		case w[0] == "command" || w[0] == "sudo" || w[0] == "env" || w[0] == "time" || w[0] == "exec" || w[0] == "noglob":
			w = w[1:]
		case strings.Contains(w[0], "=") && !strings.HasPrefix(w[0], "-") && !strings.HasPrefix(w[0], "/"):
			w = w[1:]
		default:
			return w
		}
	}
	return w
}

// gitSub skips git's global options and returns the subcommand and its args.
func gitSub(w []string) (string, []string) {
	for i := 0; i < len(w); i++ {
		switch a := w[i]; {
		case a == "-C" || a == "-c" || a == "--git-dir" || a == "--work-tree" || a == "--namespace":
			i++
		case strings.HasPrefix(a, "-"):
		default:
			return a, w[i+1:]
		}
	}
	return "", nil
}

func commitShip(args []string, result string, failed bool, ts int64) (Shipped, bool) {
	m := reCommitLine.FindStringSubmatch(result)
	if failed && m == nil {
		return Shipped{}, false
	}
	if m == nil && (strings.Contains(result, "nothing to commit") || strings.Contains(result, "no changes added")) {
		return Shipped{}, false
	}
	subject := ""
	for i := 0; i < len(args) && subject == ""; i++ {
		a := args[i]
		switch {
		case a == "-m" || a == "--message" || (len(a) > 2 && a[0] == '-' && a[1] != '-' && strings.HasSuffix(a, "m")):
			if i+1 < len(args) {
				subject = messageSubject(args[i+1])
				i++
			}
		case strings.HasPrefix(a, "--message="):
			subject = messageSubject(strings.TrimPrefix(a, "--message="))
		case a == "-F" || a == "--file":
			if i+1 < len(args) && (args[i+1] == "-" || args[i+1] == "/dev/stdin") {
				for _, h := range args {
					if strings.HasPrefix(h, heredocMark) {
						subject = firstNonEmptyLine(strings.TrimPrefix(h, heredocMark))
					}
				}
			}
			i++
		}
	}
	s := Shipped{Kind: "commit", Ts: ts}
	if m != nil {
		s.Ref = m[2]
		if subject == "" {
			subject = strings.TrimSpace(m[3])
		}
	}
	s.Label = clipWords(firstOf(firstOf(subject, s.Ref), "untitled commit"), 120)
	return s, true
}

// messageSubject returns the first line of a -m value, reading a $(cat <<EOF ...) heredoc when present.
func messageSubject(v string) string {
	if strings.HasPrefix(v, heredocMark) {
		return firstNonEmptyLine(strings.TrimPrefix(v, heredocMark))
	}
	if strings.HasPrefix(v, "$(") {
		i := strings.Index(v, "<<")
		if i < 0 {
			return ""
		}
		d, end := heredocDelim(v, i+2)
		nl := strings.IndexByte(v[end:], '\n')
		if d == "" || nl < 0 {
			return ""
		}
		body, _ := heredocBody(v, end+nl+1, d)
		return firstNonEmptyLine(body)
	}
	return firstNonEmptyLine(v)
}

func positionals(args []string, valued map[string]bool) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case strings.HasPrefix(a, heredocMark):
		case a == ">" || a == ">>" || a == "<" || a == "2>" || a == "&>":
			i++
		case strings.ContainsAny(a, "<>"):
		case strings.HasPrefix(a, "-"):
			if valued[a] {
				i++
			}
		default:
			out = append(out, a)
		}
	}
	return out
}

func pushShip(args []string, result string, failed bool, ts int64) (Shipped, bool) {
	if failed {
		return Shipped{}, false
	}
	ref := rePushRef.FindStringSubmatch(result)
	if ref == nil && strings.Contains(result, "Everything up-to-date") {
		return Shipped{}, false
	}
	pos := positionals(args, map[string]bool{"-o": true, "--push-option": true, "--repo": true, "--receive-pack": true})
	remote, branch := "origin", ""
	if len(pos) > 0 {
		remote = pos[0]
	}
	if len(pos) > 1 {
		b := pos[1]
		if i := strings.LastIndexByte(b, ':'); i >= 0 {
			b = b[i+1:]
		}
		b = strings.TrimPrefix(strings.TrimPrefix(b, "+"), "refs/heads/")
		if b != "HEAD" {
			branch = b
		}
	}
	if branch == "" && ref != nil {
		branch = strings.TrimPrefix(ref[2], "refs/heads/")
	}
	label := remote
	if branch != "" {
		label = remote + "/" + branch
	}
	return Shipped{Kind: "push", Label: label, Ts: ts}, true
}

func ghShip(w []string, result string, failed bool, ts int64) (Shipped, bool) {
	for len(w) > 0 && strings.HasPrefix(w[0], "-") {
		if w[0] == "-R" || w[0] == "--repo" {
			w = w[1:]
		}
		w = w[1:]
	}
	if len(w) < 2 || w[0] != "pr" {
		return Shipped{}, false
	}
	url := rePRURL.FindStringSubmatch(result)
	if failed && url == nil {
		return Shipped{}, false
	}
	args := w[2:]
	switch w[1] {
	case "create":
		title := ""
		for i := 0; i < len(args); i++ {
			switch a := args[i]; {
			case (a == "--title" || a == "-t") && i+1 < len(args):
				title = args[i+1]
				i++
			case strings.HasPrefix(a, "--title="):
				title = strings.TrimPrefix(a, "--title=")
			}
		}
		s := Shipped{Kind: "pr", Ts: ts}
		if url != nil {
			s.Ref = url[0]
		}
		label := firstNonEmptyLine(title)
		if label == "" && url != nil {
			label = "PR #" + url[1]
		}
		s.Label = clipWords(firstOf(label, "pull request"), 120)
		return s, true
	case "merge":
		pos := positionals(args, map[string]bool{"-t": true, "--subject": true, "-b": true, "--body": true, "-F": true, "--body-file": true,
			"-A": true, "--author-email": true, "--match-head-commit": true, "-R": true, "--repo": true})
		s := Shipped{Kind: "merge", Label: "current PR", Ts: ts}
		if len(pos) > 0 {
			p := pos[0]
			switch {
			case rePRNum.MatchString(p):
				s.Label = "PR #" + strings.TrimPrefix(p, "#")
			case rePRURL.MatchString(p):
				s.Label, s.Ref = "PR #"+rePRURL.FindStringSubmatch(p)[1], p
			default:
				s.Label = p
			}
		}
		return s, true
	}
	return Shipped{}, false
}

func pageShip(args []string, result string, failed bool, ts int64) (Shipped, bool) {
	link := reURL.FindString(result)
	if failed && link == "" {
		return Shipped{}, false
	}
	slug := ""
	for i := 0; i < len(args); i++ {
		if (args[i] == "--slug") && i+1 < len(args) {
			slug = args[i+1]
		} else if strings.HasPrefix(args[i], "--slug=") {
			slug = strings.TrimPrefix(args[i], "--slug=")
		}
	}
	pos := positionals(args, map[string]bool{"--share": true, "--title": true, "--desc": true, "--description": true, "--icon": true, "--slug": true})
	switch {
	case slug != "":
	case len(pos) > 1:
		slug = pos[1]
	case len(pos) == 1:
		slug = strings.TrimSuffix(filepath.Base(pos[0]), filepath.Ext(pos[0]))
	default:
		return Shipped{}, false
	}
	return Shipped{Kind: "page", Label: slug, Ref: link, Ts: ts}, true
}

// ── a small shell tokenizer: quotes, $(...) and heredocs ──

const heredocMark = "\x00heredoc:"

// splitCommands splits a command line into simple commands at unquoted ; & | ( ) and newlines.
func splitCommands(s string) [][]string {
	var cmds [][]string
	var words []string
	var cur strings.Builder
	inWord := false
	pending := ""
	flushWord := func() {
		if inWord {
			words = append(words, cur.String())
			cur.Reset()
			inWord = false
		}
	}
	flushCmd := func() {
		flushWord()
		if len(words) > 0 {
			cmds = append(cmds, words)
		}
		words = nil
	}
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s):
			if s[i+1] != '\n' {
				cur.WriteByte(s[i+1])
				inWord = true
			}
			i += 2
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				cur.WriteString(s[i+1:])
				i = len(s)
			} else {
				cur.WriteString(s[i+1 : i+1+j])
				i += j + 2
			}
			inWord = true
		case c == '"':
			end, txt := readDouble(s, i+1)
			cur.WriteString(txt)
			inWord = true
			i = end
		case c == '$' && i+1 < len(s) && s[i+1] == '(':
			end := skipSubst(s, i+2)
			cur.WriteString(s[i:end])
			inWord = true
			i = end
		case c == '<' && strings.HasPrefix(s[i:], "<<") && !strings.HasPrefix(s[i:], "<<<"):
			flushWord()
			pending, i = heredocDelim(s, i+2)
		case c == ' ' || c == '\t' || c == '\r':
			flushWord()
			i++
		case c == '\n':
			flushWord()
			i++
			if pending != "" {
				body, end := heredocBody(s, i, pending)
				words = append(words, heredocMark+body)
				pending, i = "", end
			}
			flushCmd()
		case c == ';' || c == '&' || c == '|' || c == '(' || c == ')':
			flushCmd()
			i++
		case c == '#' && !inWord:
			if j := strings.IndexByte(s[i:], '\n'); j < 0 {
				i = len(s)
			} else {
				i += j
			}
		default:
			cur.WriteByte(c)
			inWord = true
			i++
		}
	}
	flushCmd()
	return cmds
}

// readDouble reads a double-quoted string starting after the opening quote.
func readDouble(s string, i int) (int, string) {
	var b strings.Builder
	for i < len(s) {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s):
			switch s[i+1] {
			case '"', '\\', '$', '`':
				b.WriteByte(s[i+1])
			case '\n':
			default:
				b.WriteByte('\\')
				b.WriteByte(s[i+1])
			}
			i += 2
		case c == '"':
			return i + 1, b.String()
		case c == '$' && i+1 < len(s) && s[i+1] == '(':
			end := skipSubst(s, i+2)
			b.WriteString(s[i:end])
			i = end
		default:
			b.WriteByte(c)
			i++
		}
	}
	return i, b.String()
}

// skipSubst returns the index just past the ) that closes a $( opened before i.
func skipSubst(s string, i int) int {
	depth, pending := 1, ""
	for i < len(s) {
		switch c := s[i]; {
		case c == '\\':
			i += 2
		case c == '\'':
			if j := strings.IndexByte(s[i+1:], '\''); j < 0 {
				return len(s)
			} else {
				i += j + 2
			}
		case c == '"':
			i, _ = readDouble(s, i+1)
		case c == '<' && strings.HasPrefix(s[i:], "<<") && !strings.HasPrefix(s[i:], "<<<"):
			pending, i = heredocDelim(s, i+2)
		case c == '\n' && pending != "":
			_, i = heredocBody(s, i+1, pending)
			pending = ""
		case c == '(':
			depth++
			i++
		case c == ')':
			depth--
			i++
			if depth == 0 {
				return i
			}
		default:
			i++
		}
	}
	return len(s)
}

func heredocDelim(s string, i int) (string, int) {
	if i < len(s) && s[i] == '-' {
		i++
	}
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	if i < len(s) && (s[i] == '\'' || s[i] == '"') {
		q := s[i]
		if j := strings.IndexByte(s[i+1:], q); j >= 0 {
			return s[i+1 : i+1+j], i + j + 2
		}
	}
	j := i
	for j < len(s) && (s[j] == '_' || s[j] == '-' || s[j] < utf8.RuneSelf && (unicode.IsLetter(rune(s[j])) || unicode.IsDigit(rune(s[j])))) {
		j++
	}
	return s[i:j], j
}

// heredocBody reads lines from i up to the delimiter line; returns the body and the index of that line's end.
func heredocBody(s string, i int, delim string) (string, int) {
	start := i
	for i <= len(s) {
		end := strings.IndexByte(s[i:], '\n')
		line := s[i:]
		if end >= 0 {
			line = s[i : i+end]
		}
		if strings.TrimSpace(line) == delim {
			return s[start:i], i + len(line)
		}
		if end < 0 {
			break
		}
		i += end + 1
	}
	return s[start:], len(s)
}

// ── titles and outcome lines ──

var (
	reMdLink    = regexp.MustCompile(`!?\[([^\]]*)\](\([^)]*\))?`)
	reMdEmph    = regexp.MustCompile("\\*\\*|__|`+|~~")
	reMdStar    = regexp.MustCompile(`(^|\s)[*_]([^*_\s][^*_]*)[*_]`)
	reSentence  = regexp.MustCompile(`[.!?;](\s|$)`)
	reLeadMark  = regexp.MustCompile(`^(\s*(#{1,6}|>|[-*+]|\d+[.)]|▎|▏|│)\s+)+`)
	reTrailRepo = regexp.MustCompile(`(?i)\s+(in|of|for|on) (this|the|our|my) (repository|repo|codebase|project|worktree)\b.*$`)
)

func repoBase(cwd string) string {
	cwd = strings.TrimRight(cwd, "/")
	switch {
	case cwd == "":
		return ""
	case cwd == strings.TrimRight(homeDir(), "/"):
		return "home"
	}
	return filepath.Base(cwd)
}

// stripMarkdown turns one line of markdown into plain text.
func stripMarkdown(s string) string {
	s = reLeadMark.ReplaceAllString(s, "")
	s = reMdLink.ReplaceAllString(s, "$1")
	s = reMdEmph.ReplaceAllString(s, "")
	s = reMdStar.ReplaceAllString(s, "$1$2")
	return strings.Join(strings.Fields(s), " ")
}

func junkTitle(t, repo string) bool {
	t = strings.TrimSpace(t)
	if t == "" || strings.EqualFold(t, repo) || t == "thread" {
		return true
	}
	for _, p := range []string{"**", "[", "#", "|", "<", "`", ">", "/", "~/", "./", "{"} {
		if strings.HasPrefix(t, p) {
			return true
		}
	}
	w := strings.Fields(t)[0]
	return strings.Contains(w, "/") && !strings.Contains(w, "://")
}

// displayTitle keeps a real title and otherwise derives one from the first prompt (also when the title is a copy of it).
func displayTitle(title, repo, prompt string) string {
	copied := title != "" && strings.HasPrefix(firstNonEmptyLine(prompt), strings.TrimSpace(title))
	if !copied && !junkTitle(title, repo) {
		return strings.TrimSpace(title)
	}
	if d := titleFromPrompt(prompt); d != "" {
		return d
	}
	if c := stripMarkdown(firstNonEmptyLine(title)); c != "" && !strings.EqualFold(c, repo) {
		return clipWords(c, 60)
	}
	return firstOf(repo, "Untitled")
}

var (
	boilerplate = []string{"please ", "hey, ", "hey ", "hi, ", "hi ", "ok, ", "ok ", "okay, ", "okay ", "now, ", "now ", "so ",
		"can you ", "could you ", "would you ", "will you ", "i want you to ", "i need you to ", "i'd like you to ", "go ahead and ",
		"in this repository, ", "in this repo, ", "in this repository ", "in this repo ", "your task is to ", "the task is to ", "task: "}
	reviewObjects = map[string]bool{"the": true, "this": true, "that": true, "my": true, "our": true, "a": true, "an": true, "these": true,
		"those": true, "last": true, "latest": true, "current": true, "recent": true, "pr": true, "commit": true, "diff": true, "branch": true}
)

func titleFromPrompt(p string) string {
	line := ""
	heading := ""
	for _, ln := range strings.Split(p, "\n") {
		t := strings.TrimSpace(ln)
		if t == "" || strings.HasPrefix(t, "<") || strings.HasPrefix(t, "```") || strings.HasPrefix(t, "|") || strings.HasPrefix(t, "---") {
			continue
		}
		if strings.HasPrefix(t, "#") {
			if heading == "" {
				heading = t
			}
			continue
		}
		line = t
		break
	}
	line = stripMarkdown(firstOf(line, heading))
	if line == "" {
		return ""
	}
	sentences := splitSentences(line)
	for len(sentences) > 1 && strings.HasPrefix(strings.ToLower(sentences[0]), "you are ") && !strings.HasPrefix(strings.ToLower(sentences[0]), "you are reviewing ") {
		sentences = sentences[1:]
	}
	s := sentences[0]
	for changed := true; changed; {
		changed = false
		low := strings.ToLower(s)
		for _, b := range boilerplate {
			if strings.HasPrefix(low, b) && len(s) > len(b) {
				s, changed = strings.TrimSpace(s[len(b):]), true
				break
			}
		}
	}
	low := strings.ToLower(s)
	for _, p := range []string{"you are reviewing ", "reviewing ", "review "} {
		if strings.HasPrefix(low, p) {
			obj := strings.TrimSpace(s[len(p):])
			if f := strings.Fields(obj); len(f) > 0 && (p != "review " || reviewObjects[strings.ToLower(f[0])]) {
				s = "review of " + obj
			}
			break
		}
	}
	s = reTrailRepo.ReplaceAllString(s, "")
	s = strings.TrimRight(strings.TrimSpace(s), ".,:;!?")
	if s == "" {
		return ""
	}
	r, n := utf8.DecodeRuneInString(s)
	return clipWords(string(unicode.ToUpper(r))+s[n:], 60)
}

func splitSentences(s string) []string {
	var out []string
	for {
		loc := reSentence.FindStringIndex(s)
		if loc == nil {
			break
		}
		if part := strings.TrimSpace(s[:loc[0]]); part != "" {
			out = append(out, part)
		}
		s = s[loc[1]:]
	}
	if t := strings.TrimSpace(s); t != "" {
		out = append(out, t)
	}
	if len(out) == 0 {
		out = []string{""}
	}
	return out
}

// clipWords cuts s to at most n bytes at a word boundary, adding an ellipsis when cut.
func clipWords(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n-len("…")]
	if i := strings.LastIndexByte(cut, ' '); i > n/2 {
		cut = cut[:i]
	}
	for !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return strings.TrimRight(cut, " ,;:-") + "…"
}

// outcomeLine reduces a markdown reply to one plain sentence of at most 160 chars.
func outcomeLine(md string) string {
	var parts []string
	fence := false
	for _, ln := range strings.Split(md, "\n") {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "```") {
			fence = !fence
			continue
		}
		if fence || t == "" || strings.HasPrefix(t, "|") || strings.HasPrefix(t, "---") || strings.HasPrefix(t, "<") {
			continue
		}
		if t = stripMarkdown(t); t != "" {
			parts = append(parts, t)
		}
		if len(strings.Join(parts, " ")) > 200 {
			break
		}
	}
	text := strings.Join(parts, " ")
	if text == "" {
		return ""
	}
	out := ""
	for _, s := range splitSentences(text) {
		if out != "" {
			out += " "
		}
		out += strings.TrimRight(s, " ") + "."
		if len(out) >= 40 {
			break
		}
	}
	out = strings.ReplaceAll(out, "..", ".")
	return clipWords(out, 160)
}

// ── stacks ──

type stackItem struct {
	ID, Parent, Agent, ChildKind, Display, Repo, Outcome string
	State                                                State
	Waiting                                              bool
	ErrorsOpen                                           int
	LastAt, StartedAt                                    int64
}

var (
	reFindings   = regexp.MustCompile(`(?i)\b(findings?|defects?|p[12]|issues?)\b`)
	reNoFindings = regexp.MustCompile(`(?i)\bno (findings|defects|issues|problems)\b|\bclean\b`)
)

func verbClass(display string) string {
	f := strings.Fields(strings.ToLower(display))
	if len(f) == 0 {
		return "other"
	}
	w := strings.TrimFunc(f[0], func(r rune) bool { return !unicode.IsLetter(r) })
	switch {
	case strings.HasPrefix(w, "review"), strings.HasPrefix(w, "audit"):
		return "review"
	case strings.HasPrefix(w, "fix"), strings.HasPrefix(w, "repair"), strings.HasPrefix(w, "patch"):
		return "fix"
	case strings.HasPrefix(w, "test"), strings.HasPrefix(w, "verify"):
		return "test"
	}
	return "other"
}

func hasFindings(outcome string) bool {
	return reFindings.MatchString(outcome) && !reNoFindings.MatchString(outcome)
}

func stackLabel(n int, agent, kind, childKind string) string {
	noun := map[string]string{"review": "reviews", "fix": "fixes", "test": "test runs"}[kind]
	if noun == "" {
		noun = "runs"
		if childKind == "subagent" {
			noun = "subagents"
		}
	}
	name := agent
	if r, sz := utf8.DecodeRuneInString(agent); sz > 0 {
		name = string(unicode.ToUpper(r)) + agent[sz:]
	}
	return strconv.Itoa(n) + " " + name + " " + noun
}

// computeStacks groups children of one parent with the same agent and verb class (2 or more).
func computeStacks(items []stackItem) ([]Stack, map[string]string) {
	groups := map[string][]stackItem{}
	var order []string
	for _, it := range items {
		k := it.Parent + "|" + it.Agent + "|" + verbClass(it.Display)
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], it)
	}
	stacks := []Stack{}
	of := map[string]string{}
	for _, k := range order {
		g := groups[k]
		if len(g) < 2 {
			continue
		}
		sort.Slice(g, func(i, j int) bool { return g[i].StartedAt < g[j].StartedAt })
		kind := verbClass(g[0].Display)
		st := Stack{ID: "stack:" + g[0].Parent + ":" + g[0].Agent + ":" + kind, Parent: g[0].Parent, Agent: g[0].Agent, Kind: kind,
			Label: stackLabel(len(g), g[0].Agent, kind, g[0].ChildKind), Repo: g[0].Repo, Total: len(g), Members: make([]string, 0, len(g))}
		for _, it := range g {
			st.Members = append(st.Members, it.ID)
			of[it.ID] = st.ID
			st.LastAt = max(st.LastAt, it.LastAt)
			switch {
			case it.Waiting || it.State == StateAwaitingPermission || it.State == StateAwaitingInput:
				st.Waiting++
			case it.State == StateRunning:
				st.Running++
			case it.State == StateAbandoned:
				st.Abandoned++
			case it.ErrorsOpen > 0:
				st.Failed++
			default:
				st.Done++
			}
			if hasFindings(it.Outcome) {
				st.Findings++
			}
		}
		stacks = append(stacks, st)
	}
	sort.SliceStable(stacks, func(i, j int) bool { return stacks[i].LastAt > stacks[j].LastAt })
	return stacks, of
}

// ── timeline helpers ──

type DensityBucket struct {
	From   int64 `json:"from"`
	To     int64 `json:"to"`
	Calls  int   `json:"calls"`
	Errors int   `json:"errors"`
}

type LaneStack struct {
	Total   int `json:"total"`
	Done    int `json:"done"`
	Running int `json:"running"`
	Failed  int `json:"failed"`
}

// densityOf counts tool calls per 5-minute bucket in [from, to], keeping buckets with calls.
func densityOf(spans []Span, from, to int64) []DensityBucket {
	m := map[int64]*DensityBucket{}
	for _, sp := range spans {
		if sp.Kind != "tool" && sp.Kind != "agent" || sp.Ts < from || sp.Ts > to {
			continue
		}
		b := sp.Ts / bucketMs * bucketMs
		d := m[b]
		if d == nil {
			d = &DensityBucket{From: b, To: b + bucketMs}
			m[b] = d
		}
		d.Calls++
		if sp.Err {
			d.Errors++
		}
	}
	return sortedDensity(m)
}

func sortedDensity(m map[int64]*DensityBucket) []DensityBucket {
	out := make([]DensityBucket, 0, len(m))
	for _, d := range m {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].From < out[j].From })
	return out
}

func sumDensity(in []DensityBucket) []DensityBucket {
	m := map[int64]*DensityBucket{}
	for _, d := range in {
		if e := m[d.From]; e != nil {
			e.Calls += d.Calls
			e.Errors += d.Errors
		} else {
			c := d
			m[d.From] = &c
		}
	}
	return sortedDensity(m)
}

// unionBars merges overlapping bars of the same kind.
func unionBars(in []TimelineBar) []TimelineBar {
	sort.SliceStable(in, func(i, j int) bool {
		if in[i].Kind != in[j].Kind {
			return in[i].Kind < in[j].Kind
		}
		return in[i].From < in[j].From
	})
	var out []TimelineBar
	for _, b := range in {
		if n := len(out); n > 0 && out[n-1].Kind == b.Kind && b.From <= out[n-1].To {
			out[n-1].To = max(out[n-1].To, b.To)
			continue
		}
		out = append(out, b)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].From < out[j].From })
	return out
}

// collapseStacks replaces the lanes of stack members with one lane per stack.
func collapseStacks(lanes []TimelineLane, stacks []Stack) []TimelineLane {
	of := map[string]*Stack{}
	for i := range stacks {
		for _, m := range stacks[i].Members {
			of[m] = &stacks[i]
		}
	}
	out := make([]TimelineLane, 0, len(lanes))
	at := map[string]int{}
	for _, l := range lanes {
		st := of[l.ThreadID]
		if st == nil {
			out = append(out, l)
			continue
		}
		j, ok := at[st.ID]
		if !ok {
			state := StateCompleted
			switch {
			case st.Waiting > 0:
				state = StateAwaitingInput
			case st.Running > 0:
				state = StateRunning
			}
			j = len(out)
			at[st.ID] = j
			out = append(out, TimelineLane{ThreadID: st.ID, Kind: "stack", Title: st.Label, Display: st.Label, Agent: st.Agent, State: state,
				Parent: st.Parent, ChildKind: l.ChildKind, StartTurnTs: l.StartTurnTs, Members: st.Members,
				Stack: &LaneStack{Total: st.Total, Done: st.Done, Running: st.Running, Failed: st.Failed}})
		}
		agg := &out[j]
		agg.Errors += l.Errors
		agg.Bars = append(agg.Bars, l.Bars...)
		agg.Density = append(agg.Density, l.Density...)
	}
	for _, j := range at {
		out[j].Bars = unionBars(out[j].Bars)
		out[j].Density = sumDensity(out[j].Density)
	}
	return out
}

// ── /api/v3/now ──

type NowBar struct {
	From int64  `json:"from"`
	To   int64  `json:"to"`
	Kind string `json:"kind"` // work | wait | error
}

type NowLane struct {
	ThreadID string   `json:"threadId"`
	DrivenID string   `json:"drivenId,omitempty"`
	Display  string   `json:"display"`
	Agent    string   `json:"agent"`
	Bars     []NowBar `json:"bars"`
}

type NowResponse struct {
	NeedsYou      int       `json:"needsYou"`
	Running       int       `json:"running"`
	DoneToday     int       `json:"doneToday"`
	SpendToday    float64   `json:"spendToday"`
	ErrorsPerHour int       `json:"errorsPerHour"`
	Lanes         []NowLane `json:"lanes"`
	From          int64     `json:"from"`
	To            int64     `json:"to"`
}

const nowWindowMs = 3 * 3600_000

func buildNow(now int64) NowResponse {
	v := loadThreads()
	all := append(withoutDriven(threadsV3All(v)), drivenRowsV3(now)...)
	t0 := time.UnixMilli(now)
	today := time.Date(t0.Year(), t0.Month(), t0.Day(), 0, 0, 0, 0, time.Local).UnixMilli()
	res := NowResponse{From: now - nowWindowMs, To: now, Lanes: []NowLane{}}
	var recent []*ThreadV3
	for i := range all {
		t := &all[i]
		switch {
		case t.Attention:
			res.NeedsYou++
		case t.State == StateRunning:
			res.Running++
		case t.ParentThread == "" && t.LastAt >= today && t.State != StateAbandoned:
			res.DoneToday++
		}
		if t.ParentThread == "" && t.LastAt >= res.From {
			recent = append(recent, t)
		}
	}
	for _, s := range v.byID {
		if s.LastActivityAt < today {
			continue
		}
		d := derivedFor(s.ID)
		if d == nil {
			continue
		}
		for _, seg := range d.segs {
			span := seg.to - seg.from
			switch {
			case seg.to < today:
			case span <= 0 || seg.from >= today:
				res.SpendToday += seg.usd
			default:
				res.SpendToday += seg.usd * float64(seg.to-today) / float64(span)
			}
		}
		if s.LastActivityAt >= now-3600_000 {
			for _, c := range d.calls {
				if c.err && c.ts >= now-3600_000 {
					res.ErrorsPerHour++
				}
			}
		}
	}
	res.SpendToday += drivenNowSpend(v, today)
	res.SpendToday = float64(int64(res.SpendToday*100+0.5)) / 100
	dl, _ := drivenLanes(res.From, now, now)
	dbars := map[string][]TimelineBar{}
	for _, l := range dl {
		dbars[l.ThreadID] = l.Bars
	}
	sort.Slice(recent, func(i, j int) bool { return recent[i].LastAt > recent[j].LastAt })
	for _, t := range recent {
		if len(res.Lanes) == 8 {
			break
		}
		if t.Kind == "driven" {
			lane := NowLane{ThreadID: t.ID, DrivenID: t.ID, Display: t.Display, Agent: t.Tools[0], Bars: []NowBar{}}
			if lane.Display == "" {
				lane.Display = t.Title
			}
			for _, b := range dbars[t.ID] {
				lane.Bars = append(lane.Bars, NowBar{From: b.From, To: min(b.To, now), Kind: b.Kind})
			}
			res.Lanes = append(res.Lanes, lane)
			continue
		}
		lane := NowLane{ThreadID: t.ID, Display: displayTitle(t.Title, repoBase(t.Cwd), firstPromptOf(v, t.Sessions)), Agent: threadAgent(v, t.Sessions), Bars: []NowBar{}}
		for _, sid := range t.Sessions {
			s := v.byID[sid]
			if s == nil || s.LastActivityAt < res.From {
				continue
			}
			if tr := buildTrace(s); tr != nil {
				for _, b := range workBars(tr, agentOf(s), res.From, now) {
					if b.Kind != "idle" {
						lane.Bars = append(lane.Bars, NowBar{From: b.From, To: min(b.To, now), Kind: b.Kind})
					}
				}
			}
			if waitIntervalsFn != nil {
				for _, iv := range waitIntervalsFn(sid, res.From, now) {
					lane.Bars = append(lane.Bars, NowBar{From: iv[0], To: iv[1], Kind: "wait"})
				}
			}
		}
		sort.Slice(lane.Bars, func(i, j int) bool { return lane.Bars[i].From < lane.Bars[j].From })
		res.Lanes = append(res.Lanes, lane)
	}
	res.NeedsYou = len(needsYou()) // the one list every surface counts
	return res
}
