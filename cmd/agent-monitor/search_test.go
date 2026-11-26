package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func chipMap(pq parsedQuery) map[string]string {
	m := map[string]string{}
	for _, c := range pq.Chips {
		m[c.Key] = c.Value + "/" + c.Source
	}
	return m
}

func TestParseSearchQuery(t *testing.T) {
	repos := map[string]bool{"aah-film": true, "agent-monitor": true}
	cases := []struct {
		q     string
		chips map[string]string
		text  string
	}{
		{"codex aah-film stale conflict", map[string]string{"agent": "codex/inferred", "repo": "aah-film/inferred"}, "stale conflict"},
		{"failed go test last week", map[string]string{"is": "error/inferred", "since": "7d/inferred"}, "go test"},
		{"file:ThreadList.tsx", map[string]string{"file": "ThreadList.tsx/typed"}, ""},
		{"ThreadList.tsx render", map[string]string{"file": "ThreadList.tsx/inferred"}, "render"},
		{"app-stg-au", map[string]string{}, "app-stg-au"},
		{`agent:claude "exact phrase" foo`, map[string]string{"agent": "claude/typed"}, `"exact phrase" foo`},
		{"prompts yesterday about redis", map[string]string{"kind": "prompt/inferred", "since": "yesterday/inferred"}, "about redis"},
		{"web/src/App.tsx broken", map[string]string{"file": "web/src/App.tsx/inferred", "is": "error/inferred"}, ""},
		{"since:2026-11-08 repo:x is:error panic", map[string]string{"since": "2026-11-08/typed", "repo": "x/typed", "is": "error/typed"}, "panic"},
		{"version 1.5 notes", map[string]string{}, "version 1.5 notes"},
		{"commands on monday", map[string]string{"kind": "command/inferred", "since": "monday/inferred"}, "on"},
	}
	for _, c := range cases {
		pq := parseSearchQuery(c.q, repos)
		got := chipMap(pq)
		if len(got) != len(c.chips) {
			t.Errorf("%q: chips %v, want %v", c.q, got, c.chips)
			continue
		}
		for k, v := range c.chips {
			if got[k] != v {
				t.Errorf("%q: chip %s=%s, want %s", c.q, k, got[k], v)
			}
		}
		if pq.Text != c.text {
			t.Errorf("%q: text %q, want %q", c.q, pq.Text, c.text)
		}
	}
}

func TestFTSMatchQuoting(t *testing.T) {
	got := ftsMatch([]searchTerm{{Text: `a"b`}, {Text: "NEAR"}, {Text: "foo*"}})
	if want := `"a""b" "NEAR" "foo*"*`; got != want {
		t.Fatalf("ftsMatch = %s, want %s", got, want)
	}
	if got := ftsMatch([]searchTerm{{Text: "exact phrase", Phrase: true}}); got != `"exact phrase"` {
		t.Fatalf("phrase = %s", got)
	}
	if got := triMatch([]searchTerm{{Text: "ab"}, {Text: "my-app"}}); got != `"my-app"` {
		t.Fatalf("triMatch = %s", got)
	}
	if !identifierLike([]searchTerm{{Text: "ThreadList"}}) || identifierLike([]searchTerm{{Text: "stale"}, {Text: "conflict"}}) {
		t.Fatal("identifierLike")
	}
}

func TestResolveSince(t *testing.T) {
	now := time.Date(2026, 11, 11, 15, 30, 0, 0, time.Local) // a Wednesday
	day := time.Date(2026, 11, 11, 0, 0, 0, 0, time.Local)
	cases := map[string]time.Time{
		"today": day, "yesterday": day.AddDate(0, 0, -1), "mon": day.AddDate(0, 0, -2), "wed": day,
		"sunday": day.AddDate(0, 0, -3), "24h": now.Add(-24 * time.Hour), "7d": now.AddDate(0, 0, -7),
		"2026-11-08": time.Date(2026, 11, 8, 0, 0, 0, 0, time.Local),
	}
	for v, want := range cases {
		got, ok := resolveSince(v, now)
		if !ok || got != want.UnixMilli() {
			t.Errorf("resolveSince(%s) = %v, want %v", v, time.UnixMilli(got), want)
		}
	}
	if _, ok := resolveSince("soon", now); ok {
		t.Error("soon should not parse")
	}
}

