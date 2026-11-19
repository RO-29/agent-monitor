package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func usageNoPath(string) (string, error) { return "", errors.New("not found") }

// usageFixture builds usageLimits over home with PATH lookup off and bin as the only install dir.
func usageFixture(t *testing.T, home string) (*usageLimits, string) {
	t.Helper()
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	u := newUsageLimits(home, "", func() time.Time { return usageNow })
	u.lookPath, u.binDirs = usageNoPath, []string{bin}
	return u, bin
}

func findAgent(t *testing.T, r *usageLimitsResp, name string) agentUsage {
	t.Helper()
	for _, a := range r.Agents {
		if a.Agent == name {
			return a
		}
	}
	t.Fatalf("agent %s missing", name)
	return agentUsage{}
}

func findModel(a agentUsage, window, model string) *usageModel {
	for i := range a.Models {
		if a.Models[i].Window == window && a.Models[i].Model == model {
			return &a.Models[i]
		}
	}
	return nil
}

func writeExe(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestUsageAgentsOrderAndInstalled(t *testing.T) {
	home := usageHome(t)
	u, bin := usageFixture(t, home)
	writeExe(t, filepath.Join(bin, "claude"))
	writeExe(t, filepath.Join(bin, "cursor-agent"))
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("x"), 0o644); err != nil { // not executable
		t.Fatal(err)
	}
	r := u.get()
	var names []string
	for _, a := range r.Agents {
		names = append(names, a.Agent)
		if a.Models == nil || a.Windows == nil || a.Local == nil {
			t.Fatalf("%s: nil array in %+v", a.Agent, a)
		}
	}
	if strings.Join(names, ",") != "claude,codex,opencode,cursor,pi" {
		t.Fatalf("agents %v", names)
	}
	want := map[string]bool{"claude": true, "codex": false, "opencode": false, "cursor": true, "pi": false}
	for _, a := range r.Agents {
		if a.Installed != want[a.Agent] {
			t.Fatalf("%s installed %v", a.Agent, a.Installed)
		}
	}
	pi := findAgent(t, r, "pi")
	if !strings.Contains(pi.Note, "not installed") || len(pi.Models) != 0 || pi.Credits != nil {
		t.Fatalf("pi %+v", pi)
	}
	b, _ := json.Marshal(pi)
	if !strings.Contains(string(b), `"installed":false`) || !strings.Contains(string(b), `"models":[]`) || strings.Contains(string(b), "credits") {
		t.Fatalf("pi json %s", b)
	}
}

