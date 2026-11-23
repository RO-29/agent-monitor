# Plan: every machine is native (one list, one workspace, usage per account)

Base `main` at c3f6b01. Request from the user, 2026-11-23.

## Request

1. No separate "other machine" block. This Mac and the other machines show in the same thread list,
   and a thread on another machine opens in the same workspace as a local one.
2. The daemon is only a control plane: each machine is seen at the level its grant gives
   (View only, or Full), and the screen says which one applies.
3. Usage shows per subscription: two machines on one Claude account show as one account used on both;
   two different accounts show as two. The same for Codex.
4. Web, the Mac app (same web app) and the iPhone.

## Rules

- No build or test on this Mac; `bin/remote-build.sh <sha> --test` on rohit-mac. iOS builds on rohit-mac.
- Commit, push and `bin/privacy-scan.sh` before each build. Comments of one or two lines. No new dependency.
- No credential is read. Account identity comes from `~/.claude.json` (already read) and from the Codex
  app-server (already started for rate limits). `~/.codex/auth.json` stays unread.
- No live usage figure, email or account id in the repo. Fixtures use invented values.

## Model

- A machine is `{name, self, level, online}`. This machine is one of them (`self: true`, level `full`).
- Levels shown to people: **View only** (internal `view`) and **Full** (internal `control`). Admin stays
  local only and is never given to a machine, so a machine cannot chain to a third one.
- Every thread row and every usage reading carries `machine`. One machine: no machine label at all.
  Two or more: a small machine label on each row, and a machine filter (All, then each name).

## Part A: daemon

1. `GET /api/fleet/threads` (view): this machine's `/api/v3/threads?compact=1` plus each online peer's,
   fetched in parallel (3 s per peer, last good copy kept 60 s). Each thread gets `machine` and, for
   peers, `level`. Peer ids stay as they are; the pair `(machine, id)` is the key.
2. `GET /api/fleet/usage` (view): this machine's `/api/usage/limits` plus each peer's, each agent tagged
   with `machine`. The client groups them by account (Part C).
3. `GET /api/fleet/machines` (view): `{name, self, level, online, version}` for this machine and peers.
4. Account identity in `agentUsage`: `account {key, label, org}`.
   - Claude: `key` = first 16 hex of sha256("agent-monitor/account/" + accountUuid); `label` = emailAddress;
     `org` = organizationName.
   - Codex: `account/read` in the same app-server session as `account/rateLimits/read`. `key` from
     `accountId` (or `userId`) hashed the same way; `label` = email; `org` = workspaceName.
   - The raw ids never leave the daemon.
5. Proxy allowlist (`peerRoutes`), so the workspace works on a peer:
   - GET at view: `session/{id}/(children|question|pane-tail)`, `orch/threads/{id}`,
     `orch/threads/{id}/messages`, `v3/thread/{id}`.
   - POST at control: `session/{id}/answer`, `session/{id}/keys`, `orch/threads/{id}/(continue|cancel)`,
     `orch/uploads`.
   - The peer still checks its own grant on every call. Terminals stay out (admin only).
6. Level refresh: `GET /api/peers` reads the peer's `/api/devices/self` (new, view) to learn the current
   level, so a level change on the other side shows here within 30 s.

## Part B: web and the Mac app

1. `api/base.ts`: one `apiFor(machine)` that returns `/api` for this machine and `/api/m/<name>/api` for a
   peer. `p7`, `orch` and the watched feed take it, so WatchedChat and DrivenChat work for a peer.
2. Thread list: rows come from `/api/fleet/threads` merged with the local live feed (local rows keep the
   SSE updates). `WRow.machine` added. The `PeerThreads` block and the `/m/:peer` page go away; the
   peer needs-you rows join the main Needs you list with a machine label.
3. Route `/t/<machine>/<id>` for a peer thread (local threads keep `/t/<id>`).
4. View only: the composer is replaced by one line "View only on rohit-mac. Ask its owner for Full access."
   Approvals and questions show without buttons. Full: the composer and the answer buttons work.
   Terminal, git and diff panels hide for a peer (admin only).
5. Machine filter in the thread list header when there are two or more machines.
6. Settings > Machines: the level list shows "View only" and "Full".

## Part C: usage per account

- Group fleet usage by `(agent, account.key)`. A reading with no key groups by `(agent, machine)`.
- One card per account: plan, `label` and `org`, the machines that use it ("on rohit-mbp, rohit-mac"),
  and the windows and credits from the newest reading among those machines (shared limits are the same
  number on each machine; the newest wins).
- Local token counts stay per machine under the card.
- Web `UsagePanel` and `UsageMini`, the status line credits, and iOS `UsageView` use the grouping.

## Part D: iPhone

- The iPhone already keeps a direct grant per machine. It reads `/api/v3/threads?compact=1` and
  `/api/usage/limits` from each of its machines and merges them with the same rules (machine label,
  filter, usage per account). A thread opens with that machine's `APIClient`, and its level decides
  the composer.
- `RemoteNeedsSection` folds into the main list with a machine label.

## Order and gates

A (daemon, with tests: fleet merge with a fake peer, account key from fixtures, proxy routes and levels),
then B and C (web), then D (iOS). Each part: code, tests, commit, push, scan, `remote-build.sh --test`,
install command. iOS: the rohit-mac resign job.

## Not in this plan

- A terminal on a peer (needs a WebSocket proxy and a new level). Later, if wanted.
- Starting a new thread on a peer from this Mac. Later: the New Thread sheet gets a machine picker.
