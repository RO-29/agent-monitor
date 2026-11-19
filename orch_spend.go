package main

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// SpendRate is a Codex token price in USD per 1M tokens.
type SpendRate struct {
	In  float64 `json:"in"`
	Out float64 `json:"out"`
}

// SpendSettings are the driven spend limits (0 = off).
type SpendSettings struct {
	PerThreadUSD float64 `json:"perThreadUsd"`
	PerDayUSD    float64 `json:"perDayUsd"`
	// CodexRates prices Codex runs, which report tokens but no cost; key = model, "*" = any model.
	CodexRates map[string]SpendRate `json:"codexRates"`
}

type orchSpendError struct {
	Scope        string
	Spent, Limit float64
}

func (e *orchSpendError) Error() string {
	return fmt.Sprintf("spend limit reached: %s spent $%.2f of $%.2f", map[string]string{"thread": "this thread", "day": "today",
		"tree": "this thread and its subagents"}[e.Scope], e.Spent, e.Limit)
}

func (m *OrchManager) spendSettings() SpendSettings {
	if m.spendCfg != nil {
		return m.spendCfg()
	}
	return getSettings().Spend
}

// runCost is the run's reported cost; for Codex a configured rate wins, else the stored or built-in estimate.
func runCost(th *OThread, r *ORun, cfg SpendSettings) float64 {
	if r.Usage == nil {
		return 0
	}
	if th.Agent != "codex" {
		return r.Usage.CostUSD
	}
	if c, ok := codexRateCost(th.Model, r.Usage, cfg); ok {
		return c
	}
	if r.Usage.CostUSD > 0 {
		return r.Usage.CostUSD
	}
	c, _ := codexRunCost(th.Model, r.Usage, cfg)
	return c
}

func codexRateCost(model string, u *OUsage, cfg SpendSettings) (float64, bool) {
	rate, ok := cfg.CodexRates[model]
	if !ok {
		rate, ok = cfg.CodexRates["*"]
	}
	if !ok {
		return 0, false
	}
	return (float64(u.InTokens)*rate.In + float64(u.OutTokens)*rate.Out) / 1e6, true
}

// codexRunCost prices Codex tokens by the configured rate, else by the watched-session price table (estimated).
func codexRunCost(model string, u *OUsage, cfg SpendSettings) (cost float64, estimated bool) {
	if c, ok := codexRateCost(model, u, cfg); ok {
		return c, false
	}
	cached := min(max(u.CachedIn, 0), u.InTokens)
	if lm := strings.ToLower(model); !strings.Contains(lm, "gpt") && !strings.Contains(lm, "codex") {
		model = "codex" // the table's default row is a Claude price
	}
	return estimateCost(model, TokenUsage{Input: u.InTokens - cached, CacheRead: cached, Output: u.OutTokens}), true
}

func dayStart(now time.Time) int64 {
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local).UnixMilli()
}

type spendTotals struct {
	thread, day float64
	tree        float64 // the whole lineage of the thread's root

	lastRun         float64 // cost of the thread's newest run
	lastRunToday    bool
	allowOnce       bool
	threadFound     bool
	activeThreadIDs []string
}

