package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func gzipServe(h http.HandlerFunc, accept string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	if accept != "" {
		r.Header.Set("Accept-Encoding", accept)
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	gzipJSON(h).ServeHTTP(w, r)
	return w
}

func TestGzipJSONCompressesLargeJSON(t *testing.T) {
	big := map[string]any{"threads": strings.Repeat("thread row ", 500)}
	h := func(w http.ResponseWriter, r *http.Request) { writeJSON(w, big) }
	w := gzipServe(h, "gzip, deflate, br", nil)
	if w.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("not compressed: %v", w.Header())
	}
	zr, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(zr)
	plain := gzipServe(h, "", nil)
	if plain.Header().Get("Content-Encoding") != "" || !bytes.Equal(got, plain.Body.Bytes()) {
		t.Fatalf("gzip body differs from plain body (%d vs %d bytes)", len(got), plain.Body.Len())
	}
	if w.Body.Len() >= plain.Body.Len() {
		t.Fatalf("compressed %d >= plain %d", w.Body.Len(), plain.Body.Len())
	}
}

func TestGzipJSONPassesThroughOthers(t *testing.T) {
	small := func(w http.ResponseWriter, r *http.Request) {
		writeJSONStatus(w, http.StatusTeapot, map[string]any{"ok": true})
	}
	if w := gzipServe(small, "gzip", nil); w.Header().Get("Content-Encoding") != "" || w.Code != http.StatusTeapot || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("small body: code %d enc %q body %q", w.Code, w.Header().Get("Content-Encoding"), w.Body.String())
	}
	bin := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte(strings.Repeat("<p>x</p>", 500)))
	}
	if w := gzipServe(bin, "gzip", nil); w.Header().Get("Content-Encoding") != "" || w.Body.Len() != 4000 {
		t.Fatalf("binary compressed or cut: %q %d", w.Header().Get("Content-Encoding"), w.Body.Len())
	}
	sse := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: 1\n\n"))
		w.(http.Flusher).Flush()
	}
	if w := gzipServe(sse, "gzip", nil); w.Header().Get("Content-Encoding") != "" || w.Body.String() != "data: 1\n\n" || !w.Flushed {
		t.Fatalf("sse: enc %q body %q flushed %v", w.Header().Get("Content-Encoding"), w.Body.String(), w.Flushed)
	}
	if w := gzipServe(func(w http.ResponseWriter, r *http.Request) { writeJSON(w, strings.Repeat("a", 4000)) }, "gzip;q=0", nil); w.Header().Get("Content-Encoding") != "" {
		t.Fatal("q=0 must not compress")
	}
}

func TestGzipCompressesWebAssets(t *testing.T) {
	for _, ct := range []string{"text/javascript; charset=utf-8", "application/javascript", "text/css; charset=utf-8",
		"text/html; charset=utf-8", "image/svg+xml", "text/plain; charset=utf-8"} {
		body := strings.Repeat("function x() { return 1 }\n", 200)
		h := func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", ct)
			_, _ = w.Write([]byte(body))
		}
		w := gzipServe(h, "gzip", nil)
		if w.Header().Get("Content-Encoding") != "gzip" {
			t.Fatalf("%s not compressed", ct)
		}
		zr, err := gzip.NewReader(w.Body)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := io.ReadAll(zr); string(got) != body {
			t.Fatalf("%s: body differs", ct)
		}
	}
	part := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte(strings.Repeat("a", 4000)))
	}
	if w := gzipServe(part, "gzip", nil); w.Header().Get("Content-Encoding") != "" || w.Code != http.StatusPartialContent {
		t.Fatal("206 must pass through")
	}
}

func TestGzipZeroQualityVariants(t *testing.T) {
	h := func(w http.ResponseWriter, r *http.Request) { writeJSON(w, strings.Repeat("a", 4000)) }
	for _, ae := range []string{"gzip;q=0.0", "gzip; q=0.000", "gzip;Q=0", "br, gzip;q=0.00"} {
		if w := gzipServe(h, ae, nil); w.Header().Get("Content-Encoding") != "" {
			t.Fatalf("%q must not compress", ae)
		}
	}
	if w := gzipServe(h, "gzip;q=0.5", nil); w.Header().Get("Content-Encoding") != "gzip" {
		t.Fatal("q=0.5 must compress")
	}
}

func TestGzipPanicSendsNoPartialBody(t *testing.T) {
	for _, n := range []int{200, 5000} {
		h := func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("[" + strings.Repeat(`"x",`, n/4)))
			panic("boom")
		}
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		r.Header.Set("Accept-Encoding", "gzip")
		func() {
			defer func() {
				if recover() == nil {
					t.Fatal("panic was swallowed")
				}
			}()
			gzipJSON(http.HandlerFunc(h)).ServeHTTP(w, r)
		}()
		if w.Header().Get("Content-Encoding") == "gzip" {
			if _, err := io.ReadAll(mustGzip(t, w.Body)); err == nil {
				t.Fatalf("%d: cut gzip body reads as complete", n)
			}
		} else if w.Body.Len() != 0 {
			t.Fatalf("%d: partial body of %d bytes sent", n, w.Body.Len())
		}
	}
}

func mustGzip(t *testing.T, r io.Reader) *gzip.Reader {
	t.Helper()
	zr, err := gzip.NewReader(r)
	if err != nil {
		t.Fatal(err)
	}
	return zr
}

func TestGzipInformationalStatus(t *testing.T) {
	srv := httptest.NewServer(gzipJSON(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", "</a.js>; rel=preload")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`"` + strings.Repeat("a", 4000) + `"`))
	})))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d, want 201", resp.StatusCode)
	}
}
