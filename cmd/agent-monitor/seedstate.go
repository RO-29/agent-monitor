package main

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// seedStateVersion must change whenever claudeAggregate.addLine or codexAggregate.addLine counts differently.
const seedStateVersion = 1

// seedEntry is one transcript's aggregate over its first Off bytes, as complete lines.
type seedEntry struct {
	Mark   fileMark
	Off    int64
	Claude *claudeAggDisk
	Codex  *codexAggDisk
}

type claudeAggDisk struct {
	Tokens                                        TokenUsage
	ToolUsage                                     map[string]int
	Subagents, BgTasks, MessageCount              int
	Files                                         []string
	Title, FirstMessage, Model, Mode, LastMessage string
	LastActivityMs, FirstActivityMs               int64
	Spawns                                        []SpawnRef
	StartedWithClear                              bool
}

type codexAggDisk struct {
	Tokens                                        TokenUsage
	ToolUsage                                     map[string]int
	Files                                         []string
	MessageCount                                  int
	Title, FirstMessage, Model, Mode, LastMessage string
	LastActivityMs                                int64
}

func setKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func keySet(keys []string) map[string]struct{} {
	m := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		m[k] = struct{}{}
	}
	return m
}

func copyCounts(m map[string]int) map[string]int {
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func claudeToDisk(a *claudeAggregate) *claudeAggDisk {
	return &claudeAggDisk{Tokens: a.Tokens, ToolUsage: copyCounts(a.ToolUsage), Subagents: a.Subagents, BgTasks: a.BgTasks,
		MessageCount: a.MessageCount, Files: setKeys(a.Files), Title: a.Title, FirstMessage: a.FirstMessage, Model: a.Model,
		Mode: a.Mode, LastMessage: a.LastMessage, LastActivityMs: a.LastActivityMs, FirstActivityMs: a.FirstActivityMs,
		Spawns: append([]SpawnRef(nil), a.Spawns...), StartedWithClear: a.StartedWithClear}
}

func (d *claudeAggDisk) agg() claudeAggregate {
	return claudeAggregate{Tokens: d.Tokens, ToolUsage: copyCounts(d.ToolUsage), Subagents: d.Subagents, BgTasks: d.BgTasks,
		MessageCount: d.MessageCount, Files: keySet(d.Files), Title: d.Title, FirstMessage: d.FirstMessage, Model: d.Model,
		Mode: d.Mode, LastMessage: d.LastMessage, LastActivityMs: d.LastActivityMs, FirstActivityMs: d.FirstActivityMs,
		Spawns: append([]SpawnRef(nil), d.Spawns...), StartedWithClear: d.StartedWithClear}
}

func codexToDisk(a *codexAggregate) *codexAggDisk {
	return &codexAggDisk{Tokens: a.Tokens, ToolUsage: copyCounts(a.ToolUsage), Files: setKeys(a.Files), MessageCount: a.MessageCount,
		Title: a.Title, FirstMessage: a.FirstMessage, Model: a.Model, Mode: a.Mode, LastMessage: a.LastMessage, LastActivityMs: a.LastActivityMs}
}

func (d *codexAggDisk) agg() codexAggregate {
	return codexAggregate{Tokens: d.Tokens, ToolUsage: copyCounts(d.ToolUsage), Files: keySet(d.Files), MessageCount: d.MessageCount,
		Title: d.Title, FirstMessage: d.FirstMessage, Model: d.Model, Mode: d.Mode, LastMessage: d.LastMessage, LastActivityMs: d.LastActivityMs}
}

type seedStateFile struct {
	Version int
	Entries map[string]*seedEntry
}

// seedCache holds saved transcript aggregates while a scan runs; between scans it is empty and lives only on disk.
type seedCache struct {
	mu      sync.Mutex
	run     sync.Mutex // one scan or refresh at a time
	entries map[string]*seedEntry
	used    map[string]*seedEntry
}

var seeds = &seedCache{}

func seedStatePath() string { return filepath.Join(stateDir(), "scanpos-seed.gob") }

// begin loads the saved positions; claude and codex then resume from them until end.
func (c *seedCache) begin() {
	c.run.Lock()
	var sf seedStateFile
	if err := readGob(seedStatePath(), &sf); err != nil || sf.Version != seedStateVersion {
		sf.Entries = nil
	}
	c.mu.Lock()
	c.entries, c.used = sf.Entries, map[string]*seedEntry{}
	if c.entries == nil {
		c.entries = map[string]*seedEntry{}
	}
	c.mu.Unlock()
}

// end saves the entries used since begin and drops them from memory.
func (c *seedCache) end() {
	c.mu.Lock()
	used := c.used
	c.entries, c.used = nil, nil
	c.mu.Unlock()
	defer c.run.Unlock()
	if used == nil {
		return
	}
	if err := writeGobAtomic(seedStatePath(), seedStateFile{Version: seedStateVersion, Entries: used}); err != nil {
		log.Printf("scanpos: seed state not saved: %v", err)
	}
}

func (c *seedCache) lookup(path string) (*seedEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.used == nil {
		return nil, false
	}
	return c.entries[path], true
}

func (c *seedCache) put(path string, e *seedEntry) {
	c.mu.Lock()
	if c.used != nil {
		c.used[path] = e
	}
	c.mu.Unlock()
}

// resume opens path and returns it positioned at a valid saved offset (or 0) with the saved entry, if any.
func (c *seedCache) resume(path string, has func(*seedEntry) bool) (f *os.File, info os.FileInfo, e *seedEntry, open bool, err error) {
	if f, err = os.Open(path); err != nil {
		return
	}
	if info, err = f.Stat(); err != nil {
		f.Close()
		return
	}
	e, open = c.lookup(path)
	if e != nil && (!has(e) || !e.Mark.valid(f, info, e.Off)) {
		e = nil
	}
	off := int64(0)
	if e != nil {
		off = e.Off
	}
	if _, err = f.Seek(off, io.SeekStart); err != nil {
		f.Close()
	}
	return
}

// claudeSeed is scanClaudeJSONL(path, nil, 0), resumed from the saved position when there is one.
func (c *seedCache) claudeSeed(path string) (claudeAggregate, error) {
	f, info, e, open, err := c.resume(path, func(e *seedEntry) bool { return e.Claude != nil })
	if err != nil {
		return claudeAggregate{ToolUsage: map[string]int{}, Files: map[string]struct{}{}}, err
	}
	defer f.Close()
	agg, off := claudeAggregate{ToolUsage: map[string]int{}, Files: map[string]struct{}{}}, int64(0)
	if e != nil {
		agg, off = e.Claude.agg(), e.Off
	}
	var saved *claudeAggDisk
	done := foldLines(f, 8*1024*1024, func(line []byte, terminated bool) {
		if !terminated && open {
			saved = claudeToDisk(&agg)
		}
		agg.addLine(line, nil, nil)
	})
	if open {
		if saved == nil {
			saved = claudeToDisk(&agg)
		}
		c.put(path, &seedEntry{Mark: markFile(f, info, off+done), Off: off + done, Claude: saved})
	}
	return agg, nil
}

// codexSeed is scanCodexJSONL(path, nil, 0), resumed from the saved position when there is one.
func (c *seedCache) codexSeed(path string) (codexAggregate, error) {
	f, info, e, open, err := c.resume(path, func(e *seedEntry) bool { return e.Codex != nil })
	if err != nil {
		return codexAggregate{ToolUsage: map[string]int{}, Files: map[string]struct{}{}}, err
	}
	defer f.Close()
	agg, off := codexAggregate{ToolUsage: map[string]int{}, Files: map[string]struct{}{}}, int64(0)
	if e != nil {
		agg, off = e.Codex.agg(), e.Off
	}
	var saved *codexAggDisk
	done := foldLines(f, 16*1024*1024, func(line []byte, terminated bool) {
		if !terminated && open {
			saved = codexToDisk(&agg)
		}
		agg.addLine(line, nil, nil)
	})
	if open {
		if saved == nil {
			saved = codexToDisk(&agg)
		}
		c.put(path, &seedEntry{Mark: markFile(f, info, off+done), Off: off + done, Codex: saved})
	}
	return agg, nil
}

// seedTranscripts lists the files the claude and codex adapters seed from.
func seedTranscripts() (claude, codex []string) {
	cut := time.Now().Add(-365 * 24 * time.Hour)
	ms, _ := filepath.Glob(filepath.Join(homeDir(), ".claude", "projects", "*", "*.jsonl"))
	for _, m := range ms {
		if st, err := os.Stat(m); err == nil && !st.ModTime().Before(cut) {
			claude = append(claude, m)
		}
	}
	_ = filepath.WalkDir(filepath.Join(homeDir(), ".codex", "sessions"), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasPrefix(d.Name(), "rollout-") || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		if st, err := d.Info(); err == nil && !st.ModTime().Before(cut) {
			codex = append(codex, p)
		}
		return nil
	})
	return
}

// refreshSeedState advances every saved position to the end of its transcript, so the next start reads only newer bytes.
func refreshSeedState() {
	cl, cx := seedTranscripts()
	seeds.begin()
	for _, p := range cl {
		_, _ = seeds.claudeSeed(p)
	}
	for _, p := range cx {
		_, _ = seeds.codexSeed(p)
	}
	seeds.end()
}

// startSeedStateRefresh keeps the saved positions recent while the daemon runs and at shutdown.
func startSeedStateRefresh(every time.Duration) {
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for range t.C {
			refreshSeedState()
		}
	}()
	onShutdown(refreshSeedState)
}
