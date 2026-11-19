package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var wireMu sync.Mutex

func installDir() string {
	d := getSettings().InstallDir
	if d == "" {
		d = "~/agent-monitor"
	}
	if d == "~" || strings.HasPrefix(d, "~/") {
		d = filepath.Join(homeDir(), d[1:])
	}
	return d
}

func newWireCtx(dry bool, allow func(string) bool) *wireCtx {
	if allow == nil {
		allow = func(string) bool { return true }
	}
	return &wireCtx{home: homeDir(), installDir: installDir(), dry: dry, allow: allow, led: loadLedger(), now: time.Now(), force: map[string]bool{}}
}

func (c *wireCtx) runAll() []wireResult {
	var out []wireResult
	for _, k := range agentRegistry {
		if !c.installed(k) {
			out = append(out, wireResult{Agent: k.ID, Kind: "mcp", Action: "skipped: not installed", Dry: c.dry})
			continue
		}
		for _, st := range k.Wire {
			r := st.Run(c)
			if r.Kind == "" {
				r.Kind = st.Kind
			}
			out = append(out, r)
		}
	}
	return out
}

// wireAll runs every writer once; dry only reports. A real run also records adoptions in the ledger.
func wireAll(dry bool) []wireResult {
	wireMu.Lock()
	defer wireMu.Unlock()
	c := newWireCtx(dry, nil)
	out := c.runAll()
	if !dry && c.dirty {
		if err := c.led.save(); err != nil {
			log.Printf("wiring: save ledger: %v", err)
		}
	}
	return out
}

type wirePending struct {
	Kind  string       `json:"kind"`
	Items []wireResult `json:"items"`
}

var wireState struct {
	sync.Mutex
	Results []wireResult
	Pending []wirePending
	RanAt   int64
	Active  bool
	Why     string
}

var wireExe = os.Executable

// wireActive is true only when this binary sits in installDir, so a test daemon never rewrites live entries.
func wireActive() (bool, string) {
	exe, err := wireExe()
	if err != nil {
		return false, "cannot find this binary: " + err.Error()
	}
	dir := installDir()
	rd, e1 := filepath.EvalSymlinks(filepath.Dir(exe))
	id, e2 := filepath.EvalSymlinks(dir)
	if e1 != nil || e2 != nil || rd != id {
		return false, "this binary is not in " + dir
	}
	return true, ""
}

// wireTick adopts into the ledger, writes the kinds you approved, and lists the kinds still waiting for a tap.
func wireTick() []wireResult {
	wireMu.Lock()
	defer wireMu.Unlock()
	ok, why := wireActive()
	wireState.Lock()
	wireState.Active, wireState.Why = ok, why
	wireState.Unlock()
	if !ok {
		return nil
	}
	c := newWireCtx(true, nil)
	dry := c.runAll()
	need := map[string][]wireResult{}
	for _, r := range dry {
		if r.Action == "added" || r.Action == "needs trust in Codex" {
			need[r.Kind] = append(need[r.Kind], r)
		}
	}
	allowed := func(kind string) bool { return c.led.mode(kind) == "on" }
	real := newWireCtx(false, allowed)
	real.led = c.led
	out := real.runAll()
	if real.dirty {
		if err := real.led.save(); err != nil {
			log.Printf("wiring: save ledger: %v", err)
		}
	}
	var pending []wirePending
	for _, k := range wireKinds {
		if items := need[k]; len(items) > 0 && c.led.mode(k) == "ask" {
			pending = append(pending, wirePending{Kind: k, Items: items})
		}
	}
	wireState.Lock()
	wireState.Results, wireState.Pending, wireState.RanAt = out, pending, time.Now().UnixMilli()
	wireState.Unlock()
	return out
}

func startWiring() {
	go func() {
		for {
			wireTick()
			time.Sleep(10 * time.Minute)
		}
	}()
}

func setWireMode(kind, mode string) error {
	if mode != "on" && mode != "off" && mode != "ask" {
		return fmt.Errorf("value must be on, off or ask")
	}
	valid := false
	for _, k := range wireKinds {
		valid = valid || k == kind
	}
	if !valid {
		return fmt.Errorf("unknown kind %q", kind)
	}
	wireMu.Lock()
	defer wireMu.Unlock()
	l := loadLedger()
	l.AutoWire[kind] = mode
	return l.save()
}

func registerWiringRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/agents/wiring", func(w http.ResponseWriter, r *http.Request) {
		wireState.Lock()
		st := map[string]any{"results": wireState.Results, "pending": wireState.Pending, "ranAt": wireState.RanAt,
			"active": wireState.Active, "why": wireState.Why, "installDir": installDir()}
		wireState.Unlock()
		if st["results"] == nil {
			st["results"] = []wireResult{}
		}
		if st["pending"] == nil {
			st["pending"] = []wirePending{}
		}
		l := loadLedger()
		modes := map[string]string{}
		for _, k := range wireKinds {
			modes[k] = l.mode(k)
		}
		st["autoWire"] = modes
		files := map[string]bool{}
		legacy := []string{}
		for _, e := range l.Entries {
			files[e.File] = true
		}
		for _, res := range wireStateResults() {
			files[res.File] = true
		}
		names := []string{}
		for f := range files {
			if f != "" {
				names = append(names, f)
			}
		}
		sort.Strings(names)
		for _, f := range names {
			legacy = append(legacy, legacyBackups(newWireCtxHome().abs(f))...)
		}
		st["legacyBackups"] = legacy
		writeJSON(w, st)
	})
	mux.HandleFunc("/api/agents/wiring/confirm", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var in struct{ Kind, Value string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.Value == "" {
			in.Value = "on"
		}
		if err := setWireMode(in.Kind, in.Value); err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		go wireTick()
		writeJSON(w, map[string]any{"ok": true, "kind": in.Kind, "value": in.Value})
	})
	mux.HandleFunc("/api/agents/registry", func(w http.ResponseWriter, r *http.Request) {
		type row struct {
			ID       string   `json:"id"`
			Binaries []string `json:"binaries"`
		}
		rows := []row{}
		for _, k := range agentRegistry {
			rows = append(rows, row{k.ID, k.Binaries})
		}
		writeJSON(w, map[string]any{"agents": rows})
	})
}

func wireStateResults() []wireResult {
	wireState.Lock()
	defer wireState.Unlock()
	return append([]wireResult(nil), wireState.Results...)
}

func newWireCtxHome() *wireCtx { return &wireCtx{home: homeDir()} }

// runWireCmd is `agent-monitor wire [--dry] [--force <agent>]`; running it is your decision, so every kind is allowed.
func runWireCmd(args []string) int {
	dry := false
	force := map[string]bool{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--dry":
			dry = true
		case "--force":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "--force needs an agent id")
				return 2
			}
			i++
			force[args[i]] = true
		default:
			fmt.Fprintf(os.Stderr, "usage: agent-monitor wire [--dry] [--force <agent>]\n")
			return 2
		}
	}
	wireMu.Lock()
	defer wireMu.Unlock()
	c := newWireCtx(dry, nil)
	c.force = force
	printWire(c.runAll())
	if !dry && c.dirty {
		if err := c.led.save(); err != nil {
			fmt.Fprintf(os.Stderr, "ledger: %v\n", err)
			return 1
		}
	}
	return 0
}

func runUnwireCmd(args []string) int {
	agent := ""
	if len(args) > 0 {
		agent = args[0]
	}
	wireMu.Lock()
	defer wireMu.Unlock()
	c := newWireCtx(false, nil)
	printWire(c.unwire(agent))
	if err := c.led.save(); err != nil {
		fmt.Fprintf(os.Stderr, "ledger: %v\n", err)
		return 1
	}
	return 0
}

func printWire(rs []wireResult) {
	for _, r := range rs {
		fmt.Printf("%-13s %-5s %-34s %s", r.Agent, r.Kind, r.File, r.Action)
		if len(r.Entries) > 0 {
			fmt.Printf(" (%s)", strings.Join(r.Entries, ", "))
		}
		if r.RestartNeeded {
			fmt.Print(" [restart needed]")
		}
		fmt.Println()
		if r.Detail != "" && strings.HasPrefix(r.Action, "manual") {
			fmt.Println("    " + strings.ReplaceAll(r.Detail, "\n", "\n    "))
		}
	}
}

func init() {
	wireSessionsOlder = func(agent string, at int64) bool {
		if store == nil {
			return false
		}
		for _, s := range store.All() {
			live := s.State != StateCompleted && s.State != StateAbandoned
			if string(s.Tool) == agent && live && s.StartedAt < at {
				return true
			}
		}
		return false
	}
}
