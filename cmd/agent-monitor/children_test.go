package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Synthetic transcripts only.

func childEnv(t *testing.T) (home string) {
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("AGENT_MONITOR_HOME", t.TempDir())
	prevStore, prevReg, prevHost := store, paneRegistry, hostRegistry
	store, paneRegistry, hostRegistry = nil, nil, nil
	t.Cleanup(func() { store, paneRegistry, hostRegistry = prevStore, prevReg, prevHost })
	childScans.mu.Lock()
	childScans.m = map[string]*childScan{}
	childScans.mu.Unlock()
	crossMatchCache.mu.Lock()
	crossMatchCache.m = map[string]crossHit{}
	crossMatchCache.mu.Unlock()
	subTranscripts.mu.Lock()
	subTranscripts.m = map[string]*subTranscript{}
	subTranscripts.mu.Unlock()
	return home
}

func cToolUse(id, name string, in map[string]any) map[string]any {
	return map[string]any{"type": "tool_use", "id": id, "name": name, "input": in}
}

func writeMeta(t *testing.T, path string, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// codexRollout writes a Codex rollout whose name carries the local start time, as Codex names them.
func codexRollout(t *testing.T, home, id string, sec int, recs ...any) {
	t.Helper()
	at := time.UnixMilli(parseTimestamp(tsAt(sec))).Local()
	dir := filepath.Join(home, ".codex", "sessions", at.Format("2006/01/02"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	jsonl(t, filepath.Join(dir, "rollout-"+at.Format("2006-01-02T15-04-05")+"-"+id+".jsonl"), recs...)
}

func childByKey(t *testing.T, res *SessionChildren, key string) *SessionChild {
	t.Helper()
	for _, c := range res.Children {
		if c.Key == key {
			return c
		}
	}
	t.Fatalf("no child %q in %d children", key, len(res.Children))
	return nil
}

func TestChildrenClaudeParent(t *testing.T) {
	home := childEnv(t)
	proj := filepath.Join(home, ".claude", "projects", "-w")
	parent := filepath.Join(proj, "p1.jsonl")
	_ = os.MkdirAll(filepath.Join(proj, "p1", "subagents"), 0o755)
	asyncRes := claudeRec("user", 3, map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "Async agent launched successfully."}}},
		map[string]any{"toolUseResult": map[string]any{"isAsync": true, "status": "async_launched", "agentId": "a1"}})
	note := cUser(40, "<task-notification>\n<task-id>a1</task-id>\n<tool-use-id>t1</tool-use-id>\n<status>completed</status>\n<summary>Agent finished</summary>\n</task-notification>")
	note["isMeta"] = true
	jsonl(t, parent,
		cUser(1, "Audit the parser"),
		cAsst(2, "m1", "tool_use", cToolUse("t1", "Agent", map[string]any{"description": "Parser audit", "subagent_type": "Explore", "prompt": "Read the parser\nand report", "run_in_background": true})),
		asyncRes,
		cAsst(4, "m2", "end_turn", map[string]any{"type": "text", "text": "Started."}),
		note,
		cUser(50, "Get a second opinion"),
		cAsst(51, "m3", "tool_use", cToolUse("t2", "Bash", map[string]any{"command": `cd /w && command codex exec -s read-only "Review the parser changes"`})),
		cResult(80, "t2", "done", false),
		cAsst(81, "m4", "tool_use", cToolUse("t3", "Bash", map[string]any{"command": "git commit -F - <<'EOF'\nrun codex exec later\nEOF"})),
		cResult(82, "t3", "ok", false),
		cAsst(83, "m5", "tool_use", cToolUse("t4", "mcp__agent-monitor__talk_to_agent", map[string]any{"recipient": "%7", "message": "please rebase"})),
		cResult(84, "t4", "delivered", false),
	)
	sub := filepath.Join(proj, "p1", "subagents")
	jsonl(t, filepath.Join(sub, "agent-a1.jsonl"),
		claudeRec("user", 3, map[string]any{"role": "user", "content": "Read the parser\nand report"}, map[string]any{"isSidechain": true, "agentId": "a1"}),
		claudeRec("assistant", 5, map[string]any{"id": "s1", "role": "assistant", "model": "claude-haiku-5", "content": []any{cToolUse("t9", "Agent", map[string]any{"description": "Deep dive", "prompt": "Check the lexer"})},
			"usage": map[string]any{"input_tokens": 10, "output_tokens": 20, "cache_read_input_tokens": 100}}, map[string]any{"isSidechain": true}),
		claudeRec("assistant", 30, map[string]any{"id": "s2", "role": "assistant", "model": "claude-haiku-5", "stop_reason": "end_turn", "content": []any{map[string]any{"type": "text", "text": "Report"}},
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 5}}, map[string]any{"isSidechain": true}),
	)
	writeMeta(t, filepath.Join(sub, "agent-a1.meta.json"), map[string]any{"agentType": "Explore", "description": "Parser audit", "toolUseId": "t1", "requestShape": "background"})
	jsonl(t, filepath.Join(sub, "agent-a2.jsonl"),
		claudeRec("user", 6, map[string]any{"role": "user", "content": "Check the lexer"}, map[string]any{"isSidechain": true, "agentId": "a2"}))
	writeMeta(t, filepath.Join(sub, "agent-a2.meta.json"), map[string]any{"agentType": "general-purpose", "description": "Deep dive", "toolUseId": "t9", "finished": true})
	codexRollout(t, home, "cx1", 52,
		map[string]any{"type": "session_meta", "timestamp": tsAt(52), "payload": map[string]any{"id": "cx1", "cwd": "/w", "originator": "codex_exec", "timestamp": tsAt(52)}},
		codexEv(52, map[string]any{"type": "user_message", "message": "Review the parser changes"}))
	codexRollout(t, home, "cx2", 53,
		map[string]any{"type": "session_meta", "timestamp": tsAt(53), "payload": map[string]any{"id": "cx2", "cwd": "/elsewhere", "timestamp": tsAt(53)}},
		codexEv(53, map[string]any{"type": "user_message", "message": "Unrelated work"}))

	ref := watchedRef{ID: "claude:p1", Tool: ToolClaude, RawID: "p1", Path: parent, Cwd: "/w", Resolved: true}
	res, err := sessionChildren(ref)
	if err != nil {
		t.Fatal(err)
	}
	if res.Count != 4 {
		b, _ := json.MarshalIndent(res, "", " ")
		t.Fatalf("want 4 children (subagent, nested subagent, codex, talk), got %d\n%s", res.Count, b)
	}
	a := childByKey(t, res, "t1")
	if a.Kind != "subagent" || a.AgentID != "a1" || a.Status != "completed" || !a.Background || !a.HasTranscript ||
		a.ParentItemID != "claude:p1/t1" || a.ParentMsgID != a.ParentRunID+".0" || a.ParentPrompt != "Audit the parser" ||
		a.Model != "claude-haiku-5" || a.InTokens != 111 || a.OutTokens != 25 || a.EndedAt != parseTimestamp(tsAt(40)) || a.SubagentType != "Explore" {
		t.Fatalf("subagent %+v", a)
	}
	n := childByKey(t, res, "t9")
	if n.ParentAgentID != "a1" || n.ParentRunID != a.ParentRunID || n.Status != "completed" || n.Prompt != "Check the lexer" {
		t.Fatalf("nested %+v", n)
	}
	c := childByKey(t, res, "t2")
	if c.Kind != "agent" || c.Agent != "codex" || c.SessionID != "codex:cx1" || c.Prompt != "Review the parser changes" ||
		c.LinkBasis != "command + time + cwd + prompt" || c.ParentPrompt != "Get a second opinion" || c.Status != "completed" {
		t.Fatalf("codex child %+v", c)
	}
	tk := childByKey(t, res, "t4")
	if tk.Kind != "talk" || tk.Description != "%7" || tk.Prompt != "please rebase" {
		t.Fatalf("talk %+v", tk)
	}

	// The parse resumes from its byte cursor as the parent grows.
	appendJSONL(t, parent, cUser(90, "One more"), cAsst(91, "m6", "tool_use", cToolUse("t5", "Task", map[string]any{"description": "Late helper", "prompt": "Help"})))
	res, err = sessionChildren(ref)
	if err != nil {
		t.Fatal(err)
	}
	late := childByKey(t, res, "t5")
	if late.Status != "stopped" || late.ParentPrompt != "One more" || late.Kind != "subagent" {
		t.Fatalf("late %+v", late)
	}

	items, err := subagentItems(ref, "a1", -1)
	if err != nil || len(items.Runs) != 1 || items.Runs[0].Prompt != "Read the parser\nand report" || !strings.Contains(kinds(items.Items), "agent") {
		t.Fatalf("subagent items %v %+v", err, items)
	}
	if _, err := subagentItems(ref, "../p1", -1); err == nil {
		t.Fatal("path escape accepted")
	}
}

