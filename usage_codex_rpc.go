package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	codexRPCEvery   = 10 * time.Minute
	codexRPCTimeout = 15 * time.Second
	codexRPCExpire  = 30 * time.Minute
)

var codexReapCap = 5 * time.Second

type codexRateWin struct {
	UsedPercent        float64 `json:"usedPercent"`
	ResetsAt           *int64  `json:"resetsAt"`
	WindowDurationMins *int64  `json:"windowDurationMins"`
}

type codexRateSnap struct {
	LimitID         *string       `json:"limitId"`
	PlanType        *string       `json:"planType"`
	Primary         *codexRateWin `json:"primary"`
	Secondary       *codexRateWin `json:"secondary"`
	ReachedType     *string       `json:"rateLimitReachedType"`
	SpendReached    *bool         `json:"spendControlReached"`
	IndividualLimit *struct {
		Limit            json.RawMessage `json:"limit"`
		Used             json.RawMessage `json:"used"`
		RemainingPercent *float64        `json:"remainingPercent"`
	} `json:"individualLimit"`
}

// codexAmounts is one account/rateLimits/read answer, kept with the time it was read.
type codexAmounts struct {
	At   int64
	Snap codexRateSnap
}

type codexRPCState struct {
	mu     sync.Mutex
	at     time.Time // last attempt
	busy   bool
	res    *codexAmounts
	err    string
	logged bool
}

// codexRateRead asks a short-lived Codex app-server for the account rate limits; Codex uses its own login.
func codexRateRead(ctx context.Context) (json.RawMessage, error) {
	cmd := newCodexAppServerCmd()
	conn, err := startCodexConn(cmd, codexHandlers{request: func(c *codexConn, id json.RawMessage, _ string, _ json.RawMessage) {
		_ = c.respondError(id, -32601, "not supported")
	}})
	if err != nil {
		return nil, err
	}
	defer conn.reap(codexReapCap)
	if _, err := conn.call(ctx, "initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "agent-monitor", "title": "agent-monitor", "version": "0.1.0"},
		"capabilities": map[string]any{"experimentalApi": false},
	}); err != nil {
		return nil, err
	}
	if err := conn.notify("initialized", nil); err != nil {
		return nil, err
	}
	return conn.call(ctx, "account/rateLimits/read", map[string]any{})
}

