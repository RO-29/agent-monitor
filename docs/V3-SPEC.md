# agent-monitor v3 spec

This document states the current API and behaviour of the daemon and the web app. README.md is the
user guide; this file is the reference for contributors.

## Conventions

- Go 1.26, module root is the repository. SQLite is `modernc.org/sqlite` (pure Go, no cgo).
- JSON field names are camelCase. Timestamps are epoch milliseconds. Durations are milliseconds.
- State files live under `stateDir()` (`~/.agent-monitor`, or `AGENT_MONITOR_HOME`).
- Each feature file registers its own routes; `v3routes.go` calls them.
- Live push to the browser: `publishV3(kind, key, payload)` in `v3bus.go`. Clients receive
  `{"kind": kind, key: payload}` on `/ws`.
- Code comments: one or two short lines. No AI attribution.
- Test with a second daemon only:
  `go build -o /tmp/am/agent-monitor . && AGENT_MONITOR_HOME=/tmp/am/home AGENT_MONITOR_PORT=7778 /tmp/am/agent-monitor`.
  The web dev server proxies to `AM_TARGET` (default `127.0.0.1:7778`).
- Errors are JSON `{error}` with the HTTP status given in each section. Orchestrator spend refusals
  are 402 `{error, spent, limit, scope}`.

Design mockups: `~/agent-monitor-redesign/directions.html`, `directions-v4.html` and
`orchestrator-sketch.html`.

## Existing model

- `Session` (types.go), `Thread` and `computeThreads` (threads.go), `SessionTrace`, `Span`, `Segment`
  (trace.go; `buildTrace(sess)` parses one transcript and caches it).
- Store ids are `claude:<uuid>` and `codex:<uuid>`. Claude transcripts are
  `~/.claude/projects/*/<uuid>.jsonl` (subagents in `<uuid>/subagents/*.jsonl`); Codex rollouts are
  `~/.codex/sessions/**/rollout-*.jsonl`.
- A thread is a set of sessions joined by an explicit continuation: `/clear` in the same cwd (same
  pane, or within 30 minutes), Codex `parent_thread_id`, or a shared handoff file in the first prompt.
- Startup: the daemon binds its port first, then loads sessions in the background.
  `GET /api/health` reports `loading: true` until the adapters, `sessions.db` history and the summary
  workers are done.

# Approvals

Files: `approvals.go`, `approvals_risk.go`, `bin/codex-hook.sh`. Store: `ApprovalStore` in memory plus
an audit table in `stateDir/approvals.db`.

```ts
Approval {
  id; agent: string /* claude | codex | an ACP registry id */; sessionId /* store id, or "<agent>:<nativeId>" */;
  cwd; kind: "permission"|"question"|"plan"; toolName; input: object;
  summary /* one line: the command, the file, the question or the plan title */;
  risk: "low"|"medium"|"high"; riskReason;
  suggestions?: object[] /* Claude permission_suggestions */;
  questions?: {question, header?, multiSelect?, options:{label, description?}[]}[];
  plan?: string; options?: {optionId, name, kind}[] /* ACP permission options */;
  createdAt; holdUntil;
  state: "pending"|"allowed"|"denied"|"expired"|"auto-allowed"|"auto-denied"|"passed";
  decidedAt?; decidedBy? /* web | mac | phone | rule:<id> | terminal */; message?; ruleId?;
  alwaysOk: boolean /* Always may be offered */;
  drivenId? /* driven thread */; rootId? /* its root thread */;
}
Rule { id; action: "allow"|"deny"|"hold"; agent?; tool?; pattern? /* glob on the summary */;
       cwdPrefix?; note?; createdAt; hits }
```

## Hook endpoints

The agent calls these and blocks.

- `POST /api/approvals/hook/claude-permission`: Claude Code `PermissionRequest` hook JSON. Answer 200
  `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow","updatedInput"?,"updatedPermissions"?}}}`
  or a deny decision with `message`. No decision: 204 with an empty body.
- `POST /api/approvals/hook/claude-pretool`: `PreToolUse` with matcher `AskUserQuestion|ExitPlanMode`.
  A question answers allow with `updatedInput = {questions, answers:{<question text>: <label or text>}}`.
  A plan approval answers allow with the original input as `updatedInput`; a rejection answers
  `permissionDecision:"deny"` with the user's message as the reason. No decision: 204.
- `POST /api/approvals/hook/codex`: Codex `PermissionRequest`. Codex accepts only allow or deny (with
  message). No decision: 204. `bin/codex-hook.sh` forwards the event, calls this endpoint with
  `--max-time <hold+5>` and prints the body only on 200. Any error prints nothing and exits 0.
- The hook scripts find the daemon through `AGENT_MONITOR_URL`, else `AGENT_MONITOR_PORT`, else 7777.
  Driven processes get both variables.
- A hook call for the session of a driven thread answers 204 while that thread runs a turn. A closed
  or idle driven session gets normal hook handling.

## Decision flow

1. Build the Approval and compute `summary` and `risk` (high: prod-like projects, `rm -rf`,
   `git push --force`, `kubectl delete`, `gcloud * delete|update`, `sudo`, `launchctl`, `brew install`;
   medium: file writes and network commands; low: read-only commands and Read/Grep/Glob).
2. Match rules: deny before allow before hold. allow and deny answer at once (state `auto-*`).
   Allow rules never answer an approval of a read-only thread; deny rules still apply.
3. When `skipWhenFocused` is on and the session's tmux pane is focused (pane active, window active,
   client attached), answer 204 at once (state `passed`).
4. Else hold for `holdSeconds` (default 120; questions and plans `questionHoldSeconds`, default 600)
   for `POST /api/approvals/{id}/decide`. Timeout: `expired`, 204. Client disconnect: `passed`.
5. Driven approvals wait with no time limit unless `drivenApprovalTimeoutSec` (0 to 86400) is set;
   then they are declined after that many seconds.
6. Every change publishes `approval`; leaving pending also publishes `approval-done`.

## User endpoints

- `GET /api/approvals` → `{pending, recent /* last 100 decided */}`.
- `POST /api/approvals/{id}/decide`
  `{behavior:"allow"|"deny", message?, interrupt?, updatedInput?, always?, alwaysScope?:"agent-monitor"|"claude",
  applySuggestions?, answers?, optionId?, surface?}` → the Approval. 409 when it is no longer pending.
  - `updatedInput` is accepted only for Claude approvals (400 for others).
  - `always` adds an agent-monitor allow rule from the tool and a narrow pattern. It is refused when
    `alwaysOk` is false: high risk, a compound shell command, `request_permissions`, multi-file patches,
    or a pattern with glob characters. File rules match per cleaned path: an allow needs every patch
    path inside the pattern and no `..`; a deny or hold matches any path. `cwdPrefix` is compared by
    path containment.
  - `alwaysScope:"claude"` needs `always` on a Claude approval with suggestions (else 400). No
    agent-monitor rule is added; the answer carries `updatedPermissions` = the suggestions.
    `applySuggestions:true` with allow is an alias.
  - `optionId` picks an ACP option and sets behavior and always from its kind. `allow_always` becomes
    `allow_once` when `alwaysOk` is false.
- `GET|POST /api/approvals/rules`, `DELETE /api/approvals/rules/{id}`. Rules live in
  `stateDir/approval-rules.json`. Defaults on first run: allow Read/Grep/Glob/LS; deny `brew install*`,
  `sudo *`, `launchctl *`.
- `GET /api/approvals/audit?since=&limit=` → decided approvals, newest first.
- `GET|POST /api/approvals/settings` `{holdSeconds, questionHoldSeconds, skipWhenFocused,
  drivenApprovalTimeoutSec}` in `stateDir/approval-settings.json`.
- `GET /api/approvals/hooks-snippet` → `{claude, codex}`: hook config with the daemon's own URL and a
  timeout of holdSeconds+30. The daemon never writes user config files for these hooks.
- `pendingApprovalsBySession()` feeds the "needs you" state of watched threads.

# Search and span index

Files: `search.go`, `search_index.go`, `search_query.go`, `search_driven.go`. `AGENT_MONITOR_SEARCH=0`
turns the indexer off.

- `stateDir/search.db`, WAL. FTS5 is checked at startup; without it search uses LIKE.
- Tables: `docs(id, session, agent, cwd, repo, ts, kind /* prompt|reply|command|tool|output|error|file|summary */,
  tool, turn, span, path, text)`, `docs_fts` (porter unicode61) and `docs_tri` (trigram) on text and
  path, `spans(id, session, agent, cwd, repo, ts, dur, tool, fam, cmd, cmdKey, err, errSig, errText, child)`,
  `files(path, size, mtime, offset, session)`.
- Indexer: newest files first, throttled, then a re-scan every 20 s of appended bytes only. It reads
  prompts, replies, tool calls, tool results (first 4 KB; `error` on is_error, a non-zero exit or an
  error pattern) and compaction summaries, and pairs tool_use with tool_result into `spans`.
- Driven items are indexed after each transcript pass (session = thread id, agent = thread agent):
  user → prompt, agent → reply, command → command plus an output or error doc, fileChange → file,
  error → error, mcp/web/other → tool. Progress follows the orchestrator event seq; a re-indexed item
  replaces its docs.
- `GET /api/search/status` → `{fts5, indexedFiles, totalFiles, docs, spans, indexing, lastRunAt}`.
- Query chips: `agent: repo: cwd: kind: tool: file: is:error since: until: session: thread:`. Plain words
  become chips by rules (agent names, failure words, dates, known repo names, paths, prompt/command/reply).
  The rest is the FTS query, with prefix match on the last token. `thread:<ot_id>` and `session:<ot_id>`
  match driven docs.
- `GET /api/search?q=&scope=&kind=&limit=50&offset=0` →
  `{query:{text, chips:{key,value,source}[]}, total, facets:{kind, agent, repo, tool}, hits, tookMs, mode}`.
  Facet counts are matching docs (lines), not sessions.
  `Hit {id, session, agent, cwd, repo, ts, kind, tool, turn, span, path, snippet, threadId?, drivenId?}`.
  Rank: bm25, trigram hits for identifiers, a recency boost (half-life 14 days) and a boost for the
  scope repo. `drivenId` is set for driven docs and for watched sessions that a driven thread owns;
  `threadId` falls back to it.