func TestUsageClaudePerModelAndPlan(t *testing.T) {
	home := usageHome(t)
	cfg := map[string]any{
		"oauthAccount": map[string]any{"organizationType": "claude_max", "userRateLimitTier": "default_claude_max_20x",
			"emailAddress": "someone@example.com", "accountUuid": "uuid-secret"},
		"primaryApiKey": "sk-should-never-appear",
	}
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	opus := claudeAsst(-time.Hour, "m1", "r1", 10)
	opus["message"].(map[string]any)["model"] = "claude-opus-5-5"
	fable := claudeAsst(-2*time.Hour, "m2", "r2", 20)
	fable["message"].(map[string]any)["model"] = "claude-fable-5-2"
	oldHaiku := claudeAsst(-2*24*time.Hour, "m3", "r3", 30)
	oldHaiku["message"].(map[string]any)["model"] = "claude-haiku-5-5"
	jsonl(t, filepath.Join(home, ".claude", "projects", "p1", "s.jsonl"), opus, fable, oldHaiku,
		claudeAsst(-30*time.Minute, "m4", "r4", 5), claudeAsst(-30*time.Minute, "m4", "r4", 7))
	u, _ := usageFixture(t, home)
	r := u.get()
	a := findAgent(t, r, "claude")
	if a.Plan != "max (max_20x)" {
		t.Fatalf("plan %q", a.Plan)
	}
	out, _ := json.Marshal(r)
	for _, bad := range []string{"sk-should", "someone@", "uuid-secret"} {
		if strings.Contains(string(out), bad) {
			t.Fatalf("response leaks %q", bad)
		}
	}
	if m := findModel(a, "5h", "claude-opus-5-5"); m == nil || m.OutTokens != 10 || m.InTokens != 10 || m.CacheRead != 100 || m.Messages != 1 || !m.CostEstimated ||
		m.CostUSD != estimateCost("claude-opus-5-5", TokenUsage{Input: 10, Output: 10, CacheRead: 100}) {
		t.Fatalf("opus 5h %+v", m)
	}
	if m := findModel(a, "5h", "claude-sonnet-5"); m == nil || m.OutTokens != 7 || m.Messages != 1 {
		t.Fatalf("sonnet repeat counted once %+v", m)
	}
	if findModel(a, "5h", "claude-haiku-5-5") != nil {
		t.Fatal("haiku is outside the 5h window")
	}
	if m := findModel(a, "weekly", "claude-haiku-5-5"); m == nil || m.OutTokens != 30 {
		t.Fatalf("haiku weekly %+v", m)
	}
	var sum int64
	for _, m := range a.Models {
		if m.Window == "weekly" {
			sum += m.OutTokens
		}
	}
	if lw := findLocal(a, "weekly"); lw.Tokens.Output != sum || sum != 10+20+30+7 {
		t.Fatalf("weekly local %+v vs models %d", lw, sum)
	}
}

func TestClaudePlanLabel(t *testing.T) {
	for _, c := range [][3]string{
		{"claude_team", "default_claude_max_5x", "team (max_5x)"},
		{"claude_pro", "default_claude_pro", "pro"},
		{"", "default_claude_max_5x", "max_5x"},
		{"claude_enterprise", "default_raven", "enterprise"},
		{"", "", ""},
	} {
		if got := claudePlanLabel(c[0], c[1]); got != c[2] {
			t.Fatalf("%q %q: %q want %q", c[0], c[1], got, c[2])
		}
	}
}

func TestUsageCodexModelsAndCredits(t *testing.T) {
	home := usageHome(t)
	path := filepath.Join(home, ".codex", "sessions", "2026", "11", "17", "rollout-m.jsonl")
	rl := func(bal any) map[string]any {
		return map[string]any{"plan_type": "business", "primary": nil, "secondary": nil,
			"credits": map[string]any{"has_credits": true, "unlimited": false, "balance": bal}}
	}
	jsonl(t, path,
		map[string]any{"timestamp": usageAt(-3 * time.Hour), "type": "turn_context", "payload": map[string]any{"model": "gpt-6-astra"}},
		codexTC(-3*time.Hour, 1100, 1000, 400, 100, rl(nil)),
		map[string]any{"timestamp": usageAt(-time.Hour), "type": "turn_context", "payload": map[string]any{"model": "gpt-6-luna"}},
		codexTC(-time.Hour, 1500, 300, 0, 100, rl("12.5")),
	)
	u, _ := usageFixture(t, home)
	a := findAgent(t, u.get(), "codex")
	if a.Plan != "business" {
		t.Fatalf("plan %q", a.Plan)
	}
	if m := findModel(a, "5h", "gpt-6-astra"); m == nil || m.InTokens != 600 || m.CacheRead != 400 || m.Messages != 1 {
		t.Fatalf("astra %+v", m)
	}
	if m := findModel(a, "5h", "gpt-6-luna"); m == nil || m.InTokens != 300 || m.OutTokens != 100 {
		t.Fatalf("luna %+v", m)
	}
	c := a.Credits
	if c == nil || c.HasCredits == nil || !*c.HasCredits || c.Unlimited == nil || *c.Unlimited || string(c.Balance) != `"12.5"` ||
		c.MeasuredAt != usageNow.Add(-time.Hour).UnixMilli() {
		t.Fatalf("credits %+v", c)
	}
}

