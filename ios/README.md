# Agent Monitor — iOS app

A native SwiftUI client for the `agent-monitor` daemon. Mirrors the web
dashboard and adds phone-first controls, designed to be used over Tailscale.

## Features ported from the web UI

- **Agents** — live list of every session (tool, state, project, model, branch,
  message count, tokens, last message). Sessions needing a human float to the top.
- **Session detail (trace view)** — the phone port of the web's redesigned
  detail: a KPI strip (tokens · turns · tools · errors · duration · files), an
  **activity ribbon** (stacked density histogram over the session's timespan),
  a **faceted toolbar** (All · Prose · Tools · Errors · You + per-tool filter +
  search), and one **unified stream** of prose turns with their tool calls
  nested underneath — the `→ Bash` dispatch stubs are suppressed so a 1000-turn
  session reads cleanly. On-demand via `/api/session/:id/full`.
- **Open on Mac** — the ↗ button raises that session's tmux pane and terminal
  app on your Mac, from the phone, over the tailnet (`/api/session/focus/:id`).
- **Approvals** — pending permission prompts with Allow / Deny, in real time.
  The headline mobile feature: approve tool calls from your phone.
- **Pane bridge** — live tmux capture (polled) plus send-text, quick keys
  (Enter/Esc/Y/N/1-3/arrows/Tab/⌫), and Ctrl-C cancel.
- **Talks** — inter-agent message inbox with Allow / Deny, replies, and a
  composer to send a new talk to any registered pane.
- **Panes** — the registry: alias, tool, pane id, cwd, source, last-seen;
  swipe to rename.
- **Settings** — server URL (defaults to the Tailscale IP), connection status,
  health check, and live counts.

All data is hydrated over REST on connect and kept live through the single
`/ws` WebSocket (sessions, permissions, panes, talks are multiplexed by `kind`).

## Configuration

Default server is `http://100.68.125.93:7777` (the daemon's Tailscale bind).
Change it in **Settings → Daemon**. App Transport Security allows the cleartext
http/ws connection (see `Info.plist`), and the daemon must be bound to an
interface your phone can reach — run it with
`AGENT_MONITOR_BIND=<tailscale-ip>` (see the repo's Tailscale setup).

## Build & run

Requires Xcode 16+ (developed against Xcode 26.2, iOS 26 SDK; deployment
target iOS 17).

```sh
open ios/AgentMonitor.xcodeproj
```

1. If Xcode prompts, install the iOS platform component.
2. Select the **AgentMonitor** scheme and your iPhone (or a simulator).
3. Set your signing team on the target (bundle id `com.rohit.agentmonitor`).
4. Run. Your phone must be on the same tailnet as the Mac running the daemon.

The project uses an Xcode file-system-synchronized group, so any `.swift` file
added under `AgentMonitor/` is picked up automatically — no pbxproj edits.

## Validated

- `swiftc -typecheck` of all sources against the iOS 26.2 simulator SDK: clean.
- `xcodebuild -list`: project and scheme load correctly.

A full `.app` link/run wasn't done in the authoring environment because the iOS
platform runtime wasn't installed there; build on a Mac with Xcode's iOS
platform present.
