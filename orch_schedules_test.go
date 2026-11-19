package main

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// setNext rewrites a stored schedule's nextRunAt, as if the daemon had been down.
func (e *orchEnv) setNext(id string, at time.Time) {
	e.t.Helper()
	orchScheduleMu.Lock()
	defer orchScheduleMu.Unlock()
	ss, err := e.m.loadSchedules()
	if err != nil {
		e.t.Fatal(err)
	}
	ss[indexSchedule(ss, id)].NextRunAt = at.UnixMilli()
	if err := e.m.saveSchedules(ss); err != nil {
		e.t.Fatal(err)
	}
}

func (e *orchEnv) schedule(id string) *OSchedule {
	e.t.Helper()
	s, err := e.m.Schedule(id)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e *orchEnv) scheduledThreads(id string) []OThread {
	ths, _, _ := e.store.Threads()
	var out []OThread
	for _, th := range ths {
		if th.ScheduleID == id {
			out = append(out, th)
		}
	}
	return out
}

func TestOrchSchedulesAPI(t *testing.T) {
	e := newOrchEnv(t)
	srv := e.server()
	tpl, _ := e.m.SaveTemplate("", OTemplate{Name: "T", Agent: "codex", Prompt: "check {{area}}"})
	base := `"templateId":"` + tpl.ID + `","cwd":"` + e.cwd + `"`
	for body, want := range map[string]int{
		`{` + base + `,"cron":"61 * * * *","vars":{"area":"x"}}`:                             400,
		`{"templateId":"tpl_nope","cwd":"` + e.cwd + `","cron":"* * * * *"}`:                 400,
		`{` + base + `,"cron":"0 9 * * mon-fri"}`:                                            400,
		`{"templateId":"` + tpl.ID + `","cwd":"rel","cron":"* * * * *","vars":{"area":"x"}}`: 400,
	} {
		if code, out := postJSON(t, srv.URL+"/api/orch/schedules", body); code != want {
			t.Fatalf("%s: %d %v", body, code, out)
		}
	}
	code, out := postJSON(t, srv.URL+"/api/orch/schedules", `{`+base+`,"cron":" 0  9 * * mon-fri ","vars":{"area":"x"},"enabled":true,"name":"Weekday check"}`)
	if code != 201 {
		t.Fatalf("create %d %v", code, out)
	}
	s := out["schedule"].(map[string]any)
	id := s["id"].(string)
	next, _ := s["nextRuns"].([]any)
	if s["cron"] != "0 9 * * mon-fri" || len(next) != 3 || int64(s["nextRunAt"].(float64)) != int64(next[0].(float64)) {
		t.Fatalf("schedule %v", s)
	}
	if wd := time.UnixMilli(int64(next[0].(float64))).Weekday(); wd == time.Saturday || wd == time.Sunday {
		t.Fatalf("weekend run %v", wd)
	}
	code, out = doJSON(t, "PUT", srv.URL+"/api/orch/schedules/"+id, `{`+base+`,"cron":"0 9 * * *","vars":{"area":"y"},"enabled":false}`)
	if code != 200 || out["schedule"].(map[string]any)["nextRunAt"].(float64) != 0 || len(out["schedule"].(map[string]any)["nextRuns"].([]any)) != 0 {
		t.Fatalf("disable %d %v", code, out)
	}
	if code, _ := doJSON(t, "DELETE", srv.URL+"/api/orch/templates/"+tpl.ID, ""); code != 409 {
		t.Fatalf("delete used template %d", code)
	}
	var prev map[string]any
	getJSONURL(t, srv.URL+"/api/orch/cron?expr=*/5+*+*+*+*", &prev)
	if prev["valid"] != true || len(prev["next"].([]any)) != 5 {
		t.Fatalf("preview %v", prev)
	}
	getJSONURL(t, srv.URL+"/api/orch/cron?expr=bad", &prev)
	if prev["valid"] != false || prev["error"] == "" {
		t.Fatalf("bad preview %v", prev)
	}
	code, out = postJSON(t, srv.URL+"/api/orch/schedules/"+id+"/run-now", "")
	if code != 201 {
		t.Fatalf("run-now %d %v", code, out)
	}
	thID := out["thread"].(map[string]any)["id"].(string)
	if th := e.thread(thID); th.ScheduleID != id || th.CatchUp || th.TemplateID != tpl.ID {
		t.Fatalf("thread %+v", th)
	}
	if s := e.schedule(id); s.LastThreadID != thID || s.LastRunAt == 0 {
		t.Fatalf("after run-now %+v", s)
	}
	e.idle(thID)
	if code, _ := doJSON(t, "DELETE", srv.URL+"/api/orch/schedules/"+id, ""); code != 200 {
		t.Fatalf("delete %d", code)
	}
	if code, _ := doJSON(t, "DELETE", srv.URL+"/api/orch/schedules/"+id, ""); code != 404 {
		t.Fatalf("delete again %d", code)
	}
}

