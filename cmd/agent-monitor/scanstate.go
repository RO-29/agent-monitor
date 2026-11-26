package main

import (
	"bufio"
	"bytes"
	"encoding/gob"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

var shutdownHooks struct {
	mu sync.Mutex
	fn []func()
}

// onShutdown registers fn to run once when the daemon stops.
func onShutdown(fn func()) {
	shutdownHooks.mu.Lock()
	shutdownHooks.fn = append(shutdownHooks.fn, fn)
	shutdownHooks.mu.Unlock()
}

func runShutdownHooks() {
	shutdownHooks.mu.Lock()
	fns := shutdownHooks.fn
	shutdownHooks.fn = nil
	shutdownHooks.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

// scanHeadLen is how much of a file's start must be unchanged for a saved position to stay valid.
const scanHeadLen = 4096

// fileMark identifies the file a saved scan position belongs to.
type fileMark struct {
	Dev, Ino uint64
	HeadLen  int
	Head     uint64
}

// markFile records f's identity and a hash of its first min(scanHeadLen, upto) bytes.
func markFile(f *os.File, info os.FileInfo, upto int64) fileMark {
	m := fileMark{HeadLen: int(min(upto, scanHeadLen))}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		m.Dev, m.Ino = uint64(st.Dev), st.Ino
	}
	m.Head, _ = headHash(f, m.HeadLen)
	return m
}

func headHash(f *os.File, n int) (uint64, bool) {
	buf := make([]byte, n)
	if k, _ := f.ReadAt(buf, 0); k != n {
		return 0, false
	}
	h := fnv.New64a()
	h.Write(buf)
	return h.Sum64(), true
}

// valid reports whether f is the same file, not truncated below off, with the same first bytes.
func (m fileMark) valid(f *os.File, info os.FileInfo, off int64) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || uint64(st.Dev) != m.Dev || st.Ino != m.Ino || info.Size() < off {
		return false
	}
	h, ok := headHash(f, m.HeadLen)
	return ok && h == m.Head
}

// foldLines passes r's lines to fn exactly as bufio.Scanner splits them, and returns the length of the newline-terminated ones.
func foldLines(r io.Reader, maxTok int, fn func(line []byte, terminated bool)) int64 {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxTok)
	adv, term := 0, false
	sc.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		a, tok, err := bufio.ScanLines(data, atEOF)
		if tok != nil {
			adv, term = a, a > 0 && data[a-1] == '\n'
		}
		return a, tok, err
	})
	var done int64
	for sc.Scan() {
		fn(sc.Bytes(), term)
		if term {
			done += int64(adv)
		}
	}
	return done
}

// writeGobAtomic writes v to path through a temp file and a rename.
func writeGobAtomic(path string, v any) error {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(v); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readGob(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return gob.NewDecoder(bufio.NewReader(f)).Decode(v)
}
