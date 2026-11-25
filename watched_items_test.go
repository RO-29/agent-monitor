package main

import (
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Synthetic transcripts only: no real session content in the repo.

func claudeRec(typ string, sec int, msg map[string]any, extra map[string]any) map[string]any {
	m := map[string]any{"type": typ, "timestamp": tsAt(sec), "cwd": "/w", "message": msg}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func cUser(sec int, text string) map[string]any {
	return claudeRec("user", sec, map[string]any{"role": "user", "content": text}, nil)
}

func cAsst(sec int, id string, stop string, parts ...any) map[string]any {
	return claudeRec("assistant", sec, map[string]any{"id": id, "role": "assistant", "model": "claude-sonnet-5", "stop_reason": stop, "content": parts,
		"usage": map[string]any{"input_tokens": 5, "output_tokens": 40, "cache_read_input_tokens": 1000, "cache_creation_input_tokens": 100}}, nil)
}

func cResult(sec int, id, text string, isErr bool) map[string]any {
	return claudeRec("user", sec, map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": id, "is_error": isErr, "content": text}}}, nil)
}

func appendJSONL(t *testing.T, path string, recs ...any) {
	t.Helper()
	tmp := path + ".part"
	jsonl(t, tmp, recs...)
	b, _ := os.ReadFile(tmp)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(b); err != nil {
		t.Fatal(err)
	}
}

func fileSize(t *testing.T, p string) int64 {
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return st.Size()
}

func kinds(items []*OItem) string {
	var k []string
	for _, it := range items {
		k = append(k, it.Kind)
	}
	return strings.Join(k, ",")
}

func claudeRef(path string) watchedRef {
	return watchedRef{ID: "claude:s1", Tool: ToolClaude, RawID: "s1", Path: path, Resolved: true}
}

func TestWatchedItemsClaude(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s1.jsonl")
	jsonl(t, path,
		claudeRec("user", 0, map[string]any{"role": "user", "content": "<system-reminder>x</system-reminder>"}, map[string]any{"isMeta": true}),
		cUser(1, "Fix the build"),
		cAsst(2, "m1", "", map[string]any{"type": "thinking", "thinking": "look first"}),
		cAsst(3, "m1", "", map[string]any{"type": "text", "text": "Running the tests.\n\nFull text, two paragraphs."}),
		cAsst(4, "m1", "tool_use", map[string]any{"type": "tool_use", "id": "t1", "name": "Bash", "input": map[string]any{"command": "go test ./...", "description": "tests"}}),
		cResult(9, "t1", "Exit code 2\nFAIL", true),
		cAsst(10, "m2", "tool_use", map[string]any{"type": "tool_use", "id": "t2", "name": "Edit", "input": map[string]any{"file_path": "/w/a.go", "old_string": "a\nb", "new_string": "a\nb\nc"}}),
		cResult(11, "t2", "ok", false),
		cAsst(12, "m3", "end_turn", map[string]any{"type": "text", "text": "Done."}),
		map[string]any{"type": "system", "subtype": "turn_duration", "timestamp": tsAt(12)},
		cUser(20, "Now commit"),
	)
	res, err := watchedItemsFor(claudeRef(path), -1, -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Runs) != 2 || res.More || res.Earlier != nil {
		t.Fatalf("runs=%d more=%v", len(res.Runs), res.More)
	}
	if got := kinds(res.Items); got != "user,reasoning,agent,command,fileChange,agent,user" {
		t.Fatalf("kinds %s", got)
	}
	cmd := res.Items[3]
	if cmd.Status != "failed" || cmd.ExitCode == nil || *cmd.ExitCode != 2 || cmd.EndedAt-cmd.StartedAt != 5000 || cmd.Command != "go test ./..." {
		t.Fatalf("command %+v", cmd)
	}
	if fc := res.Items[4]; len(fc.Files) != 1 || fc.Files[0].Add != 3 || fc.Files[0].Del != 2 {
		t.Fatalf("fileChange %+v", fc.Files)
	}
	if res.Items[2].Text != "Running the tests.\n\nFull text, two paragraphs." {
		t.Fatal("agent text collapsed")
	}
	r1 := res.Runs[0]
	if r1.Status != "completed" || r1.Prompt != "Fix the build" || r1.Index != 1 || r1.Usage == nil {
		t.Fatalf("run1 %+v", r1)
	}
	// m1 repeats its usage on three lines: count once (m1, m2, m3 = 3 messages).
	if r1.Usage.OutTokens != 120 || r1.Usage.InTokens != 3*1105 || r1.Usage.CostUSD <= 0 {
		t.Fatalf("usage %+v", *r1.Usage)
	}
	if res.Runs[1].Status != "running" || res.Cursor != fileSize(t, path) {
		t.Fatalf("run2 %s cursor %d", res.Runs[1].Status, res.Cursor)
	}

	// Incremental: a tool call starts, then finishes.
	appendJSONL(t, path, cAsst(21, "m4", "tool_use", map[string]any{"type": "tool_use", "id": "t3", "name": "Bash", "input": map[string]any{"command": "git commit"}}))
	inc, _ := watchedItemsFor(claudeRef(path), res.Cursor, -1)
	if len(inc.Items) != 1 || inc.Items[0].Status != "started" || len(inc.Runs) != 1 {
		t.Fatalf("increment 1: %s runs=%d", kinds(inc.Items), len(inc.Runs))
	}
	appendJSONL(t, path, cResult(23, "t3", "ok", false))
	inc2, _ := watchedItemsFor(claudeRef(path), inc.Cursor, -1)
	if len(inc2.Items) != 1 || inc2.Items[0].ID != inc.Items[0].ID || inc2.Items[0].Status != "completed" || *inc2.Items[0].ExitCode != 0 {
		t.Fatalf("increment 2: %+v", inc2.Items)
	}
	none, _ := watchedItemsFor(claudeRef(path), inc2.Cursor, -1)
	if len(none.Items) != 0 || len(none.Runs) != 0 {
		t.Fatal("no new lines must return nothing")
	}

	// A rewritten (shorter) file resets the cursor.
	jsonl(t, path, cUser(1, "fresh"))
	rs, _ := watchedItemsFor(claudeRef(path), inc2.Cursor, -1)
	if !rs.Reset || len(rs.Runs) != 1 || rs.Runs[0].Prompt != "fresh" {
		t.Fatalf("reset %+v", rs)
	}
}

func TestWatchedItemsPaging(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s1.jsonl")
	var recs []any
	for i := 0; i < 100; i++ {
		recs = append(recs, cUser(i*10, fmt.Sprintf("prompt %d", i)),
			cAsst(i*10+1, fmt.Sprintf("m%d", i), "end_turn", map[string]any{"type": "text", "text": fmt.Sprintf("reply %d", i)}))
	}
	jsonl(t, path, recs...)
	ref := claudeRef(path)
	ref.ID = "claude:paging"
	first, _ := watchedItemsFor(ref, -1, -1)
	if len(first.Runs) != 40 || !first.More || first.Earlier == nil || first.Runs[0].Prompt != "prompt 60" || first.Runs[0].Index != 61 {
		t.Fatalf("first page: %d runs, first %q", len(first.Runs), first.Runs[0].Prompt)
	}
	p2, _ := watchedItemsFor(ref, -1, *first.Earlier)
	if len(p2.Runs) != 40 || p2.Runs[0].Prompt != "prompt 20" || p2.Runs[39].Prompt != "prompt 59" || !p2.More {
		t.Fatalf("page 2: %d runs %q..%q", len(p2.Runs), p2.Runs[0].Prompt, p2.Runs[len(p2.Runs)-1].Prompt)
	}
	p3, _ := watchedItemsFor(ref, -1, *p2.Earlier)
	if len(p3.Runs) != 20 || p3.More || p3.Earlier != nil {
		t.Fatalf("page 3: %d runs more=%v", len(p3.Runs), p3.More)
	}
	// An old cursor rematerialises the turn that holds it.
	old, _ := watchedItemsFor(ref, *p2.Earlier, -1)
	if len(old.Runs) != 80 {
		t.Fatalf("after an old cursor: %d runs", len(old.Runs))
	}
}

func codexEv(sec int, payload map[string]any) map[string]any {
	return map[string]any{"type": "event_msg", "timestamp": tsAt(sec), "payload": payload}
}

func codexResp(sec int, payload map[string]any) map[string]any {
	return map[string]any{"type": "response_item", "timestamp": tsAt(sec), "payload": payload}
}

func codexDone(sec int, item map[string]any, startMs int64) map[string]any {
	return codexEv(sec, map[string]any{"type": "item_completed", "item": item, "started_at_ms": startMs})
}

func TestWatchedItemsCodexItemCompleted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-x.jsonl")
	ms := func(sec int) int64 { return parseTimestamp(tsAt(sec)) }
	userText := []any{map[string]any{"type": "input_text", "text": "Fix the parser"}}
	jsonl(t, path,
		map[string]any{"type": "session_meta", "timestamp": tsAt(0), "payload": map[string]any{"id": "c1"}},
		codexEv(0, map[string]any{"type": "task_started", "turn_id": "u1"}),
		codexResp(0, map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "# AGENTS.md instructions for /w"}}}),
		map[string]any{"type": "turn_context", "timestamp": tsAt(0), "payload": map[string]any{"model": "gpt-6", "cwd": "/w"}},
		codexResp(1, map[string]any{"type": "message", "role": "user", "content": userText}),
		codexDone(1, map[string]any{"type": "UserMessage", "id": "um1", "content": []any{map[string]any{"type": "text", "text": "Fix the parser"}}}, ms(1)),
		codexResp(2, map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "I will look."}}}),
		codexDone(2, map[string]any{"type": "AgentMessage", "id": "am1", "content": []any{map[string]any{"type": "Text", "text": "I will look."}}}, ms(2)),
		codexDone(3, map[string]any{"type": "Reasoning", "id": "r1", "summary_text": []any{"Plan the fix"}}, ms(3)),
		codexResp(3, map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": "x1", "input": "tools.exec_command(...)"}),
		codexDone(6, map[string]any{"type": "CommandExecution", "id": "ce1", "command": []any{"/bin/zsh", "-lc", "go test"}, "cwd": "file:///w",
			"status": "completed", "aggregated_output": "FAIL", "exit_code": 1, "duration": map[string]any{"secs": 2, "nanos": 500000000}}, ms(4)),
		codexDone(7, map[string]any{"type": "FileChange", "id": "fc1", "status": "completed", "changes": map[string]any{
			"/w/p.go":   map[string]any{"type": "update", "unified_diff": "@@ -1 +1,2 @@\n-a\n+a\n+b\n"},
			"/w/new.go": map[string]any{"type": "add", "content": "x\ny\nz\n"}}}, ms(7)),
		codexDone(8, map[string]any{"type": "McpToolCall", "id": "mc1", "server": "graph", "tool": "query", "status": "completed",
			"result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}}, "duration": map[string]any{"secs": 1}}, ms(7)),
		codexDone(9, map[string]any{"type": "Extension", "kind": "web.search", "id": "ws1", "query": "go json", "action": map[string]any{}}, ms(9)),
		codexEv(9, map[string]any{"type": "token_count", "info": map[string]any{
			"total_token_usage": map[string]any{"input_tokens": 1000, "cached_input_tokens": 600, "output_tokens": 50},
			"last_token_usage":  map[string]any{"input_tokens": 1000, "cached_input_tokens": 600, "output_tokens": 50}}}),
		codexEv(10, map[string]any{"type": "task_complete", "last_agent_message": "I will look."}),
		codexResp(20, map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "thanks"}}}),
		codexDone(20, map[string]any{"type": "UserMessage", "id": "um2", "content": []any{map[string]any{"type": "text", "text": "thanks"}}}, ms(20)),
	)
	ref := watchedRef{ID: "codex:c1", Tool: ToolCodex, RawID: "c1", Path: path, Resolved: true}
	res, err := watchedItemsFor(ref, -1, -1)
	if err != nil {
		t.Fatal(err)
	}
	if got := kinds(res.Items); got != "user,agent,reasoning,command,fileChange,mcp,web,user" {
		t.Fatalf("kinds %s", got)
	}
	cmd := res.Items[3]
	if cmd.Status != "failed" || *cmd.ExitCode != 1 || cmd.Command != "go test" || cmd.Cwd != "/w" || cmd.EndedAt-cmd.StartedAt != 2500 {
		t.Fatalf("command %+v", cmd)
	}
	files := map[string]OFile{}
	for _, f := range res.Items[4].Files {
		files[f.Path] = f
	}
	if files["/w/p.go"].Add != 2 || files["/w/p.go"].Del != 1 || files["/w/new.go"].Add != 3 {
		t.Fatalf("files %+v", files)
	}
	r := res.Runs[0]
	if len(res.Runs) != 2 || r.Status != "completed" || r.Usage == nil || r.Usage.InTokens != 1000 || r.Usage.OutTokens != 50 || r.Usage.CostUSD <= 0 {
		t.Fatalf("runs %+v usage %+v", r, r.Usage)
	}
}

