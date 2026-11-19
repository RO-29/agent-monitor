package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Local usage of opencode, cursor and pi, plus install detection and the Claude plan name.

// defaultBinDirs are install dirs checked when PATH (often minimal under launchd) misses a binary.
func defaultBinDirs(home string) []string {
	return []string{"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin",
		filepath.Join(home, ".local", "bin"), filepath.Join(home, ".npm-global", "bin"), filepath.Join(home, ".bun", "bin"),
		filepath.Join(home, ".volta", "bin"), filepath.Join(home, ".opencode", "bin"), filepath.Join(home, ".claude", "local"),
		filepath.Join(home, ".cargo", "bin"), filepath.Join(home, "go", "bin")}
}

// installed reports whether any of names is an executable on PATH or in a known install dir.
func (u *usageLimits) installed(names ...string) bool {
	for _, n := range names {
		if u.lookPath != nil {
			if _, err := u.lookPath(n); err == nil {
				return true
			}
		}
		for _, d := range u.binDirs {
			if st, err := os.Stat(filepath.Join(d, n)); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
				return true
			}
		}
	}
	return false
}

// cachedPlan holds a plan name read from a config file, keyed by its mtime.
type cachedPlan struct {
	mod    time.Time
	at     time.Time
	plan   string
	loaded bool
}

const claudePlanRecheck = 10 * time.Minute

// claudePlanName decodes only oauthAccount plan fields of ~/.claude.json; nothing else is kept or logged.
func (u *usageLimits) claudePlanName(now time.Time) string {
	c := &u.claudePlan
	if c.loaded && now.Sub(c.at) < claudePlanRecheck {
		return c.plan
	}
	c.at, c.loaded = now, true
	st, err := os.Stat(u.claudeCfg)
	if err != nil {
		c.plan, c.mod = "", time.Time{}
		return ""
	}
	if st.ModTime().Equal(c.mod) {
		return c.plan
	}
	c.mod = st.ModTime()
	var cfg struct {
		OauthAccount *struct {
			OrganizationType  string `json:"organizationType"`
			UserRateLimitTier string `json:"userRateLimitTier"`
			BillingType       string `json:"billingType"`
		} `json:"oauthAccount"`
	}
	b, err := os.ReadFile(u.claudeCfg)
	if err != nil || json.Unmarshal(b, &cfg) != nil || cfg.OauthAccount == nil {
		c.plan = ""
		return ""
	}
	c.plan = claudePlanLabel(cfg.OauthAccount.OrganizationType, cfg.OauthAccount.UserRateLimitTier)
	return c.plan
}

// claudePlanLabel turns ("claude_team", "default_claude_max_5x") into "team (max_5x)".
func claudePlanLabel(orgType, tier string) string {
	plan := strings.TrimPrefix(orgType, "claude_")
	t := tier
	if i := strings.Index(t, "claude_"); i >= 0 {
		t = t[i+len("claude_"):]
	}
	if t == plan || strings.HasPrefix(t, "default") {
		t = ""
	}
	switch {
	case plan == "":
		return t
	case t == "":
		return plan
	}
	return plan + " (" + t + ")"
}

// opencodeMsg is one assistant message file; opencode rewrites the file as the reply grows.
type opencodeMsg struct {
	mod   time.Time
	size  int64
	at    int64
	model string
	tok   TokenUsage
	cost  float64
}

type opencodeUsageScanner struct {
	root  string
	msgs  map[string]*opencodeMsg // at -1: not an assistant reply; nil: read failed, retried next scan
	nowFn func() time.Time
}

func newOpencodeUsageScanner(root string, now func() time.Time) *opencodeUsageScanner {
	return &opencodeUsageScanner{root: root, msgs: map[string]*opencodeMsg{}, nowFn: now}
}

