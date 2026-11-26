package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// A "!" message in a driven thread runs as a shell command, as Claude Code's bash mode does in a terminal.
// The output shows in the chat at once; the agent sees it with the next message, and no turn starts.

const (
	bangTimeout = 10 * time.Minute
	bangOutMax  = 30_000
)

var orchBangs = struct {
	sync.Mutex
	ctx map[string][]string // thread id → finished commands the agent has not seen
}{ctx: map[string][]string{}}

// bangCommand returns the command of a "!" message, or "" for an ordinary message.
func bangCommand(text string) string {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "!") {
		return ""
	}
	return strings.TrimSpace(t[1:])
}

func (m *OrchManager) runBang(id, command string) (string, error) {
	th, err := m.store.Thread(id)
	if err != nil {
		return "", err
	}
	runID := orchID("ob_")
	itemID := runID + ".cmd"
	now := time.Now().UnixMilli()
	if err := m.store.write(func(t *orchTx) error {
		var n int
		_ = t.tx.QueryRow(`SELECT COUNT(*) FROM runs WHERE thread_id=?`, id).Scan(&n)
		run := &ORun{ID: runID, ThreadID: id, Index: n, Prompt: "!" + command, Status: "running", StartedAt: now}
		if err := t.putRun(run); err != nil {
			return err
		}
		t.emit(id, "run.started", map[string]any{"run": run})
		it := &OItem{ID: itemID, ThreadID: id, RunID: runID, Kind: "command", Status: "started", By: "user",
			Text: command, Command: command, Cwd: th.Cwd, StartedAt: now}
		if err := t.putItem(it); err != nil {
			return err
		}
		t.emit(id, "item.started", map[string]any{"item": it})
		return nil
	}); err != nil {
		return "", err
	}
	go m.finishBang(id, runID, itemID, th.Cwd, command)
	return "bash", nil
}

func (m *OrchManager) finishBang(id, runID, itemID, cwd, command string) {
	stdout, stderr, code := runShell(cwd, command)
	out := clipBangOutput(stdout)
	if e := clipBangOutput(stderr); e != "" {
		out = strings.TrimRight(out+"\n"+e, "\n")
	}
	_ = m.store.write(func(t *orchTx) error {
		if it, err := t.item(itemID); err == nil {
			it.Status, it.Output, it.ExitCode, it.EndedAt = "completed", out, &code, t.now
			if code != 0 {
				it.Status = "failed"
			}
			_ = t.putItem(it)
			t.emit(id, "item.completed", map[string]any{"item": it})
		}
		if run, err := t.run(runID); err == nil {
			run.Status, run.EndedAt = "completed", t.now
			_ = t.putRun(run)
			t.emit(id, "run.ended", map[string]any{"run": run})
		}
		return nil
	})
	block := fmt.Sprintf("<bash-input>%s</bash-input>\n<bash-stdout>%s</bash-stdout><bash-stderr>%s</bash-stderr>",
		command, clipBangOutput(stdout), clipBangOutput(stderr))
	orchBangs.Lock()
	orchBangs.ctx[id] = append(orchBangs.ctx[id], block)
	orchBangs.Unlock()
}

// takeBangContext returns the finished commands the agent has not seen, ready to go before a message.
func takeBangContext(id string) string {
	orchBangs.Lock()
	defer orchBangs.Unlock()
	blocks := orchBangs.ctx[id]
	delete(orchBangs.ctx, id)
	if len(blocks) == 0 {
		return ""
	}
	return strings.Join(blocks, "\n\n") + "\n\n"
}

// runShell runs a command in the user's login shell; a timeout kills its whole process group.
func runShell(dir, command string) (stdout, stderr string, code int) {
	sh := os.Getenv("SHELL")
	if sh == "" {
		sh = "/bin/zsh"
	}
	ctx, cancel := context.WithTimeout(context.Background(), bangTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, sh, "-lc", command)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err := cmd.Run()
	code = cmd.ProcessState.ExitCode()
	if ctx.Err() == context.DeadlineExceeded {
		e.WriteString(fmt.Sprintf("\nagent-monitor stopped the command after %s", bangTimeout))
	} else if err != nil && cmd.ProcessState == nil {
		e.WriteString(err.Error())
		code = -1
	}
	return o.String(), e.String(), code
}

func clipBangOutput(s string) string {
	s = strings.TrimRight(reANSI.ReplaceAllString(s, ""), "\n")
	if len(s) > bangOutMax {
		s = "[" + fmt.Sprint(len(s)-bangOutMax) + " bytes cut]\n" + strings.ToValidUTF8(s[len(s)-bangOutMax:], "")
	}
	return s
}
