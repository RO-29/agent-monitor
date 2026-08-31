package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Synthetic Claude Code transcript: /clear start, two turns with tool calls,
// one auto compaction with its summary, a subagent run, a correction, a
// repeated pr-link, and a memory write.
func writeClaudeFixture(t *testing.T) (string, *Session) {
	t.Helper()
	dir := t.TempDir()
	projDir := filepath.Join(dir, "-Users-x-proj")
	sid := "11111111-2222-3333-4444-555555555555"
	if err := os.MkdirAll(filepath.Join(projDir, sid, "subagents"), 0o755); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	ts := func(sec int) string { return base.Add(time.Duration(sec) * time.Second).Format(time.RFC3339Nano) }
	line := func(v any) string { b, _ := json.Marshal(v); return string(b) }
	user := func(sec int, text string, extra map[string]any) string {
		m := map[string]any{"type": "user", "timestamp": ts(sec), "sessionId": sid, "message": map[string]any{"role": "user", "content": text}}
		for k, v := range extra {
			m[k] = v
		}
		return line(m)
	}
	asst := func(sec int, parts []any, in, out, cr int) string {
		return line(map[string]any{"type": "assistant", "timestamp": ts(sec), "sessionId": sid, "message": map[string]any{
			"role": "assistant", "model": "claude-opus-5", "content": parts,
			"usage": map[string]any{"input_tokens": in, "output_tokens": out, "cache_read_input_tokens": cr, "cache_creation_input_tokens": 0}}})
	}
	toolUse := func(id, name string, input map[string]any) any {
		return map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}
	}
	toolRes := func(sec int, id, text string, isErr bool) string {
		return line(map[string]any{"type": "user", "timestamp": ts(sec), "sessionId": sid, "message": map[string]any{"role": "user",
			"content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "is_error": isErr, "content": text}}}})
	}
	memDir := filepath.Join(dir, ".claude", "projects", "-Users-x-proj", "memory")
	_ = os.MkdirAll(memDir, 0o755)
	memPath := filepath.Join(memDir, "always-run-tests.md")
	_ = os.WriteFile(memPath, []byte("---\nname: always-run-tests\ndescription: run the tests before every push\nmetadata:\n  type: feedback\n---\n\nbody\n"), 0o644)

	lines := []string{
		user(0, "<command-name>/clear</command-name><command-message>clear</command-message><command-args></command-args>", nil),
		user(1, "<local-command-caveat>Caveat: ignore</local-command-caveat>", nil),
		user(2, "Build the widget and run the tests", nil),
		asst(3, []any{map[string]any{"type": "text", "text": "Reading the code first."}}, 1000, 50, 20000),
		asst(4, []any{toolUse("t1", "Read", map[string]any{"file_path": "/Users/x/proj/a.go"})}, 1000, 30, 21000),
		toolRes(5, "t1", "package main", false),
		asst(6, []any{toolUse("t2", "Bash", map[string]any{"command": "go test ./..."})}, 1000, 30, 22000),
		toolRes(20, "t2", "FAIL\nExit code 1", true),
		asst(21, []any{toolUse("t3", "Agent", map[string]any{"description": "Write the report", "prompt": "Write it", "subagent_type": "general-purpose"})}, 1000, 30, 23000),
		toolRes(60, "t3", "done", false),
		asst(61, []any{toolUse("t4", "Write", map[string]any{"file_path": memPath, "content": "x"})}, 1000, 30, 23500),
		toolRes(62, "t4", "ok", false),
		line(map[string]any{"type": "pr-link", "sessionId": sid, "prNumber": 7, "prUrl": "https://example.test/pr/7", "timestamp": ts(63)}),
		line(map[string]any{"type": "pr-link", "sessionId": sid, "prNumber": 7, "prUrl": "https://example.test/pr/7", "timestamp": ts(64)}),
		line(map[string]any{"type": "system", "subtype": "turn_duration", "durationMs": 62000, "timestamp": ts(65), "sessionId": sid}),
		user(70, "no — the tests must run in the container, not on the host", nil),
		asst(71, []any{map[string]any{"type": "text", "text": "Switching to the container."}}, 1000, 40, 24000),
		line(map[string]any{"type": "system", "subtype": "compact_boundary", "timestamp": ts(100), "sessionId": sid, "content": "Conversation compacted",
			"compactMetadata": map[string]any{"trigger": "auto", "preTokens": 1000000, "postTokens": 20000, "cumulativeDroppedTokens": 980000, "durationMs": 90000}}),
		user(101, "This session is being continued from a previous conversation.\n\nSummary:\n1. **Primary Request and Intent:**\n   Build the widget with tests in the container.\n\n2. **Errors and fixes:**\n   - go test failed on the host; moved to the container.\n\n3. **Pending Tasks:**\n   - Open the PR review.\n", map[string]any{"isCompactSummary": true, "isVisibleInTranscriptOnly": true}),
		user(110, "continue with the PR", nil),
		asst(111, []any{map[string]any{"type": "text", "text": "Opening the PR."}}, 500, 40, 30000),
		line(map[string]any{"type": "cost-state", "sessionId": sid, "totalCostUSD": 1.5}),
	}
	transcript := filepath.Join(projDir, sid+".jsonl")
	if err := os.WriteFile(transcript, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// subagent transcript linked by toolUseId
	agent := filepath.Join(projDir, sid, "subagents", "agent-abc.jsonl")
	_ = os.WriteFile(filepath.Join(projDir, sid, "subagents", "agent-abc.meta.json"), []byte(`{"agentType":"general-purpose","toolUseId":"t3","spawnDepth":1}`), 0o644)
	child := []string{
		// a FORKED subagent replays the parent's history first — including the
		// Agent call that spawned it (t3) and earlier tool ids (t1). These must
		// not become duplicate spans or a self-parented Agent.
		line(map[string]any{"type": "assistant", "timestamp": ts(4), "isSidechain": true, "agentId": "abc", "message": map[string]any{"role": "assistant", "content": []any{toolUse("t1", "Read", map[string]any{"file_path": "/Users/x/proj/a.go"})}}}),
		line(map[string]any{"type": "assistant", "timestamp": ts(21), "isSidechain": true, "agentId": "abc", "message": map[string]any{"role": "assistant", "content": []any{toolUse("t3", "Agent", map[string]any{"description": "Write the report", "prompt": "Write it"})}}}),
		line(map[string]any{"type": "assistant", "timestamp": ts(25), "isSidechain": true, "agentId": "abc", "message": map[string]any{"role": "assistant", "content": []any{toolUse("c1", "Bash", map[string]any{"command": "echo hi"})}}}),
		line(map[string]any{"type": "user", "timestamp": ts(30), "isSidechain": true, "agentId": "abc", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "c1", "content": "hi"}}}}),
	}
	_ = os.WriteFile(agent, []byte(strings.Join(child, "\n")+"\n"), 0o644)
	sess := &Session{ID: "claude:" + sid, Tool: ToolClaude, SessionID: sid, Cwd: "/Users/x/proj", TranscriptPath: transcript, Model: "claude-opus-5"}
	return dir, sess
}

