package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// OrchHub fans committed events out to stream subscribers.
type OrchHub struct {
	mu   sync.Mutex
	subs map[int]chan OEvent
	next int
}

const orchSubBuffer = 1024

func newOrchHub() *OrchHub { return &OrchHub{subs: map[int]chan OEvent{}} }

// subscribe returns a channel that closes when the subscriber falls too far behind.
func (h *OrchHub) subscribe() (<-chan OEvent, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	id := h.next
	h.next++
	ch := make(chan OEvent, orchSubBuffer)
	h.subs[id] = ch
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if c, ok := h.subs[id]; ok {
			delete(h.subs, id)
			close(c)
		}
	}
}

func (h *OrchHub) publish(ev OEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, ch := range h.subs {
		select {
		case ch <- ev:
		default:
			delete(h.subs, id)
			close(ch)
		}
	}
}

var orchHeartbeat = 15 * time.Second

// serveStream replays events after ?after= from the DB, then streams live ones.
func serveOrchStream(store *OrchStore, w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > after {
			after = n
		}
	}
	live, cancel := store.hub.subscribe()
	defer cancel()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": orch stream\n\n")
	flusher.Flush()

	send := func(ev OEvent) bool {
		b, _ := json.Marshal(ev)
		if _, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", ev.Seq, b); err != nil {
			return false
		}
		return true
	}
	last := after
	for {
		batch, err := store.EventsAfter(last, 1000)
		if err != nil {
			return
		}
		for _, ev := range batch {
			if !send(ev) {
				return
			}
			last = ev.Seq
		}
		flusher.Flush()
		if len(batch) < 1000 {
			break
		}
	}

	tick := time.NewTicker(orchHeartbeat)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-live:
			if !ok {
				return // fell behind; the client reconnects with its last seq
			}
			if ev.Seq <= last {
				continue
			}
			if !send(ev) {
				return
			}
			last = ev.Seq
			flusher.Flush()
		case <-tick.C:
			if _, err := fmt.Fprint(w, ": hb\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
