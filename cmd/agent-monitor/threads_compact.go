package main

import (
	"net/url"
	"sort"
	"strconv"
)

// threadCompact is a list row: the fields the iOS and list views read, without traces, tokens and strips.
type threadCompact struct {
	ID              string   `json:"id"`
	Title           string   `json:"title"`
	Cwd             string   `json:"cwd"`
	Ref             string   `json:"ref,omitempty"`
	Tools           []string `json:"tools"`
	Sessions        []string `json:"sessions"`
	State           State    `json:"state"`
	StartedAt       int64    `json:"startedAt"`
	LastAt          int64    `json:"lastAt"`
	Turns           int      `json:"turns"`
	Attention       bool     `json:"attention"`
	Model           string   `json:"model,omitempty"`
	ParentThread    string   `json:"parentThread,omitempty"`
	ChildKind       string   `json:"childKind,omitempty"`
	ChildCount      int      `json:"childCount"`
	ChildAttention  int      `json:"childAttention"`
	Approvals       int      `json:"approvals"`
	LiveActivity    string   `json:"liveActivity,omitempty"`
	DrivenID        string   `json:"drivenId,omitempty"`
	Handover        string   `json:"handover,omitempty"`
	TerminalSession string   `json:"terminalSession,omitempty"`
	Kind            string   `json:"kind,omitempty"`
	Status          string   `json:"status,omitempty"`
	Group           string   `json:"group,omitempty"`
	Phase           string   `json:"phase,omitempty"`
	SpendUSD        float64  `json:"spendUsd,omitempty"`
	Display         string   `json:"display"`
	Repo            string   `json:"repo"`
	OutcomeLine     string   `json:"outcomeLine,omitempty"`
	ErrorsOpen      int      `json:"errorsOpen"`
}

func compactThread(t ThreadV3) threadCompact {
	return threadCompact{ID: t.ID, Title: t.Title, Cwd: t.Cwd, Ref: t.Ref, Tools: t.Tools, Sessions: t.Sessions, State: t.State,
		StartedAt: t.StartedAt, LastAt: t.LastAt, Turns: t.Turns, Attention: t.Attention, Model: t.Model,
		ParentThread: t.ParentThread, ChildKind: t.ChildKind, ChildCount: t.ChildCount, ChildAttention: t.ChildAttention,
		Approvals: t.Approvals, LiveActivity: t.LiveActivity, DrivenID: t.DrivenID, Handover: t.Handover,
		TerminalSession: t.TerminalSession, Kind: t.Kind, Status: t.Status, Group: t.Group, Phase: t.Phase, SpendUSD: t.SpendUSD,
		Display: t.Display, Repo: t.Repo, OutcomeLine: t.OutcomeLine, ErrorsOpen: t.ErrorsOpen}
}

// shapeThreads applies compact=1, since=<ms> and limit=<n> to a threadsResponse; without them it returns res as is.
func shapeThreads(res map[string]any, q url.Values) map[string]any {
	compact := q.Get("compact") == "1"
	since, _ := strconv.ParseInt(q.Get("since"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	if !compact && since <= 0 && limit <= 0 {
		return res
	}
	threads, _ := res["threads"].([]ThreadV3)
	out := map[string]any{}
	for k, v := range res {
		out[k] = v
	}
	if since > 0 || limit > 0 {
		kept := make([]ThreadV3, 0, len(threads))
		for _, t := range threads {
			if t.LastAt >= since {
				kept = append(kept, t)
			}
		}
		if limit > 0 && len(kept) > limit {
			sort.SliceStable(kept, func(i, j int) bool { return kept[i].LastAt > kept[j].LastAt })
			kept = kept[:limit]
		}
		ids := make(map[string]bool, len(kept))
		for _, t := range kept {
			ids[t.ID] = true
		}
		if of, ok := res["threadOf"].(map[string]string); ok {
			sub := map[string]string{}
			for s, th := range of {
				if ids[th] {
					sub[s] = th
				}
			}
			out["threadOf"] = sub
		}
		threads = kept
	}
	out["threads"] = threads
	if compact {
		rows := make([]threadCompact, len(threads))
		for i, t := range threads {
			rows[i] = compactThread(t)
		}
		out["threads"] = rows
		delete(out, "stacks")
	}
	return out
}