func TestSnippetMarks(t *testing.T) {
	if got := markSnippet("x <b> \x01hit\x02 y"); got != "x &lt;b&gt; <mark>hit</mark> y" {
		t.Fatalf("markSnippet = %q", got)
	}
	got := makeSnippet("the go test failed in <pkg> and go test again", []string{"go test"}, 80)
	if strings.Count(got, "<mark>go test</mark>") != 2 || !strings.Contains(got, "&lt;pkg&gt;") {
		t.Fatalf("makeSnippet = %q", got)
	}
}

func TestErrorSignature(t *testing.T) {
	sig, sample := errorSignature("Exit code 1\nok   pkg/a 0.1s\n--- FAIL: TestFoo (0.02s)\n    foo_test.go:42: want 3")
	if sig != "--- FAIL: TestFoo (Ns)" {
		t.Errorf("sig = %q", sig)
	}
	if !strings.HasPrefix(sample, "--- FAIL: TestFoo") {
		t.Errorf("sample = %q", sample)
	}
	sig, _ = errorSignature(`Error: Cannot find module '/Users/x/app/a.js' at 0xdeadbeef line 42`)
	if sig != "Error: Cannot find module <q> at <hex> line N" {
		t.Errorf("sig = %q", sig)
	}
	sig, _ = errorSignature("panic: runtime error: index out of range [5] with length 3\n\ngoroutine 1 [running]:")
	if sig != "panic: runtime error: index out of range [N] with length N" {
		t.Errorf("sig = %q", sig)
	}
	sig, _ = errorSignature("cat: /tmp/nope/file.txt: No such file or directory\nexit code 1")
	if sig != "exit code N" {
		t.Errorf("sig = %q", sig)
	}
}

func TestResultLooksFailed(t *testing.T) {
	cases := []struct {
		fam  string
		code int
		text string
		want bool
	}{
		{"bash", -1, "Exit code 2\nboom", true},
		{"bash", -1, "Process exited with code 0\nOutput:", false},
		{"bash", -1, "Chunk ID: x\nWall time: 1s\nProcess exited with code 1\n", true},
		{"bash", 1, "", true},
		{"bash", 0, "error: but exit 0", false},
		{"bash", -1, "error: unknown flag", true},
		{"read", -1, "error: in a file body", false},
	}
	for _, c := range cases {
		if got := resultLooksFailed(c.fam, c.code, c.text); got != c.want {
			t.Errorf("resultLooksFailed(%q,%d,%q) = %v", c.fam, c.code, c.text, got)
		}
	}
}