func TestOrchScheduleTickOverlapAndCatchUp(t *testing.T) {
	e := newOrchEnv(t)
	tpl, _ := e.m.SaveTemplate("", OTemplate{Name: "Hang", Agent: "codex", Sandbox: "read-only", Prompt: "{{mode}}"})
	s, err := e.m.SaveSchedule("", OSchedule{TemplateID: tpl.ID, Cwd: e.cwd, Cron: "*/5 * * * *", Enabled: true, Vars: map[string]string{"mode": "hang"}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	e.m.scheduleTick(now)
	if n := len(e.scheduledThreads(s.ID)); n != 0 {
		t.Fatalf("ran before its time: %d", n)
	}

	// due 30 s ago: one on-time run
	e.setNext(s.ID, now.Add(-30*time.Second))
	e.m.scheduleTick(now)
	ths := e.scheduledThreads(s.ID)
	if len(ths) != 1 || ths[0].CatchUp {
		t.Fatalf("on-time run %+v", ths)
	}
	first := ths[0].ID
	e.hanging(first)
	if got := e.schedule(s.ID); got.NextRunAt <= now.UnixMilli() || got.LastThreadID != first {
		t.Fatalf("after run %+v", got)
	}

	// due again while the first still runs: skipped, no overlap
	e.setNext(s.ID, now.Add(-10*time.Second))
	e.m.scheduleTick(now)
	got := e.schedule(s.ID)
	if n := len(e.scheduledThreads(s.ID)); n != 1 || got.LastSkipAt == 0 || !strings.Contains(got.LastError, "still runs") {
		t.Fatalf("overlap: %d threads, %+v", n, got)
	}
	if _, _, err := e.m.RunScheduleNow(s.ID); err == nil {
		t.Fatal("run-now while running")
	}
	if err := e.m.Interrupt(first); err != nil {
		t.Fatal(err)
	}
	e.idle(first)

	// missed for three days while the daemon was down: exactly one flagged catch-up run
	e.setNext(s.ID, now.Add(-72*time.Hour))
	e.m.scheduleTick(now)
	e.m.scheduleTick(now)
	ths = e.scheduledThreads(s.ID)
	if len(ths) != 2 {
		t.Fatalf("catch-up threads %d", len(ths))
	}
	var catchUp *OThread
	for i := range ths {
		if ths[i].ID != first {
			catchUp = &ths[i]
		}
	}
	got = e.schedule(s.ID)
	if !catchUp.CatchUp || !got.LastCatchUp || got.LastError != "" || got.NextRunAt <= now.UnixMilli() {
		t.Fatalf("catch-up %+v schedule %+v", catchUp, got)
	}
	e.hanging(catchUp.ID)
	e.m.Interrupt(catchUp.ID)
	e.idle(catchUp.ID)

	// disabled schedules never fire
	s2, _ := e.m.SaveSchedule(s.ID, OSchedule{TemplateID: tpl.ID, Cwd: e.cwd, Cron: "* * * * *", Enabled: false, Vars: map[string]string{"mode": "hang"}})
	if s2.LastThreadID != catchUp.ID {
		t.Fatalf("update dropped history %+v", s2)
	}
	e.setNext(s.ID, now.Add(-time.Minute))
	e.m.scheduleTick(now)
	if n := len(e.scheduledThreads(s.ID)); n != 2 {
		t.Fatalf("disabled fired: %d", n)
	}
}

func TestOrchScheduleSpendLimit(t *testing.T) {
	e := newOrchEnv(t)
	var limit atomic.Value // read by spendAfterRun goroutines while the test sets it
	limit.Store(0.0)
	e.m.spendCfg = func() SpendSettings {
		return SpendSettings{PerDayUSD: limit.Load().(float64), CodexRates: map[string]SpendRate{"*": {In: 1000, Out: 1000}}}
	}
	th := e.create("hello")
	e.idle(th.ID)
	limit.Store(0.01)
	tpl, _ := e.m.SaveTemplate("", OTemplate{Name: "T", Agent: "codex", Prompt: "hello"})
	s, err := e.m.SaveSchedule("", OSchedule{TemplateID: tpl.ID, Cwd: e.cwd, Cron: "* * * * *", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	e.setNext(s.ID, now.Add(-20*time.Second))
	e.m.scheduleTick(now)
	got := e.schedule(s.ID)
	if n := len(e.scheduledThreads(s.ID)); n != 0 || !strings.Contains(got.LastError, "spend limit") || got.LastErrorAt == 0 {
		t.Fatalf("spend: %d threads, %+v", n, got)
	}
	srv := e.server()
	if code, out := postJSON(t, srv.URL+"/api/orch/schedules/"+s.ID+"/run-now", ""); code != 402 || out["scope"] != "day" {
		t.Fatalf("run-now over limit %d %v", code, out)
	}
}
