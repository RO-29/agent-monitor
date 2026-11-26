package main

import (
	"bufio"
	"compress/gzip"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
)

// gzipMin is the smallest body that gzipJSON compresses.
const gzipMin = 1024

// gzipTypes are the text media types worth compressing (API JSON and the embedded web assets).
var gzipTypes = map[string]bool{"application/json": true, "text/javascript": true, "application/javascript": true,
	"text/css": true, "text/html": true, "image/svg+xml": true, "text/plain": true}

var gzipWriters = sync.Pool{New: func() any { w, _ := gzip.NewWriterLevel(nil, gzip.DefaultCompression); return w }}

// gzipJSON compresses text bodies of gzipMin bytes or more for clients that accept gzip; streams and upgrades pass through.
func gzipJSON(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead || !acceptsGzip(r) || websocket.IsWebSocketUpgrade(r) ||
			strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipWriter{ResponseWriter: w}
		defer func() {
			if p := recover(); p != nil {
				gw.abort() // never close a cut body as if it were complete
				panic(p)
			}
			gw.finish()
		}()
		next.ServeHTTP(gw, r)
	})
}

func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		enc, q, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(enc), "gzip") {
			continue
		}
		k, v, ok := strings.Cut(strings.ReplaceAll(q, " ", ""), "=")
		if !ok || !strings.EqualFold(k, "q") {
			return true
		}
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			return true
		}
	}
	return false
}

// gzipWriter holds the first gzipMin bytes, then picks gzip or plain from the content type.
type gzipWriter struct {
	http.ResponseWriter
	status  int
	buf     []byte
	decided bool
	zw      *gzip.Writer
}

func (g *gzipWriter) Unwrap() http.ResponseWriter { return g.ResponseWriter }

func (g *gzipWriter) WriteHeader(code int) {
	if g.decided || code >= 100 && code < 200 {
		g.ResponseWriter.WriteHeader(code) // 1xx is informational, not the final status
		return
	}
	if g.status == 0 {
		g.status = code
	}
}

func (g *gzipWriter) Write(p []byte) (int, error) {
	if g.decided {
		if g.zw != nil {
			return g.zw.Write(p)
		}
		return g.ResponseWriter.Write(p)
	}
	g.buf = append(g.buf, p...)
	if len(g.buf) >= gzipMin {
		if err := g.decide(true); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func (g *gzipWriter) decide(big bool) error {
	g.decided = true
	h := g.Header()
	h.Add("Vary", "Accept-Encoding")
	ct := h.Get("Content-Type")
	if ct == "" && len(g.buf) > 0 {
		ct = http.DetectContentType(g.buf)
	}
	mt, _, _ := strings.Cut(ct, ";")
	compress := big && gzipTypes[strings.ToLower(strings.TrimSpace(mt))] && h.Get("Content-Encoding") == "" &&
		g.status != http.StatusNoContent && g.status != http.StatusNotModified && g.status != http.StatusPartialContent
	if compress {
		h.Del("Content-Length")
		h.Set("Content-Encoding", "gzip")
	}
	if g.status != 0 {
		g.ResponseWriter.WriteHeader(g.status)
	}
	buf := g.buf
	g.buf = nil
	if compress {
		g.zw = gzipWriters.Get().(*gzip.Writer)
		g.zw.Reset(g.ResponseWriter)
		_, err := g.zw.Write(buf)
		return err
	}
	if len(buf) == 0 {
		return nil
	}
	_, err := g.ResponseWriter.Write(buf)
	return err
}

func (g *gzipWriter) Flush() {
	if !g.decided {
		_ = g.decide(false)
	}
	if g.zw != nil {
		_ = g.zw.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (g *gzipWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := g.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("hijack not supported")
	}
	g.decided = true
	return h.Hijack()
}

// abort drops what a panicking handler left: the buffer is never sent and the gzip stream gets no trailer.
func (g *gzipWriter) abort() {
	g.decided, g.buf, g.zw = true, nil, nil
}

func (g *gzipWriter) finish() {
	if !g.decided {
		_ = g.decide(false)
	}
	if g.zw != nil {
		_ = g.zw.Close()
		g.zw.Reset(nil)
		gzipWriters.Put(g.zw)
		g.zw = nil
	}
}
