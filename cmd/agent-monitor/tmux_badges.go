package main

import (
	"log"
	"os"
	"os/exec"
	"time"
)

// The window option @am_badge shows the state of the agents in a tmux window: "● " waits for an approval,
// "◐ " works, "✓ " ended its turn.

const (
	badgeNone = iota
	badgeYourTurn
	badgeWorking
	badgeNeedsYou
)

var badgeText = map[int]string{badgeYourTurn: "✓ ", badgeWorking: "◐ ", badgeNeedsYou: "● "}

func badgeRank(s State) int {
	switch s {
	case StateAwaitingPermission:
		return badgeNeedsYou
	case StateAwaitingInput:
		return badgeYourTurn
	case StateRunning:
		return badgeWorking
	}
	return badgeNone
}

// windowBadges maps each tmux window to the highest rank of the sessions in its panes.
func windowBadges(paneState map[string]State, panes []TmuxPane) map[string]int {
	out := map[string]int{}
	for _, p := range panes {
		if s, ok := paneState[p.PaneID]; ok && badgeRank(s) > out[p.WindowID] {
			out[p.WindowID] = badgeRank(s)
		}
	}
	return out
}

type tmuxBadger struct {
	shown map[string]int // window id -> rank set last time; only these windows are ever changed
	set   func(window, text string) error
}

func newTmuxBadger() *tmuxBadger {
	return &tmuxBadger{shown: map[string]int{}, set: func(window, text string) error {
		if text == "" {
			return exec.Command("tmux", "set-option", "-w", "-u", "-t", window, "@am_badge").Run()
		}
		return exec.Command("tmux", "set-option", "-w", "-t", window, "@am_badge", text).Run()
	}}
}

// apply sets the windows whose rank changed and clears the ones that no longer hold a busy agent.
func (b *tmuxBadger) apply(want map[string]int) {
	for w, r := range want {
		if r == badgeNone || b.shown[w] == r {
			continue
		}
		if b.set(w, badgeText[r]) == nil {
			b.shown[w] = r
		}
	}
	for w := range b.shown {
		if want[w] == badgeNone {
			_ = b.set(w, "") // a closed window fails here, which is fine
			delete(b.shown, w)
		}
	}
}

// paneStates reads the state of every session that has a tmux pane.
func paneStates() map[string]State {
	out := map[string]State{}
	if store == nil {
		return out
	}
	add := func(pane, agentID string) {
		if pane == "" || agentID == "" {
			return
		}
		if s := store.Get(agentID); s != nil && badgeRank(s.State) > badgeRank(out[pane]) {
			out[pane] = s.State
		}
	}
	if paneRegistry != nil {
		for _, r := range paneRegistry.List() {
			add(r.PaneID, r.AgentID)
		}
	}
	if hostRegistry != nil {
		for _, h := range hostRegistry.List() {
			add(h.TmuxPane, h.AgentID)
		}
	}
	return out
}

func startTmuxBadges() {
	if os.Getenv("AGENT_MONITOR_NO_TMUX_BADGES") == "1" {
		return
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		return
	}
	b := newTmuxBadger()
	go func() {
		failed := false
		for range time.Tick(3 * time.Second) {
			panes, err := listTmuxPanes()
			if err != nil {
				if !failed {
					log.Printf("tmux badges: %v", err)
				}
				failed = true
				continue
			}
			failed = false
			b.apply(windowBadges(paneStates(), panes))
		}
	}()
}
