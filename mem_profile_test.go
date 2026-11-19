package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Read-only heap harness over ~/.claude and ~/.codex: AM_MEMPROF=1, optional AM_MEMPROF_DB (a sessions.db to copy),
// AM_MEMPROF_VERIFY=1 (check pruned traces against full parses) and AM_MEMPROF_OUT (heap profile path).
func TestMemProfileRealData(t *testing.T) {
	if os.Getenv("AM_MEMPROF") == "" {
		t.Skip("set AM_MEMPROF=1 to profile against ~/.claude and ~/.codex")
	}
	state := t.TempDir()
	if src := os.Getenv("AM_MEMPROF_DB"); src != "" {
		for _, suf := range []string{"", "-wal"} {
			if b, err := os.ReadFile(src + suf); err == nil {
				_ = os.WriteFile(filepath.Join(state, "sessions.db"+suf), b, 0o600)
			}
		}
	}
	t.Setenv("AGENT_MONITOR_HOME", state)
	t.Cleanup(func() { initTraceTables(nil) })

	var last uint64
	mark := func(label string) {
		runtime.GC()
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		t.Logf("%-28s heapAlloc=%7.1f MB (+%7.1f)  heapSys=%7.1f MB  heapIdle=%7.1f MB  released=%7.1f MB  rss=%7.1f MB  goroutines=%d",
			label, mb(ms.HeapAlloc), mb(ms.HeapAlloc)-mb(last), mb(ms.HeapSys), mb(ms.HeapIdle), mb(ms.HeapReleased), procRSS(), runtime.NumGoroutine())
		last = ms.HeapAlloc
	}
	mark("baseline")

	store = NewStore()
	paneRegistry = NewPaneRegistry()
	s := store
	for _, a := range []func(*Store){startClaudeAdapter, startCodexAdapter, startOpencodeAdapter, startCursorAdapter, startCursorAgentAdapter} {
		a(s)
	}
	mark(fmt.Sprintf("store+tails (%d sess)", len(s.All())))

	db, err := OpenSessionDB()
	if err != nil {
		t.Fatal(err)
	}
	_ = StartSessionPersistence(s, db, time.Hour)
	initTraceTables(db)
	mark(fmt.Sprintf("history+summaries (%d sess)", len(s.All())))

	startSummaryWorkers(s)
	time.Sleep(4 * time.Second)
	for {
		summaryQueue.mu.Lock()
		n := len(summaryQueue.pending)
		summaryQueue.mu.Unlock()
		if n == 0 && len(summaryQueue.ch) == 0 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	traceCache.mu.Lock()
	nt := len(traceCache.m)
	traceCache.mu.Unlock()
	mark(fmt.Sprintf("summaries (%d traces)", nt))

	usageLimitsSvc = newUsageLimits(homeDir(), filepath.Join(state, "usage-history.json"), nil)
	usageLimitsSvc.get()
	mark("usage limits")

	body, _ := json.Marshal(threadsResponse(false, false))
	mark("threads (default)")
	all, _ := json.Marshal(threadsResponse(true, true))
	mux := http.NewServeMux()
	mux.HandleFunc("/t", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, shapeThreads(threadsResponse(false, false), r.URL.Query()))
	})
	h := gzipJSON(mux)
	get := func(q string, gz bool) int {
		r := httptest.NewRequest("GET", "/t"+q, nil)
		if gz {
			r.Header.Set("Accept-Encoding", "gzip")
		}
		w := httptest.NewRecorder()
		start := time.Now()
		h.ServeHTTP(w, r)
		t.Logf("GET /api/v3/threads%-12s gzip=%-5v %8d B  enc=%q  %v", q, gz, w.Body.Len(), w.Header().Get("Content-Encoding"), time.Since(start).Round(time.Millisecond))
		return w.Body.Len()
	}
	get("", false)
	get("", true)
	get("?compact=1", false)
	get("?compact=1", true)
	t.Logf("threads payload: default=%d B gzip(in-test)=%d B; all=%d B", len(body), gzLen(body), len(all))

	fieldSizes(t, body)
	now := time.Now()
	_ = evalMonitors(now.UnixMilli())
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	_ = buildIntel(day.AddDate(0, 0, -6).UnixMilli(), now.UnixMilli())
	_ = buildTimeline(day.AddDate(0, 0, -6).UnixMilli(), now.UnixMilli(), true)
	_ = buildNow(now.UnixMilli())
	mark("monitors+intel+timeline")

	v := loadThreads()
	n := 0
	for _, th := range threadsV3All(v) {
		if n >= 40 {
			break
		}
		childrenOf(th.ID)
		n++
	}
	mark("children of 40 threads")

	n = 0
	for _, th := range threadsV3All(v) {
		if n >= 32 || len(th.Sessions) == 0 {
			continue
		}
		if ref := resolveWatched(th.Sessions[len(th.Sessions)-1]); ref.Resolved && (ref.Tool == ToolClaude || ref.Tool == ToolCodex) {
			_, _ = watchedItemsFor(ref, -1, -1)
			n++
		}
	}
	mark(fmt.Sprintf("watched items (%d)", n))
	goMem(t)

	traceStats(t)
	if os.Getenv("AM_MEMPROF_VERIFY") != "" {
		verifyPrunedShipped(t)
	}
	writeHeap("")
	tr, p := evictIdleTraces(time.Now().Add(traceIdleTTL + time.Minute))
	mark(fmt.Sprintf("evict idle (%d tr, %d parsers)", tr, p))
	w := evictIdleWatched(time.Now().Add(watchedIdleTTL + time.Minute))
	mark(fmt.Sprintf("evict watched (%d)", w))
	debug.FreeOSMemory()
	mark("free OS memory")
	goMem(t)
	writeHeap(".idle")
	runtime.KeepAlive(s)
}