// parseCodexAmounts picks the "codex" bucket of rateLimitsByLimitId, else the single rateLimits view.
func parseCodexAmounts(raw json.RawMessage, at int64) (*codexAmounts, error) {
	var r struct {
		RateLimits *codexRateSnap           `json:"rateLimits"`
		ByID       map[string]codexRateSnap `json:"rateLimitsByLimitId"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	if s, ok := r.ByID["codex"]; ok {
		return &codexAmounts{At: at, Snap: s}, nil
	}
	if r.RateLimits == nil {
		return nil, errors.New("no rateLimits in the answer")
	}
	return &codexAmounts{At: at, Snap: *r.RateLimits}, nil
}

// jsonKeyPaths lists the field names of a JSON value (no values), for a one-time log of the answer's shape.
func jsonKeyPaths(raw json.RawMessage) string {
	set := map[string]bool{}
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, c := range x {
				p := k
				if prefix != "" {
					p = prefix + "." + k
				}
				set[p] = true
				walk(p, c)
			}
		case []any:
			for _, c := range x {
				walk(prefix+"[]", c)
			}
		}
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	walk("", v)
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// kickCodexAmounts starts a background read when Codex is installed and the last try is older than codexRPCEvery.
func (u *usageLimits) kickCodexAmounts() {
	if !u.installed("codex") {
		return
	}
	s := &u.codexRPC
	now := u.now()
	s.mu.Lock()
	if s.busy || (!s.at.IsZero() && now.Sub(s.at) < codexRPCEvery) {
		s.mu.Unlock()
		return
	}
	s.busy, s.at = true, now
	s.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), codexRPCTimeout)
		defer cancel()
		raw, err := u.codexRead(ctx)
		var res *codexAmounts
		if err == nil {
			s.mu.Lock()
			first := !s.logged
			s.logged = true
			s.mu.Unlock()
			if first {
				log.Printf("usage: codex account/rateLimits/read fields: %s", jsonKeyPaths(raw))
			}
			res, err = parseCodexAmounts(raw, time.Now().UnixMilli())
		}
		s.mu.Lock()
		s.busy = false
		if err != nil {
			s.err = "Codex amounts unavailable: " + clipString(strings.TrimSpace(err.Error()), 200)
		} else {
			s.res, s.err = res, ""
		}
		s.mu.Unlock()
		u.mu.Lock()
		u.cached = nil // the next get() shows the new reading
		u.mu.Unlock()
	}()
}

func rawFloat(b json.RawMessage) (float64, bool) {
	t := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if t == "" || t == "null" {
		return 0, false
	}
	f, err := strconv.ParseFloat(t, 64)
	return f, err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
}

// applyCodexAmounts overlays the app-server answer on the rollout-based Codex usage; an error only sets the note.
func (u *usageLimits) applyCodexAmounts(a *agentUsage, now time.Time, flagsAt, creditsAt int64) {
	s := &u.codexRPC
	s.mu.Lock()
	res, errText := s.res, s.err
	s.mu.Unlock()
	if errText != "" {
		a.Note = errText
	}
	if res == nil || now.UnixMilli()-res.At > codexRPCExpire.Milliseconds() {
		return
	}
	sn := res.Snap
	if sn.PlanType != nil && *sn.PlanType != "" {
		a.Plan = *sn.PlanType
	}
	for i, w := range []*codexRateWin{sn.Primary, sn.Secondary} {
		if w == nil || res.At < a.MeasuredAt {
			continue
		}
		cw := &codexWin{UsedPercent: &w.UsedPercent}
		if w.WindowDurationMins != nil {
			cw.WindowMinutes = int(*w.WindowDurationMins)
		}
		key := codexWinKey(cw, i == 0)
		mins := map[string]int{"5h": 300, "weekly": 10080}[key]
		if cw.WindowMinutes > 0 {
			mins = cw.WindowMinutes
		}
		var resets int64
		if w.ResetsAt != nil {
			resets = *w.ResetsAt * 1000
		}
		nw := u.window("codex", key, mins, &w.UsedPercent, resets, res.At, now)
		for j := range a.Windows {
			if a.Windows[j].Key == key {
				a.Windows[j] = nw
			}
		}
		a.MeasuredAt = max(a.MeasuredAt, res.At)
		if a.Note != "" && strings.HasPrefix(a.Note, "No plan reading") {
			a.Note = ""
		}
	}
	if il := sn.IndividualLimit; il != nil {
		used, ok1 := rawFloat(il.Used)
		limit, ok2 := rawFloat(il.Limit)
		if ok1 && ok2 && res.At > creditsAt {
			pct := 0.0
			if il.RemainingPercent != nil {
				pct = 100 - *il.RemainingPercent
			} else if limit > 0 {
				pct = used / limit * 100
			}
			pct = math.Max(0, math.Min(100, pct))
			c := &usageCredits{Source: "Codex app-server account/rateLimits/read", MeasuredAt: res.At, Spent: &used, Limit: &limit, Percent: &pct}
			c.CapReached = sn.SpendReached != nil && *sn.SpendReached
			a.Credits = c
		}
	}
	if res.At <= flagsAt {
		return
	}
	fl := &codexFlags{Spend: sn.SpendReached != nil && *sn.SpendReached}
	if sn.ReachedType != nil {
		fl.Reached = *sn.ReachedType
	}
	if al := codexAlert(fl); al != "" {
		a.Alert = al
	} else if sn.SpendReached != nil || sn.ReachedType != nil {
		a.Alert = ""
	}
	if a.Note == "" || strings.HasPrefix(a.Note, "Business plan") {
		if sn.Primary == nil && sn.Secondary == nil && a.Credits != nil {
			a.Note = ""
		}
	}
}
