package main

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

//go:embed web/*
var webFS embed.FS

var store *Store
var permStore *PermStore
var paneRegistry *PaneRegistry
var talkStore *TalkStore

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true }, // bound to 127.0.0.1
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "install", "install-hooks":
			// "install" is the new umbrella that wires hooks AND MCP entries
			// for both Claude and Codex. "install-hooks" is kept as an alias
			// since the README + earlier prompts reference it.
			installAll()
			return
		case "mcp-perm-server":
			runMCPPermServer()
			return
		case "run":
			// agent-monitor run <agent> [args...] — register-then-exec wrapper.
			runAgentWrapper(os.Args[2:])
			return
		case "send":
			// agent-monitor send <recipient> "msg" — agent-to-agent talk.
			runSendCmd(os.Args[2:])
			return
		case "list-agents", "list":
			runListAgentsCmd()
			return
		case "name":
			// agent-monitor name <agentId-or-current> <alias>
			runNameCmd(os.Args[2:])
			return
		case "id":
			// agent-monitor id — print the agent id registered to $TMUX_PANE.
			runIDCmd()
			return
		case "resolve":
			// agent-monitor resolve <alias|id|pane|sid-prefix>
			runResolveCmd(os.Args[2:])
			return
		case "read":
			// agent-monitor read <recipient> [lines]
			runReadCmd(os.Args[2:])
			return
		case "type":
			// agent-monitor type <recipient> "text" (no Enter)
			runTypeCmd(os.Args[2:])
			return
		case "keys":
			// agent-monitor keys <recipient> <key> [key...]
			runKeysCmd(os.Args[2:])
			return
		case "stop":
			// agent-monitor stop — kill the daemon ONLY (the process owning
			// the listen port), leaving any mcp-perm-server children alone.
			// Using `pkill -f /agent-monitor` would kill those children too,
			// which closes codex/claude's MCP transport unrecoverably.
			runStopCmd()
			return
		case "restart":
			runStopCmd()
			runDaemon()
			return
		case "version", "-v", "--version":
			fmt.Println("agent-monitor 0.3.0 (go)")
			return
		}
	}
	runServer()
}

// runDaemon is what the no-args invocation does — extracted so `restart`
// can call it after stopping the previous instance.
func runDaemon() { runServer() }

