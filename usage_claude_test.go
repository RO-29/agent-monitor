package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Invented figures only.
func claudeCfgJSON(account, cachedAccount string, fetched time.Time, spendEnabled bool) string {
	reset := usageNow.Add(48 * time.Hour).Format(time.RFC3339)
	return fmt.Sprintf(`{"oauthAccount":{"accountUuid":%q,"organizationType":"claude_team","userRateLimitTier":"default_claude_max_5x"},
"cachedUsageUtilization":{"fetchedAtMs":%d,"accountUuid":%q,"utilization":{
 "limits":[
  {"kind":"session","percent":33,"resets_at":%q,"is_active":false},
  {"kind":"weekly_all","percent":41,"resets_at":%q,"is_active":true},
  {"kind":"weekly_scoped","percent":12,"resets_at":%q,"is_active":true,"scope":{"model":{"display_name":"Fable"},"surface":null}},
  {"kind":"weekly_scoped","percent":99,"resets_at":%q,"is_active":false,"scope":{"model":{"display_name":"Other"}}}],
 "spend":{"used":{"amount_minor":1250,"currency":"USD","exponent":2},"limit":{"amount_minor":5000,"currency":"USD","exponent":2},"percent":25,"enabled":%v},
 "extra_usage":{"spend_limit_reached":true}}}}`,
		account, fetched.UnixMilli(), cachedAccount, reset, reset, reset, reset, spendEnabled)
}

func claudeWith(t *testing.T, cfg string) agentUsage {
	t.Helper()
	home := usageHome(t)
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	u := newUsageLimits(home, "", func() time.Time { return usageNow })
	return u.get().Agents[0]
}

func TestUsageClaudeCachedUtilization(t *testing.T) {
	fetched := usageNow.Add(-5 * time.Minute)
	a := claudeWith(t, claudeCfgJSON("acct-1", "acct-1", fetched, true))
	if a.MeasuredAt != fetched.UnixMilli() || a.Stale {
		t.Fatalf("age must come from fetchedAtMs: %d stale %v", a.MeasuredAt, a.Stale)
	}
	if w := findWin(a, "weekly"); w.UsedPercent == nil || *w.UsedPercent != 41 {
		t.Fatalf("weekly_all %+v", w)
	}
	if w := findWin(a, "5h"); w.UsedPercent != nil {
		t.Fatalf("inactive session row must not become a window: %+v", w)
	}
	var scoped []usageWindow
	for _, w := range a.Windows {
		if w.Key == "weekly_scoped" {
			scoped = append(scoped, w)
		}
	}
	if len(scoped) != 1 || scoped[0].Label != "Week (Fable)" || *scoped[0].UsedPercent != 12 {
		t.Fatalf("scoped windows %+v", scoped)
	}
	c := a.Credits
	if c == nil || *c.Spent != 12.5 || *c.Limit != 50 || c.Currency != "USD" || !c.CapReached || a.Alert == "" {
		t.Fatalf("credits %+v alert %q", c, a.Alert)
	}
}

func TestUsageClaudeCachedIgnored(t *testing.T) {
	fetched := usageNow.Add(-5 * time.Minute)
	a := claudeWith(t, claudeCfgJSON("acct-1", "acct-2", fetched, true))
	if a.Credits != nil || findWin(a, "weekly").UsedPercent != nil || a.MeasuredAt == fetched.UnixMilli() {
		t.Fatalf("another account's reading must be ignored: %+v", a)
	}
	a = claudeWith(t, claudeCfgJSON("acct-1", "acct-1", fetched, false))
	if a.Credits != nil || a.Alert != "" {
		t.Fatalf("disabled spend must give no credits: %+v", a.Credits)
	}
}

func TestUsageClaudeCachedStale(t *testing.T) {
	a := claudeWith(t, claudeCfgJSON("acct-1", "acct-1", usageNow.Add(-2*time.Hour), true))
	if !a.Stale || a.MeasuredAt != usageNow.Add(-2*time.Hour).UnixMilli() {
		t.Fatalf("stale %v measuredAt %d", a.Stale, a.MeasuredAt)
	}
}

func TestUsageCodexFlags(t *testing.T) {
	cases := []struct {
		reached, spend, want string
	}{
		{`"workspace_member_usage_limit_reached"`, `null`, "Workspace spend cap reached (set by the workspace owner)."},
		{`"workspace_owner_usage_limit_reached"`, `null`, "Workspace spend cap reached. Raise it in the workspace settings."},
		{`"workspace_owner_credits_depleted"`, `null`, "Workspace credits are used up. Add credits in the workspace settings."},
		{`"workspace_member_credits_depleted"`, `null`, "Workspace credits are used up. Ask the workspace owner to add credits."},
		{`"rate_limit_reached"`, `null`, "Rate limit reached. Usage resumes when the window resets."},
		{`null`, `{"limit":"x"}`, "Workspace spend cap reached."},
		{`null`, `false`, ""},
	}
	for _, c := range cases {
		home := usageHome(t)
		path := filepath.Join(home, ".codex", "sessions", "2026", "11", "17", "rollout-f.jsonl")
		line := fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":null,"rate_limits":{"primary":null,"secondary":null,"plan_type":"business","rate_limit_reached_type":%s,"spend_control_reached":%s}}}`+"\n",
			usageAt(-time.Hour), c.reached, c.spend)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
			t.Fatal(err)
		}
		a := newUsageLimits(home, "", func() time.Time { return usageNow }).get().Agents[1]
		if a.Alert != c.want || a.Note != "Business plan: limits are set by the workspace owner." {
			t.Fatalf("%s %s: alert %q note %q", c.reached, c.spend, a.Alert, a.Note)
		}
	}
}
