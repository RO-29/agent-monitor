package main

import (
	"errors"
	"log"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// TmuxPane is a snapshot of one tmux pane, useful for matching to an
// agent-monitor Session. We pull a stable set of fields so the matcher can
// score by pid (most reliable) or cwd (fallback).
type TmuxPane struct {
	PaneID         string // %42
	SessionName    string
	WindowID       string
	PanePID        int
	CurrentCommand string
	CurrentPath    string
	StartCommand   string
}

// listTmuxPanes shells out to `tmux list-panes -a` with a custom format.
// Returns nil + error if tmux isn't running. Cheap (~5ms) so we can call this
// on every send rather than maintain stale state. We capture stderr too so
// callers can show "no server running on ..." instead of just "exit status 1".
func listTmuxPanes() ([]TmuxPane, error) {
	cmd := exec.Command("tmux", "list-panes", "-a", "-F",
		"#{pane_id}\t#{session_name}\t#{window_id}\t#{pane_pid}\t#{pane_current_command}\t#{pane_current_path}\t#{pane_start_command}")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return nil, errors.New(strings.TrimSpace(string(ee.Stderr)))
		}
		// "exec: tmux: not found" — tmux isn't on PATH at all.
		return nil, err
	}
	var panes []TmuxPane
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 6 {
			continue
		}
		pid, _ := strconv.Atoi(f[3])
		p := TmuxPane{
			PaneID: f[0], SessionName: f[1], WindowID: f[2],
			PanePID: pid, CurrentCommand: f[4], CurrentPath: f[5],
		}
		if len(f) > 6 {
			p.StartCommand = f[6]
		}
		panes = append(panes, p)
	}
	return panes, nil
}

// HEURISTIC MATCHER REMOVED. Pane↔session links now come from
// pane_registry.go, populated by the SessionStart hook (Claude) or the
// `agent-monitor run` wrapper (everything else). The functions below are
// kept ONLY because some callers still pass through them — they're dead in
// the registry-only world but cheap to leave for now in case we want them
// for diagnostics.
func procTree() map[int][]int {
	out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=").Output()
	if err != nil {
		return nil
	}
	tree := make(map[int][]int)
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		tree[ppid] = append(tree[ppid], pid)
	}
	return tree
}

// isDescendant returns true if target is root or any of its descendants.
// Bounded depth to defend against pathological process trees / cycles.
func isDescendant(tree map[int][]int, root, target int) bool {
	if root == target {
		return true
	}
	stack := []int{root}
	visited := map[int]bool{root: true}
	for depth := 0; len(stack) > 0 && depth < 1024; depth++ {
		next := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, c := range tree[next] {
			if c == target {
				return true
			}
			if !visited[c] {
				visited[c] = true
				stack = append(stack, c)
			}
		}
	}
	return false
}

// PaneCandidate is a scored pane with the reasons it was picked. We surface
// the full candidate list (not just the winner) so the UI can offer a manual
// override when matching is ambiguous.
type PaneCandidate struct {
	Pane    TmuxPane `json:"pane"`
	Score   int      `json:"score"`
	Reasons []string `json:"reasons"`
}