- `GET /api/search/context?id=&n=2` → the doc and n docs on each side in the same session.
- `GET /api/thread/{id}` adds `turns: {sessionId: prompts}` from the index when the thread has 2 or more
  sessions.
- `POST /api/search/ask {q}` → `{answer, citations, costUsd}` from the top 20 hits and
  `claude -p --model haiku`. 400 when enrichment is off.
- MCP tools (`mcp_perm.go`, every session, watched and driven; driven Claude and Codex pre-allow them):
  - `search_sessions {query, agent?, repo?, since?, limit? (10, max 50)}`: `GET /api/search` with the
    filters as chips and `limit=200`, grouped by session in rank order. Each session:
    `{id, agent, title, repo, cwd, lastActivity, hits, snippets[3]{text, kind, turn, ts}, url}`; titles
    from `GET /api/v3/threads?all=1&driven=1`; snippets plain text; `url` = daemon URL + the Workspace hit
    route. The text result lists the same; `structuredContent` = `{query, total, sessions}`.
  - `session_context {sessionId, turn?, around? (1, max 5)}`: driven ids read `GET /api/orch/threads/{id}`;
    watched ids read `GET /api/session/{id}/items` and page `before=` until the turn is loaded. It renders
    turns `turn-around .. turn+around` (default turn: the last) as plain text: user, agent, `$ command`
    with exit code and 600 chars of output, edits, tools, errors. Over 8 KB the turns farthest from the
    hit go first, then the text is clipped. When the items route fails it answers from the index.
- Span views (`since` default 7d, optional `repo`, `agent`): `GET /api/spans`, `GET /api/tools/latency`,
  `GET /api/errors`, `GET /api/graph`.

# Web app

React 18, react-router 6, Vite, TypeScript. `npm run build` = `tsc --noEmit && vite build`, output
`web/dist`, embedded with `go:embed`. `web/mdcheck.mjs` tests both Markdown renderers
(`v3/components/Markdown.tsx` for the Workspace, `lib/markdown.tsx` for the classic views and
approval cards). Sample data loads only in dev builds with `?mock=1`.

Routes:

- `/w`, `/w/new`, `/w/{threadId}`: the Workspace, the home page (`/` redirects here).
- `/w/search[?q=&mode=ask]`: the search page in the Workspace (rail Search). Query box, chips, facets
  (agent, repo, tool from the API; time as `since:`), results grouped by session with every snippet on
  demand. Ask POSTs `/api/search/ask` only on Enter or the button, shows the model, spent today and the
  cap from `GET /api/settings`, maps "daily cap reached" and "enrichment is disabled" to plain
  messages, and links each `[n]` to its hit.
- Thread filter: titles filter at once (typed `agent:` and `repo:` chips too); from 2 characters a
  250 ms debounced `GET /api/search?limit=100` adds "In transcripts", one row per session. Enter opens
  the top title match, else the top transcript result.
- Hit route: `/w/{target}?hit=&ts=&session=&q=`. Driven docs: target = thread, `hit` = item id. Watched
  docs: target = thread root, `hit` = turn, `session` = the session. `q` = marked words for the match.
  The chat picks the run by turn when its start agrees with `ts`, else the run that holds `ts`; in it
  the item that contains a `q` word, nearest `ts`. A watched feed pages `before=` until the run is
  loaded (driven details hold every item). The item's group opens, its output expands, it scrolls to
  the centre and flashes for 4.5 s.
- Session tabs: a watched thread with 2 or more `thread.sessions` shows tabs, newest first (start time,
  first prompt, turns from `/api/thread/{id}`, else from the index). Each tab loads its own items; only
  the newest polls live. The hit's `session` picks the tab. Driven threads have no tabs.
- Snippets render as text and `<mark>` elements from a parse of the server HTML, never as HTML. Thread list
  (Needs you / Working / Ready / Settled, driven and watched rows), chat, right panel (workspace, Git
  box, changes, lineage, usage, spend).
- `/t/{threadId}`: thread view (Story, Transcript, Errors). Driven ids open in the Workspace.
- Under More: `/desk` (Desk), `/control` (Control room), `/messages[/{id}]` (Messages), `/timeline`,
  `/threads` and `/inbox/{list}`, `/board`, `/explore`, `/errors`, `/tracing`,
  `/traces/{sessionId}/{turn}`, `/monitors`, `/intel`, `/rules`.
- `/notifications`, `/settings/devices`, `/settings/notifications`, `/settings/limits`,
  `/settings/templates`, `/settings/schedules`.
- `/pair` (no credentials needed), `/menubar` (AgentBar popover), `/float` (AgentFloat panel),
  `/tv` (AgentTV widget, replaced by `/float`), `/classic/*`
  (the classic UI; `/session/*`, `/thread/*` and `/learnings` redirect there).

Approval cards: one keyboard dispatcher decides the focused card or the newest card in view. It ignores
key repeat, a pending `g` chord, dialogs, menus, the palette, text fields and the terminal. High risk
and plans need a second press within 4 s. A setting turns single-key approval off. Cards show the full
command, the tool input, the diff for edits, the risk reason and the exact Always rule.

Markdown (Workspace renderer): escape first, then headings, emphasis, code, lists, tables and
http/https links. It shows the real link target when the text differs, parses at most 200 KB per render
(Show more), and re-parses streaming text at most every 150 ms.

# Ops API

- `GET /api/v3/threads` = `/api/threads` plus `parentThread`, `childCount`, `childAttention`, `display`,
  `repo`, `outcomeLine`, `shipped[]`, `errorsOpen`, `activity[12]`, `activityErr[12]`, `stackId`, and
  `stacks[]` on the response. Children are hidden from the top level unless `?all=1`. Watched sessions
  that a driven thread owns get `drivenId` and are hidden unless `driven=1`.
  - `?compact=1` returns list rows only: `id`, `title`, `cwd`, `ref`, `tools`, `sessions`, `state`,
    `startedAt`, `lastAt`, `turns`, `attention`, `model`, `parentThread`, `childKind`, `childCount`,
    `childAttention`, `approvals`, `liveActivity`, `drivenId`, `handover`, `terminalSession`, `kind`,
    `status`, `spendUsd`, `display`, `repo`, `outcomeLine`, `errorsOpen`. It drops `tokens`, `edges`,
    `lastPoint`, `lastOutcome`, the segment strip (`segments`, `segWeights`, `segKinds`, `sessionSegs`),
    the trace counts (`compacts`, `clears`, `errors`, `childErrors`, `learnings`, `outputs`, `costUsd`,
    `costEstimated`), `shipped`, `activity`, `activityErr`, `stackId`, and `stacks[]`.
  - `?since=<ms>` keeps threads with `lastAt >= since`; `?limit=<n>` keeps the newest n by `lastAt`.
    With either, `threadOf` lists only the sessions of the returned threads.
  - JSON responses over 1 KB are gzip-compressed when the request sends `Accept-Encoding: gzip`.
  - Driven threads are rows with `kind:"driven"`, `id` = `drivenId` = the orchestrator id, `status`,
    `spendUsd`, `tools:[agent]`, `turns` = runs, `tokens` from run usage, `lastOutcome` (thread error or
    last agent message), `approvals` = open requests, `attention`, `parentThread` with
    `childKind:"delegated"`, `childCount`, `ref` (branch), `liveActivity`, `activity`, `activityErr`.
    `state`: running/starting → running; waiting or open request → awaiting-permission; closed →
    completed; error → idle; else completed when the thread has runs or is ready/settled, otherwise idle.
  - A stack is 2 or more children of one parent with the same agent and verb class:
    `{id, parent, agent, kind, label, repo, total, running, waiting, done, failed, abandoned, findings,
    lastAt, members}`.
- `GET /api/v3/thread/{id}/children` → `{parent?, children:[{threadId, sessionId, kind, spanId, ts, title,
  state, findings?, errors, attention}]}`.
- `GET /api/v3/now` → `{needsYou, running, doneToday, spendToday, errorsPerHour, lanes, from, to}`. Driven
  rows count; driven runs started today add their cost when no watched transcript covers them.
- `GET /api/v3/timeline?day=&stacks=1` → lanes with `display`, `density`, bars (work, wait, idle,
  error), `waitMs`. Driven threads are lanes with `drivenId`; watched threads that a driven thread owns
  are left out.
- `GET /api/v3/trace/{sessionId}?turn=`, `GET /api/v3/monitors`, `GET /api/v3/intel?range=`,
  `GET /api/v3/health` → `{hooks, search, approvals}`.
- Trace turn spans carry `tokens` (each Claude message id counted once; Codex the delta of
  `total_token_usage`, input without cached input) and `usd` (the price-table estimate, scaled to
  `cost-state` when the Claude transcript has it).

# Watched session API

`{id}` takes the session id with any prefix or none: `claude:<uuid>`, `codex:<uuid>` and `<uuid>`
resolve to the same session (store first, then the transcript on disk).

## Pane registrations

- `POST /api/pane/register` and the load of `panes.json` correct the agent prefix: a store session
  under the other prefix wins, then a transcript on disk, then the UUID version (Claude v4, Codex v7).
- Every registry lookup (`/api/session/send|keys|cancel|pane|pane-view|focus/{id}`,
  `/api/pane/registration/{id}`, `/api/pane/name/{id}`, `agent-monitor send`) falls back to the raw
  session id.
- A registration replaces any older entry for the same raw session id or the same pane.

## GET /api/session/{id}/pane

```
WatchedPane {
  registered: bool      // a registration exists
  live: bool            // the tmux pane exists now
  sendable: bool        // live and the agent process is alive
  agentId?: string; agent?: string; paneId?: string;
  tmuxTarget?: string   // "<session>:<window id>.<pane id>", only when live
  pid?: number; command?: string
}
```

Always 200. Text goes to `POST /api/session/send/{id}`; send only when `sendable`. For a driven id
(`ot_...`), `POST /api/session/send/{id} {text}` starts or steers a turn (→ `{ok, driven:true,
mode:"turn"|"steer"}`, orchestrator errors, 402 spend) and `POST /api/session/cancel/{id}` interrupts.

## GET /api/session/{id}/items[?after=<cursor>|?before=<cursor>]

```
WatchedItems { id; agent: "claude"|"codex"; items: [OItem]; runs: [ORun];
  cursor: number   // byte offset after the last complete line read
  more: bool; earlier?: number; reset?: true }
```

