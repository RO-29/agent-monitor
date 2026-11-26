package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestWatchedItemsScheduledTaskOpensItsOwnTurn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wake.jsonl")
	jsonl(t, path,
		cUser(1, "Fix the logger"),
		cAsst(2, "m1", "end_turn", map[string]any{"type": "text", "text": "The long answer."}),
		map[string]any{"type": "system", "subtype": "turn_duration", "timestamp": tsAt(2)},
		map[string]any{"type": "system", "subtype": "scheduled_task_fire", "content": "Running scheduled task", "timestamp": tsAt(3)},
		claudeRec("user", 3, map[string]any{"role": "user", "content": "Ticket watch (silent unless something changed).\nMore detail."},
			map[string]any{"isMeta": true, "promptSource": "system", "wakeupSource": "schedule_wakeup"}),
		cAsst(4, "m2", "end_turn", map[string]any{"type": "text", "text": "No change on the ticket."}),
	)
	res, err := watchedItemsFor(claudeRef(path), -1, -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Runs) != 2 {
		t.Fatalf("want 2 runs, got %d (%s)", len(res.Runs), kinds(res.Items))
	}
	byRun := map[string][]*OItem{}
	for _, it := range res.Items {
		byRun[it.RunID] = append(byRun[it.RunID], it)
	}
	first, second := byRun[res.Runs[0].ID], byRun[res.Runs[1].ID]
	if len(first) == 0 || first[len(first)-1].Text != "The long answer." {
		t.Fatalf("first run: %s", kinds(first))
	}
	if second[0].Kind != "user" || second[0].By != "schedule" || second[0].Text != "Scheduled task: Ticket watch (silent unless something changed)." {
		t.Fatalf("wake item: %+v", second[0])
	}
	if !strings.Contains(second[len(second)-1].Text, "No change") {
		t.Fatalf("second run: %s", kinds(second))
	}
}
