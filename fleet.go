package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// The fleet routes show this machine and its peers as one set. They need admin (no access rule),
// so a machine grant cannot use them to reach a third machine.

const (
	fleetPeerTimeout = 4 * time.Second
	fleetKeep        = 60 * time.Second // a peer's last good answer is shown this long when it stops answering
)

var machineName = struct {
	sync.Mutex
	name   string
	loaded bool
}{}

func machineNamePath() string { return filepath.Join(stateDir(), "machine-name") }

// selfMachineName is the name set in Settings, else the Mac's computer name, else the short host name.
func selfMachineName() string {
	machineName.Lock()
	defer machineName.Unlock()
	if !machineName.loaded {
		machineName.loaded = true
		if b, err := os.ReadFile(machineNamePath()); err == nil {
			machineName.name = strings.TrimSpace(string(b))
		}
		if machineName.name == "" {
			if out, err := exec.Command("scutil", "--get", "ComputerName").Output(); err == nil {
				machineName.name = strings.TrimSpace(string(out))
			}
		}
		if machineName.name == "" {
			h, _ := os.Hostname()
			h = strings.TrimSuffix(strings.TrimSuffix(h, "."), ".local")
			if i := strings.IndexByte(h, '.'); i > 0 {
				h = h[:i]
			}
			machineName.name = h
		}
	}
	return firstOf(machineName.name, "this machine")
}

// setMachineName keeps the name in the state dir; "" goes back to the computer name.
func setMachineName(n string) error {
	n = strings.TrimSpace(n)
	if len([]rune(n)) > 40 || strings.ContainsAny(n, "/\\:@\n\r\t") {
		return errors.New("use at most 40 characters and no / \\ : @")
	}
	var err error
	if n == "" {
		err = os.Remove(machineNamePath())
		if os.IsNotExist(err) {
			err = nil
		}
	} else {
		err = os.WriteFile(machineNamePath(), []byte(n+"\n"), 0o600)
	}
	machineName.Lock()
	machineName.loaded = false
	machineName.Unlock()
	return err
}

type fleetPeer struct {
	Name   string `json:"name"`
	Self   bool   `json:"self,omitempty"`
	Level  string `json:"level"` // view, control, or full for this machine
	Status string `json:"status"`
}

type fleetCacheEntry struct {
	at   time.Time
	body json.RawMessage
}

var fleetCache = struct {
	sync.Mutex
	m map[string]fleetCacheEntry
}{m: map[string]fleetCacheEntry{}}

// fleetGetFn is replaced in tests.
var fleetGetFn = fleetGet

// fleetGet reads one GET route from a peer with this machine's grant token; only JSON is accepted.
func fleetGet(p Peer, route string) (json.RawMessage, error) {
	if _, err := peerURLAllowed(p.URL); err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodGet, p.URL+"/api/"+route, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.Token)
	req.Header.Set("Accept", "application/json")
	c := &http.Client{Timeout: fleetPeerTimeout, CheckRedirect: peerClient.CheckRedirect}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		return nil, &fleetErr{resp.Status}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil || !json.Valid(b) {
		return nil, &fleetErr{"bad JSON"}
	}
	return b, nil
}

type fleetErr struct{ s string }

func (e *fleetErr) Error() string { return e.s }

// fleetFetch asks every peer for route in parallel and keeps each answer for fleetKeep.
// The result maps peer name to its answer; a peer with no fresh or kept answer is left out.
func fleetFetch(ps []Peer, route string) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	now := time.Now()
	for _, p := range ps {
		wg.Add(1)
		go func(p Peer) {
			defer wg.Done()
			key := p.Name + " " + route
			b, err := fleetGetFn(p, route)
			fleetCache.Lock()
			if err == nil {
				fleetCache.m[key] = fleetCacheEntry{at: now, body: b}
			} else if e, ok := fleetCache.m[key]; ok && now.Sub(e.at) < fleetKeep {
				b, err = e.body, nil
			}
			fleetCache.Unlock()
			if err == nil {
				mu.Lock()
				out[p.Name] = b
				mu.Unlock()
			}
		}(p)
	}
	wg.Wait()
	return out
}

func fleetPeersSnapshot() []Peer {
	peerStore.Lock()
	defer peerStore.Unlock()
	loadPeersLocked()
	return append([]Peer(nil), peerStore.peers...)
}

// fleetMachines lists this machine first, then the peers with their live status and level.
func fleetMachines(ps []Peer) []fleetPeer {
	out := []fleetPeer{{Name: selfMachineName(), Self: true, Level: "full", Status: "online"}}
	st := peerStatus(ps)
	for _, p := range ps {
		h := st[p.Name]
		out = append(out, fleetPeer{Name: p.Name, Level: h.Level, Status: h.Status})
	}
	sort.SliceStable(out[1:], func(i, j int) bool { return out[1+i].Name < out[1+j].Name })
	return out
}

// fleetTag adds "machine" and "level" to each object of the array at key in a peer's answer.
func fleetTag(body json.RawMessage, key, machine, level string) []json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return nil
	}
	var items []map[string]json.RawMessage
	if json.Unmarshal(m[key], &items) != nil {
		return nil
	}
	mj, _ := json.Marshal(machine)
	lj, _ := json.Marshal(level)
	out := make([]json.RawMessage, 0, len(items))
	for _, it := range items {
		if it == nil {
			continue
		}
		it["machine"], it["level"] = mj, lj
		if b, err := json.Marshal(it); err == nil {
			out = append(out, b)
		}
	}
	return out
}

// fleetPeerItems fetches route from the online peers and tags the array at key with each peer's name.
func fleetPeerItems(route, key string) (machines []fleetPeer, items []json.RawMessage) {
	ps := fleetPeersSnapshot()
	machines = fleetMachines(ps)
	level := map[string]string{}
	var online []Peer
	for _, m := range machines[1:] {
		level[m.Name] = m.Level
	}
	for _, p := range ps {
		for _, m := range machines {
			if m.Name == p.Name && !m.Self && m.Status == "online" {
				online = append(online, p)
			}
		}
	}
	got := fleetFetch(online, route)
	names := make([]string, 0, len(got))
	for n := range got {
		names = append(names, n)
	}
	sort.Strings(names)
	items = []json.RawMessage{}
	for _, n := range names {
		items = append(items, fleetTag(got[n], key, n, level[n])...)
	}
	return machines, items
}

func registerFleetRoutes(mux *http.ServeMux) {
	mux.HandleFunc("PUT /api/fleet/name", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "bad json"})
			return
		}
		if err := setMachineName(in.Name); err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "name": selfMachineName()})
	})
	mux.HandleFunc("GET /api/fleet/machines", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"machines": fleetMachines(fleetPeersSnapshot())})
	})
	// Peer threads only: the client merges them with this machine's own live feed.
	mux.HandleFunc("GET /api/fleet/threads", func(w http.ResponseWriter, r *http.Request) {
		machines, items := fleetPeerItems("v3/threads?compact=1&all=1", "threads")
		writeJSON(w, map[string]any{"machines": machines, "threads": items})
	})
	// Every machine's usage readings, this machine first; the client groups them by account.
	mux.HandleFunc("GET /api/fleet/usage", func(w http.ResponseWriter, r *http.Request) {
		machines, items := fleetPeerItems("usage/limits", "agents")
		usageLimitsSvc.kickCodexAmounts()
		self, _ := json.Marshal(usageLimitsSvc.get())
		own := fleetTag(self, "agents", machines[0].Name, "full")
		writeJSON(w, map[string]any{"machines": machines, "agents": append(own, items...)})
	})
}