- No params: the last 40 turns. `after=`: items and runs changed at or after the cursor (upsert by
  `id`). A cursor past the end answers a first page with `reset:true`. `before=`: the 40 turns before
  that offset.
- Errors: 400 bad offset, 404 no transcript, 501 another agent.
- `ORun.id` = `<session>@<offset>`; `index` is 1-based; `status` running, completed or interrupted.
  `usage` = `{inTokens /* input + cache read + cache write */, outTokens, costUsd /* estimate */}`.
- `OItem.id`: tool calls `<session>/<call id>`, others `<session>@<offset>.<n>`, stable across pages.
- Kinds: user, agent, reasoning (empty or encrypted reasoning skipped), command (`exitCode`, output last
  8 KB, failed on a non-zero exit), fileChange (`files` with add/del), mcp, web, plan (TodoWrite,
  update_plan), error, other. Codex messages written twice are merged.
- Lines over 8 MB are skipped with a marker item. An in-place rewrite (shrink, older mtime, or a changed
  hash of the first 4 KB or of the 4 KB before the indexed end) resets the index and the client gets a
  fresh page.
- The transcript is indexed once into turn offsets; only requested turns are parsed. 32 sessions stay
  cached.

# Orchestrator

Driven threads are agent processes that the daemon starts and owns. They are children of the daemon:
`agent-monitor stop` (SIGINT or SIGTERM) closes their stdin, then sends SIGTERM and SIGKILL to their
process groups. After a restart the threads are idle and the next message resumes the native session.

## Model

```
OThread  {id /* ot_... */, kind:"driven", agent /* claude | codex | ACP registry id */, title, cwd, repo,
          branch, model, sandbox, status:"starting"|"idle"|"running"|"waiting"|"error"|"closed",
          nativeId, parentId?, createdAt, updatedAt, error?, approvalPolicy?, permissionMode?,
          children?:[id], depth, workspace?:"here"|"worktree"|"shared"|"read-only", worktree?:OWorktree,
          group:"need"|"working"|"ready"|"settled", settledAt?, snoozedUntil?, pinned?, notifyParent?,
          readyAt?, reportedAt?, spendAllowOnce?, review?:OReview, reviewOf?, autoReview?, merge?:OMerge,
          templateId?, scheduleId?, catchUp?}
OWorktree {path, branch, removed?, origin?, originBranch?}
ORun     {id, threadId, index, prompt, status:"running"|"completed"|"interrupted"|"failed", startedAt,
          endedAt?, usage?:{inTokens, outTokens, costUsd?, cachedInTokens?, estimated?}, turnId?}
OItem    {id, threadId, runId, kind:"user"|"agent"|"reasoning"|"command"|"fileChange"|"mcp"|"web"|"plan"|
          "error"|"review"|"other", status:"started"|"completed"|"failed"|"declined", text, command?, cwd?,
          exitCode?, output? /* last 8 KB */, files?:[{path, kind, add, del}],
          attachments?:[{id, mime, name, size, url}], childId?, startedAt, endedAt?}
ORequest {id, threadId, runId, itemId?, kind:"command"|"fileChange"|"permissions"|"question"|"plan",
          approvalId, state:"open"|"closed"}
OEvent   {seq, threadId, at, type, data}
```

Event types: `thread.updated{thread}`, `run.started{run}`, `run.ended{run}`, `item.started{item}`,
`item.delta{itemId, text}` (append), `item.completed{item}`, `request.opened{request}`,
`request.closed{request}`. A child change also emits `thread.updated` for each ancestor whose group
changes. A delta that arrives after its item completed is dropped. The end of an older run never sets
idle while a newer run of the thread runs.

Storage: `stateDir/orch.db` with tables for threads, runs, items, requests and an `events` log with a
monotonic `seq`. Deltas are coalesced for about 250 ms; the snapshot always holds the full text.
Events are kept 7 days. Streamed item text in the browser is capped at 256 KB.

Groups, in this order: closed, snoozed (until `snoozedUntil`) or `settledAt` → settled; an open request
on the thread or a descendant, or status error → need; running or a running descendant → working;
done → ready, and settled after 24 h in ready (`readyAt`). A new run clears `settledAt`.

## Thread API

All routes are under the auth middleware.

- `GET /api/orch/threads` → `{threads, seq}`.
- `POST /api/orch/threads` `{agent, cwd, prompt, model?, sandbox?, permissionMode?, workspace?:"here"|"worktree"|"read-only",
  children?:[childSpec], attachments?:[uploadId]}` → `{thread}`. The cwd must exist. 400 when an ACP
  binary is missing or worktree is asked outside a git repo; 409 when git times out.
  - permissionMode (Claude and ACP): `manual` (default) | `acceptEdits` | `plan` | `auto` (Claude only).
  - With `children`, the daemon creates the parent and each child (as delegate does), starts the
    children, then starts the parent with its prompt plus the child ids and titles. When a child cannot
    start, the started children and the parent are closed and the call answers 422
    `{error:"fan-out rolled back: <reason>"}`.
- `GET /api/orch/threads/{id}` → `{thread, children, runs, items, requests, seq}`.
- `POST /api/orch/threads/{id}/messages {text, attachments?}` starts a turn, or steers the running turn
  (409 when the agent cannot steer). Text may be empty when attachments are present.
- `POST /api/orch/threads/{id}/interrupt`: no body interrupts the thread; a JSON body cascades to the
  children unless `cascade:false`.
- `POST /api/orch/threads/{id}/close {cascade?: true}`: closes every open descendant (deepest first),
  then the thread.
- `POST /api/orch/threads/{id}/settle {settled}`, `/snooze {until /* ms or 0 */}`, `/pin {pinned}`.
- `POST /api/orch/threads/{id}/allow-once` → `{ok, thread}`: see Spend limits.
- `GET /api/orch/stream?after={seq}` → text/event-stream, one `data: OEvent` per event, replays
  `seq > after`, then live. Heartbeat every 15 s.
- `POST /api/orch/continue {sessionId, prompt?, model?}`: turns a watched session (`claude:<uuid>` or
  `codex:<id>`, or a thread id, which resolves to its newest session) into a driven thread with
  `nativeId` = that session, the session's cwd and title, and its model (the newest model in the
  transcript unless `model` is given). 201 created, 200 existing, 404 unknown session, 409
  `{error:"live"}` while a live process owns the session (live pid on the session or its pane, or state
  running, awaiting-permission or awaiting-input).
- Handover rule (one owner per native session). The driven thread holds its session while its process
  exists, or while nothing touched the session after the driven mark = max(thread createdAt, newest run
  start or end, last process exit). Terminal use = the session's `lastActivityAt` is more than 15 s after
  the mark, or a terminal process holds it now: a live pid, a pane registered after the mark, or state
  running, awaiting-permission or awaiting-input with activity after the mark. While the driven thread
  holds the session, the watched row gets `drivenId` and leaves the watched lists. After terminal use,
  the watched row shows as usual and the driven row gets `handover:"terminal"` and
  `terminalSession` (the session id); the UI shows "Continued in a terminal" and opens the watched
  view. Driven rows with a native session otherwise carry `handover:"driven"`. A new driven turn moves
  the mark, so the driven thread takes the session back.
- `POST /api/orch/threads/{id}/messages` (and Continue with a prompt on an existing thread) returns 409
  `{error:"live"}` while a terminal process holds the thread's native session, so two processes never
  run one session.
- A driven thread with a native session shows that session's earlier conversation (items older than
  the thread's `createdAt`, from `GET /api/session/{agent}:{nativeId}/items`) read-only above its own
  runs, under the divider "Continued in agent-monitor". With no runs it says "Send a message to continue
  this session" and focuses the composer; Continue here with no message creates the thread this way.

Approvals: every driver request creates an `Approval` in the ApprovalStore (same risk scoring and
rules) and an `ORequest` with its `approvalId`. The UI decides through `POST /api/approvals/{id}/decide`;
the decision goes back to the agent. A cancel during the wait closes the request.

## Claude driver

Spawn: `claude -p --input-format stream-json --output-format stream-json --verbose
--include-partial-messages --permission-prompt-tool stdio --session-id <uuid>` (or `--resume <uuid>`),
plus `--model` and `--permission-mode`. The binary is `AGENT_MONITOR_CLAUDE_BIN`, else
`~/.local/bin/claude`, else PATH.

- manual → `--permission-mode default`; acceptEdits, plan and auto map to the CLI modes.
- Manual threads add `--settings {"permissions":{"ask":[Bash, Write, Edit, MultiEdit, NotebookEdit,
  WebFetch, WebSearch, <user MCP allow rules>]}}`; acceptEdits threads ask for Bash, WebFetch, WebSearch
  and the user's MCP allow rules. Ask rules rank above the user's allow rules, so these calls reach the
  approval store. The user's MCP allow rules are mirrored because permission rules accept no `mcp__*`
  wildcard; agent-monitor's own server is left out.
- Read-only threads: `--disallowedTools Edit,Write,MultiEdit,NotebookEdit`, an ask rule for Bash,
  `--strict-mcp-config`, and permission mode default or plan. See Read-only Bash.
- Item mapping: assistant text → agent (deltas from partial messages), thinking → reasoning, Bash →
  command (exitCode 0, or parsed from "Exit code N"), Edit/Write/MultiEdit/NotebookEdit → fileChange
  (add/del counts), `mcp__*` → mcp, WebFetch/WebSearch → web, TodoWrite → plan (text = input JSON),
  Task/Agent → other, everything else → other.
- Request kinds: Bash → command, edit tools → fileChange, AskUserQuestion → question (the answer goes
  back as `updatedInput.answers`), ExitPlanMode → plan (allow approves; deny with a message keeps
  planning), others → permissions.
- `result` ends the run with usage: inTokens = input + cache creation + cache read; costUsd = the sum of
  `model_usage_since_last_result` costUSD.
- Images: the user message content becomes image blocks (base64) followed by a text block, also for a
  steer.

### Observed Claude stream-json protocol (claude 2.1.325, probes 2026-11-16)

- `--permission-prompt-tool stdio` is required. Without it, a prompt is denied at once with
  `system/permission_denied` and no `control_request` reaches the host.
