# Plan: every agent, every surface

Branch `feat/agent-adapters` in worktree `~/agent-monitor-adapters`. Base: `main` at 3ba0ee0.
Revision 3: changed after two Fable reviews (13 plus 4 required changes, all applied below).

## Goal

Every coding agent on this Mac can:

1. receive a message from the user or from another agent,
2. show its conversation and its open permission prompt in agent-monitor,
3. have that prompt answered from agent-monitor (phone, web, Mac app),
4. call `talk_to_agent` and the other agent-monitor MCP tools.

The user does nothing except run the daemon, after one confirmation of the first wiring run (see
"First run"). The one other exception is typing into a GUI app with no outside input path (Cursor desktop
chat, Claude desktop, Claude extension in VS Code): that needs a one-time macOS Accessibility grant, and it
is phase 8, built only after the user agrees.

## Fixed rules for the whole build

- No build or test on this Mac. Every build and test runs on rohit-mac via `bin/remote-build.sh <sha> --test`.
  Phase 1 extends that script: `--test` also runs `cargo test` in `rust/am-helper`, and prints the full
  failing output, not only the last line.
- Commit and push before each build. Privacy scan on every staged diff, now as `bin/privacy-scan.sh`
  (added in phase 1, the same grep as before, exit 1 on a hit). It is checked once against a known bad
  input before it counts as a gate.
- No new Go dependency. JSON edits splice bytes; TOML and YAML edits only append (see writer rules).
- Code comments: one or two lines.
- Nothing writes to an app bundle, `state.vscdb`, or a VS Code extension folder. No debug port, no signal to
  another process, no TCC bypass.

## Writer rules (phase 1, used by every later phase)

1. **Scope.** Wiring adds MCP server entries and hook entries only. It never changes permissions or approval
   settings: `installClaudePermissions` (adds `Bash`, `Read`, `Write`, `Edit` to `permissions.allow`) and
   `installCodexApproval` (sets `approval_mode = "auto"`) stay out of `wireAll` and remain manual commands.
2. **Adoption.** An existing entry with the same `command` and `args` (path compared after resolving
   symlinks and `~`) is adopted into the ledger as ours. Other keys in it (pi `exposure`, Codex
   `.tools.*` subtables) are left alone and do not count as a difference.
3. **User edits.** An entry with our name but a different `command`/`args` is `skipped: user-edited`.
4. **Tombstone.** When the ledger has an entry and the file does not, the user removed it. The entry is
   marked removed in the ledger and never added again until `agent-monitor wire --force <agent>`.
5. **JSON keeps bytes.** No map round trip. The writer finds the target object with a token scanner and
   splices the new member in, so key order, escapes and formatting of the rest of the file stay byte for
   byte. If the scanner cannot place it, report `manual: <snippet>`. `~/.claude.json` (live sessions rewrite
   it) is written with `~/.local/bin/claude mcp add --scope user` (resolved path, never a shell function or
   PATH lookup), else skipped. `mcp add`/`mcp remove` flags are checked against `--help` on rohit-mac first.
6. **TOML and YAML append only.** Without a parser, the writer cannot detect a parse error, a quoted header
   or an inline table. Rule: append a new block only when our server name appears nowhere in the file
   (plain substring check), else `manual: <snippet>`. Codex MCP entries prefer `~/.local/bin/codex mcp add`.
   CLI writes follow the same backup, ledger and before/after checks as direct writes.
7. **Codex hooks.** `hooks.json` entries are appended, never reordered or rewritten. The writer never
   touches `trusted_hash` in `config.toml`. A new hook is reported `needs trust in Codex` until Codex
   records a hash for it.
8. **Hook dedupe** matches on the resolved script path (any path whose base name is `claude-hook.sh`,
   `codex-hook.sh` or `cursor-hook.sh` under any agent-monitor checkout), never on the exact command string.
9. **Backups.** Before the first write of a file each day: `<file>.bak-am-<YYYY-MM-DD>`, same mode as the
   original (0600 stays 0600). Keep the newest 5 `.bak-am-*` per file. The existing `.bak-*` files from the
   old installers are listed in `/api/agents/wiring` but not deleted.
10. **Atomic write.** Temp file in the same directory, fsync, mode kept, rename. Before the rename compare
    mtime and size with the read; if the file changed, retry once, then `skipped: changed during write`.
