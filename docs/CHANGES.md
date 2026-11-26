# Changes on feat/orchestrator

A reviewer's guide to the branch `feat/orchestrator` against `49172c8` (v4: Desk, Control room and
Messages over the v3 approvals, search and ops API). The branch turns agent-monitor from a monitor
into an orchestrator: it starts and drives agents itself, and the Workspace is the home page.

Size: 96 Go files (+26.8k lines, about half tests), 70 web source files (+11.8k lines). Go tests: 277
test functions (61 at the base).

## What the branch adds

### Orchestrator core (phases 0 and 1)

- `orch_store.go`: SQLite `orch.db` with threads, runs, items, requests and an event log with a
  monotonic seq. `GET /api/orch/stream` replays and streams events (SSE).
- `orch_codex.go`: drives Codex through `codex app-server` (JSON-RPC): turns, steer, interrupt, resume
  on the native thread id, approvals through the existing ApprovalStore.
- `orch_api.go`: `/api/orch/threads` create, list, detail, messages, interrupt, close.
- A watched session that a driven thread owns is hidden from the watched lists (`drivenId`).

### Claude driver and Continue here (phase 2)

- `orch_claude.go`: drives `claude -p` with stream-json in and out and `--permission-prompt-tool stdio`.
  Questions and plan approvals become requests. Manual and acceptEdits threads pass ask rules so the
  user's own allow rules cannot skip agent-monitor.
- `orch_continue.go`: `POST /api/orch/continue` turns an ended watched session into a driven thread
  with the same session id, folder and model.
- V3-SPEC.md records the observed stream-json protocol.

### Subagents and lineage (phase 3)

- `orch_lineage.go`, `orch_groups.go`: `delegate_task`, `wait_tasks`, `task_status` MCP tools for
  driven agents; worktree, shared and read-only children; fan-out from New thread; parent notify with
  fenced, untrusted subagent output; groups Needs you / Working / Ready / Settled; settle, snooze, pin.
- Caps: depth 2, 8 running children, 32 subagents per tree; a child is never less restricted than
  its parent.

### Git panel (phase 4)

- `orch_git.go`, `orch_git_api.go`, `orch_review.go`: checkpoints under `refs/am/cp`, diff, restore,
  commit, push, PR, open, worktree remove, continue in the parent checkout, and the read-only Git box
  and diff for watched folders.

### ACP agents and devices (phase 5)

- `orch_acp.go`, `orch_agents.go`: ACP client (opencode, cursor-agent, gemini by registry entry, user
  entries in `acp-agents.json`). fs reads and writes stay inside the workspace through `os.OpenRoot`.
- `devices.go`, `auth_sessions.go`, `host_guard.go`, `origin_guard.go`, `known_cwd.go`: per-device
  tokens, one-time pair codes, `/pair`, `requireDeviceToken`, password sessions, Host allowlist,
  origin check on reads, known-folder check for cwd parameters.
- AgentBar (`mac/main.swift`, source only): `AGENT_MONITOR_TOKEN` as a Bearer token and the
  notification stream.

### Terminal, notifications, spend, driven rows everywhere (phase 6)

- `term.go`: browser terminal on a pty per thread (`creack/pty`, `@xterm/xterm`), remote access only
  with `remoteTerminal` and a device token.
- `notify.go`, `webpush.go`, `web/public/sw.js`: notification centre, batching, quiet hours, SSE,
  Web Push (VAPID, aes128gcm) without a third-party service.
- `orch_spend.go`: per-thread and per-day limits, 80% notice, 100% stop with 402, Allow once, Codex
  priced by user rates or the built-in table.
- `orch_views.go`, `search_driven.go`: driven threads in `/api/v3/threads`, now, timeline, stacks and
  search; `/api/session/send|cancel/{ot_id}` reach driven threads.
- `orch_fixes.go`: close cascade, Continue keeps the model, Claude-scope Always, read-only Bash parser,
  fan-out rollback, ACP turn timeout, watched diff.

### Watched sessions as full threads

- `watched_items.go`, `watched.go`: `GET /api/session/{id}/items` (cursor paging over the transcript,
  same OItem/ORun shape as driven threads) and `GET /api/session/{id}/pane`. Pane registrations get the
  correct agent prefix. Turn spans carry per-turn tokens and cost.

### Composer, review and merge, templates, schedules (phase 7)

- `orch_composer.go`, `orch_uploads.go`: `@` files, `$` skills, image uploads and attachments for all
  three drivers.
