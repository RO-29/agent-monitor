package main

import (
	"log"
	"math"
	"os"
	"runtime/debug"
	"runtime/metrics"
	"sync"
	"time"
)

// Defaults when GOMEMLIMIT or GOGC is unset: a 160 MiB soft limit, and a heap goal of 1.5x the live heap.
const (
	defaultMemLimit  = 160 << 20
	defaultGCPercent = 50
)

// startMemGuard sets the GC defaults and evicts idle caches every minute.
func startMemGuard() {
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(defaultMemLimit)
	}
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(defaultGCPercent)
	}
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		var freed time.Time
		for now := range t.C {
			warnLiveHeap(now, debug.SetMemoryLimit(-1))
			tr, p := evictIdleTraces(now)
			w := evictIdleWatched(now)
			if tr+p+w == 0 {
				continue
			}
			log.Printf("mem: evicted %d idle trace(s), %d parser(s), %d watched", tr, p, w)
			// The scavenger returns freed pages slowly; after an eviction give them back at once, at most once a minute.
			if now.Sub(freed) >= 55*time.Second {
				debug.FreeOSMemory()
				freed = now
			}
		}
	}()
}

// heapWarnEvery spaces the live-heap warnings; above the soft limit the GC runs often and burns CPU.
const heapWarnEvery = time.Hour

var heapWarn struct {
	sync.Mutex
	at time.Time
}

// warnLiveHeap logs when the live heap passes 80 % of limit, at most once per heapWarnEvery.
func warnLiveHeap(now time.Time, limit int64) bool {
	if limit <= 0 || limit == math.MaxInt64 {
		return false
	}
	sample := []metrics.Sample{{Name: "/gc/heap/live:bytes"}}
	metrics.Read(sample)
	if sample[0].Value.Kind() != metrics.KindUint64 {
		return false
	}
	live := sample[0].Value.Uint64()
	if live*5 < uint64(limit)*4 {
		return false
	}
	heapWarn.Lock()
	defer heapWarn.Unlock()
	if !heapWarn.at.IsZero() && now.Sub(heapWarn.at) < heapWarnEvery {
		return false
	}
	heapWarn.at = now
	log.Printf("mem: live heap %d MiB is %d%% of the %d MiB soft limit; the GC may use much CPU", live>>20, live*100/uint64(limit), limit>>20)
	return true
}