11. **Ledger** at `stateDir()/wiring.json` (`~/.agent-monitor/wiring.json`). Each entry records `added` or
    `adopted`. `agent-monitor unwire` removes only `added` entries, never `adopted` ones: JSON by splice,
    Claude and Codex by `mcp remove`, a TOML or YAML block only when its bytes still match what we wrote,
    else `manual: remove <snippet>`.
12. **Paths.** Hook and MCP commands use `installDir` (setting, default `~/agent-monitor`), never
    `os.Executable()`. Wiring runs only in the daemon whose binary sits in `installDir`, so a test daemon on
    rohit-mac (`~/am-build`) never rewrites live entries. Old installers return errors instead of `os.Exit`.

## First run

`autoWire` is recorded per kind of write: `mcp` (an MCP server entry), `hook` (a hook entry that runs our
script), `extension` (a code file the agent loads, such as the pi extension). Each kind is `ask` (default),
`on` or `off`. For each kind in `ask`, the daemon runs `wireAll(dry=true)` and shows one card in the web app
and the phone that lists the exact files and entries of that kind. One tap sets that kind to `on`; a new
kind asks again. Later installs of an approved kind are wired with no question. The confirm route needs the
admin level (`access.go`), so a phone at the control level cannot approve it. The global rule forbids silent
persistent changes, so this card is the one step.

## Phase 1: agent registry and wiring

New `agent_registry.go`:

```go
type agentKind struct {
    ID       string         // "claude", "codex", "cursor-agent", "pi", "hermes", "opencode", ...
    Binaries []string       // names searched on PATH and known install dirs
    Wire     []wireStep     // MCP entry, hook entry
    Prompt   *promptMatcher // terminal prompt shape and answer keys; nil if a hook answers
    ACP      *acpAgent      // nil when the agent has no ACP mode
}
```

- `wireAll(dry bool) []wireResult` at daemon start (goroutine) and every 10 minutes. Result: agent, file,
  action (`added`, `adopted`, `unchanged`, `removed by user`, `skipped: <why>`, `manual: <snippet>`,
  `needs trust in Codex`), and `restart needed` for sessions older than the write.
- `GET /api/agents/wiring`; the web Settings page lists it.
- `installAll()` becomes `wireAll(false)` plus the two manual permission installers, as today.
- Remove the `claude or codex` limit in `upsertRule` (`approvals.go:767`): any registry ID is valid.
- `rust/am-helper/src/mcp.rs`: `looks_like_agent` and `guess_tool` read the registry list from the daemon
  at helper start, with a built-in fallback list including pi and hermes.
- `place.go`: label `claude-vscode` sessions "VS Code: Claude extension".
- Hardcoded `/Users/rohit/bin/tmux-pane-id.sh` in `bin/codex-hook.sh:25` and `rust/am-helper/src/hook.rs:11`
  becomes `$HOME/bin/tmux-pane-id.sh` with a fallback to the copy in `installDir/bin`.

Scope in phase 1: MCP entries for Claude, Codex, cursor-agent, pi, opencode, Hermes, and (only when the
binary exists) Gemini, Amp, Goose, Qwen, Kiro, Crush, Grok. Hook entries: Claude and Cursor (the Python
installer logic moves into Go with path-based dedupe), and Codex `hooks.json` (new Go owner, append only).

Tests (on rohit-mac): table tests per writer with a temp HOME: absent file, empty file, identical entry,
adopted entry with extra keys, user-edited entry, tombstone, JSON with odd key order and `<>&` (bytes kept),
TOML name present elsewhere, YAML key present, file changed during write, `unwire` restores the exact bytes.
Fixtures copy the shapes of the real files on this Mac with values replaced.

## Phase 2: pi

pi has no approval prompt of its own (`docs/security.md:3`): every tool runs unless an extension blocks it.

- The daemon writes `~/.pi/agent/extensions/agent-monitor.ts` only after the first-run confirmation.
- Policy lives in the daemon (rules and `classifyRisk`, `approvals_risk.go:140`), so the extension asks the
  daemon for each call (`POST`, short timeout). The daemon answers `run`, `deny` (a deny rule: block at
  once) or `held` (a hold rule or a risky bash command: a card opens, the extension waits for the answer).
