package main

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// The iPhone asks here for a new build when its signing profile nears its end; ios/resign.sh does the work.
const iosResignLabel = "com.rohit.agent-monitor-ios-resign"

var iosRenewGap = 30 * time.Minute

var iosRenewState struct {
	sync.Mutex
	last time.Time
}

var iosKick = func() error {
	return exec.Command("launchctl", "kickstart", "gui/"+strconv.Itoa(os.Getuid())+"/"+iosResignLabel).Run()
}

// iosRenewFlag holds the expiry (Unix ms) the iPhone reported; resign.sh renews the profile while it exists.
func iosRenewFlag() string { return filepath.Join(stateDir(), "ios-resign", "renew") }

func registerIOSRenewRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/ios/renew", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Expires int64 `json:"expires"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&in) != nil || in.Expires <= 0 {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "expires (Unix ms) is required"})
			return
		}
		writeJSON(w, iosRenew(in.Expires, time.Now()))
	})
}

func iosRenew(expires int64, now time.Time) map[string]any {
	iosRenewState.Lock()
	defer iosRenewState.Unlock()
	flag := iosRenewFlag()
	_ = os.MkdirAll(filepath.Dir(flag), 0o755)
	if err := os.WriteFile(flag, []byte(strconv.FormatInt(expires, 10)+"\n"), 0o644); err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	if !iosRenewState.last.IsZero() && now.Sub(iosRenewState.last) < iosRenewGap {
		return map[string]any{"ok": true, "started": false}
	}
	if err := iosKick(); err != nil {
		_ = os.Remove(flag)
		return map[string]any{"ok": false, "error": "this Mac has no iPhone build job (" + iosResignLabel + ")"}
	}
	iosRenewState.last = now
	return map[string]any{"ok": true, "started": true}
}
