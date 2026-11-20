package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestFakeCodexAppServer is the fake app-server: the tests run this binary as "codex app-server".
func TestFakeCodexAppServer(t *testing.T) {
	mode := os.Getenv("FAKE_CODEX_APP_SERVER")
	if mode == "" {
		t.Skip("helper process")
	}
	if mode == "sleep" {
		time.Sleep(25 * time.Second)
		os.Exit(0)
	}
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		var m rpcMsg
		if json.Unmarshal(in.Bytes(), &m) != nil || len(m.ID) == 0 {
			continue
		}
		var res string
		if mode == "escape" && m.Method == "initialize" {
			gc := exec.Command(os.Args[0], "-test.run=^TestFakeCodexAppServer$")
			gc.Env = append(os.Environ(), "FAKE_CODEX_APP_SERVER=sleep")
			gc.Stdout = os.Stdout
			gc.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			_ = gc.Start()
		}
		switch m.Method {
		case "initialize":
			res = `{"userAgent":"fake"}`
		case "account/rateLimits/read":
			if mode == "hang" || mode == "escape" {
				time.Sleep(time.Hour)
			}
			if mode == "error" {
				os.Stdout.WriteString(`{"id":` + string(m.ID) + `,"error":{"code":-32000,"message":"not signed in"}}` + "\n")
				continue
			}
			var b bytes.Buffer
			_ = json.Compact(&b, []byte(fakeRateLimitsJSON))
			res = b.String()
		}
		os.Stdout.WriteString(`{"id":` + string(m.ID) + `,"result":` + res + "}\n")
	}
	os.Exit(0)
}

// Invented figures only.
const fakeRateLimitsJSON = `{"rateLimits":{"limitId":"codex","planType":"business","primary":null,"secondary":{"usedPercent":17,"windowDurationMins":10080,"resetsAt":1795000000},
"spendControlReached":false,"individualLimit":{"limit":"200.00","used":"50.00","remainingPercent":75,"resetsAt":1795000000}},
"rateLimitsByLimitId":{"codex":{"limitId":"codex","planType":"business","primary":null,"secondary":{"usedPercent":17,"windowDurationMins":10080,"resetsAt":1795000000},
"spendControlReached":false,"individualLimit":{"limit":"200.00","used":"50.00","remainingPercent":75,"resetsAt":1795000000}}},
"rateLimitResetCredits":{"availableCount":0,"credits":null},"futureField":{"x":1}}`

func fakeCodexBin(t *testing.T, mode string) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "codex")
	script := "#!/bin/sh\nFAKE_CODEX_APP_SERVER=" + mode + " exec '" + os.Args[0] + "' -test.run='^TestFakeCodexAppServer$'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_MONITOR_CODEX_BIN", bin)
}

