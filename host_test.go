package main

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// TestHelperAgent is the fake agent: it echoes lines as GOT:<line>, reports its size and exits on "exit N".
func TestHelperAgent(t *testing.T) {
	if os.Getenv("AM_FAKE_AGENT") != "1" {
		t.Skip("helper process only")
	}
	fmt.Printf("READY host=%s\n", os.Getenv("AM_HOST_ID"))
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "exit "):
			n, _ := strconv.Atoi(strings.TrimPrefix(line, "exit "))
			os.Exit(n)
		case line == "size":
			ws, err := pty.GetsizeFull(os.Stdin)
			if err != nil {
				fmt.Printf("SIZE error %v\n", err)
				continue
			}
			fmt.Printf("SIZE %dx%d\n", ws.Cols, ws.Rows)
		case line == "tstp":
			// Stop the whole process group, as Codex and Claude Code do on Ctrl-Z.
			_ = syscall.Kill(0, syscall.SIGTSTP)
			fmt.Printf("RESUMED\n")
		case line == "color":
			fmt.Print("\x1b]0;title\x07\x1b[31mRED\x1b[0m plain\r\n")
		default:
			fmt.Printf("GOT:%s\n", line)
		}
	}
	os.Exit(3)
}

func fakeAgentArgv() []string { return []string{os.Args[0], "-test.run=^TestHelperAgent$"} }

func fakeAgentEnv() []string { return append(os.Environ(), "AM_FAKE_AGENT=1") }

// shortTempDir keeps unix socket paths under the 104-byte macOS limit.
func shortTempDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "amh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// userTerm is the test's side of the user's terminal.
type userTerm struct {
	master, slave *os.File
	mu            sync.Mutex
	out           bytes.Buffer
}