func runServer() {
	port := 7777
	if v := os.Getenv("AGENT_MONITOR_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			port = n
		}
	}

	store = NewStore()
	permStore = NewPermStore()
	paneRegistry = NewPaneRegistry()
	talkStore = NewTalkStore()
	// Pane GC: remove registrations whose pane_id no longer exists in tmux.
	// Once at startup (catches tmux-restart renumbering, agent CLI exits)
	// and then on a slow ticker so the registry stays tidy without manual
	// `forget` clicks. listTmuxPanes is ~5ms so this is essentially free.
	if panes, err := listTmuxPanes(); err == nil {
		if dropped := paneRegistry.GarbageCollect(panes); len(dropped) > 0 {
			log.Printf("pane GC: dropped %d orphan registration(s)", len(dropped))
		}
	}
	// Dedupe sweep: pre-existing registries built up before register-time
	// eviction can have multiple entries on the same paneId. Keep the
	// newest, drop the rest. One-shot at startup; new registrations evict
	// in-place from now on.
	if dropped := paneRegistry.DedupeByPane(); len(dropped) > 0 {
		log.Printf("pane dedupe: dropped %d duplicate-on-pane registration(s)", len(dropped))
	}
	go func() {
		// 15s ticker — fast enough that exiting an agent CLI without a hook
		// (codex/cursor-agent/opencode) gets noticed quickly; slow enough
		// that we're not constantly forking ps + tmux. Negligible cost.
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for range t.C {
			panes, err := listTmuxPanes()
			if err != nil {
				continue
			}
			if dropped := paneRegistry.GarbageCollect(panes); len(dropped) > 0 {
				log.Printf("pane GC: dropped %d orphan registration(s)", len(dropped))
			}
		}
	}()
	// Pair wrapper-launched announcements with the next session of matching
	// tool/cwd. PairWithSession is a no-op when no pending wrapper matches,
	// so it's safe to fire on every upsert (cheap map walk).
	store.Subscribe(func(e ServerEvent) {
		if e.Kind == "upsert" && e.Session != nil {
			paneRegistry.PairWithSession(e.Session)
		}
	})
	store.StartDecayLoop()
	// Detect AskUserQuestion / permission prompts from pane content (they don't
	// fire the Notification hook) so they show up in the Approvals tab.
	startPanePromptWatcher(store, paneRegistry)

	// Auto-kill agent tmux sessions (claude-…/codex-… from the zsh wrappers)
	// idle for >7 days, so the terminal doesn't accumulate dead widgets.
	const agentTmuxMaxIdle = 7 * 24 * time.Hour
	sweepStaleAgentTmux(agentTmuxMaxIdle)
	go func() {
		t := time.NewTicker(1 * time.Hour)
		defer t.Stop()
		for range t.C {
			sweepStaleAgentTmux(agentTmuxMaxIdle)
		}
	}()
	startClaudeAdapter(store)
	startCodexAdapter(store)
	startOpencodeAdapter(store)
	startCursorAdapter(store)
	startCursorAgentAdapter(store)

	// Durable session history — mirror the volatile store to SQLite every 2
	// minutes (and on shutdown) so history outlives restarts + the decay GC.
	var flushSessions func()
	if db, err := OpenSessionDB(); err != nil {
		log.Printf("sessions.db: disabled (%v)", err)
	} else {
		flushSessions = StartSessionPersistence(store, db, 2*time.Minute)
		defer db.Close()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"sessions": store.All()})
	})
	// /api/chains — related-session graph (continuation chains). Precise linkage
	// by shared handoff/resume plan reference + same-pane continuity.
	mux.HandleFunc("/api/chains", func(w http.ResponseWriter, r *http.Request) {
		panes := map[string]string{}
		for _, reg := range paneRegistry.List() {
			panes[reg.AgentID] = reg.PaneID
		}
		sessions := store.All()
		chains, chainOf := computeChains(sessions, panes)
		parentOf, childrenOf := matchSpawns(sessions)
		writeJSON(w, map[string]any{
			"chains": chains, "chainOf": chainOf,
			"spawnParent": parentOf, "spawnChildren": childrenOf,
		})
	})
	mux.HandleFunc("/api/session/", func(w http.ResponseWriter, r *http.Request) {
		// /api/session/<id>/full
		path := strings.TrimPrefix(r.URL.Path, "/api/session/")
		if !strings.HasSuffix(path, "/full") {
			http.NotFound(w, r)
			return
		}
		id := strings.TrimSuffix(path, "/full")
		sess := store.Get(id)
		if sess == nil {
			writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "not found"})
			return
		}
		detail := buildSessionDetail(sess)
		writeJSON(w, detail)
	})
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		// tmuxPanes: total live panes across tmux — lets the UI warn when the
		// terminal is getting crowded. registeredPanes: how many are agent-monitor
		// drivable. Cheap (~5ms) and only polled on the health tick.
		tmuxPanes := 0
		if ps, err := listTmuxPanes(); err == nil {
			tmuxPanes = len(ps)
		}
		writeJSON(w, map[string]any{
			"ok": true, "sessions": len(store.All()),
			"tmuxPanes": tmuxPanes, "registeredPanes": len(paneRegistry.List()),
		})
	})
	mux.HandleFunc("/events/claude", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var p ClaudeHookPayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		handleClaudeHook(store, p)
		writeJSON(w, map[string]any{"ok": true})
	})
	mux.HandleFunc("/api/permissions", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"requests": permStore.List()})
	})
	mux.HandleFunc("/api/permission/request", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			ToolName  string         `json:"toolName"`
			Input     map[string]any `json:"input"`
			ToolUseID string         `json:"toolUseId"`
			Cwd       string         `json:"cwd"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		req := &PermissionRequest{
			ToolName:  body.ToolName,
			Input:     body.Input,
			ToolUseID: body.ToolUseID,
			Cwd:       body.Cwd,
		}
		permStore.Register(req)
		writeJSON(w, map[string]any{"id": req.ID})
	})
	mux.HandleFunc("/api/permission/wait/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/permission/wait/")
		timeout := 600 * time.Second
		if v := r.URL.Query().Get("timeout"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				timeout = time.Duration(n) * time.Second
			}
		}
		// Hijack-friendly: don't buffer the response. The client is long-polling.
		flusher, _ := w.(http.Flusher)
		_ = flusher
		resp, ok := permStore.Wait(id, timeout)
		if !ok {
			writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "not found"})
			return
		}
		writeJSON(w, resp)
	})
	mux.HandleFunc("/api/permission/", func(w http.ResponseWriter, r *http.Request) {
		// /api/permission/<id>/respond
		path := strings.TrimPrefix(r.URL.Path, "/api/permission/")
		if !strings.HasSuffix(path, "/respond") {
			http.NotFound(w, r)
			return
		}
		id := strings.TrimSuffix(path, "/respond")
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body PermissionResponse
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if body.Behavior != "allow" && body.Behavior != "deny" {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "behavior must be allow|deny"})
			return
		}
		if !permStore.Respond(id, body) {
			writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "not found or already answered"})
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})
	// resolvePane returns the tmux pane recorded in the registry for an
	// agent id. Takes a string instead of a *Session because pane drivability
	// is decoupled from session liveness — once registered, you can talk to
	// the pane even after the session has decayed out of the in-memory store
	// (which happens at ~60min idle). The registry is the source of truth.
	resolvePane := func(agentID string) (*TmuxPane, *PaneRegistration, error) {
		if agentID == "" {
			return nil, nil, errors.New("no agent id")
		}
		reg := paneRegistry.Get(agentID)
		if reg == nil {
			return nil, nil, nil
		}
		panes, err := listTmuxPanes()
		if err != nil {
			return nil, reg, err
		}
		for i := range panes {
			if panes[i].PaneID == reg.PaneID {
				paneRegistry.Touch(agentID)
				return &panes[i], reg, nil
			}
		}
		// Registered pane has vanished (tmux restarted, pane killed). Caller
		// should surface this to the UI so the user can re-launch / forget.
		return nil, reg, errors.New("registered pane no longer exists in tmux")
	}

	// Tmux bridge — send text or Ctrl-C into the pane that hosts a session.
	// We re-resolve the pane on every call (cheap) so re-attached sessions
	// still find the right pane after the user moves panes around.
	mux.HandleFunc("/api/session/send/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/api/session/send/")
		var body struct {
			Text  string `json:"text"`
			Enter bool   `json:"enter"`
			Pane  string `json:"pane"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		pane, reg, err := resolvePane(id)
		if err != nil && reg == nil {
			writeJSONStatus(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
			return
		}
		if reg == nil {
			writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "no pane registered — launch the agent via `agent-monitor run` or the SessionStart hook"})
			return
		}
		if pane == nil {
			msg := "registered pane no longer exists in tmux — re-launch the agent or POST DELETE /api/pane/registration to forget"
			if err != nil {
				msg = err.Error()
			}
			writeJSONStatus(w, http.StatusGone, map[string]any{"error": msg, "registered": true, "paneId": reg.PaneID})
			return
		}
		if err := SendToTmuxPane(pane.PaneID, body.Text, body.Enter); err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "pane": pane.PaneID, "session": pane.SessionName})
	})
	// Keys passthrough — send arbitrary tmux key names to the pane. The body
	// is `{keys: ["1", "Enter"]}` etc. Used by the quick-key buttons in the
	// pane bridge (Y/N for permission prompts, arrows for menus, ...).
	mux.HandleFunc("/api/session/keys/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/api/session/keys/")
		var body struct {
			Keys []string `json:"keys"`
			Pane string   `json:"pane"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		pane, reg, err := resolvePane(id)
		if err != nil && reg == nil {
			writeJSONStatus(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
			return
		}
		if reg == nil {
			writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "no pane registered"})
			return
		}
		if pane == nil {
			msg := "registered pane no longer exists in tmux — re-launch the agent or POST DELETE /api/pane/registration to forget"
			if err != nil {
				msg = err.Error()
			}
			writeJSONStatus(w, http.StatusGone, map[string]any{"error": msg, "registered": true, "paneId": reg.PaneID})
			return
		}
		if err := SendKeysToTmuxPane(pane.PaneID, body.Keys); err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "pane": pane.PaneID, "keys": body.Keys})
	})
	mux.HandleFunc("/api/session/cancel/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/api/session/cancel/")
		pane, reg, err := resolvePane(id)
		if err != nil && reg == nil {
			writeJSONStatus(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
			return
		}
		if reg == nil {
			writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "no pane registered"})
			return
		}
		if pane == nil {
			msg := "registered pane no longer exists in tmux — re-launch the agent or POST DELETE /api/pane/registration to forget"
			if err != nil {
				msg = err.Error()
			}
			writeJSONStatus(w, http.StatusGone, map[string]any{"error": msg, "registered": true, "paneId": reg.PaneID})
			return
		}
		if err := SendCtrlCToTmuxPane(pane.PaneID); err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "pane": pane.PaneID})
	})
	// Pane lookup — registry-backed. Returns the registered pane (if any) so
	// the web bridge can show its tmux address, alias, and live status. No
	// candidates / heuristic fallback — registration is the source of truth.
	mux.HandleFunc("/api/session/pane/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/session/pane/")
		pane, reg, err := resolvePane(id)
		if reg == nil {
			writeJSON(w, map[string]any{"ok": false, "registered": false})
			return
		}
		resp := map[string]any{
			"registered":   true,
			"agentId":      reg.AgentID,
			"paneId":       reg.PaneID,
			"alias":        reg.Alias,
			"source":       reg.Source,
			"registeredAt": reg.RegisteredAt,
			"lastSeenAt":   reg.LastSeenAt,
		}
		if err != nil || pane == nil {
			resp["ok"] = false
			resp["error"] = "registered pane no longer exists in tmux — re-launch the agent or POST DELETE /api/pane/registration to forget"
			writeJSON(w, resp)
			return
		}
		resp["ok"] = true
		resp["sessionName"] = pane.SessionName
		resp["command"] = pane.CurrentCommand
		resp["path"] = pane.CurrentPath
		resp["pid"] = pane.PanePID
		writeJSON(w, resp)
	})

	// Focus — "take me to the terminal where it's running". Selects the pane in
	// tmux, switches the attached client to its session, and raises the hosting
	// terminal app. Used by the AgentTV widget (click a tile) and the dashboard.
	mux.HandleFunc("/api/session/focus/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONStatus(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "reason": "POST only"})
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/api/session/focus/")
		pane, reg, err := resolvePane(id)
		if reg == nil {
			writeJSON(w, map[string]any{"ok": false, "reason": "unregistered"})
			return
		}
		if err != nil || pane == nil {
			writeJSON(w, map[string]any{"ok": false, "reason": "stale"})
			return
		}
		term, ferr := focusTmuxPane(pane)
		if ferr != nil {
			writeJSON(w, map[string]any{"ok": false, "reason": ferr.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "paneId": pane.PaneID, "sessionName": pane.SessionName, "terminal": term})
	})

	// Live pane viewer — returns the rendered buffer of the matched pane as
	// plain text (with ANSI escapes intact). The frontend polls this every
	// ~1.2s while a session is selected. Cheap: ~10ms tmux fork per call.
	mux.HandleFunc("/api/session/pane-view/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/session/pane-view/")
		pane, reg, err := resolvePane(id)
		if reg == nil {
			writeJSON(w, map[string]any{"ok": false})
			return
		}
		if err != nil || pane == nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		lines := 0
		if v := r.URL.Query().Get("lines"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				lines = n
			}
		}
		// plain=1 strips ANSI escapes for raw-text clients (iOS); the web
		// bridge omits it and renders colour.
		plain := r.URL.Query().Get("plain") == "1"
		content, err := CapturePane(pane.PaneID, lines, plain)
		if err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{
			"ok":      true,
			"paneId":  pane.PaneID,
			"content": content,
			"command": pane.CurrentCommand,
		})
	})

	// ─── Pane registry endpoints ──────────────────────────────────────────
	// POST /api/pane/register  — body: PaneRegistration JSON. Used by both
	// the Claude SessionStart hook and the wrapper's post-exec re-register.
	mux.HandleFunc("/api/pane/register", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Tool       Tool   `json:"tool"`
			SessionID  string `json:"sessionId"`
			PaneID     string `json:"paneId"`
			TmuxSocket string `json:"tmuxSocket"`
			Cwd        string `json:"cwd"`
			Pid        int    `json:"pid"`
			Source     string `json:"source"` // "hook" | "wrapper" | "manual"
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if body.Tool == "" || body.SessionID == "" || body.PaneID == "" {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "tool, sessionId, paneId required"})
			return
		}
		if body.Source == "" {
			body.Source = "hook"
		}
		reg := &PaneRegistration{
			AgentID:    sid(body.Tool, body.SessionID),
			Tool:       body.Tool,
			SessionID:  body.SessionID,
			PaneID:     body.PaneID,
			TmuxSocket: body.TmuxSocket,
			Cwd:        body.Cwd,
			Pid:        body.Pid,
			Source:     body.Source,
		}
		paneRegistry.Register(reg)
		writeJSON(w, map[string]any{"ok": true, "agentId": reg.AgentID})
	})
	// POST /api/pane/announce — wrapper-only. Records a pending pairing keyed
	// by tool/cwd; the next session of that tool from that cwd within 60s
	// inherits the pane.
	mux.HandleFunc("/api/pane/announce", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Tool       Tool   `json:"tool"`
			PaneID     string `json:"paneId"`
			TmuxSocket string `json:"tmuxSocket"`
			Cwd        string `json:"cwd"`
			Pid        int    `json:"pid"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if body.Tool == "" || body.PaneID == "" {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "tool, paneId required"})
			return
		}
		paneRegistry.AnnouncePending(pendingWrapper{
			Tool: body.Tool, PaneID: body.PaneID,
			TmuxSocket: body.TmuxSocket, Cwd: body.Cwd, Pid: body.Pid,
		})
		writeJSON(w, map[string]any{"ok": true})
	})
	// GET /api/panes — list every registered pane (for "talk to" pickers).
	mux.HandleFunc("/api/panes", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"panes": paneRegistry.List()})
	})
	// POST /api/panes/sweep — run GC + dedupe right now, then return the
	// cleaned-up registry. Cheap (~10ms) — used by the MCP list_agents tool
	// before responding so the model never sees duplicate-on-pane rows.
	mux.HandleFunc("/api/panes/sweep", func(w http.ResponseWriter, r *http.Request) {
		var gcDropped, dedupeDropped int
		if panes, err := listTmuxPanes(); err == nil {
			gcDropped = len(paneRegistry.GarbageCollect(panes))
		}
		dedupeDropped = len(paneRegistry.DedupeByPane())
		writeJSON(w, map[string]any{
			"ok":            true,
			"gcDropped":     gcDropped,
			"dedupeDropped": dedupeDropped,
			"panes":         paneRegistry.List(),
		})
	})
	// GET /api/tmux/panes — list every LIVE tmux pane on the system. Used by
	// the empty-state "Register this pane" picker for sessions that started
	// before the SessionStart hook deployed.
	mux.HandleFunc("/api/tmux/panes", func(w http.ResponseWriter, r *http.Request) {
		panes, err := listTmuxPanes()
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "panes": panes})
	})
	// /api/pane/registration/{agentId} — GET reads, DELETE forgets.
	mux.HandleFunc("/api/pane/registration/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/pane/registration/")
		switch r.Method {
		case http.MethodGet:
			reg := paneRegistry.Get(id)
			if reg == nil {
				writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "no registration"})
				return
			}
			writeJSON(w, reg)
		case http.MethodDelete:
			paneRegistry.Forget(id)
			writeJSON(w, map[string]any{"ok": true})
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	// POST /api/pane/name/{agentId}  body: {alias: "..."} — sets/clears the
	// human-friendly name used by `agent-monitor send <alias>`.
	mux.HandleFunc("/api/pane/name/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/api/pane/name/")
		var body struct {
			Alias string `json:"alias"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if err := paneRegistry.SetAlias(id, body.Alias); err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})

	// ─── Talk endpoints (agent-to-agent messaging) ───────────────────────
	// POST /api/talk/request  body: {fromAgent, toAgent, message}
	// Creates a pending talk, broadcasts to recipient's web view, returns id.
	// Sender then long-polls /api/talk/{id}/await for the outcome.
	mux.HandleFunc("/api/talk/request", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			FromAgent string `json:"fromAgent"` // sender id (or empty for "web user")
			ToAgent   string `json:"toAgent"`   // recipient resolver: id|alias|pane|sid-prefix
			Message   string `json:"message"`
			// ReplyOnly skips the Allow/Deny banner and marks the talk as
			// pending_reply. Used by talk_to_agent(wait_for_reply=true) where
			// the message is delivered directly and the talk only exists as a
			// rendezvous point for /reply.
			ReplyOnly bool `json:"replyOnly"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if body.ToAgent == "" || body.Message == "" {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "toAgent and message required"})
			return
		}
		// Resolve recipient. Anything from id/alias/pane/sid-prefix works.
		toReg := paneRegistry.ResolveRecipient(body.ToAgent)
		if toReg == nil {
			writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "recipient not found in pane registry"})
			return
		}
		fromLabel := "web"
		if body.FromAgent != "" {
			if from := paneRegistry.Get(body.FromAgent); from != nil {
				fromLabel = from.Alias
				if fromLabel == "" {
					fromLabel = body.FromAgent
				}
			} else {
				fromLabel = body.FromAgent
			}
		}
		toLabel := toReg.Alias
		if toLabel == "" {
			toLabel = toReg.AgentID
		}
		t := &Talk{
			FromAgent: body.FromAgent,
			FromLabel: fromLabel,
			ToAgent:   toReg.AgentID,
			ToLabel:   toLabel,
			Message:   body.Message,
		}
		if body.ReplyOnly {
			t.Status = "pending_reply"
		}
		id := talkStore.Register(t)
		writeJSON(w, map[string]any{"ok": true, "id": id, "toAgent": toReg.AgentID})
	})
	// POST /api/talk/{id}/respond  body: {behavior: "allow"|"deny", reason?}
	// On allow we deliver the message to the recipient's pane (fenced block
	// + Enter). On deny we just record the refusal; sender's await unblocks.
	mux.HandleFunc("/api/talk/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/talk/")
		// route: <id>/respond  OR  <id>/await
		parts := strings.SplitN(path, "/", 2)
		if len(parts) != 2 {
			http.NotFound(w, r)
			return
		}
		id, action := parts[0], parts[1]
		switch action {
		case "respond":
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			var body struct {
				Behavior string `json:"behavior"`
				Reason   string `json:"reason"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
			if body.Behavior != "allow" && body.Behavior != "deny" {
				writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "behavior must be allow|deny"})
				return
			}
			t := talkStore.Get(id)
			if t == nil {
				writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "talk not found"})
				return
			}
			if t.Status == "pending_reply" {
				writeJSONStatus(w, http.StatusConflict, map[string]any{"error": "talk is awaiting a reply, not allow/deny — the message was already delivered"})
				return
			}
			if body.Behavior == "deny" {
				_, _ = talkStore.Respond(id, "denied", body.Reason)
				writeJSON(w, map[string]any{"ok": true, "status": "denied"})
				return
			}
			// allow: deliver to recipient pane. Registry-only — no need for the
			// session to be live in the store; the pane is the real thing.
			pane, _, perr := resolvePane(t.ToAgent)
			if pane == nil {
				_, _ = talkStore.Respond(id, "error", "recipient has no live pane")
				writeJSONStatus(w, http.StatusServiceUnavailable, map[string]any{"error": "recipient pane unavailable"})
				return
			}
			_ = perr
			delivery := FormatPaneDelivery(t.FromLabel, t.Message)
			if err := SendToTmuxPane(pane.PaneID, delivery, true); err != nil {
				_, _ = talkStore.Respond(id, "error", err.Error())
				writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			_, _ = talkStore.Respond(id, "delivered", "")
			writeJSON(w, map[string]any{"ok": true, "status": "delivered", "pane": pane.PaneID})
		case "await":
			// long-poll up to 10 minutes; sender CLI uses this to block.
			timeout := 10 * time.Minute
			if v := r.URL.Query().Get("timeout"); v != "" {
				if n, err := strconv.Atoi(v); err == nil && n > 0 {
					timeout = time.Duration(n) * time.Second
				}
			}
			resp, ok := talkStore.Wait(id, timeout)
			if !ok {
				writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "talk not found"})
				return
			}
			writeJSON(w, map[string]any{"status": resp.Status, "reason": resp.Reason})
		case "reply":
			// Recipient says "I'm done — here's my answer". Unblocks any
			// /await-reply caller and resolves the talk.
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			var body struct {
				Message string `json:"message"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
			t, err := talkStore.Reply(id, body.Message)
			if err != nil {
				writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": err.Error()})
				return
			}
			writeJSON(w, map[string]any{"ok": true, "fromAgent": t.FromAgent, "toAgent": t.ToAgent})
		case "await-reply":
			// Sender long-polls here; unblocks when /reply lands or timeout.
			timeout := 10 * time.Minute
			if v := r.URL.Query().Get("timeout"); v != "" {
				if n, err := strconv.Atoi(v); err == nil && n > 0 {
					timeout = time.Duration(n) * time.Second
				}
			}
			env, ok := talkStore.AwaitReply(id, timeout)
			if !ok {
				writeJSON(w, map[string]any{"status": "timeout", "reply": ""})
				return
			}
			writeJSON(w, map[string]any{"status": "replied", "reply": env.Message})
		default:
			http.NotFound(w, r)
		}
	})
	// GET /api/talks — currently-pending incoming talks (replayable on WS).
	mux.HandleFunc("/api/talks", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"talks": talkStore.List()})
	})

	mux.HandleFunc("/ws", handleWS)

	staticFS, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatalf("embed sub: %v", err)
	}
	indexBytes, err := fs.ReadFile(staticFS, "index.html")
	if err != nil {
		log.Fatalf("embed read index.html: %v", err)
	}
	// /tv — compact "peek-a-boo" glance board of live sessions. Served on a
	// clean path so the AgentTV floating widget (a WKWebView) can point at it.
	tvBytes, err := fs.ReadFile(staticFS, "tv.html")
	if err != nil {
		log.Fatalf("embed read tv.html: %v", err)
	}
	mux.HandleFunc("/tv", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store, max-age=0")
		w.Write(tvBytes)
	})
	fileServer := http.FileServer(http.FS(staticFS))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Serve index.html directly — http.FileServer redirects /index.html to ./
		// when canonicalising URLs, which loops if we rewrite "/" to "/index.html".
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			// no-store on the SPA shell so a daemon rebuild is picked up on
			// the next page load instead of being held by the browser cache.
			w.Header().Set("Cache-Control", "no-store, max-age=0")
			w.Write(indexBytes)
			return
		}
		fileServer.ServeHTTP(w, r)
	})

	// Always bind 127.0.0.1 so local clients (the MCP permission server and
	// the `agent-monitor` CLI wrapper — both hardcode 127.0.0.1) keep working,
	// even when we ALSO expose on another interface for remote access.
	// AGENT_MONITOR_BIND adds a SECOND listener: set it to a specific Tailscale
	// IP (e.g. 100.64.x.y) to expose ONLY over the tailnet, or 0.0.0.0 for all
	// interfaces. It no longer replaces the loopback listener.
	binds := []string{"127.0.0.1"}
	if v := os.Getenv("AGENT_MONITOR_BIND"); v != "" && v != "127.0.0.1" && v != "localhost" {
		binds = append(binds, v)
		log.Printf("⚠ exposed beyond localhost on %s:%d — anyone who can reach it controls every registered tmux pane.", v, port)
		log.Printf("  Reachable URLs:")
		for _, u := range reachableURLs(port) {
			log.Printf("    %s", u)
		}
	}
	// Flush session history to SQLite on Ctrl-C / SIGTERM so an intentional
	// restart keeps the latest state instead of waiting on the 2-min ticker.
	if flushSessions != nil {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-sigCh
			log.Printf("shutting down — flushing session history…")
			flushSessions()
			os.Exit(0)
		}()
	}

	// Each listener runs on its own goroutine; the first fatal error wins.
	errCh := make(chan error, len(binds))
	for _, b := range binds {
		addr := fmt.Sprintf("%s:%d", b, port)
		log.Printf("agent-monitor listening on http://%s", addr)
		go func(a string) { errCh <- http.ListenAndServe(a, mux) }(addr)
	}
	log.Fatal(<-errCh)
}