func TestCommandKey(t *testing.T) {
	cases := map[string]string{
		"cd /x && git commit -m 'msg'":            "git commit",
		"go test ./... -run X":                    "go test <path>",
		"FOO=1 npm run build":                     "npm run build",
		"kubectl get pods -n x":                   "kubectl get pods",
		"sed -n '1,20p' file.go":                  "sed",
		"/usr/bin/python3 script.py | head":       "python3 script.py",
		"git show ac2617e4c096116d5458ac:main.tf": "git show <hash>:main.tf",
		"gh pr view 1234":                         "gh pr view",
		"curl -s https://x":                       "curl",
		"cd /tmp; for q in a b; do x; done":       "for q",
	}
	for in, want := range cases {
		if got := commandKey(in); got != want {
			t.Errorf("commandKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPercentileAndTrend(t *testing.T) {
	d := []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if percentile(d, 0.5) != 5 || percentile(d, 0.95) != 10 || percentile(d, 0) != 1 || percentile(nil, 0.5) != 0 {
		t.Fatalf("percentile wrong: %d %d", percentile(d, 0.5), percentile(d, 0.95))
	}
	if percentile([]int64{42}, 0.95) != 42 {
		t.Fatal("single")
	}
	now := time.Date(2026, 11, 11, 15, 0, 0, 0, time.Local)
	tr := dailyTrend([]int64{now.UnixMilli(), now.Add(-time.Hour).UnixMilli(), now.AddDate(0, 0, -1).UnixMilli(), now.AddDate(0, 0, -6).UnixMilli(), now.AddDate(0, 0, -9).UnixMilli()}, now)
	if tr[6] != 2 || tr[5] != 1 || tr[0] != 1 {
		t.Fatalf("trend = %v", tr)
	}
	b := histogram([]spanRow{{Dur: 5}, {Dur: 150, Err: true}, {Dur: 9_000_000}})
	if b[0].N != 1 || b[3].N != 1 || b[3].Failed != 1 || b[len(b)-1].N != 1 || b[len(b)-1].ToMs != -1 {
		t.Fatalf("histogram = %+v", b)
	}
}

func TestAskPrompt(t *testing.T) {
	hits := []searchHit{{ID: 7, Session: "claude:a", Agent: "claude", Repo: "r", Kind: "error", Tool: "Bash", Ts: 1}, {ID: 9, Session: "codex:b", Agent: "codex", Kind: "reply", Ts: 2}}
	p := buildAskPrompt("why did go test fail?", hits, map[int64]string{7: "FAIL  pkg  \n  boom", 9: "fixed it"})
	for _, want := range []string{"QUESTION: why did go test fail?", "[1] ", "claude · r · error · Bash", "FAIL pkg boom", "[2] ", "fixed it", "Cite every claim with [n]"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
	cs := askCitations("It failed [1]. Then fixed [2][1]. Bogus [5].", hits)
	if len(cs) != 2 || cs[0].HitID != 7 || cs[1].Session != "codex:b" {
		t.Fatalf("citations = %+v", cs)
	}
}

func TestGraph(t *testing.T) {
	rows := []spanRow{
		{Agent: "claude", Fam: "agent", Tool: "Agent", Dur: 100},
		{Agent: "claude", Fam: "bash", Tool: "Bash", Cmd: "cd x && codex exec 'do it'", Dur: 5000, Err: true},
		{Agent: "codex", Fam: "mcp", Tool: "mcp__agent-monitor__list_agents", Dur: 20},
		{Agent: "claude", Fam: "bash", Tool: "Bash", Cmd: "ls"},
	}
	nodes, edges := buildGraph(rows)
	ids := map[string]string{}
	for _, n := range nodes {
		ids[n.ID] = n.Kind
	}
	if ids["claude/subagent"] != "subagent" || ids["codex"] != "agent" || ids["delegated:codex"] != "delegated" || ids["mcp:agent-monitor"] != "mcp" {
		t.Fatalf("nodes = %+v", nodes)
	}
	found := false
	for _, e := range edges {
		if e.From == "claude" && e.To == "delegated:codex" && e.FailRate == 1 && e.P95 == 5000 {
			found = true
		}
	}
	if !found || len(edges) != 3 {
		t.Fatalf("edges = %+v", edges)
	}
}

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

func countRows(t *testing.T, ix *searchIndex, q string, args ...any) int {
	t.Helper()
	var n int
	if err := ix.w.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func newTestIndex(t *testing.T) (*searchIndex, string) {
	t.Helper()
	dir := t.TempDir()
	ix, err := openSearchIndex(filepath.Join(dir, "search.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ix.w.Close(); ix.r.Close() })
	ix.throttle = 0
	return ix, dir
}

func TestIncrementalClaudeIndex(t *testing.T) {
	ix, dir := newTestIndex(t)
	if !ix.fts5 {
		t.Log("FTS5 unavailable: LIKE fallback in use")
	}
	proj := filepath.Join(dir, "projects", "-tmp-demo")
	_ = os.MkdirAll(proj, 0o755)
	path := filepath.Join(proj, "11111111-2222-3333-4444-555555555555.jsonl")
	writeLines(t, path,
		`{"type":"api-request-blob","message":{"role":"user","content":[{"type":"text","text":"SHOULD NOT INDEX"}]}}`,
		`{"type":"user","cwd":"/tmp/demo","timestamp":"2026-11-10T10:00:00Z","message":{"role":"user","content":"please run the zebra tests<system-reminder>noise</system-reminder>"}}`,
		`{"type":"assistant","cwd":"/tmp/demo","timestamp":"2026-11-10T10:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":"Running the zebra suite now."},{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"go test ./zebra/..."}}]}}`,
		`{"type":"user","cwd":"/tmp/demo","timestamp":"2026-11-10T10:00:04Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","is_error":true,"content":"Exit code 1\n--- FAIL: TestZebra (0.01s)"}]}}`,
		`{"type":"user","isMeta":true,"timestamp":"2026-11-10T10:00:05Z","message":{"role":"user","content":"meta line"}}`,
	)
	if err := ix.indexFile(path, "claude"); err != nil {
		t.Fatal(err)
	}
	docs := countRows(t, ix, `SELECT COUNT(*) FROM docs`)
	if docs != 4 { // prompt, reply, command, error
		t.Fatalf("docs after first pass = %d, want 4", docs)
	}
	if n := countRows(t, ix, `SELECT COUNT(*) FROM docs WHERE text LIKE '%SHOULD NOT%' OR text LIKE '%noise%' OR text LIKE '%meta line%'`); n != 0 {
		t.Fatalf("noise indexed: %d", n)
	}
	var dur int64
	var errFlag int
	var sig, key, repo string
	if err := ix.w.QueryRow(`SELECT dur, err, errSig, cmdKey, repo FROM spans WHERE ref='toolu_1'`).Scan(&dur, &errFlag, &sig, &key, &repo); err != nil {
		t.Fatal(err)
	}
	if dur != 3000 || errFlag != 1 || sig != "--- FAIL: TestZebra (Ns)" || key != "go test <path>" || repo != "demo" {
		t.Fatalf("span dur=%d err=%d sig=%q key=%q repo=%q", dur, errFlag, sig, key, repo)
	}
	// Tool call in one batch, result in a later append.
	writeLines(t, path,
		`{"type":"assistant","cwd":"/tmp/demo","timestamp":"2026-11-10T10:01:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_2","name":"Edit","input":{"file_path":"/tmp/demo/web/ThreadList.tsx","old_string":"a","new_string":"zebraRow()"}}]}}`)
	if err := ix.indexFile(path, "claude"); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, ix, `SELECT COUNT(*) FROM docs`); got != docs+1 {
		t.Fatalf("docs after append = %d, want %d", got, docs+1)
	}
	writeLines(t, path,
		`{"type":"user","cwd":"/tmp/demo","timestamp":"2026-11-10T10:01:02Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_2","content":"ok"}]}}`)
	// A partial line (no newline yet) must wait.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString(`{"type":"user","cwd":"/tmp/demo","timestamp":"2026-11-10T10:02:00Z","message":{"role":"user","content":"partial`)
	f.Close()
	if err := ix.indexFile(path, "claude"); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, ix, `SELECT COUNT(*) FROM docs`); got != docs+2 {
		t.Fatalf("docs after result = %d, want %d", got, docs+2)
	}
	if d := countRows(t, ix, `SELECT dur FROM spans WHERE ref='toolu_2'`); d != 2000 {
		t.Fatalf("toolu_2 dur = %d", d)
	}
	// Re-index with no change adds nothing.
	if err := ix.indexFile(path, "claude"); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, ix, `SELECT COUNT(*) FROM docs`); got != docs+2 {
		t.Fatalf("docs after no-op = %d", got)
	}

	now := time.Date(2026, 11, 11, 0, 0, 0, 0, time.Local)
	res, err := ix.search(parseSearchQuery("zebra", nil), "demo", "", 10, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if res.Total < 3 || len(res.Hits) == 0 || !strings.Contains(res.Hits[0].Snippet, "<mark>") {
		t.Fatalf("search zebra: total=%d hits=%+v", res.Total, res.Hits)
	}
	if res.Facets["tool"]["Bash"] == 0 {
		t.Fatalf("tool facet: %v", res.Facets)
	}
	sid := "claude:11111111-2222-3333-4444-555555555555"
	if n := ix.promptCounts([]string{sid}); n[sid] != 1 {
		t.Fatalf("promptCounts %v", n)
	}
	res, err = ix.search(parseSearchQuery("file:ThreadList.tsx", nil), "", "", 10, 0, now)
	if err != nil || res.Total != 1 || res.Hits[0].Kind != "file" || res.Hits[0].Path != "/tmp/demo/web/ThreadList.tsx" {
		t.Fatalf("file chip: %v %+v", err, res)
	}
	res, err = ix.search(parseSearchQuery("is:error TestZebra", nil), "", "", 10, 0, now)
	if err != nil || res.Total != 1 || res.Hits[0].Kind != "error" || res.Hits[0].Tool != "Bash" {
		t.Fatalf("is:error: %v %+v", err, res)
	}
	doc, before, after, err := ix.context(res.Hits[0].ID, 2)
	if err != nil || doc.Kind != "error" || len(before) != 2 || len(after) != 2 {
		t.Fatalf("context: %v before=%d after=%d", err, len(before), len(after))
	}
}

func TestCodexItemIndex(t *testing.T) {
	ix, dir := newTestIndex(t)
	path := filepath.Join(dir, "rollout-2026-11-10T10-00-00-01a1df10-633b-7031-a610-56ef0beef44d.jsonl")
	writeLines(t, path,
		`{"timestamp":"2026-11-10T10:00:00Z","type":"session_meta","payload":{"id":"01a1df10-633b-7031-a610-56ef0beef44d","cwd":"/tmp/codexrepo"}}`,
		`{"timestamp":"2026-11-10T10:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"# AGENTS.md instructions\nignore"}]}}`,
		`{"timestamp":"2026-11-10T10:00:02Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"fix the stale conflict"}]}}`,
		`{"timestamp":"2026-11-10T10:00:02Z","type":"event_msg","payload":{"type":"user_message","message":"fix the stale conflict"}}`,
		`{"timestamp":"2026-11-10T10:00:03Z","type":"event_msg","payload":{"type":"item_completed","started_at_ms":1794300000000,"item":{"type":"CommandExecution","id":"exec-1","command":["/bin/zsh","-lc","npm test"],"cwd":"file:///tmp/codexrepo","exit_code":1,"aggregated_output":"FAIL src/a.test.ts\n  expected 1","duration":{"secs":2,"nanos":500000000}}}}`,
		`{"timestamp":"2026-11-10T10:00:04Z","type":"response_item","payload":{"type":"custom_tool_call","call_id":"c1","name":"exec","input":"tools.exec_command({cmd:'npm test'})"}}`,
		`{"timestamp":"2026-11-10T10:00:05Z","type":"response_item","payload":{"type":"function_call","call_id":"c2","name":"exec_command","arguments":"{\"cmd\":\"git status\",\"workdir\":\"/tmp/codexrepo\"}"}}`,
		`{"timestamp":"2026-11-10T10:00:06Z","type":"response_item","payload":{"type":"function_call_output","call_id":"c2","output":"Chunk ID: a\nProcess exited with code 0\nOutput:\nclean"}}`,
	)
	if err := ix.indexFile(path, "codex"); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, ix, `SELECT COUNT(*) FROM docs WHERE kind='prompt'`); n != 1 {
		t.Fatalf("prompts = %d, want 1 (deduped, system prompt skipped)", n)
	}
	var dur int64
	var e int
	var sig, sess string
	if err := ix.w.QueryRow(`SELECT dur, err, errSig, session FROM spans WHERE ref='exec-1'`).Scan(&dur, &e, &sig, &sess); err != nil {
		t.Fatal(err)
	}
	if dur != 2500 || e != 1 || sig != "FAIL <path>" || sess != "codex:01a1df10-633b-7031-a610-56ef0beef44d" {
		t.Fatalf("exec-1 dur=%d err=%d sig=%q sess=%s", dur, e, sig, sess)
	}
	if n := countRows(t, ix, `SELECT COUNT(*) FROM spans`); n != 2 {
		t.Fatalf("spans = %d, want 2 (exec wrapper skipped)", n)
	}
	if n := countRows(t, ix, `SELECT err FROM spans WHERE ref='c2'`); n != 0 {
		t.Fatal("c2 should succeed")
	}
}

func TestHeadFingerprintDetectsRewrite(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.jsonl")
	os.WriteFile(p, []byte(strings.Repeat("a", 1000)), 0o644)
	f, _ := os.Open(p)
	head := fileHead(f, 1000)
	f.Close()
	os.WriteFile(p, []byte(strings.Repeat("b", 1000)), 0o644)
	f, _ = os.Open(p)
	defer f.Close()
	if !headChanged(head, f, 1000) {
		t.Fatal("same-size rewrite not detected")
	}
	os.WriteFile(p, []byte(strings.Repeat("a", 1000)+"more"), 0o644)
	f2, _ := os.Open(p)
	defer f2.Close()
	if headChanged(head, f2, 1004) {
		t.Fatal("an append must not count as a rewrite")
	}
}
