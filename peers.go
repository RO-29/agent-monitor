package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Peer is another machine whose daemon granted this one access; the token is that machine's grant.
type Peer struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	Token   string `json:"token,omitempty"`
	Level   string `json:"level"`
	AddedAt int64  `json:"addedAt"`
}

var peerStore = struct {
	sync.Mutex
	peers []Peer
	read  bool
}{}

func peersPath() string { return filepath.Join(stateDir(), "peers.json") }

func loadPeersLocked() {
	if peerStore.read {
		return
	}
	peerStore.read = true
	b, err := os.ReadFile(peersPath())
	if err == nil {
		_ = json.Unmarshal(b, &peerStore.peers)
	}
}

func savePeersLocked() error {
	b, _ := json.MarshalIndent(peerStore.peers, "", "  ")
	tmp := peersPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, peersPath())
}

func peerByName(name string) (Peer, bool) {
	peerStore.Lock()
	defer peerStore.Unlock()
	loadPeersLocked()
	for _, p := range peerStore.peers {
		if p.Name == name {
			return p, true
		}
	}
	return Peer{}, false
}

// peerLoopbackOK lets tests reach an in-process peer; it is never set outside tests.
var peerLoopbackOK bool

var peerNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,40}$`)

// peerURLAllowed refuses a peer address that is this machine, loopback, link-local or unspecified,
// so a peer entry can never turn the proxy into a way back into this daemon or the local network.
func peerURLAllowed(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(raw), "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" || u.User != nil || u.RawQuery != "" {
		return nil, errors.New("give the peer as http://<tailscale-ip>:7777")
	}
	ips, err := net.LookupIP(u.Hostname())
	if err != nil || len(ips) == 0 {
		return nil, fmt.Errorf("cannot resolve %s", u.Hostname())
	}
	own := map[string]bool{}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				own[n.IP.String()] = true
			}
		}
	}
	for _, ip := range ips {
		if peerLoopbackOK && ip.IsLoopback() {
			continue
		}
		if ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || own[ip.String()] {
			return nil, fmt.Errorf("%s is this machine or a local address", ip)
		}
	}
	return u, nil
}

// peerClient never follows redirects: a peer answers itself or not at all.
var peerClient = &http.Client{
	Timeout:       15 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// claimPeer trades a machine pairing code from the peer for that peer's grant token.
func claimPeer(rawURL, code, name string) (Peer, error) {
	u, err := peerURLAllowed(rawURL)
	if err != nil {
		return Peer{}, err
	}
	body, _ := json.Marshal(map[string]string{"code": strings.TrimSpace(code)})
	resp, err := peerClient.Post(u.String()+"/api/devices/session", "application/json", bytes.NewReader(body))
	if err != nil {
		return Peer{}, fmt.Errorf("cannot reach %s: %v", u.Host, err)
	}
	defer resp.Body.Close()
	var out struct {
		Token  string `json:"token"`
		Error  string `json:"error"`
		Device struct {
			Name  string `json:"name"`
			Kind  string `json:"kind"`
			Level string `json:"level"`
		} `json:"device"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out)
	if resp.StatusCode != http.StatusOK {
		return Peer{}, fmt.Errorf("the peer refused the code: %s", firstOf(out.Error, resp.Status))
	}
	if out.Device.Kind != "machine" || !strings.HasPrefix(out.Token, deviceTokenPrefix) {
		return Peer{}, errors.New("that code is not a machine pairing code; create one under Machines on the other machine")
	}
	if name == "" {
		name = u.Hostname()
	}
	if !peerNameRe.MatchString(name) {
		return Peer{}, errors.New("name: letters, digits, dot, dash or underscore, up to 41 characters")
	}
	p := Peer{Name: name, URL: u.String(), Token: out.Token, Level: out.Device.Level, AddedAt: time.Now().UnixMilli()}
	peerStore.Lock()
	defer peerStore.Unlock()
	loadPeersLocked()
	kept := peerStore.peers[:0]
	for _, o := range peerStore.peers {
		if o.Name != name {
			kept = append(kept, o)
		}
	}
	peerStore.peers = append(kept, p)
	return p, savePeersLocked()
}

// peerRoutes are the only peer routes the proxy passes on; the peer still checks its own grant level.
var peerRoutes = []struct {
	method string
	re     *regexp.Regexp
}{
	{"GET", regexp.MustCompile(`^(needs-you|sessions|health|usage/limits|v3/threads|orch/threads)$`)},
	{"GET", regexp.MustCompile(`^session/[A-Za-z0-9:._-]{1,120}/(items|place|cli-queue)$`)},
	{"POST", regexp.MustCompile(`^approvals/[A-Za-z0-9_-]{1,80}/decide$`)},
	{"POST", regexp.MustCompile(`^session/prompt/[A-Za-z0-9:._-]{1,120}$`)},
	{"POST", regexp.MustCompile(`^session/[A-Za-z0-9:._-]{1,120}/cli-queue/send$`)},
}

