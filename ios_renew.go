package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The iPhone asks here for a new build when its signing profile nears its end; ios/resign.sh does the work.
const iosResignLabel = "com.rohit.agent-monitor-ios-resign"

var iosRenewGap = 30 * time.Minute

var iosRenewState struct {
	sync.Mutex
	last   time.Time
	asking bool // the request is on its way to the peers
}

var iosKick = func() error {
	return exec.Command("launchctl", "kickstart", "gui/"+strconv.Itoa(os.Getuid())+"/"+iosResignLabel).Run()
}

// iosRenewFlag holds the expiry (Unix ms) the iPhone reported; resign.sh renews the profile while it exists.
func iosRenewFlag() string { return filepath.Join(stateDir(), "ios-resign", "renew") }

func registerIOSRenewRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/ios/renew", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Expires int64  `json:"expires"`
			Via     string `json:"via"`
		}
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&in) != nil || in.Expires <= 0 {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "expires (Unix ms) is required"})
			return
		}
		writeJSON(w, iosRenew(in.Expires, time.Now(), in.Via != ""))
	})
}

// iosRenewMaxAhead bounds the reported end: a free-team profile lasts 7 days, so a later end is not real.
const iosRenewMaxAhead = 8 * 24 * time.Hour

func iosRenew(expires int64, now time.Time, via bool) map[string]any {
	if expires > now.Add(iosRenewMaxAhead).UnixMilli() {
		return map[string]any{"ok": false, "error": "expires is more than 8 days ahead"}
	}
	iosRenewState.Lock()
	if iosRenewState.asking {
		iosRenewState.Unlock()
		return map[string]any{"ok": false, "error": "a renewal request is still on its way to the other Macs; try again in a minute"}
	}
	if !iosRenewState.last.IsZero() && now.Sub(iosRenewState.last) < iosRenewGap {
		iosRenewState.Unlock()
		return map[string]any{"ok": true, "started": false}
	}
	iosRenewState.last = now // also limits how often a Mac with no job asks its peers
	flag := iosRenewFlag()
	_ = os.MkdirAll(filepath.Dir(flag), 0o755)
	if err := os.WriteFile(flag, []byte(strconv.FormatInt(expires, 10)+"\n"), 0o644); err != nil {
		iosRenewState.last = time.Time{}
		iosRenewState.Unlock()
		return map[string]any{"ok": false, "error": err.Error()}
	}
	if err := iosKick(); err == nil {
		iosRenewState.Unlock()
		return map[string]any{"ok": true, "started": true}
	}
	_ = os.Remove(flag)
	iosRenewState.asking = !via
	iosRenewState.Unlock()
	// The phone talks to this Mac; the build job may run on a peer. A forwarded request goes no further.
	if !via {
		name, started, ok := iosRenewPeers(expires)
		iosRenewState.Lock()
		iosRenewState.asking = false
		iosRenewState.Unlock()
		if ok {
			return map[string]any{"ok": true, "started": started, "peer": name}
		}
	}
	iosRenewState.Lock()
	if iosRenewState.last.Equal(now) {
		iosRenewState.last = time.Time{}
	}
	iosRenewState.Unlock()
	return map[string]any{"ok": false, "error": "this Mac has no iPhone build job (" + iosResignLabel + ")"}
}

// iosRenewPeers passes the request to each peer machine until one has the build job; it runs without the lock.
var iosRenewPeers = func(expires int64) (name string, started, ok bool) {
	peerStore.Lock()
	loadPeersLocked()
	ps := slices.Clone(peerStore.peers)
	peerStore.Unlock()
	host, _ := os.Hostname()
	body := fmt.Sprintf(`{"expires":%d,"via":%q}`, expires, firstOf(host, "peer"))
	for _, p := range ps {
		if _, err := peerURLAllowed(p.URL); err != nil {
			continue
		}
		req, err := http.NewRequest(http.MethodPost, p.URL+"/api/ios/renew", strings.NewReader(body))
		if err != nil {
			continue
		}
		req.Header.Set("Authorization", "Bearer "+p.Token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := peerClient.Do(req)
		if err != nil {
			continue
		}
		var out struct {
			OK      bool `json:"ok"`
			Started bool `json:"started"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&out)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK && out.OK {
			return p.Name, out.Started, true
		}
	}
	return "", false, false
}