func TestClaudeTraceSegmentsAndSpans(t *testing.T) {
	_, sess := writeClaudeFixture(t)
	tr := buildClaudeTrace(sess)
	if tr == nil {
		t.Fatal("nil trace")
	}
	if len(tr.Segments) != 2 {
		t.Fatalf("segments = %d, want 2 (clear start + compact)", len(tr.Segments))
	}
	if tr.Segments[0].Boundary.Kind != "clear" {
		t.Errorf("first boundary = %s, want clear", tr.Segments[0].Boundary.Kind)
	}
	b := tr.Segments[1].Boundary
	if b.Kind != "compact" || b.PreTokens != 1000000 || b.PostTokens != 20000 || b.DroppedTokens != 980000 {
		t.Errorf("compact boundary = %+v", b)
	}
	if !strings.Contains(strings.ToLower(b.Summary), "primary request") || len(b.Sections) < 2 {
		t.Errorf("summary not attached: sections=%v", b.Sections)
	}
	var turns, tools, agents, users, errs, depth2 int
	var agentDur int64
	for _, sp := range tr.Spans {
		switch sp.Kind {
		case "turn":
			turns++
		case "tool":
			tools++
		case "agent":
			agents++
			agentDur = sp.Dur
		case "user":
			users++
		}
		if sp.Err {
			errs++
		}
		if sp.Depth == 2 {
			depth2++
		}
	}
	if turns != 3 || users != 3 {
		t.Errorf("turns=%d users=%d, want 3/3", turns, users)
	}
	if tools != 4 || agents != 1 || depth2 != 1 {
		t.Errorf("tools=%d agents=%d depth2=%d, want 4/1/1 (subagent child nested, replays dropped)", tools, agents, depth2)
	}
	ids := map[string]bool{}
	for _, sp := range tr.Spans {
		if ids[sp.ID] {
			t.Errorf("duplicate span id %s", sp.ID)
		}
		ids[sp.ID] = true
		if sp.Parent == sp.ID {
			t.Errorf("span %s is its own parent", sp.ID)
		}
	}
	if agentDur != 39000 {
		t.Errorf("agent span dur = %d, want 39000 (tool_result pairing)", agentDur)
	}
	if errs != 1 {
		t.Errorf("errors=%d, want 1 (is_error on go test)", errs)
	}
	// turn 1 closed by turn_duration at +65s → 63 s
	for _, sp := range tr.Spans {
		if sp.Kind == "turn" && sp.Name == "turn 1" && sp.Dur != 63000 {
			t.Errorf("turn 1 dur = %d, want 63000", sp.Dur)
		}
	}
	// segment assignment: the post-compact prompt lands in segment 1
	if tr.Segments[1].Turns != 1 || tr.Segments[0].Turns != 2 {
		t.Errorf("turns per segment = %d/%d, want 2/1", tr.Segments[0].Turns, tr.Segments[1].Turns)
	}
	if tr.CostUSD != 1.5 || tr.CostEstimated {
		t.Errorf("cost = %v est=%v, want exact 1.5", tr.CostUSD, tr.CostEstimated)
	}
	if tr.ContextUsed != 30500 {
		t.Errorf("contextUsed = %d, want 30500 (last input+cache)", tr.ContextUsed)
	}
}