func TestWatchedItemsCodexLegacy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-y.jsonl")
	jsonl(t, path,
		map[string]any{"type": "turn_context", "timestamp": tsAt(0), "payload": map[string]any{"model": "gpt-5-codex"}},
		codexResp(1, map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "list files"}}}),
		codexEv(1, map[string]any{"type": "user_message", "message": "list files"}),
		codexEv(2, map[string]any{"type": "agent_reasoning", "text": "simple"}),
		codexResp(3, map[string]any{"type": "function_call", "name": "exec_command", "call_id": "c1", "arguments": `{"cmd":"ls","workdir":"/w"}`}),
		codexResp(4, map[string]any{"type": "function_call_output", "call_id": "c1", "output": "a\nb"}),
		codexEv(4, map[string]any{"type": "exec_command_end", "call_id": "c1", "exit_code": 0, "aggregated_output": "a\nb"}),
		codexResp(5, map[string]any{"type": "custom_tool_call", "name": "apply_patch", "call_id": "c2", "input": "*** Begin Patch\n*** Update File: /w/a\n@@\n-x\n+y\n+z\n*** End Patch"}),
		codexEv(6, map[string]any{"type": "patch_apply_end", "call_id": "c2", "success": true}),
		codexEv(7, map[string]any{"type": "turn_aborted"}),
	)
	ref := watchedRef{ID: "codex:y", Tool: ToolCodex, RawID: "y", Path: path, Resolved: true}
	res, _ := watchedItemsFor(ref, -1, -1)
	if got := kinds(res.Items); got != "user,reasoning,command,fileChange" {
		t.Fatalf("kinds %s", got)
	}
	if c := res.Items[2]; c.Status != "completed" || *c.ExitCode != 0 || c.Cwd != "/w" || c.Command != "ls" {
		t.Fatalf("command %+v", c)
	}
	if f := res.Items[3].Files; len(f) != 1 || f[0].Add != 2 || f[0].Del != 1 {
		t.Fatalf("patch %+v", f)
	}
	if len(res.Runs) != 1 || res.Runs[0].Status != "interrupted" {
		t.Fatalf("runs %+v", res.Runs)
	}
}