- `--resume <id>` keeps the session id and needs the original cwd.
- Host to CLI (stdin, one JSON per line):
  - `{"type":"control_request","request_id":R,"request":{"subtype":"initialize"}}` once per spawn,
    before the first user line.
  - `{"type":"user","message":{"role":"user","content":TEXT},"parent_tool_use_id":null,"session_id":""}`
    starts a turn. Written during a turn, the CLI folds it into that turn (steer).
  - `{"type":"control_request","request_id":R,"request":{"subtype":"interrupt"}}`: the turn ends with
    `result` subtype `error_during_execution`, `terminal_reason:"aborted_tools"`; the process stays up.
  - Permission answer: `{"type":"control_response","response":{"subtype":"success","request_id":R,
    "response":{"behavior":"allow","updatedInput":{...}}}}` or `{"behavior":"deny","message":M}`
    (optional `"interrupt":true`). Errors: `{"subtype":"error","request_id":R,"error":M}`.
- CLI to host (stdout):
  - `control_request` subtype `can_use_tool` `{tool_name, display_name, input, description?,
    permission_suggestions?, blocked_path?, tool_use_id}`. AskUserQuestion: allow with
    `updatedInput = input + answers:{<question>: <label>}`. ExitPlanMode `{plan, planFilePath}`: allow
    approves (a `system/status` with `permissionMode:"default"` follows); deny returns the message to
    the model as an error tool_result.
  - `system/init`, `system/status`, `system/task_started`, `system/task_notification`,
    `system/thinking_tokens`, `rate_limit_event`.
  - `stream_event` with Anthropic stream events (`message_start`, `content_block_start|delta|stop`,
    `message_delta`, `message_stop`).
  - `assistant` with one content block per line, sent before that block's `content_block_stop`;
    tool_use blocks carry the full input.
  - `user` with `tool_result` blocks and `tool_use_result` (Bash: stdout, stderr, interrupted).
  - `result` `{subtype, is_error, result, terminal_reason, usage, total_cost_usd /* cumulative */,
    model_usage_since_last_result, permission_denials, num_turns}`.
  - Lines with a non-null `parent_tool_use_id` belong to a Claude subagent and are not mapped.
- A base64 image block in a stream-json user line is accepted. The CLI saves it under its temp folder,
  and the model may Read that path, which can surface as a permission request outside the cwd.

## Codex driver

`codex app-server` over JSON-RPC (schema from `codex app-server generate-json-schema`, codex-cli
0.169.0). The binary is `AGENT_MONITOR_CODEX_BIN`, else `~/.local/bin/codex`, else PATH.

- Resume with `thread/resume` on `nativeId`. A message during a turn uses `turn/steer`.
  Interrupt is `turn/interrupt`.
- Input is `[{type:"text", text, text_elements:[]}, {type:"localImage", path}...]`.
- Sandbox: `read-only` or `workspace-write`. Read-only threads decline file-change and permission
  requests without asking.
- Codex reports tokens but no cost. Each token update stores a cost: the user's `codexRates` when set,
  else the built-in price table with cached input at the cache rate, marked `usage.estimated`.

## ACP driver

Agent Client Protocol over stdio JSON-RPC 2.0 (`orch_acp.go`).

- initialize `{protocolVersion:1, clientCapabilities:{fs:{readTextFile, writeTextFile}, terminal:false},
  clientInfo}`; session/new `{cwd, mcpServers:[agent-monitor]}`; session/load or session/resume when
  advertised; session/set_model; session/set_mode; session/prompt; session/cancel.
- Resume: session/load, else session/resume, else a new session plus an "other" item that says the
  agent does not see the earlier messages (nativeId changes).
- permissionMode: manual | acceptEdits | plan (selects the agent's plan mode). A model goes to
  session/set_model; a failure becomes a thread note. Steer is not possible: a message during a turn
  answers 409.
- Updates: agent_message_chunk → agent, agent_thought_chunk → reasoning, tool_call and
  tool_call_update (merged by toolCallId; execute → command, edit/delete/move → fileChange with diff
  counts, fetch → web, search → other unless the title says web, others → other), plan → one plan item
  per run, usage_update → cost. Updates during session/load are dropped.
- session/request_permission → Approval with `options`; the answer is
  `{outcome:{outcome:"selected", optionId}}`, or `{outcome:{outcome:"cancelled"}}` on interrupt.
  acceptEdits answers edit/delete/move prompts allow_once; a read-only workspace answers reject_once.
- fs/read_text_file and fs/write_text_file accept only absolute paths inside the thread cwd or its
  worktree. Both go through `os.OpenRoot` on the workspace, so no symlink leaves it; reads take regular
  files only and cap at 8 MB. Errors: -32000 (outside, denied), -32002 (not found). Writes need an
  Approval (tool Write, kind fileChange) unless acceptEdits allows them after deny rules; a read-only
  workspace refuses writes.
- A read-only ACP thread does not start a turn when the agent offers no plan mode.
- Images go as `{type:"image", mimeType, data}` only when initialize reported
  `promptCapabilities.image`; else 400. When the agent has not run yet, the daemon starts it once to
  read its capabilities.
- `acpTurnTimeoutMin` (setting, default 20): a turn with no update and no agent request for that long is
  interrupted and gets an "other" item that names the setting. A pending approval pauses the clock.
  After session/cancel the driver waits 10 s, then ends the run.

### Observed ACP protocol (opencode 1.1.44, cursor-agent 2026.11.05, probes 2026-11-16)

- One JSON per line, every message carries `"jsonrpc":"2.0"`.
- opencode initialize answers `{protocolVersion:1, agentCapabilities:{loadSession:true,
  mcpCapabilities, promptCapabilities:{embeddedContext, image}, sessionCapabilities:{fork, list, resume}},
  authMethods, agentInfo}`.
- session/new answers `{sessionId:"ses_...", models:{currentModelId, availableModels}, modes:
  {currentModeId:"build", availableModes}}`; opencode modes include build and plan.
  `session/set_model {sessionId, modelId}` works but is not in the stable schema.
- session/load replays the history as `session/update` before it answers.
- session/prompt answers `{stopReason:"end_turn"|"max_tokens"|"max_turn_requests"|"refusal"|"cancelled"}`.
  opencode sends no usage block; the schema's `usage_update {used, size, cost?}` is read as cost.
- An expired provider login answers `end_turn` at once with no update (the driver adds an error item);
  a provider quota error leaves session/prompt unanswered (the turn timeout and cancel grace end it).
- Ignored updates: user_message_chunk, available_commands_update, current_mode_update,
  config_option_update, session_info_update, notice, compaction updates.
- cursor-agent has a hidden `acp` subcommand (loadSession false; session/resume, set_mode, set_model).
  Its `--help` fails while the login keychain is locked.
- Live tool calls and permission prompts were not observed; that mapping follows the published schema
  and the fake agent in the tests.

## Agent registry

- `GET /api/orch/agents` → `[{id, name, kind:"claude"|"codex"|"acp", installed, version?, path?, models?}]`.
  Claude and Codex come first, then the ACP registry. Claude models: opus, sonnet, haiku. ACP rows get
  `[{id, name}]` after a session reported them. Binaries resolve through PATH, then `~/.local/bin`,
  `/opt/homebrew/bin`, `/usr/local/bin`; `installed` uses a 3 s `--version`, cached 10 minutes.
- Built-in ACP entries: opencode `["opencode","acp"]`, gemini `["gemini","--experimental-acp"]`, cursor
  `["cursor-agent","acp"]`.
- User entries in `stateDir/acp-agents.json`: `[{id, name, command, args, env?:{NAME: value}}]`; id
  `^[a-z0-9][a-z0-9_-]{0,39}$`, not claude or codex. A user entry with a built-in id replaces it.

## Subagents and lineage

Driven processes get `AM_ORCH_THREAD`, `AM_ORCH_DEPTH`, `AM_DAEMON_URL`, `AGENT_MONITOR_URL` and
`AGENT_MONITOR_PORT`, and agent-monitor's MCP server: Claude through `--mcp-config` plus
`--allowedTools` for the three tools (`MCP_TOOL_TIMEOUT` 1900 s); Codex through
`-c mcp_servers.agent-monitor.{command,args,env.*,tool_timeout_sec=1900,tools.<t>.approval_mode="approve"}`;
ACP through session/new. The command is the one `~/.claude.json` registers (override
`AGENT_MONITOR_MCP_BIN`). The tools are offered only when `AM_ORCH_THREAD` is set.

- `delegate_task {title, prompt, agent, model?, workspace:"shared"|"worktree"|"read-only", permissionMode?|sandbox?}`
  → `{threadId}`. Not offered at depth 2.
- `wait_tasks {threadIds?, timeoutSec? /* default 600, max 1800 */}` → `[{threadId, title, status, result,
  costUsd}]`; without ids it waits for all running children of the caller.
- `task_status {threadIds?}` → the same shape, no wait.
- HTTP behind the tools: `POST /api/orch/threads/{id}/delegate {title, prompt, agent, model?, workspace?,
  permissionMode?, sandbox?, notifyParent?}` → 201 `{threadId, thread}`; structural limits 409, bad input
  400, spend 402. `GET /api/orch/threads/{id}/tasks?ids=&wait=` → `{tasks:[{threadId, title, status,
  result, costUsd, group}], timedOut}`; only descendants of `{id}`. status is the thread status while
  active, else the last run status, or error/closed.

Workspaces: shared = the parent cwd; worktree = `git worktree add` under `stateDir/wt/<childId>` on
branch `am/<slug>-<short id>` from the parent HEAD (400 outside a git repo, 409 on a git timeout);
read-only = Codex read-only sandbox, Claude read-only rules, ACP plan mode. A failed thread insert
removes the worktree and branch made for it. Worktrees are never removed by the daemon itself. A
worktree thread is named after its main repository.

Limits: depth 2; 8 running children per parent; 32 subagents per root thread over its life. A child is
never less restricted than its parent: a parent that cannot write (read-only workspace, Codex
read-only sandbox, plan mode) gets read-only children, and the child's level (plan < manual <
acceptEdits < auto; Codex untrusted = manual, on-request = acceptEdits, never = auto) is lowered to the
parent's. Delegation is refused (402) when the day limit is reached or when the root thread and its
subagents together reached the per-thread limit.

Parent notify: when the last running child of a parent ends and the parent is idle, the daemon sends the
parent one message "Subagents finished:" with each child's title, status and result (2 KB each). Each
result is inside a `<subagent-output thread="...">` block marked as untrusted subagent output. When a
spend limit refuses the message, the children stay unreported, one "Subagent results are waiting"
notification goes out, and the next child run end or Allow once delivers it. `notifyParent:false` on a
child turns this off. `reportedAt` records when the parent saw a result.

