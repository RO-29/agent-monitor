package main

import (
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	orchScheduleTick = 20 * time.Second
	// orchCatchUpLate marks a run as a catch-up when it starts this long after its time.
	orchCatchUpLate = 2 * time.Minute
)

// OSchedule runs a template on a cron schedule inside the daemon.
type OSchedule struct {
	ID           string            `json:"id"`
	Name         string            `json:"name,omitempty"`
	TemplateID   string            `json:"templateId"`
	Cwd          string            `json:"cwd"`
	Where        string            `json:"where,omitempty"` // "", "terminal" or "headless"; empty uses the template's
	Vars         map[string]string `json:"vars,omitempty"`
	Cron         string            `json:"cron"`
	Enabled      bool              `json:"enabled"`
	LastRunAt    int64             `json:"lastRunAt,omitempty"`
	LastThreadID string            `json:"lastThreadId,omitempty"`
	LastSession  string            `json:"lastSession,omitempty"`
	LastHostID   string            `json:"lastHostId,omitempty"`
	LastCatchUp  bool              `json:"lastCatchUp,omitempty"`
	LastSkipAt   int64             `json:"lastSkipAt,omitempty"`
	LastError    string            `json:"lastError,omitempty"`
	LastErrorAt  int64             `json:"lastErrorAt,omitempty"`
	NextRunAt    int64             `json:"nextRunAt"`
	NextRuns     []int64           `json:"nextRuns"`
	CreatedAt    int64             `json:"createdAt"`
	UpdatedAt    int64             `json:"updatedAt"`
}

var (
	orchScheduleMu sync.Mutex
	// orchScheduleFiring holds schedules between the busy check and the stored LastThreadID; guarded by orchScheduleMu.
	orchScheduleFiring = map[string]bool{}
)

// orchScheduleMaxWorktrees caps the live worktrees that one schedule's threads may hold.
const orchScheduleMaxWorktrees = 5

func (m *OrchManager) schedulesPath() string { return filepath.Join(m.stateDir, "schedules.json") }

func (m *OrchManager) loadSchedules() ([]OSchedule, error) {
	return readJSONList[OSchedule](m, m.schedulesPath())
}

func (m *OrchManager) saveSchedules(ss []OSchedule) error {
	for i := range ss {
		ss[i].NextRuns = nil
	}
	return writeJSONFile(m.schedulesPath(), ss)
}

// withNextRuns fills the next three run times for the API.
func withNextRuns(s OSchedule, now time.Time) OSchedule {
	s.NextRuns = []int64{}
	if c, err := parseCron(s.Cron); err == nil && s.Enabled {
		for _, t := range c.nextN(now, 3) {
			s.NextRuns = append(s.NextRuns, t.UnixMilli())
		}
	}
	return s
}

func nextRunAt(expr string, after time.Time) int64 {
	c, err := parseCron(expr)
	if err != nil {
		return 0
	}
	if t := c.next(after); !t.IsZero() {
		return t.UnixMilli()
	}
	return 0
}

// schedulesUsing lists the schedules that run a template.
func (m *OrchManager) schedulesUsing(templateID string) ([]string, error) {
	ss, err := m.loadSchedules()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, s := range ss {
		if s.TemplateID == templateID {
			out = append(out, s.ID)
		}
	}
	return out, nil
}

// checkSchedule validates a schedule body against its template; a missing variable answers 400.
func (m *OrchManager) checkSchedule(s *OSchedule) error {
	s.Name = clipString(strings.TrimSpace(s.Name), 120)
	if _, err := parseCron(s.Cron); err != nil {
		return orchBadRequest("cron: " + err.Error())
	}
	s.Cron = strings.Join(strings.Fields(s.Cron), " ")
	switch s.Where {
	case "", "terminal", "headless":
	default:
		return orchBadRequest("where must be terminal, headless or empty")
	}
	t, err := m.Template(s.TemplateID)
	if err != nil {
		return orchBadRequest("unknown templateId " + s.TemplateID)
	}
	_, err = m.templateCreate(t, orchTemplateRun{Cwd: s.Cwd, Vars: s.Vars})
	return err
}