func TestWatchedRoutesResolveAnyPrefix(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("AGENT_MONITOR_HOME", t.TempDir())
	id := "abcdef12-3456-4789-8abc-def012345678"
	dir := filepath.Join(cfg, "projects", "-w")
	_ = os.MkdirAll(dir, 0o755)
	jsonl(t, filepath.Join(dir, id+".jsonl"), cUser(1, "hi"))
	prevStore, prevReg := store, paneRegistry
	store, paneRegistry = nil, NewPaneRegistry()
	t.Cleanup(func() { store, paneRegistry = prevStore, prevReg })

	rec := httptest.NewRecorder()
	if !watchedSessionRoute(rec, httptest.NewRequest("GET", "/api/session/codex:"+id+"/items", nil), "codex:"+id+"/items") {
		t.Fatal("route not handled")
	}
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"id":"claude:`+id) {
		t.Fatalf("items: %d %s", rec.Code, rec.Body.String())
	}

	prevList := listPanesFn
	listPanesFn = func() ([]TmuxPane, error) {
		return []TmuxPane{{PaneID: "%9", SessionName: "work", WindowID: "@2", PanePID: 42, CurrentCommand: "node"}}, nil
	}
	t.Cleanup(func() { listPanesFn = prevList })
	paneRegistry.byAgentID["codex:"+id] = &PaneRegistration{AgentID: "codex:" + id, Tool: ToolCodex, SessionID: id, PaneID: "%9"}
	p := watchedPane("claude:" + id)
	if !p.Registered || !p.Live || !p.Sendable || p.TmuxTarget != "work:@2.%9" || p.Pid != 42 {
		t.Fatalf("pane %+v", p)
	}
	listPanesFn = func() ([]TmuxPane, error) {
		return []TmuxPane{{PaneID: "%9", SessionName: "work", WindowID: "@2", CurrentCommand: "zsh"}}, nil
	}
	if p := watchedPane(id); !p.Live || p.Sendable {
		t.Fatalf("shell in pane must not be sendable: %+v", p)
	}
	if p := watchedPane("claude:nope"); p.Registered || p.Sendable {
		t.Fatalf("unknown %+v", p)
	}
}

