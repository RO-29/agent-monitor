package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// v4Fixture fills the global store and trace caches with n synthetic sessions.
func v4Fixture(tb testing.TB, n int) int64 {
	dir := tb.TempDir()
	tb.Setenv("AGENT_MONITOR_HOME", dir)
	paneRegistry = NewPaneRegistry()
	store = NewStore()
	childCache.Lock()
	childCache.links = nil
	childCache.Unlock()
	now := time.Now().UnixMilli()
	for i := 0; i < n; i++ {
		tool := ToolClaude
		if i%2 == 1 {
			tool = ToolCodex
		}
		uid := fmt.Sprintf("s%05d", i)
		id := string(tool) + ":" + uid
		p := filepath.Join(dir, uid+".jsonl")
		_ = os.WriteFile(p, []byte("{}\n"), 0o644)
		st, _ := os.Stat(p)
		start := now - int64(i)*120_000
		cwd := fmt.Sprintf("/work/repo%02d", i%20)
		s := &Session{ID: id, Tool: tool, SessionID: uid, Cwd: cwd, State: StateCompleted, StartedAt: start,
			LastActivityAt: start + 600_000, FirstMessage: "Please fix the flaky login test in this repository", Title: fmt.Sprintf("repo%02d", i%20),
			TranscriptPath: p}
		if i < 6 {
			s.State = StateRunning
		}
		store.sessions[id] = s
		tr := synthTrace(id, start, 60)
		traceCache.m[id] = &traceCacheEntry{size: st.Size(), mtime: st.ModTime().UnixMilli(), trace: tr}
		sum := summarizeTrace(tr)
		sum.LastOutcome = "**Done.** Fixed the test, see [the PR](https://example.com/pr/1)."
		summaryCache.m[id] = &summaryEntry{size: st.Size(), mtime: st.ModTime().UnixMilli(), sum: sum}
	}
	return now
}

// synthTrace builds a trace with turns of bash/read/edit calls and one commit.
func synthTrace(id string, start int64, calls int) *SessionTrace {
	tr := &SessionTrace{SessionID: id, io: spanIOMap{}, FirstTs: start}
	tr.Segments = []Segment{{Index: 0, FromTs: start, ToTs: start + int64(calls)*10_000}}
	turn := ""
	for i := 0; i < calls; i++ {
		ts := start + int64(i)*10_000
		if i%10 == 0 {
			turn = fmt.Sprintf("t%d", i)
			tr.Spans = append(tr.Spans, Span{ID: turn, Kind: "turn", Ts: ts, Dur: 100_000})
		}
		sp := Span{ID: fmt.Sprintf("s%d", i), Kind: "tool", Name: "Bash", Fam: "bash", Ts: ts + 1, Dur: 2000, Depth: 1, Parent: turn, Res: "go test ./..."}
		args := `{"command":"go test ./..."}`
		if i%7 == 3 {
			sp.Err = true
		}
		if i == calls-2 {
			args = `{"command":"git commit -m \"Fix flaky login test\""}`
			sp.Res = "git commit -m \"Fix flaky login test\""
			tr.io.set(sp.ID, SpanIO{Args: args, Result: "[main 1a2b3c4] Fix flaky login test\n 1 file changed"})
		} else {
			tr.io.set(sp.ID, SpanIO{Args: args})
		}
		tr.Spans = append(tr.Spans, sp)
	}
	tr.LastTs = start + int64(calls)*10_000
	tr.Segments[0].Errors, _ = countErrors(tr.Spans)
	return tr
}

func BenchmarkThreadsHandler(b *testing.B) {
	v4Fixture(b, 2000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := json.Marshal(threadsResponse(true, false)); err != nil {
			b.Fatal(err)
		}
	}
}