func (m *OrchManager) SaveSchedule(id string, s OSchedule) (*OSchedule, error) {
	if err := m.checkSchedule(&s); err != nil {
		return nil, err
	}
	orchScheduleMu.Lock()
	defer orchScheduleMu.Unlock()
	ss, err := m.loadSchedules()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := OSchedule{ID: id, Name: s.Name, TemplateID: s.TemplateID, Cwd: s.Cwd, Where: s.Where, Vars: s.Vars, Cron: s.Cron, Enabled: s.Enabled, UpdatedAt: now.UnixMilli()}
	if id == "" {
		out.ID, out.CreatedAt = orchID("sch_"), now.UnixMilli()
		ss = append(ss, out)
	} else {
		i := indexSchedule(ss, id)
		if i < 0 {
			return nil, &orchHTTPError{http.StatusNotFound, "schedule not found"}
		}
		prev := ss[i]
		out.CreatedAt, out.LastRunAt, out.LastThreadID, out.LastCatchUp = prev.CreatedAt, prev.LastRunAt, prev.LastThreadID, prev.LastCatchUp
		out.LastSession, out.LastHostID = prev.LastSession, prev.LastHostID
		out.LastSkipAt, out.LastError, out.LastErrorAt = prev.LastSkipAt, prev.LastError, prev.LastErrorAt
		ss[i] = out
	}
	// A new, changed or re-enabled schedule counts from now: nothing before it is a missed run.
	out.NextRunAt = 0
	if out.Enabled {
		out.NextRunAt = nextRunAt(out.Cron, now)
	}
	ss[indexSchedule(ss, out.ID)] = out
	if err := m.saveSchedules(ss); err != nil {
		return nil, err
	}
	res := withNextRuns(out, now)
	return &res, nil
}

func indexSchedule(ss []OSchedule, id string) int {
	for i := range ss {
		if ss[i].ID == id {
			return i
		}
	}
	return -1
}

func (m *OrchManager) DeleteSchedule(id string) error {
	orchScheduleMu.Lock()
	defer orchScheduleMu.Unlock()
	ss, err := m.loadSchedules()
	if err != nil {
		return err
	}
	i := indexSchedule(ss, id)
	if i < 0 {
		return &orchHTTPError{http.StatusNotFound, "schedule not found"}
	}
	return m.saveSchedules(append(ss[:i], ss[i+1:]...))
}

func (m *OrchManager) Schedule(id string) (*OSchedule, error) {
	ss, err := m.loadSchedules()
	if err != nil {
		return nil, err
	}
	i := indexSchedule(ss, id)
	if i < 0 {
		return nil, &orchHTTPError{http.StatusNotFound, "schedule not found"}
	}
	s := withNextRuns(ss[i], time.Now())
	return &s, nil
}

// scheduleBusy reports whether the schedule's previous thread (or any of its subagents) still runs.
func (m *OrchManager) scheduleBusy(s *OSchedule) bool {
	if s.LastHostID != "" && hostRegistry != nil && hostRegistry.Get(s.LastHostID) != nil {
		return true
	}
	if s.LastThreadID == "" {
		return false
	}
	th, err := m.store.Thread(s.LastThreadID)
	if err != nil {
		return false
	}
	if activeStatus(th.Status) {
		return true
	}
	desc, _ := m.store.Subtree(th.ID)
	for _, d := range desc {
		if activeStatus(d.Status) {
			return true
		}
	}
	return false
}

// liveWorktrees counts the worktrees that a schedule's threads and their subagents still hold.
func (m *OrchManager) liveWorktrees(scheduleID string) int {
	ths, _, err := m.store.Threads()
	if err != nil {
		return 0
	}
	n := 0
	for _, th := range ths {
		if th.ScheduleID != scheduleID {
			continue
		}
		all := []OThread{th}
		if desc, err := m.store.Subtree(th.ID); err == nil {
			all = append(all, desc...)
		}
		seen := map[string]bool{}
		for _, t := range all {
			if t.Worktree != nil && !t.Worktree.Removed && !seen[t.ID] && pathExists(t.Worktree.Path) {
				seen[t.ID] = true
				n++
			}
		}
	}
	return n
}