// scan re-reads only message files whose size or mtime changed; session dirs untouched for a week are skipped.
func (s *opencodeUsageScanner) scan() {
	cut := s.nowFn().Add(-usageWeek - time.Hour)
	sessions, err := os.ReadDir(s.root)
	if err != nil {
		s.msgs = map[string]*opencodeMsg{}
		return
	}
	seen := map[string]bool{}
	for _, sd := range sessions {
		if !sd.IsDir() {
			continue
		}
		info, err := sd.Info()
		if err != nil || info.ModTime().Before(cut) {
			continue
		}
		dir := filepath.Join(s.root, sd.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, fe := range files {
			name := fe.Name()
			if !strings.HasPrefix(name, "msg_") || !strings.HasSuffix(name, ".json") {
				continue
			}
			fi, err := fe.Info()
			if err != nil || fi.ModTime().Before(cut) {
				continue
			}
			p := filepath.Join(dir, name)
			seen[p] = true
			if m, ok := s.msgs[p]; ok && m != nil && m.size == fi.Size() && m.mod.Equal(fi.ModTime()) {
				continue
			}
			s.msgs[p] = readOpencodeMsg(p, fi)
		}
	}
	keep := cut.UnixMilli()
	for p, m := range s.msgs {
		if !seen[p] || (m != nil && m.at >= 0 && m.at < keep) {
			delete(s.msgs, p)
		}
	}
}

func readOpencodeMsg(p string, fi os.FileInfo) *opencodeMsg {
	if fi.Size() > 4<<20 {
		return &opencodeMsg{mod: fi.ModTime(), size: fi.Size(), at: -1}
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var rec struct {
		Role       string  `json:"role"`
		ModelID    string  `json:"modelID"`
		ProviderID string  `json:"providerID"`
		Cost       float64 `json:"cost"`
		Tokens     *struct {
			Input     int64 `json:"input"`
			Output    int64 `json:"output"`
			Reasoning int64 `json:"reasoning"`
			Cache     struct {
				Read  int64 `json:"read"`
				Write int64 `json:"write"`
			} `json:"cache"`
		} `json:"tokens"`
		Time struct {
			Created int64 `json:"created"`
		} `json:"time"`
	}
	if json.Unmarshal(b, &rec) != nil || rec.Role != "assistant" || rec.Tokens == nil || rec.Time.Created == 0 {
		return &opencodeMsg{mod: fi.ModTime(), size: fi.Size(), at: -1}
	}
	model := rec.ModelID
	if rec.ProviderID != "" && model != "" {
		model = rec.ProviderID + "/" + model
	}
	if model == "" {
		model = "unknown"
	}
	t := rec.Tokens
	return &opencodeMsg{mod: fi.ModTime(), size: fi.Size(), at: rec.Time.Created, model: model, cost: rec.Cost,
		tok: TokenUsage{Input: t.Input, Output: t.Output + t.Reasoning, CacheRead: t.Cache.Read, CacheCreate: t.Cache.Write}}
}

func (s *opencodeUsageScanner) modelTotals(from int64) map[string]*usageModel {
	out := map[string]*usageModel{}
	for _, m := range s.msgs {
		if m == nil || m.at < from || m.model == "" {
			continue
		}
		r := out[m.model]
		if r == nil {
			r = &usageModel{Model: m.model}
			out[m.model] = r
		}
		r.InTokens += m.tok.Input
		r.OutTokens += m.tok.Output
		r.CacheRead += m.tok.CacheRead
		r.CacheWrite += m.tok.CacheCreate
		r.CostUSD += m.cost
		r.Messages++
	}
	return out
}

func (u *usageLimits) opencodeUsage(now time.Time) agentUsage {
	a := agentUsage{Agent: "opencode", Installed: u.installed("opencode"), Source: "opencode message files", SourcePath: u.opencode.root,
		LocalSource: "opencode messages in ~/.local/share/opencode/storage/message, cost as recorded by opencode (0 for subscription providers)",
		Windows:     []usageWindow{}}
	if !pathExists(u.opencode.root) {
		a.Note = "No opencode message storage found."
	} else {
		a.Note = "opencode has no plan windows; reasoning tokens count as output."
	}
	a.Local, a.Models = u.local(u.opencode.modelTotals, nil, now, false)
	return a
}

func (u *usageLimits) cursorUsage(now time.Time) agentUsage {
	a := agentUsage{Agent: "cursor", Installed: u.installed("cursor-agent", "cursor"), Source: "Cursor ai-code-tracking database", SourcePath: u.cursorDB,
		LocalSource: "~/.cursor/ai-tracking/ai-code-tracking.db: requests that wrote code, per model. Cursor records no token counts or cost locally.",
		Windows:     []usageWindow{}}
	var db *sql.DB
	if pathExists(u.cursorDB) {
		if d, err := sql.Open("sqlite", "file:"+u.cursorDB+"?mode=ro&_pragma=busy_timeout(2000)&_pragma=query_only(1)"); err == nil {
			db = d
			defer db.Close()
		}
	}
	failed := false
	totals := func(from int64) map[string]*usageModel {
		out := map[string]*usageModel{}
		if db == nil {
			return out
		}
		rows, err := db.Query(`SELECT COALESCE(NULLIF(model, ''), 'unknown'), COUNT(DISTINCT COALESCE(NULLIF(requestId, ''), hash))
			FROM ai_code_hashes WHERE createdAt >= ? GROUP BY 1`, from)
		if err != nil {
			failed = true
			return out
		}
		defer rows.Close()
		for rows.Next() {
			var m string
			var n int
			if rows.Scan(&m, &n) == nil {
				out[m] = &usageModel{Model: m, Messages: n}
			}
		}
		return out
	}
	a.Local, a.Models = u.local(totals, nil, now, false)
	switch {
	case db == nil:
		a.Note = "No Cursor ai-code-tracking database found. Cursor keeps no plan, token or credit data locally."
	case failed:
		a.Note = "The Cursor ai-code-tracking database could not be read."
	default:
		a.Note = "Cursor keeps no plan, token or credit data locally; messages count code-writing requests only."
	}
	return a
}

func (u *usageLimits) piUsage() agentUsage {
	a := agentUsage{Agent: "pi", Installed: u.installed("pi"), Source: "none", Windows: []usageWindow{}, Local: []usageLocal{}, Models: []usageModel{},
		LocalSource: "none"}
	if a.Installed {
		a.Note = "pi is installed, but agent-monitor does not read its local usage yet."
	} else {
		a.Note = "pi is not installed: no pi binary on PATH or in the known install dirs."
	}
	return a
}