func TestUsageCodexCreditsSurviveRestore(t *testing.T) {
	home := usageHome(t)
	hist := filepath.Join(t.TempDir(), "usage-history.json")
	path := filepath.Join(home, ".codex", "sessions", "2026", "11", "17", "rollout-c.jsonl")
	jsonl(t, path,
		map[string]any{"timestamp": usageAt(-time.Hour), "type": "turn_context", "payload": map[string]any{"model": "gpt-6-luna"}},
		codexTC(-time.Hour, 100, 60, 0, 40, map[string]any{"credits": map[string]any{"has_credits": false, "balance": 3}}))
	clock := func() time.Time { return usageNow }
	u1 := newUsageLimits(home, hist, clock)
	u1.get()
	u1.shutdown()
	u2 := newUsageLimits(home, hist, clock)
	u2.lookPath = usageNoPath
	a := findAgent(t, u2.get(), "codex")
	if a.Credits == nil || string(a.Credits.Balance) != "3" || findModel(a, "weekly", "gpt-6-luna") == nil {
		t.Fatalf("restored codex %+v", a)
	}
}

func opencodeMsgFile(t *testing.T, path, role, provider, model string, created int64, in, out, reasoning, cr, cw int64, cost float64) {
	t.Helper()
	m := map[string]any{"id": filepath.Base(path), "role": role, "providerID": provider, "modelID": model, "cost": cost,
		"time":   map[string]any{"created": created},
		"tokens": map[string]any{"input": in, "output": out, "reasoning": reasoning, "cache": map[string]any{"read": cr, "write": cw}}}
	b, _ := json.Marshal(m)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestUsageOpencodeMessages(t *testing.T) {
	home := usageHome(t)
	root := filepath.Join(home, ".local", "share", "opencode", "storage", "message")
	ms := func(d time.Duration) int64 { return usageNow.Add(d).UnixMilli() }
	s1 := filepath.Join(root, "ses_a")
	opencodeMsgFile(t, filepath.Join(s1, "msg_1.json"), "assistant", "anthropic", "claude-sonnet-5", ms(-time.Hour), 100, 20, 5, 1000, 50, 0.25)
	opencodeMsgFile(t, filepath.Join(s1, "msg_2.json"), "user", "anthropic", "claude-sonnet-5", ms(-time.Hour), 0, 0, 0, 0, 0, 0)
	opencodeMsgFile(t, filepath.Join(s1, "msg_3.json"), "assistant", "openai", "gpt-5.2", ms(-2*24*time.Hour), 7, 3, 0, 0, 0, 0)
	u, bin := usageFixture(t, home)
	writeExe(t, filepath.Join(bin, "opencode"))
	a := findAgent(t, u.get(), "opencode")
	if !a.Installed || a.Plan != "" || a.Credits != nil {
		t.Fatalf("opencode %+v", a)
	}
	m := findModel(a, "5h", "anthropic/claude-sonnet-5")
	if m == nil || m.InTokens != 100 || m.OutTokens != 25 || m.CacheRead != 1000 || m.CacheWrite != 50 || m.CostUSD != 0.25 || m.Messages != 1 || m.CostEstimated {
		t.Fatalf("5h sonnet %+v", m)
	}
	if findModel(a, "5h", "openai/gpt-5.2") != nil || findModel(a, "weekly", "openai/gpt-5.2") == nil {
		t.Fatalf("gpt window rows %+v", a.Models)
	}

	// opencode rewrites the message file as the reply grows: the new totals replace the old ones.
	opencodeMsgFile(t, filepath.Join(s1, "msg_1.json"), "assistant", "anthropic", "claude-sonnet-5", ms(-time.Hour), 100, 200, 5, 1000, 50, 0.75)
	future := usageNow.Add(time.Minute)
	_ = os.Chtimes(filepath.Join(s1, "msg_1.json"), future, future)
	u.cached = nil
	a = findAgent(t, u.get(), "opencode")
	if m := findModel(a, "5h", "anthropic/claude-sonnet-5"); m == nil || m.OutTokens != 205 || m.CostUSD != 0.75 || m.Messages != 1 {
		t.Fatalf("rewritten %+v", m)
	}
	if l := findLocal(a, "5h"); l.Messages != 1 || l.Tokens.Output != 205 {
		t.Fatalf("local 5h %+v", l)
	}

	// A deleted session drops out.
	if err := os.RemoveAll(s1); err != nil {
		t.Fatal(err)
	}
	u.cached = nil
	if a = findAgent(t, u.get(), "opencode"); len(a.Models) != 0 || len(u.opencode.msgs) != 0 {
		t.Fatalf("after delete %+v", a.Models)
	}
}

func TestUsageOpencodeSkipsOldSessionDirs(t *testing.T) {
	home := usageHome(t)
	root := filepath.Join(home, ".local", "share", "opencode", "storage", "message")
	old := filepath.Join(root, "ses_old")
	opencodeMsgFile(t, filepath.Join(old, "msg_1.json"), "assistant", "p", "m", usageNow.Add(-time.Hour).UnixMilli(), 1, 1, 0, 0, 0, 0)
	past := usageNow.Add(-30 * 24 * time.Hour)
	_ = os.Chtimes(old, past, past)
	u, _ := usageFixture(t, home)
	if a := findAgent(t, u.get(), "opencode"); len(a.Models) != 0 {
		t.Fatalf("old dir counted %+v", a.Models)
	}
}

func TestUsageCursorTrackingDB(t *testing.T) {
	home := usageHome(t)
	dbPath := filepath.Join(home, ".cursor", "ai-tracking", "ai-code-tracking.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE ai_code_hashes (hash TEXT PRIMARY KEY, source TEXT NOT NULL, fileExtension TEXT, fileName TEXT,
		requestId TEXT, conversationId TEXT, timestamp INTEGER, model TEXT, createdAt INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	ms := func(d time.Duration) int64 { return usageNow.Add(d).UnixMilli() }
	for _, r := range []struct {
		hash, req, model string
		at               int64
	}{
		{"h1", "r1", "kimi-k3", ms(-time.Hour)},
		{"h2", "r1", "kimi-k3", ms(-time.Hour)}, // same request: one message
		{"h3", "r2", "kimi-k3", ms(-3 * 24 * time.Hour)},
		{"h4", "", "composer-2.5", ms(-10 * time.Minute)},
		{"h5", "r9", "kimi-k3", ms(-9 * 24 * time.Hour)},
	} {
		if _, err := db.Exec(`INSERT INTO ai_code_hashes (hash, source, requestId, model, createdAt) VALUES (?, 'composer', ?, ?, ?)`,
			r.hash, r.req, r.model, r.at); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	u, _ := usageFixture(t, home)
	a := findAgent(t, u.get(), "cursor")
	if m := findModel(a, "5h", "kimi-k3"); m == nil || m.Messages != 1 || m.InTokens != 0 || m.CostUSD != 0 {
		t.Fatalf("kimi 5h %+v", m)
	}
	if m := findModel(a, "weekly", "kimi-k3"); m == nil || m.Messages != 2 {
		t.Fatalf("kimi weekly %+v", m)
	}
	if m := findModel(a, "5h", "composer-2.5"); m == nil || m.Messages != 1 {
		t.Fatalf("composer %+v", m)
	}
	if a.Plan != "" || a.Credits != nil || !strings.Contains(a.Note, "no plan, token or credit") {
		t.Fatalf("cursor %+v", a)
	}
}

func TestUsageCursorMissingDB(t *testing.T) {
	u, _ := usageFixture(t, usageHome(t))
	a := findAgent(t, u.get(), "cursor")
	if len(a.Models) != 0 || !strings.Contains(a.Note, "No Cursor") {
		t.Fatalf("cursor %+v", a)
	}
}
