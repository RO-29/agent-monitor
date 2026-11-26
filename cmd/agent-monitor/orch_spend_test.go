package main

import (
	"math"
	"strings"
	"sync"
	"testing"
)

type notifySink struct {
	mu  sync.Mutex
	evs []notifyEvent
}

func (s *notifySink) add(ev notifyEvent) {
	s.mu.Lock()
	s.evs = append(s.evs, ev)
	s.mu.Unlock()
}

func (s *notifySink) titles(kind string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, e := range s.evs {
		if e.Kind == kind {
			out = append(out, e.Title)
		}
	}
	return out
}

func TestSpendThreadLimitAllowOnceAndNotify(t *testing.T) {
	e := newOrchEnv(t)
	cfg := SpendSettings{PerThreadUSD: 0.015}
	e.m.spendCfg = func() SpendSettings { return cfg }
	sink := &notifySink{}
	e.m.notifier = sink.add
	srv := e.server()

	th := e.createClaude("hello", "")
	e.idle(th.ID)
	e.until("80% notice", func() bool { return len(sink.titles("spend")) == 1 })
	if got := sink.titles("spend")[0]; !strings.Contains(got, "80%") {
		t.Fatalf("notice %q", got)
	}
	if _, err := e.m.Send(th.ID, "hello"); err != nil {
		t.Fatal(err)
	}
	e.until("second run", func() bool { r := e.runs(th.ID); return len(r) == 2 && r[1].Status == "completed" })
	e.until("100% notice", func() bool { return len(sink.titles("spend")) == 2 })
	if got := sink.titles("spend")[1]; !strings.Contains(got, "reached its spend limit") {
		t.Fatalf("notice %q", got)
	}

	code, out := postJSON(t, srv.URL+"/api/orch/threads/"+th.ID+"/messages", `{"text":"more"}`)
	if code != 402 || out["scope"] != "thread" || out["limit"].(float64) != 0.015 || out["spent"].(float64) != 0.03 {
		t.Fatalf("over limit: %d %v", code, out)
	}
	st := e.m.SpendStatus(th.ID)
	if ts := st["thread"].(spendScope); !ts.Blocked || ts.Spent != 0.03 {
		t.Fatalf("status %+v", st)
	}
	if code, out := postJSON(t, srv.URL+"/api/orch/threads/"+th.ID+"/allow-once", ""); code != 200 || out["thread"].(map[string]any)["spendAllowOnce"] != true {
		t.Fatalf("allow once: %d %v", code, out)
	}
	if ts := e.m.SpendStatus(th.ID)["thread"].(spendScope); ts.Blocked || !ts.AllowedOnce {
		t.Fatalf("allowed status %+v", ts)
	}
	if code, out := postJSON(t, srv.URL+"/api/orch/threads/"+th.ID+"/messages", `{"text":"hello"}`); code != 200 {
		t.Fatalf("allowed send: %d %v", code, out)
	}
	e.until("third run", func() bool { r := e.runs(th.ID); return len(r) == 3 && r[2].Status == "completed" })
	if _, err := e.m.Send(th.ID, "again"); err == nil {
		t.Fatal("allow once let two turns through")
	}
	if n := len(sink.titles("spend")); n != 2 {
		t.Fatalf("extra notices after the limit: %d", n)
	}
	cfg.PerThreadUSD = 1
	if _, err := e.m.Send(th.ID, "hello"); err != nil {
		t.Fatalf("raised limit still blocks: %v", err)
	}
	e.until("fourth run", func() bool { r := e.runs(th.ID); return len(r) == 4 && r[3].Status != "running" })
}

func TestSpendDayLimitBlocksNewThreads(t *testing.T) {
	e := newOrchEnv(t)
	cfg := SpendSettings{PerDayUSD: 0.01}
	e.m.spendCfg = func() SpendSettings { return cfg }
	srv := e.server()
	th := e.createClaude("hello", "")
	e.idle(th.ID)
	code, out := postJSON(t, srv.URL+"/api/orch/threads", `{"agent":"codex","cwd":"`+e.cwd+`","prompt":"hello"}`)
	if code != 402 || out["scope"] != "day" {
		t.Fatalf("new thread over the day limit: %d %v", code, out)
	}
	if _, err := e.m.Delegate(th.ID, orchChildSpec{Prompt: "x", Agent: "codex"}); err == nil {
		t.Fatal("delegate over the day limit")
	}
	if d := e.m.SpendStatus("")["day"].(spendScope); !d.Blocked || d.Limit != 0.01 {
		t.Fatalf("day status %+v", d)
	}
}

func TestSpendCodexRates(t *testing.T) {
	cfg := SpendSettings{CodexRates: map[string]SpendRate{"gpt-x": {In: 2, Out: 10}, "*": {In: 1, Out: 1}}}
	r := &ORun{Usage: &OUsage{InTokens: 1_000_000, OutTokens: 500_000}}
	if c := runCost(&OThread{Agent: "codex", Model: "gpt-x"}, r, cfg); c != 7 {
		t.Fatalf("model rate %v", c)
	}
	if c := runCost(&OThread{Agent: "codex"}, r, cfg); c != 1.5 {
		t.Fatalf("default rate %v", c)
	}
	// No rate: the built-in table, gpt prices 1.25 in and 10 out per 1M.
	if c := runCost(&OThread{Agent: "codex", Model: "gpt-6-luna"}, r, SpendSettings{}); c != 6.25 {
		t.Fatalf("no rate %v", c)
	}
	if c, est := codexRunCost("", &OUsage{InTokens: 1_000_000, CachedIn: 800_000}, SpendSettings{}); !est || math.Abs(c-0.35) > 1e-9 {
		t.Fatalf("cached estimate %v %v", c, est)
	}
	if c, est := codexRunCost("gpt-x", r.Usage, cfg); est || c != 7 {
		t.Fatalf("a configured rate is not an estimate: %v %v", c, est)
	}
	r.Usage.CostUSD = 0.5
	if c := runCost(&OThread{Agent: "claude"}, r, cfg); c != 0.5 {
		t.Fatalf("reported cost %v", c)
	}
	if c := runCost(&OThread{Agent: "codex"}, r, SpendSettings{}); c != 0.5 {
		t.Fatalf("stored estimate %v", c)
	}
	if c := runCost(&OThread{Agent: "codex", Model: "gpt-x"}, r, cfg); c != 7 {
		t.Fatalf("a configured rate wins over the stored estimate %v", c)
	}
}

func TestOrchThreadCosts(t *testing.T) {
	e := newOrchEnv(t)
	th := e.create("hello")
	e.waitRunEnd()
	_ = e.m.store.write(func(tx *orchTx) error {
		return tx.putRun(&ORun{ID: "r_cost", ThreadID: th.ID, Index: 9, Status: "completed", Usage: &OUsage{InTokens: 2_000_000, OutTokens: 500_000, CostUSD: 1.234}})
	})
	if got := e.m.ThreadCosts()[th.ID]; got <= 0 {
		t.Fatalf("cost = %v, want more than 0", got)
	}
}
