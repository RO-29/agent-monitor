package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// TestHelperLeader is the pty leader for host tests.
func TestHelperLeader(t *testing.T) {
	if os.Getenv("AM_PTY_LEADER") != "1" {
		t.Skip("helper process only")
	}
	for i, a := range os.Args {
		if a == "--" {
			os.Exit(runPtyLeader(os.Args[i:]))
		}
	}
	os.Exit(2)
}

func termIsRaw(t *testing.T, f *os.File) bool {
	t.Helper()
	tio, err := unix.IoctlGetTermios(int(f.Fd()), ioctlGetTermios)
	if err != nil {
		t.Fatal(err)
	}
	return tio.Lflag&(unix.ICANON|unix.ECHO) == 0
}

func TestHostCtrlZSuspendsAndResumes(t *testing.T) {
	u := newUserTerm(t)
	stopped := make(chan bool, 1)
	resume := make(chan struct{})
	h, done := startHost(t, hostOptions{
		Argv: fakeAgentArgv(), Env: append(fakeAgentEnv(), "AM_PTY_LEADER=1"), SockDir: shortTempDir(t),
		In: u.slave, Out: u.slave, TTY: u.slave,
		Leader: []string{os.Args[0], "-test.run=^TestHelperLeader$"},
		Suspend: func() {
			stopped <- termIsRaw(t, u.slave)
			<-resume
		},
	})
	u.waitFor(t, "READY")
	if lp, ap := h.cmd.Process.Pid, int(h.agentPid.Load()); ap == lp || ap <= 0 {
		t.Fatalf("agent pid %d, leader %d", ap, lp)
	}
	if !termIsRaw(t, u.slave) {
		t.Fatal("terminal not raw while the agent runs")
	}
	_, _ = u.master.Write([]byte("tstp\r"))
	select {
	case raw := <-stopped:
		if raw {
			t.Fatal("terminal still raw while the host is suspended")
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("agent stop did not suspend the host; terminal %q", u.String())
	}
	if strings.Contains(u.String(), "RESUMED") {
		t.Fatal("agent ran on while suspended")
	}
	_ = pty.Setsize(u.master, &pty.Winsize{Cols: 120, Rows: 40})
	close(resume)
	u.waitFor(t, "RESUMED")
	if !termIsRaw(t, u.slave) {
		t.Fatal("raw mode not restored after resume")
	}
	_, _ = u.master.Write([]byte("size\r"))
	u.waitFor(t, "SIZE 120x40")
	_, _ = u.master.Write([]byte("exit 7\r"))
	if r := waitExit(t, done); r.code != 7 {
		t.Fatalf("exit %+v", r)
	}
}

func (u *userTerm) String() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.out.String()
}