func TestClaudeTraceLearningsOutputsChapter(t *testing.T) {
	_, sess := writeClaudeFixture(t)
	tr := buildClaudeTrace(sess)
	var corr, mem, sum int
	for _, l := range tr.Learnings {
		switch l.Source {
		case "correction":
			corr++
			if !l.Heuristic {
				t.Errorf("correction must be flagged heuristic")
			}
		case "memory":
			mem++
			if !strings.Contains(l.Text, "feedback: run the tests") {
				t.Errorf("memory learning text = %q", l.Text)
			}
		case "summary":
			sum++
		}
	}
	if corr != 1 || mem != 1 || sum < 1 {
		t.Errorf("learnings corr=%d mem=%d sum=%d", corr, mem, sum)
	}
	prs := 0
	for _, o := range tr.Outputs {
		if o.Kind == "pr" {
			prs++
		}
	}
	if prs != 1 {
		t.Errorf("pr outputs = %d, want 1 (deduped)", prs)
	}
	ch0 := tr.Segments[0].Chapter
	if ch0 == nil || !strings.HasPrefix(ch0.Point, "Build the widget") {
		t.Fatalf("chapter 0 point = %+v", ch0)
	}
	if len(ch0.IntentChanges) != 2 {
		t.Errorf("intent changes = %d, want 2", len(ch0.IntentChanges))
	}
	if len(ch0.Open) != 1 || !strings.Contains(ch0.Open[0], "PR review") {
		t.Errorf("open threads from the closing summary = %v", ch0.Open)
	}
	ch1 := tr.Segments[1].Chapter
	if ch1 == nil || !strings.Contains(ch1.Point, "container") {
		t.Errorf("chapter 1 point should come from the carried summary: %+v", ch1)
	}
	for _, u := range tr.Spans {
		if u.Kind == "user" && strings.HasPrefix(u.Text, "no —") && u.Flag != "correction" {
			t.Errorf("correction prompt not flagged")
		}
	}
}

func TestThreadsLinkOnClearOnly(t *testing.T) {
	now := time.Now().UnixMilli()
	a := &Session{ID: "claude:a", Tool: ToolClaude, SessionID: "a", Cwd: "/p", StartedAt: now - 3*3600_000, LastActivityAt: now - 3600_000}
	b := &Session{ID: "claude:b", Tool: ToolClaude, SessionID: "b", Cwd: "/p", StartedAt: now - 3600_000 + 20_000, LastActivityAt: now}
	c := &Session{ID: "claude:c", Tool: ToolClaude, SessionID: "c", Cwd: "/p", StartedAt: now - 10_000, LastActivityAt: now}
	setStartedWithClear("claude:b", true)
	defer setStartedWithClear("claude:b", false)
	threads, threadOf := computeThreads([]*Session{a, b, c}, map[string]string{})
	if threadOf["claude:a"] != threadOf["claude:b"] {
		t.Errorf("a and b must share a thread (b started with /clear 20 s after a ended)")
	}
	if threadOf["claude:c"] == threadOf["claude:a"] {
		t.Errorf("c must NOT join the thread: same cwd is no longer a link")
	}
	if len(threads) != 2 {
		t.Errorf("threads = %d, want 2", len(threads))
	}
	for _, th := range threads {
		if len(th.Sessions) == 2 && (len(th.Edges) != 1 || th.Edges[0].Kind != "clear") {
			t.Errorf("edge = %+v, want one clear edge", th.Edges)
		}
	}
}

func TestParseSummarySections(t *testing.T) {
	s := "Summary:\n1. **Primary Request and Intent:**\n   Do X.\n\n2. **Pending Tasks:**\n   - finish Y\n   - ship Z\n"
	sec := parseSummarySections(s)
	if !strings.Contains(sec["primary request and intent"], "Do X") {
		t.Errorf("sections = %v", sec)
	}
	if got := bullets(sectionLike(sec, "pending"), 5, 100); len(got) != 2 || got[0] != "finish Y" {
		t.Errorf("bullets = %v", got)
	}
	// newer summaries use markdown headings and bold bullet labels
	s2 := "## 1. Primary Request and Intent\nDo X.\n\n## 4. Errors and fixes\n- **Build failed**: fixed the import\n"
	sec2 := parseSummarySections(s2)
	if !strings.Contains(sec2["primary request and intent"], "Do X") {
		t.Errorf("## headings not parsed: %v", sec2)
	}
	if got := bullets(sectionLike(sec2, "errors and fixes"), 5, 100); len(got) != 1 || got[0] != "Build failed: fixed the import" {
		t.Errorf("bold residue: %v", got)
	}
}