func TestDisplayTitle(t *testing.T) {
	cases := []struct{ title, repo, prompt, want string }{
		{"Fix flaky login test", "app", "whatever", "Fix flaky login test"},
		{"You are reviewing the sketch feature.", "app", "You are reviewing the sketch feature.", "Review of the sketch feature"},
		{"app", "app", "You are reviewing the last commit in this repository. Report defects.", "Review of the last commit"},
		{"**P2, [src/app.ts:12](src/app.ts) null check**", "app", "You are reviewing the last commit in this repository", "Review of the last commit"},
		{"", "app", "Please review the sketch feature and list problems", "Review of the sketch feature and list problems"},
		{"src/server/main.go", "app", "In this repository, add a health endpoint.", "Add a health endpoint"},
		{"# Plan", "app", "can you migrate the cache to the new client library so that every call goes through one pool of connections", "Migrate the cache to the new client library so that…"},
		{"**P1, [src/db.ts]** leak", "app", "", "P1, src/db.ts leak"},
		{"", "app", "", "app"},
		{"", "app", "You are a senior engineer. Fix the retry loop in the uploader.", "Fix the retry loop in the uploader"},
		{"", "app", "<command-name>/goal</command-name><command-args>ship the onboarding page</command-args>", "Ship the onboarding page"},
	}
	for _, c := range cases {
		got := displayTitle(c.title, c.repo, claudePromptText(c.prompt))
		if got != c.want {
			t.Errorf("displayTitle(%q, %q, %q) = %q, want %q", c.title, c.repo, c.prompt, got, c.want)
		}
		if len(got) > 60 && got != c.title {
			t.Errorf("too long: %q", got)
		}
	}
}