func TestWatchedItemsUserBangCommands(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bang.jsonl")
	jsonl(t, path,
		cUser(1, "Deploy it"),
		cAsst(2, "m1", "end_turn", map[string]any{"type": "text", "text": "Run the command."}),
		map[string]any{"type": "system", "subtype": "turn_duration", "timestamp": tsAt(2)},
		claudeRec("user", 3, map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "<bash-input>cd x &amp;&amp; make</bash-input>\n"},
			map[string]any{"type": "text", "text": "<bash-stdout>built ok\n</bash-stdout><bash-stderr></bash-stderr>"}}}, nil),
		cUser(4, "<bash-input>ls nope</bash-input>"),
		cUser(5, "<bash-stdout></bash-stdout><bash-stderr>ls: nope: No such file</bash-stderr>"),
	)
	res, err := watchedItemsFor(claudeRef(path), -1, -1)
	if err != nil {
		t.Fatal(err)
	}
	var bangs []*OItem
	for _, it := range res.Items {
		if it.By == "user" {
			bangs = append(bangs, it)
		}
	}
	if len(bangs) != 2 {
		t.Fatalf("want 2 user commands, got %d (%s)", len(bangs), kinds(res.Items))
	}
	if b := bangs[0]; b.Kind != "command" || b.Command != "cd x && make" || b.Output != "built ok" || b.Status != "completed" {
		t.Fatalf("combined line: %+v", b)
	}
	if b := bangs[1]; b.Command != "ls nope" || b.Output != "ls: nope: No such file" {
		t.Fatalf("split lines: %+v", b)
	}
	// Clients key items by id: a ! turn must not hold an empty prompt item with the command's id.
	seen := map[string]bool{}
	for _, it := range res.Items {
		if seen[it.ID] {
			t.Fatalf("duplicate id %s (%s)", it.ID, kinds(res.Items))
		}
		seen[it.ID] = true
		if it.Kind == "user" && strings.TrimSpace(it.Text) == "" {
			t.Fatalf("empty user item (%s)", kinds(res.Items))
		}
	}
}

