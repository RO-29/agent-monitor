package main

import (
	"path/filepath"
	"testing"
)

// feedOrder flattens items the way the chat shows them: runs by index, items by start time within a run.
func feedOrder(res *WatchedItems) []*OItem {
	idx := map[string]int{}
	for _, r := range res.Runs {
		idx[r.ID] = r.Index
	}
	out := append([]*OItem(nil), res.Items...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0; j-- {
			a, b := out[j-1], out[j]
			if idx[a.RunID] < idx[b.RunID] || idx[a.RunID] == idx[b.RunID] && a.StartedAt <= b.StartedAt {
				break
			}
			out[j-1], out[j] = b, a
		}
	}
	return out
}

func runOf(res *WatchedItems, id string) *ORun {
	for _, r := range res.Runs {
		if r.ID == id {
			return r
		}
	}
	return nil
}

func TestWatchedBangAnswerOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "order.jsonl")
	turnEnd := func(sec int) map[string]any {
		return map[string]any{"type": "system", "subtype": "turn_duration", "timestamp": tsAt(sec)}
	}
	jsonl(t, path,
		cUser(1, "Deploy it"),
		cAsst(2, "m1", "tool_use", cToolUse("tu1", "Bash", map[string]any{"command": "make deploy"})),
		cResult(3, "tu1", "deployed", false),
		cAsst(4, "m2", "end_turn", map[string]any{"type": "text", "text": "Answer A"}),
		turnEnd(4),
		cUser(5, "<bash-input>make check</bash-input>"),
		cUser(6, "<bash-stdout>all good</bash-stdout><bash-stderr></bash-stderr>"),
	)
	ref := watchedRef{ID: "claude:order", Tool: ToolClaude, RawID: "order", Path: path, Resolved: true}
	res, err := watchedItemsFor(ref, -1, -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Runs) != 2 {
		t.Fatalf("a ! command after the turn ended opens its own run: %d runs", len(res.Runs))
	}
	bangRun := res.Runs[1]
	if bangRun.Prompt != "!make check" || bangRun.Status != "completed" {
		t.Fatalf("bang run %+v", bangRun)
	}

	// The agent answers the ! command: the answer belongs to the bang's run, after it.
	appendJSONL(t, path,
		cAsst(7, "m3", "tool_use", cToolUse("tu2", "Bash", map[string]any{"command": "sleep 600"})),
		cAsst(8, "m4", "end_turn", map[string]any{"type": "text", "text": "Answer B"}),
		turnEnd(8),
		cUser(9, "Next"),
		cAsst(10, "m5", "tool_use", cToolUse("tu3", "Bash", map[string]any{"command": "sleep 900"})),
		cUser(11, "[Request interrupted by user]"),
	)
	res2, err := watchedItemsFor(ref, res.Cursor, -1)
	if err != nil {
		t.Fatal(err)
	}
	if r := runOf(res2, bangRun.ID); r == nil || r.Status != "completed" {
		t.Fatalf("bang run after its answer: %+v", r)
	}
	full, err := watchedItemsFor(ref, -1, -1)
	if err != nil {
		t.Fatal(err)
	}
	var seq []string
	for _, it := range feedOrder(full) {
		switch {
		case it.By == "user":
			seq = append(seq, "!"+it.Command)
		case it.Kind == "agent":
			seq = append(seq, it.Text)
		case it.Kind == "command":
			seq = append(seq, "$"+it.Command+":"+it.Status)
		case it.Kind == "user" && it.Text != "":
			seq = append(seq, "> "+it.Text)
		}
	}
	want := []string{"> Deploy it", "$make deploy:completed", "Answer A", "!make check", "$sleep 600:completed", "Answer B", "> Next", "$sleep 900:failed"}
	if len(seq) != len(want) {
		t.Fatalf("order\n got %q\nwant %q", seq, want)
	}
	for i := range want {
		if seq[i] != want[i] {
			t.Fatalf("order\n got %q\nwant %q", seq, want)
		}
	}
	for _, it := range full.Items {
		if it.By == "user" && it.RunID != bangRun.ID {
			t.Fatalf("the ! command must sit in its own run, not after Answer A's: %+v", it)
		}
	}
	if last := full.Runs[len(full.Runs)-1]; last.Status != "interrupted" {
		t.Fatalf("last run %+v", last)
	}
}

func TestWatchedBangMidTurnStaysInTurn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mid.jsonl")
	jsonl(t, path,
		cUser(1, "Work"),
		cAsst(2, "m1", "tool_use", cToolUse("tu1", "Bash", map[string]any{"command": "make"})),
		cUser(3, "<bash-input>git status</bash-input>"),
		cUser(4, "<bash-stdout>clean</bash-stdout><bash-stderr></bash-stderr>"),
		cResult(5, "tu1", "ok", false),
	)
	res, err := watchedItemsFor(watchedRef{ID: "claude:mid", Tool: ToolClaude, RawID: "mid", Path: path, Resolved: true}, -1, -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Runs) != 1 || res.Runs[0].Status != "running" {
		t.Fatalf("a ! command while the agent works stays in that turn: %+v", res.Runs)
	}
}
