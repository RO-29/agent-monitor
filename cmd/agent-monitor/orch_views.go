package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
)

// drivenRun is one run of a driven thread, reduced to what the list views need.
type drivenRun struct {
	Start, End int64
	Status     string
	Cost       float64
	In, Out    int64
}

// drivenAgg is the per-thread summary the v3 views show for a driven thread.
type drivenAgg struct {
	th        OThread
	runs      []drivenRun
	lastAgent string
	open      int
	items     []int64 // item start times in the activity window
	itemErrs  []int64
}

func (a *drivenAgg) cost() float64 {
	c := 0.0
	for _, r := range a.runs {
		c += r.Cost
	}
	return c
}

// drivenSnapshot reads every driven thread with its runs, last agent message and open requests in one read.
func drivenSnapshot(store *OrchStore, activityFrom int64) []*drivenAgg {
	if store == nil {
		return nil
	}
	var out []*drivenAgg
	_ = store.read(func(tx *sql.Tx) error {
		threads, err := scanJSONRows[OThread](tx.Query(`SELECT json FROM threads`))
		if err != nil {
			return err
		}
		by := map[string]*drivenAgg{}
		for _, th := range threads {
			a := &drivenAgg{th: th}
			by[th.ID] = a
			out = append(out, a)
		}
		runs, _ := scanJSONRows[ORun](tx.Query(`SELECT json FROM runs ORDER BY idx`))
		for _, r := range runs {
			if a := by[r.ThreadID]; a != nil {
				dr := drivenRun{Start: r.StartedAt, End: r.EndedAt, Status: r.Status}
				if r.Usage != nil {
					dr.Cost, dr.In, dr.Out = r.Usage.CostUSD, r.Usage.InTokens, r.Usage.OutTokens
				}
				a.runs = append(a.runs, dr)
			}
		}
		rows, err := tx.Query(`SELECT thread_id, COUNT(*) FROM requests WHERE json_extract(json,'$.state')='open' GROUP BY thread_id`)
		if err == nil {
			for rows.Next() {
				var id string
				var n int
				if rows.Scan(&id, &n) == nil && by[id] != nil {
					by[id].open = n
				}
			}
			rows.Close()
		}
		for _, a := range out {
			var js string
			if tx.QueryRow(`SELECT json FROM items WHERE thread_id=? AND json_extract(json,'$.kind')='agent' AND json_extract(json,'$.text')<>'' ORDER BY ord DESC LIMIT 1`, a.th.ID).Scan(&js) == nil {
				var it OItem
				if json.Unmarshal([]byte(js), &it) == nil {
					a.lastAgent = it.Text
				}
			}
			if activityFrom > 0 && a.th.UpdatedAt >= activityFrom {
				r, err := tx.Query(`SELECT json_extract(json,'$.startedAt'), json_extract(json,'$.status') FROM items WHERE thread_id=? ORDER BY ord DESC LIMIT 2000`, a.th.ID)
				if err != nil {
					continue
				}
				for r.Next() {
					var at int64
					var st sql.NullString
					if r.Scan(&at, &st) != nil {
						continue
					}
					if at < activityFrom {
						break
					}
					a.items = append(a.items, at)
					if st.String == "failed" {
						a.itemErrs = append(a.itemErrs, at)
					}
				}
				r.Close()
			}
		}
		return nil
	})
	return out
}

// drivenState maps an orchestrator status to a watched session state (same rule as the web fallback).
func drivenState(a *drivenAgg) State {
	switch {
	case a.th.Status == "running" || a.th.Status == "starting":
		return StateRunning
	case a.th.Status == "waiting" || a.open > 0:
		return StateAwaitingPermission
	case a.th.Status == "closed":
		return StateCompleted
	case a.th.Status == "error":
		return StateIdle
	case len(a.runs) > 0 || a.th.Group == "ready" || a.th.Group == "settled":
		return StateCompleted
	}
	return StateIdle
}

// drivenRowV3 is a first-class v3 row for a driven thread; row id and drivenId are the orchestrator id.
func drivenRowV3(a *drivenAgg, now int64) ThreadV3 {
	th := &a.th
	t := Thread{ID: th.ID, Title: th.Title, Cwd: th.Cwd, Ref: th.Branch, Tools: []string{th.Agent}, Sessions: []string{},
		State: drivenState(a), StartedAt: th.CreatedAt, LastAt: th.UpdatedAt, Turns: len(a.runs), Model: th.Model, Edges: []ThreadEdge{},
		Segments: 1, SegWeights: []int64{1}, SegKinds: []string{}, SessionSegs: []int{1}}
	if th.Worktree != nil && !th.Worktree.Removed {
		t.Ref = th.Worktree.Branch
	}
	for _, r := range a.runs {
		t.Tokens.Input += r.In
		t.Tokens.Output += r.Out
	}
	t.CostUSD = a.cost()
	t.LastOutcome = firstOf(th.Error, a.lastAgent)
	if th.Status == "error" {
		t.Errors = 1
	}
	t.Attention = th.Status == "error" || th.Status == "waiting" || a.open > 0
	if th.Group != "" {
		t.Attention = th.Group == "need" // a settled or snoozed failure no longer needs you
	}
	row := ThreadV3{Thread: t, DrivenID: th.ID, Kind: "driven", Status: th.Status, Group: th.Group, SpendUSD: t.CostUSD, Approvals: a.open,
		ChildCount: len(th.Children)}
	row.Phase = threadPhase(&row, nil, nil, 0)
	if th.ParentID != "" {
		row.ParentThread, row.ChildKind = th.ParentID, "delegated"
	}
	row.Repo = firstOf(th.Repo, repoBase(th.Cwd))
	row.LiveActivity = clipString(firstLine(t.LastOutcome), 140)
	hourAgo := alignUp(now) - activityLen*bucketMs
	if th.UpdatedAt >= hourAgo {
		row.Activity, row.ActivityErr = make([]int, activityLen), make([]int, activityLen)
		for _, at := range a.items {
			if i := int((at - hourAgo) / bucketMs); i >= 0 && i < activityLen {
				row.Activity[i]++
			}
		}
		for _, at := range a.itemErrs {
			if i := int((at - hourAgo) / bucketMs); i >= 0 && i < activityLen {
				row.ActivityErr[i]++
			}
		}
	}
	return row
}

