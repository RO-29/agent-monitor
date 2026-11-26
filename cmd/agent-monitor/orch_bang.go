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

// orchBangs holds the cancel function of each running command, by thread id.
var orchBangs = struct {
	sync.Mutex
	running map[string]context.CancelFunc
}{running: map[string]context.CancelFunc{}}

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
	orchBangs.Lock()
	if orchBangs.running[id] != nil {
		orchBangs.Unlock()
		return "", orchConflict("a ! command still runs in this thread; stop it first")
	}
	ctx, cancel := context.WithTimeout(context.Background(), bangTimeout)
	orchBangs.running[id] = cancel
	orchBangs.Unlock()
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
		bangDone(id)
		return "", err
	}
	go m.finishBang(ctx, id, runID, itemID, th.Cwd, command)
	return "bash", nil
}

func bangDone(id string) {
	orchBangs.Lock()
	if cancel := orchBangs.running[id]; cancel != nil {
		cancel() // frees the timer
	}
	delete(orchBangs.running, id)
	orchBangs.Unlock()
}

// stopBang ends the running "!" command of a thread; false when none runs.
func stopBang(id string) bool {
	orchBangs.Lock()
	cancel := orchBangs.running[id]
	orchBangs.Unlock()
	if cancel == nil {
		return false
	}
	cancel()
	return true
}

func bangRunning(id string) bool {
	orchBangs.Lock()
	defer orchBangs.Unlock()
	return orchBangs.running[id] != nil
}

func (m *OrchManager) finishBang(ctx context.Context, id, runID, itemID, cwd, command string) {
	stdout, stderr, code, stopped := runShell(ctx, cwd, command)
	bangDone(id)
	out := clipBangOutput(stdout)
	if e := clipBangOutput(stderr); e != "" {
		out = strings.TrimRight(out+"\n"+e, "\n")
	}
	block := fmt.Sprintf("<bash-input>%s</bash-input>\n<bash-stdout>%s</bash-stdout><bash-stderr>%s</bash-stderr>",
		command, clipBangOutput(stdout), clipBangOutput(stderr))
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
			if stopped {
				run.Status = "interrupted"
			}
			_ = t.putRun(run)
			t.emit(id, "run.ended", map[string]any{"run": run})
		}
		// Kept in the database, so a daemon restart before the next message does not lose it.
		_, err := t.tx.Exec(`INSERT INTO bang_ctx(thread_id, block) VALUES(?, ?)`, id, block)
		return err
	})
}

// bangContext returns the finished commands the agent has not seen, ready to go before a message, and the last row id.
func (s *OrchStore) bangContext(id string) (string, int64) {
	rows, err := s.db.Query(`SELECT seq, block FROM bang_ctx WHERE thread_id=? ORDER BY seq`, id)
	if err != nil {
		return "", 0
	}
	defer rows.Close()
	var blocks []string
	var last int64
	for rows.Next() {
		var b string
		if rows.Scan(&last, &b) == nil {
			blocks = append(blocks, b)
		}
	}
	if len(blocks) == 0 {
		return "", 0
	}
	return strings.Join(blocks, "\n\n") + "\n\n", last
}

// dropBangContext forgets the commands up to seq, after the agent got them.
func (s *OrchStore) dropBangContext(id string, seq int64) {
	if seq == 0 {
		return
	}
	_ = s.write(func(t *orchTx) error {
		_, err := t.tx.Exec(`DELETE FROM bang_ctx WHERE thread_id=? AND seq<=?`, id, seq)
		return err
	})
}

// takeBangContext returns and forgets the pending commands of a thread.
func (s *OrchStore) takeBangContext(id string) string {
	ctx, seq := s.bangContext(id)
	s.dropBangContext(id, seq)
	return ctx
}

// runShell runs a command in the user's login shell; a timeout or a stop kills its whole process group.
func runShell(ctx context.Context, dir, command string) (stdout, stderr string, code int, stopped bool) {
	sh := os.Getenv("SHELL")
	if sh == "" {
		sh = "/bin/zsh"
	}
	cmd := exec.CommandContext(ctx, sh, "-lc", command)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err := cmd.Run()
	code = cmd.ProcessState.ExitCode()
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		e.WriteString(fmt.Sprintf("\nagent-monitor stopped the command after %s", bangTimeout))
		stopped = true
	case ctx.Err() != nil:
		e.WriteString("\nstopped by the user")
		stopped = true
	case err != nil && cmd.ProcessState == nil:
		e.WriteString(err.Error())
		code = -1
	}
	return o.String(), e.String(), code, stopped
}

func clipBangOutput(s string) string {
	s = strings.TrimRight(reANSI.ReplaceAllString(s, ""), "\n")
	if len(s) > bangOutMax {
		s = "[" + fmt.Sprint(len(s)-bangOutMax) + " bytes cut]\n" + strings.ToValidUTF8(s[len(s)-bangOutMax:], "")
	}
	return s
}
