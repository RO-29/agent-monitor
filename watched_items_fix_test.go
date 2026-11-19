package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// L3: a line over the cap is skipped with a marker and later lines still parse.
func TestWatchedItemsSkipsOverlongLine(t *testing.T) {
	old := watchedLineMax
	watchedLineMax = 2048
	defer func() { watchedLineMax = old }()
	path := filepath.Join(t.TempDir(), "s1.jsonl")
	jsonl(t, path,
		cUser(1, "go"),
		cAsst(2, "m1", "", map[string]any{"type": "text", "text": strings.Repeat("x", 3<<20)}),
		cAsst(3, "m2", "end_turn", map[string]any{"type": "text", "text": "after"}),
	)
	ref := claudeRef(path)
	ref.ID = "claude:overlong"
	res, err := watchedItemsFor(ref, -1, -1)
	if err != nil {
		t.Fatal(err)
	}
	if got := kinds(res.Items); got != "user,other,agent" || res.Items[2].Text != "after" || res.Cursor != fileSize(t, path) {
		t.Fatalf("items %s cursor %d", got, res.Cursor)
	}
	if !strings.Contains(res.Items[1].Text, "skipped") {
		t.Fatalf("marker %+v", res.Items[1])
	}
}

// L3: a file rewritten in place to the same size is seen and resets.
func TestWatchedItemsSeesInPlaceRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s1.jsonl")
	jsonl(t, path, cUser(1, "first prompt AAAA"), cAsst(2, "m1", "end_turn", map[string]any{"type": "text", "text": "reply"}))
	ref := claudeRef(path)
	ref.ID = "claude:rewrite"
	res, err := watchedItemsFor(ref, -1, -1)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(path)
	b, _ := os.ReadFile(path)
	nb := strings.Replace(string(b), "first prompt AAAA", "other prompt BBBB", 1)
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteAt([]byte(nb), 0)
	f.Close()
	os.Chtimes(path, st.ModTime().Add(time.Second), st.ModTime().Add(time.Second))
	rs, err := watchedItemsFor(ref, res.Cursor, -1)
	if err != nil {
		t.Fatal(err)
	}
	if !rs.Reset || len(rs.Runs) != 1 || rs.Runs[0].Prompt != "other prompt BBBB" {
		t.Fatalf("rewrite not seen: reset=%v runs=%+v", rs.Reset, rs.Runs)
	}
}

// A session that stopped during a tool call and then got a new prompt closes the tool row.
func TestWatchedNewPromptClosesOrphanTool(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orphan.jsonl")
	jsonl(t, path,
		cUser(1, "run the tests"),
		cAsst(2, "m1", "tool_use", map[string]any{"type": "tool_use", "id": "tu1", "name": "Bash", "input": map[string]any{"command": "go test ./..."}}),
		cUser(5, "never mind, do this instead"),
		cAsst(6, "m2", "end_turn", map[string]any{"type": "text", "text": "ok"}),
	)
	ref := claudeRef(path)
	ref.ID = "claude:orphan"
	res, err := watchedItemsFor(ref, -1, -1)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range res.Items {
		if it.Status == "started" {
			t.Fatalf("item still started: %+v", it)
		}
	}
	if len(res.Runs) != 2 {
		t.Fatalf("runs %d", len(res.Runs))
	}
}

// A ! command with no <bash-stdout> line ends when the next prompt opens a turn.
func TestWatchedBangWithoutOutputCloses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bangnoout.jsonl")
	jsonl(t, path,
		cUser(1, "hi"),
		cAsst(2, "m1", "end_turn", map[string]any{"type": "text", "text": "hello"}),
		cUser(3, "<bash-input>sleep 100</bash-input>"),
		cUser(9, "next prompt"),
		cAsst(10, "m2", "end_turn", map[string]any{"type": "text", "text": "done"}),
	)
	ref := claudeRef(path)
	ref.ID = "claude:bangnoout"
	res, err := watchedItemsFor(ref, -1, -1)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range res.Items {
		if it.By == "user" && it.Command == "sleep 100" {
			found = true
			if it.Status == "started" {
				t.Fatalf("! command still started: %+v", it)
			}
		}
	}
	if !found {
		t.Fatalf("no ! item: %s", kinds(res.Items))
	}
}

// "[Request interrupted" in assistant text or a tool result is not an interrupt.
func TestWatchedInterruptOnlyOnUserLines(t *testing.T) {
	for i, mid := range []map[string]any{
		cAsst(2, "m1", "tool_use", map[string]any{"type": "text", "text": "[Request interrupted by user] is what the log says"},
			map[string]any{"type": "tool_use", "id": "tu1", "name": "Bash", "input": map[string]any{"command": "grep x"}}),
		cResult(3, "tu1", "log.txt: [Request interrupted by user]", false),
	} {
		path := filepath.Join(t.TempDir(), "intr.jsonl")
		jsonl(t, path,
			cUser(1, "look at the log"),
			cAsst(2, "m0", "tool_use", map[string]any{"type": "tool_use", "id": "tu1", "name": "Bash", "input": map[string]any{"command": "grep x"}}),
			mid,
			cUser(4, "<bash-input>ls</bash-input>"),
			cUser(5, "<bash-stdout>a</bash-stdout><bash-stderr></bash-stderr>"),
			cAsst(6, "m2", "end_turn", map[string]any{"type": "text", "text": "done"}),
		)
		ref := claudeRef(path)
		ref.ID = "claude:intr" + strconv.Itoa(i)
		res, err := watchedItemsFor(ref, -1, -1)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Runs) != 1 {
			t.Fatalf("a ! in a running turn opened a turn of its own: %d runs", len(res.Runs))
		}
	}
	interrupt := claudeRec("user", 2, map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "[Request interrupted by user]"}}}, nil)
	b, _ := json.Marshal(interrupt)
	if !claudeInterruptLine(b) {
		t.Fatal("a real interrupt line is not seen")
	}
}
