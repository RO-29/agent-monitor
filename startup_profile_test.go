package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Read-only restart harness over ~/.claude and ~/.codex: AM_STARTPROF=1 and AM_STARTPROF_STATE=<dir> (kept between runs,
// so the first run is cold and the next is warm); AM_STARTPROF_DB seeds the dir with a copy of a sessions.db.
func TestStartupProfileRealData(t *testing.T) {
	if os.Getenv("AM_STARTPROF") == "" {
		t.Skip("set AM_STARTPROF=1 and AM_STARTPROF_STATE=<dir> to profile a restart against ~/.claude and ~/.codex")
	}
	state := os.Getenv("AM_STARTPROF_STATE")
	if state == "" {
		state = t.TempDir()
	}
	_ = os.MkdirAll(state, 0o755)
	if src := os.Getenv("AM_STARTPROF_DB"); src != "" && !pathExists(filepath.Join(state, "sessions.db")) {
		for _, suf := range []string{"", "-wal"} {
			if b, err := os.ReadFile(src + suf); err == nil {
				_ = os.WriteFile(filepath.Join(state, "sessions.db"+suf), b, 0o600)
			}
		}
	}
	t.Setenv("AGENT_MONITOR_HOME", state)
	t.Cleanup(func() { initTraceTables(nil) })

	type row struct {
		name      string
		cpu, wall time.Duration
	}
	var rows []row
	measure := func(name string, fn func()) {
		c0, w0 := cpuTime(), time.Now()
		fn()
		rows = append(rows, row{name, cpuTime() - c0, time.Since(w0)})
	}

	store = NewStore()
	paneRegistry = NewPaneRegistry()
	s := store
	total0, wall0 := cpuTime(), time.Now()
	seeds.begin()
	for _, a := range []struct {
		name string
		fn   func(*Store)
	}{{"adapter claude", startClaudeAdapter}, {"adapter codex", startCodexAdapter}, {"adapter opencode", startOpencodeAdapter},
		{"adapter cursor", startCursorAdapter}, {"adapter cursor-agent", startCursorAgentAdapter}} {
		measure(a.name, func() { a.fn(s) })
	}
	measure("seed state save", seeds.end)
	var flush func()
	measure("sessions.db restore", func() {
		db, err := OpenSessionDB()
		if err != nil {
			t.Fatal(err)
		}
		flush = StartSessionPersistence(s, db, time.Hour)
		initTraceTables(db)
	})
	stale, staleBytes := 0, int64(0)
	for _, sess := range s.All() {
		st, err := os.Stat(sess.TranscriptPath)
		if sess.TranscriptPath == "" || err != nil {
			continue
		}
		summaryCache.mu.Lock()
		e := summaryCache.m[sess.ID]
		summaryCache.mu.Unlock()
		if e == nil || e.size != st.Size() || e.mtime != st.ModTime().UnixMilli() {
			stale++
			staleBytes += st.Size()
			if os.Getenv("AM_STARTPROF_STALE") != "" {
				t.Logf("stale summary %s %d B cached=%v", sess.ID, st.Size(), e != nil)
			}
		}
	}
	t.Logf("trace summaries to recompute: %d (%.1f MB of transcripts)", stale, float64(staleBytes)/(1<<20))
	measure("trace summaries", func() {
		startSummaryWorkers(s)
		time.Sleep(4 * time.Second)
		for {
			summaryQueue.mu.Lock()
			n := len(summaryQueue.pending)
			summaryQueue.mu.Unlock()
			if n == 0 && len(summaryQueue.ch) == 0 {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
	})
	measure("usage limits", func() {
		usageLimitsSvc = newUsageLimits(homeDir(), filepath.Join(state, "usage-history.json"), nil)
		usageLimitsSvc.get()
	})
	measure("search pass (stat + files rows)", func() { searchWarmCheck(t, state) })
	totalCPU, totalWall := cpuTime()-total0, time.Since(wall0)
	measure("shutdown save", func() {
		flush()
		runShutdownHooks()
		refreshSeedState()
		usageLimitsSvc.shutdown()
	})

	load, _ := exec.Command("sysctl", "-n", "vm.loadavg").Output()
	t.Logf("sessions=%d  state=%s  load=%s", len(s.All()), state, strings.TrimSpace(string(load)))
	for _, r := range rows {
		t.Logf("%-34s cpu=%8.2fs  wall=%8.2fs", r.name, r.cpu.Seconds(), r.wall.Seconds())
	}
	t.Logf("%-34s cpu=%8.2fs  wall=%8.2fs (trace summaries wall includes a fixed 4 s wait)", "startup total", totalCPU.Seconds(), totalWall.Seconds())
	if out := os.Getenv("AM_STARTPROF_OUT"); out != "" {
		f, err := os.OpenFile(out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			for _, r := range rows {
				fmt.Fprintf(f, "%s\t%.3f\t%.3f\n", r.name, r.cpu.Seconds(), r.wall.Seconds())
			}
			fmt.Fprintf(f, "startup total\t%.3f\t%.3f\n", totalCPU.Seconds(), totalWall.Seconds())
			f.Close()
		}
	}
}

// searchWarmCheck runs a search pass against a search.db in state, when the state dir has one.
func searchWarmCheck(t *testing.T, state string) {
	p := filepath.Join(state, "search.db")
	if !pathExists(p) {
		files := listTranscripts()
		t.Logf("search: no search.db in state dir; listed %d transcripts only", len(files))
		return
	}
	ix, err := openSearchIndex(p)
	if err != nil {
		t.Logf("search: %v", err)
		return
	}
	defer ix.w.Close()
	ix.throttle = 0
	ix.pass()
}

func cpuTime() time.Duration {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}
