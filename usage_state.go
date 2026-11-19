package main

import (
	"log"
	"os"
	"path/filepath"
	"time"
)

// usageStateVersion must change whenever claudeLine or codexLine counts differently.
const usageStateVersion = 2

// usageSaveEvery spaces the saves of the scan positions; shutdown saves too.
const usageSaveEvery = 5 * time.Minute

type usageBucketDisk struct {
	Min   int64
	Model string
	Tok   TokenUsage
	Cost  float64
	N     int
}

type codexLatestDisk struct {
	W  *codexWin
	At int64
}

type usageFileDisk struct {
	Path      string
	Mark      fileMark
	Off       int64
	ModNs     int64
	Buckets   []usageBucketDisk
	PrevTotal int64
	Model     string
	Plan      string
	PlanAt    int64
	Latest    [2]codexLatestDisk
	Credits   *codexCredits
	CreditsAt int64
}

type usageSeenDisk struct {
	H        uint64
	F        int32
	Min, Out int64
}

type usageScannerDisk struct {
	Root  string
	Files []usageFileDisk
	Seen  []usageSeenDisk
}

type usageStateFile struct {
	Version       int
	Claude, Codex usageScannerDisk
}

// snapshot copies the scanner for saving; pending readings must already be in the history.
func (s *usageScanner) snapshot() usageScannerDisk {
	d := usageScannerDisk{Root: s.root, Files: make([]usageFileDisk, 0, len(s.files))}
	idx := make(map[*usageFile]int32, len(s.files))
	for p, f := range s.files {
		fd := usageFileDisk{Path: p, Off: f.off, ModNs: f.mod.UnixNano(), PrevTotal: f.prevTotal, Model: f.model, Plan: f.plan, PlanAt: f.planAt,
			Credits: f.credits, CreditsAt: f.creditsAt, Buckets: make([]usageBucketDisk, len(f.buckets))}
		for i, b := range f.buckets {
			fd.Buckets[i] = usageBucketDisk{Min: b.min, Model: b.model, Tok: b.tok, Cost: b.cost, N: b.n}
		}
		for i, l := range f.latest {
			fd.Latest[i] = codexLatestDisk{W: l.w, At: l.at}
		}
		if fh, err := os.Open(p); err == nil {
			if info, err := fh.Stat(); err == nil {
				fd.Mark = markFile(fh, info, f.off)
			}
			fh.Close()
		}
		idx[f] = int32(len(d.Files))
		d.Files = append(d.Files, fd)
	}
	d.Seen = make([]usageSeenDisk, 0, len(s.seen))
	for h, v := range s.seen {
		if i, ok := idx[v.f]; ok {
			d.Seen = append(d.Seen, usageSeenDisk{H: h, F: i, Min: v.min, Out: v.out})
		}
	}
	return d
}

// restore loads d into an empty scanner; it keeps nothing when a saved file was replaced or truncated.
func (s *usageScanner) restore(d usageScannerDisk) bool {
	if d.Root != s.root {
		return false
	}
	files := make(map[string]*usageFile, len(d.Files))
	ptrs := make([]*usageFile, len(d.Files))
	for i, fd := range d.Files {
		if fh, err := os.Open(fd.Path); err == nil {
			info, err := fh.Stat()
			ok := err == nil && fd.Mark.valid(fh, info, fd.Off)
			fh.Close()
			if !ok {
				return false
			}
		}
		f := &usageFile{off: fd.Off, mod: time.Unix(0, fd.ModNs), prevTotal: fd.PrevTotal, model: fd.Model, plan: fd.Plan, planAt: fd.PlanAt,
			credits: fd.Credits, creditsAt: fd.CreditsAt, buckets: make([]usageBucket, len(fd.Buckets))}
		for j, b := range fd.Buckets {
			f.buckets[j] = usageBucket{min: b.Min, model: b.Model, tok: b.Tok, cost: b.Cost, n: b.N}
		}
		for j, l := range fd.Latest {
			f.latest[j] = codexLatest{w: l.W, at: l.At}
		}
		files[fd.Path], ptrs[i] = f, f
	}
	seen := make(map[uint64]usageSeen, len(d.Seen))
	for _, v := range d.Seen {
		if v.F < 0 || int(v.F) >= len(ptrs) {
			return false
		}
		seen[v.H] = usageSeen{f: ptrs[v.F], min: v.Min, out: v.Out}
	}
	s.files, s.seen = files, seen
	return true
}

func (u *usageLimits) loadState() {
	if u.statePath == "" {
		return
	}
	var sf usageStateFile
	if err := readGob(u.statePath, &sf); err != nil || sf.Version != usageStateVersion {
		return
	}
	if !u.claude.restore(sf.Claude) || !u.codex.restore(sf.Codex) {
		u.claude.files, u.claude.seen = map[string]*usageFile{}, map[uint64]usageSeen{}
		u.codex.files, u.codex.seen = map[string]*usageFile{}, map[uint64]usageSeen{}
	}
}

// saveState writes the scan positions; caller holds u.mu.
func (u *usageLimits) saveState() {
	if u.statePath == "" || len(u.codex.pending) > 0 {
		return
	}
	sf := usageStateFile{Version: usageStateVersion, Claude: u.claude.snapshot(), Codex: u.codex.snapshot()}
	if err := writeGobAtomic(u.statePath, sf); err != nil {
		log.Printf("scanpos: usage state not saved: %v", err)
		return
	}
	u.savedAt = u.now()
}

// usageStatePath keeps the scan positions next to the usage history.
func usageStatePath(histPath string) string {
	if histPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(histPath), "scanpos-usage.gob")
}