// fireSchedule starts one run of s and records the outcome; the caller has put s.ID in orchScheduleFiring.
func (m *OrchManager) fireSchedule(s OSchedule, catchUp bool, now time.Time) (*orchLaunched, error) {
	l, err := func() (*orchLaunched, error) {
		t, err := m.Template(s.TemplateID)
		if err != nil {
			return nil, err
		}
		in, err := m.templateCreate(t, orchTemplateRun{Cwd: s.Cwd, Vars: s.Vars})
		if err != nil {
			return nil, err
		}
		if n := m.liveWorktrees(s.ID); n >= orchScheduleMaxWorktrees {
			return nil, orchConflict(fmt.Sprintf("schedule %s already holds %d worktrees; remove some before it runs again", firstOf(s.Name, s.ID), n))
		}
		in.ScheduleID, in.CatchUp = s.ID, catchUp
		if s.Where != "" {
			in.Where = s.Where
		}
		return m.launch(in)
	}()
	orchScheduleMu.Lock()
	delete(orchScheduleFiring, s.ID)
	ss, lerr := m.loadSchedules()
	if lerr != nil {
		log.Printf("orch: schedules: %v", lerr)
	} else if i := indexSchedule(ss, s.ID); i >= 0 {
		if err != nil {
			ss[i].LastError, ss[i].LastErrorAt = err.Error(), now.UnixMilli()
		} else {
			ss[i].LastRunAt, ss[i].LastCatchUp, ss[i].LastError, ss[i].LastErrorAt = now.UnixMilli(), catchUp, "", 0
			ss[i].LastThreadID, ss[i].LastSession, ss[i].LastHostID = "", "", ""
			if l.Thread != nil {
				ss[i].LastThreadID = l.Thread.ID
			} else if l.Terminal != nil {
				ss[i].LastSession, ss[i].LastHostID = l.Terminal.Session, l.Terminal.HostID
			}
		}
		if serr := m.saveSchedules(ss); serr != nil {
			log.Printf("orch: schedules: %v", serr)
		}
	}
	orchScheduleMu.Unlock()
	if err != nil {
		m.notify(notifyEvent{Kind: "error", Title: "Schedule " + firstOf(s.Name, s.ID) + " did not start", Body: clipString(err.Error(), 300)})
	}
	return l, err
}

// scheduleTick fires every enabled schedule that is due: at most one run each, however many times it was missed.
func (m *OrchManager) scheduleTick(now time.Time) {
	type due struct {
		s       OSchedule
		catchUp bool
	}
	var fire []due
	orchScheduleMu.Lock()
	ss, err := m.loadSchedules()
	if err != nil {
		orchScheduleMu.Unlock()
		log.Printf("orch: schedules: %v", err)
		return
	}
	changed := false
	for i := range ss {
		s := &ss[i]
		if !s.Enabled {
			continue
		}
		if s.NextRunAt == 0 {
			if n := nextRunAt(s.Cron, now); n != 0 {
				s.NextRunAt, changed = n, true
			}
			continue
		}
		if s.NextRunAt > now.UnixMilli() {
			continue
		}
		late := now.Sub(time.UnixMilli(s.NextRunAt)) > orchCatchUpLate
		s.NextRunAt, changed = nextRunAt(s.Cron, now), true
		if orchScheduleFiring[s.ID] || m.scheduleBusy(s) {
			s.LastSkipAt = now.UnixMilli()
			s.LastError, s.LastErrorAt = "skipped: the previous run "+firstOf(s.LastThreadID, s.LastSession)+" still runs", now.UnixMilli()
			continue
		}
		orchScheduleFiring[s.ID] = true
		fire = append(fire, due{*s, late})
	}
	if changed {
		if err := m.saveSchedules(ss); err != nil {
			log.Printf("orch: schedules: %v", err)
		}
	}
	orchScheduleMu.Unlock()
	for _, d := range fire {
		if _, err := m.fireSchedule(d.s, d.catchUp, now); err != nil {
			log.Printf("orch: schedule %s: %v", d.s.ID, err)
		}
	}
}

