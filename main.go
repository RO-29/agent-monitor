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
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"

	"github.com/gorilla/websocket"
)

// The Vite build in web/dist is embedded, so go build makes one binary.
//
//go:embed web/dist
var webFS embed.FS

var store *Store
var permStore *PermStore
var paneRegistry *PaneRegistry
var talkStore *TalkStore

var upgrader = websocket.Upgrader{
	CheckOrigin: sameOriginRequest,
}

func main() {
	ensureToolEnv()
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "install", "install-hooks":
			// "install" wires hooks and MCP entries for Claude and Codex; "install-hooks" is an alias.
			installAll()
			return
		case "wire":
			os.Exit(runWireCmd(os.Args[2:]))
		case "unwire":
			os.Exit(runUnwireCmd(os.Args[2:]))
		case "mcp-perm-server", "run", "__pty-leader":
			// The Rust am-helper next to this binary serves these modes; there is no Go copy.
			execRustHelper(os.Args[1:])
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
			// Stop kills only the listener; pkill would also kill mcp-perm-server children and break their MCP transport.
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

	startMemGuard()
	store = NewStore()
	permStore = NewPermStore()
	paneRegistry = NewPaneRegistry()
	hostRegistry = NewHostRegistry(filepath.Join(stateDir(), "hosts.json"))
	talkStore = NewTalkStore()
	// Drop registrations whose pane is gone, at startup and then on a ticker.
	if panes, err := listTmuxPanes(); err == nil {
		if dropped := paneRegistry.GarbageCollect(panes); len(dropped) > 0 {
			log.Printf("pane GC: dropped %d orphan registration(s)", len(dropped))
		}
	}
	// One-shot sweep: keep the newest registration per pane.
	if dropped := paneRegistry.DedupeByPane(); len(dropped) > 0 {
		log.Printf("pane dedupe: dropped %d duplicate-on-pane registration(s)", len(dropped))
	}
	go func() {
		// 15s: notices hookless CLIs exiting without forking ps and tmux too often.
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
	// Pair wrapper announcements with the next session of the same tool and cwd.
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
	loadSettings()
	// Scanning every transcript takes seconds, so the listener starts first and sessions fill in.
	var flushMu sync.Mutex
	var flushSessions func()
	go func() {
		flush := loadSessionSources(store)
		flushMu.Lock()
		flushSessions = flush
		flushMu.Unlock()
	}()

	mux := http.NewServeMux()
	registerTraceRoutes(mux)
	registerV3Routes(mux)
	startWiring()
	mux.HandleFunc("/api/sessions", func(w http.ResponseWriter, r *http.Request) {
		all := store.All()
		// ?waiting=1 returns only sessions waiting on the user; the full list is several MB.
		if r.URL.Query().Get("waiting") == "1" {
			all = waitingSessions(all)
		}
		if r.URL.Query().Get("slim") == "1" {
			all = slimSessions(all) // the iPhone list; ended sessions without their event history
		}
		writeJSON(w, map[string]any{"sessions": all})
	})
	// /api/chains — related-session graph (continuation chains). Precise linkage
	// by shared handoff/resume plan reference + same-pane continuity.
	mux.HandleFunc("/api/chains", func(w http.ResponseWriter, r *http.Request) {
		panes := map[string]string{}
		for _, reg := range paneRegistry.List() {
			panes[reg.AgentID] = reg.PaneID
		}
		sessions := store.All()
		// Chains are now derived from threads (explicit continuations only);
		// the response shape is unchanged for the iOS app and older clients.
		threads, _ := computeThreads(sessions, panes)
		chains, chainOf := chainsFromThreads(threads, sessions)
		if chains == nil {
			chains = []Chain{}
		}
		parentOf, childrenOf := matchSpawns(sessions)
		writeJSON(w, map[string]any{
			"chains": chains, "chainOf": chainOf,
			"spawnParent": parentOf, "spawnChildren": childrenOf,
		})
	})
	mux.HandleFunc("/api/session/", func(w http.ResponseWriter, r *http.Request) {
		// /api/session/<id>/full
		path := strings.TrimPrefix(r.URL.Path, "/api/session/")
		if watchedSessionRoute(w, r, path) {
			return
		}
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
		// Live tmux panes and drivable panes, for the crowded-terminal warning.
		tmuxPanes := 0
		if ps, err := listTmuxPanes(); err == nil {
			tmuxPanes = len(ps)
		}
		writeJSON(w, map[string]any{
			"ok": true, "sessions": len(store.All()), "loading": !sessionsLoaded.Load(),
			"tmuxPanes": tmuxPanes, "registeredPanes": len(paneRegistry.List()),
			"machine": selfMachineName(), "level": callerLevel(r).String(),
		})
	})
	hookHandler := func(tool Tool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			var p ClaudeHookPayload
			if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
				writeJSONStatus(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
				return
			}
			tool, p = cursorHookAsCursor(tool, p)
			if tool == ToolCursor && claudeOwnsID(p.SessionID) {
				// A Cursor CLI started inside a Claude Code session inherits that session's id: not a chat of its own.
				writeJSON(w, map[string]any{"ok": true, "ignored": "inherited claude session id"})
				return
			}
			if tool == ToolCursor {
				adoptCursorPane(p.SessionID)
			}
			if tool == ToolCursor && p.HookEventName == "Stop" {
				// Cursor at its prompt is idle, not a terminal question waiting for an answer.
				store.SetState(ToolCursor, p.SessionID, StateIdle, "")
			} else {
				handleHook(store, tool, p)
			}
			// Hooks run under `agent-monitor run` name their host, linking the session to it.
			if hid := r.URL.Query().Get("host"); hid != "" && hostRegistry != nil {
				ppid, _ := strconv.Atoi(r.URL.Query().Get("ppid"))
				hostRegistry.LinkFromHook(hid, tool, p.SessionID, ppid)
			}
			if approvalStore != nil && p.SessionID != "" {
				approvalStore.terminalEvent(tool, p.SessionID, p.HookEventName, p.ToolName)
			}
			writeJSON(w, map[string]any{"ok": true})
		}
	}
	mux.HandleFunc("/events/claude", hookHandler(ToolClaude))
	mux.HandleFunc("/events/codex", hookHandler(ToolCodex))
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
		// The same allow and deny rules as hook approvals; a match is answered with no card.
		if approvalStore != nil {
			a := &Approval{ID: "perm-" + randHex(6), Agent: "claude", Kind: "permission", ToolName: body.ToolName, Input: body.Input, Cwd: body.Cwd, CreatedAt: time.Now().UnixMilli()}
			if d := approvalStore.ruleDecision(a); d != nil {
				permStore.RegisterDecided(req, PermissionResponse{Behavior: d.Behavior, Reason: d.Message})
				writeJSON(w, map[string]any{"id": req.ID})
				return
			}
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
		resp, ok := permStore.Wait(r.Context(), id, timeout)
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
	// resolvePane uses the registry, so a pane stays drivable after its session decays from memory.
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

	// Send text or Ctrl-C to a session's pane; the pane is resolved on each call.
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
		if drivenSessionAction(w, id, "send", body.Text) {
			return
		}
		rt, code, err := withTerminal(id, func(t string) error { return targetText(t, body.Text, body.Enter) })
		if err != nil {
			writeJSONStatus(w, code, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "pane": rt.PaneID, "session": rt.SessionName, "host": rt.HostID})
	})
	// Send tmux key names to the pane, for example {keys: ["1", "Enter"]}.
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
		rt, code, err := withTerminal(id, func(t string) error { return targetKeys(t, body.Keys) })
		if err != nil {
			writeJSONStatus(w, code, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "pane": rt.PaneID, "host": rt.HostID, "keys": body.Keys})
	})
	mux.HandleFunc("/api/session/cancel/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/api/session/cancel/")
		if drivenSessionAction(w, id, "cancel", "") {
			return
		}
		rt, code, err := withTerminal(id, func(t string) error { return targetKeys(t, []string{"C-c"}) })
		if err != nil {
			writeJSONStatus(w, code, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "pane": rt.PaneID, "host": rt.HostID})
	})
	// The registered pane for a session; no heuristic fallback.
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

	// Focus selects the pane, switches the tmux client and raises the terminal app.
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

	// Rendered pane buffer with ANSI escapes, polled by the frontend while a session is open.
	mux.HandleFunc("/api/session/pane-view/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/session/pane-view/")
		lines := 0
		if v := r.URL.Query().Get("lines"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				lines = n
			}
		}
		// plain=1 strips ANSI escapes for raw-text clients (iOS); a host always answers plain text.
		plain := r.URL.Query().Get("plain") == "1"
		var content string
		rt, code, err := readTerminal(id, func(t string) error {
			c, err := targetCapture(t, lines, plain)
			content = c
			return err
		})
		if err != nil {
			if code == http.StatusNotFound {
				writeJSON(w, map[string]any{"ok": false})
				return
			}
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{
			"ok":      true,
			"paneId":  rt.PaneID,
			"hostId":  rt.HostID,
			"plain":   rt.HostID != "" || plain,
			"content": content,
			"command": rt.Command,
		})
	})

	// POST /api/pane/register: body is a PaneRegistration, from the SessionStart hook or the wrapper.
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
		if body.Tool == ToolClaude && cursorChatExists(body.SessionID) {
			body.Tool = ToolCursor // Cursor CLI runs Claude Code hooks with its own chat id
		}
		// Codex runs a background memories session in the same pane; it must not take the pane from the real one.
		if codexInternalCwd(body.Tool, body.Cwd) {
			writeJSON(w, map[string]any{"ok": true, "ignored": "codex internal session"})
			return
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
	// POST /api/pane/announce: the next session of that tool and cwd within 60s inherits the pane.
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
	// POST /api/panes/sweep: GC and dedupe now, then return the registry.
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
	// GET /api/tmux/panes: every live tmux pane, for the register-this-pane picker.
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

	// POST /api/talk/request {fromAgent, toAgent, message}: the sender long-polls /api/talk/{id}/await.
	mux.HandleFunc("/api/talk/request", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			FromAgent string `json:"fromAgent"` // sender id (or empty for "web user")
			ToAgent   string `json:"toAgent"`   // recipient resolver: id|alias|pane|sid-prefix
			Message   string `json:"message"`
			// ReplyOnly skips the Allow/Deny banner; the talk is a rendezvous for /reply.
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
			// A hosted session has no pane registration.
			if tg, err := resolveTargetIn(sessionTargets(), body.ToAgent); err == nil {
				toReg = &PaneRegistration{AgentID: tg.AgentID, Tool: tg.Tool, SessionID: tg.SessionID, Alias: tg.Alias}
			}
		}
		if toReg == nil {
			writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "recipient not found among tmux panes and session hosts"})
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
	// POST /api/talk/{id}/respond {behavior, reason?}: allow delivers to the pane, deny records the refusal.
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
			delivery := FormatPaneDelivery(t.FromLabel, t.Message)
			rt, code, err := withTerminal(t.ToAgent, func(tg string) error { return targetText(tg, delivery, true) })
			if err != nil {
				if rt == nil {
					_, _ = talkStore.Respond(id, "error", "recipient has no live pane")
					writeJSONStatus(w, http.StatusServiceUnavailable, map[string]any{"error": "recipient pane unavailable"})
					return
				}
				_, _ = talkStore.Respond(id, "error", err.Error())
				writeJSONStatus(w, code, map[string]any{"error": err.Error()})
				return
			}
			_, _ = talkStore.Respond(id, "delivered", "")
			writeJSON(w, map[string]any{"ok": true, "status": "delivered", "pane": rt.PaneID, "host": rt.HostID})
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

	staticFS, err := fs.Sub(webFS, "web/dist")
	if err != nil {
		log.Fatalf("embed sub: %v", err)
	}
	indexBytes, err := fs.ReadFile(staticFS, "index.html")
	if err != nil {
		log.Fatalf("embed read index.html: %v", err)
	}
	fileServer := http.FileServer(http.FS(staticFS))
	// SPA: hashed assets from web/dist; every other path gets the shell for client routing.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if strings.HasPrefix(p, "api/") { // an unknown API path is an error, not the app page
			writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "no such API route"})
			return
		}
		if p != "" && p != "index.html" {
			if f, err := staticFS.Open(p); err == nil {
				f.Close()
				if strings.HasPrefix(p, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				fileServer.ServeHTTP(w, r)
				return
			}
			// A chunk from an older build is gone: a 404, not the HTML shell, so the page knows to reload.
			if strings.HasPrefix(p, "assets/") {
				http.NotFound(w, r)
				return
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// no-store on the SPA shell so a daemon rebuild is picked up on the
		// next page load instead of being held by the browser cache.
		w.Header().Set("Cache-Control", "no-store, max-age=0")
		w.Write(indexBytes)
	})

	// Loopback is always bound for local clients; AGENT_MONITOR_BIND adds a second listener (tailnet IP or 0.0.0.0).
	binds := []string{"127.0.0.1"}
	remoteExposed := false
	if v := os.Getenv("AGENT_MONITOR_BIND"); v != "" && v != "127.0.0.1" && v != "localhost" {
		binds = append(binds, v)
		remoteExposed = true
		log.Printf("⚠ exposed beyond localhost on %s:%d — anyone who can reach it controls every registered tmux pane.", v, port)
		log.Printf("  Reachable URLs:")
		for _, u := range reachableURLs(port) {
			log.Printf("    %s", u)
		}
	}

	// Password-gate non-loopback traffic. Loopback (local agents, hooks, MCP,
	// CLI) is always exempt, so this only affects LAN/Tailscale/public access.
	initAuth(remoteExposed)
	handler := hostGuard(originGuard(authMiddleware(gzipJSON(mux))))
	// Ctrl-C / SIGTERM (agent-monitor stop): stop driven children, which run in their own
	// process groups, then flush session history and the orchestrator store.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Printf("shutting down: stopping driven agents and flushing state")
		if orchMgr != nil {
			orchMgr.Shutdown()
			_ = orchMgr.store.Close()
		}
		flushMu.Lock()
		flush := flushSessions
		flushMu.Unlock()
		if flush != nil {
			flush()
		}
		runShutdownHooks()
		os.Exit(0)
	}()

	if exe, err := os.Executable(); err == nil && helperFor(exe) == exe {
		log.Printf("WARNING: %s is missing next to %s; terminal sessions and the MCP permission server will not start. Build it with rust/am-helper/build.sh", rustHelperName, exe)
	}
	// Each listener runs on its own goroutine; the first fatal error wins.
	errCh := make(chan error, len(binds))
	for _, b := range binds {
		addr := fmt.Sprintf("%s:%d", b, port)
		log.Printf("agent-monitor listening on http://%s", addr)
		go func(a string) { errCh <- http.ListenAndServe(a, handler) }(addr)
	}
	log.Fatal(<-errCh)
}