func TestChildrenCodexParent(t *testing.T) {
	home := childEnv(t)
	ms := func(sec int) int64 { return parseTimestamp(tsAt(sec)) }
	codexRollout(t, home, "par", 0,
		map[string]any{"type": "session_meta", "timestamp": tsAt(0), "payload": map[string]any{"id": "par", "cwd": "/w"}},
		map[string]any{"type": "turn_context", "timestamp": tsAt(0), "payload": map[string]any{"cwd": "/w", "model": "gpt-6"}},
		codexEv(1, map[string]any{"type": "user_message", "message": "Split the audit"}),
		codexResp(2, map[string]any{"type": "function_call", "name": "spawn_agent", "call_id": "c1", "arguments": `{"agent_type":"explorer","message":"Audit the repos"}`}),
		codexResp(2, map[string]any{"type": "function_call_output", "call_id": "c1", "output": `{"agent_id":"kid1","nickname":"Mill"}`}),
		codexResp(9, map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "<subagent_notification>\n{\"agent_path\":\"kid1\",\"status\":{\"completed\":\"done\"}}\n</subagent_notification>"}}}),
		codexEv(10, map[string]any{"type": "user_message", "message": "Ask Claude too"}),
		codexDone(20, map[string]any{"type": "CommandExecution", "id": "ce1", "command": []any{"/bin/zsh", "-lc", `claude -p "Summarise the diff please" --output-format json`}, "cwd": "file:///w",
			"status": "completed", "exit_code": 0, "duration": map[string]any{"secs": 8, "nanos": 0}}, ms(11)),
	)
	_ = os.MkdirAll(filepath.Join(home, ".claude", "projects", "-w"), 0o755)
	jsonl(t, filepath.Join(home, ".claude", "projects", "-w", "cl1.jsonl"),
		map[string]any{"type": "user", "timestamp": tsAt(12), "cwd": "/w", "message": map[string]any{"role": "user", "content": "Summarise the diff please"}})
	ref := resolveWatched("codex:par")
	if !ref.Resolved {
		t.Fatal("parent not resolved")
	}
	res, err := sessionChildren(ref)
	if err != nil {
		t.Fatal(err)
	}
	if res.Count != 2 {
		b, _ := json.MarshalIndent(res, "", " ")
		t.Fatalf("want 2 children, got %s", b)
	}
	s := childByKey(t, res, "c1")
	if s.Kind != "subagent" || s.SessionID != "codex:kid1" || s.Status != "completed" || s.SubagentType != "explorer" || s.ParentPrompt != "Split the audit" || s.ParentItemID != "codex:par/c1" {
		t.Fatalf("codex subagent %+v", s)
	}
	c := childByKey(t, res, "ce1")
	if c.Agent != "claude" || c.SessionID != "claude:cl1" || c.ParentPrompt != "Ask Claude too" || !strings.HasPrefix(c.LinkBasis, "command + time + cwd") || c.StartedAt != ms(12) {
		t.Fatalf("claude child %+v", c)
	}

	rec := httptest.NewRecorder()
	if !watchedSessionRoute(rec, httptest.NewRequest("GET", "/x", nil), "codex:par/children") || rec.Code != 200 || !strings.Contains(rec.Body.String(), `"count":2`) {
		t.Fatalf("route %d %s", rec.Code, rec.Body.String())
	}
}

