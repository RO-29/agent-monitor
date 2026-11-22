package main

import (
	"path/filepath"
	"testing"
)

// codeModeFixture mirrors Claude Code's Code Mode records with invented content.
func codeModeFixture(t *testing.T) string {
	path := filepath.Join(t.TempDir(), "s1.jsonl")
	ev := `<event nonce="n1" kind="repl-eval" at="` + tsAt(8) + `" eval="1" ms="5000" status="settled">` + "\n<result>{ files: 2 }</result>\n</event>"
	jsonl(t, path,
		cUser(1, "Count the files"),
		cAsst(2, "m1", "tool_use", map[string]any{"type": "tool_use", "id": "r1", "name": "REPL", "input": map[string]any{"code": "await $`ls`", "description": "List files"}}),
		claudeRec("user", 3, map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "r1", "content": "(dispatched #1)"}}},
			map[string]any{"toolUseResult": map[string]any{"code": "await $`ls`", "result": "(dispatched #1)", "evalId": "1", "asyncDispatched": true}}),
		map[string]any{"type": "attachment", "timestamp": tsAt(8), "attachment": map[string]any{"type": "poll_events", "envelopes": []any{ev}}},
		claudeRec("assistant", 8, map[string]any{"id": "v1", "role": "assistant", "model": "<synthetic>", "content": []any{
			map[string]any{"type": "tool_use", "id": "repl_a", "name": "Bash", "input": map[string]any{"command": "ls"}}}}, map[string]any{"isVirtual": true}),
		claudeRec("user", 8, map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "repl_a", "content": "a\nb"}}}, map[string]any{"isVirtual": true}),
		cAsst(10, "m2", "end_turn", map[string]any{"type": "text", "text": "Two files."}),
	)
	return path
}

func TestCodeModeWatchedItems(t *testing.T) {
	res, err := watchedItemsFor(claudeRef(codeModeFixture(t)), -1, -1)
	if err != nil {
		t.Fatal(err)
	}
	var script, inner *OItem
	for _, it := range res.Items {
		switch it.Command {
		case "REPL":
			script = it
		case "ls":
			inner = it
		}
	}
	if script == nil || inner == nil {
		t.Fatalf("items %s", kinds(res.Items))
	}
	if script.Status != "completed" || script.Text != "List files" || script.EndedAt-script.StartedAt != 6000 || script.Output != "{ files: 2 }" {
		t.Fatalf("script %+v", script)
	}
	if inner.ParentID != script.ID || inner.Status != "completed" {
		t.Fatalf("inner %+v", inner)
	}
}

func TestCodeModeTrace(t *testing.T) {
	tr := buildClaudeTrace(&Session{ID: "claude:s1", Tool: ToolClaude, SessionID: "s1", Cwd: "/w", TranscriptPath: codeModeFixture(t)})
	var script, inner *Span
	for i := range tr.Spans {
		switch {
		case tr.Spans[i].Name == "REPL":
			script = &tr.Spans[i]
		case tr.Spans[i].Name == "Bash":
			inner = &tr.Spans[i]
		}
	}
	if script == nil || inner == nil {
		t.Fatal("spans missing")
	}
	if script.Dur != 6000 || inner.Parent != script.ID || inner.Depth != script.Depth+1 || inner.Ts != script.Ts {
		t.Fatalf("script %+v inner %+v", *script, *inner)
	}
}

func TestParseReplEventsFailed(t *testing.T) {
	evs := parseReplEvents(`<event nonce="x" kind="repl-eval" at="` + tsAt(3) + `" eval="7" ms="12" status="rejected"><error>boom</error></event><event kind="other" eval="9"></event>`)
	if len(evs) != 1 || evs[0].eval != "7" || evs[0].status != "rejected" || evs[0].ms != 12 {
		t.Fatalf("events %+v", evs)
	}
}
