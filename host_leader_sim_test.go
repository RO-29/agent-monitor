package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

// runPtyLeader leads the pty session and runs the agent in its own foreground group, which is not orphaned,
// so the kernel does not drop the agent's SIGTSTP. args: "--", binary, arguments; fd 3 gets the agent pid.
func runPtyLeader(args []string) int {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "agent-monitor __pty-leader: agent required")
		return 2
	}
	pidOut := os.NewFile(3, "agent-pid")
	if pidOut != nil {
		syscall.CloseOnExec(3)
	}
	// tcsetpgrp from the background needs SIGTTOU ignored; the agent inherits that.
	signal.Ignore(syscall.SIGTTOU)
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Foreground: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "agent-monitor: %v\r\n", err)
		return 126
	}
	pid := cmd.Process.Pid
	if pidOut != nil {
		fmt.Fprintf(pidOut, "%d\n", pid)
		_ = pidOut.Close()
	}
	go func() {
		for s := range sigs {
			_ = syscall.Kill(-pid, s.(syscall.Signal))
		}
	}()
	for {
		var ws syscall.WaitStatus
		_, err := syscall.Wait4(pid, &ws, syscall.WUNTRACED, nil)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return 1
		}
		switch {
		case waitStopped(ws):
			// SIGSTOP is never dropped; the host sees this stop and sends SIGCONT when the user resumes.
			_ = syscall.Kill(os.Getpid(), syscall.SIGSTOP)
			_ = unix.IoctlSetPointerInt(0, unix.TIOCSPGRP, pid)
			_ = syscall.Kill(-pid, syscall.SIGCONT)
		case ws.Signaled():
			return 128 + int(ws.Signal())
		case ws.Exited():
			return ws.ExitStatus()
		}
	}
}

// waitStopped is WaitStatus.Stopped without the BSD rule that hides SIGSTOP stops.
func waitStopped(ws syscall.WaitStatus) bool { return ws&0x7f == 0x7f }
