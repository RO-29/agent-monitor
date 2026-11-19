package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// drivenViewEnv is an orchestrator env installed as the global orchMgr, with a fresh watched store.
func drivenViewEnv(t *testing.T) *orchEnv {
	t.Helper()
	v4Fixture(t, 4)
	e := newOrchEnv(t)
	old := orchMgr
	orchMgr = e.m
	t.Cleanup(func() { orchMgr = old })
	return e
}

func TestDrivenRowsInThreadsResponse(t *testing.T) {
	e := drivenViewEnv(t)
	parent := e.create("hello")
	e.idle(parent.ID)
	kid := e.delegate(parent.ID, orchChildSpec{Title: "Review the parser", Prompt: "hello", Agent: "codex"})
	e.until("notify run", func() bool { return notifyRuns(e.runs(parent.ID)) == 1 })
	e.idle(parent.ID)
	e.idle(kid.ID)

	raw, _ := json.Marshal(threadsResponse(true, false))
	var got struct {
		Threads []map[string]any `json:"threads"`
	}
	_ = json.Unmarshal(raw, &got)
	rows := map[string]map[string]any{}
	watched := 0
	for _, th := range got.Threads {
		if th["kind"] == "driven" {
			rows[th["id"].(string)] = th
		} else {
			watched++
		}
	}
	if watched != 4 {
		t.Fatalf("watched rows %d", watched)
	}
	p := rows[parent.ID]
	if p == nil || p["drivenId"] != parent.ID || p["status"] != "idle" || p["state"] != "completed" || p["lastOutcome"] != "pong" ||
		p["turns"].(float64) != 2 || p["tools"].([]any)[0] != "codex" || p["display"] == "" || p["childCount"].(float64) != 1 {
		t.Fatalf("parent row %v", p)
	}
	k := rows[kid.ID]
	if k == nil || k["parentThread"] != parent.ID || k["childKind"] != "delegated" {
		t.Fatalf("child row %v", k)
	}
	if a, ok := p["activity"].([]any); !ok || len(a) != activityLen {
		t.Fatalf("activity %v", p["activity"])
	}
	for _, th := range threadsResponse(false, false)["threads"].([]ThreadV3) {
		if th.ID == kid.ID {
			t.Fatal("driven child in the top-level list")
		}
	}
	// Watched sessions that a driven thread owns still leave the watched list.
	native := e.thread(parent.ID).NativeID
	store.sessions["codex:"+native] = &Session{ID: "codex:" + native, Tool: ToolCodex, SessionID: native, Cwd: "/x", StartedAt: 1, LastActivityAt: 2}
	for _, th := range threadsResponse(true, false)["threads"].([]ThreadV3) {
		if th.ID == "codex:"+native {
			t.Fatal("driven copy listed")
		}
	}
}

func TestDrivenRowsInNowAndTimeline(t *testing.T) {
	e := drivenViewEnv(t)
	th := e.create("hang")
	e.hanging(th.ID)
	now := time.Now().UnixMilli()
	n := buildNow(now)
	var lane *NowLane
	for i := range n.Lanes {
		if n.Lanes[i].ThreadID == th.ID {
			lane = &n.Lanes[i]
		}
	}
	if lane == nil || lane.DrivenID != th.ID || lane.Agent != "codex" || len(lane.Bars) == 0 || lane.Bars[0].Kind != "work" {
		t.Fatalf("now lane %+v", lane)
	}
	if n.Running < 1 {
		t.Fatalf("running %d", n.Running)
	}
	tl := buildTimeline(now-3600_000, now+60_000, true)["lanes"].([]TimelineLane)
	found := false
	for _, l := range tl {
		if l.ThreadID == th.ID {
			found = l.DrivenID == th.ID && l.State == StateRunning && len(l.Bars) > 0
		}
	}
	if !found {
		t.Fatalf("no driven timeline lane in %+v", tl)
	}
	_ = e.m.Interrupt(th.ID)
}

func TestDrivenSessionSendAndCancel(t *testing.T) {
	e := drivenViewEnv(t)
	th := e.create("hello")
	e.idle(th.ID)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/send/")
		if !drivenSessionAction(w, id, "send", "hang") {
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	code, out := postJSON(t, srv.URL+"/send/"+th.ID, "{}")
	if code != 200 || out["mode"] != "turn" || out["driven"] != true {
		t.Fatalf("send %d %v", code, out)
	}
	e.hanging(th.ID)
	if code, _ := postJSON(t, srv.URL+"/send/claude:abc", "{}"); code != 404 {
		t.Fatalf("watched id must fall through: %d", code)
	}
	rec := httptest.NewRecorder()
	if !drivenSessionAction(rec, th.ID, "cancel", "") || rec.Code != 200 {
		t.Fatalf("cancel %d %s", rec.Code, rec.Body)
	}
	e.idle(th.ID)
}

func TestSearchIndexesDrivenItems(t *testing.T) {
	e := newOrchEnv(t)
	old := orchMgr
	orchMgr = e.m
	defer func() { orchMgr = old }()
	ix, _ := newTestIndex(t)
	th := e.create("find the zebrafish bug")
	e.idle(th.ID)
	n, err := ix.indexDriven(e.store)
	if err != nil || n == 0 {
		t.Fatalf("first pass %d %v", n, err)
	}
	if again, _ := ix.indexDriven(e.store); again != 0 {
		t.Fatalf("second pass re-indexed %d items", again)
	}
	pq := parseSearchQuery("zebrafish", nil)
	res, err := ix.search(pq, "", "", 20, 0, time.Now())
	if err != nil || len(res.Hits) != 1 || res.Hits[0].Kind != "prompt" || res.Hits[0].DrivenID != th.ID || res.Hits[0].ThreadID != th.ID || res.Hits[0].Agent != "codex" {
		t.Fatalf("hits %+v %v", res, err)
	}
	res, _ = ix.search(parseSearchQuery("a.go", nil), "", "file", 20, 0, time.Now())
	if len(res.Hits) == 0 || res.Hits[0].Session != th.ID {
		t.Fatalf("file hit %+v", res.Hits)
	}
	if _, err := e.m.Send(th.ID, "and the narwhal one"); err != nil {
		t.Fatal(err)
	}
	e.until("second run", func() bool { r := e.runs(th.ID); return len(r) == 2 && r[1].Status == "completed" })
	if n, _ := ix.indexDriven(e.store); n == 0 {
		t.Fatal("incremental pass found nothing")
	}
	res, _ = ix.search(parseSearchQuery("narwhal", nil), "", "", 20, 0, time.Now())
	if len(res.Hits) != 1 {
		t.Fatalf("narwhal hits %+v", res.Hits)
	}
	// A fresh index (seq 0) rescans every finished item without duplicates.
	_, _ = ix.w.Exec(`UPDATE search_meta SET v=0 WHERE k='orch_seq'`)
	_, _ = ix.indexDriven(e.store)
	res, _ = ix.search(parseSearchQuery("zebrafish", nil), "", "", 20, 0, time.Now())
	if len(res.Hits) != 1 {
		t.Fatalf("rescan duplicated docs: %d hits", len(res.Hits))
	}
}