func TestWatchedItemsIdleEviction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s1.jsonl")
	jsonl(t, path, cUser(1, "Fix the build"),
		cAsst(4, "m1", "tool_use", map[string]any{"type": "tool_use", "id": "t1", "name": "Bash", "input": map[string]any{"command": "go test"}}))
	ref := claudeRef(path)
	res, err := watchedItemsFor(ref, -1, -1)
	if err != nil {
		t.Fatal(err)
	}
	if evictIdleWatched(time.Now()) != 0 {
		t.Fatal("a session read just now must stay")
	}
	if evictIdleWatched(time.Now().Add(watchedIdleTTL+time.Minute)) == 0 {
		t.Fatal("an idle session must go")
	}
	// A client that kept its cursor gets only the change, with no reset, after the rebuild.
	appendJSONL(t, path, cResult(9, "t1", "ok", false))
	inc, err := watchedItemsFor(ref, res.Cursor, -1)
	if err != nil || inc.Reset || len(inc.Items) != 1 || inc.Items[0].Status != "completed" {
		t.Fatalf("after eviction: %+v %v", inc, err)
	}
}

func TestWatchedItemsClaudeQueuedMessage(t *testing.T) {
	queued := func(sec int, prompt any, origin string) map[string]any {
		a := map[string]any{"type": "queued_command", "prompt": prompt, "commandMode": "prompt"}
		if origin != "" {
			a["origin"] = map[string]any{"kind": origin}
		}
		return map[string]any{"type": "attachment", "timestamp": tsAt(sec), "attachment": a}
	}
	path := filepath.Join(t.TempDir(), "s1.jsonl")
	jsonl(t, path,
		cUser(1, "Fix the build"),
		cAsst(2, "m1", "tool_use", map[string]any{"type": "tool_use", "id": "t1", "name": "Bash", "input": map[string]any{"command": "make"}}),
		queued(3, "also run the linter", "human"),
		queued(3, "<agent-message from=\"x\">report</agent-message>", "agent"),
		queued(4, []any{map[string]any{"type": "text", "text": "and push it"}}, ""),
		cResult(5, "t1", "ok", false),
		cAsst(6, "m2", "end_turn", map[string]any{"type": "text", "text": "Done, linted and pushed."}),
	)
	res, err := watchedItemsFor(claudeRef(path), -1, -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Runs) != 1 {
		t.Fatalf("a message typed mid-turn stays in its turn: runs=%d", len(res.Runs))
	}
	if got := kinds(res.Items); got != "user,command,user,user,agent" {
		t.Fatalf("kinds %s", got)
	}
	if res.Items[2].Text != "also run the linter" || res.Items[3].Text != "and push it" || res.Items[2].StartedAt != parseTimestamp(tsAt(3)) {
		t.Fatalf("queued messages: %+v %+v", res.Items[2], res.Items[3])
	}
}