// reachableURLs collects every IPv4 address bound to a non-loopback interface
// (filtering out link-local) and returns http URLs at the given port. Used to
// print the Tailscale / LAN URL on startup so the user doesn't have to guess.
func reachableURLs(port int) []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	out := []string{fmt.Sprintf("http://localhost:%d", port)}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ip, _, err := net.ParseCIDR(a.String())
			if err != nil || ip.To4() == nil || ip.IsLinkLocalUnicast() {
				continue
			}
			tag := ""
			// Tailscale's CGNAT range — annotate so the user can spot it instantly.
			if strings.HasPrefix(ip.String(), "100.") {
				tag = " (Tailscale)"
			}
			out = append(out, fmt.Sprintf("http://%s:%d%s", ip.String(), port, tag))
		}
	}
	return out
}

func handleWS(w http.ResponseWriter, r *http.Request) {
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	out := make(chan any, 128)
	unsubSess := store.Subscribe(func(e ServerEvent) {
		select {
		case out <- e:
		default:
		}
	})
	unsubPerm := permStore.Subscribe(func(e PermEvent) {
		select {
		case out <- e:
		default:
		}
	})
	unsubPane := paneRegistry.Subscribe(func(e PaneEvent) {
		select {
		case out <- e:
		default:
		}
	})
	unsubTalk := talkStore.Subscribe(func(e TalkEvent) {
		select {
		case out <- e:
		default:
		}
	})
	go func() {
		defer func() {
			unsubSess()
			unsubPerm()
			unsubPane()
			unsubTalk()
			close(out)
			c.Close()
		}()
		for {
			if _, _, err := c.NextReader(); err != nil {
				return
			}
		}
	}()
	for e := range out {
		if err := c.WriteJSON(e); err != nil {
			return
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
