package main

import (
	"errors"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// TmuxPane is a snapshot of one tmux pane.
type TmuxPane struct {
	PaneID         string // %42
	SessionName    string
	WindowID       string
	PanePID        int
	CurrentCommand string
	CurrentPath    string
	StartCommand   string
}

// listTmuxPanes runs tmux list-panes -a; the error keeps tmux's stderr.
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

// procTree maps each pid to its children.
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

// SendToTmuxPane types text literally (-l) and sends Enter as a separate command.
// asPaste: text with a line break or over 300 characters goes as one bracketed paste.
func asPaste(text string) bool {
	return strings.Contains(text, "\n") || utf8.RuneCountInString(text) > 300
}

var pasteSettle = 300 * time.Millisecond

func SendToTmuxPane(paneID, text string, sendEnter bool) error {
	if paneID == "" {
		return errors.New("paneID required")
	}
	paste := asPaste(text)
	if paste {
		// One bracketed paste: typed fast as keys, a long or multi-line text loses its start in Claude Code.
		buf := "am-paste-" + strings.TrimPrefix(paneID, "%")
		load := exec.Command("tmux", "load-buffer", "-b", buf, "-")
		load.Stdin = strings.NewReader(text)
		if err := load.Run(); err != nil {
			return err
		}
		if err := exec.Command("tmux", "paste-buffer", "-p", "-d", "-b", buf, "-t", paneID).Run(); err != nil {
			return err
		}
	} else if text != "" {
		if err := exec.Command("tmux", "send-keys", "-t", paneID, "-l", "--", text).Run(); err != nil {
			return err
		}
	}
	if sendEnter {
		// The text must be registered before Enter; a paste needs longer.
		time.Sleep(20 * time.Millisecond)
		if paste {
			time.Sleep(pasteSettle)
		}
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

// SendKeysToTmuxPane sends tmux key names in one call, so "y Enter" is atomic.
func SendKeysToTmuxPane(paneID string, keys []string) error {
	if paneID == "" {
		return errors.New("paneID required")
	}
	if len(keys) == 0 {
		return errors.New("keys required")
	}
	args := append([]string{"send-keys", "-t", paneID, "--"}, keys...) // a key such as "-t" must not become an option
	return exec.Command("tmux", args...).Run()
}

// sweepStaleAgentTmux kills our claude-/codex- tmux sessions idle longer than maxIdle.
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

// CapturePane returns a pane's buffer with up to lines of scrollback.
// plain drops ANSI escapes.
// capturePanesSep splits the output of one tmux call that captures several panes.
const capturePanesSep = "\x1eam-pane-end\x1e"

// CapturePanes reads the last lines of several panes with one tmux process; a failed pane makes the whole call fail.
func CapturePanes(paneIDs []string, lines int) (map[string]string, error) {
	if len(paneIDs) == 0 {
		return map[string]string{}, nil
	}
	var args []string
	for i, id := range paneIDs {
		if i > 0 {
			args = append(args, ";")
		}
		args = append(args, "capture-pane", "-p", "-t", id, "-S", "-"+strconv.Itoa(lines), ";", "display-message", "-p", capturePanesSep)
	}
	out, err := exec.Command("tmux", args...).Output()
	if err != nil {
		return nil, err
	}
	parts := strings.Split(string(out), capturePanesSep+"\n")
	if len(parts) != len(paneIDs)+1 {
		return nil, errors.New("tmux returned a different number of panes")
	}
	res := make(map[string]string, len(paneIDs))
	for i, id := range paneIDs {
		res[id] = parts[i]
	}
	return res, nil
}

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
