package main

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
)

// liveConn is one in-flight request made with a device token; revoke closes it.
type liveConn struct {
	device string
	term   bool
	cancel context.CancelFunc
	mu     sync.Mutex
	conn   net.Conn
	closed bool
}

func (c *liveConn) close() {
	c.cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	if c.conn != nil {
		_ = c.conn.Close()
	}
}

type liveConnSet struct {
	mu sync.Mutex
	m  map[*liveConn]struct{}
}

var liveConns = &liveConnSet{m: map[*liveConn]struct{}{}}

type liveConnKey struct{}

func isTermPath(p string) bool {
	return p == "/api/term" || (strings.HasPrefix(p, "/api/orch/threads/") && strings.HasSuffix(p, "/term"))
}

// track runs next with a cancellable context and a hijack-aware writer, registered under the device.
func (s *liveConnSet) track(device string, w http.ResponseWriter, r *http.Request, next http.Handler) {
	ctx, cancel := context.WithCancel(r.Context())
	c := &liveConn{device: device, term: isTermPath(r.URL.Path), cancel: cancel}
	s.mu.Lock()
	s.m[c] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.m, c)
		s.mu.Unlock()
		cancel()
	}()
	next.ServeHTTP(&trackedWriter{ResponseWriter: w, c: c}, r.WithContext(context.WithValue(ctx, liveConnKey{}, c)))
}

func (s *liveConnSet) closeWhere(r *http.Request, match func(*liveConn) bool) int {
	self, _ := r.Context().Value(liveConnKey{}).(*liveConn)
	s.mu.Lock()
	var hit []*liveConn
	for c := range s.m {
		if c != self && match(c) {
			hit = append(hit, c)
		}
	}
	s.mu.Unlock()
	for _, c := range hit {
		c.close()
	}
	return len(hit)
}

// closeDevice ends every live request of a device except the caller's own.
func (s *liveConnSet) closeDevice(r *http.Request, id string) int {
	return s.closeWhere(r, func(c *liveConn) bool { return c.device == id })
}

// closeRemoteTerms ends terminals opened with a device token (remote terminals need one).
func (s *liveConnSet) closeRemoteTerms(r *http.Request) int {
	return s.closeWhere(r, func(c *liveConn) bool { return c.term })
}

type trackedWriter struct {
	http.ResponseWriter
	c *liveConn
}

func (w *trackedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *trackedWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *trackedWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("hijack not supported")
	}
	conn, rw, err := h.Hijack()
	if err != nil {
		return nil, nil, err
	}
	w.c.mu.Lock()
	defer w.c.mu.Unlock()
	if w.c.closed {
		conn.Close()
		return nil, nil, errors.New("device revoked")
	}
	w.c.conn = conn
	return conn, rw, nil
}

// onDeviceRevoked closes the device's live connections and deletes its push subscriptions.
func onDeviceRevoked(r *http.Request, id string) {
	liveConns.closeDevice(r, id)
	if notifier != nil {
		_ = notifier.removeDeviceSubs(id)
	}
}