// spendTotals reads the thread's spend and the day's driven spend in one read.
func (m *OrchManager) spendTotals(threadID string, now time.Time) spendTotals {
	cfg := m.spendSettings()
	var st spendTotals
	today := dayStart(now)
	_ = m.store.read(func(tx *sql.Tx) error {
		threads, err := scanJSONRows[OThread](tx.Query(`SELECT json FROM threads`))
		if err != nil {
			return err
		}
		by := map[string]*OThread{}
		for i := range threads {
			by[threads[i].ID] = &threads[i]
			if activeStatus(threads[i].Status) {
				st.activeThreadIDs = append(st.activeThreadIDs, threads[i].ID)
			}
		}
		if th := by[threadID]; th != nil {
			st.threadFound, st.allowOnce = true, th.SpendAllowOnce
		}
		rootOf := func(id string) string {
			for i := 0; i < 64; i++ {
				th := by[id]
				if th == nil || th.ParentID == "" {
					break
				}
				id = th.ParentID
			}
			return id
		}
		root := rootOf(threadID)
		runs, err := scanJSONRows[ORun](tx.Query(`SELECT json FROM runs ORDER BY idx`))
		if err != nil {
			return err
		}
		for i := range runs {
			r := &runs[i]
			th := by[r.ThreadID]
			if th == nil {
				continue
			}
			c := runCost(th, r, cfg)
			if r.StartedAt >= today {
				st.day += c
			}
			if threadID != "" && rootOf(r.ThreadID) == root {
				st.tree += c
			}
			if r.ThreadID == threadID {
				st.thread += c
				st.lastRun, st.lastRunToday = c, r.StartedAt >= today
			}
		}
		return nil
	})
	return st
}

// spendGate refuses a new turn when a limit is reached; threadID "" checks the day only.
// An "allow once" grant lets one turn through and is used up here.
func (m *OrchManager) spendGate(threadID string) error {
	cfg := m.spendSettings()
	if cfg.PerThreadUSD <= 0 && cfg.PerDayUSD <= 0 {
		return nil
	}
	st := m.spendTotals(threadID, time.Now())
	var blocked *orchSpendError
	switch {
	case threadID != "" && cfg.PerThreadUSD > 0 && st.thread >= cfg.PerThreadUSD:
		blocked = &orchSpendError{"thread", st.thread, cfg.PerThreadUSD}
	case cfg.PerDayUSD > 0 && st.day >= cfg.PerDayUSD:
		blocked = &orchSpendError{"day", st.day, cfg.PerDayUSD}
	}
	if blocked == nil {
		return nil
	}
	if threadID != "" && st.allowOnce {
		_, _ = m.UpdateMeta(threadID, func(th *OThread, now int64) { th.SpendAllowOnce = false })
		m.smu.Lock()
		m.spendGranted[threadID] = true
		m.smu.Unlock()
		return nil
	}
	return blocked
}

// delegateSpendGate refuses a new subagent when the day limit is reached, or when the parent's
// whole lineage (root and every subagent) has spent the per-thread limit.
func (m *OrchManager) delegateSpendGate(parentID string) error {
	cfg := m.spendSettings()
	if cfg.PerThreadUSD <= 0 && cfg.PerDayUSD <= 0 {
		return nil
	}
	st := m.spendTotals(parentID, time.Now())
	switch {
	case cfg.PerDayUSD > 0 && st.day >= cfg.PerDayUSD:
		return &orchSpendError{"day", st.day, cfg.PerDayUSD}
	case cfg.PerThreadUSD > 0 && st.tree >= cfg.PerThreadUSD:
		return &orchSpendError{"tree", st.tree, cfg.PerThreadUSD}
	}
	return nil
}

// AllowOnce lets a thread start one more turn past its spend limits.
// A subagent report held by the limit is delivered now.
func (m *OrchManager) AllowOnce(id string) (*OThread, error) {
	th, err := m.UpdateMeta(id, func(th *OThread, now int64) { th.SpendAllowOnce = true })
	if err == nil {
		go m.maybeNotify(id)
	}
	return th, err
}

// spendAfterRun notifies at 80% and stops at 100% when the run that just ended crossed a limit.
func (m *OrchManager) spendAfterRun(id string) {
	cfg := m.spendSettings()
	if cfg.PerThreadUSD <= 0 && cfg.PerDayUSD <= 0 {
		return
	}
	st := m.spendTotals(id, time.Now())
	if !st.threadFound {
		return
	}
	if cfg.PerThreadUSD > 0 {
		m.spendCrossed("thread", id, st.thread-st.lastRun, st.thread, cfg.PerThreadUSD, nil)
	}
	if cfg.PerDayUSD > 0 && st.lastRunToday {
		m.spendCrossed("day", id, st.day-st.lastRun, st.day, cfg.PerDayUSD, st.activeThreadIDs)
	}
}

