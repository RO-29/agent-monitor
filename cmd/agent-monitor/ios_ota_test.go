package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOTARequestNeedsTheToken(t *testing.T) {
	t.Setenv("AGENT_MONITOR_HOME", t.TempDir())
	_ = os.MkdirAll(otaDir(), 0o700)
	tok := strings.Repeat("a1", 20)
	_ = os.WriteFile(filepath.Join(otaDir(), "token"), []byte(tok+"\n"), 0o600)
	cases := map[string]string{
		"/ios-ota/" + tok + "/manifest.plist":   "manifest.plist",
		"/ios-ota/" + tok + "/":                 "index.html",
		"/ios-ota/" + tok + "/AgentMonitor.ipa": "AgentMonitor.ipa",
		"/ios-ota/" + tok + "/token":            "",
		"/ios-ota/" + tok + "/../sessions.db":   "",
		"/ios-ota/wrong/manifest.plist":         "",
		"/api/sessions":                         "",
	}
	for p, want := range cases {
		if got := otaRequest(p); got != want {
			t.Errorf("%s: got %q, want %q", p, got, want)
		}
	}
	_ = os.WriteFile(filepath.Join(otaDir(), "token"), []byte("short"), 0o600)
	if otaRequest("/ios-ota/short/manifest.plist") != "" {
		t.Error("a short token must never open the files")
	}
}
