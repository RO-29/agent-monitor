package main

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSpanIOMapRoundTrip(t *testing.T) {
	m := spanIOMap{}
	long := strings.Repeat("héllo wörld ", 3000)
	m.set("a", SpanIO{Args: "short", Result: long})
	if m["a"].result.z == nil {
		t.Fatal("long result not compressed")
	}
	if got := m.get("a"); got.Args != "short" || got.Result != long {
		t.Fatalf("round trip mismatch: args=%q len(result)=%d", got.Args, len(got.Result))
	}
	m.setResult("a", "done")
	if m.args("a") != "short" || m.result("a") != "done" {
		t.Fatalf("setResult: %+v", m.get("a"))
	}
	if (m.get("missing") != SpanIO{}) {
		t.Fatal("missing id should be empty")
	}
}

func TestPrunedTraceKeepsShippedAndServesFullIO(t *testing.T) {
	_, sess := writeClaudeFixture(t)
	sess.ID = "claude:pruned-io"
	pruned := buildClaudeTrace(sess)
	p := newClaudeTraceParser(sess)
	p.tr.ioAll = true
	p.advance()
	full := p.snapshot(sess)
	if len(full.io) == 0 || len(pruned.io) >= len(full.io) {
		t.Fatalf("io entries pruned=%d full=%d", len(pruned.io), len(full.io))
	}
	if !reflect.DeepEqual(traceShipped(pruned), traceShipped(full)) {
		t.Fatalf("shipped differs\n got %+v\nwant %+v", traceShipped(pruned), traceShipped(full))
	}
	for _, sp := range full.Spans {
		if got, want := spanIOFor(sess, pruned, sp.ID), full.io.get(sp.ID); got != want {
			t.Fatalf("span %s io %+v, want %+v", sp.ID, got, want)
		}
	}
}

func TestEvictIdleTracesKeepsDerived(t *testing.T) {
	_, sess := writeClaudeFixture(t)
	sess.ID = "claude:evict-idle"
	t.Cleanup(func() {
		traceCache.mu.Lock()
		delete(traceCache.m, sess.ID)
		traceCache.mu.Unlock()
		claudeParsers.mu.Lock()
		delete(claudeParsers.m, sess.ID)
		claudeParsers.mu.Unlock()
	})
	tr := buildTrace(sess)
	if tr == nil {
		t.Fatal("no trace")
	}
	before := *derivedFor(sess.ID)
	now := time.Now()
	if evictIdleTraces(now); peekTrace(sess.ID) == nil {
		t.Fatal("fresh trace evicted")
	}
	// Subagent files are fresh, so the parser outlives the idle time while its trace is cached.
	claudeParsers.mu.Lock()
	claudeParsers.m[sess.ID].lastUse = now.Add(-time.Hour)
	claudeParsers.mu.Unlock()
	hasParser := func() bool {
		claudeParsers.mu.Lock()
		defer claudeParsers.mu.Unlock()
		return claudeParsers.m[sess.ID] != nil
	}
	if evictIdleTraces(now); !hasParser() {
		t.Fatal("parser with live subagents evicted while its trace is cached")
	}
	if evictIdleTraces(now.Add(traceIdleTTL + time.Minute)); hasParser() {
		t.Fatal("idle parser kept after its trace left")
	}
	if peekTrace(sess.ID) != nil {
		t.Fatal("trace still cached")
	}
	after := derivedFor(sess.ID)
	if after == nil || !reflect.DeepEqual(after.shipped, before.shipped) || after.errors != before.errors ||
		after.errorsOpen != before.errorsOpen || !reflect.DeepEqual(after.calls, before.calls) || !reflect.DeepEqual(after.segs, before.segs) {
		t.Fatalf("derived changed after eviction: %+v vs %+v", after, before)
	}
	if again := buildTrace(sess); again == nil || len(again.Spans) != len(tr.Spans) {
		t.Fatal("rebuild after eviction differs")
	}
}
