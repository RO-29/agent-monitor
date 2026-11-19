package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// registerTraceRoutes adds the trace / thread / chapter / learning endpoints
// (DESIGN.md §9). Existing endpoints are untouched.
func registerTraceRoutes(mux *http.ServeMux) {
	// GET /api/threads?state=&tool=&project=
	mux.HandleFunc("/api/threads", func(w http.ResponseWriter, r *http.Request) {
		sessions := store.All()
		threads, threadOf := computeThreads(sessions, panesBySession())
		byID := map[string]*Session{}
		for _, s := range sessions {
			byID[s.ID] = s
		}
		attachTraceSummaries(threads, byID)
		q := r.URL.Query()
		var out []Thread
		for _, t := range threads {
			if v := q.Get("tool"); v != "" && v != "all" && !contains(t.Tools, v) {
				continue
			}
			if v := q.Get("state"); v != "" && v != "all" {
				switch v {
				case "live":
					if !(t.State == StateRunning || t.State == StateIdle || t.State == StateAwaitingInput || t.State == StateAwaitingPermission) {
						continue
					}
				case "attention":
					if !t.Attention {
						continue
					}
				default:
					if string(t.State) != v {
						continue
					}
				}
			}
			if v := q.Get("project"); v != "" && v != "all" && t.Cwd != v {
				continue
			}
			out = append(out, t)
		}
		if out == nil {
			out = []Thread{}
		}
		writeJSON(w, map[string]any{"threads": out, "threadOf": threadOf})
	})

	// GET /api/thread/{id}[/learnings|/story]: the thread, its ledger, or the chapters of every member.
	mux.HandleFunc("/api/thread/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/thread/")
		parts := strings.SplitN(rest, "/", 2)
		id := parts[0]
		sub := ""
		if len(parts) == 2 {
			sub = parts[1]
		}
		sessions := store.All()
		threads, threadOf := computeThreads(sessions, panesBySession())
		byID := map[string]*Session{}
		for _, s := range sessions {
			byID[s.ID] = s
		}
		root := threadOf[id]
		if root == "" {
			root = id
		}
		var th *Thread
		for i := range threads {
			if threads[i].ID == root {
				th = &threads[i]
			}
		}
		if th == nil {
			writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "thread not found"})
			return
		}
		attachTraceSummaries([]Thread{*th}, byID)
		members := []*Session{}
		for _, sid := range th.Sessions {
			if s := byID[sid]; s != nil {
				members = append(members, s)
			}
		}
		switch sub {
		case "":
			sums := map[string]*TraceSummary{}
			for _, m := range members {
				if s := cachedSummary(m); s != nil {
					sums[m.ID] = s
				}
			}
			out := map[string]any{"thread": th, "sessions": members, "summaries": sums}
			if searchIx != nil && len(th.Sessions) > 1 {
				out["turns"] = searchIx.promptCounts(th.Sessions)
			}
			writeJSON(w, out)
		case "story":
			type memberStory struct {
				Session  *Session   `json:"session"`
				Segments []Segment  `json:"segments"`
				Trace    *traceMeta `json:"trace"`
			}
			var story []memberStory
			for _, m := range members {
				tr := buildTrace(m)
				ms := memberStory{Session: m, Segments: []Segment{}}
				if tr != nil {
					ms.Segments = withEnriched(m.ID, tr.Segments)
					ms.Trace = metaOf(tr)
				}
				story = append(story, ms)
			}
			writeJSON(w, map[string]any{"thread": th, "members": story})
		case "learnings":
			var all []Learning
			var outs []Output
			for _, m := range members {
				tr := buildTrace(m)
				if tr == nil {
					continue
				}
				for _, l := range tr.Learnings {
					l.Ref = firstOf(l.Ref, m.ID)
					all = append(all, withSession(l, m.ID))
				}
				for _, o := range tr.Outputs {
					all = append(all, Learning{ID: spanID("lrn", o.Ref+o.Label), Source: "output", Text: o.Label, Evidence: o.Kind + " · " + clipString(o.Ref, 120), Ts: o.Ts, Seg: o.Seg, Ref: m.ID})
					outs = append(outs, o)
				}
				// enriched chapters may add learnings
				for i := range tr.Segments {
					if c := loadEnrichedChapter(m.ID, i); c != nil {
						for _, l := range c.Learnings {
							if l.Source == "memory" || l.Source == "output" {
								continue
							}
							l.Ref = m.ID
							l.Seg = i
							all = append(all, l)
						}
					}
				}
			}
			sort.Slice(all, func(i, j int) bool { return all[i].Ts < all[j].Ts })
			if all == nil {
				all = []Learning{}
			}
			writeJSON(w, map[string]any{"thread": th, "learnings": dedupeLearnings(all), "outputs": outs})
		default:
			http.NotFound(w, r)
		}
	})

	// GET /api/session/{id}/{trace,segments,spans,span/{spanId},chapter/{seg}}; POST .../enrich/{seg} and .../promote.
	mux.HandleFunc("/api/trace/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/trace/")
		parts := strings.Split(rest, "/")
		if len(parts) < 2 {
			http.NotFound(w, r)
			return
		}
		id := parts[0]
		sess := store.Get(id)
		if sess == nil {
			writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "session not found"})
			return
		}
		tr := buildTrace(sess)
		if tr == nil {
			writeJSONStatus(w, http.StatusUnprocessableEntity, map[string]any{"error": "no trace parser for this session"})
			return
		}
		q := r.URL.Query()
		switch parts[1] {
		case "full", "trace":
			spans := filterSpans(tr.Spans, q)
			writeJSON(w, map[string]any{
				"sessionId": tr.SessionID, "tool": tr.Tool, "segments": withEnriched(sess.ID, tr.Segments), "spans": spans,
				"learnings": tr.Learnings, "outputs": tr.Outputs, "costUsd": tr.CostUSD, "costEstimated": tr.CostEstimated,
				"contextUsed": tr.ContextUsed, "contextWindow": tr.ContextWindow, "model": tr.Model,
				"firstTs": tr.FirstTs, "lastTs": tr.LastTs, "generatedAt": tr.GeneratedAt, "spanTotal": len(tr.Spans),
			})
		case "segments":
			writeJSON(w, map[string]any{"segments": withEnriched(sess.ID, tr.Segments), "meta": metaOf(tr)})
		case "spans":
			spans := filterSpans(tr.Spans, q)
			writeJSON(w, map[string]any{"spans": spans, "total": len(tr.Spans)})
		case "span":
			if len(parts) < 3 {
				http.NotFound(w, r)
				return
			}
			var sp *Span
			for i := range tr.Spans {
				if tr.Spans[i].ID == parts[2] {
					sp = &tr.Spans[i]
				}
			}
			if sp == nil {
				writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "span not found"})
				return
			}
			io := spanIOFor(sess, tr, sp.ID)
			// same command elsewhere in this session (Bash only)
			var same []map[string]any
			if sp.Fam == "bash" {
				for _, o := range tr.Spans {
					if o.Fam == "bash" && o.Res == sp.Res && o.ID != sp.ID {
						same = append(same, map[string]any{"id": o.ID, "ts": o.Ts, "dur": o.Dur, "err": o.Err, "seg": o.Seg})
					}
				}
			}
			var parents []Span
			cur := sp.Parent
			for cur != "" {
				found := false
				for i := range tr.Spans {
					if tr.Spans[i].ID == cur {
						parents = append([]Span{tr.Spans[i]}, parents...)
						cur = tr.Spans[i].Parent
						found = true
						break
					}
				}
				if !found {
					break
				}
			}
			// what happened next: the next user prompt after this span
			var next *Span
			for i := range tr.Spans {
				if tr.Spans[i].Kind == "user" && tr.Spans[i].Ts > sp.Ts+sp.Dur {
					next = &tr.Spans[i]
					break
				}
			}
			writeJSON(w, map[string]any{"span": sp, "args": io.Args, "result": io.Result, "same": same, "parents": parents, "next": next})
		case "chapter":
			seg := atoiDefault(parts, 2, -1)
			if seg < 0 || seg >= len(tr.Segments) {
				writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "segment not found"})
				return
			}
			det := tr.Segments[seg].Chapter
			enr := loadEnrichedChapter(sess.ID, seg)
			writeJSON(w, map[string]any{"deterministic": det, "enriched": enr, "segment": tr.Segments[seg]})
		case "enrich":
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			seg := atoiDefault(parts, 2, -1)
			force := q.Get("force") == "1"
			c, errStr := enrichSegment(sess, tr, seg, force)
			if errStr != "" {
				writeJSONStatus(w, http.StatusBadGateway, map[string]any{"error": errStr})
				return
			}
			writeJSON(w, map[string]any{"ok": true, "chapter": c})
		case "promote":
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			var body struct {
				Text string `json:"text"`
				Type string `json:"type"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if strings.TrimSpace(body.Text) == "" {
				writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "text required"})
				return
			}
			path, err := promoteLearning(sess, strings.TrimSpace(body.Text), body.Type)
			if err != nil {
				writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			writeJSON(w, map[string]any{"ok": true, "path": path})
		default:
			http.NotFound(w, r)
		}
	})

	// GET /api/context/live → per live session: used vs window
	mux.HandleFunc("/api/context/live", func(w http.ResponseWriter, r *http.Request) {
		type ctxRow struct {
			SessionID string `json:"sessionId"`
			Used      int64  `json:"used"`
			Window    int64  `json:"window"`
			Segments  int    `json:"segments"`
			State     State  `json:"state"`
		}
		var rows []ctxRow
		for _, s := range store.All() {
			if !(s.State == StateRunning || s.State == StateIdle || s.State == StateAwaitingInput || s.State == StateAwaitingPermission) {
				continue
			}
			sum := cachedSummary(s)
			if sum == nil {
				continue
			}
			rows = append(rows, ctxRow{SessionID: s.ID, Used: sum.ContextUsed, Window: sum.ContextWindow, Segments: sum.Segments, State: s.State})
		}
		if rows == nil {
			rows = []ctxRow{}
		}
		writeJSON(w, map[string]any{"context": rows})
	})

	// GET /api/summaries → session id → TraceSummary (for list badges)
	mux.HandleFunc("/api/summaries", func(w http.ResponseWriter, r *http.Request) {
		out := map[string]*TraceSummary{}
		for _, s := range store.All() {
			if sum := cachedSummary(s); sum != nil {
				out[s.ID] = sum
			}
		}
		writeJSON(w, map[string]any{"summaries": out})
	})

	// GET|POST /api/settings
	mux.HandleFunc("/api/settings", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			cur := getSettings()
			var body struct {
				EnrichEnabled *bool    `json:"enrichEnabled"`
				EnrichModel   *string  `json:"enrichModel"`
				DailyCapUSD   *float64 `json:"dailyCapUsd"`
				RequireDevice *bool    `json:"requireDeviceToken"`
				AcpTimeout    *int     `json:"acpTurnTimeoutMin"`
				RemoteTerm    *bool    `json:"remoteTerminal"`
				UploadMaxAge  *int     `json:"uploadMaxAgeHours"`
				Spend         *struct {
					PerThreadUSD *float64             `json:"perThreadUsd"`
					PerDayUSD    *float64             `json:"perDayUsd"`
					CodexRates   map[string]SpendRate `json:"codexRates"`
				} `json:"spend"`
				Notify *struct {
					NeedsYou            *bool   `json:"needsYou"`
					Finished            *bool   `json:"finished"`
					Error               *bool   `json:"error"`
					Spend               *bool   `json:"spend"`
					QuietStart          *string `json:"quietStart"`
					QuietEnd            *string `json:"quietEnd"`
					QuietAllowsNeedsYou *bool   `json:"quietAllowsNeedsYou"`
					BatchSec            *int    `json:"batchSec"`
				} `json:"notify"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if (body.RequireDevice != nil || body.RemoteTerm != nil) && !localCaller(r) {
				writeJSONStatus(w, http.StatusForbidden, map[string]any{"error": "change remote access settings on the Mac itself"})
				return
			}
			if body.RequireDevice != nil {
				if err := checkRequireDeviceToken(r, *body.RequireDevice); err != nil {
					writeJSONStatus(w, http.StatusConflict, map[string]any{"error": strings.TrimPrefix(err.Error(), "lockout: ")})
					return
				}
				cur.RequireDeviceToken = *body.RequireDevice
			}
			if nb := body.Notify; nb != nil {
				nc := cur.Notify
				for _, f := range []struct {
					dst **bool
					v   *bool
				}{{&nc.NeedsYou, nb.NeedsYou}, {&nc.Finished, nb.Finished}, {&nc.Error, nb.Error},
					{&nc.Spend, nb.Spend}, {&nc.QuietAllowsNeedsYou, nb.QuietAllowsNeedsYou}} {
					if f.v != nil {
						*f.dst = f.v
					}
				}
				if nb.QuietStart != nil {
					nc.QuietStart = *nb.QuietStart
				}
				if nb.QuietEnd != nil {
					nc.QuietEnd = *nb.QuietEnd
				}
				if nb.BatchSec != nil {
					nc.BatchSec = nb.BatchSec
				}
				if err := validNotifySettings(nc); err != nil {
					writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
					return
				}
				cur.Notify = nc
			}
			if sp := body.Spend; sp != nil {
				if (sp.PerThreadUSD != nil && *sp.PerThreadUSD < 0) || (sp.PerDayUSD != nil && *sp.PerDayUSD < 0) {
					writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "spend limits must be 0 (off) or more"})
					return
				}
				if sp.PerThreadUSD != nil {
					cur.Spend.PerThreadUSD = *sp.PerThreadUSD
				}
				if sp.PerDayUSD != nil {
					cur.Spend.PerDayUSD = *sp.PerDayUSD
				}
				if sp.CodexRates != nil {
					cur.Spend.CodexRates = sp.CodexRates
				}
			}
			if body.RemoteTerm != nil {
				cur.RemoteTerminal = *body.RemoteTerm
			}
			if v := body.UploadMaxAge; v != nil {
				if *v < -1 || *v > 24*365 {
					writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "uploadMaxAgeHours must be -1 (keep), 0 (30 days) or 1 to 8760"})
					return
				}
				cur.UploadMaxAgeHours = *v
			}
			if body.AcpTimeout != nil && *body.AcpTimeout >= 0 {
				cur.AcpTurnTimeoutMin = *body.AcpTimeout
			}
			if body.EnrichEnabled != nil {
				cur.EnrichEnabled = *body.EnrichEnabled
			}
			if body.EnrichModel != nil && *body.EnrichModel != "" {
				cur.EnrichModel = *body.EnrichModel
			}
			if body.DailyCapUSD != nil && *body.DailyCapUSD > 0 {
				cur.DailyCapUSD = *body.DailyCapUSD
			}
			saveSettings(cur)
			if body.UploadMaxAge != nil && orchMgr != nil {
				go func() {
					orchUploadMu.Lock()
					defer orchUploadMu.Unlock()
					orchMgr.pruneUploads()
				}()
			}
			if body.RemoteTerm != nil && !*body.RemoteTerm {
				liveConns.closeRemoteTerms(r)
			}
		}
		writeJSON(w, getSettings())
	})
}

