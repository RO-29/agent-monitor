package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
)

func appendRecs(t *testing.T, path string, recs ...any) {
	t.Helper()
	var b []byte
	for _, r := range recs {
		x, _ := json.Marshal(r)
		b = append(append(b, x...), '\n')
	}
	appendText(t, path, string(b))
}

// usageDump is a scanner's counted state keyed by path, comparable across instances.
func usageDump(s *usageScanner) map[string]any {
	files := map[string]any{}
	for p, f := range s.files {
		files[p] = fmt.Sprintf("off=%d buckets=%v prev=%d model=%s plan=%s/%d latest=%v,%d/%v,%d", f.off, f.buckets, f.prevTotal, f.model, f.plan, f.planAt,
			f.latest[0].w != nil, f.latest[0].at, f.latest[1].w != nil, f.latest[1].at)
	}
	var seen []string
	for h, v := range s.seen {
		path := ""
		for p, f := range s.files {
			if f == v.f {
				path = p
			}
		}
		seen = append(seen, fmt.Sprintf("%x@%s/%d/%d", h, path, v.min, v.out))
	}
	sort.Strings(seen)
	return map[string]any{"files": files, "seen": seen}
}

// TestUsageResumeMatchesColdScan checks a restored scanner against a fresh one after appends, truncation, replacement and deletion.
func TestUsageResumeMatchesColdScan(t *testing.T) {
	home := usageHome(t)
	hist := filepath.Join(t.TempDir(), "usage-history.json")
	dir := filepath.Join(home, ".claude", "projects", "p1")
	a, b, c := filepath.Join(dir, "a.jsonl"), filepath.Join(dir, "b.jsonl"), filepath.Join(dir, "c.jsonl")
	cx := filepath.Join(home, ".codex", "sessions", "2026", "11", "17", "rollout-x.jsonl")
	rl := map[string]any{"plan_type": "pro", "primary": map[string]any{"used_percent": 12.0, "window_minutes": 300, "resets_at": usageNow.Add(time.Hour).Unix()}}
	jsonl(t, a, claudeAsst(-3*time.Hour, "m1", "r1", 5), claudeAsst(-2*time.Hour, "m2", "r2", 7))
	jsonl(t, b, claudeAsst(-time.Hour, "m1", "r1", 50), claudeAsst(-time.Hour, "", "r3", 1))
	jsonl(t, c, claudeAsst(-30*time.Minute, "m4", "r4", 3))
	jsonl(t, cx, map[string]any{"type": "turn_context", "payload": map[string]any{"model": "gpt-5"}}, codexTC(-2*time.Hour, 100, 60, 10, 30, rl))

	clock := func() time.Time { return usageNow }
	check := func(step string) {
		t.Helper()
		warm := newUsageLimits(home, hist, clock)
		w := warm.get()
		cold := newUsageLimits(home, "", clock)
		c := cold.get()
		for _, pair := range [][2]*usageScanner{{warm.claude, cold.claude}, {warm.codex, cold.codex}} {
			if dw, dc := usageDump(pair[0]), usageDump(pair[1]); !reflect.DeepEqual(dw, dc) {
				t.Fatalf("%s: %s resume differs\nwarm=%v\ncold=%v", step, pair[0].root, dw, dc)
			}
		}
		for i := range w.Agents {
			// totals sums files in map order, so cost can differ in the last float bits between any two scans
			for _, l := range [][]usageLocal{w.Agents[i].Local, c.Agents[i].Local} {
				for j := range l {
					l[j].CostUSD = math.Round(l[j].CostUSD*1e9) / 1e9
				}
			}
			if !reflect.DeepEqual(w.Agents[i].Local, c.Agents[i].Local) || w.Agents[i].Plan != c.Agents[i].Plan {
				t.Fatalf("%s: %s totals differ\nwarm=%+v\ncold=%+v", step, w.Agents[i].Agent, w.Agents[i].Local, c.Agents[i].Local)
			}
		}
	}
	check("cold")
	if !pathExists(usageStatePath(hist)) {
		t.Fatal("no usage state saved")
	}
	check("unchanged")
	r := newUsageLimits(home, hist, clock)
	if r.loadState(); len(r.claude.files) != 3 || len(r.codex.files) != 1 || len(r.claude.seen) == 0 {
		t.Fatalf("saved state not restored: %d claude, %d codex files", len(r.claude.files), len(r.codex.files))
	}
	appendRecs(t, a, claudeAsst(-20*time.Minute, "m5", "r5", 9))
	appendRecs(t, c, claudeAsst(-10*time.Minute, "m1", "r1", 70)) // a later repeat moves m1 into c
	appendRecs(t, cx, codexTC(-time.Hour, 180, 70, 20, 40, rl))
	check("appended")
	appendText(t, a, `{"type":"assistant","timestamp":"`) // partial last line
	check("partial last line")
	if err := os.Truncate(c, 10); err != nil {
		t.Fatal(err)
	}
	check("truncated")
	if err := os.WriteFile(b, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	jsonl(t, b, claudeAsst(-5*time.Minute, "m9", "r9", 2), claudeAsst(-4*time.Minute, "m10", "r10", 2), claudeAsst(-3*time.Minute, "m11", "r11", 2))
	check("rewritten in place")
	_ = os.Remove(cx)
	jsonl(t, cx, codexTC(-30*time.Minute, 500, 300, 0, 200, nil))
	check("replaced")
	_ = os.Remove(b)
	check("deleted")
}
