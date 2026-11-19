package main

import (
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// F10: uploads have a total quota, old files are removed, and remote callers do not see paths.
func TestReviewF10UploadQuotaAndPath(t *testing.T) {
	e := newOrchEnv(t)
	mux := http.NewServeMux()
	e.m.routes(mux)
	png := onePixelPNG
	post := func(addr string) *httptest.ResponseRecorder {
		var body strings.Builder
		mw := multipart.NewWriter(&body)
		fw, _ := mw.CreateFormFile("file", "a.png")
		fw.Write(png)
		mw.Close()
		r := httptest.NewRequest("POST", "/api/orch/uploads", strings.NewReader(body.String()))
		r.Header.Set("Content-Type", mw.FormDataContentType())
		r.RemoteAddr = addr
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	old := filepath.Join(e.m.uploadsDir(), "new", "up_00000000000000aa.png")
	os.MkdirAll(filepath.Dir(old), 0o700)
	os.WriteFile(old, png, 0o600)
	past := time.Now().Add(-31 * 24 * time.Hour)
	os.Chtimes(old, past, past)
	w := post(remoteAddr)
	if w.Code != 201 || strings.Contains(w.Body.String(), `"path"`) || strings.Contains(w.Body.String(), e.m.uploadsDir()) {
		t.Fatalf("remote upload %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(old); err == nil {
		t.Fatal("upload older than 30 days kept")
	}
	if w := post("127.0.0.1:1"); w.Code != 201 || !strings.Contains(w.Body.String(), `"path"`) {
		t.Fatalf("local upload %d %s", w.Code, w.Body)
	}
	oldQuota := orchUploadQuota
	orchUploadQuota = int64(len(png)) * 2
	t.Cleanup(func() { orchUploadQuota = oldQuota })
	if w := post("127.0.0.1:1"); w.Code != http.StatusInsufficientStorage {
		t.Fatalf("upload over quota: %d %s", w.Code, w.Body)
	}
}