// sessionsLoaded is set once the startup transcript scan and session history load finish.
var sessionsLoaded atomic.Bool

// loadSessionSources seeds the store from every agent's transcripts, then restores history; it returns the shutdown flush.
func loadSessionSources(s *Store) (flush func()) {
	start := time.Now()
	defer func() {
		sessionsLoaded.Store(true)
		log.Printf("sessions loaded in %v", time.Since(start).Round(time.Millisecond))
	}()
	seeds.begin()
	var wg sync.WaitGroup
	for _, adapter := range []func(*Store){startClaudeAdapter, startCodexAdapter, startOpencodeAdapter, startCursorAdapter, startCursorAgentAdapter, startCursorDesktopWatcher} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			adapter(s)
		}()
	}
	wg.Wait()
	seeds.end()
	startSeedStateRefresh(10 * time.Minute)
	// Durable history: the store is mirrored to SQLite every 2 minutes and on shutdown.
	if db, err := OpenSessionDB(); err != nil {
		log.Printf("sessions.db: disabled (%v)", err)
		initTraceTables(nil)
	} else {
		flush = StartSessionPersistence(s, db, 2*time.Minute)
		initTraceTables(db)
	}
	// Trace summaries are computed off the request path and cached in SQLite.
	startSummaryWorkers(s)
	return flush
}

