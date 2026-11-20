# Plan: status line in the thread screen, clear usage, text selection

Branch `feat/status-usage` in worktree `~/agent-monitor-status`, base `main` at 7501283.

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

- New `paneStatusLine(tool, screen string) []string` beside `cliInputBox` (cli_queue.go):
  - Claude Code: the non-empty lines after the closing rule of the input box, ANSI removed, at most 4
    lines, each clipped to 200 runes.
  - Codex: the non-empty lines below the `›` composer row.
  - Empty when a prompt or a slash panel is open (no input box found), so stale text never shows.
- Served in two places, from one capture: the pane-view response (`main.go:507`) gets `statusLine`, and
  the cli-queue view (`GET /api/session/:id/cli-queue`, which both thread screens already poll every 2 s)
  gets `statusLine`. The cli-queue view already captures the screen, so this adds no tmux call.
- Host-registry sessions (am-helper) use the same function on the host screen.
- Web `WatchedChat.tsx`: one muted monospaced row (wraps on narrow screens) directly above the composer,
  shown whenever `statusLine` is non-empty. It replaces the model-only label at line 480 when present.
- iOS `ThreadComposer.swift`: the same row above the text field, `.font(.caption.monospaced())`,
  secondary colour, `lineLimit(4)`.
- Tests: Claude screen with a 3-line footer, Codex footer, prompt open gives empty, slash panel gives
  empty, ANSI and clip.

## Part B: usage page

- Claude (`usage_agents.go`, `usage_limits.go:619` already opens `~/.claude.json`): decode
  `cachedUsageUtilization {fetchedAtMs, accountUuid, utilization}`. Use it only when `accountUuid`
  equals `oauthAccount.accountUuid`.
  - `limits[]` rows become windows: `session` (Current session), `weekly_all` (Current week, all models),
    `weekly_scoped` with `scope.model.display_name` (Current week, Fable). Keep `resets_at`.
  - `spend` (`used`, `limit`, minor units with `exponent`) becomes `credits` with `spentUsd`, `limitUsd`,
    `percent`, `resetsAt` when present, `capReached` from `extra_usage.spend_limit_reached`.
  - The reading age (`fetchedAtMs`) is shown; older than 6 hours is marked stale. The status-line copy
    (dietclaude) stays as the fallback and for the history chart.
- Codex (no credential): read `rate_limit_reached_type` and `spend_control_reached` from the newest rollout
  `token_count.rate_limits`. `workspace_member_usage_limit_reached` shows "Workspace spend cap reached (set
  by the workspace owner)"; the other enum values get their own text. Plan `business` with no windows says
  "Business plan: limits are set by the workspace owner".
- Codex amounts (`account/rateLimits/read` on a Codex app-server) are NOT in this plan: the call runs with
  the user's Codex login. It is a separate item that needs the user's yes.
- Web `components/Usage.tsx` and `views/settings/Limits.tsx`, iOS `UsageView.swift`: one card per agent:
  plan name, each window as a bar with its label and reset time, a credits bar "<spent> of <limit> spent,
  resets Dec 1", a red line when a cap is reached, and the reading age.
- Tests: fixture of `cachedUsageUtilization` (values replaced), account mismatch ignored, stale flag,
  Codex enum values.

## Part C: pi detection

- One helper `agentBinDirs()` used by `usage_agents.go:15`, `agent_registry.go:128`, and
  `orch_agents.go:150`: the existing dirs plus `~/.nvm/versions/node/<default>/bin` (from
  `~/.nvm/alias/default`, else the newest version). Test with a temp HOME.

## Part D: text selection on iOS

- New `SelectableText: UIViewRepresentable` (non-editable, selectable `UITextView`, no scrolling, sizes to
  its content, dynamic type, link detection off) for plain text, and an attributed variant for `Prose`
  markdown (render the markdown to `AttributedString`, then `NSAttributedString`).
- Use it for the user bubble (`ThreadChatView.swift:697`) and assistant prose (`Components.swift:436`).
  Tool output and code blocks keep `Text` plus a "Select text" context-menu item that opens a sheet with
  `SelectableText` (long output in a sheet is easier to select on a phone).
- The existing context menus stay; a long press on selectable text starts selection, so the menu moves to
  a small "…" button on each message.
- Web and the Mac app already allow partial selection (checked: no `user-select: none` on messages).

## Order and gates

A (status line), C (pi), B (usage), D (iOS selection). Each part: code, tests, commit, push, scan,
`remote-build.sh --test`, then a Fable review of the diff (Codex review is blocked by the spend cap), then
merge to `main` and the install command. iOS parts also run `ios/resign.sh` on rohit-mac.
