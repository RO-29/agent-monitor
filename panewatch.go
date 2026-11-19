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
var reMenuCursor = regexp.MustCompile(`^\s*❯\s*[1-9]\.`)

// panePromptOpen: a menu-like screen is a prompt unless Claude's input box (❯, ! or # mode) is drawn below the menu text.
func panePromptOpen(screen string) bool {
	tail := lastLines(screen, 24)
	if !rePanePrompt.MatchString(tail) {
		return false
	}
	lines := strings.Split(strings.TrimRight(tail, "\n"), "\n")
	last, rules := -1, []int{}
	for i, ln := range lines {
		if rePanePrompt.MatchString(ln) {
			last = i
		}
		if isBoxRule(strings.TrimSpace(ln)) {
			rules = append(rules, i)
		}
	}
	for k := 0; k+1 < len(rules); k++ {
		a, b := rules[k], rules[k+1]
		box := strings.TrimSpace(strings.Join(lines[a+1:b], "\n"))
		if a > last && strings.ContainsAny(firstRune(box), "❯!#") && !reMenuCursor.MatchString(box) {
			return false
		}
	}
	return true
}

func firstRune(s string) string {
	for _, r := range s {
		return string(r)
	}
	return ""
}

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
			wts := promptWatchTargets(pr)
			screens := captureWatchTargets(wts)
			for _, wt := range wts {
				sess := s.Get(wt.agentID)
				if sess == nil {
					continue
				}
				content, ok := screens[wt.target]
				if !ok {
					continue // pane vanished / tmux busy — leave state as-is
				}
				applyPaneScreen(s, sess, content)
			}
		}
	}()
}

// applyPaneScreen keeps the "prompt open" state in step with the screen.
// A finished turn (awaiting-input) has no prompt on screen and is left alone.
func applyPaneScreen(s *Store, sess *Session, content string) {
	atPrompt := panePromptOpen(content)
	switch {
	case atPrompt && sess.State != StateAwaitingPermission && parsedPromptOpen(content):
		s.SetState(sess.Tool, sess.SessionID, StateAwaitingPermission, promptSummary(lastLines(content, 24)))
	case !atPrompt && sess.State == StateAwaitingPermission:
		s.SetState(sess.Tool, sess.SessionID, StateRunning, "") // the prompt was answered
	}
}

// parsedPromptOpen is true only for a prompt agent-monitor can read: the broad pattern also matches output such as "(y/n)".
func parsedPromptOpen(screen string) bool {
	return claudePermOptions(screen) != nil || codexPermOptions(screen) != nil ||
		strings.Contains(screen, "Enter to select") || len(planOptionsOnScreen(screen)) > 0
}

type promptWatchTarget struct{ agentID, target string }

var capturePanesFn = CapturePanes

// captureWatchTargets reads every watched screen: all tmux panes with one process, hosts by their own route.
// A failed batch falls back to one capture per pane, so one dead pane does not hide the others.
func captureWatchTargets(wts []promptWatchTarget) map[string]string {
	out := map[string]string{}
	var panes []string
	for _, wt := range wts {
		if _, isHost, _ := targetHost(wt.target); isHost {
			if c, err := targetCapture(wt.target, 24, true); err == nil {
				out[wt.target] = c
			}
		} else {
			panes = append(panes, wt.target)
		}
	}
	// A pane that failed lately stays out of the one-call capture, so one dead pane does not fail every tick.
	var batch, alone []string
	now := time.Now()
	for _, p := range panes {
		if t, bad := badPanes[p]; bad && now.Sub(t) < 30*time.Second {
			alone = append(alone, p)
		} else {
			batch = append(batch, p)
		}
	}
	if m, err := capturePanesFn(batch, 24); err == nil {
		for k, v := range m {
			out[k] = v
		}
	} else {
		alone = append(alone, batch...)
	}
	for _, p := range alone {
		if c, err := targetCapture(p, 24, true); err == nil {
			out[p] = c
			delete(badPanes, p)
		} else {
			badPanes[p] = now
		}
	}
	return out
}

// badPanes holds panes whose capture failed lately; only the watcher goroutine uses it.
var badPanes = map[string]time.Time{}

// promptWatchTargets lists every session with a terminal: hosts first, then tmux panes without a host.
func promptWatchTargets(pr *PaneRegistry) []promptWatchTarget {
	var out []promptWatchTarget
	seen := map[string]bool{}
	if hostRegistry != nil {
		for _, h := range hostRegistry.List() {
			if h.SessionID == "" {
				continue
			}
			id := sid(h.Tool, h.SessionID)
			seen[h.SessionID] = true
			out = append(out, promptWatchTarget{agentID: id, target: hostTargetPrefix + h.HostID})
		}
	}
	for _, reg := range pr.List() {
		if seen[reg.SessionID] {
			continue
		}
		out = append(out, promptWatchTarget{agentID: reg.AgentID, target: reg.PaneID})
	}
	return out
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
