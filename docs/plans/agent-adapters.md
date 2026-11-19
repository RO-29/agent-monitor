# Plan: every agent, every surface

Branch `feat/agent-adapters` in worktree `~/agent-monitor-adapters`. Base: `main` at 3ba0ee0.

## Goal

Every coding agent on this Mac can:

1. receive a message from the user or from another agent,
2. show its conversation and its open permission prompt in agent-monitor,
3. have that prompt answered from agent-monitor (phone, web, Mac app),
4. call `talk_to_agent` and the other agent-monitor MCP tools.

The user does nothing except run the daemon. The one exception is typing into a GUI app that has no
outside input path (Cursor desktop chat, Claude desktop, Claude extension in VS Code): that needs a
one-time macOS Accessibility grant, and it is phase 8, built only after the user agrees.

## Fixed rules for the whole build

- No build or test on this Mac. Every build and test runs on rohit-mac via `bin/remote-build.sh <sha> --test`.
- Commit and push before each build. Run the privacy scan on every staged diff.
- No new Go dependency. Config writers handle JSON, TOML and YAML with targeted edits.
- Code comments: one or two lines.
- Nothing writes to an app bundle, `state.vscdb`, or a VS Code extension folder. No debug port, no signal to
  another process, no TCC bypass.

## Safety rules for the config writer (phase 1, used by every later phase)

Every write into another agent's config file follows these rules:

1. Read and parse the file. If it does not parse, write nothing and report `skipped: parse error`.
2. If our entry exists with the same content, write nothing. If it exists with different content that
   the user edited (it does not match any value we ever wrote, see the ledger), write nothing and report
   `skipped: user-edited`.
3. Before the first write of a file each day, copy it to `<file>.bak-am-<YYYY-MM-DD>` (mode kept).
4. Write to a temp file in the same directory, fsync, then rename. Keep the original mode.
5. Record each write in `~/.config/agent-monitor/wiring.json` (file, key, value hash, time).
   `agent-monitor unwire` removes exactly those entries and nothing else.
6. Setting `autoWire` (default true) turns the start-time wiring off. `agent-monitor wire --dry-run` prints
   the planned changes and writes nothing.
7. YAML: only append a new top-level block when the key is absent. When the key exists, report
   `manual: add <snippet>` and write nothing.

## Phase 1: agent registry and start-time wiring

New `agent_registry.go`:

```go
type agentKind struct {
    ID       string   // "claude", "codex", "cursor-agent", "pi", "hermes", "opencode", ...
    Binaries []string // names searched on PATH and known install dirs
    Wire     []wireStep // MCP entry, hook entry, extension file
    Prompt   *promptMatcher // terminal prompt shape and answer keys; nil if a hook answers
    ACP      *acpAgent // nil when the agent has no ACP mode
}
```

- `wireAll(dry bool) []wireResult` runs at daemon start in a goroutine, then every 10 minutes (new installs).
  Each result: agent, file, action (`added`, `unchanged`, `skipped: <why>`, `manual: <snippet>`), and
  `restart needed` for sessions that started before the write.
- `GET /api/agents/wiring` returns the results. The web Settings page shows them (read-only list).
- `installAll()` becomes a thin call to `wireAll(false)` with the old printout.
- The hook and MCP commands point to the install path from settings (`installDir`, default
  `~/agent-monitor`), never to `os.Executable()`: a test daemon on rohit-mac (`~/am-build`) must not rewrite
  the live entries. Wiring runs only in the daemon whose binary sits in `installDir`.
- The old installers call `os.Exit` on a missing file. They return errors instead, so wiring can never stop
  the daemon.
- Before the rename, compare the file's mtime and size with the values read in step 1; if the user saved
  the file in between, retry once, then report `skipped: changed during write`.
- Remove the `claude or codex` limit in `upsertRule` (approvals.go): any registry ID is valid.
- `rust/am-helper/src/mcp.rs`: `looks_like_agent` and `guess_tool` take the registry's list (passed from the
  daemon at helper start, with a built-in fallback list), so `register_self` from pi or hermes gets a kind.
- `place.go`: label `claude-vscode` sessions ("VS Code: Claude extension").

Agents wired in phase 1 (MCP entry only; hooks come in their own phase): Claude, Codex, cursor-agent (move
`bin/install-cursor-hooks.py` logic into Go), pi (`~/.pi/agent/mcp.json`), opencode
(`~/.config/opencode/opencode.json` `mcp`), Hermes (`~/.hermes/config.yaml` `mcp_servers`), and for the
agents not installed here, the writers exist but run only when the binary is found: Gemini, Amp, Goose,
Qwen, Kiro, Crush, Grok.