func TestAgentInCommand(t *testing.T) {
	cases := map[string]Tool{
		`codex exec "fix it"`:                               ToolCodex,
		`cd /w && command codex exec -s read-only - < p.md`: ToolCodex,
		`~/.local/bin/codex -m gpt-6 exec "x"`:              ToolCodex,
		`(command codex review --base main > out 2>&1 &)`:   ToolCodex,
		`claude -p "summarise"`:                             ToolClaude,
		`timeout 600 claude --model opus -p "x"`:            ToolClaude,
		`git commit -m "codex exec support"`:                "",
		"cat > f <<'EOF'\ncodex exec here\nEOF":             "",
		`grep -n claude -p file.go`:                         "",
		`ls ~/.codex/sessions`:                              "",
		`claude mcp list`:                                   "",
	}
	for cmd, want := range cases {
		if got := agentInCommand(cmd); got != want {
			t.Errorf("%q: got %q want %q", cmd, got, want)
		}
	}
	if p := commandPrompt(`codex exec --full-auto "Review the parser changes" | tee x`, ToolCodex); p != "Review the parser changes" {
		t.Errorf("prompt %q", p)
	}
}

// An orphan subagent lands in the turn open at its start, found from timestamps kept by the scan.
func TestChildOrphanParentByTimestamp(t *testing.T) {
	home := childEnv(t)
	proj := filepath.Join(home, ".claude", "projects", "-w")
	parent := filepath.Join(proj, "p2.jsonl")
	sub := filepath.Join(proj, "p2", "subagents")
	_ = os.MkdirAll(sub, 0o755)
	jsonl(t, parent, cUser(1, "First"), cAsst(2, "m1", "end_turn", map[string]any{"type": "text", "text": "ok"}),
		cUser(10, "Second"), cAsst(11, "m2", "end_turn", map[string]any{"type": "text", "text": "ok"}),
		cUser(20, "Third"))
	jsonl(t, filepath.Join(sub, "agent-o1.jsonl"),
		claudeRec("user", 12, map[string]any{"role": "user", "content": "orphan work"}, map[string]any{"isSidechain": true}))
	ref := watchedRef{ID: "claude:p2", Tool: ToolClaude, RawID: "p2", Path: parent, Cwd: "/w", Resolved: true}
	res, err := sessionChildren(ref)
	if err != nil {
		t.Fatal(err)
	}
	o := childByKey(t, res, "agent-o1")
	if o.ParentPrompt != "Second" || o.ParentRunID == "" {
		t.Fatalf("orphan %+v", o)
	}
	s := childScanFor(parent)
	defer s.release()
	if len(s.tsVal) != 3 || s.tsVal[1] != parseTimestamp(tsAt(10)) {
		t.Fatalf("turn timestamps %v", s.tsVal)
	}
	early := &SessionChild{}
	s.setParentByTs(ref.ID, early, parseTimestamp(tsAt(0)))
	late := &SessionChild{}
	s.setParentByTs(ref.ID, late, parseTimestamp(tsAt(99)))
	if early.ParentRunID != "" || late.ParentPrompt != "Third" {
		t.Fatalf("early %+v late %+v", early, late)
	}

	// The timestamps grow with the incremental scan.
	appendJSONL(t, parent, cUser(30, "Fourth"))
	jsonl(t, filepath.Join(sub, "agent-o2.jsonl"),
		claudeRec("user", 31, map[string]any{"role": "user", "content": "late orphan"}, map[string]any{"isSidechain": true}))
	if res, err = sessionChildren(ref); err != nil {
		t.Fatal(err)
	}
	if o2 := childByKey(t, res, "agent-o2"); o2.ParentPrompt != "Fourth" {
		t.Fatalf("late orphan %+v", o2)
	}
}

