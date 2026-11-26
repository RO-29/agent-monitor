package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// boxHost fakes a Claude Code terminal: an input box that typed text may or may not reach.
type boxHost struct {
	mu       sync.Mutex
	drawn    bool   // the input box is on screen
	echo     bool   // typed text shows in the box
	box      string // the box text
	failRead bool   // screen reads fail
	inputs   []string
	keys     []string
	onExit   func() // run when "/exit" is entered
}

func (b *boxHost) screen() string {
	if !b.drawn {
		return " ▐▛███▜▌   Claude Code\n starting…"
	}
	rule := strings.Repeat("─", 60)
	line := b.box
	if line == "" {
		line = `Try "edit host.go to…"`
	}
	return " Claude Code\n" + rule + "\n❯ " + line + "\n" + rule + "\n  ⏵⏵ auto mode on"
}

func (b *boxHost) handle(req hostRequest) hostResponse {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch req.Op {
	case "ping":
		return hostResponse{OK: true, OutBytes: 4096, IdleMs: 5000}
	case "size":
		return hostResponse{OK: true, Cols: 100, Rows: 30}
	case "screen":
		if b.failRead {
			return hostResponse{Error: "read failed"}
		}
		return hostResponse{OK: true, Text: b.screen()}
	case "input":
		b.inputs = append(b.inputs, req.Data)
		if b.echo {
			b.box += req.Data
		}
		return hostResponse{OK: true}
	case "keys":
		for _, k := range req.Keys {
			b.keys = append(b.keys, k)
			if k == "Enter" {
				if strings.TrimSpace(b.box) == "/exit" && b.onExit != nil {
					b.onExit()
				}
				b.box = ""
			}
		}
		return hostResponse{OK: true}
	}
	return hostResponse{Error: "unknown op"}
}

func (b *boxHost) got() ([]string, []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.inputs...), append([]string(nil), b.keys...)
}

// serveBoxHost registers a host backed by b, with agentPid as its agent, and links it to session sid.
func serveBoxHost(t *testing.T, b *boxHost, agentPid int, sid string) *HostInfo {
	t.Helper()
	id := newHostID()
	sock := shortTempDir(t) + "/" + id + ".sock"
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				line, err := bufio.NewReader(c).ReadBytes('\n')
				if err != nil {
					return
				}
				var req hostRequest
				_ = json.Unmarshal(line, &req)
				_ = json.NewEncoder(c).Encode(b.handle(req))
			}()
		}
	}()
	h, err := hostRegistry.Register(hostRegistration{HostID: id, Pid: os.Getpid(), AgentPid: agentPid, Agent: "claude", Cwd: "/tmp", Socket: sock})
	if err != nil {
		t.Fatal(err)
	}
	if sid != "" {
		hostRegistry.LinkFromHook(id, ToolClaude, sid, 0)
	}
	return h
}

func withBoxRegistry(t *testing.T) {
	t.Helper()
	prevHR, prevStore := hostRegistry, store
	t.Setenv("AGENT_MONITOR_HOME", t.TempDir())
	hostRegistry = NewHostRegistry(shortTempDir(t) + "/hosts.json")
	store = NewStore()
	prevPoll, prevTyped := hostReadyPoll, hostTypedWithin
	hostReadyPoll, hostTypedWithin = 20*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { hostRegistry, store, hostReadyPoll, hostTypedWithin = prevHR, prevStore, prevPoll, prevTyped })
}

func TestTypeFirstPromptWaitsForTheBox(t *testing.T) {
	withBoxRegistry(t)
	b := &boxHost{echo: true}
	h := serveBoxHost(t, b, os.Getpid(), "")
	if done, err := typeFirstPrompt(h, "fix the build"); done || err != nil {
		t.Fatalf("no box yet: done %v err %v", done, err)
	}
	if in, _ := b.got(); len(in) != 0 {
		t.Fatalf("typed before the box: %v", in)
	}
	b.mu.Lock()
	b.drawn = true
	b.mu.Unlock()
	if done, err := typeFirstPrompt(h, "fix the build"); !done || err != nil {
		t.Fatalf("box drawn: done %v err %v", done, err)
	}
	in, keys := b.got()
	if len(in) != 1 || in[0] != "fix the build" || len(keys) != 1 || keys[0] != "Enter" {
		t.Fatalf("inputs %v keys %v", in, keys)
	}
}

func TestTypeFirstPromptNeedsProofAndNeverTypesTwice(t *testing.T) {
	withBoxRegistry(t)
	b := &boxHost{drawn: true, echo: false} // keys lost: the box keeps its placeholder
	h := serveBoxHost(t, b, os.Getpid(), "")
	done, err := typeFirstPrompt(h, "fix the build")
	if !done || err == nil {
		t.Fatalf("lost keys: done %v err %v, want done with an error", done, err)
	}
	in, keys := b.got()
	if len(in) != 1 || len(keys) != 0 {
		t.Fatalf("lost keys must not retype or press Enter: inputs %v keys %v", in, keys)
	}
}

