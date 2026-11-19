package main

import (
	"context"
	"path/filepath"
	"testing"
)

// L4: the file cache keeps at most orchFilesCwds folders.
func TestComposerFileCacheBounded(t *testing.T) {
	for i := 0; i < orchFilesCwds+8; i++ {
		d := t.TempDir()
		writeT(t, filepath.Join(d, "f.txt"), "x\n")
		listFiles(d)
	}
	orchFileCache.Lock()
	n := len(orchFileCache.m)
	orchFileCache.Unlock()
	if n > orchFilesCwds {
		t.Fatalf("cache holds %d folders", n)
	}
}

// L4: the walk stops at its deadline.
func TestComposerWalkStopsAtDeadline(t *testing.T) {
	d := t.TempDir()
	for _, n := range []string{"a.txt", "b/c.txt", "b/d/e.txt"} {
		writeT(t, filepath.Join(d, n), "x\n")
	}
	if got := walkFiles(context.Background(), d); len(got) != 3 {
		t.Fatalf("walk %v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := walkFiles(ctx, d); len(got) != 0 {
		t.Fatalf("walk after the deadline %v", got)
	}
}