Approvals of a child carry `drivenId` (the child) and `rootId`, so the parent view can show them.

## Read-only Bash

A Bash call of a read-only Claude child is allowed at once only when it is read-only by this parser;
else it becomes an Approval that allow rules cannot answer. Allowed calls are audited as auto-allowed.

- The command is tokenized with sh quoting. Any expansion, substitution, redirection, chaining (`;`,
  `&`, `||`), glob, env assignment, `~`, control character or unterminated quote makes it not
  read-only. Only a plain pipe joins stages.
- Each stage is an allowlisted program, and each flag must be on that program's list. Values never
  start with a dash. Operands and flag values stay inside the thread cwd: no absolute path outside it
  and no `..` element.
- Programs: `ls cat head tail wc pwd stat du df which cut diff cmp basename dirname realpath readlink
  grep rg echo sort true`, `find` with a fixed set of primaries (no -delete, -exec or -fprint family),
  and `git` (optionally after `--no-pager`) `status diff log show blame rev-parse ls-files`.
- Everything else (sed, awk, xargs, env, tee, jq, nl, git grep, go, npm, make, ...) needs the user.

## Git

Driven threads in a git repository; all git runs as the user with their own config (never `-c user.*`,
no trailers added). Checkpoint, restore, commit and merge take one lock per repository (git common dir)
and refuse while any thread runs in the same work tree. `AGENT_MONITOR_GH_BIN` names `gh`.

Checkpoints: at each run start and end, in a private index seeded from a copy of the real index:
`add -A`, write-tree, commit-tree (parent = previous checkpoint, else HEAD), update-ref
`refs/am/cp/<threadId>/<run>-start|-end`. Identity `agent-monitor <agent-monitor@localhost>`,
`--no-gpg-sign`, never pushed. An unchanged tree is skipped. `refs/am/base/<threadId>` is HEAD at the
first checkpoint. Embedded repositories without a commit are left out with a note. A checkpoint that
takes more than 20 s is skipped (failed "checkpoint" item) and checkpoints stay off for that thread
until the daemon restarts. A daily sweep prunes refs of threads closed for more than 30 days, by git
common dir, so refs go even when the worktree is gone.

Routes:

- `GET /api/orch/threads/{id}/git` → `{isRepo, repo, root, branch, head, upstream?, ahead, behind, dirty
  /* changed file count */, defaultBranch, files:[{path, status, add, del}], checkpoints:[{runIndex,
  kind:"start"|"end"|"restore", ref, sha, at, prompt, add, del, files}], worktree?}`. Status words:
  added, modified, deleted, renamed, copied, typechange, untracked. Status reads `git diff` against HEAD
  and `ls-files` with `GIT_OPTIONAL_LOCKS=0`, so a poll writes no object. `isRepo:false` answers 200
  with empty fields.
- `GET /api/orch/git?cwd=` → the same without checkpoints (read-only box for watched threads).
- `GET /api/orch/threads/{id}/diff?from=&to=&path=&run=` → `{from, to, files:[{path, oldPath?, status,
  add, del, binary, patch, truncated?}], truncated}`. Refs: a checkpoint ref or short ref (`3-end`), a
  sha, `base`, `head`, `worktree` (default to). `from` defaults to base. `run=<index>` diffs that run.
  Patch cap 400 KB total, 120 KB per file. Unknown ref 404, ref starting with `-` 400, not a repo 409.
  Paths are literal pathspecs.
- `GET /api/orch/git/diff?cwd=&from=head&to=worktree&path=` → the same shape for watched folders; refs
  head, worktree or a sha (7 to 40 hex).
- `POST /api/orch/threads/{id}/restore {ref}` → `{ok, safetyRef, written, deleted, kept}`. 409 while a
  run is active. A safety checkpoint (`<run>-restore`, `-restore-2`, ...) comes first. Restore lists
  every change from the safety checkpoint to the target and checks each path before it changes one.
  It never deletes a path that the current or the target ignore rules ignore (`kept`), never writes
  over a file or through a symlink that no checkpoint holds (409 with the paths), deletes before it
  writes, and stops on truncated git output or a failing check-ignore.
- `POST /api/orch/threads/{id}/commit {message, paths?}` → `{ok, sha}`. The commit is built in a private
  index seeded from HEAD; with paths only those (literal) are committed and files staged before stay
  staged; with no paths every change is committed. The user's hooks run. The real index changes only
  after success. 400 empty message, 409 running or nothing to commit, 422 hook or git failure
  `{error, output}`.
- `POST /api/orch/threads/{id}/push {setUpstream?}` → `{ok, output}`. Pushes
  `refs/heads/<b>:refs/heads/<b>` to origin, never with force; writes upstream config only with
  `setUpstream`. 403 for main, master, the remote default branch, `init.defaultBranch`, and trunk or
  develop when origin/HEAD is unknown. 409 detached HEAD, 422 git failure.
- `POST /api/orch/threads/{id}/pr {title, body?, draft? /* true */, base?}` → `{ok, url}` via
  `gh pr create --head <branch>`. 400 empty title, 409 detached or the branch is not on origin, 422 gh
  failure.
- `POST /api/orch/threads/{id}/open {target:"editor"|"terminal"|"finder"}`: editor = `cursor` or
  `code` on PATH, else `open -t`; terminal = a tmux window in the most recent session, else Terminal;
  finder = `open`. 403 for any non-loopback caller or proxy header.
- `POST /api/orch/threads/{id}/worktree/remove {force?}` → `{ok, thread}`. Takes a forced checkpoint
  first and names its ref in the note, then `git worktree remove`; the branch is kept. 400 no worktree,
  409 running, dirty (output = porcelain status) or holding ignored files without force (output =
  the paths).
- `POST /api/orch/threads/{id}/worktree/parent` → `{ok, thread}`: after a removal, moves the thread cwd
  to `worktree.origin` (older threads: the parent thread's cwd), sets workspace here, stops the agent
  process and adds an item command "cwd". Claude transcripts are copied to the new project folder so
  `--resume` finds them. 400 no removed worktree, 409 running or origin unknown.
- Every mutating route adds an item kind "other" with command "git commit", "git push",
  "gh pr create", "restore", "git worktree remove", "git merge" or "checkpoint".

## Review and merge

- `POST /api/orch/threads/{id}/review {agent? /* default codex */, model?, focus?, run?}` → 201
  `{threadId, thread, parent}`. The child is read-only, has `reviewOf` = the thread, `notifyParent:false`,
  depth + 1 (the depth limit does not apply) and title "Review: <title>". Its prompt holds the first
  run's prompt, the focus, the file list, patches up to 60 KB and a `git diff` command for the rest, and
  asks for one fenced JSON block `{"summary", "findings":[{severity, file, line, title, body}]}`.
  409 not a repo, empty diff, or a review child still active; 400 on a review thread; 402 day limit.
- `OReview {state:"running"|"done"|"failed", childId, agent, run?, focus?, findings, summary?, error?,
  startedAt, endedAt?}`; `OFinding {severity:"critical"|"high"|"medium"|"low"|"info", file?, line?,
  title, body?}`. The reviewed thread gets one item kind "review" with `childId`.
- Parsing the child's last agent message: the last fenced json block (or unlabelled block that starts
  with `{` or `[`), then a bare `{... "findings" ...}` object, then `##` to `####` headings. Key aliases
  and severity words (blocker/p0 → critical, major/error/p1 → high, moderate/warning/p2 → medium,
  minor/p3 → low, nit/note/suggestion/style/p4 → info, unknown → medium) are accepted. A failed,
  interrupted or closed child sets state failed. A finished review sends one "finished" notification.
- `POST /api/orch/threads/{id}/merge {into?, strategy?:"squash"|"merge", message?}` → `{ok, sha, into,
  branch, strategy, applied:"checkout"|"ref", thread}`. `into` defaults to `worktree.originBranch`, then
  the origin checkout's branch, then the default branch.
  - Mechanism: `git merge-tree --write-tree` (git 2.38 or later, else 422), `commit-tree`, and a
    compare-and-swap `update-ref`. Nothing is checked out and nothing is left on disk after a crash.
    Git hooks never run (`core.hooksPath` is empty for every call). It never pushes.
  - When `into` is checked out, that checkout must be clean, still at the old commit, and hold no path
    the merge adds; it is then fast-forwarded with `read-tree -m -u` (applied "checkout"), and the ref
    update is rolled back on failure. Else only the ref moves (applied "ref").
  - Errors: 400 no worktree, bad strategy, bad branch, into = the thread branch; 404 unknown branch;
    409 run active in either work tree, uncommitted changes, dirty target checkout, nothing to merge,
    `into` moved, a branch in a rebase or bisect, `{error:"merge conflicts", files}`; 422 git failure.
  - Default message: squash = thread title; merge = `Merge branch '<branch>' into <into>`. The thread
    gets `merge {into, strategy, sha, at}`.

## Composer

- `GET /api/orch/files?cwd=&q=` → `[{path, kind:"file"|"dir"}]`, max 50, paths relative to cwd. Inside a
  repo `git ls-files -co --exclude-standard`; else a walk (no hidden entries, no node_modules, depth 8,
  3 s deadline). Fuzzy = case-insensitive subsequence, basename matches first. Cached per cwd for 30 s,
  16 folders at most (least recently used goes first).
- `GET /api/orch/skills?agent=&cwd=` → `[{name, description, source:"user"|"project"|"plugin"|"codex-prompt",
  plugin?, agent, insert, path}]`. Sources: `~/.claude/skills` (or `$CLAUDE_CONFIG_DIR`), project
  `.claude/skills`, enabled plugin skills, `~/.codex/prompts` (or `$CODEX_HOME`). `insert`: Claude
  `/<name>`, plugin `/<plugin>:<name>`, Codex `/prompts:<name>`. An unknown cwd is ignored.
- `POST /api/orch/uploads` (multipart, field `file`, optional `threadId`) → 201 `{id:"up_<16 hex>", path,
  mime, name, size, url}`. The type is sniffed (png, jpeg, webp, gif; else 415). Over 10 MB 413, empty
  400, unknown threadId 404. Stored as `stateDir/uploads/<threadId|new>/<id>.<ext>` (0600). Total quota
  500 MB; files older than 30 days are deleted. Remote callers do not get `path`.
