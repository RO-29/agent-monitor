package main

import (
	"log"
	"os"
)

// orchResumeText goes to each driven thread whose turn the last daemon stop ended.
const orchResumeText = "agent-monitor restarted and ended your last turn while it ran. " +
	"Continue the task from where you stopped. If the task was complete, say so in one line."

// ContinueInterrupted sends one continue message to each thread whose turn the last daemon stop ended.
// AGENT_MONITOR_NO_RESUME=1 turns this off.
func (m *OrchManager) ContinueInterrupted() []string {
	if os.Getenv("AGENT_MONITOR_NO_RESUME") == "1" {
		return nil
	}
	m.store.mu.Lock()
	ids := m.store.interrupted
	m.store.interrupted = nil
	m.store.mu.Unlock()
	var sent []string
	for _, id := range ids {
		th, err := m.store.Thread(id)
		if err != nil || th.Status == "closed" {
			continue
		}
		if _, err := m.Send(id, orchResumeText); err != nil {
			log.Printf("orch: continue %s after restart: %v", id, err)
			continue
		}
		sent = append(sent, id)
	}
	return sent
}
