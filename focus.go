package main

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// focusTmuxPane is the "take me to the terminal" primitive. Given a live tmux
// pane it: (1) makes that pane the active pane in its tmux session, (2) switches
// any attached client that's viewing a different session over to it, and
// (3) brings the hosting terminal app to the foreground. Returns the terminal
// app it raised (display name), or "" if it couldn't identify one.
func focusTmuxPane(p *TmuxPane) (string, error) {
	if p == nil || p.PaneID == "" {
		return "", fmt.Errorf("no pane")
	}
	// 1) select the pane's window + pane so the attached client displays it.
	_ = exec.Command("tmux", "select-window", "-t", p.PaneID).Run()
	_ = exec.Command("tmux", "select-pane", "-t", p.PaneID).Run()

	// 2) point every client that's on another session at this one — that's what
	//    "go to where it's running" means. Harmless if already here.
	clients := listTmuxClients()
	for _, c := range clients {
		if c.Session != p.SessionName && c.Name != "" {
			_ = exec.Command("tmux", "switch-client", "-c", c.Name, "-t", p.SessionName).Run()
		}
	}

	// 3) raise the terminal emulator hosting that client.
	term := detectTerminalApp(clients, p.SessionName)
	if term.bundle != "" {
		_ = exec.Command("open", "-b", term.bundle).Run()
	}
	return term.name, nil
}

type tmuxClient struct{ Name, Session, TTY string }

func listTmuxClients() []tmuxClient {
	out, err := exec.Command("tmux", "list-clients", "-F",
		"#{client_name}\t#{session_name}\t#{client_tty}").Output()
	if err != nil {
		return nil
	}
	var cs []tmuxClient
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l == "" {
			continue
		}
		f := strings.Split(l, "\t")
		if len(f) < 3 {
			continue
		}
		cs = append(cs, tmuxClient{Name: f[0], Session: f[1], TTY: f[2]})
	}
	return cs
}

type termApp struct{ name, bundle string }

// Known macOS terminal emulators: an executable-path substring → (display
// name, bundle id used with `open -b`). Ordered by rough popularity so the
// running-terminal fallback prefers the likely host.
var knownTerms = []struct{ match, name, bundle string }{
	{"iterm", "iTerm", "com.googlecode.iterm2"},
	{"ghostty", "Ghostty", "com.mitchellh.ghostty"},
	{"wezterm", "WezTerm", "com.github.wez.wezterm"},
	{"kitty", "kitty", "net.kovidgoyal.kitty"},
	{"alacritty", "Alacritty", "org.alacritty"},
	{"warp", "Warp", "dev.warp.Warp-Stable"},
	{"hyper", "Hyper", "co.zeit.hyper"},
	{"tabby", "Tabby", "org.tabby"},
	{"rio", "Rio", "com.raphaelamorim.rio"},
	// Apple Terminal last: its executable path just contains "Terminal", which
	// is a broad match, so only fall back to it if nothing else hit.
	{"/terminal.app/", "Terminal", "com.apple.Terminal"},
}

// detectTerminalApp finds the terminal hosting the tmux client by walking up
// from the client's tty processes to an ancestor that's a known terminal app;
// falls back to the first known terminal currently running.
func detectTerminalApp(clients []tmuxClient, session string) termApp {
	var tty string
	for _, c := range clients {
		if c.Session == session {
			tty = c.TTY
			break
		}
	}
	if tty == "" && len(clients) > 0 {
		tty = clients[0].TTY
	}
	ppid, comm := procPPIDComm()
	if tty != "" {
		if t := terminalFromTTY(tty, ppid, comm); t.bundle != "" {
			return t
		}
	}
	return firstRunningTerminal(comm)
}

func matchTerm(execPath string) (termApp, bool) {
	lp := strings.ToLower(execPath)
	for _, kt := range knownTerms {
		if strings.Contains(lp, kt.match) {
			return termApp{kt.name, kt.bundle}, true
		}
	}
	return termApp{}, false
}

// terminalFromTTY walks the ppid chain up from each process on the client tty
// (shell, tmux client, login) until it hits the terminal app that spawned them.
func terminalFromTTY(tty string, ppid map[int]int, comm map[int]string) termApp {
	short := strings.TrimPrefix(tty, "/dev/")
	seed, err := exec.Command("ps", "-t", short, "-o", "pid=").Output()
	if err != nil {
		return termApp{}
	}
	for _, tok := range strings.Fields(string(seed)) {
		pid, _ := strconv.Atoi(tok)
		for i := 0; pid > 1 && i < 20; i++ {
			if t, ok := matchTerm(comm[pid]); ok {
				return t
			}
			pid = ppid[pid]
		}
	}
	return termApp{}
}

func firstRunningTerminal(comm map[int]string) termApp {
	for _, kt := range knownTerms {
		for _, c := range comm {
			if strings.Contains(strings.ToLower(c), kt.match) {
				return termApp{kt.name, kt.bundle}
			}
		}
	}
	return termApp{}
}

// procPPIDComm snapshots every process's parent pid and executable path.
func procPPIDComm() (map[int]int, map[int]string) {
	ppid := map[int]int{}
	comm := map[int]string{}
	out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=,comm=").Output()
	if err != nil {
		return ppid, comm
	}
	for _, l := range strings.Split(string(out), "\n") {
		f := strings.Fields(l)
		if len(f) < 3 {
			continue
		}
		pid, _ := strconv.Atoi(f[0])
		pp, _ := strconv.Atoi(f[1])
		ppid[pid] = pp
		comm[pid] = strings.Join(f[2:], " ")
	}
	return ppid, comm
}
