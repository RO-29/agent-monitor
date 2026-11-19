package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func claudeFixtureLines(from, n int) string {
	var b strings.Builder
	for i := from; i < from+n; i++ {
		ts := fmt.Sprintf("2026-11-16T10:%02d:%02dZ", i/60%60, i%60)
		switch i % 5 {
		case 0:
			fmt.Fprintf(&b, `{"type":"user","timestamp":%q,"message":{"role":"user","content":"prompt %d"}}`+"\n", ts, i)
		case 1:
			fmt.Fprintf(&b, `{"type":"assistant","timestamp":%q,"message":{"role":"assistant","model":"claude-opus-4","usage":{"input_tokens":%d,"output_tokens":7,"cache_read_input_tokens":3},"content":[{"type":"text","text":"answer %d"},{"type":"tool_use","id":"t%d","name":"Edit","input":{"file_path":"/tmp/f%d.go"}}]}}`+"\n", ts, i, i, i, i%7)
		case 2:
			fmt.Fprintf(&b, `{"type":"assistant","timestamp":%q,"message":{"role":"assistant","content":[{"type":"tool_use","id":"a%d","name":"Agent","input":{"prompt":"child %d\nmore"}}]}}`+"\n", ts, i, i)
		case 3:
			fmt.Fprintf(&b, `{"type":"ai-title","aiTitle":"title %d"}`+"\n", i)
		default:
			fmt.Fprintf(&b, `{"type":"queue-operation","content":"<summary>bg %d</summary>"}`+"\n", i)
		}
	}
	return b.String()
}

func codexFixtureLines(from, n int) string {
	var b strings.Builder
	if from == 0 {
		b.WriteString(`{"type":"session_meta","timestamp":"2026-11-16T10:00:00Z","payload":{"id":"s1","cwd":"/tmp/x"}}` + "\n")
	}
	for i := from; i < from+n; i++ {
		ts := fmt.Sprintf("2026-11-16T10:%02d:%02dZ", i/60%60, i%60)
		switch i % 4 {
		case 0:
			fmt.Fprintf(&b, `{"type":"event_msg","timestamp":%q,"payload":{"type":"user_message","message":"ask %d"}}`+"\n", ts, i)
		case 1:
			fmt.Fprintf(&b, `{"type":"response_item","timestamp":%q,"payload":{"type":"function_call","name":"shell","call_id":"c%d","arguments":"{\"command\":[\"ls\"]}"}}`+"\n", ts, i)
		case 2:
			fmt.Fprintf(&b, `{"type":"turn_context","timestamp":%q,"payload":{"model":"gpt-5.%d","approval_policy":"never"}}`+"\n", ts, i%3)
		default:
			fmt.Fprintf(&b, `{"type":"event_msg","timestamp":%q,"payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":%d,"output_tokens":%d,"cached_input_tokens":1}}}}`+"\n", ts, i*10, i)
		}
	}
	return b.String()
}

func appendText(t *testing.T, p, s string) {
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
	f.Close()
}

// TestSeedResumeMatchesColdScan checks that a resumed aggregate equals a full scan after appends, partial lines, truncation, replacement and deletion.
func TestSeedResumeMatchesColdScan(t *testing.T) {
	t.Setenv("AGENT_MONITOR_HOME", t.TempDir())
	dir := t.TempDir()
	cp, xp := filepath.Join(dir, "c.jsonl"), filepath.Join(dir, "rollout-x.jsonl")
	appendText(t, cp, claudeFixtureLines(0, 400))
	appendText(t, xp, codexFixtureLines(0, 400))

	check := func(step string) {
		t.Helper()
		seeds.begin()
		warmC, errC := seeds.claudeSeed(cp)
		warmX, errX := seeds.codexSeed(xp)
		seeds.end()
		coldC, errC2 := scanClaudeJSONL(cp, nil, 0)
		coldX, errX2 := scanCodexJSONL(xp, nil, 0)
		if (errC == nil) != (errC2 == nil) || (errX == nil) != (errX2 == nil) {
			t.Fatalf("%s: errors differ: %v/%v %v/%v", step, errC, errC2, errX, errX2)
		}
		if !reflect.DeepEqual(warmC, coldC) {
			t.Fatalf("%s: claude resume differs\nwarm=%+v\ncold=%+v", step, warmC, coldC)
		}
		if !reflect.DeepEqual(warmX, coldX) {
			t.Fatalf("%s: codex resume differs\nwarm=%+v\ncold=%+v", step, warmX, coldX)
		}
	}
	check("cold")
	check("unchanged")
	appendText(t, cp, claudeFixtureLines(400, 37))
	appendText(t, xp, codexFixtureLines(400, 37))
	check("appended")

	// A complete JSON record without its newline counts in a full scan, so it must count once resumed too.
	tail := claudeFixtureLines(438, 2)
	appendText(t, cp, strings.TrimSuffix(tail, "\n"))
	appendText(t, xp, `{"type":"event_msg","timestamp":"2026-11-16T11:00:00Z","payload":{"type":"agent_message","message":"half`)
	check("partial last line")
	appendText(t, cp, "\n"+claudeFixtureLines(440, 5))
	appendText(t, xp, ` done"}}`+"\n"+codexFixtureLines(441, 5))
	check("partial line completed")

	if err := os.Truncate(cp, 2000); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(xp, 1500); err != nil {
		t.Fatal(err)
	}
	check("truncated")
	appendText(t, cp, "\n"+claudeFixtureLines(500, 50))
	check("truncated then appended")

	// Rewritten in place: same inode, longer than the saved offset, different first bytes.
	if err := os.WriteFile(cp, []byte(claudeFixtureLines(2000, 700)), 0o644); err != nil {
		t.Fatal(err)
	}
	check("rewritten in place")

	// Replaced: a new file at the same path, longer than the saved offset, with a different start.
	for _, p := range []string{cp, xp} {
		_ = os.Remove(p)
	}
	appendText(t, cp, claudeFixtureLines(1000, 900))
	appendText(t, xp, `{"type":"session_meta","timestamp":"2026-11-17T10:00:00Z","payload":{"id":"s2","cwd":"/tmp/y"}}`+"\n"+codexFixtureLines(1000, 900))
	check("replaced")

	_ = os.Remove(cp)
	_ = os.Remove(xp)
	check("deleted")
	seeds.begin()
	n := len(seeds.entries)
	seeds.end()
	if n != 0 {
		t.Fatalf("deleted files kept %d saved entries", n)
	}
}

func TestSeedResumeSkipsReadBytes(t *testing.T) {
	t.Setenv("AGENT_MONITOR_HOME", t.TempDir())
	p := filepath.Join(t.TempDir(), "c.jsonl")
	appendText(t, p, claudeFixtureLines(0, 50))
	seeds.begin()
	_, _ = seeds.claudeSeed(p)
	seeds.end()
	seeds.begin()
	e := seeds.entries[p]
	seeds.end()
	st, _ := os.Stat(p)
	if e == nil || e.Off != st.Size() {
		t.Fatalf("saved offset %+v, want %d", e, st.Size())
	}
}
