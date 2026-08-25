package main

import (
	"regexp"
	"strings"
	"time"
)

// Claude shows decision prompts in its TUI — AskUserQuestion menus and
// tool-permission prompts — that need a human answer. These arrive as tool
// calls, so the PreToolUse hook marks the session "running" and the Notification
// hook (which sets awaiting-*) never fires for them. So they'd never reach the
// Approvals tab. We instead detect the prompt straight from the pane content and
// flip the session to awaiting-input while it's showing, so it surfaces.

// Strong signals that a pane is parked on a decision prompt (not just idle at
// the "❯ " input box or busy with a spinner).
var rePanePrompt = regexp.MustCompile(`(?i)(enter to select|enter to confirm|↑/↓ to navigate|to navigate|` +
	`do you want to proceed|do you want to create|❯\s*[1-9]\.\s|(?:^|\n)\s*[1-9]\.\s+(yes|no|allow|don't|do not)|` +
	// Codex approval prompts (interactive; not logged as JSONL events):
	`allow codex|allow command|allow this|approve (command|this|the|patch)|apply patch\?|run (this )?command\?|\[y/n\]|\(y/n\))`)

// startPanePromptWatcher polls registered panes and keeps a session's awaiting
// state in sync with what its pane actually shows. For a pane we can see, the
// pane IS the source of truth: a decision prompt on screen ⇒ awaiting, none ⇒
// not awaiting. It's edge-triggered (writes only when the awaiting-ness flips),
// and stateless — so a stale awaiting always clears once its prompt is gone,
// even across a daemon restart (the previous owned-set approach could leave a
// prompt stuck awaiting forever after a restart).
func startPanePromptWatcher(s *Store, pr *PaneRegistry) {
	go func() {
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		for range t.C {
			for _, reg := range pr.List() {
				sess := s.Get(reg.AgentID)
				if sess == nil {
					continue
				}
				content, err := CapturePane(reg.PaneID, 24, true)
				if err != nil {
					continue // pane vanished / tmux busy — leave state as-is
				}
				atPrompt := rePanePrompt.MatchString(lastLines(content, 24))
				isAwaiting := sess.State == StateAwaitingPermission || sess.State == StateAwaitingInput
				switch {
				case atPrompt && !isAwaiting:
					s.SetState(reg.Tool, reg.SessionID, StateAwaitingInput, promptSummary(lastLines(content, 24)))
				case !atPrompt && isAwaiting:
					// Prompt answered / gone → clear the stale approval.
					s.SetState(reg.Tool, reg.SessionID, StateRunning, "")
				}
			}
		}
	}()
}

// lastLines returns the final n non-empty-trimmed lines of s.
func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	end := len(lines)
	for end > 0 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	start := end - n
	if start < 0 {
		start = 0
	}
	return strings.Join(lines[start:end], "\n")
}

// promptSummary picks a one-line gist for the session message — the question
// line (ends in "?") if present, else the first offered option.
func promptSummary(tail string) string {
	for _, ln := range strings.Split(tail, "\n") {
		t := strings.TrimSpace(ln)
		if strings.HasSuffix(t, "?") && len(t) > 3 {
			return clip(t, 160)
		}
	}
	return "Claude is waiting for your answer"
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
