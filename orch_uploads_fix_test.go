package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestTrimPartialRuneLongInput(t *testing.T) {
	s := []byte(strings.Repeat("a", 10) + "हि")
	for cut := 1; cut < len("हि"); cut++ {
		b := s[:len(s)-cut]
		if got := trimPartialRune(b); !utf8.Valid(got) {
			t.Fatalf("cut %d: %q not trimmed", cut, got)
		}
	}
	if got := trimPartialRune(s); len(got) != len(s) {
		t.Fatal("a full rune was trimmed")
	}
}

// A text upload with a multibyte rune across byte 512 must stay readable after it is saved.
func TestUploadRuneAcrossSniffWindow(t *testing.T) {
	e := newOrchEnv(t)
	for _, word := range []string{"नमस्ते ", "漢字テスト", "😀"} {
		for pad := 0; pad < 4; pad++ {
			body := []byte(strings.Repeat("x", 510-pad) + strings.Repeat(word, 200))
			img, err := e.m.SaveUploadTyped("new", "notes.txt", "text/plain", bytes.NewReader(body))
			if err != nil {
				t.Fatalf("%q pad %d: save: %v", word, pad, err)
			}
			if _, err := e.m.Upload(img.ID); err != nil {
				t.Fatalf("%q pad %d: saved upload not found: %v", word, pad, err)
			}
		}
	}
}

func TestUploadMarkupInTextFiles(t *testing.T) {
	e := newOrchEnv(t)
	for _, c := range []struct{ name, ctype, body, mime string }{
		{"a.md", "text/markdown", "<!-- note -->\n# Title\n", "text/markdown"},
		{"b.md", "", "<div align=\"center\">\n\nx\n</div>\n", "text/markdown"},
		{"c.md", "application/octet-stream", "<h1>Title</h1>\n", "text/markdown"},
		{"d.txt", "text/plain", "<p class=x>para</p>\n", "text/plain"},
		{"e.txt", "", "<a href=x>link</a>\n", "text/plain"},
		{"f.csv", "text/csv", "<?xml version=\"1.0\"?>,b\n", "text/csv"},
	} {
		img, err := e.m.SaveUploadTyped("new", c.name, c.ctype, strings.NewReader(c.body))
		if err != nil || img.Mime != c.mime {
			t.Fatalf("%s: %v %+v", c.name, err, img)
		}
		if got, err := e.m.Upload(img.ID); err != nil || got.Mime != c.mime {
			t.Fatalf("%s: Upload %v %+v", c.name, err, got)
		}
	}
	for _, c := range []struct{ name, ctype, body string }{
		{"x.svg", "image/svg+xml", "<?xml version=\"1.0\"?><svg/>"},
		{"blob", "", "<div>x</div>"},
		{"pic.png", "image/png", "<!-- x --> text"},
	} {
		if _, err := e.m.SaveUploadTyped("new", c.name, c.ctype, strings.NewReader(c.body)); err == nil {
			t.Fatalf("%s accepted", c.name)
		}
	}
}

func TestUploadScanMatchesWholeCheck(t *testing.T) {
	cases := [][]byte{
		[]byte("plain ascii"), []byte("नमस्ते दुनिया 漢字 😀"), {0xff, 'a'}, []byte("a\x00b"),
		append([]byte("ok "), 0xe0, 0xa4), append([]byte("bad "), 0xe0, 'x', 'y'), {0x80, 0x80},
	}
	for _, c := range cases {
		want := bytes.IndexByte(c, 0) < 0 && utf8.Valid(trimPartialRune(c))
		for step := 1; step <= len(c); step++ {
			s := &uploadScan{}
			for i := 0; i < len(c); i += step {
				s.Write(c[i:min(len(c), i+step)])
			}
			if !s.bad != want || s.size != int64(len(c)) {
				t.Fatalf("%q step %d: text %v want %v", c, step, !s.bad, want)
			}
		}
	}
}

func TestJSONFileValidMatchesJSONValid(t *testing.T) {
	dir := t.TempDir()
	for i, c := range []string{`{"a":1}`, `[1,2,{"b":[true,null]}]`, `"s"`, `1e999`, ` {"a":1} `, `{"a":1} x`, `{"a" 1}`,
		`[1,]`, `{"a":1}{}`, `[`, `{"a":"é"}`, `12345678901234567890123`, `nul`} {
		p := filepath.Join(dir, "f.json")
		os.WriteFile(p, []byte(c), 0o600)
		if got, want := jsonFileValid(p), json.Valid([]byte(c)); got != want {
			t.Fatalf("%d %q: got %v want %v", i, c, got, want)
		}
	}
}