// drivenRowsV3 returns every driven thread as a v3 row (nil when the orchestrator is off).
func drivenRowsV3(now int64) []ThreadV3 {
	if orchMgr == nil {
		return nil
	}
	aggs := drivenSnapshot(orchMgr.store, alignUp(now)-activityLen*bucketMs)
	out := make([]ThreadV3, 0, len(aggs))
	for _, a := range aggs {
		row := drivenRowV3(a, now)
		if a.th.NativeID != "" {
			row.Handover = "driven"
			if h := orchMgr.handover(a.th.ID); h.Terminal {
				row.Handover, row.TerminalSession = "terminal", h.SessionID
			}
		}
		out = append(out, row)
	}
	return out
}

// drivenLanes are timeline lanes for driven threads: a bar per run (error when it failed) and approval waits.
func drivenLanes(from, to, now int64) ([]TimelineLane, int64) {
	if orchMgr == nil {
		return nil, 0
	}
	var lanes []TimelineLane
	var waitTotal int64
	for _, a := range drivenSnapshot(orchMgr.store, 0) {
		th := &a.th
		lane := TimelineLane{ThreadID: th.ID, DrivenID: th.ID, Title: th.Title, Display: th.Title, Agent: th.Agent, State: drivenState(a),
			Outcome: firstOf(th.Error, a.lastAgent), Bars: []TimelineBar{}}
		if th.Status == "error" {
			lane.Errors = 1
		}
		if th.ParentID != "" {
			lane.Parent, lane.ChildKind, lane.StartTurnTs = th.ParentID, "delegated", th.CreatedAt
		}
		for _, r := range a.runs {
			end := r.End
			if end == 0 {
				end = now
			}
			if end < from || r.Start > to {
				continue
			}
			kind := "work"
			if r.Status == "failed" {
				kind = "error"
			}
			lane.Bars = append(lane.Bars, TimelineBar{From: max(r.Start, from), To: min(max(end, r.Start+1000), to), Kind: kind, Agent: th.Agent})
		}
		if waitIntervalsFn != nil && th.NativeID != "" {
			for _, iv := range waitIntervalsFn(th.Agent+":"+th.NativeID, from, to) {
				lane.Bars = append(lane.Bars, TimelineBar{From: iv[0], To: iv[1], Kind: "wait", Agent: th.Agent})
				waitTotal += iv[1] - iv[0]
			}
		}
		if len(lane.Bars) == 0 {
			continue
		}
		sort.Slice(lane.Bars, func(i, j int) bool { return lane.Bars[i].From < lane.Bars[j].From })
		lanes = append(lanes, lane)
	}
	return lanes, waitTotal
}

// drivenNowSpend is the cost of driven runs started today, for threads without a watched transcript
// (a driven Claude or Codex session is already counted from its transcript).
func drivenNowSpend(v *threadView, today int64) float64 {
	if orchMgr == nil {
		return 0
	}
	watched := map[string]bool{}
	for id := range v.byID {
		if _, native, ok := strings.Cut(id, ":"); ok {
			watched[native] = true
		}
	}
	spend := 0.0
	for _, a := range drivenSnapshot(orchMgr.store, 0) {
		if watched[a.th.NativeID] {
			continue
		}
		for _, r := range a.runs {
			if r.Start >= today {
				spend += r.Cost
			}
		}
	}
	return spend
}

// drivenSessionAction lets /api/session/send and /cancel reach a driven thread by its orchestrator id.
func drivenSessionAction(w http.ResponseWriter, id, action, text string) bool {
	if orchMgr == nil || !strings.HasPrefix(id, "ot_") {
		return false
	}
	var err error
	out := map[string]any{"ok": true, "driven": true}
	switch action {
	case "send":
		var mode string
		mode, err = orchMgr.Send(id, text)
		out["mode"] = mode
	case "cancel":
		err = orchMgr.Interrupt(id)
	}
	if err != nil {
		orchWriteErr(w, err)
		return true
	}
	writeJSON(w, out)
	return true
}