func peerRouteAllowed(method, rest string) bool {
	for _, r := range peerRoutes {
		if r.method == method && r.re.MatchString(rest) {
			return true
		}
	}
	return false
}

// servePeerProxy passes one allowed request to a peer with this machine's grant token. The request is
// built from scratch: no cookie, header or credential of the caller goes to the peer, and only JSON
// comes back.
func servePeerProxy(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/m/")
	name, rest, ok := strings.Cut(rest, "/api/")
	if !ok {
		writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "use /api/m/<peer>/api/<route>"})
		return
	}
	p, found := peerByName(name)
	if !found {
		writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "no such peer"})
		return
	}
	if !peerRouteAllowed(r.Method, rest) {
		writeJSONStatus(w, http.StatusForbidden, map[string]any{"error": "agent-monitor does not pass this route to other machines"})
		return
	}
	if _, err := peerURLAllowed(p.URL); err != nil {
		writeJSONStatus(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	var body io.Reader
	if r.Method == http.MethodPost {
		b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "bad body"})
			return
		}
		body = bytes.NewReader(b)
	}
	target := p.URL + "/api/" + rest
	if q := r.URL.Query(); len(q) > 0 {
		target += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, body)
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	req.Header.Set("Authorization", "Bearer "+p.Token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := peerClient.Do(req)
	if err != nil {
		writeJSONStatus(w, http.StatusBadGateway, map[string]any{"error": "peer unreachable: " + err.Error(), "peer": p.Name})
		return
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeJSONStatus(w, http.StatusBadGateway, map[string]any{"error": "the peer answered with " + firstOf(ct, "no content type"), "peer": p.Name})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, 16<<20))
}

// peerStatus asks each peer for its health with a short timeout; it never fails the list.
func peerStatus(ps []Peer) map[string]string {
	out := map[string]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, p := range ps {
		wg.Add(1)
		go func(p Peer) {
			defer wg.Done()
			st := "offline"
			req, _ := http.NewRequest(http.MethodGet, p.URL+"/api/health", nil)
			req.Header.Set("Authorization", "Bearer "+p.Token)
			c := &http.Client{Timeout: 3 * time.Second, CheckRedirect: peerClient.CheckRedirect}
			if resp, err := c.Do(req); err == nil {
				resp.Body.Close()
				switch resp.StatusCode {
				case http.StatusOK:
					st = "online"
				case http.StatusUnauthorized:
					st = "revoked"
				default:
					st = "error " + resp.Status
				}
			}
			mu.Lock()
			out[p.Name] = st
			mu.Unlock()
		}(p)
	}
	wg.Wait()
	return out
}

func registerPeerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/peers", func(w http.ResponseWriter, r *http.Request) {
		peerStore.Lock()
		loadPeersLocked()
		ps := append([]Peer(nil), peerStore.peers...)
		peerStore.Unlock()
		st := peerStatus(ps)
		out := []map[string]any{}
		for _, p := range ps {
			out = append(out, map[string]any{"name": p.Name, "url": p.URL, "level": p.Level, "addedAt": p.AddedAt, "status": st[p.Name]})
		}
		sort.Slice(out, func(i, j int) bool { return out[i]["name"].(string) < out[j]["name"].(string) })
		writeJSON(w, map[string]any{"peers": out})
	})
	mux.HandleFunc("POST /api/peers", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			URL  string `json:"url"`
			Code string `json:"code"`
			Name string `json:"name"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "bad json"})
			return
		}
		p, err := claimPeer(in.URL, in.Code, in.Name)
		if err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSONStatus(w, http.StatusCreated, map[string]any{"ok": true, "peer": map[string]any{"name": p.Name, "url": p.URL, "level": p.Level}})
	})
	mux.HandleFunc("DELETE /api/peers/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		peerStore.Lock()
		defer peerStore.Unlock()
		loadPeersLocked()
		kept, found := peerStore.peers[:0], false
		for _, p := range peerStore.peers {
			if p.Name == name {
				found = true
				continue
			}
			kept = append(kept, p)
		}
		if !found {
			writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "no such peer"})
			return
		}
		peerStore.peers = kept
		if err := savePeersLocked(); err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("/api/m/", servePeerProxy)
}
