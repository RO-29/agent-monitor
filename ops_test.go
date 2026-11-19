package main

import "testing"

func TestCodexExecRegex(t *testing.T) {
	yes := []string{"codex exec -s read-only - < p.md", "command codex exec --json", "~/.local/bin/codex exec x", "cd a && codex e 'review'"}
	no := []string{"codex resume abc", "echo codexexec", "rg codex exec_test", "git log --grep codex"}
	for _, s := range yes {
		if !reCodexExec.MatchString(s) {
			t.Errorf("want match: %q", s)
		}
	}
	for _, s := range no {
		if reCodexExec.MatchString(s) {
			t.Errorf("want no match: %q", s)
		}
	}
}

func TestMatchCodexRunPicksClosestInWindow(t *testing.T) {
	codex := []*Session{
		{ID: "codex:a", Cwd: "/r", StartedAt: 1_000},
		{ID: "codex:b", Cwd: "/r/", StartedAt: 10_500},
		{ID: "codex:c", Cwd: "/other", StartedAt: 10_100},
		{ID: "codex:d", Cwd: "/r", StartedAt: 40_000},
	}
	used := map[string]bool{}
	if got := matchCodexRun(codex, used, "/r", 10_000); got == nil || got.ID != "codex:b" {
		t.Fatalf("got %v, want codex:b", got)
	}
	used["codex:b"] = true
	if got := matchCodexRun(codex, used, "/r", 10_000); got != nil {
		t.Fatalf("got %v, want nil (a is before the window, d after it)", got.ID)
	}
}

func TestWorkBarsMergeAndFoldIdle(t *testing.T) {
	tr := &SessionTrace{Spans: []Span{
		{Kind: "turn", Ts: 0, Dur: 60_000},
		{Kind: "tool", Ts: 90_000, Dur: 10_000, Depth: 1},
		{Kind: "tool", Ts: 30 * 60_000, Dur: 60_000, Depth: 1, Err: true},
	}}
	bars := workBars(tr, "claude", 0, 3_600_000)
	var work, idle, errs int
	for _, b := range bars {
		switch b.Kind {
		case "work":
			work++
		case "idle":
			idle++
		case "error":
			errs++
		}
	}
	if work != 2 || idle != 1 || errs != 1 {
		t.Fatalf("work=%d idle=%d err=%d, want 2/1/1: %+v", work, idle, errs, bars)
	}
	if bars[0].To != 100_000 {
		t.Fatalf("first bar should merge the turn and the tool call 30 s later, ends at %d", bars[0].To)
	}
}

func TestOrderLanesPutsChildrenUnderParent(t *testing.T) {
	lanes := []TimelineLane{{ThreadID: "p"}, {ThreadID: "x"}, {ThreadID: "c", Parent: "p"}, {ThreadID: "orphan", Parent: "gone"}}
	got := orderLanes(lanes)
	want := []string{"p", "c", "x", "orphan"}
	for i, l := range got {
		if l.ThreadID != want[i] {
			t.Fatalf("order %v, want %v", ids(got), want)
		}
	}
}

func ids(ls []TimelineLane) []string {
	out := []string{}
	for _, l := range ls {
		out = append(out, l.ThreadID)
	}
	return out
}

func TestTurnBounds(t *testing.T) {
	tr := &SessionTrace{Spans: []Span{
		{Kind: "user", Ts: 1, Text: "first"}, {Kind: "turn", Ts: 2},
		{Kind: "tool", Ts: 3, Depth: 1},
		{Kind: "turn", Ts: 9},
	}}
	starts, prompts := turnBounds(tr)
	if len(starts) != 2 || starts[1] != 9 || prompts[0] != "first" || prompts[1] != "" {
		t.Fatalf("starts=%v prompts=%q", starts, prompts)
	}
}

func TestCmdDir(t *testing.T) {
	h := homeDir()
	cases := map[string]string{
		"cd ~/aah-film && command codex exec -s read-only -": h + "/aah-film",
		"codex exec -C /r/x --json":                          "/r/x",
		"cd sub && codex e 'hi'":                             "/base/sub",
		`cd "/a b" && codex exec`:                            "/a b",
		"codex exec -s read-only - < prompt.md":              "",
	}
	for cmd, want := range cases {
		if got := cmdDir(cmd, "/base"); got != want {
			t.Errorf("cmdDir(%q) = %q, want %q", cmd, got, want)
		}
	}
}

func TestCmdDirCleansRelative(t *testing.T) {
	if got := cmdDir("cd ../worker && codex exec review", "/work/parent"); got != "/work/worker" {
		t.Fatalf("got %q", got)
	}
}

// An empty link list is cached like any other: the second call must not scan the store.
func TestChildLinksCachesEmptyResult(t *testing.T) {
	prev := store
	t.Cleanup(func() {
		store = prev
		childCache.Lock()
		childCache.links = nil
		childCache.Unlock()
	})
	store = NewStore()
	childCache.Lock()
	childCache.links = nil
	childCache.Unlock()
	if got := childLinks(); len(got) != 0 {
		t.Fatalf("links %v", got)
	}
	store = nil
	if got := childLinks(); len(got) != 0 {
		t.Fatalf("cached links %v", got)
	}
}
