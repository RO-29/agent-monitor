package main

import "sync"

// V3Event is a WebSocket message for the v3 UI: {kind, ...payload fields}.
type V3Event map[string]any

type v3BusT struct {
	mu        sync.Mutex
	listeners map[int]func(V3Event)
	next      int
}

var v3Bus = &v3BusT{listeners: map[int]func(V3Event){}}

func (b *v3BusT) subscribe(l func(V3Event)) func() {
	b.mu.Lock()
	id := b.next
	b.next++
	b.listeners[id] = l
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		delete(b.listeners, id)
		b.mu.Unlock()
	}
}

// publishV3 sends {kind: kind, <key>: payload} to every connected client.
func publishV3(kind, key string, payload any) {
	e := V3Event{"kind": kind}
	if key != "" {
		e[key] = payload
	}
	v3Bus.mu.Lock()
	ls := make([]func(V3Event), 0, len(v3Bus.listeners))
	for _, l := range v3Bus.listeners {
		ls = append(ls, l)
	}
	v3Bus.mu.Unlock()
	for _, l := range ls {
		l(e)
	}
}