// The body streams to disk: saving 8 MB must not allocate a buffer of that size.
func TestUploadStreamsToDisk(t *testing.T) {
	e := newOrchEnv(t)
	const n = 8 << 20
	src := io.LimitReader(repeatReader("line of text नमस्ते\n"), n)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	img, err := e.m.SaveUploadTyped("new", "big.txt", "text/plain", src)
	runtime.ReadMemStats(&after)
	if err != nil || img.Size != n {
		t.Fatalf("save: %v %+v", err, img)
	}
	if got := after.TotalAlloc - before.TotalAlloc; got > 3<<20 {
		t.Fatalf("saving %d bytes allocated %d bytes", n, got)
	}
	if left, _ := filepath.Glob(filepath.Join(e.m.uploadsDir(), orchUploadTmp+"*")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

type repeatReader string

func (r repeatReader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		n += copy(p[n:], r)
	}
	return n, nil
}

func TestUploadConcurrencyCap(t *testing.T) {
	e := newOrchEnv(t)
	prev := orchUploadWait
	orchUploadWait = 50 * time.Millisecond
	t.Cleanup(func() { orchUploadWait = prev })
	for i := 0; i < cap(orchUploadSlots); i++ {
		orchUploadSlots <- struct{}{}
	}
	_, err := e.m.SaveUpload("new", "p.png", bytes.NewReader(onePixelPNG))
	for i := 0; i < cap(orchUploadSlots); i++ {
		<-orchUploadSlots
	}
	var he *orchHTTPError
	if err == nil || !errors.As(err, &he) || he.code != http.StatusTooManyRequests {
		t.Fatalf("upload with every slot busy: %v", err)
	}
	if _, err := e.m.SaveUpload("new", "p.png", bytes.NewReader(onePixelPNG)); err != nil {
		t.Fatalf("upload after slots freed: %v", err)
	}
}

func TestBigImagesGoAsPaths(t *testing.T) {
	small := orchImage{OAttachment: OAttachment{ID: "up_1", Mime: "image/png", Size: 1 << 20}, Path: "/a.png"}
	big := orchImage{OAttachment: OAttachment{ID: "up_2", Mime: "image/png", Size: orchModelImageMax + 1}, Path: "/b.png"}
	pics, files := splitUploads([]orchImage{small, big})
	if len(pics) != 1 || pics[0].ID != "up_1" || len(files) != 1 || files[0].ID != "up_2" {
		t.Fatalf("pics %v files %v", pics, files)
	}
}

func TestImageBase64FromDisk(t *testing.T) {
	p := filepath.Join(t.TempDir(), "p.png")
	os.WriteFile(p, onePixelPNG, 0o600)
	got, err := imageBase64(orchImage{Path: p})
	if err != nil {
		t.Fatal(err)
	}
	var want bytes.Buffer
	enc := base64.NewEncoder(base64.StdEncoding, &want)
	enc.Write(onePixelPNG)
	enc.Close()
	if got != want.String() {
		t.Fatalf("base64 differs")
	}
}

// A body over the limit in a skipped form field reports the real 25 MB limit.
func TestUploadTooLargeMessage(t *testing.T) {
	e := newOrchEnv(t)
	mux := http.NewServeMux()
	e.m.routes(mux)
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		fw, _ := mw.CreateFormField("note")
		io.Copy(fw, io.LimitReader(repeatReader("x"), orchUploadMax+(2<<20)))
		mw.Close()
		pw.Close()
	}()
	r := httptest.NewRequest("POST", "/api/orch/uploads", pr)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.RemoteAddr = "127.0.0.1:1"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	io.Copy(io.Discard, pr)
	if w.Code != http.StatusRequestEntityTooLarge || !strings.Contains(w.Body.String(), "25 MB") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

// Clients send several files at once and do not retry, so uploads past the cap wait for a slot.
func TestUploadParallelWaitsForSlot(t *testing.T) {
	e := newOrchEnv(t)
	mux := http.NewServeMux()
	e.m.routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	const n = 5
	gate := make(chan struct{})
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		pr, pw := io.Pipe()
		mw := multipart.NewWriter(pw)
		go func() {
			fw, _ := mw.CreateFormFile("file", "p.png")
			fw.Write(onePixelPNG[:8])
			<-gate
			fw.Write(onePixelPNG[8:])
			mw.Close()
			pw.Close()
		}()
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := http.Post(srv.URL+"/api/orch/uploads", mw.FormDataContentType(), pr)
			if err != nil {
				t.Error(err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			codes[i] = resp.StatusCode
		}(i)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(orchUploadSlots) < cap(orchUploadSlots) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond)
	close(gate)
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusCreated {
			t.Fatalf("upload %d: status %d (all %v)", i, c, codes)
		}
	}
}
