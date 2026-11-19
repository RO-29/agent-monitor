package main

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"
	"time"
)

func TestWarnLiveHeapOnceNearLimit(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		heapWarn.Lock()
		heapWarn.at = time.Time{}
		heapWarn.Unlock()
	})
	now := time.Now()
	if warnLiveHeap(now, 1<<40) {
		t.Fatal("warned far below the limit")
	}
	if !warnLiveHeap(now, 1) || !strings.Contains(buf.String(), "soft limit") {
		t.Fatalf("no warning above the limit: %q", buf.String())
	}
	if warnLiveHeap(now.Add(time.Minute), 1) {
		t.Fatal("warned twice inside the rate limit")
	}
	if !warnLiveHeap(now.Add(heapWarnEvery), 1) {
		t.Fatal("no warning after the rate limit")
	}
}