func (m *OrchManager) spendCrossed(scope, id string, before, after, limit float64, active []string) {
	what := "This thread"
	if scope == "day" {
		what = "Driven spend today"
	}
	switch {
	case before < limit && after >= limit:
		m.notify(notifyEvent{Kind: "spend", ThreadID: id, Title: fmt.Sprintf("%s reached its spend limit", what),
			Body: fmt.Sprintf("$%.2f of $%.2f. New turns are refused until the limit is raised or the thread is allowed once more.", after, limit)})
		stop := active
		if scope == "thread" {
			stop = []string{id}
		}
		for _, tid := range stop {
			if th, err := m.store.Thread(tid); err == nil && activeStatus(th.Status) {
				if err := m.Interrupt(tid); err != nil {
					log.Printf("orch: spend stop %s: %v", tid, err)
				}
			}
		}
	case before < 0.8*limit && after >= 0.8*limit:
		m.notify(notifyEvent{Kind: "spend", ThreadID: id, Title: fmt.Sprintf("%s is at 80%% of its spend limit", what),
			Body: fmt.Sprintf("$%.2f of $%.2f.", after, limit)})
	}
}

// spendSweep stops running threads that went past a limit mid-run (Codex reports tokens while a turn runs).
func (m *OrchManager) spendSweep() {
	cfg := m.spendSettings()
	if cfg.PerThreadUSD <= 0 && cfg.PerDayUSD <= 0 {
		return
	}
	st := m.spendTotals("", time.Now())
	for _, id := range st.activeThreadIDs {
		t := m.spendTotals(id, time.Now())
		over := (cfg.PerThreadUSD > 0 && t.thread >= cfg.PerThreadUSD) || (cfg.PerDayUSD > 0 && t.day >= cfg.PerDayUSD)
		if !over || t.allowOnce {
			continue
		}
		m.smu.Lock()
		if m.spendGranted[id] {
			m.smu.Unlock()
			continue
		}
		done := m.spendStopped[id]
		m.spendStopped[id] = true
		m.smu.Unlock()
		if done {
			continue
		}
		if err := m.Interrupt(id); err == nil {
			m.notify(notifyEvent{Kind: "spend", ThreadID: id, Title: "A running thread went past a spend limit and was stopped"})
		}
	}
}

type spendScope struct {
	Spent       float64 `json:"spent"`
	Limit       float64 `json:"limit"`
	AllowedOnce bool    `json:"allowedOnce,omitempty"`
	Blocked     bool    `json:"blocked"`
}

func (m *OrchManager) SpendStatus(threadID string) map[string]any {
	cfg := m.spendSettings()
	st := m.spendTotals(threadID, time.Now())
	dayBlocked := cfg.PerDayUSD > 0 && st.day >= cfg.PerDayUSD
	out := map[string]any{"day": spendScope{Spent: round2(st.day), Limit: cfg.PerDayUSD, Blocked: dayBlocked}}
	if threadID != "" && st.threadFound {
		b := (cfg.PerThreadUSD > 0 && st.thread >= cfg.PerThreadUSD) || dayBlocked
		out["thread"] = spendScope{Spent: round2(st.thread), Limit: cfg.PerThreadUSD, AllowedOnce: st.allowOnce, Blocked: b && !st.allowOnce}
	}
	return out
}

func round2(x float64) float64 { return float64(int64(x*100+0.5)) / 100 }

func (m *OrchManager) spendRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/orch/spend", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("thread")
		if id != "" {
			if _, err := m.store.Thread(id); err != nil {
				orchWriteErr(w, err)
				return
			}
		}
		writeJSON(w, m.SpendStatus(id))
	})
	mux.HandleFunc("POST /api/orch/threads/{id}/allow-once", func(w http.ResponseWriter, r *http.Request) {
		th, err := m.AllowOnce(r.PathValue("id"))
		if err != nil {
			orchWriteErr(w, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "thread": th})
	})
}
