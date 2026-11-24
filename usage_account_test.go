package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestUsageAccountClaude(t *testing.T) {
	cfg := strings.Replace(claudeCfgJSON("acct-1", "acct-1", usageNow.Add(-time.Minute), true),
		`"accountUuid":"acct-1","organizationType"`, `"accountUuid":"acct-1","emailAddress":"dev@example.com","organizationName":"Example Co","organizationType"`, 1)
	a := claudeWith(t, cfg)
	if a.Account == nil || a.Account.Label != "d•••@example.com" || a.Account.Org != "Example Co" || len(a.Account.Key) != 16 {
		t.Fatalf("account %+v", a.Account)
	}
	b, _ := json.Marshal(a)
	if strings.Contains(string(b), "acct-1") || strings.Contains(string(b), "dev@") {
		t.Fatal("the raw account id or the full email left the daemon")
	}
	if other := claudeWith(t, claudeCfgJSON("acct-2", "acct-2", usageNow, true)); other.Account == nil || other.Account.Key == a.Account.Key {
		t.Fatalf("two accounts share a key: %+v", other.Account)
	}
	if same := claudeWith(t, claudeCfgJSON("acct-1", "acct-1", usageNow, true)); same.Account.Key != a.Account.Key {
		t.Fatal("one account gave two keys")
	}
}

func TestUsageAccountCodex(t *testing.T) {
	a, _ := codexUsageOf(t, "ok", true)
	if a.Account == nil || a.Account.Label != "d•••@example.com" || a.Account.Org != "Example Co" || a.Account.Key == "" {
		t.Fatalf("account %+v", a.Account)
	}
	raw := withCodexAccount(json.RawMessage(`{"accountId":"acc-9","rateLimits":{}}`), json.RawMessage(`{"userId":"u-1","account":{"type":"chatgpt","email":"x@example.com"}}`))
	k := codexAccountOf(raw)
	if k == nil || k.Key != newUsageAccount("acc-9", "", "").Key || strings.Contains(string(raw), "agentMonitorAccount") == false {
		t.Fatalf("accountId must win: %+v", k)
	}
	if codexAccountOf(withCodexAccount(json.RawMessage(`{"rateLimits":{}}`), json.RawMessage(`{"account":{"email":"x@example.com"}}`))) != nil {
		t.Fatal("an email alone must give no account key")
	}
	if codexAccountOf(json.RawMessage(`{"rateLimits":{}}`)) != nil {
		t.Fatal("no id must give no account")
	}
}