// RunScheduleNow starts the schedule at once; 409 while its previous thread runs.
func (m *OrchManager) RunScheduleNow(id string) (*OThread, *OSchedule, error) {
	orchScheduleMu.Lock()
	s, err := m.Schedule(id)
	if err != nil {
		orchScheduleMu.Unlock()
		return nil, nil, err
	}
	if orchScheduleFiring[id] {
		orchScheduleMu.Unlock()
		return nil, nil, orchConflict("the schedule is starting a thread now")
	}
	if m.scheduleBusy(s) {
		orchScheduleMu.Unlock()
		return nil, nil, orchConflict("the previous run " + firstOf(s.LastThreadID, s.LastSession) + " still runs")
	}
	orchScheduleFiring[id] = true
	orchScheduleMu.Unlock()
	l, err := m.fireSchedule(*s, false, time.Now())
	if err != nil {
		return nil, nil, err
	}
	s, _ = m.Schedule(id)
	return l.Thread, s, nil
}

// scheduleLoop runs inside the daemon; there is no LaunchAgent and no cron entry.
func (m *OrchManager) scheduleLoop() {
	m.scheduleTick(time.Now())
	for range time.Tick(orchScheduleTick) {
		m.scheduleTick(time.Now())
	}
}

func (m *OrchManager) scheduleRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/orch/schedules", func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		out := []OSchedule{}
		ss, err := m.loadSchedules()
		if err != nil {
			orchWriteErr(w, err)
			return
		}
		for _, s := range ss {
			out = append(out, withNextRuns(s, now))
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("GET /api/orch/schedules/{id}", func(w http.ResponseWriter, r *http.Request) {
		s, err := m.Schedule(r.PathValue("id"))
		if err != nil {
			orchWriteErr(w, err)
			return
		}
		writeJSON(w, s)
	})
	save := func(w http.ResponseWriter, r *http.Request, id string) {
		var in OSchedule
		if !decodeOptional(w, r, &in) {
			return
		}
		s, err := m.SaveSchedule(id, in)
		if err != nil {
			templateWriteErr(w, err)
			return
		}
		status := http.StatusOK
		if id == "" {
			status = http.StatusCreated
		}
		writeJSONStatus(w, status, map[string]any{"schedule": s})
	}
	mux.HandleFunc("POST /api/orch/schedules", func(w http.ResponseWriter, r *http.Request) { save(w, r, "") })
	mux.HandleFunc("PUT /api/orch/schedules/{id}", func(w http.ResponseWriter, r *http.Request) { save(w, r, r.PathValue("id")) })
	mux.HandleFunc("DELETE /api/orch/schedules/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := m.DeleteSchedule(r.PathValue("id")); err != nil {
			orchWriteErr(w, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/orch/schedules/{id}/run-now", func(w http.ResponseWriter, r *http.Request) {
		th, s, err := m.RunScheduleNow(r.PathValue("id"))
		if err != nil {
			templateWriteErr(w, err)
			return
		}
		if th == nil && s != nil && s.LastSession != "" {
			writeJSONStatus(w, http.StatusCreated, map[string]any{"terminal": terminalStarted{Session: s.LastSession, HostID: s.LastHostID, Attach: "tmux attach -t " + s.LastSession}, "schedule": s})
			return
		}
		writeJSONStatus(w, http.StatusCreated, map[string]any{"thread": th, "schedule": s})
	})
	mux.HandleFunc("GET /api/orch/cron", func(w http.ResponseWriter, r *http.Request) {
		c, err := parseCron(r.URL.Query().Get("expr"))
		if err != nil {
			writeJSON(w, map[string]any{"valid": false, "error": err.Error(), "next": []int64{}})
			return
		}
		next := []int64{}
		for _, t := range c.nextN(time.Now(), 5) {
			next = append(next, t.UnixMilli())
		}
		writeJSON(w, map[string]any{"valid": true, "next": next})
	})
}