// scoreTmuxPanesForSession returns every pane that scored above zero, sorted
// best-first, with the matched signals. The auto-picker (findTmuxPaneForSession)
// uses this internally; the API also returns it so the UI can show the user
// what we considered and let them override.
func scoreTmuxPanesForSession(panes []TmuxPane, sess *Session) []PaneCandidate {
	if sess == nil {
		return nil
	}
	tree := procTree()
	var out []PaneCandidate
	for i := range panes {
		p := panes[i]
		score := 0
		var reasons []string
		if sess.Pid > 0 && p.PanePID > 0 {
			if p.PanePID == sess.Pid {
				score += 100
				reasons = append(reasons, "pid-exact")
			} else if tree != nil && isDescendant(tree, p.PanePID, sess.Pid) {
				score += 100
				reasons = append(reasons, "pid-descendant")
			}
		}
		// CWD scoring. cwd-exact (+10) outweighs cwd-prefix (+5) — that's
		// what disambiguates a session running at $HOME from one running in a
		// subdirectory: the home session's exact match beats the subdir
		// session's prefix match against the same pane.
		if sess.Cwd != "" {
			if p.CurrentPath == sess.Cwd {
				score += 10
				reasons = append(reasons, "cwd-exact")
			} else if strings.HasPrefix(sess.Cwd, p.CurrentPath+"/") {
				score += 5
				reasons = append(reasons, "cwd-prefix")
			}
		}
		cmd := strings.ToLower(p.CurrentCommand)
		switch sess.Tool {
		case ToolClaude:
			if cmd == "claude" || strings.Contains(cmd, "claude") {
				score += 20
				reasons = append(reasons, "cmd-claude")
			} else if cmd == "node" {
				score += 3
				reasons = append(reasons, "cmd-node")
			}
		case ToolCodex:
			if strings.Contains(cmd, "codex") {
				score += 20
				reasons = append(reasons, "cmd-codex")
			} else if cmd == "node" {
				score += 3
				reasons = append(reasons, "cmd-node")
			}
		case ToolCursorAgent:
			if strings.Contains(cmd, "cursor-agent") {
				score += 20
				reasons = append(reasons, "cmd-cursor-agent")
			}
		}
		if score > 0 {
			out = append(out, PaneCandidate{Pane: p, Score: score, Reasons: reasons})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

// findTmuxPaneForSession scores every pane and returns the best match — but
// only if it's unambiguous. Ambiguous (tied) matches return nil so the caller
// can show "no pane matched" rather than guess wrong and route input into a
// neighbor's pane (which is what the user would see otherwise).
//
// Scoring:
//
//	+100  pane_pid IS sess.Pid, or sess.Pid is a process-tree descendant
//	 +20  tool name matches (claude/codex/cursor-agent in pane command)
//	 +10  cwd is identical
//	  +5  cwd is a prefix (sess is a subdirectory of pane cwd)
//	  +3  pane command is "node" (claude-code embeds node) — weakest signal
func findTmuxPaneForSession(panes []TmuxPane, sess *Session) *TmuxPane {
	candidates := scoreTmuxPanesForSession(panes, sess)
	if len(candidates) == 0 {
		return nil
	}
	const minConfidence = 25 // pid-descent=100; tool+cwd=30; below this, refuse.
	best := candidates[0]
	if best.Score < minConfidence {
		return nil
	}
	// Refuse ties at the top score — better to ask the user than to silently
	// route their prompt into a neighbor's session.
	if len(candidates) > 1 && candidates[1].Score == best.Score {
		return nil
	}
	p := best.Pane
	return &p
}

// findTmuxPaneByID returns the pane with matching id (e.g. "%3") from the
// list, or nil. Used for manual overrides — the UI lets the user pick a pane
// id when the auto-matcher is wrong, and we look it up here before sending.
func findTmuxPaneByID(panes []TmuxPane, paneID string) *TmuxPane {
	for i := range panes {
		if panes[i].PaneID == paneID {
			return &panes[i]
		}
	}
	return nil
}


// SendToTmuxPane writes text into the pane and presses Enter. Uses `-l` for
// literal mode so newlines, spaces, and special characters in user input are
// preserved verbatim instead of being interpreted as tmux key names. The
// final Enter is sent as a separate command so it submits the line.
func SendToTmuxPane(paneID, text string, sendEnter bool) error {
	if paneID == "" {
		return errors.New("paneID required")
	}
	if text != "" {
		if err := exec.Command("tmux", "send-keys", "-t", paneID, "-l", text).Run(); err != nil {
			return err
		}
	}
	if sendEnter {
		// Tiny delay so the literal text is registered before Enter — some
		// tmux versions race when commands fire back-to-back.
		time.Sleep(20 * time.Millisecond)
		if err := exec.Command("tmux", "send-keys", "-t", paneID, "Enter").Run(); err != nil {
			return err
		}
	}
	return nil
}

// SendCtrlCToTmuxPane sends Ctrl-C (SIGINT to the foreground process). Useful
// for cancelling a stuck claude/codex session from the web.
func SendCtrlCToTmuxPane(paneID string) error {
	if paneID == "" {
		return errors.New("paneID required")
	}
	return exec.Command("tmux", "send-keys", "-t", paneID, "C-c").Run()
}

// SendKeysToTmuxPane forwards tmux key names to a pane in a single call —
// each name is interpreted by tmux send-keys (e.g. "Enter", "Escape", "Up",
// "1", "y"). Use this for menu navigation / permission answers from the web.
// Multiple names get sent as one batch so a press like "y Enter" submits
// atomically.
func SendKeysToTmuxPane(paneID string, keys []string) error {
	if paneID == "" {
		return errors.New("paneID required")
	}
	if len(keys) == 0 {
		return errors.New("keys required")
	}
	args := append([]string{"send-keys", "-t", paneID}, keys...)
	return exec.Command("tmux", args...).Run()
}

// sweepStaleAgentTmux kills tmux sessions we created via the claude()/codex()
// zsh wrappers (named "claude-…" / "codex-…") once they've been idle longer
// than maxIdle. Only touches those prefixes — never the user's own sessions.
// "Idle" = tmux #{session_activity} (last time any pane in it saw activity).
func sweepStaleAgentTmux(maxIdle time.Duration) {
	out, err := exec.Command("tmux", "list-sessions", "-F", "#{session_name}\t#{session_activity}").Output()
	if err != nil {
		return // no server / no tmux — nothing to sweep
	}
	now := time.Now().Unix()
	cutoff := int64(maxIdle.Seconds())
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name, actStr, ok := strings.Cut(line, "\t")
		if !ok || !(strings.HasPrefix(name, "claude-") || strings.HasPrefix(name, "codex-")) {
			continue
		}
		act, err := strconv.ParseInt(strings.TrimSpace(actStr), 10, 64)
		if err != nil || act == 0 || now-act <= cutoff {
			continue
		}
		if err := exec.Command("tmux", "kill-session", "-t", name).Run(); err == nil {
			log.Printf("tmux GC: killed stale agent session %q (idle %dd)", name, (now-act)/86400)
		}
	}
}

// CapturePane returns the visible buffer of a tmux pane as a single string.
// lines is the scroll-back depth counted from the bottom; 0 means just the
// visible viewport. We cap at a few thousand to avoid pathological responses if
// a pane has a giant scrollback. When plain is false the output keeps ANSI
// escape sequences (-e) so a terminal-emulator client (the web bridge) can
// render colour; plain=true drops them for clients that show raw text (iOS).
func CapturePane(paneID string, lines int, plain bool) (string, error) {
	if paneID == "" {
		return "", errors.New("paneID required")
	}
	if lines < 0 {
		lines = 0
	}
	if lines > 5000 {
		lines = 5000
	}
	args := []string{"capture-pane", "-p", "-t", paneID}
	if !plain {
		args = append(args, "-e")
	}
	if lines > 0 {
		args = append(args, "-S", "-"+strconv.Itoa(lines))
	}
	out, err := exec.Command("tmux", args...).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}