// reachableURLs lists http URLs on non-loopback, non-link-local IPv4 addresses, for the startup banner.
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
	// out is never closed: a bus can still call a listener after unsubscribe; done ends the writer.
	out, done, full := make(chan any, 1024), make(chan struct{}), make(chan struct{})
	var fullOnce sync.Once
	send := func(e any) {
		select {
		case <-done:
		case out <- e:
		default:
			// A dropped event would leave a stale card; close the socket so the client reconnects and resyncs.
			fullOnce.Do(func() { close(full) })
		}
	}
	unsubSess := store.Subscribe(func(e ServerEvent) { send(e) })
	unsubPerm := permStore.Subscribe(func(e PermEvent) { send(e) })
	unsubPane := paneRegistry.Subscribe(func(e PaneEvent) { send(e) })
	unsubTalk := talkStore.Subscribe(func(e TalkEvent) { send(e) })
	unsubTrace := traceBus.subscribe(func(e TraceEvent) { send(e) })
	unsubV3 := v3Bus.subscribe(func(e V3Event) { send(e) })
	go func() {
		defer func() {
			unsubV3()
			unsubSess()
			unsubPerm()
			unsubPane()
			unsubTalk()
			unsubTrace()
			close(done)
			c.Close()
		}()
		for {
			if _, _, err := c.NextReader(); err != nil {
				return
			}
		}
	}()
	writeWait := 60 * time.Second
	for {
		select {
		case <-done:
			return
		case <-full:
			c.Close()
			return
		case e := <-out:
			// A stalled peer must not hold the buffer; the first write is the snapshot, which needs longer on a slow link.
			_ = c.SetWriteDeadline(time.Now().Add(writeWait))
			writeWait = 10 * time.Second
			if err := c.WriteJSON(e); err != nil {
				c.Close() // the reader then returns and unsubscribes
				return
			}
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

// waitingSessions keeps the sessions that wait on the user.
func waitingSessions(all []*Session) []*Session {
	out := make([]*Session, 0, 4)
	for _, s := range all {
		if (s.State == StateAwaitingInput || s.State == StateAwaitingPermission) && !cursorEcho(s) {
			out = append(out, s)
		}
	}
	return out
}

// codexInternalCwd is true for sessions Codex runs for itself, such as memories, inside its home folder.
func codexInternalCwd(tool Tool, cwd string) bool {
	if tool != ToolCodex || cwd == "" {
		return false
	}
	home := filepath.Clean(codexHome())
	c := filepath.Clean(cwd)
	return c == home || strings.HasPrefix(c, home+string(filepath.Separator))
}

// Cursor CLI runs Claude Code hooks with its own event names (sessionStart, stop) and its chat id.
var cursorHookEvents = map[string]string{"sessionStart": "SessionStart", "sessionEnd": "SessionEnd", "stop": "Stop",
	"beforeSubmitPrompt": "UserPromptSubmit", "preToolUse": "PreToolUse", "postToolUse": "PostToolUse", "subagentStop": "SubagentStop"}

func cursorHookAsCursor(tool Tool, p ClaudeHookPayload) (Tool, ClaudeHookPayload) {
	if tool != ToolClaude || p.HookEventName == "" || !unicode.IsLower(rune(p.HookEventName[0])) {
		return tool, p
	}
	if ev, ok := cursorHookEvents[p.HookEventName]; ok {
		p.HookEventName = ev
	}
	return ToolCursor, p
}

// adoptCursorPane moves a pane filed under the Claude-hook id of a Cursor chat to the Cursor session.
func adoptCursorPane(chatID string) {
	if paneRegistry == nil || chatID == "" {
		return
	}
	old := paneRegistry.Get(sid(ToolClaude, chatID))
	if old == nil || old.Tool != ToolClaude {
		return
	}
	reg := *old
	reg.Tool, reg.AgentID, reg.RegisteredAt = ToolCursor, sid(ToolCursor, chatID), 0
	paneRegistry.Forget(old.AgentID)
	paneRegistry.Register(&reg)
}

// claudeOwnsID is true when a Claude Code session with a transcript has this id.
func claudeOwnsID(id string) bool {
	if store == nil || id == "" {
		return false
	}
	c := store.Get(sid(ToolClaude, id))
	return c != nil && c.TranscriptPath != ""
}

// cursorEcho is a Cursor record made from an inherited Claude session id (see claudeOwnsID).
func cursorEcho(s *Session) bool {
	return s.Tool == ToolCursor && claudeOwnsID(s.SessionID) && !cursorChatExists(s.SessionID)
}

// cursorChatExists is true when ~/.cursor/chats holds a chat with this id.
func cursorChatExists(id string) bool {
	if id == "" || strings.ContainsAny(id, "/*?[") {
		return false
	}
	if cursorChatIDs(10 * time.Second)[id] {
		return true
	}
	return cursorChatIDs(time.Second)[id] // a chat Cursor made a moment ago
}

// The thread list asks once per session on every request; one listing every 10 s answers them all.
var cursorIDCache struct {
	sync.Mutex
	at  time.Time
	dir string
	ids map[string]bool
}

func cursorChatIDs(maxAge time.Duration) map[string]bool {
	root := filepath.Join(homeDir(), ".cursor", "chats")
	cursorIDCache.Lock()
	defer cursorIDCache.Unlock()
	if cursorIDCache.ids != nil && cursorIDCache.dir == root && time.Since(cursorIDCache.at) < maxAge {
		return cursorIDCache.ids
	}
	ids := map[string]bool{}
	wss, _ := os.ReadDir(root)
	for _, ws := range wss {
		chats, _ := os.ReadDir(filepath.Join(root, ws.Name()))
		for _, c := range chats {
			ids[c.Name()] = true
		}
	}
	cursorIDCache.at, cursorIDCache.dir, cursorIDCache.ids = time.Now(), root, ids
	return ids
}
