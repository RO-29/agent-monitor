package main

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"
)

var errBadSuggestKey = errors.New("bad suggestion key")

// RuleSuggestion: a call the user allowed by hand often, with no deny, that no allow rule covers yet.
type RuleSuggestion struct {
	Key       string `json:"key"`
	Agent     string `json:"agent,omitempty"`
	Tool      string `json:"tool"`
	Pattern   string `json:"pattern"`
	CwdPrefix string `json:"cwdPrefix,omitempty"`
	Count     int    `json:"count"`
	LastAt    int64  `json:"lastAt"`
	Example   string `json:"example"`
}

const (
	suggestWindow = 30 * 24 * time.Hour
	suggestMin    = 3
	suggestMax    = 8
)

func suggestKey(agent, tool, pattern, cwd string) string {
	h := sha1.Sum([]byte(agent + "\x00" + tool + "\x00" + pattern + "\x00" + cwd))
	return hex.EncodeToString(h[:6])
}

// ruleSuggestions reads the audit log; only human "allowed" answers count, and one deny of the same shape vetoes it.
func (s *ApprovalStore) ruleSuggestions(now time.Time) []RuleSuggestion {
	list, err := s.audit(now.Add(-suggestWindow).UnixMilli(), 5000)
	if err != nil {
		return []RuleSuggestion{}
	}
	s.mu.Lock()
	rules := append([]Rule{}, s.rules...)
	dismissed := s.loadDismissedLocked()
	s.mu.Unlock()
	type acc struct {
		sug    RuleSuggestion
		last   *Approval
		denied bool
	}
	by := map[string]*acc{}
	var order []string
	for _, a := range list {
		if a.Kind != "permission" || (a.State != "allowed" && a.State != "denied") {
			continue
		}
		p := alwaysPattern(a)
		// No folder means a rule for every folder: broader than what the user saw.
		if p == "" || a.Cwd == "" || isShellTool(a.ToolName) && isCompoundShell(a.Summary) {
			continue
		}
		k := suggestKey(a.Agent, a.ToolName, p, a.Cwd)
		x := by[k]
		if x == nil {
			x = &acc{sug: RuleSuggestion{Key: k, Agent: a.Agent, Tool: a.ToolName, Pattern: p, CwdPrefix: a.Cwd}}
			by[k] = x
			order = append(order, k)
		}
		if a.State == "denied" {
			x.denied = true
			continue
		}
		if !a.AlwaysOK {
			x.denied = true // high risk: a rule may never allow it
			continue
		}
		x.sug.Count++
		if a.DecidedAt > x.sug.LastAt {
			x.sug.LastAt, x.sug.Example, x.last = a.DecidedAt, clipString(a.Summary, 200), a
		}
	}
	out := []RuleSuggestion{}
	for _, k := range order {
		x := by[k]
		if x.denied || x.sug.Count < suggestMin || dismissed[k] || x.last == nil {
			continue
		}
		covered := false
		for _, r := range rules {
			if r.Action == "allow" && ruleMatches(r, x.last) {
				covered = true
				break
			}
		}
		if !covered {
			out = append(out, x.sug)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].LastAt > out[j].LastAt
	})
	if len(out) > suggestMax {
		out = out[:suggestMax]
	}
	return out
}

func (s *ApprovalStore) dismissedPath() string {
	return filepath.Join(s.dir, "rule-suggest-dismissed.json")
}

func (s *ApprovalStore) loadDismissedLocked() map[string]bool {
	m := map[string]bool{}
	if b, err := os.ReadFile(s.dismissedPath()); err == nil {
		var keys []string
		if json.Unmarshal(b, &keys) == nil {
			for _, k := range keys {
				m[k] = true
			}
		}
	}
	return m
}

func (s *ApprovalStore) dismissSuggestion(key string) error {
	if _, err := hex.DecodeString(key); err != nil || len(key) != 12 {
		return errBadSuggestKey
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.loadDismissedLocked()
	m[key] = true
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > 500 {
		keys = keys[len(keys)-500:]
	}
	return writeFileAtomic(s.dismissedPath(), keys)
}
