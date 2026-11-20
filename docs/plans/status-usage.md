# Plan: status line in the thread screen, clear usage, text selection

Branch `feat/status-usage` in worktree `~/agent-monitor-status`, base `main` at 7501283.
Revision 2: changed after the Fable review (10 required changes applied).

## Requests (from the user, 2026-11-20)

1. Each agent's terminal status line is always visible in the thread screen (web and iOS).
2. Part of a message can be selected in the apps.
3. The Usage page shows clearly: Claude usage credits (spent of limit), Claude Fable weekly, the
   subscription windows (session and weekly), and Codex workspace usage.
4. pi shows "not installed" while it is installed (bug).

## Rules

- No build or test on this Mac; `bin/remote-build.sh <sha> --test` on rohit-mac. iOS builds on rohit-mac.
- Commit, push, `bin/privacy-scan.sh` before each build. One or two line comments. No new dependency.
- No credential is read by agent-monitor. No network call to Anthropic or OpenAI.

## Part A: status line (daemon, web, iOS)

- New `paneStatusLine(tool, screen string) []string` (plain capture, ANSI removed, at most 4 lines of 200
  runes). Fixtures from real screens in `testdata/screens/` (session ids replaced).
  - Claude Code: find the rule pair that holds the `❯` (or `!`) line, by index (new helper returning the
    indexes; `cliInputBox` keeps its text API). Take the non-empty lines after the closing rule and stop at
    the next `isBoxRule` line (panels below draw rules). When no `❯` pair exists, return nothing (no fallback
    pair). When the lines below the box are a slash or `@` suggestion list (first non-empty line starts with
    `/` or `@` after trimming, or two or more lines of the `  /name  description` shape), return nothing.
  - Codex: from the bottom up, take the contiguous non-empty lines; stop at a blank line or at the line
    starting with `›`. This skips the continuation lines of a multi-line draft (they sit above a blank line,
    see `codex-draft.txt`). Return nothing when `panePromptOpen` is true.
  - Other agents: nothing in this part.
- Transport: the thread screens poll different routes. Web polls `GET /api/queue/{thread}` every 5 s when
  owned (`queue.go:706`, `web/src/v3/api/queue.ts:188`); iOS polls the cli-queue view every 5 s
  (`ThreadComposer.swift:506`). Add `statusLine` to the cli-queue view struct and copy it into the
  `/api/queue/{thread}` response; keep it when `errCLINotSupported` drops the CLI view (Codex), by computing
  it in a separate step that only needs the pane target.
- Cost: one plain `capture-pane` per poll per open thread (5 s), reused when the view already captured the
  plain screen. `typedScreen` (dim removed) is not used, because it drops the dim footer.
- Host-registry sessions use the tmux capture when a TmuxPane exists, else the host screen (which can lose
  rows: then the line is simply missing).
- Web `WatchedChat.tsx`: a muted monospaced block above the composer, shown when `statusLine` is non-empty;
  it replaces the model-only label at line 480 when present.
- iOS `ThreadComposer.swift`: the same, `.font(.caption.monospaced())`, secondary colour, `lineLimit(4)`.
- Tests: Claude idle, Claude with a panel rule below, Claude slash suggestions, Claude prompt open, Codex
  idle, Codex multi-line draft, Codex prompt open, ANSI and clip.

## Part B: usage page

- Claude: decode `cachedUsageUtilization {fetchedAtMs, accountUuid, utilization}` in the same read of
  `~/.claude.json` that `claudePlanName` does (`usage_agents.go:50-72`, mtime cache plus 10 min recheck;
  `get()` caches 30 s). Use it only when `accountUuid` equals `oauthAccount.accountUuid`; never return the
  account id.
  - `limits[]` rows with `is_active` true become windows: `session`, `weekly_all`, `weekly_scoped` with the
    label "Week (<scope.model.display_name>)". Window keys are open strings from now on.
  - `spend` with `enabled` true becomes `credits`: `spent`, `limit` as numbers from `amount_minor` and
    `exponent`, the `currency` code, `percent`, and `capReached` from `extra_usage.spend_limit_reached`.
    No reset date (the source has none).
  - Reading age from `fetchedAtMs`; marked stale after 1 hour. The status-line copy (dietclaude) stays as
    the fallback and for the history chart.
