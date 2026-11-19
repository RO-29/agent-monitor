package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// sessionTarget is an agent session reachable through a session host, a tmux pane, or both.
type sessionTarget struct {
	AgentID   string `json:"agentId"`
	Tool      Tool   `json:"tool"`
	SessionID string `json:"sessionId"`
	PaneID    string `json:"paneId,omitempty"`
	HostID    string `json:"hostId,omitempty"`
	Alias     string `json:"alias,omitempty"`
	Cwd       string `json:"cwd,omitempty"`
}

// sessionTargets merges registered panes and linked hosts by session.
func sessionTargets() []sessionTarget {
	byRaw := map[string]*sessionTarget{}
	var order []string
	add := func(t sessionTarget) *sessionTarget {
		_, raw := splitAgentID(t.AgentID)
		if cur := byRaw[raw]; cur != nil {
			return cur
		}
		c := t
		byRaw[raw] = &c
		order = append(order, raw)
		return &c
	}
	if paneRegistry != nil {
		for _, p := range paneRegistry.List() {
			add(sessionTarget{AgentID: p.AgentID, Tool: p.Tool, SessionID: p.SessionID, PaneID: p.PaneID, Alias: p.Alias, Cwd: p.Cwd})
		}
	}
	if hostRegistry != nil {
		for _, h := range hostRegistry.List() {
			if h.SessionID == "" {
				continue
			}
			t := add(sessionTarget{AgentID: string(h.Tool) + ":" + h.SessionID, Tool: h.Tool, SessionID: h.SessionID, PaneID: h.TmuxPane, Cwd: h.Cwd})
			t.HostID = h.HostID
		}
	}
	out := make([]sessionTarget, 0, len(order))
	for _, raw := range order {
		out = append(out, *byRaw[raw])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].AgentID < out[j].AgentID })
	return out
}

// resolveTargetIn finds one target by alias, agent id, pane id, host id, session-id prefix or a unique tool name.
func resolveTargetIn(list []sessionTarget, q string) (*sessionTarget, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, errors.New("recipient required")
	}
	_, qraw := splitAgentID(q)
	for _, match := range []func(t sessionTarget) bool{
		func(t sessionTarget) bool { return t.Alias != "" && strings.EqualFold(t.Alias, q) },
		func(t sessionTarget) bool { return t.AgentID == q || t.SessionID == qraw },
		func(t sessionTarget) bool { return strings.HasPrefix(q, "%") && t.PaneID == q },
		func(t sessionTarget) bool { return t.HostID != "" && t.HostID == q },
	} {
		for i := range list {
			if match(list[i]) {
				return &list[i], nil
			}
		}
	}
	var hits []int
	if len(q) >= 6 {
		for i := range list {
			if strings.HasPrefix(list[i].SessionID, q) {
				hits = append(hits, i)
			}
		}
		if len(hits) > 1 {
			return nil, fmt.Errorf("ambiguous prefix %q matches multiple agents", q)
		}
		if len(hits) == 1 {
			return &list[hits[0]], nil
		}
	}
	var ids []string
	for i := range list {
		if strings.EqualFold(string(list[i].Tool), q) {
			hits = append(hits, i)
			ids = append(ids, list[i].AgentID)
		}
	}
	switch {
	case len(hits) == 1:
		return &list[hits[0]], nil
	case len(hits) > 1:
		return nil, fmt.Errorf("multiple %s agents, be more specific (alias or full id): %v", q, ids)
	}
	return nil, fmt.Errorf("no agent matches %q (try `agent-monitor list`)", q)
}

func registerTargetRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/targets", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"targets": sessionTargets()})
	})
	mux.HandleFunc("GET /api/targets/resolve", func(w http.ResponseWriter, r *http.Request) {
		t, err := resolveTargetIn(sessionTargets(), r.URL.Query().Get("q"))
		if err != nil {
			writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"target": t})
	})
}

// ── CLI side ──

var cliHTTP = &http.Client{Timeout: 5 * time.Second}

// resolveViaTargets asks the daemon's host-aware resolver; ok is false when the daemon predates it.
func resolveViaTargets(q string) (string, error, bool) {
	resp, err := cliHTTP.Get(daemonURL() + "/api/targets/resolve?q=" + url.QueryEscape(q))
	if err != nil {
		return "", fmt.Errorf("daemon unreachable: %w", err), true
	}
	defer resp.Body.Close()
	var out struct {
		Target *sessionTarget `json:"target"`
		Error  string         `json:"error"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return "", nil, false
	}
	if out.Target != nil && out.Target.AgentID != "" {
		return out.Target.AgentID, nil, true
	}
	if out.Error != "" {
		return "", errors.New(out.Error), true
	}
	return "", nil, false
}

func fetchTargets() ([]sessionTarget, bool) {
	resp, err := cliHTTP.Get(daemonURL() + "/api/targets")
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	var out struct {
		Targets *[]sessionTarget `json:"targets"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&out) != nil || out.Targets == nil {
		return nil, false
	}
	return *out.Targets, true
}

func printTargets(w io.Writer, list []sessionTarget) {
	if len(list) == 0 {
		fmt.Fprintln(w, "(no registered agents)")
		return
	}
	fmt.Fprintf(w, "%-30s %-12s %-6s %-16s %-8s %s\n", "AGENT-ID", "TOOL", "PANE", "HOST", "ALIAS", "CWD")
	dash := func(s string, n int) string {
		if s == "" {
			return "-"
		}
		return truncate(s, n)
	}
	for _, t := range list {
		fmt.Fprintf(w, "%-30s %-12s %-6s %-16s %-8s %s\n", truncate(t.AgentID, 30), t.Tool, dash(t.PaneID, 6), dash(t.HostID, 16), dash(t.Alias, 8), t.Cwd)
	}
}

// hostSelfAgentID is the session linked to $AM_HOST_ID, the host this process runs under.
func hostSelfAgentID() string {
	id := os.Getenv("AM_HOST_ID")
	if !hostIDPattern.MatchString(id) {
		return ""
	}
	resp, err := cliHTTP.Get(daemonURL() + "/api/hosts/" + id)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var h HostInfo
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&h) != nil {
		return ""
	}
	return h.AgentID
}
