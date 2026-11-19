package main

import (
	"os"
	"strings"
	"sync"
	"time"
)

// TailFile invokes onLine for every newline-terminated chunk appended to path
// after the call returns. Survives truncation by detecting size shrink.
// Stops when the returned closer is invoked. All tails share one polling
// goroutine; a file that has been quiet is checked less often.
func TailFile(path string, onLine func(string)) func() {
	t := &tailEntry{path: path, onLine: onLine, lastGrow: time.Now()}
	if st, err := os.Stat(path); err == nil {
		t.pos = st.Size()
		t.lastGrow = st.ModTime()
	}
	tailer.mu.Lock()
	tailer.entries[t] = struct{}{}
	tailer.mu.Unlock()
	tailer.once.Do(func() { go tailLoop() })
	var once sync.Once
	return func() {
		once.Do(func() {
			tailer.mu.Lock()
			delete(tailer.entries, t)
			tailer.mu.Unlock()
		})
	}
}

type tailEntry struct {
	path     string
	onLine   func(string)
	pos      int64
	leftover string
	lastGrow time.Time
	next     time.Time
}

var tailer = struct {
	mu      sync.Mutex
	once    sync.Once
	entries map[*tailEntry]struct{}
}{entries: map[*tailEntry]struct{}{}}

// tailInterval keeps live files at the original 1 s cadence and backs off to
// 30 s for files quiet for half an hour (a resumed session wakes within 30 s).
func tailInterval(quiet time.Duration) time.Duration {
	return min(max(quiet/60, time.Second), 30*time.Second)
}

func tailLoop() {
	tk := time.NewTicker(1 * time.Second)
	defer tk.Stop()
	for now := range tk.C {
		tailer.mu.Lock()
		due := make([]*tailEntry, 0, len(tailer.entries))
		for t := range tailer.entries {
			if !now.Before(t.next) {
				due = append(due, t)
			}
		}
		tailer.mu.Unlock()
		for _, t := range due {
			tailer.mu.Lock()
			_, live := tailer.entries[t]
			tailer.mu.Unlock()
			if live {
				t.poll(now)
			}
		}
	}
}

func (t *tailEntry) poll(now time.Time) {
	defer func() { t.next = now.Add(tailInterval(now.Sub(t.lastGrow))) }()
	st, err := os.Stat(t.path)
	if err != nil {
		return
	}
	if st.Size() < t.pos {
		t.pos = 0 // truncated/rotated
		t.leftover = ""
	}
	if st.Size() == t.pos {
		return
	}
	t.lastGrow = now
	f, err := os.Open(t.path)
	if err != nil {
		return
	}
	if _, err := f.Seek(t.pos, 0); err != nil {
		f.Close()
		return
	}
	buf := make([]byte, st.Size()-t.pos)
	read, _ := f.Read(buf)
	f.Close()
	t.pos += int64(read)
	text := t.leftover + string(buf[:read])
	lines := strings.Split(text, "\n")
	// Last element is the partial line (no trailing newline yet) — save it.
	t.leftover = lines[len(lines)-1]
	for _, line := range lines[:len(lines)-1] {
		if line != "" {
			t.onLine(line)
		}
	}
}
