package main

import (
	"time"
)

// handoverGrace absorbs hook and transcript writes that land just after a driven turn or process ends.
const handoverGrace int64 = 15_000

// orchHandover says who owns a driven thread's native session now: the driven thread or a terminal.
type orchHandover struct {
	SessionID string // store id, agent:native
	Terminal  bool   // the terminal used the session after the driven thread last did
	Live      bool   // a terminal process holds the session now
}

func (m *OrchManager) noteProcEnd(id string) {
	m.mu.Lock()
	m.procEnd[id] = time.Now().UnixMilli()
	m.mu.Unlock()
}

func (m *OrchManager) procAlive(id string) bool {
	m.mu.Lock()
	d := m.drivers[id]
	m.mu.Unlock()
	return d != nil && d.procAlive()
}

// handover applies the rule to thread id; a missing thread or one without a native session stays driven.
func (m *OrchManager) handover(id string) orchHandover {
	th, err := m.store.Thread(id)
	if err != nil || th.NativeID == "" {
		return orchHandover{}
	}
	h := orchHandover{SessionID: th.Agent + ":" + th.NativeID}
	if m.procAlive(id) {
		return h
	}
	mark := max(th.CreatedAt, m.store.LastRunAt(id))
	m.mu.Lock()
	mark = max(mark, m.procEnd[id])
	m.mu.Unlock()
	var s *Session
	if store != nil {
		s = store.Get(h.SessionID)
	}
	var reg *PaneRegistration
	if paneRegistry != nil {
		reg = paneRegistry.Get(h.SessionID)
	}
	h.Terminal, h.Live = terminalUse(mark, s, reg)
	return h
}

// terminalUse: activity on the session after mark (the driven thread's last touch) came from a terminal.
// Live is a live pid, a pane registered after mark, or an active state with activity after mark.
func terminalUse(mark int64, s *Session, reg *PaneRegistration) (after, live bool) {
	if reg != nil && reg.PaneID != "" && reg.RegisteredAt > mark && (reg.Pid == 0 || pidAlive(reg.Pid)) {
		live = true
	}
	if s != nil {
		after = s.LastActivityAt > mark+handoverGrace
		if s.Pid > 0 && pidAlive(s.Pid) {
			live = true
		}
		switch s.State {
		case StateRunning, StateAwaitingPermission, StateAwaitingInput:
			live = live || after
		}
	}
	return after || live, live
}
