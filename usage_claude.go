package main

import (
	"encoding/json"
	"math"
	"strings"
	"time"
)

// usageStaleAfter marks a Claude plan reading as old: Claude Code refreshes it only while it runs.
const usageStaleAfter = time.Hour

type claudeMoney struct {
	AmountMinor float64 `json:"amount_minor"`
	Currency    string  `json:"currency"`
	Exponent    int     `json:"exponent"`
}

func (m claudeMoney) major() float64 { return m.AmountMinor / math.Pow10(m.Exponent) }

type claudeLimitRow struct {
	Kind     string   `json:"kind"`
	Percent  *float64 `json:"percent"`
	ResetsAt string   `json:"resets_at"`
	Active   bool     `json:"is_active"`
	Scope    *struct {
		Model *struct {
			DisplayName string `json:"display_name"`
		} `json:"model"`
	} `json:"scope"`
}

// claudeCachedUsage is the part of cachedUsageUtilization the usage page uses; it holds no account id.
type claudeCachedUsage struct {
	FetchedAtMs int64
	Limits      []claudeLimitRow
	Spend       *claudeSpend
	CapReached  bool
}

type claudeSpend struct {
	Used    claudeMoney `json:"used"`
	Limit   claudeMoney `json:"limit"`
	Percent *float64    `json:"percent"`
	Enabled bool        `json:"enabled"`
}

func parseClaudeUtilization(fetchedAtMs int64, raw json.RawMessage) *claudeCachedUsage {
	var u struct {
		Limits     []claudeLimitRow `json:"limits"`
		Spend      *claudeSpend     `json:"spend"`
		ExtraUsage *struct {
			Reached bool `json:"spend_limit_reached"`
		} `json:"extra_usage"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &u) != nil {
		return nil
	}
	c := &claudeCachedUsage{FetchedAtMs: fetchedAtMs, Limits: u.Limits, Spend: u.Spend}
	c.CapReached = u.ExtraUsage != nil && u.ExtraUsage.Reached
	return c
}

// applyClaudeUtilization overlays the cached reading from ~/.claude.json on the statusLine windows.
func (u *usageLimits) applyClaudeUtilization(a *agentUsage, now time.Time) {
	u.claudeConfig(now)
	c := u.claudePlan.usage
	if c == nil || c.FetchedAtMs <= 0 {
		return
	}
	stMeasured := a.MeasuredAt
	a.MeasuredAt = max(a.MeasuredAt, c.FetchedAtMs)
	a.Source = "Claude Code cached usage (~/.claude.json)"
	a.Stale = now.Sub(time.UnixMilli(a.MeasuredAt)) > usageStaleAfter
	a.Note = ""
	scoped := 0
	for _, r := range c.Limits {
		// is_active marks the binding limit only; /usage shows every row.
		if r.Percent == nil {
			continue
		}
		t, err := time.Parse(time.RFC3339, r.ResetsAt)
		resets := int64(0)
		if err == nil {
			resets = roundMinuteMs(t.UnixMilli())
		}
		switch r.Kind {
		case "session", "weekly_all":
			key, mins := "5h", 300
			if r.Kind == "weekly_all" {
				key, mins = "weekly", 10080
			}
			for i := range a.Windows {
				// The cache wins only when newer than the statusLine copy, or when the copy lacks the window.
				old := a.Windows[i]
				if old.Key == key && (c.FetchedAtMs > stMeasured || (old.UsedPercent == nil && old.LastPercent == nil)) {
					pct := u.monotonic("claude/"+key, r.Percent, resets)
					a.Windows[i] = u.window("claude", key, mins, pct, resets, c.FetchedAtMs, now)
				}
			}
		case "weekly_scoped":
			label := "Week (scoped)"
			if r.Scope != nil && r.Scope.Model != nil && r.Scope.Model.DisplayName != "" {
				label = "Week (" + r.Scope.Model.DisplayName + ")"
			}
			key := "weekly_scoped"
			if scoped > 0 {
				key += ":" + strings.ToLower(strings.ReplaceAll(label, " ", "-"))
			}
			scoped++
			pct := u.monotonic("claude/"+key, r.Percent, resets)
			w := u.window("claude", key, 10080, pct, resets, c.FetchedAtMs, now)
			w.Label = label
			a.Windows = append(a.Windows, w)
		}
	}
	if s := c.Spend; s != nil && s.Enabled {
		spent, limit := s.Used.major(), s.Limit.major()
		cr := &usageCredits{Source: "Claude Code cached usage (~/.claude.json)", MeasuredAt: c.FetchedAtMs,
			Spent: &spent, Limit: &limit, Currency: s.Limit.Currency, Percent: s.Percent, CapReached: c.CapReached}
		if cr.Currency == "" {
			cr.Currency = s.Used.Currency
		}
		a.Credits = cr
		if c.CapReached {
			a.Alert = "Extra usage spend limit reached."
		}
	}
}

// roundMinuteMs rounds a reset time to the minute, so the statusLine copy and the cache share one window identity.
func roundMinuteMs(ms int64) int64 { return (ms + 30_000) / 60_000 * 60_000 }