type traceMeta struct {
	CostUSD       float64 `json:"costUsd"`
	CostEstimated bool    `json:"costEstimated"`
	ContextUsed   int64   `json:"contextUsed"`
	ContextWindow int64   `json:"contextWindow"`
	Model         string  `json:"model"`
	FirstTs       int64   `json:"firstTs"`
	LastTs        int64   `json:"lastTs"`
	Spans         int     `json:"spans"`
	Learnings     int     `json:"learnings"`
	Outputs       int     `json:"outputs"`
}

func metaOf(tr *SessionTrace) *traceMeta {
	return &traceMeta{CostUSD: tr.CostUSD, CostEstimated: tr.CostEstimated, ContextUsed: tr.ContextUsed, ContextWindow: tr.ContextWindow,
		Model: tr.Model, FirstTs: tr.FirstTs, LastTs: tr.LastTs, Spans: len(tr.Spans), Learnings: len(tr.Learnings), Outputs: len(tr.Outputs)}
}

// withEnriched overlays stored LLM chapters on the deterministic ones.
func withEnriched(sessionID string, segs []Segment) []Segment {
	out := make([]Segment, len(segs))
	copy(out, segs)
	for i := range out {
		if c := loadEnrichedChapter(sessionID, i); c != nil {
			out[i].Chapter = c
		}
	}
	return out
}

