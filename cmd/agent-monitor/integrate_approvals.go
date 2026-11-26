package main

import (
	"sort"
	"sync"
	"time"
)

// Feeds approval data into ops.go (needs-you counts, wait bars, monitors).
func init() {
	pendingApprovalsFn = pendingApprovalsBySession
	pendingApprovalAgesFn = func() []struct {
		SessionID string
		Since     int64
	} {
		var out []struct {
			SessionID string
			Since     int64
		}
		if approvalStore == nil {
			return out
		}
		approvalStore.mu.Lock()
		defer approvalStore.mu.Unlock()
		for _, p := range approvalStore.pending {
			out = append(out, struct {
				SessionID string
				Since     int64
			}{p.a.SessionID, p.a.CreatedAt})
		}
		return out
	}
	waitIntervalsFn = func(sessionID string, from, to int64) [][2]int64 {
		var out [][2]int64
		now := time.Now().UnixMilli()
		for _, a := range recentDecided() {
			if a.SessionID == sessionID && a.DecidedBy != "" && a.State != "auto-allowed" && a.State != "auto-denied" && a.State != "passed" {
				if a.DecidedAt >= from && a.CreatedAt <= to && a.DecidedAt > a.CreatedAt {
					out = append(out, [2]int64{a.CreatedAt, a.DecidedAt})
				}
			}
		}
		for _, p := range pendingApprovalAgesFn() {
			if p.SessionID == sessionID && p.Since <= to {
				out = append(out, [2]int64{p.Since, min(now, to)})
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
		return out
	}
	repeatDeniesFn = func(since int64) []struct {
		SessionID, Summary string
		Count              int
	} {
		type key struct{ s, sum string }
		n := map[key]int{}
		for _, a := range recentDecided() {
			if a.DecidedAt >= since && (a.State == "denied" || a.State == "auto-denied") {
				n[key{a.SessionID, a.Summary}]++
			}
		}
		var out []struct {
			SessionID, Summary string
			Count              int
		}
		for k, c := range n {
			out = append(out, struct {
				SessionID, Summary string
				Count              int
			}{k.s, k.sum, c})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })
		return out
	}
}

var decidedCache = struct {
	sync.Mutex
	at   int64
	list []*Approval
}{}

// recentDecided returns the last 8 days of decided approvals, cached for 15 s.
func recentDecided() []*Approval {
	if approvalStore == nil {
		return nil
	}
	decidedCache.Lock()
	defer decidedCache.Unlock()
	now := time.Now().UnixMilli()
	if decidedCache.list != nil && now-decidedCache.at < 15_000 {
		return decidedCache.list
	}
	list, _ := approvalStore.audit(now-8*86_400_000, 5000)
	decidedCache.list, decidedCache.at = list, now
	return list
}
