package main

import (
	"fmt"
	"strings"
)

// A side question goes to a read-only child agent with the recent conversation as context,
// so the thread's own agent keeps working and never sees the question.

const sideTurns = 6 // user and agent messages carried as context

func sideCommand(text string) (string, bool) {
	t := strings.TrimSpace(text)
	if q, ok := strings.CutPrefix(t, "/side "); ok && strings.TrimSpace(q) != "" {
		return strings.TrimSpace(q), true
	}
	return "", false
}

// Side starts a read-only child that answers question about thread id.
func (m *OrchManager) Side(id, question string) (*OThread, error) {
	d, err := m.store.ThreadDetail(id)
	if err != nil {
		return nil, err
	}
	var turns []string
	for i := len(d.Items) - 1; i >= 0 && len(turns) < sideTurns; i-- {
		it := d.Items[i]
		if (it.Kind != "user" && it.Kind != "agent") || strings.TrimSpace(it.Text) == "" {
			continue
		}
		who := "Agent"
		if it.Kind == "user" {
			who = "User"
		}
		turns = append(turns, fmt.Sprintf("%s: %s", who, clipString(strings.TrimSpace(it.Text), 1500)))
	}
	for i, j := 0, len(turns)-1; i < j; i, j = i+1, j-1 {
		turns[i], turns[j] = turns[j], turns[i]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Answer a side question about another agent's work in %s. ", d.Thread.Cwd)
	b.WriteString("That agent keeps working and does not see this. Read files if you need to, change nothing, and answer briefly.\n\n")
	fmt.Fprintf(&b, "Its task: %s\n\n", d.Thread.Title)
	if len(turns) > 0 {
		b.WriteString("Its latest messages:\n" + strings.Join(turns, "\n\n") + "\n\n")
	}
	b.WriteString("Side question: " + question)
	return m.Delegate(id, orchChildSpec{
		Title:        orchTitle("Side: " + question),
		Prompt:       b.String(),
		Agent:        d.Thread.Agent,
		Model:        d.Thread.Model,
		Workspace:    "read-only",
		NotifyParent: boolPtr(false),
	})
}