func TestOutcomeLine(t *testing.T) {
	cases := []struct{ in, want string }{
		{"**Done.** Fixed the test, see [the PR](https://example.com/pr/1).", "Done. Fixed the test, see the PR."},
		{"## Result\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\nAll `12` checks pass on the branch now", "Result All 12 checks pass on the branch now."},
		{"```go\nx := 1\n```\n- one finding in `auth.go`", "one finding in auth.go."},
		{"", ""},
	}
	for _, c := range cases {
		if got := outcomeLine(c.in); got != c.want {
			t.Errorf("outcomeLine(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	long := outcomeLine("This sentence keeps going with many words so that the line must be cut at a word boundary well before it reaches the end of all of these words here and more and more and more and more")
	if len(long) > 160 || !strings.HasSuffix(long, "…") {
		t.Errorf("bad clip: %q (%d)", long, len(long))
	}
}

func TestExtractShipped(t *testing.T) {
	heredoc := "git add -A && git commit -m \"$(cat <<'MSG'\nAdd stack lanes to the timeline\n\nLonger body with \\\"quotes\\\" and (parens).\nMSG\n)\""
	prBody := "gh pr create --title \"Add now endpoint\" --body \"$(cat <<'MSG'\nbody (with parens)\nMSG\n)\""
	cases := []struct {
		name, cmd, result string
		failed            bool
		want              []Shipped
	}{
		{"commit -m", `git commit -m "Fix flaky login test"`, "[main 1a2b3c4] Fix flaky login test\n 1 file changed", false,
			[]Shipped{{Kind: "commit", Label: "Fix flaky login test", Ref: "1a2b3c4"}}},
		{"commit heredoc", heredoc, "[feat/x 9f8e7d6] Add stack lanes to the timeline", false,
			[]Shipped{{Kind: "commit", Label: "Add stack lanes to the timeline", Ref: "9f8e7d6"}}},
		{"commit -F heredoc", "git commit -F - <<MSG\nSubject from stdin\nMSG", "", false,
			[]Shipped{{Kind: "commit", Label: "Subject from stdin"}}},
		{"commit amend no message", "git commit --amend --no-edit", "[main abc1234] Earlier subject", false,
			[]Shipped{{Kind: "commit", Label: "Earlier subject", Ref: "abc1234"}}},
		{"commit no evidence", "git commit --amend --no-edit", "", false,
			[]Shipped{{Kind: "commit", Label: "untitled commit"}}},
		{"commit -am and push", `cd /w/app && git commit -am 'Tidy logs' && git push -u origin feat/logs`,
			"[feat/logs 0011223] Tidy logs\nTo github.com:o/app.git\n * [new branch]      feat/logs -> feat/logs", false,
			[]Shipped{{Kind: "commit", Label: "Tidy logs", Ref: "0011223"}, {Kind: "push", Label: "origin/feat/logs"}}},
		{"nothing to commit", `git commit -m "x"`, "nothing to commit, working tree clean", false, nil},
		{"failed commit", `git commit -m "x"`, "error: hook failed", true, nil},
		{"git -C commit", `git -C /w/app commit -m "Bump version"`, "", false, []Shipped{{Kind: "commit", Label: "Bump version"}}},
		{"push from result", "git push", "To github.com:o/app.git\n   1a2b3c4..5d6e7f8  main -> main", false, []Shipped{{Kind: "push", Label: "origin/main"}}},
		{"push up to date", "git push", "Everything up-to-date", false, nil},
		{"push refspec", "git push --force-with-lease origin HEAD:refs/heads/fix/a", "", false, []Shipped{{Kind: "push", Label: "origin/fix/a"}}},
		{"pr create", prBody, "https://github.com/o/app/pull/42\n", false,
			[]Shipped{{Kind: "pr", Label: "Add now endpoint", Ref: "https://github.com/o/app/pull/42"}}},
		{"pr create fill", "gh pr create --fill", "https://github.com/o/app/pull/7", false, []Shipped{{Kind: "pr", Label: "PR #7", Ref: "https://github.com/o/app/pull/7"}}},
		{"pr merge", "gh pr merge 42 --squash --delete-branch", "", false, []Shipped{{Kind: "merge", Label: "PR #42"}}},
		{"pr view ignored", "gh pr view 42", "", false, nil},
		{"host.sh slug", "~/artifact-home/bin/host.sh --me report.html weekly-report", "Published: https://example.pages.dev/weekly-report/", false,
			[]Shipped{{Kind: "page", Label: "weekly-report", Ref: "https://example.pages.dev/weekly-report/"}}},
		{"doc.sh share", "bash ~/artifact-home/bin/doc.sh --share a@example.com notes/plan.md", "", false, []Shipped{{Kind: "page", Label: "plan"}}},
		{"grep git commit ignored", `rg "git commit" README.md; echo git push`, "", false, nil},
		{"env prefix", `GIT_EDITOR=true git commit -m "Codex change"`, "", false, []Shipped{{Kind: "commit", Label: "Codex change"}}},
	}
	for _, c := range cases {
		got := extractShipped(c.cmd, c.result, c.failed, 5)
		if len(got) != len(c.want) {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
			continue
		}
		for i := range got {
			w := c.want[i]
			w.Ts = 5
			if got[i] != w {
				t.Errorf("%s[%d]: got %+v, want %+v", c.name, i, got[i], w)
			}
			if got[i].Label == "git commit" || got[i].Label == "" {
				t.Errorf("%s: bad label %q", c.name, got[i].Label)
			}
		}
	}
}

func TestCommandFromArgs(t *testing.T) {
	cases := map[string]string{
		`{"command":"git status"}`:                         "git status",
		`{"cmd":"git push"}`:                               "git push",
		`{"command":["bash","-lc","git commit -m \"x\""]}`: `git commit -m "x"`,
		`not json`: "",
	}
	for in, want := range cases {
		if got := commandFromArgs(in); got != want {
			t.Errorf("commandFromArgs(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestMergeShippedDedupNewestFirst(t *testing.T) {
	in := []Shipped{{Kind: "pr", Label: "Add x", Ref: "u1", Ts: 1}, {Kind: "pr", Label: "PR #1", Ref: "u1", Ts: 2}, {Kind: "push", Label: "origin/main", Ts: 3}, {Kind: "push", Label: "origin/main", Ts: 4}}
	for i := 0; i < 20; i++ {
		in = append(in, Shipped{Kind: "commit", Label: fmt.Sprint("c", i), Ref: fmt.Sprint(i), Ts: int64(10 + i)})
	}
	got := mergeShipped(append([]Shipped(nil), in...))
	if len(got) != maxShipped || got[0].Ts != 29 {
		t.Fatalf("len=%d first=%+v", len(got), got[0])
	}
	got = mergeShipped(append([]Shipped(nil), in[:4]...))
	if len(got) != 2 || got[1].Label != "Add x" || got[0].Ts != 3 {
		t.Fatalf("dedupe: %+v", got)
	}
}

func TestCountErrorsRecovery(t *testing.T) {
	spans := []Span{
		{Kind: "turn", ID: "t1"},
		{Kind: "tool", Name: "Bash", Parent: "t1", Ts: 1, Err: true},
		{Kind: "tool", Name: "Bash", Parent: "t1", Ts: 2},
		{Kind: "tool", Name: "Edit", Parent: "t1", Ts: 3, Err: true},
		{Kind: "tool", Name: "Read", Parent: "t1", Ts: 4},
		{Kind: "tool", Name: "Edit", Parent: "t2", Ts: 5},
		{Kind: "tool", Name: "Bash", Parent: "t2", Ts: 6, Err: true},
	}
	total, open := countErrors(spans)
	if total != 3 || open != 2 {
		t.Fatalf("total=%d open=%d, want 3/2", total, open)
	}
}

func TestActivityAndDensity(t *testing.T) {
	from := int64(1_000 * bucketMs)
	spans := []Span{{Kind: "tool", Ts: from + 10}, {Kind: "tool", Ts: from + 20, Err: true}, {Kind: "turn", Ts: from + 30},
		{Kind: "agent", Ts: from + 11*bucketMs}, {Kind: "tool", Ts: from - 1}, {Kind: "tool", Ts: from + 12*bucketMs}}
	calls, errs := make([]int, activityLen), make([]int, activityLen)
	addActivity(spans, from, calls, errs)
	if calls[0] != 2 || errs[0] != 1 || calls[11] != 1 || sumInts(calls) != 3 {
		t.Fatalf("calls=%v errs=%v", calls, errs)
	}
	d := densityOf(spans, from, from+12*bucketMs)
	if len(d) != 3 || d[0].Calls != 2 || d[0].Errors != 1 || d[0].To-d[0].From != bucketMs {
		t.Fatalf("density %+v", d)
	}
	s := sumDensity(append(d, DensityBucket{From: d[0].From, To: d[0].To, Calls: 5}))
	if len(s) != 3 || s[0].Calls != 7 {
		t.Fatalf("sum %+v", s)
	}
}

func sumInts(xs []int) int {
	n := 0
	for _, x := range xs {
		n += x
	}
	return n
}

func TestComputeStacks(t *testing.T) {
	items := []stackItem{
		{ID: "c1", Parent: "p", Agent: "codex", ChildKind: "delegated", Display: "Review of the last commit", State: StateCompleted, Outcome: "2 findings: P2 in auth", StartedAt: 1, LastAt: 10},
		{ID: "c2", Parent: "p", Agent: "codex", ChildKind: "delegated", Display: "Reviewing the diff", State: StateRunning, StartedAt: 2, LastAt: 20},
		{ID: "c3", Parent: "p", Agent: "codex", ChildKind: "delegated", Display: "Review of sketch", State: StateCompleted, Outcome: "No findings, clean", StartedAt: 3, LastAt: 5},
		{ID: "c4", Parent: "p", Agent: "codex", ChildKind: "delegated", Display: "Review of api", State: StateIdle, ErrorsOpen: 1, StartedAt: 4},
		{ID: "c5", Parent: "p", Agent: "codex", Display: "Fix the build", State: StateCompleted},
		{ID: "s1", Parent: "p", Agent: "claude", ChildKind: "subagent", Display: "Explore the code", State: StateCompleted},
		{ID: "s2", Parent: "p", Agent: "claude", ChildKind: "subagent", Display: "Search configs", State: StateAwaitingPermission},
		{ID: "s3", Parent: "p", Agent: "claude", ChildKind: "subagent", Display: "Map callers", State: StateAbandoned},
	}
	stacks, of := computeStacks(items)
	if len(stacks) != 2 {
		t.Fatalf("stacks %+v", stacks)
	}
	r := stacks[0]
	if r.Label != "4 Codex reviews" || r.Kind != "review" || r.Total != 4 || r.Running != 1 || r.Done != 2 || r.Failed != 1 || r.Findings != 1 || r.LastAt != 20 {
		t.Fatalf("review stack %+v", r)
	}
	sub := stacks[1]
	if sub.Label != "3 Claude subagents" || sub.Waiting != 1 || sub.Abandoned != 1 || sub.Done != 1 {
		t.Fatalf("subagent stack %+v", sub)
	}
	if of["c5"] != "" || of["c1"] != r.ID || of["s3"] != sub.ID {
		t.Fatalf("membership %v", of)
	}
}

func TestCollapseStacks(t *testing.T) {
	st := []Stack{{ID: "stack:p", Parent: "p", Agent: "codex", Label: "2 Codex reviews", Total: 2, Done: 2, Members: []string{"a", "b"}}}
	lanes := []TimelineLane{
		{ThreadID: "p", Bars: []TimelineBar{{From: 0, To: 10, Kind: "work"}}},
		{ThreadID: "a", Parent: "p", Errors: 1, Bars: []TimelineBar{{From: 0, To: 10, Kind: "work"}}, Density: []DensityBucket{{From: 0, To: bucketMs, Calls: 2}}},
		{ThreadID: "b", Parent: "p", Bars: []TimelineBar{{From: 5, To: 20, Kind: "work"}, {From: 30, To: 40, Kind: "error"}}, Density: []DensityBucket{{From: 0, To: bucketMs, Calls: 3}}},
	}
	got := orderLanes(collapseStacks(lanes, st))
	if len(got) != 2 || got[1].Kind != "stack" || got[1].ThreadID != "stack:p" || got[1].Display != "2 Codex reviews" {
		t.Fatalf("lanes %+v", got)
	}
	s := got[1]
	if len(s.Bars) != 2 || s.Bars[0].To != 20 || s.Density[0].Calls != 5 || s.Errors != 1 || s.Stack.Total != 2 || len(s.Members) != 2 {
		t.Fatalf("stack lane %+v", s)
	}
}

func TestSplitCommands(t *testing.T) {
	got := splitCommands("cd 'a b' && git commit -m \"it's (fine); ok\" | tee x # git push\necho done")
	if len(got) != 4 || got[1][3] != "it's (fine); ok" || got[0][1] != "a b" || got[3][0] != "echo" {
		t.Fatalf("%q", got)
	}
}

// stackFixture adds a parent Claude thread that delegated three Codex reviews and one fix.
func stackFixture(t *testing.T) (parent string, now int64) {
	now = v4Fixture(t, 40)
	dir := t.TempDir()
	start := now - 30*60_000
	p := filepath.Join(dir, "parent.jsonl")
	_ = os.WriteFile(p, []byte("{}\n"), 0o644)
	st, _ := os.Stat(p)
	parent = "claude:parent"
	store.sessions[parent] = &Session{ID: parent, Tool: ToolClaude, SessionID: "parent", Cwd: "/work/stacked", State: StateRunning,
		StartedAt: start, LastActivityAt: now - 60_000, Title: "stacked", FirstMessage: "Please review the sketch feature with codex", TranscriptPath: p}
	tr := &SessionTrace{SessionID: parent, io: spanIOMap{}, FirstTs: start, LastTs: now - 60_000}
	tr.Spans = append(tr.Spans, Span{ID: "t0", Kind: "turn", Ts: start, Dur: 25 * 60_000})
	prompts := []string{"You are reviewing the last commit in this repository.", "You are reviewing the sketch feature.", "Review the diff for defects.", "Fix the failing build."}
	outcomes := []string{"Found 2 findings: P2 in auth.go", "No findings, clean.", "One issue in the parser.", "Build passes."}
	for i, pr := range prompts {
		ts := start + int64(i+1)*60_000
		id := fmt.Sprintf("b%d", i)
		tr.Spans = append(tr.Spans, Span{ID: id, Kind: "tool", Name: "Bash", Fam: "bash", Parent: "t0", Depth: 1, Ts: ts, Dur: 30_000, Res: "codex exec -s read-only -"})
		tr.io.set(id, SpanIO{Args: `{"command":"codex exec -s read-only - < p.md"}`})
		cid := fmt.Sprintf("codex:kid%d", i)
		cp := filepath.Join(dir, fmt.Sprintf("kid%d.jsonl", i))
		_ = os.WriteFile(cp, []byte("{}\n"), 0o644)
		cst, _ := os.Stat(cp)
		store.sessions[cid] = &Session{ID: cid, Tool: ToolCodex, SessionID: fmt.Sprintf("kid%d", i), Cwd: "/work/stacked", State: StateCompleted,
			StartedAt: ts + 1000, LastActivityAt: ts + 20_000, FirstMessage: pr, TranscriptPath: cp}
		ktr := synthTrace(cid, ts+1000, 12)
		traceCache.m[cid] = &traceCacheEntry{size: cst.Size(), mtime: cst.ModTime().UnixMilli(), trace: ktr}
		sum := summarizeTrace(ktr)
		sum.LastOutcome = outcomes[i]
		summaryCache.m[cid] = &summaryEntry{size: cst.Size(), mtime: cst.ModTime().UnixMilli(), sum: sum}
	}
	tr.Spans = append(tr.Spans, Span{ID: "push", Kind: "tool", Name: "Bash", Fam: "bash", Parent: "t0", Depth: 1, Ts: now - 120_000, Dur: 1000, Res: "git push"})
	tr.io.set("push", SpanIO{Args: `{"command":"git push origin feat/sketch"}`, Result: "To github.com:o/r.git\n   1a2b3c4..5d6e7f8  feat/sketch -> feat/sketch"})
	tr.Segments = []Segment{{FromTs: start, ToTs: now - 60_000, UsdEst: 1.5}}
	traceCache.m[parent] = &traceCacheEntry{size: st.Size(), mtime: st.ModTime().UnixMilli(), trace: tr}
	summaryCache.m[parent] = &summaryEntry{size: st.Size(), mtime: st.ModTime().UnixMilli(), sum: summarizeTrace(tr)}
	return parent, now
}

func TestThreadsResponseV4Fields(t *testing.T) {
	parent, _ := stackFixture(t)
	raw, err := json.Marshal(threadsResponse(true, false))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Threads  []map[string]any  `json:"threads"`
		ThreadOf map[string]string `json:"threadOf"`
		Stacks   []Stack           `json:"stacks"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Stacks) != 1 {
		t.Fatalf("stacks %+v", got.Stacks)
	}
	s := got.Stacks[0]
	if s.Label != "3 Codex reviews" || s.Parent != parent || s.Findings != 2 || s.Total != 3 || s.Repo != "stacked" {
		t.Fatalf("stack %+v", s)
	}
	byID := map[string]map[string]any{}
	for _, th := range got.Threads {
		byID[th["id"].(string)] = th
	}
	p := byID[parent]
	if p["display"] != "Review of the sketch feature with codex" || p["repo"] != "stacked" {
		t.Fatalf("parent display/repo: %v %v", p["display"], p["repo"])
	}
	if a, ok := p["activity"].([]any); !ok || len(a) != activityLen {
		t.Fatalf("parent activity %v", p["activity"])
	}
	sh, _ := p["shipped"].([]any)
	if len(sh) != 1 || sh[0].(map[string]any)["label"] != "origin/feat/sketch" {
		t.Fatalf("parent shipped %v", p["shipped"])
	}
	k0 := byID["codex:kid0"]
	if k0["display"] != "Review of the last commit" || k0["stackId"] != s.ID || k0["outcomeLine"] != "Found 2 findings: P2 in auth.go." {
		t.Fatalf("child: %v / %v / %v", k0["display"], k0["stackId"], k0["outcomeLine"])
	}
	if byID["codex:kid3"]["stackId"] != nil {
		t.Fatalf("fix child must not join the review stack")
	}
	old := byID["codex:s00039"]
	if _, ok := old["activity"]; ok {
		t.Fatalf("thread idle for hours has activity")
	}
	if old["errorsOpen"].(float64) <= 0 || old["errorsOpen"].(float64) > old["errors"].(float64) {
		t.Fatalf("errorsOpen %v errors %v", old["errorsOpen"], old["errors"])
	}
	if got.ThreadOf["codex:kid0"] != "codex:kid0" {
		t.Fatalf("threadOf %v", got.ThreadOf["codex:kid0"])
	}
	for _, th := range threadsResponse(false, false)["threads"].([]ThreadV3) {
		if th.ParentThread != "" {
			t.Fatalf("child in top-level list")
		}
	}
}

func TestBuildNow(t *testing.T) {
	parent, now := stackFixture(t)
	n := buildNow(now)
	if n.Running == 0 || len(n.Lanes) == 0 || len(n.Lanes) > 8 || !hasLane(n.Lanes, parent) || n.From != now-nowWindowMs {
		t.Fatalf("now %+v", n)
	}
	if n.SpendToday <= 0 || n.ErrorsPerHour == 0 {
		t.Fatalf("spendToday %v errorsPerHour %v", n.SpendToday, n.ErrorsPerHour)
	}
	for _, b := range n.Lanes[len(n.Lanes)-1].Bars {
		if b.Kind != "work" && b.Kind != "wait" && b.Kind != "error" {
			t.Fatalf("bar kind %q", b.Kind)
		}
	}
}

func TestTimelineStacks(t *testing.T) {
	_, now := stackFixture(t)
	day := now - 12*3600_000
	plain := buildTimeline(day, now+60_000, false)["lanes"].([]TimelineLane)
	coll := buildTimeline(day, now+60_000, true)["lanes"].([]TimelineLane)
	if len(coll) != len(plain)-2 {
		t.Fatalf("plain %d collapsed %d, want 2 fewer", len(plain), len(coll))
	}
	var stack *TimelineLane
	for i := range coll {
		if coll[i].Kind == "stack" {
			stack = &coll[i]
		}
		if coll[i].Display == "" {
			t.Fatalf("lane %s without display", coll[i].ThreadID)
		}
	}
	if stack == nil || stack.Display != "3 Codex reviews" || len(stack.Members) != 3 || len(stack.Density) == 0 || stack.Stack.Total != 3 {
		t.Fatalf("stack lane %+v", stack)
	}
}

func TestV4JSONFieldNames(t *testing.T) {
	raw, _ := json.Marshal(ThreadV3{V4Fields: V4Fields{Display: "d", Repo: "r", OutcomeLine: "o", Shipped: []Shipped{{Kind: "pr", Label: "l", Ref: "u", Ts: 1}},
		ErrorsOpen: 1, Activity: []int{1}, ActivityErr: []int{0}, StackID: "s"}})
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	for _, k := range []string{"display", "repo", "outcomeLine", "shipped", "errorsOpen", "activity", "activityErr", "stackId", "approvals", "id", "title"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing %s in %s", k, raw)
		}
	}
}

func hasLane(ls []NowLane, id string) bool {
	for _, l := range ls {
		if l.ThreadID == id {
			return true
		}
	}
	return false
}