- `GET /api/orch/uploads/{id}` → the bytes with the sniffed type, `nosniff` and
  `Content-Security-Policy: default-src 'none'; sandbox`.
- Messages and thread creation take `attachments` (max 10).

## Message queue

A message sent while its thread is busy waits in a queue on the daemon, one queue per thread,
driven (`ot_...`) or watched (the v3 thread id). The queue is stored in `orch.db` (table `queue`), so it
survives a reload, a restart and works from the phone.

```
QItem {id /* q_... */, threadId, text, attachments? /* upload ids, driven only */, session? /* watched
       target session */, createdAt, state:"queued"|"sending"|"sent"|"failed", error?, sentAt?, mode?:"turn"|"steer"|"pane"}
```

- `GET /api/queue/{thread}[?session=]` → `{items, paused?:"terminal", terminal?:[text], cli?}`. `cli` is the
  CLI's own queue (below). `terminal` is best effort for other agents: the lines between the spinner and
  the input box while a "Press up to edit queued messages" hint is on screen.
- `POST /api/queue/{thread} {text, attachments?, session?}` → 201 `{item, items}`. 400 empty text or
  attachments for a watched session, 404 unknown driven thread, 409 at 50 messages. Text up to 64 KB.
- `PATCH /api/queue/{thread}/{id} {text?, index?}` edits the text (a failed message goes back to queued)
  and moves the message to `index` (0 = first) → `{items}`.
- `DELETE /api/queue/{thread}/{id}` removes one message; `DELETE /api/queue/{thread}` removes all but a
  message in flight → `{items}`. 409 while that message is sending.
- `POST /api/queue/{thread}/{id}/send-now` → `{item, mode, items}`. Driven: `SendWith` (a turn when idle,
  a steer when running). Watched: typed into the tmux pane at once; the agent's own TUI queues it. A 409
  (cannot steer, terminal live) leaves the message queued with `error`; other errors mark it failed.
- Every change emits `queue.updated {threadId, items, sent?}` on the orch stream.
- Dispatch: the oldest queued message goes when the thread is ready, one message per idle transition.
  Triggers: the end of a driven run, a watched session's Stop or task_complete event, an add, and a 3 s
  sweep. Driven: status idle, and a run started or ended since the last attempt; while the handover rule
  gives the session to a terminal, dispatch pauses (`paused:"terminal"`). Watched: the pane is sendable,
  shows no "esc to interrupt" and no question or permission prompt, the last hook event ends a turn, and
  the session looked busy (spinner or a UserPromptSubmit, tool or task_started event) since the last
  attempt. A failed message is skipped by dispatch and stays until it is sent, edited or removed. A
  message in `sending` at start-up turns failed, so a restart never sends it twice.

### The CLI's own queue

A message typed into Claude Code while it works waits in Claude Code's own queue. Claude Code writes each
change of that queue into the session transcript as a `queue-operation` record, and agent-monitor reads
the queue from there.

- Records: `enqueue` (with `content`), `dequeue` (no content), `remove` (with `content` and a `reason`:
  `absorbed_mid_turn`, `delivered_to_agent`, `cleared_on_cancel`; `dropped_by_hook` has no content) and
  `popAll` (one record for each message that Up moved into the input box).
- Replay: `enqueue` appends. `dequeue` takes the oldest message: Claude Code takes the highest priority
  first, but the priority is not logged. `remove` and `popAll` take the first message with the same text;
  one without content changes nothing. When a user record delivers a message that the dequeue did not
  take, that message goes and the one the dequeue took goes back to the front (until the next assistant
  record). Claude Code holds no queue while it waits at its prompt, so an idle session shows none.
- Kinds: `system` for the notifications Claude Code queues itself (`<task-notification>`,
  `<agent-message>` and tags that end in `-notification`, `-message` or `-event`), else `user`.
- `CLIQueueItem {id /* cq<byte offset> */, text, at, kind:"user"|"system"}`.
- `GET /api/session/{id}/cli-queue[?all=1]` → `{agent, supported, items, hidden?, reachable, note?,
  canRemove, removeWhy?}`. Without `all`, `items` holds user messages and `hidden` counts the
  notifications. `GET /api/queue/{thread}?session=` carries the same view as `cli` (`cliAll=1` adds the
  notifications); for a Claude session it replaces `terminal`.
- `POST /api/session/{id}/cli-queue/send {text}` types the message and Enter into the terminal (host
  first, then the tmux pane), as if typed; a busy Claude Code queues it → `{ok, queued}`. 409 when a
  question or permission prompt is on screen or the terminal is not reachable; the web then uses
  agent-monitor's queue.
- A `!` command: text that starts with `!` to a Claude or Codex session is typed as `!` alone, a 150 ms
  pause, then the command (leading spaces dropped) and Enter. Claude Code switches to bash mode on the
  `!` key in an empty input box; Codex runs `!cmd` as a user shell command (rollouts record
  `source:"user_shell"`). `!` with no command → 400. Other agents get the text unchanged.
- `POST /api/session/{id}/cli-queue/{send-now|edit|remove} {id?}` press Claude Code's own keys, only while
  the screen shows its "Press up to edit queued messages" hint (the input box is empty) and no prompt,
  else 409. Keys checked on Claude Code 2.1.326:
  - `send-now`: `Esc`. Claude Code stops the turn, then sends every queued message as the next turn. Its
    own `ctrl+enter` (CSI `13;5u`) does the same.
  - `edit` → `{text}`: `Up` moves every queued user message into the input box, joined with newlines, with
    the cursor at the end. agent-monitor checks that the box shows the first message, presses Backspace
    once for each character plus 8, and checks that the box is empty. `ctrl+u` is not used: it deletes
    only to the start of the cursor's line.
  - `remove`: as `edit` without the text, only when one user message is queued (409 otherwise, with the
    reason in `removeWhy`).
- Sessions carry `cliQueued`, the number of queued user messages; the transcript tail recounts it on each
  queue record. The thread list shows "N queued" for a live session.
- Codex 0.169 writes no queue records into its rollout. `~/.codex/queue_1.sqlite` has a `queued_items`
  table, empty on the machine checked, so a Codex session shows its queue only in its terminal.

## Templates

- `OTemplate {id, name, description?, agent, model?, permissionMode?, sandbox?, workspace:"here"|"worktree"|"read-only",
  prompt, children?, review?, reviewAgent?, builtin?, vars:[names], createdAt?, updatedAt?}` in
  `stateDir/templates.json`.
- Built-ins: `builtin-fix-and-review` (Claude, acceptEdits, worktree, Codex review; vars cwd, branch,
  task) and `builtin-read-only-investigation` (Claude, plan, read-only; vars cwd, question).
- `GET /api/orch/templates` (built-ins first, then by name), `GET /{id}`, `POST` → 201 `{template}`,
  `PUT /{id}`, `DELETE /{id}` (409 when a schedule uses it), `POST /{id}/copy {name?}`. Built-ins answer
  403 to PUT and DELETE. Validation 400: name 1 to 80 characters, prompts required, known agents and
  modes, at most 8 children.
- `POST /api/orch/templates/{id}/run {cwd, vars?}` → 201 `{thread}`. `{{name}}` is filled from vars;
  cwd, branch and date (local YYYY-MM-DD) are automatic. Missing variables → 400 `{error, missing}`.
  The thread carries `templateId` and, with review on, `autoReview`: after a run completes with no
  active descendant, one review starts; when it cannot start, the review state is failed with the error.
- Writes go through a synced temporary file and a rename. A file that does not parse is moved to
  `<name>.corrupt-<time>`, reported, and the write that found it is refused.

## Schedules

- `OSchedule {id:"sch_...", name?, templateId, cwd, vars?, cron, enabled, lastRunAt?, lastThreadId?,
  lastCatchUp?, lastSkipAt?, lastError?, lastErrorAt?, nextRunAt /* 0 when disabled */, nextRuns /* 3 */,
  createdAt, updatedAt}` in `stateDir/schedules.json` (same durable write as templates).
- `GET /api/orch/schedules`, `GET /{id}`, `POST` → 201 `{schedule}`, `PUT /{id}`, `DELETE /{id}`,
  `POST /{id}/run-now` → 201 `{thread, schedule}` (409 while the previous thread runs). Validation 400:
  cron, unknown template, cwd, missing variables. Saving recomputes `nextRunAt` from now.
- `GET /api/orch/cron?expr=` → `{valid, error?, next:[5 ms]}`.
- Cron: 5 fields, local time, lists of `*`, `n`, `a-b` with `/step`; month and weekday names; weekday 7
  = Sunday; macros `@yearly @annually @monthly @weekly @daily @midnight @hourly`. Day rule as Vixie cron.
  A wall-clock minute repeated by a fall-back change runs once; a minute skipped by a spring-forward
  change runs once, at the first minute after the gap. An expression with no run is rejected.
- A ticker in the daemon (every 20 s and once at start) runs a due schedule at most once, and
  `nextRunAt` moves past now, so missed runs are never replayed. A run more than 2 minutes late is
  flagged `catchUp`. Run-now and the ticker share one in-flight set. A schedule is skipped while its
  previous thread or a descendant is active, or while its threads hold 5 live worktrees. Threads carry
  `scheduleId` and `templateId`. A refused run sets `lastError` and sends an "error" notification.

## Spend limits

- Settings `spend: {perThreadUsd, perDayUsd, codexRates:{<model>|"*": {in, out}}}` (GET/POST
  `/api/settings`, POST merges). 0 = off; negative 400. Rates are USD per 1M tokens and win over the
  built-in Codex estimate.
- Thread spend = the thread's own runs. Day spend = driven runs started since local midnight.
- A new turn (messages, session send, Continue with a prompt, parent notify) is refused while the thread
  or the day is at its limit; thread creation and delegate are refused while the day is. 402
  `{error, spent, limit, scope:"thread"|"day"|"tree"}`.
- Crossing 80% sends a "spend" notification; crossing 100% sends another and interrupts the running
  driven threads of that scope. A 30 s sweep interrupts a thread that went over mid-run, once per run.
- `POST /api/orch/threads/{id}/allow-once` sets `spendAllowOnce`; the next refused turn goes through and
  the sweep leaves that turn alone. Raising or turning off the limit also lifts the block.
- `GET /api/orch/spend?thread=` → `{day:{spent, limit, blocked}, thread?:{spent, limit, allowedOnce?, blocked}}`.