- `orch_review.go`: review child with parsed findings; merge with `git merge-tree` and a guarded ref
  update.
- `orch_templates.go`, `orch_schedules.go`, `orch_cron.go`: templates with variables and two
  built-ins; schedules on an in-daemon cron ticker.

### Web app

- The Workspace (`/w`) is home. Desk, Control room, Messages and Timeline stay under More with the
  other v3 views. Settings pages: Devices, Notifications, Limits, Templates, Schedules.
- Workspace: thread list, chat, approval cards, composer with pickers and images, right panel with
  Git box, diff view, lineage, spend; terminal drawer.

### Startup

- The daemon binds the port first and loads sessions in the background; `/api/health` reports
  `loading`. Hook scripts follow `AGENT_MONITOR_URL` or `AGENT_MONITOR_PORT`.

### Cleanup (phase 8)

- Removed unreferenced code (Go: an unused tmux pane scorer and five helpers; web: 13 unused exports
  and unused imports), merged duplicate helpers (`firstNonEmpty` into `firstOf`, `clipUTF8` into a
  UTF-8 safe `clipString`, one span-family pill table), cut every comment block in changed files to
  one or two lines, and rewrote README.md and V3-SPEC.md.

## Security review and fixes

Each finding below has a regression test that failed before its fix (`review_*_test.go`,
`orch_review_fix_test.go`, `orch_git_fix_test.go`, `orch_merge_fix_test.go`, `orch_sched_fix_test.go`,
`orch_composer_fix_test.go`, `watched_items_fix_test.go`).

Auth and remote access (`fix/review-auth`):

- Host allowlist against DNS rebinding; a proxied loopback request counts as remote; only the Mac or a
  paired device adds devices and flips the remote switches (`host_guard_test.go`; no finding id).
- F4: a cross-site GET cannot reach `/api/` or `/ws`, and cwd parameters must be a known folder.
- F5: revoking a device closes its live connections; turning `remoteTerminal` off closes remote shells.
- F6: a device may revoke only itself and may not mint devices.
- F7: the pair link carries a one-time code that expires; the token never appears in a URL.
- F8: Web Push goes only to known push services, with no redirects.
- F9: the pair link never uses the request Host; `X-Forwarded-Proto` counts only from a local proxy.
- F10: uploads have a 500 MB quota and a 30-day expiry, and remote callers do not see paths.
- F11: the cookie holds a session id, not the password; `?token=` is ignored; failed logins are limited.
- S1: the default VAPID subject is not localhost; `AGENT_MONITOR_VAPID_SUB` wins.

Drivers and approvals (`fix/review-drivers`):

- C1: quoted flags, `git grep -O`, env reads and paths outside cwd no longer pass the read-only allowlist.
- C2: a child is never less restricted than its parent.
- C3: close during process start kills the process and never sends the prompt.
- C4: allow rules never answer read-only Codex or ACP threads; read-only file changes are declined.
- C5: Always never creates a rule wider than the call the user saw.
- C6: the hook skip holds only while the driven thread runs a turn.
- C7: an ACP write never follows a symlink out of the workspace.
- C8: acceptEdits covers edits inside the workspace only; deny rules still apply; read-only needs plan mode.
- C9: an over-limit lineage cannot delegate; the tree has a total cap; a refused report waits.
- C10: the spend sweep leaves alone the turn that Allow once let through.
- C11: a delta that arrives after its item completed is dropped.
- C12: the end of an old run never sets idle over a newer running run.
- C13: shutdown stops driven processes quickly and does not record them as failures.
- D3: driven approvals wait for the user unless `drivenApprovalTimeoutSec` is set.
- D4: subagent results reach the parent fenced as untrusted data.
- L1: an edited input is refused where the agent cannot apply it.
- L5: a failed thread insert removes the worktree and branch made for it.

Git, merge, schedules and composer (`fix/review-git`):

