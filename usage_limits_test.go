package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var usageNow = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

func usageHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	for _, d := range []string{".dietclaude", ".claude/projects/p1", ".codex/sessions/2026/11/17"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func usageAt(d time.Duration) string { return usageNow.Add(d).Format(time.RFC3339Nano) }

func claudeAsst(at time.Duration, id, req string, out int64) map[string]any {
	return map[string]any{"type": "assistant", "timestamp": usageAt(at), "requestId": req, "message": map[string]any{
		"id": id, "role": "assistant", "model": "claude-sonnet-5", "content": []any{},
		"usage": map[string]any{"input_tokens": 10, "output_tokens": out, "cache_read_input_tokens": 100, "cache_creation_input_tokens": 0}}}
}

func findWin(a agentUsage, key string) usageWindow {
	for _, w := range a.Windows {
		if w.Key == key {
			return w
		}
	}
	return usageWindow{}
}

func findLocal(a agentUsage, key string) usageLocal {
	for _, l := range a.Local {
		if l.Key == key {
			return l
		}
	}
	return usageLocal{}
}

func TestUsageClaudePlanAndLocal(t *testing.T) {
	home := usageHome(t)
	reset5 := usageNow.Add(2 * time.Hour).Unix()
	resetW := usageNow.Add(3 * 24 * time.Hour).Unix()
	st := map[string]any{"updated_at": float64(usageNow.Add(-time.Minute).Unix()), "rate_limits": map[string]any{
		"five_hour": map[string]any{"used_percentage": 42.5, "resets_at": reset5},
		"seven_day": map[string]any{"used_percentage": 130, "resets_at": resetW},
	}}
	b, _ := json.Marshal(st)
	if err := os.WriteFile(filepath.Join(home, ".dietclaude", "state.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	tr := filepath.Join(home, ".claude", "projects", "p1", "s.jsonl")
	jsonl(t, tr,
		map[string]any{"type": "user", "timestamp": usageAt(-time.Hour), "message": map[string]any{"role": "user", "content": "usage"}},
		claudeAsst(-time.Hour, "m1", "r1", 5),
		claudeAsst(-time.Hour, "m1", "r1", 50),  // same reply, later chunk: counted once with the larger output
		claudeAsst(-4*time.Hour, "m2", "r2", 7), // before the plan 5h window start (now - 3h)
		claudeAsst(-10*24*time.Hour, "m3", "r3", 9),
	)

	u := newUsageLimits(home, filepath.Join(home, "hist.json"), func() time.Time { return usageNow })
	r := u.get()
	a := r.Agents[0]
	if a.Agent != "claude" || a.Note != "" {
		t.Fatalf("agent %q note %q", a.Agent, a.Note)
	}
	w5 := findWin(a, "5h")
	if w5.UsedPercent == nil || *w5.UsedPercent != 42.5 || w5.ResetsAt != reset5*1000 || w5.WindowMinutes != 300 {
		t.Fatalf("5h window %+v", w5)
	}
	if ww := findWin(a, "weekly"); ww.UsedPercent == nil || *ww.UsedPercent != 100 {
		t.Fatalf("weekly percent must clamp to 100: %+v", ww)
	}
	if len(w5.History) != 1 || w5.History[0].Pct != 42.5 {
		t.Fatalf("history %+v", w5.History)
	}
	l5 := findLocal(a, "5h")
	if !l5.Aligned || l5.From != usageNow.Add(-3*time.Hour).UnixMilli() {
		t.Fatalf("local 5h must start at the plan window start: %+v", l5)
	}
	if l5.Messages != 1 || l5.Tokens.Output != 50 || l5.Tokens.CacheRead != 100 {
		t.Fatalf("local 5h %+v", l5)
	}
	if lw := findLocal(a, "weekly"); lw.Messages != 2 || lw.Tokens.Output != 57 {
		t.Fatalf("local weekly %+v", lw)
	}
	if _, err := os.Stat(filepath.Join(home, "hist.json")); err != nil {
		t.Fatalf("history file not saved: %v", err)
	}
}

func TestUsageClaudeExpiredAndMissing(t *testing.T) {
	home := usageHome(t)
	u := newUsageLimits(home, "", func() time.Time { return usageNow })
	a := u.get().Agents[0]
	if a.Note == "" || findWin(a, "5h").UsedPercent != nil {
		t.Fatalf("missing state must give a note and no percent: %+v", a)
	}

	st := `{"updated_at": 1, "rate_limits": {"five_hour": {"used_percentage": 80, "resets_at": ` +
		jsonNum(usageNow.Add(-time.Minute).Unix()) + `}}}`
	if err := os.WriteFile(filepath.Join(home, ".dietclaude", "state.json"), []byte(st), 0o644); err != nil {
		t.Fatal(err)
	}
	u = newUsageLimits(home, "", func() time.Time { return usageNow })
	w := findWin(u.get().Agents[0], "5h")
	if !w.Expired || w.UsedPercent != nil || w.LastPercent == nil || *w.LastPercent != 80 {
		t.Fatalf("a reading past its reset must be expired: %+v", w)
	}
	if l := findLocal(u.get().Agents[0], "5h"); l.Aligned {
		t.Fatalf("an expired window must not align the local window: %+v", l)
	}
}

func jsonNum(v int64) string { b, _ := json.Marshal(v); return string(b) }

func codexTC(at time.Duration, total, in, cached, out int64, rl map[string]any) map[string]any {
	p := map[string]any{"type": "token_count", "info": map[string]any{
		"total_token_usage": map[string]any{"input_tokens": in, "cached_input_tokens": cached, "output_tokens": out, "total_tokens": total},
		"last_token_usage":  map[string]any{"input_tokens": in, "cached_input_tokens": cached, "output_tokens": out, "total_tokens": total},
	}}
	if rl != nil {
		p["rate_limits"] = rl
	}
	return map[string]any{"timestamp": usageAt(at), "type": "event_msg", "payload": p}
}

func TestUsageCodexRolloutWindows(t *testing.T) {
	home := usageHome(t)
	path := filepath.Join(home, ".codex", "sessions", "2026", "11", "17", "rollout-a.jsonl")
	resetW := usageNow.Add(48 * time.Hour).Unix()
	rl := func(p5, pw float64) map[string]any {
		return map[string]any{"plan_type": "pro",
			"primary":   map[string]any{"used_percent": p5, "window_minutes": 300, "resets_in_seconds": 3600},
			"secondary": map[string]any{"used_percent": pw, "window_minutes": 10080, "resets_at": resetW}}
	}
	jsonl(t, path,
		map[string]any{"timestamp": usageAt(-2 * time.Hour), "type": "turn_context", "payload": map[string]any{"model": "gpt-5.5"}},
		codexTC(-2*time.Hour, 1100, 1000, 400, 100, rl(10, 3)),
		codexTC(-2*time.Hour+time.Second, 1100, 1000, 400, 100, rl(10, 3)), // repeated event: not counted twice
		codexTC(-30*time.Minute, 1600, 400, 100, 100, rl(25, 4)),
	)
	u := newUsageLimits(home, "", func() time.Time { return usageNow })
	a := u.get().Agents[1]
	if a.Agent != "codex" || a.Plan != "pro" || a.Note != "" {
		t.Fatalf("codex agent %+v", a)
	}
	w5 := findWin(a, "5h")
	wantReset := usageNow.Add(-30*time.Minute + time.Hour).UnixMilli()
	if w5.UsedPercent == nil || *w5.UsedPercent != 25 || w5.ResetsAt != wantReset {
		t.Fatalf("5h %+v want reset %d", w5, wantReset)
	}
	if len(w5.History) != 2 || w5.History[0].Pct != 10 {
		t.Fatalf("5h history %+v", w5.History)
	}
	ww := findWin(a, "weekly")
	if ww.UsedPercent == nil || *ww.UsedPercent != 4 || ww.ResetsAt != resetW*1000 || ww.WindowMinutes != 10080 {
		t.Fatalf("weekly %+v", ww)
	}
	if a.MeasuredAt != usageNow.Add(-30*time.Minute).UnixMilli() {
		t.Fatalf("measuredAt %d", a.MeasuredAt)
	}
	lw := findLocal(a, "weekly")
	if lw.Messages != 2 || lw.Tokens.Input != 600+300 || lw.Tokens.CacheRead != 500 || lw.Tokens.Output != 200 {
		t.Fatalf("local weekly %+v", lw)
	}
}

func TestUsageCodexNullWindowsAndIncrementalScan(t *testing.T) {
	home := usageHome(t)
	path := filepath.Join(home, ".codex", "sessions", "2026", "11", "17", "rollout-b.jsonl")
	nullRL := map[string]any{"primary": nil, "secondary": nil, "plan_type": "business"}
	jsonl(t, path, codexTC(-time.Hour, 110, 100, 0, 10, nullRL))
	now := usageNow
	u := newUsageLimits(home, "", func() time.Time { return now })
	a := u.get().Agents[1]
	if a.Note == "" || a.Plan != "business" || findWin(a, "5h").UsedPercent != nil {
		t.Fatalf("null windows must give a note and no percent: %+v", a)
	}
	if l := findLocal(a, "5h"); l.Messages != 1 || l.Tokens.Output != 10 {
		t.Fatalf("local 5h %+v", l)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	line, _ := json.Marshal(codexTC(-time.Minute, 150, 30, 0, 10, nullRL))
	_, _ = f.Write(append(line, '\n'))
	_, _ = f.Write([]byte(`{"timestamp":"partial`))
	f.Close()

	if l := findLocal(u.get().Agents[1], "5h"); l.Messages != 1 {
		t.Fatalf("the cache must hold for 30 s: %+v", l)
	}
	now = now.Add(usageCacheTTL)
	if l := findLocal(u.get().Agents[1], "5h"); l.Messages != 2 || l.Tokens.Output != 20 || l.Tokens.Input != 130 {
		t.Fatalf("appended event must be read once: %+v", l)
	}
}

func TestUsageHistoryDedup(t *testing.T) {
	h := loadUsageHistory("")
	b := usageNow.UnixMilli()
	h.add("k", b, 10, usageNow)
	h.add("k", b, 12, usageNow)      // same time: ignored
	h.add("k", b+1000, 10, usageNow) // same percent within 15 min: ignored
	h.add("k", b+2000, 11, usageNow)
	h.add("k", b-8*24*3600*1000, 5, usageNow)
	if got := h.get("k", 0); len(got) != 2 || got[1].Pct != 11 {
		t.Fatalf("history %+v", got)
	}
}

func TestUsageMonotonicPerWindow(t *testing.T) {
	u := &usageLimits{}
	f := func(v float64) *float64 { return &v }
	if got := *u.monotonic("claude/5h", f(65), 1000); got != 65 {
		t.Fatalf("first reading %v", got)
	}
	if got := *u.monotonic("claude/5h", f(45), 1000); got != 65 {
		t.Fatalf("an older session's lower reading replaced the peak: %v", got)
	}
	if got := *u.monotonic("claude/5h", f(3), 2000); got != 3 {
		t.Fatalf("a new window kept the old peak: %v", got)
	}
	if u.monotonic("claude/5h", nil, 2000) != nil {
		t.Fatal("no reading must stay nil")
	}
}

func TestUsageClaudeRepeatAcrossFilesAndBuckets(t *testing.T) {
	home := usageHome(t)
	dir := filepath.Join(home, ".claude", "projects", "p1")
	jsonl(t, filepath.Join(dir, "a.jsonl"),
		claudeAsst(-3*time.Hour, "m1", "r1", 5),
		claudeAsst(-3*time.Hour+30*time.Second, "m2", "r2", 7),
	)
	// A fork copies m1 two minutes later with a larger output; m1 counts once there.
	jsonl(t, filepath.Join(dir, "b.jsonl"),
		claudeAsst(-3*time.Hour+2*time.Minute, "m1", "r1", 50),
		claudeAsst(-3*time.Hour+2*time.Minute, "", "r3", 1),
	)
	now := usageNow
	u := newUsageLimits(home, "", func() time.Time { return now })
	sc := u.claude
	sc.scan()
	if tok, _, n := sc.totals(usageNow.Add(-4 * time.Hour).UnixMilli()); n != 3 || tok.Output != 58 {
		t.Fatalf("all: n=%d out=%d", n, tok.Output)
	}
	// Bucket granularity: a window starting inside a minute counts that whole minute.
	if _, _, n := sc.totals(usageNow.Add(-3*time.Hour + 45*time.Second).UnixMilli()); n != 3 {
		t.Fatalf("window inside the first minute: n=%d", n)
	}
	if tok, _, n := sc.totals(usageNow.Add(-3*time.Hour + time.Minute).UnixMilli()); n != 2 || tok.Output != 51 {
		t.Fatalf("from the second minute: n=%d out=%d", n, tok.Output)
	}
	// Removing the fork counts m1 again from its first copy.
	if err := os.Remove(filepath.Join(dir, "b.jsonl")); err != nil {
		t.Fatal(err)
	}
	sc.scan()
	if tok, _, n := sc.totals(0); n != 2 || tok.Output != 12 {
		t.Fatalf("after removing the fork: n=%d %+v", n, tok)
	}
	if len(sc.seen) != 2 {
		t.Fatalf("dedup marks: %d", len(sc.seen))
	}
	// Buckets older than the week plus one hour are dropped with their marks.
	now = usageNow.Add(usageWeek)
	sc.scan()
	if _, _, n := sc.totals(0); n != 0 || len(sc.seen) != 0 {
		t.Fatalf("old buckets kept: n=%d seen=%d", n, len(sc.seen))
	}
}

func TestUsageCodexReadingsFeedHistoryOnce(t *testing.T) {
	home := usageHome(t)
	path := filepath.Join(home, ".codex", "sessions", "2026", "11", "17", "rollout-c.jsonl")
	rl := func(p float64) map[string]any {
		return map[string]any{"plan_type": "pro", "primary": map[string]any{"used_percent": p, "window_minutes": 300, "resets_in_seconds": 7200}}
	}
	jsonl(t, path, codexTC(-3*time.Hour, 110, 100, 0, 10, rl(10)))
	now := usageNow
	u := newUsageLimits(home, "", func() time.Time { return now })
	if h := findWin(u.get().Agents[1], "5h").History; len(h) != 1 {
		t.Fatalf("history %+v", h)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	line, _ := json.Marshal(codexTC(-time.Hour, 150, 30, 0, 10, rl(20)))
	_, _ = f.Write(append(line, '\n'))
	f.Close()
	now = now.Add(usageCacheTTL)
	a := u.get().Agents[1]
	w := findWin(a, "5h")
	if len(w.History) != 2 || w.UsedPercent == nil || *w.UsedPercent != 20 || a.MeasuredAt != usageNow.Add(-time.Hour).UnixMilli() {
		t.Fatalf("after append: %+v measured %d", w, a.MeasuredAt)
	}
	if len(u.codex.pending) != 0 {
		t.Fatalf("readings must leave the queue once in the history: %d", len(u.codex.pending))
	}
	now = usageNow.Add(usageWeek + 2*time.Hour)
	if a := u.get().Agents[1]; a.Plan != "" || a.MeasuredAt != 0 || findWin(a, "5h").UsedPercent != nil {
		t.Fatalf("readings older than the week must expire: %+v", a)
	}
}

// A file that ages out of the window drops its marks without rebuilding the other files.
func TestUsageAgedOutFileDoesNotRebuild(t *testing.T) {
	home := usageHome(t)
	dir := filepath.Join(home, ".claude", "projects", "p1")
	old, cur := filepath.Join(dir, "old.jsonl"), filepath.Join(dir, "new.jsonl")
	jsonl(t, old, claudeAsst(-6*24*time.Hour, "m1", "r1", 5))
	jsonl(t, cur, claudeAsst(-time.Hour, "m2", "r2", 7))
	if err := os.Chtimes(old, usageNow.Add(-6*24*time.Hour), usageNow.Add(-6*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(cur, usageNow.Add(-time.Hour), usageNow.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	now := usageNow
	sc := newUsageLimits(home, "", func() time.Time { return now }).claude
	sc.scan()
	before := sc.files[cur]
	if before == nil || sc.files[old] == nil || len(sc.seen) != 2 {
		t.Fatalf("first scan: files=%d seen=%d", len(sc.files), len(sc.seen))
	}
	now = usageNow.Add(2 * 24 * time.Hour)
	sc.scan()
	if sc.files[old] != nil {
		t.Fatal("aged-out file still tracked")
	}
	if sc.files[cur] != before {
		t.Fatal("aging one file out rebuilt the others")
	}
	if tok, _, n := sc.totals(0); n != 1 || tok.Output != 7 {
		t.Fatalf("after aging out: n=%d out=%d", n, tok.Output)
	}
}