## Notifications

- `ONotification {id, kind:"needs-you"|"finished"|"error"|"spend", title, body?, threadId?, url?, at, read?,
  count?, quiet?}` in `stateDir/notify.db`, kept 30 days or 2000 items.
- Sources (driven threads only): needs-you = a request opened; error = a run failed or the thread went to
  error; finished = a root run completed with no running descendant, or one "N subagents finished"
  notice when the last running child of a parent ends; spend = the 80% and 100% crossings.
- Settings `notify {needsYou, finished, error, spend /* null = on */, quietStart, quietEnd /* "HH:MM" */,
  quietAllowsNeedsYou /* null = true */, batchSec /* null = 20, 0 = none, max 600 */}`. During quiet hours
  a notification is stored and streamed with `quiet:true` and never pushed.
- Batching per kind: the first event goes at once and opens a window; events inside it fold into one
  notification at its end.
- `GET /api/notify?after=&limit=` → `{items, unread}`; `POST /api/notify/read {ids?, all?}`;
  `GET /api/notify/stream` (SSE, `id:` per item, `?after=` or Last-Event-ID replays up to 500, `: hb` every
  15 s); `POST /api/notify/test`.
- Web Push: `GET /api/notify/vapid` → `{publicKey}`; `POST /api/notify/subscribe <PushSubscription>`;
  `POST /api/notify/unsubscribe {endpoint}`. Keys in `stateDir/vapid.json` (0600). Messages are RFC 8291
  aes128gcm with a VAPID (RFC 8292, ES256) header, TTL 86400, Urgency high for needs-you and error.
  Endpoints must be on push.apple.com, fcm.googleapis.com, push.services.mozilla.com or
  notify.windows.com, port 443; at most 50 subscriptions; no redirects; 404 or 410 deletes a
  subscription. VAPID subject: `AGENT_MONITOR_VAPID_SUB` when it is a mailto: or https:// URL that is
  not localhost, else `mailto:agent-monitor@example.com`.
- The web app opens only same-origin relative paths from a notification.
- AgentBar reads the stream with its Bearer token and posts UserNotifications for finished, error and
  spend.

## Terminal

- WebSocket `GET /api/orch/threads/{id}/term` (one shell per driven thread) and `GET /api/term?cwd=`
  (one per cleaned cwd). Optional `cols`, `rows` (1 to 1000, default 80x24) size a new shell. The
  upgrade must be same-origin.
- Errors before the upgrade: 400 bad cwd, 404 unknown thread, 403 remote refused, 401 no credential,
  429 when 6 shells run and this key has none.
- Access: loopback without a proxy header is allowed. Every other caller needs `remoteTerminal: true` and
  a paired device token. The password alone is always 403. Turning `remoteTerminal` off closes remote
  terminals.
- Shell: `$SHELL -l` (fallback `/bin/zsh`) on a pty in the cwd, with the daemon env minus
  `AGENT_MONITOR_*` names that contain PASSWORD, TOKEN, SECRET, KEY or VAPID, minus `AM_ORCH_*`, and with
  `TERM=xterm-256color`. Input and output are never logged.
- Lifetime: the shell stays while a client is attached and 30 minutes after the last one leaves, then
  SIGHUP and SIGKILL 3 s later. Several clients may attach; the last resize wins.
- Server to client: text `{"type":"hello","new","cwd","pid","cols","rows","scrollback"}`, one binary
  frame with the scrollback (last 256 KB), then binary output frames, and on exit
  `{"type":"exit","code"}` and a close frame. Ping every 30 s; a client 512 frames behind is
  disconnected.
- Client to server: binary input, or text `{"type":"input","data"}`; resize
  `{"type":"resize","cols","rows"}`. Bad messages are ignored. Max message 1 MB.
- The web client pauses reconnects while the tab is hidden and stops after 10 attempts.

# Devices and auth

- Loopback callers without a proxy header (X-Forwarded-For, Forwarded, X-Real-Ip,
  Tailscale-User-Login) need no credential. A request through `tailscale serve` is remote.
- Every request's Host must be an IP literal, `localhost` or a name in `AGENT_MONITOR_ALLOWED_HOSTS`
  (421 otherwise), which blocks DNS rebinding.
- GET, HEAD and writes to `/api/` and `/ws` get an origin check. Native clients without Origin or
  Sec-Fetch-Site pass; page navigations are not checked.
- cwd on `/api/orch/git`, `/api/orch/git/diff`, `/api/orch/files` and `/api/term` must equal or be inside
  the folder of a watched session or a driven thread (403 otherwise). A session in `/` or the home folder
  allows only that folder itself.
- Password: `AGENT_MONITOR_PASSWORD`, else the first line of `stateDir/password`. `POST /api/login` sets
  `am_auth` to a random session id (sha256 in `stateDir/auth_sessions.json`, 30 days, bound to the current
  password). 429 after 10 failures in 10 minutes per client. `?token=` is never read. A Bearer password
  works for native apps.
- Devices (`stateDir/devices.db`): token = `amd_` + 32 random bytes base64url, stored as sha256.
  Accepted as `Authorization: Bearer amd_...` or cookie `am_device` (HttpOnly, SameSite=Lax, Secure on
  TLS or when a loopback proxy sends `X-Forwarded-Proto: https`). `lastUsedAt` and `lastIp` update at
  most once a minute. No secret is logged.
  - `POST /api/devices {name}` → 201 `{device, token, pairUrl}`, only from the Mac (403 remote); name 1 to
    80 characters. `pairUrl` = base + `/pair#` + a one-time code that expires after 5 minutes.
  - `GET /api/devices` → `[{id, name, createdAt, lastUsedAt, lastIp, revokedAt, current}]`.
  - `DELETE /api/devices/{id}` → `{ok, device}`; 404 unknown; 403 when a remote caller revokes another
    device; 409 when a remote caller would revoke the last active device while `requireDeviceToken` is
    on. A revoke closes the device's WebSockets, terminals and SSE streams and deletes its push
    subscriptions.
  - `POST /api/devices/session {code}` (or `{token}` for AgentBar) sets `am_device` for 400 days; 401 on a
    bad value.
  - Pair base, from config only: `AGENT_MONITOR_PAIR_BASE`; else `AGENT_MONITOR_BIND` when it is a
    Tailscale address; for 0.0.0.0 the first Tailscale interface address; another specific address;
    else `https://` + the first `AGENT_MONITOR_ALLOWED_HOSTS` name; else `http://127.0.0.1:<port>`.
- Without credentials a remote caller reaches only `GET /pair`, `GET /assets/*` and
  `POST /api/devices/session`.
- `requireDeviceToken` (setting, default false): `POST /api/settings {requireDeviceToken:true}` answers
  409 when the caller is remote with the password only, or when no other active device exists. Turning
  it off always works. While on, `/api/login` answers 403 to remote callers, and a server without a
  password still requires a device token remotely. `remoteTerminal` and `requireDeviceToken` change
  only from the Mac.
- AgentBar: `AGENT_MONITOR_TOKEN` adds the Bearer header to its requests and sets `am_device` for the
  popover web view.

# Known limits

- **D1. The approval authority is reachable from the agents it governs.** Loopback callers need no
  credential, and every driven process gets `AM_DAEMON_URL`. An agent that may make one HTTP request (a
  `curl` the user allowed) can list and decide its own approvals, add allow rules, or create threads
  with looser modes. The delegate endpoints take the caller's thread id from the URL with no proof.
  Mitigations in place: read-only children run only an allowlisted Bash subset with no network program,
  allow rules never answer a read-only thread, Always never creates a rule wider than the call the user
  saw, and children are capped at the parent's level. Not built: a daemon secret for decide, rules and
  thread create, and a per-thread token for the delegate tools.
- **D2. "Read-only" is enforced per agent, not by the OS.** Claude children get denied edit tools, an
  ask rule for Bash, `--strict-mcp-config` and the read-only Bash parser; every other command is an
  Approval that only the user can allow. Codex children get the Codex read-only sandbox, and their
  file-change and permission requests are declined. ACP children must accept plan mode, edit requests
  are rejected and fs writes are refused. An allowlisted program with an unknown side effect still runs.
  The durable fix, an OS sandbox (a Seatbelt profile that denies writes outside a temp folder) for every
  read-only child, is not built.
- A driven approval waits for the user with no time limit unless `drivenApprovalTimeoutSec` is set.
- ACP tool calls and permission prompts were not observed live (see the observed protocol section).
- Merge needs git 2.38 or later.

# Session host (no tmux dependency)

User rules (2026-11-17): start a session from agent-monitor or from a terminal; in a terminal, tmux is the
user's choice; agent-monitor always starts its sessions inside tmux; every approval and question shows in
the terminal AND in web, phone and Mac app at the same time and can be answered in either; every command
(agent tool calls, ! commands, slash commands, local command output) shows.

## Host
`agent-monitor run <agent> [args]` becomes a session host: it starts the agent on a pty (creack/pty,
already a dependency), puts the user's terminal in raw mode, relays bytes both ways and forwards resize
(SIGWINCH) and signals. It is a transparent pass-through: no visible change in the terminal. It keeps a
ring buffer of recent output (256 KB) and opens a control socket `stateDir/hosts/<hostId>.sock` (mode 0600,
in a 0700 dir). The agent gets env `AM_HOST_ID`. The host registers with the daemon (POST
/api/hosts {hostId, pid, agentPid, agent, cwd, tty, tmuxPane?}) and re-registers when the daemon
restarts. Hook scripts send `AM_HOST_ID` with each event so the daemon links session id ↔ host. If the
daemon is down the host still relays. When the agent exits, the host exits with its code and removes
the socket.
Control protocol (unix socket, JSON lines): {op:"input", data} (bytes typed as the user), {op:"keys",
keys:[...]} (named keys: Enter, Esc, Tab, Up, Down, C-c, digits), {op:"screen"} → recent output with ANSI
stripped (last N lines), {op:"size"}, {op:"ping"}. Only the daemon (same uid) can connect; the socket
path is never sent to remote callers.
Daemon: GET /api/hosts, and the existing send / keys / pane-capture / question-answer paths prefer the host
when the session has one and fall back to tmux, then to "not reachable".
Shell functions (user applies them): `claude` / `codex` run `agent-monitor run …` directly (no tmux);
`claude --tmux` or env AGENT_MONITOR_TMUX=1 keeps today's tmux behaviour. Sessions started from
agent-monitor (New thread "In a terminal") run `tmux new-session -d -s am-<id> agent-monitor run …`.