func codexUsageOf(t *testing.T, mode string, wait bool) (agentUsage, *usageLimits) {
	t.Helper()
	fakeCodexBin(t, mode)
	home := usageHome(t)
	u := newUsageLimits(home, "", func() time.Time { return usageNow })
	u.lookPath = func(string) (string, error) { return "/x/codex", nil }
	u.kickCodexAmounts()
	if wait {
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			u.codexRPC.mu.Lock()
			busy := u.codexRPC.busy
			u.codexRPC.mu.Unlock()
			if !busy {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	return u.get().Agents[1], u
}

func TestCodexAmountsFromAppServer(t *testing.T) {
	a, u := codexUsageOf(t, "ok", true)
	c := a.Credits
	if c == nil || c.Spent == nil || *c.Spent != 50 || *c.Limit != 200 || *c.Percent != 25 || c.CapReached {
		t.Fatalf("credits %+v", c)
	}
	w := findWin(a, "weekly")
	if w.UsedPercent == nil || *w.UsedPercent != 17 || w.ResetsAt != 1795000000*1000 {
		t.Fatalf("weekly %+v", w)
	}
	if a.Plan != "business" || a.Note != "" {
		t.Fatalf("plan %q note %q", a.Plan, a.Note)
	}
	u.kickCodexAmounts() // inside the 10 minute window: no second call
	u.codexRPC.mu.Lock()
	busy := u.codexRPC.busy
	u.codexRPC.mu.Unlock()
	if busy {
		t.Fatal("a second read started inside the interval")
	}
}

func TestCodexAmountsErrorKeepsRolloutFlags(t *testing.T) {
	a, _ := codexUsageOf(t, "error", true)
	if !strings.Contains(a.Note, "not signed in") || a.Credits != nil {
		t.Fatalf("note %q credits %+v", a.Note, a.Credits)
	}
}

func TestCodexAmountsTimeout(t *testing.T) {
	fakeCodexBin(t, "hang")
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := codexRateRead(ctx)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 10*time.Second {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
}

func TestCodexAmountsNotInstalledNoCall(t *testing.T) {
	u := newUsageLimits(usageHome(t), "", func() time.Time { return usageNow })
	u.lookPath, u.binDirs = usageNoPath, nil
	called := false
	u.codexRead = func(context.Context) (json.RawMessage, error) { called = true; return nil, nil }
	u.kickCodexAmounts()
	time.Sleep(50 * time.Millisecond)
	if called {
		t.Fatal("codex is not installed: no call")
	}
}

func TestCodexAmountsLenientAndKeyPaths(t *testing.T) {
	res, err := parseCodexAmounts(json.RawMessage(fakeRateLimitsJSON), 1)
	if err != nil || res.Snap.IndividualLimit == nil {
		t.Fatalf("%v %+v", err, res)
	}
	if k := jsonKeyPaths(json.RawMessage(fakeRateLimitsJSON)); !strings.Contains(k, "rateLimits.individualLimit.used") || strings.Contains(k, "50.00") {
		t.Fatalf("key paths %q", k)
	}
	capped := `{"rateLimits":{"rateLimitReachedType":"workspace_member_usage_limit_reached","spendControlReached":true}}`
	r2, _ := parseCodexAmounts(json.RawMessage(capped), 1)
	u := newUsageLimits(usageHome(t), "", func() time.Time { return usageNow })
	r2.At = usageNow.UnixMilli()
	u.codexRPC.res = r2
	a := agentUsage{}
	u.applyCodexAmounts(&a, usageNow, 0, 0)
	if a.Alert != "Workspace spend cap reached (set by the workspace owner)." {
		t.Fatalf("alert %q", a.Alert)
	}
}

func TestCodexAmountsEscapedGrandchildDoesNotBlock(t *testing.T) {
	fakeCodexBin(t, "escape")
	prev := codexReapCap
	codexReapCap = time.Second
	defer func() { codexReapCap = prev }()
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := codexRateRead(ctx)
	if err == nil || time.Since(start) > 12*time.Second {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
}

func TestCodexAmountsStaleAndNewer(t *testing.T) {
	res, _ := parseCodexAmounts(json.RawMessage(`{"rateLimits":{"rateLimitReachedType":"rate_limit_reached","spendControlReached":true}}`), usageNow.UnixMilli()-1000)
	u := newUsageLimits(usageHome(t), "", func() time.Time { return usageNow })
	u.codexRPC.res = res
	a := agentUsage{Alert: "rollout alert"}
	u.applyCodexAmounts(&a, usageNow, usageNow.UnixMilli(), 0)
	if a.Alert != "rollout alert" {
		t.Fatalf("an older answer must not override newer rollout flags: %q", a.Alert)
	}
	res.At = usageNow.UnixMilli() - (31 * time.Minute).Milliseconds()
	b := agentUsage{}
	u.applyCodexAmounts(&b, usageNow, 0, 0)
	if b.Alert != "" {
		t.Fatalf("an answer older than 30 minutes must drop: %q", b.Alert)
	}
}

func TestRawFloatRejectsNaNAndInf(t *testing.T) {
	for _, in := range []string{`"NaN"`, `"Inf"`, `"-Inf"`, `"+Infinity"`} {
		if _, ok := rawFloat(json.RawMessage(in)); ok {
			t.Fatalf("%s accepted", in)
		}
	}
	if f, ok := rawFloat(json.RawMessage(`"12.50"`)); !ok || math.Abs(f-12.5) > 1e-9 {
		t.Fatal("plain number")
	}
}