func TestTypeFirstPromptFailedReadIsNoProof(t *testing.T) {
	withBoxRegistry(t)
	b := &boxHost{drawn: true, echo: true}
	h := serveBoxHost(t, b, os.Getpid(), "")
	b.mu.Lock()
	b.failRead = true
	b.mu.Unlock()
	if done, _ := typeFirstPrompt(h, "fix the build"); done {
		t.Fatal("a failed read counted as a drawn box")
	}
	if _, keys := b.got(); len(keys) != 0 {
		t.Fatalf("Enter pressed without proof: %v", keys)
	}
}

func agentProc(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd
}

func TestEndSessionRefusesADraft(t *testing.T) {
	withBoxRegistry(t)
	p := agentProc(t)
	b := &boxHost{drawn: true, box: "delete the old migration"}
	serveBoxHost(t, b, p.Process.Pid, "s-draft")
	store.Upsert(UpsertInput{Tool: ToolClaude, SessionID: "s-draft", State: StateAwaitingInput, HasState: true})
	if _, code, err := endSession("claude:s-draft"); code != statusRefused || err == nil {
		t.Fatalf("code %d err %v", code, err)
	}
	if in, keys := b.got(); len(in) != 0 || len(keys) != 0 {
		t.Fatalf("typed into a draft: inputs %v keys %v", in, keys)
	}
}

func TestEndSessionExitsTheAgent(t *testing.T) {
	withBoxRegistry(t)
	prevInt := sessionInterrupt
	sessionInterrupt = 10 * time.Millisecond
	t.Cleanup(func() { sessionInterrupt = prevInt })
	p := agentProc(t)
	b := &boxHost{drawn: true, echo: true, onExit: func() { _ = p.Process.Kill() }}
	serveBoxHost(t, b, p.Process.Pid, "s-run")
	store.Upsert(UpsertInput{Tool: ToolClaude, SessionID: "s-run", State: StateRunning, HasState: true})
	ended, code, err := endSession("claude:s-run")
	if err != nil || code != 200 || !ended {
		t.Fatalf("ended %v code %d err %v", ended, code, err)
	}
	in, keys := b.got()
	if len(in) != 1 || in[0] != "/exit" || len(keys) < 2 || keys[0] != "Escape" || keys[len(keys)-1] != "Enter" {
		t.Fatalf("inputs %v keys %v, want Escape, /exit, Enter", in, keys)
	}
	if st := store.Get("claude:s-run").State; st != StateCompleted {
		t.Fatalf("state %s, want completed", st)
	}
}

// Claude Code gets a multi-line text typed, with "\" Enter per line break: a paste would reach the model as pasted content.
func TestHostSendTextTypesLinesForClaude(t *testing.T) {
	withBoxRegistry(t)
	prev, prevPause := pasteSettle, typePause
	pasteSettle, typePause = time.Millisecond, time.Millisecond
	t.Cleanup(func() { pasteSettle, typePause = prev, prevPause })
	b := &boxHost{drawn: true}
	h := serveBoxHost(t, b, os.Getpid(), "")
	long := "look at these\n\n- /a.png\n- /b.png"
	if err := hostSendText(h, long, true); err != nil {
		t.Fatal(err)
	}
	if err := hostSendText(h, "short", false); err != nil {
		t.Fatal(err)
	}
	in, keys := b.got()
	want := []string{"look at these", "\\", "\\", "- /a.png", "\\", "- /b.png", "short"}
	if !slices.Equal(in, want) || len(keys) != 4 {
		t.Fatalf("inputs %q keys %v", in, keys)
	}
}

// A host in a tmux pane is read through tmux, which renders every escape; the helper's copy is the fallback.
func TestTargetCapturePrefersTmuxForHost(t *testing.T) {
	withBoxRegistry(t)
	b := &boxHost{drawn: true}
	h := serveBoxHost(t, b, os.Getpid(), "")
	if _, err := hostRegistry.Register(hostRegistration{HostID: h.HostID, Pid: os.Getpid(), AgentPid: os.Getpid(), Agent: "claude", Cwd: "/tmp", Socket: h.Socket, TmuxPane: "%77"}); err != nil {
		t.Fatal(err)
	}
	prev := tmuxCaptureFn
	t.Cleanup(func() { tmuxCaptureFn = prev })
	tmuxCaptureFn = func(p string, _ int, _ bool) (string, error) { return "from tmux " + p, nil }
	if s, _ := targetCapture(hostTargetPrefix+h.HostID, 40, true); s != "from tmux %77" {
		t.Fatalf("got %q", s)
	}
	tmuxCaptureFn = func(string, int, bool) (string, error) { return "", errors.New("no pane") }
	if s, _ := targetCapture(hostTargetPrefix+h.HostID, 40, true); !strings.Contains(s, "Claude Code") {
		t.Fatalf("fallback got %q", s)
	}
}