## Approvals and questions in both places
For a session with a host (or a registered tmux pane): the PermissionRequest / PreToolUse hook records the
request as a pending Approval (source "terminal-shown"), returns no-opinion at once so the native prompt
shows in the terminal immediately, and the card shows everywhere at the same time. A decision on a card
is delivered as keys through the host (verified key maps per agent and prompt kind). An answer given in
the terminal is detected from the transcript (tool_result) or the next hook event, and closes the card
everywhere (state "answered-in-terminal"). Auto rules still answer immediately through the hook
response. Sessions with no host and no pane keep today's hold-then-pass behaviour.
Setting `approvalsMode`: "both" (default, the above) or "hold" (hold-then-pass for every session).
Key maps (checked on Claude Code 2.1.326 and Codex 0.169.0; a decision is typed only when the prompt for
this request is on screen, else 409):
- Claude permission ("Do you want to …?", numbered choices, "Esc to cancel"): Yes = `1`; don't ask again =
  the digit of the "Yes, and don't ask again / always allow / accept edits" choice (409 when absent); No =
  the digit of "No"; No with a message = Up/Down from the cursor to "No", Tab, the text, Enter.
- Claude questions and plans: the term_question.go flows; a plan is approved with "manually approve edits"
  when offered, denied with text through "Tell Claude what to change", else Esc.
- Codex exec and patch ("Would you like to …", "Press enter to confirm"): the letter of choice 1 (`y`);
  No = Esc, and a message follows as the next prompt.
A card stays "sent" until the prompt leaves the screen or the transcript shows the result; still on screen
after 90 s, it becomes answerable again.

## All commands
Slash commands (<command-name>/<command-args>/<local-command-stdout>), ! commands (done), and the
real output of a ! command Claude Code moved to the background (it prints the output file path; the
daemon tails that file while it grows) show as user items (`by:"user"`, kind "command", Text = the
slash command name for slash commands).

## Part D (2026-11-17)
- Start from agent-monitor: New thread, the floating panel and the phone have Where: "In a terminal"
  (default for Claude and Codex) or "Headless" (fan-out rows and ACP agents always). A terminal start posts
  `/api/orch/terminal {agent, cwd, prompt, model?, permissionMode?, sandbox?, approvalPolicy?}`; the fields
  map to flags through an allow list (Claude `--model`, `--permission-mode acceptEdits|plan|auto`; Codex
  `--model`, `-s read-only|workspace-write`, `-a untrusted|on-request|never`), so remote callers may set
  them; raw `args` stay loopback only. The UI shows the tmux session and `tmux attach -t am-xxxx`, polls
  `GET /api/hosts/{id}` until `agentId` appears (a hook linked the session; a `host` event fires too), then
  opens that thread.
- Templates have `where` (empty = headless, for old templates; new ones default to terminal); schedules
  have `where` ("" = the template's) and record `lastSession`/`lastHostId`; a live host counts as busy.
  Template and schedule runs answer `{terminal}` instead of `{thread}`.
- CLI: `GET /api/targets` merges tmux panes and linked hosts; `/api/targets/resolve?q=` resolves alias,
  agent id, pane, host id, session-id prefix or a unique tool name. send/read/type/keys/list use them; a
  talk to a hosted session resolves on the daemon; `id` and `send` use `AM_HOST_ID`.
- Ctrl-Z: a pty session leader's process group is orphaned and the kernel drops SIGTSTP there, so the
  host runs `agent-monitor __pty-leader -- <agent>`, which starts the agent in its own foreground group
  (Setpgid, Foreground; SIGTTOU ignored, which the agent inherits) and reports its pid on fd 3. An agent
  stop makes the leader SIGSTOP itself; the host sees it (wait4 WUNTRACED; Go's BSD WaitStatus.Stopped
  hides SIGSTOP, so the status bits are tested), restores the terminal and sends itself SIGTSTP. On
  resume: raw mode, size, SIGCONT to the leader, which re-takes the foreground and continues the agent's
  group. A host that leads its own session (tmux exec) continues the agent at once.
- Trace and search: slash and ! commands become trace spans (kind tool, `by:"user"`, name `!` or the
  slash command, output as the result) and search docs (kind command, tool "user"), from the
  watched_cmds.go parsers. The search index keeps a generation in `PRAGMA user_version`; a lower one
  rebuilds the index once.
- A Codex ! command before the first prompt is a turn start (run prompt `!<cmd>`, completed with the
  command).

# Usage, voice and command history (2026-11-18)

All routes use the same auth as the rest of the API: loopback, or a paired device token.

## GET /api/usage/limits

Cached for 30 s. Transcripts are read incrementally; the scan positions are saved in
`scanpos-usage.gob` (version 2: buckets are per minute and per model).

```
{ generatedAt: ms, agents: [AgentUsage] }   // always 5 agents, in this order: claude, codex, opencode, cursor, pi
AgentUsage {
  agent: "claude"|"codex"|"opencode"|"cursor"|"pi"
  installed: bool          // binary on PATH or in a known install dir (/opt/homebrew/bin, /usr/local/bin,
                           // ~/.local/bin, ~/.npm-global/bin, ~/.bun/bin, ~/.volta/bin, ~/.opencode/bin, ...)
  source: string; sourcePath?: string; measuredAt?: ms
  plan?: string            // omitted when unknown
  note?: string
  windows: [UsageWindow]   // plan windows; [] for opencode, cursor, pi
  local: [UsageLocal]      // "5h" and "weekly" totals; [] for pi
  localSource: string
  models: [UsageModel]     // never null
  credits?: UsageCredits   // only when the agent records it locally
}
UsageWindow { key: "5h"|"weekly", label, windowMinutes, usedPercent: number|null, lastPercent?, resetsAt?: ms,
  expired?: bool, history: [{at: ms, pct}] }
UsageLocal { key: "5h"|"weekly", label, from: ms, aligned: bool, tokens: {input, output, cacheRead, cacheCreate},
  costUsd, costEstimated: bool, messages }
UsageModel { window: "5h"|"weekly", model: string, inTokens, outTokens, cacheRead, cacheWrite,
  costUsd, costEstimated: bool, messages }
UsageCredits { source: string, measuredAt?: ms, hasCredits?: bool, unlimited?: bool,
  balance?: number|string /* as recorded */, spendUsd?: number }
```

- `models` has one row per model per window. The rows of one window add up to that window's `local` entry.
  The "5h" window starts at the plan window start when a plan reading has a future reset (`aligned`),
  else at now minus 5 h; "weekly" likewise with 7 days. Sorted by window, then model name.
- Sources (read only; no credential, token or keychain file is read):

| agent | installed | plan | models | credits |
|---|---|---|---|---|
| claude | `claude` | `~/.claude.json` `oauthAccount.organizationType` and `userRateLimitTier` only (`"team (max_5x)"`); re-read at most every 10 min | `~/.claude/projects/**/*.jsonl` assistant `message.model` + `usage`, cost estimated from the price table | none recorded |
| codex | `codex` | rollout `rate_limits.plan_type` | `~/.codex/sessions/**/*.jsonl` `token_count` growth, model from the last `turn_context` ("unknown" before one) | rollout `rate_limits.credits` `{has_credits, unlimited, balance}` |
| opencode | `opencode` | none (provider auth only) | `~/.local/share/opencode/storage/message/<session>/msg_*.json`: `role:"assistant"`, `providerID/modelID`, `tokens.{input, output+reasoning, cache.read, cache.write}`, `cost` as recorded (0 for subscription providers), `time.created` | none |
| cursor | `cursor-agent` or `cursor` | none recorded locally | `~/.cursor/ai-tracking/ai-code-tracking.db` (read-only): `ai_code_hashes` distinct `requestId` per `model` by `createdAt`. Tokens and cost are 0: Cursor records none locally | none |
| pi | `pi` | none | none (`note` says why) | none |

## POST /api/voice/clean

```
→ {text: string, mode?: string /* [A-Za-z0-9_-]{1,32}, passed as DICTATE_MODE */}
← {text: string, cleaned: bool, ms: number, error?: string}
```

- Runs `/bin/bash ~/voice-dictation/clean.sh` (env `AM_VOICE_CLEAN` overrides the path) with the raw
  text on stdin and reads the clean text from stdout. No shell string is built. The script reads its
  engine from `~/.config/voice-dictation/dictate.conf`.
- Limits: 8 KB of text (413 above), 20 s timeout (the script's process group is killed), at most 2 runs
  at once (a third request waits up to 5 s for a slot).
- A missing script, a non-zero exit, empty output, a timeout or a busy cleaner answer 200 with the raw
  text, `cleaned:false` and `error`. 400 only for a bad body or mode. Empty text answers at once.

## GET /api/voice/status

`{available: bool /* the script exists */, engine: string}`. `engine` is the last `engine=` line of
`dictate.conf` (env `AM_VOICE_CONF` overrides the path), else `claude` as in clean.sh. No other line
and no token file is read.

## GET /api/session/{id}/commands[?limit=200]

`{items: [CmdItem]}`, newest first, limit 1 to 500 (default 200). 404 no transcript, 501 an agent other
than Claude or Codex.

```
CmdItem { cmd: string, at: ms, exitCode?: number, output: string /* first 2 KB, ANSI removed */, bg?: true,
  sessionId?: string, cwd?: string /* recent only */ }
```

- Claude: a user line with `<bash-input>` starts a command; `<bash-stdout>`/`<bash-stderr>` on it or a
  later line is its output. Claude Code records no exit code for a foreground `!` command, so
  `exitCode` is absent. A command moved to the background has `bg:true`; its `<task-notification>`
  supplies `exitCode`. Sidechain lines are skipped.
- Codex: `item_completed` items with `source:"user_shell"`: the command (shell wrapper removed),
  `aggregated_output` and `exit_code`.
- Each transcript is read incrementally (64 transcripts cached, 500 newest commands each); a smaller
  or replaced file is read again from the start.

## GET /api/commands/recent[?limit=50]

`{items: [CmdItem]}` with `sessionId` and `cwd`, newest first, one item per distinct `cmd` (the newest),
limit 1 to 500 (default 50). It reads the 40 most recently active Claude and Codex sessions of the last
14 days.
