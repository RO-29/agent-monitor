package main

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// attentionWindow is how recent a tmux client's input must be to count as "looking at it now".
const attentionWindow = 60 * time.Second

const attentionClientFormat = "#{client_tty}\t#{client_session}\t#{client_activity}\t#{client_flags}\t#{pane_id}\t#{focus-events}"

// attentionProbe decides whether the user is looking at a pane; the commands are swappable for tests.
type attentionProbe struct {
	listClients func(socket string) (string, error)
	frontBundle func() (string, error)
	now         func() time.Time
	ttl         time.Duration

	mu      sync.Mutex
	clients map[string]cachedOut
	front   cachedOut
}

type cachedOut struct {
	at  time.Time
	out string
	err error
}

func newAttentionProbe() *attentionProbe {
	return &attentionProbe{listClients: tmuxListClientsOut, frontBundle: macFrontBundle, now: time.Now, ttl: time.Second, clients: map[string]cachedOut{}}
}

func tmuxListClientsOut(socket string) (string, error) {
	args := []string{}
	if socket != "" && pathExists(socket) {
		args = append(args, "-S", socket)
	}
	args = append(args, "list-clients", "-F", attentionClientFormat)
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux", args...).Output()
	return string(out), err
}

// macFrontBundle prints the frontmost app's bundle id line, for example "CFBundleIdentifier"="com.googlecode.iterm2".
func macFrontBundle() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/sh", "-c", `lsappinfo info -only bundleid "$(lsappinfo front)"`).Output()
	return string(out), err
}

// focused is the ApprovalStore hook: true only when every signal says the user is at this pane.
func (p *attentionProbe) focused(storeID string) bool {
	if paneRegistry == nil {
		return false
	}
	reg := paneRegistry.Get(storeID)
	if reg == nil || reg.PaneID == "" {
		return false
	}
	ok, _ := p.check(reg.PaneID, strings.SplitN(reg.TmuxSocket, ",", 2)[0])
	return ok
}

func (p *attentionProbe) check(paneID, socket string) (bool, string) {
	now := p.now()
	p.mu.Lock()
	c, hit := p.clients[socket]
	if !hit || now.Sub(c.at) > p.ttl {
		c.out, c.err = p.listClients(socket)
		c.at = now
		p.clients[socket] = c
	}
	f := p.front
	if f.at.IsZero() || now.Sub(f.at) > p.ttl {
		f.out, f.err = p.frontBundle()
		f.at = now
		p.front = f
	}
	p.mu.Unlock()
	if c.err != nil {
		return false, "tmux clients unknown"
	}
	if f.err != nil {
		return false, "front app unknown"
	}
	return paneAttention(c.out, paneID, f.out, now)
}

// paneAttention is the decision table; any unknown signal means not focused.
func paneAttention(clientsOut, paneID, frontOut string, now time.Time) (bool, string) {
	bundle := parseBundleID(frontOut)
	if bundle == "" {
		return false, "front app unknown"
	}
	if !isTerminalBundle(bundle) {
		return false, "front app is " + bundle
	}
	why := "no tmux client shows the pane"
	for _, line := range strings.Split(clientsOut, "\n") {
		f := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(f) < 6 || f[4] != paneID {
			continue
		}
		if f[5] != "1" {
			why = "tmux focus-events is off"
			continue
		}
		if !hasFlag(f[3], "focused") {
			why = "tmux client not focused"
			continue
		}
		act, err := strconv.ParseInt(strings.TrimSpace(f[2]), 10, 64)
		if err != nil || act <= 0 {
			why = "client activity unknown"
			continue
		}
		if now.Sub(time.Unix(act, 0)) > attentionWindow {
			why = "no input in the last minute"
			continue
		}
		return true, "terminal has focus"
	}
	return false, why
}

func hasFlag(flags, want string) bool {
	for _, f := range strings.Split(flags, ",") {
		if strings.TrimSpace(f) == want {
			return true
		}
	}
	return false
}

func parseBundleID(out string) string {
	_, v, ok := strings.Cut(strings.TrimSpace(out), "=")
	if !ok {
		return ""
	}
	return strings.Trim(strings.TrimSpace(v), `"`)
}

func isTerminalBundle(b string) bool {
	for _, kt := range knownTerms {
		if strings.EqualFold(kt.bundle, b) {
			return true
		}
	}
	return false
}