- H1: restore never replaces an ignored file where the target needs a directory.
- H2: a case-only rename is restored, not lost.
- H3: merge never overwrites an ignored file in the target checkout.
- H4: a truncated git listing aborts restore before any change.
- M1: restore never deletes a path that the target's ignore rules ignore.
- M2: a failed commit leaves the index as it was.
- M3: restore or commit on a parent is refused while its shared child runs.
- M4: an embedded repository without a commit is left out of checkpoints with a warning.
- M5: a status read writes no object and leaves the index as it was.
- M6: prune deletes refs even when the worktree root is gone.
- M7: merge runs no hook and adds no worktree.
- M8: concurrent run-now calls start one thread.
- M9: a torn `schedules.json` is moved aside and reported, never replaced in silence.
- M10: worktree removal refuses ignored files without force and checkpoints first.
- L2: with no origin/HEAD, develop and `init.defaultBranch` are not pushed; push writes no upstream config.
- L3: a transcript line over 8 MB is skipped with a marker and later lines still parse.
- L4: the composer file cache keeps at most 16 folders; the walk stops after 3 s.
- L6: a schedule stops making worktrees at a cap of 5.

Web (`fix/review-web`, no finding ids): approval cards show the full request and take `a`/`d` through
one guarded dispatcher with a setting to turn single keys off; notification clicks open only
same-origin relative paths; Markdown caps parsing at 200 KB and shows the real link target; PR links
open only for https; the cron preview asks the daemon; per-thread state resets on thread change;
terminal reconnects pause while hidden; sample data loads only in dev builds.

Known limits that stay open (V3-SPEC.md, Known limits): D1, driven agents can reach the loopback API
that governs them; D2, read-only is enforced per agent, not by an OS sandbox.

## Test commands

```bash
go vet ./...
go test -count=1 ./...                    # about 90 s
cd web && npm run build                   # tsc --noEmit and vite build into web/dist
cd web && node mdcheck.mjs                # both Markdown renderers, 55 checks
cd web && node slackcheck.mjs             # Slack converter and prompt export, 33 checks
```

Live tests call real agents and cost money. They are skipped unless their variable is set:

```bash
AM_LIVE_CODEX=1 go test -run TestOrchLiveCodex -count=1 .
AM_LIVE_CLAUDE=1 go test -run TestOrchLiveClaude -count=1 .
AM_LIVE_DELEGATE=1 go test -run TestOrchLiveDelegate -count=1 .
AM_LIVE_ACP=1 AM_LIVE_ACP_MODEL=<model> go test -run TestOrchLiveACP -count=1 .
```

A second daemon for manual checks, never on port 7777:

```bash
go build -o /tmp/am/agent-monitor . && AGENT_MONITOR_HOME=/tmp/am/home AGENT_MONITOR_PORT=7778 /tmp/am/agent-monitor
cd web && AM_TARGET=127.0.0.1:7778 npx vite --port 5174
```

## Deploy

The live daemon runs from the `~/agent-monitor` worktree, branch `v3-live`, at `9a1d04f`. Both
worktrees share one repository, so the branch is visible there.

```bash
cd ~/agent-monitor
git merge --ff-only feat/orchestrator     # v3-live is an ancestor of feat/orchestrator
go build -o agent-monitor ./cmd/agent-monitor               # web/dist is committed; bin/build.sh also rebuilds it
./agent-monitor restart                   # or: ./agent-monitor stop, then start it as you usually do
```

Then open `http://localhost:7777/w` and check `GET /api/health` until `loading` is false. Driven
threads that ran under the old binary are idle after the restart; the next message resumes them.
Rebuild AgentBar from `mac/` only if you want the notification stream there.

## Roll back

```bash
cd ~/agent-monitor
git checkout 9a1d04f                      # detached; or git reset --hard 9a1d04f on v3-live
go build -o agent-monitor .
./agent-monitor restart
```

State files that the newer build adds (`notify.db`, `vapid.json`, `templates.json`,
`schedules.json`, `auth_sessions.json`, `uploads/`) are ignored by `9a1d04f`. Rows the newer build
wrote to `orch.db` carry extra JSON fields that the older build ignores.

## Not verified

- No daemon from this branch was run in phase 8. The cleanup is covered by the commands above only.
- The live tests (`AM_LIVE_*`) were not run in phase 8.
- ACP tool calls and permission prompts with a real model were never observed (expired logins and
  quota on this Mac); the mapping follows the schema and a fake agent.
- Gemini CLI is not installed; its registry entry is untested.
- AgentBar changes are source only and were not built.
- Web Push on a real iPhone over `tailscale serve` is not covered by tests.
- Roll back from a state folder that the newer build wrote is reasoned from the code, not tested.
- `~/Library/LaunchAgents/com.rohit.agent-monitor.plist` names `bun run server.ts`, not this binary,
  so how the live daemon is started on this Mac is not confirmed.
- The first auth fixes and the web review fixes have no finding ids in the tests or commits.
