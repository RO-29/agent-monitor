package main

import (
	"os"
	"strings"
	"sync"
	"time"
)

// TailFile invokes onLine for every newline-terminated chunk appended to path
// after the call returns. Survives truncation by detecting size shrink.
// Stops when the returned closer is invoked. Pure polling — no fsnotify dep,
// which keeps the binary lean and matches the original 1-second poll cadence.
func TailFile(path string, onLine func(string)) func() {
	stopCh := make(chan struct{})
	var once sync.Once

	go func() {
		var pos int64
		if st, err := os.Stat(path); err == nil {
			pos = st.Size()
		}

		t := time.NewTicker(1 * time.Second)
		defer t.Stop()
		var leftover string
		for {
			select {
			case <-stopCh:
				return
			case <-t.C:
			}
			st, err := os.Stat(path)
			if err != nil {
				continue
			}
			if st.Size() < pos {
				pos = 0 // truncated/rotated
				leftover = ""
			}
			if st.Size() == pos {
				continue
			}
			f, err := os.Open(path)
			if err != nil {
				continue
			}
			if _, err := f.Seek(pos, 0); err != nil {
				f.Close()
				continue
			}
			n := st.Size() - pos
			buf := make([]byte, n)
			read, _ := f.Read(buf)
			f.Close()
			pos += int64(read)
			text := leftover + string(buf[:read])
			lines := strings.Split(text, "\n")
			// Last element is the partial line (no trailing newline yet) — save it.
			leftover = lines[len(lines)-1]
			for _, line := range lines[:len(lines)-1] {
				if line != "" {
					onLine(line)
				}
			}
		}
	}()

	return func() { once.Do(func() { close(stopCh) }) }
}