// verifyPrunedShipped checks list-view values from every pruned cached trace against a full-IO parse.
func verifyPrunedShipped(t *testing.T) {
	traceCache.mu.Lock()
	ents := map[string]*SessionTrace{}
	for id, e := range traceCache.m {
		ents[id] = e.trace
	}
	traceCache.mu.Unlock()
	checked := 0
	for id, tr := range ents {
		sess := store.Get(id)
		if sess == nil {
			continue
		}
		var full *SessionTrace
		switch sess.Tool {
		case ToolClaude:
			p := newClaudeTraceParser(sess)
			p.tr.ioAll = true
			p.advance()
			full = p.snapshot(sess)
		case ToolCodex:
			full = buildCodexTraceIO(sess, true)
		}
		if full == nil || len(full.Spans) != len(tr.Spans) {
			continue // the transcript grew since the cached parse
		}
		if a, b := traceShipped(tr), traceShipped(full); !reflect.DeepEqual(a, b) {
			t.Errorf("%s: shipped differs: pruned %d, full %d", id, len(a), len(b))
		}
		for _, sp := range full.Spans {
			if sp.Fam == "bash" && tr.io.args(sp.ID) != full.io.args(sp.ID) && strings.Contains(full.io.args(sp.ID), "codex") {
				t.Errorf("%s: codex args dropped for %s", id, sp.ID)
			}
		}
		checked++
	}
	t.Logf("verified shipped and codex args on %d traces", checked)
}

func fieldSizes(t *testing.T, body []byte) {
	var top map[string]json.RawMessage
	_ = json.Unmarshal(body, &top)
	for k, v := range top {
		t.Logf("top %-10s %8d B", k, len(v))
	}
	var th struct{ Threads []map[string]json.RawMessage }
	_ = json.Unmarshal(body, &th)
	per := map[string]int{}
	for _, m := range th.Threads {
		for k, v := range m {
			per[k] += len(v) + len(k) + 4
		}
	}
	keys := make([]string, 0, len(per))
	for k := range per {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return per[keys[i]] > per[keys[j]] })
	for _, k := range keys {
		t.Logf("field %-16s %8d B", k, per[k])
	}
}

func mb(b uint64) float64 { return float64(b) / (1 << 20) }

// procRSS is this process's resident set in MB, as ps reports it.
func procRSS() float64 {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return 0
	}
	kb, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	return kb / 1024
}

// goMem logs what the Go runtime holds from the OS, to tell it apart from cgo-free C memory such as SQLite's.
func goMem(t *testing.T) {
	ss := []metrics.Sample{{Name: "/memory/classes/total:bytes"}, {Name: "/memory/classes/heap/released:bytes"}, {Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(ss)
	total, rel := ss[0].Value.Uint64(), ss[1].Value.Uint64()
	t.Logf("go runtime: mapped=%.1f MB released=%.1f MB resident=%.1f MB objects=%.1f MB; process rss=%.1f MB footprint=%s",
		mb(total), mb(rel), mb(total-rel), mb(ss[2].Value.Uint64()), procRSS(), footprint())
}

// footprint is macOS's phys_footprint (what Activity Monitor shows), which drops released pages that ps rss still counts.
func footprint() string {
	out, err := exec.Command("footprint", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return "n/a"
	}
	if _, after, ok := strings.Cut(string(out), "Footprint: "); ok {
		v, _, _ := strings.Cut(after, " (")
		return v
	}
	return "n/a"
}

func writeHeap(suffix string) {
	if p := os.Getenv("AM_MEMPROF_OUT"); p != "" {
		f, err := os.Create(p + suffix)
		if err != nil {
			return
		}
		_ = pprof.WriteHeapProfile(f)
		f.Close()
	}
}

func gzLen(b []byte) int {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = io.Copy(zw, bytes.NewReader(b))
	zw.Close()
	return buf.Len()
}

func traceStats(t *testing.T) {
	traceCache.mu.Lock()
	defer traceCache.mu.Unlock()
	var spans, ioN, ioB int
	for _, e := range traceCache.m {
		spans += len(e.trace.Spans)
		for _, v := range e.trace.io {
			ioN++
			ioB += len(v.args.s) + len(v.args.z) + len(v.result.s) + len(v.result.z)
		}
	}
	claudeParsers.mu.Lock()
	np := len(claudeParsers.m)
	claudeParsers.mu.Unlock()
	t.Logf("traceCache: %d traces, %d spans, %d io entries, %.1f MB io text; claudeParsers=%d", len(traceCache.m), spans, ioN, float64(ioB)/(1<<20), np)
}