Tests: one table test per writer with a temp HOME and golden files: absent file, empty file, our entry
present, user-edited entry, parse error, YAML key present. A test that `unwire` restores the exact bytes.

## Phase 2: Codex IPC socket (VS Code extension and the Codex app)

New `codex_ipc.go`, client for `~/.codex/ipc/ipc.sock` (4-byte little-endian length, then JSON).

1. Read the protocol from the installed extension's `out/extension.js` first and write it down in
   `docs/codex-ipc.md` with the extension version.
2. Read-only client: `initialize`, then follow `thread-stream-state-changed` snapshots and patches. Map each
   thread to an agent-monitor place, so GUI Codex threads appear with live state.
3. Send and answer: `thread-follower-start-turn`, `-command-approval-decision`, `-file-approval-decision`,
   `-permissions-request-approval-response`. Enabled only when the extension version and each method's
   version match the values in `docs/codex-ipc.md`; else read-only with a note on the place.
4. One client connection only; reconnect with backoff; never hold the socket when the bus has no threads.

Risk: undocumented protocol. A second client could disturb the extension; the first live test is
read-only and watches the extension log for errors.

## Phase 3: pi extension

The daemon writes `~/.pi/agent/extensions/agent-monitor.ts` (plain source, owned by the ledger):

- `tool_call` handler: POST the call to the daemon's permission endpoint and wait for the answer; return
  `{block: true, reason}` on deny. Timeout: fall back to pi's own behaviour.
- Auth: the extension reads the local control token from `~/.config/agent-monitor/` at load (mode 0600,
  same file the hooks use) and never writes it into the extension source.
- Inbound messages: long-poll the daemon for this session's queue and deliver with
  `pi.sendUserMessage(text, {deliverAs: "steer"})`.
- Session discovery: read `~/.pi/agent/sessions/*.jsonl` for the place list and transcript.

Live after `/reload` in pi, or on the next pi start.

## Phase 4: opencode

- Session reader: keep the JSON storage reader (1.1.x, installed here), add the SQLite `opencode.db`
  reader for 1.2 and later (read-only, modernc sqlite already in the module graph).
- HTTP adapter: find the TUI's server port (from the process and its listening socket), then
  `POST /session/:id/prompt_async` to send, `GET /event` to follow, `POST /session/:id/permissions/:id` to
  answer. When no port is found, fall back to tmux keys.

## Phase 5: Hermes

- MCP entry (phase 1), session reader for `~/.hermes/state.db` (read-only).
- Prompt matcher and keys `o` once, `s` session, `a` always, `d` deny (Enter means deny: never send Enter
  alone to a Hermes prompt).
- ACP built-in `hermes acp` for daemon-started threads.

## Phase 6: ACP built-ins and native hooks for agents not installed here

ACP: `goose acp`, `kiro-cli acp`, `grok agent stdio`, Qwen, `pi-acp`. Hooks: Qwen and Kiro
`PermissionRequest`, Crush `PreToolUse`, Amp plugin `tool.call`, Gemini policy TOML. Each ships with a
test against the documented shape and stays marked `unverified` in `/api/agents/wiring` until a live run.

## Phase 7: terminal prompt matchers

aider (`y/n/a/s/d` then Enter), Goose, Grok, Gemini, muse. A matcher is added only from a real captured
pane (fixture file in `testdata/prompts/<agent>.txt`). Agents with no capture keep the generic "open the
terminal" card.

## Phase 8 (needs the user's yes): Accessibility for GUI apps

The signed `agent-monitor.app` asks for Accessibility once. Then: Cursor desktop chat (type and send; Enter
runs, Escape skips a pending card), Claude desktop and the VS Code Claude extension (type and send).
Watch Cursor's `desktop_bridge` gate: when Cursor turns it on, use `cursor desktop send` instead of typing.

## Order and gates

Each phase: code, tests, commit, push, `remote-build.sh --test` on rohit-mac, a Codex review of the phase
diff, then merge to `main` (fast-forward) and give the user the install command. A phase that fails its
gate does not merge.

## Not verified at plan time

The Codex IPC write methods, opencode port discovery, pi's extension API field names (read from the
installed pi 1.5.0 docs before coding), hooks for the agents that are not installed, and which "muse" the
user means.
