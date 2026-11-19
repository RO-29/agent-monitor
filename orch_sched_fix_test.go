package main

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// L1: a repeated fall-back minute runs once, a skipped spring-forward minute runs once, an impossible date is refused.
func TestCronDaylightSaving(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip(err)
	}
	fmtAll := func(ts []time.Time) string {
		var s []string
		for _, x := range ts {
			s = append(s, x.Format(time.RFC3339))
		}
		return strings.Join(s, " ")
	}
	c, _ := parseCron("30 1 * * *")
	if got := fmtAll(c.nextN(time.Date(2026, 10, 31, 12, 0, 0, 0, ny), 3)); got != "2026-11-01T01:30:00-04:00 2026-11-02T01:30:00-05:00 2026-11-03T01:30:00-05:00" {
		t.Fatalf("fall back: %s", got)
	}
	c, _ = parseCron("*/20 1 * * *")
	if got := fmtAll(c.nextN(time.Date(2026, 11, 1, 1, 30, 0, 0, ny), 3)); got != "2026-11-01T01:40:00-04:00 2026-11-02T01:00:00-05:00 2026-11-02T01:20:00-05:00" {
		t.Fatalf("fall back steps: %s", got)
	}
	c, _ = parseCron("30 2 * * *")
	if got := fmtAll(c.nextN(time.Date(2026, 3, 7, 12, 0, 0, 0, ny), 2)); got != "2026-03-08T03:00:00-04:00 2026-03-09T02:30:00-04:00" {
		t.Fatalf("spring forward: %s", got)
	}
	c, _ = parseCron("0 3 * * *")
	if got := fmtAll(c.nextN(time.Date(2026, 3, 7, 12, 0, 0, 0, ny), 2)); got != "2026-03-08T03:00:00-04:00 2026-03-09T03:00:00-04:00" {
		t.Fatalf("after the gap: %s", got)
	}
	for _, ex := range []string{"0 0 30 2 *", "0 0 31 4 *"} {
		if _, err := parseCron(ex); err == nil {
			t.Fatalf("%q accepted", ex)
		}
	}
	if _, err := parseCron("0 0 29 2 *"); err != nil {
		t.Fatalf("leap day refused: %v", err)
	}
}

// M8: concurrent run-now calls start one thread.
func TestScheduleRunNowFiresOnce(t *testing.T) {
	e := newOrchEnv(t)
	tpl, err := e.m.SaveTemplate("", OTemplate{Name: "T", Agent: "codex", Prompt: "hang"})
	if err != nil {
		t.Fatal(err)
	}
	s, err := e.m.SaveSchedule("", OSchedule{TemplateID: tpl.ID, Cwd: e.cwd, Cron: "0 0 1 1 *", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _, _ = e.m.RunScheduleNow(s.ID) }()
	}
	wg.Wait()
	ths := e.scheduledThreads(s.ID)
	if len(ths) != 1 {
		t.Fatalf("threads %d", len(ths))
	}
	e.hanging(ths[0].ID)
	_ = e.m.Interrupt(ths[0].ID)
	e.idle(ths[0].ID)
}

// M9: a torn schedules.json is moved aside and reported, never replaced in silence.
func TestScheduleCorruptFileMovedAside(t *testing.T) {
	e := newOrchEnv(t)
	tpl, _ := e.m.SaveTemplate("", OTemplate{Name: "T", Agent: "codex", Prompt: "p"})
	for i := 0; i < 3; i++ {
		if _, err := e.m.SaveSchedule("", OSchedule{TemplateID: tpl.ID, Cwd: e.cwd, Cron: "0 0 1 1 *"}); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(e.m.schedulesPath())
	os.WriteFile(e.m.schedulesPath(), b[:len(b)/2], 0o600)
	if _, err := e.m.SaveSchedule("", OSchedule{TemplateID: tpl.ID, Cwd: e.cwd, Cron: "0 0 1 1 *"}); err == nil || !strings.Contains(err.Error(), ".corrupt-") {
		t.Fatalf("save over a torn file: %v", err)
	}
	ents, _ := os.ReadDir(e.dir)
	var aside string
	for _, x := range ents {
		if strings.HasPrefix(x.Name(), "schedules.json.corrupt-") {
			aside = x.Name()
		}
	}
	if aside == "" {
		t.Fatal("torn file not kept")
	}
	if got, _ := os.ReadFile(e.dir + "/" + aside); string(got) != string(b[:len(b)/2]) {
		t.Fatal("kept file differs")
	}
	tb, _ := os.ReadFile(e.m.templatesPath())
	os.WriteFile(e.m.templatesPath(), tb[:len(tb)/2], 0o600)
	if _, err := e.m.SaveTemplate("", OTemplate{Name: "U", Agent: "codex", Prompt: "p"}); err == nil {
		t.Fatal("template save over a torn file")
	}
}

// M9: a write leaves no temporary file and a read error other than "missing" is not an empty list.
func TestStateFileReadErrorIsNotEmpty(t *testing.T) {
	e := newOrchEnv(t)
	if err := os.MkdirAll(e.m.schedulesPath(), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.loadSchedules(); err == nil {
		t.Fatal("unreadable schedules.json read as empty")
	}
	os.RemoveAll(e.m.schedulesPath())
	if err := writeJSONFile(e.m.schedulesPath(), []OSchedule{}); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(e.dir)
	for _, x := range ents {
		if strings.Contains(x.Name(), ".tmp") {
			t.Fatalf("temporary file left: %s", x.Name())
		}
	}
}

// L6: a schedule stops making worktrees at the cap.
func TestScheduleWorktreeCap(t *testing.T) {
	e := newOrchEnv(t)
	repo := gitTestRepo(t)
	tpl, err := e.m.SaveTemplate("", OTemplate{Name: "W", Agent: "codex", Prompt: "hello", Workspace: "worktree"})
	if err != nil {
		t.Fatal(err)
	}
	s, err := e.m.SaveSchedule("", OSchedule{TemplateID: tpl.ID, Cwd: repo, Cron: "0 0 1 1 *", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < orchScheduleMaxWorktrees; i++ {
		th, _, err := e.m.RunScheduleNow(s.ID)
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		e.idle(th.ID)
	}
	if _, _, err := e.m.RunScheduleNow(s.ID); err == nil || !strings.Contains(err.Error(), "worktrees") {
		t.Fatalf("run past the cap: %v", err)
	}
	if n := len(e.scheduledThreads(s.ID)); n != orchScheduleMaxWorktrees {
		t.Fatalf("threads %d", n)
	}
}