func withSession(l Learning, sid string) Learning {
	if l.Ref == "" {
		l.Ref = sid
	}
	return l
}

func dedupeLearnings(in []Learning) []Learning {
	seen := map[string]bool{}
	var out []Learning
	for _, l := range in {
		k := l.Source + "|" + strings.ToLower(clipString(l.Text, 80))
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, l)
	}
	if out == nil {
		out = []Learning{}
	}
	return out
}

func filterSpans(spans []Span, q map[string][]string) []Span {
	get := func(k string) string {
		if v, ok := q[k]; ok && len(v) > 0 {
			return v[0]
		}
		return ""
	}
	from, _ := strconv.ParseInt(get("from"), 10, 64)
	to, _ := strconv.ParseInt(get("to"), 10, 64)
	minDur, _ := strconv.ParseInt(get("minDur"), 10, 64)
	seg := -1
	if v := get("seg"); v != "" {
		seg, _ = strconv.Atoi(v)
	}
	out := make([]Span, 0, len(spans))
	for _, s := range spans {
		if seg >= 0 && s.Seg != seg {
			continue
		}
		if from > 0 && s.Ts+s.Dur < from {
			continue
		}
		if to > 0 && s.Ts > to {
			continue
		}
		if minDur > 0 && s.Dur < minDur && s.Kind == "tool" {
			continue
		}
		out = append(out, s)
	}
	return out
}

func atoiDefault(parts []string, i, def int) int {
	if i >= len(parts) {
		return def
	}
	n, err := strconv.Atoi(parts[i])
	if err != nil {
		return def
	}
	return n
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func panesBySession() map[string]string {
	panes := map[string]string{}
	for _, reg := range paneRegistry.List() {
		panes[reg.AgentID] = reg.PaneID
	}
	return panes
}

func qInt(s string, d int) int {
	if s == "" {
		return d
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return d
	}
	return n
}