- Daemon down or no answer to the first request: the call runs, which is pi's own behaviour. Only a call
  the daemon accepted as `held` fails closed: no answer in the timeout means
  `{block: true, reason: "no answer from agent-monitor"}`.
- Inbound messages: long-poll the daemon from `session_start`, stop in `session_shutdown`; deliver with
  `pi.sendUserMessage(text, {deliverAs: "steer"})`.
- Auth: none. Loopback calls are trusted by `localCaller` (`orch_git_api.go:246`), the same as the hooks.
- MCP: prefer `pi.registerMcpServer` inside the extension over the `mcp.json` edit; keep the existing
  hand-added `mcp.json` entry (adopted, not rewritten).
- Sessions: `~/.pi/agent/sessions/--<path>--/*.jsonl`.
- Live after `/reload` in pi, or on the next start.

## Phase 3: Codex IPC socket (VS Code extension and the Codex app)

Second clients are by design (`registerClient`, `client-discovery`), but a client is never passive.

1. Write `docs/codex-ipc.md` from the installed extension (`out/extension.js` and
   `webview/assets/app-initial-*.js` for the follower handshake), with the extension version and each
   method's version (`thread-stream-state-changed` is 13).
2. Client rules: never create or unlink the socket; at least 60 s between connects (each connect broadcasts
   `client-status-changed`); answer every `client-discovery-request` with
   `canHandle: false` at once; one connection, reconnect with backoff; connect while the socket exists and
   disconnect after 10 minutes with no thread events.
3. Read: follow `thread-stream-state-changed` snapshots and patches; honour `targetClientIds`.
4. Send and answer (`thread-follower-start-turn`, `-command-approval-decision`, `-file-approval-decision`,
   `-permissions-request-approval-response`): enabled only when every pinned version matches; else
   read-only with a note on the place.
5. First live test: `initialize`, discovery answers and reading only. It sends no follower handshake and no
   other broadcast. The extension's log is watched for errors before any write method is enabled.

## Phase 4: opencode

- Keep the JSON storage reader (1.1.x, installed here); add a read-only SQLite `opencode.db` reader for
  1.2 and later.
- HTTP adapter: find the TUI's server port from its listening socket; `POST /session/:id/prompt_async` to
  send, `GET /event` to follow, `POST /session/:id/permissions/:id` to answer. No port found: tmux keys.

## Phase 5: Hermes

- Session reader for `~/.hermes/state.db` (read-only).
- Prompt matcher, keys `o` once, `s` session, `a` always, `d` deny. Never send Enter alone to a Hermes
  prompt: Enter means deny.
- ACP built-in `hermes acp` for daemon-started threads.

## Phase 6: ACP built-ins and hooks for agents not installed here

ACP: `goose acp`, `kiro-cli acp`, `grok agent stdio`, Qwen, `pi-acp`. Hooks: Qwen and Kiro
`PermissionRequest`, Crush `PreToolUse`, Amp plugin `tool.call`, Gemini policy TOML. Each is tested against
the documented shape and marked `unverified` in `/api/agents/wiring` until a live run.

## Phase 7: terminal prompt matchers

aider (`y/n/a/s/d` then Enter), Goose, Grok, Gemini, muse. A matcher is added only from a real captured pane
(`testdata/prompts/<agent>.txt`). Agents with no capture keep the generic "open the terminal" card.

## Phase 8 (needs the user's yes): Accessibility for GUI apps

The `agent-monitor.app` asks for Accessibility once. The grant is tied to the code signature: an ad-hoc
signature changes on each build and resets it, so phase 8 starts with a stable signing identity on
rohit-mac. Then: Cursor desktop chat (type and send; Enter runs, Escape skips a pending card), Claude desktop
and the VS Code Claude extension (type and send). When Cursor turns on its `desktop_bridge` gate, use
`cursor desktop send` instead of typing.

## Order and gates

Phases run 1, 2 (pi), 3 (Codex IPC, highest risk), 4 to 7, then 8 on the user's yes. Each phase: code,
tests, commit, push, privacy scan, `remote-build.sh --test` (Go and Rust) on rohit-mac, a Codex review of the
phase diff, then a fast-forward merge to `main` and the install command for the user. A phase that fails a
gate does not merge.

## Not verified at plan time

The Codex IPC write methods, opencode port discovery, hooks for agents not installed here, and which "muse"
the user means.