func newUserTerm(t *testing.T) *userTerm {
	t.Helper()
	m, s, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	_ = pty.Setsize(m, &pty.Winsize{Cols: 100, Rows: 30})
	u := &userTerm{master: m, slave: s}
	go func() {
		b := make([]byte, 4096)
		for {
			n, err := m.Read(b)
			if n > 0 {
				u.mu.Lock()
				u.out.Write(b[:n])
				u.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { m.Close(); s.Close() })
	return u
}

func (u *userTerm) waitFor(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		u.mu.Lock()
		got := u.out.String()
		u.mu.Unlock()
		if strings.Contains(got, want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	t.Fatalf("terminal never showed %q; got %q", want, u.out.String())
}

type runResult struct {
	code int
	err  error
}

func startHost(t *testing.T, opt hostOptions) (*sessionHost, chan runResult) {
	t.Helper()
	h := newSessionHost(opt)
	done := make(chan runResult, 1)
	go func() {
		code, err := h.Run()
		done <- runResult{code, err}
	}()
	return h, done
}

func waitExit(t *testing.T, done chan runResult) runResult {
	t.Helper()
	select {
	case r := <-done:
		return r
	case <-time.After(15 * time.Second):
		t.Fatal("host did not exit")
	}
	return runResult{}
}

func sockCall(t *testing.T, sock string, req hostRequest) hostResponse {
	t.Helper()
	r, err := hostCall(&HostInfo{Socket: sock}, req)
	if err != nil {
		t.Fatalf("%s: %v", req.Op, err)
	}
	return r
}

func TestHostRelayResizeSocketAndExit(t *testing.T) {
	u := newUserTerm(t)
	regs := make(chan hostRegistration, 16)
	removed := make(chan string, 1)
	h, done := startHost(t, hostOptions{
		Argv: fakeAgentArgv(), Env: fakeAgentEnv(), SockDir: shortTempDir(t),
		In: u.slave, Out: u.slave, TTY: u.slave,
		Register: func(r hostRegistration) error { regs <- r; return nil },
		Remove:   func(id string) { removed <- id },
	})
	u.waitFor(t, "READY host="+h.opt.HostID)

	// The user's terminal is raw while the host runs.
	tio, err := unix.IoctlGetTermios(int(u.slave.Fd()), ioctlGetTermios)
	if err != nil || tio.Lflag&(unix.ICANON|unix.ECHO) != 0 {
		t.Fatalf("terminal not raw: %v lflag=%x", err, tio.Lflag)
	}

	if _, err := u.master.Write([]byte("hello\r")); err != nil {
		t.Fatal(err)
	}
	u.waitFor(t, "GOT:hello")

	_ = pty.Setsize(u.master, &pty.Winsize{Cols: 120, Rows: 40})
	h.winch <- syscall.SIGWINCH
	time.Sleep(100 * time.Millisecond)
	_, _ = u.master.Write([]byte("size\r"))
	u.waitFor(t, "SIZE 120x40")

	var reg hostRegistration
	select {
	case reg = <-regs:
	case <-time.After(5 * time.Second):
		t.Fatal("no registration")
	}
	if reg.HostID != h.opt.HostID || reg.Pid != os.Getpid() || reg.AgentPid <= 0 || reg.Socket == "" {
		t.Fatalf("bad registration %+v", reg)
	}
	st, err := os.Stat(reg.Socket)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode: %v %v", err, st)
	}

	sockCall(t, reg.Socket, hostRequest{Op: "input", Data: "via-socket\r"})
	u.waitFor(t, "GOT:via-socket")
	sockCall(t, reg.Socket, hostRequest{Op: "keys", Keys: []string{"x", "y", "Enter"}})
	u.waitFor(t, "GOT:xy")
	sockCall(t, reg.Socket, hostRequest{Op: "input", Data: "color\r"})
	u.waitFor(t, "plain")

	scr := sockCall(t, reg.Socket, hostRequest{Op: "screen", Lines: 20})
	if !strings.Contains(scr.Text, "GOT:xy") || !strings.Contains(scr.Text, "RED plain") || strings.ContainsAny(scr.Text, "\x1b\r\x07") {
		t.Fatalf("screen: %q", scr.Text)
	}
	if sz := sockCall(t, reg.Socket, hostRequest{Op: "size"}); sz.Cols != 120 || sz.Rows != 40 {
		t.Fatalf("size: %+v", sz)
	}
	if p := sockCall(t, reg.Socket, hostRequest{Op: "ping"}); p.HostID != h.opt.HostID || p.OutBytes == 0 {
		t.Fatalf("ping: %+v", p)
	}
	if _, err := hostCall(&HostInfo{Socket: reg.Socket}, hostRequest{Op: "keys", Keys: []string{"Nope"}}); err == nil {
		t.Fatal("unknown key accepted")
	}

	_, _ = u.master.Write([]byte("exit 7\r"))
	r := waitExit(t, done)
	if r.err != nil || r.code != 7 {
		t.Fatalf("exit: %+v", r)
	}
	if _, err := os.Stat(reg.Socket); !os.IsNotExist(err) {
		t.Fatalf("socket not removed: %v", err)
	}
	if id := <-removed; id != h.opt.HostID {
		t.Fatalf("removed %q", id)
	}
	tio, _ = unix.IoctlGetTermios(int(u.slave.Fd()), ioctlGetTermios)
	if tio.Lflag&unix.ICANON == 0 || tio.Lflag&unix.ECHO == 0 {
		t.Fatalf("terminal not restored: lflag=%x", tio.Lflag)
	}
}

func TestHostRunsWithDaemonDown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	t.Setenv("AGENT_MONITOR_PORT", strconv.Itoa(port))
	u := newUserTerm(t)
	h, done := startHost(t, hostOptions{
		Argv: fakeAgentArgv(), Env: fakeAgentEnv(), SockDir: shortTempDir(t),
		In: u.slave, Out: u.slave, TTY: u.slave, Register: postHostRegistration, Remove: deleteHostRegistration,
	})
	u.waitFor(t, "READY host="+h.opt.HostID)
	_, _ = u.master.Write([]byte("still works\r"))
	u.waitFor(t, "GOT:still works")
	_, _ = u.master.Write([]byte("exit 0\r"))
	if r := waitExit(t, done); r.code != 0 || r.err != nil {
		t.Fatalf("exit: %+v", r)
	}
}

func TestHostMissingAgent(t *testing.T) {
	h := newSessionHost(hostOptions{Argv: []string{"am-no-such-agent-xyz"}, SockDir: shortTempDir(t)})
	if code, err := h.Run(); code != 127 || err == nil {
		t.Fatalf("got %d %v", code, err)
	}
}

func TestHostChildEnvDropsOuterHost(t *testing.T) {
	got := hostChildEnv([]string{"A=1", "AM_HOST_ID=outer", "AM_HOST_ID_ASSIGN=x", "B=2"})
	if strings.Join(got, ",") != "A=1,B=2" {
		t.Fatalf("got %v", got)
	}
}

func TestHostKeyBytes(t *testing.T) {
	got, err := hostKeyBytes([]string{"Enter", "Escape", "C-c", "Up", "3", "BTab"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"\r", "\x1b", "\x03", "\x1b[A", "3", "\x1b[Z"}
	for i := range want {
		if string(got[i]) != want[i] {
			t.Fatalf("key %d: %q want %q", i, got[i], want[i])
		}
	}
	if _, err := hostKeyBytes([]string{"Enter", "Bogus"}); err == nil {
		t.Fatal("bogus key accepted")
	}
}
