package main

import (
	"crypto/subtle"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// iPhone install over the internet (ios/ota.sh): iOS fetches an itms-services manifest and the .ipa itself, with no
// cookie or password, so these three files are public under a long random token that ota.sh writes.
var otaFiles = map[string]string{
	"":                 "index.html",
	"index.html":       "index.html",
	"manifest.plist":   "manifest.plist",
	"AgentMonitor.ipa": "AgentMonitor.ipa",
}

func otaDir() string { return filepath.Join(stateDir(), "ios-ota") }

// otaRequest returns the file an /ios-ota/<token>/<file> request names, or "" when the token or the name is wrong.
func otaRequest(p string) string {
	rest, ok := strings.CutPrefix(p, "/ios-ota/")
	if !ok {
		return ""
	}
	tok, name, _ := strings.Cut(rest, "/")
	want, err := os.ReadFile(filepath.Join(otaDir(), "token"))
	w := strings.TrimSpace(string(want))
	if err != nil || len(w) < 32 || subtle.ConstantTimeCompare([]byte(tok), []byte(w)) != 1 {
		return ""
	}
	return otaFiles[name]
}

func registerOTARoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /ios-ota/", func(w http.ResponseWriter, r *http.Request) {
		f := otaRequest(r.URL.Path)
		if f == "" {
			http.NotFound(w, r)
			return
		}
		switch filepath.Ext(f) {
		case ".plist":
			w.Header().Set("Content-Type", "text/xml")
		case ".ipa":
			w.Header().Set("Content-Type", "application/octet-stream")
		}
		w.Header().Set("Cache-Control", "no-store")
		log.Printf("ios-ota: %s %s (%s)", r.Method, f, clipString(r.UserAgent(), 60)) // shows how far an install got
		http.ServeFile(w, r, filepath.Join(otaDir(), f))
	})
}