- Codex (no credential): decode `rate_limit_reached_type` (string) and `spend_control_reached` (raw JSON,
  type unknown) from the newest rollout `token_count.rate_limits`, with the reading time.
  `workspace_member_usage_limit_reached` shows "Workspace spend cap reached (set by the workspace owner)";
  `workspace_owner_usage_limit_reached`, `workspace_owner_credits_depleted`,
  `workspace_member_credits_depleted` and `rate_limit_reached` get their own text. Plan `business` with no
  windows says "Business plan: limits are set by the workspace owner".
- Codex amounts: see Part E.
- Clients that assume two windows change too: `web/src/v3/api/usage.ts:9,25` (key union becomes string),
  `Usage.tsx:18` (`SHORT` map gets entries for the new keys and a fallback), `UsageMini` (`Usage.tsx:85`,
  used by `ThreadList.tsx:14`), `usage_limits.go:713` labels, iOS `V3Models.swift:485` and
  `UsageView.swift:101,136` (chip label per key; `ForEach` stays keyed by `key`, now unique).
- Each agent card: plan name, each window as a bar with label and reset time, a credits bar "<spent> of
  <limit> <currency> spent", a red line when a cap is reached, and the reading age.
- Tests: a `cachedUsageUtilization` fixture with invented values, account mismatch ignored, inactive row
  and disabled spend ignored, stale flag, the Codex enum values and a non-null `spend_control_reached`.

## Part C: pi detection

- One helper `agentBinDirs()` used by `usage_agents.go:15`, `agent_registry.go:128` and
  `orch_agents.go:149`: the existing dirs plus the nvm default bin dir. Resolve `~/.nvm/alias/default`
  through alias chains (`lts/*`, `22`, `node`) against `~/.nvm/versions/node/*`, comparing versions
  numerically; with no alias, the newest version.
- A binary found in such a dir runs with that dir first on the child `PATH` (`binaryVersion`,
  `orch_acp.go:416` ACP start), because pi starts with `#!/usr/bin/env node`.
- Tests with a temp HOME: alias to a version, alias chain, no alias, PATH of the child.

## Part D: text selection on iOS

- Keep `Text`. Add "Select Text" to `CopyMenuItems` (`CopyKit.swift:170`). It opens a sheet with a
  non-editable, selectable `UITextView` (`SelectableText: UIViewRepresentable`) holding the plain text of
  that message or block. `CopyButton` and the existing context menus show the item, so every message and
  tool output gets it with no layout change.
- Web and the Mac app already allow partial selection (no `user-select: none` on messages).

## Part E: Codex amounts (user approved 2026-11-20)

- Confirmed with `codex app-server generate-json-schema` (no network): request `account/rateLimits/read`, params
  `{}` (optional `excludeResetCreditDetails`, `supportsLunaReserve`); response `rateLimits` (required),
  `rateLimitsByLimitId`, `rateLimitResetCredits`, `ordinaryUsageAllowed`. A snapshot has `primary`/`secondary`
  `{usedPercent, windowDurationMins, resetsAt}`, `individualLimit {limit, used, remainingPercent, resetsAt}`
  (limit and used are decimal strings), `spendControlReached`, `rateLimitReachedType`, `planType`, `credits`.
- The daemon starts `codex app-server` (same launch code as driven threads, `newCodexAppServerCmd`), runs
  `initialize`, `initialized`, `account/rateLimits/read`, then stops it. Codex uses its own login: agent-monitor
  reads no token or auth file.
- Lazy and cached: at most every 10 minutes, only when Codex is installed and someone opens the usage page or a
  Codex thread. The read runs in the background (15 s timeout); the next poll shows it.
- The `codex` bucket maps to windows (when present), a credits bar (`individualLimit`: used of limit, percent
  from `remainingPercent`) and the cap alert. An error keeps the rollout flags and shows the error as the note.
  The field names of the answer are logged once, never values.
- Tests use a fake app-server (this test binary as `codex`); the real one is never called.

## Order and gates

A (status line), C (pi), B (usage), D (iOS selection). Each part: code, tests, commit, push, scan,
`remote-build.sh --test`, then a Fable review of the diff (Codex review is blocked by the spend cap), then
merge to `main` and the install command. iOS parts also run `ios/resign.sh` on rohit-mac.