// The subagent transcript is parsed once, then read from a byte cursor; a page holds the newest subItemsCap items.
func TestChildSubagentTranscriptCursor(t *testing.T) {
	home := childEnv(t)
	proj := filepath.Join(home, ".claude", "projects", "-w")
	parent := filepath.Join(proj, "p3.jsonl")
	sub := filepath.Join(proj, "p3", "subagents")
	_ = os.MkdirAll(sub, 0o755)
	jsonl(t, parent, cUser(1, "Go"))
	recs := []any{claudeRec("user", 2, map[string]any{"role": "user", "content": "Do the work"}, map[string]any{"isSidechain": true})}
	for i := 0; i < subItemsCap+50; i++ {
		r := cAsst(3, fmt.Sprintf("s%d", i), "", map[string]any{"type": "text", "text": fmt.Sprintf("step %d", i)})
		r["isSidechain"] = true
		recs = append(recs, r)
	}
	path := filepath.Join(sub, "agent-b1.jsonl")
	jsonl(t, path, recs...)
	ref := watchedRef{ID: "claude:p3", Tool: ToolClaude, RawID: "p3", Path: parent, Cwd: "/w", Resolved: true}
	first, err := subagentItems(ref, "b1", -1)
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(path)
	last := first.Items[len(first.Items)-1]
	if len(first.Items) != subItemsCap || !first.More || first.Cursor != fi.Size() || first.Reset || len(first.Runs) != 1 || last.Text != fmt.Sprintf("step %d", subItemsCap+49) {
		t.Fatalf("first page: %d items more=%v cursor=%d/%d reset=%v runs=%d last=%q", len(first.Items), first.More, first.Cursor, fi.Size(), first.Reset, len(first.Runs), last.Text)
	}
	cached := subTranscriptFor(path).p

	same, err := subagentItems(ref, "b1", first.Cursor)
	if err != nil || len(same.Items) != 0 || same.Reset || same.Cursor != first.Cursor || subTranscriptFor(path).p != cached {
		t.Fatalf("idle poll %v %+v", err, same)
	}

	a := cAsst(4, "new1", "", map[string]any{"type": "text", "text": "step new"})
	a["isSidechain"] = true
	appendJSONL(t, path, a)
	next, err := subagentItems(ref, "b1", first.Cursor)
	if err != nil || len(next.Items) != 1 || next.Items[0].Text != "step new" || next.Cursor <= first.Cursor || next.Reset || subTranscriptFor(path).p != cached {
		t.Fatalf("incremental poll %v %+v", err, next)
	}

	stale, err := subagentItems(ref, "b1", next.Cursor+1000)
	if err != nil || !stale.Reset || len(stale.Items) != subItemsCap {
		t.Fatalf("cursor past the end %v reset=%v items=%d", err, stale.Reset, len(stale.Items))
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", fmt.Sprintf("/api/session/claude:p3/children/b1/items?after=%d", next.Cursor), nil)
	if !watchedSessionRoute(rec, req, "claude:p3/children/b1/items") {
		t.Fatal("route not served")
	}
	var got WatchedItems
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &got) != nil || len(got.Items) != 0 || got.Cursor != next.Cursor {
		t.Fatalf("after route %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	childrenRoute(rec, httptest.NewRequest("GET", "/x?after=-4", nil), "claude:p3/children/b1/items")
	if rec.Code != 400 {
		t.Fatalf("bad after %d", rec.Code)
	}
}

// A running background command links only to a child that started within crossStartWindow of it.
func TestChildCrossWindowCap(t *testing.T) {
	home := childEnv(t)
	proj := filepath.Join(home, ".claude", "projects", "-w")
	parent := filepath.Join(proj, "p4.jsonl")
	_ = os.MkdirAll(proj, 0o755)
	jsonl(t, parent, cUser(1, "Start a reviewer"),
		cAsst(2, "m1", "tool_use", cToolUse("b1", "Bash", map[string]any{"command": `codex exec "Review the branch now" &`, "run_in_background": true})))
	codexRollout(t, home, "late", 2+120,
		map[string]any{"type": "session_meta", "timestamp": tsAt(122), "payload": map[string]any{"id": "late", "cwd": "/w", "timestamp": tsAt(122)}},
		codexEv(122, map[string]any{"type": "user_message", "message": "Unrelated later work"}))
	ref := watchedRef{ID: "claude:p4", Tool: ToolClaude, RawID: "p4", Path: parent, Cwd: "/w", Resolved: true}
	res, err := sessionChildren(ref)
	if err != nil {
		t.Fatal(err)
	}
	if c := childByKey(t, res, "b1"); c.SessionID != "" {
		t.Fatalf("later session in the same cwd linked: %+v", c)
	}

	// A miss is cached for a short time, then the match runs again.
	codexRollout(t, home, "kid", 4,
		map[string]any{"type": "session_meta", "timestamp": tsAt(4), "payload": map[string]any{"id": "kid", "cwd": "/w", "timestamp": tsAt(4)}},
		codexEv(4, map[string]any{"type": "user_message", "message": "Review the branch now"}))
	if res, _ = sessionChildren(ref); childByKey(t, res, "b1").SessionID != "" {
		t.Fatal("cached miss not used")
	}
	key := parent + "|b1"
	crossMatchCache.mu.Lock()
	h := crossMatchCache.m[key]
	h.until = time.Now().UnixMilli() - 1
	crossMatchCache.m[key] = h
	crossMatchCache.mu.Unlock()
	res, _ = sessionChildren(ref)
	c := childByKey(t, res, "b1")
	if c.SessionID != "codex:kid" || c.StartedAt != parseTimestamp(tsAt(4)) || !strings.Contains(c.LinkBasis, "prompt") {
		t.Fatalf("child not linked after the miss expired: %+v", c)
	}

	// A hit is kept: the rollouts are not read again.
	_ = os.RemoveAll(filepath.Join(home, ".codex"))
	res, _ = sessionChildren(ref)
	if c2 := childByKey(t, res, "b1"); c2.SessionID != "codex:kid" || c2.StartedAt != c.StartedAt || c2.LinkBasis != c.LinkBasis {
		t.Fatalf("cached hit %+v", c2)
	}
}

// One scan per path: an entry in use is never evicted, and parallel requests share it.
func TestChildScanEvictionRace(t *testing.T) {
	home := childEnv(t)
	proj := filepath.Join(home, ".claude", "projects", "-w")
	_ = os.MkdirAll(filepath.Join(proj, "hot", "subagents"), 0o755)
	hot := filepath.Join(proj, "hot.jsonl")
	jsonl(t, hot, cUser(1, "Go"), cAsst(2, "m1", "tool_use", cToolUse("t1", "Agent", map[string]any{"description": "x", "prompt": "y"})))
	jsonl(t, filepath.Join(proj, "hot", "subagents", "agent-a1.jsonl"),
		claudeRec("user", 2, map[string]any{"role": "user", "content": "y"}, map[string]any{"isSidechain": true}))
	writeMeta(t, filepath.Join(proj, "hot", "subagents", "agent-a1.meta.json"), map[string]any{"toolUseId": "t1"})

	held := childScanFor(hot)
	for i := 0; i < childScanMax*2; i++ {
		childScanFor(fmt.Sprintf("/none/%d", i)).release()
	}
	if again := childScanFor(hot); again != held {
		t.Fatal("a scan in use was evicted")
	} else {
		again.release()
	}
	held.release()

	hotRef := watchedRef{ID: "claude:hot", Tool: ToolClaude, RawID: "hot", Path: hot, Cwd: "/w", Resolved: true}
	var wg sync.WaitGroup
	for i := 0; i < childScanMax+16; i++ {
		p := filepath.Join(proj, fmt.Sprintf("c%d.jsonl", i))
		jsonl(t, p, cUser(1, "Hi"))
		ref := watchedRef{ID: fmt.Sprintf("claude:c%d", i), Tool: ToolClaude, RawID: fmt.Sprintf("c%d", i), Path: p, Cwd: "/w", Resolved: true}
		wg.Add(3)
		go func() { defer wg.Done(); _, _ = sessionChildren(ref) }()
		go func() { defer wg.Done(); _, _ = sessionChildren(hotRef) }()
		go func() { defer wg.Done(); _, _ = subagentItems(hotRef, "a1", -1) }()
	}
	wg.Wait()
	res, err := sessionChildren(hotRef)
	if err != nil || childByKey(t, res, "t1").AgentID != "a1" {
		t.Fatalf("hot parent after the race: %v", err)
	}
}
