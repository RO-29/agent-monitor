# agent-monitor — design notes

This document describes *why* agent-monitor is shaped the way it is. The
[README](README.md) covers what it does and how to use it; this file
covers the rationale for the architecture choices, the state machines
that aren't obvious from the code, and the trade-offs we explicitly took.

The intended audience is a future contributor (or future self) who wants
to make a non-trivial change without re-discovering every constraint.

---

## 1. North star: *registered, not inferred*

The earliest version of this project tried to match agent CLIs to tmux
panes by **heuristic** — score each pane by cwd similarity, command name,
process tree, and pick the best. This always lost to one of three
realities:

- Multiple Claude/Codex sessions in the same directory.
- The agent runs as `node` (Claude/Codex are both Node apps), making
  command-name matching ambiguous.
- The user moves panes around (kills a pane, splits a new one), and pane
  IDs renumber.

We replaced the matcher with an **explicit registry**: every drivable
pane is recorded by name when it starts. This makes:

- mis-routing impossible (a registration *is* the truth)
- the daemon dramatically simpler (~150 fewer lines, no scoring loops)
- failures actionable (you either have a registration or you don't)

The whole architecture below is downstream of this decision.

### Three registration paths

| Path     | When it fires                                               | Source field |
| -------- | ----------------------------------------------------------- | ------------ |
| Hook     | Claude `SessionStart` hook → `bin/claude-hook.sh` → daemon  | `hook`       |
| Wrapper  | `agent-monitor run <agent>` → announces, exec's, pairs      | `wrapper`    |
| Manual   | Web UI empty-state picker (or `register_self` MCP tool)     | `manual` / `register_self` |

A `sourceRank()` function (`pane_registry.go:sourceRank`) lets a higher-
priority source overwrite a lower one's entry for the same agent ID
(`manual > wrapper > hook`). Same-pane eviction (next section) takes
care of cross-agent collisions.

### One-agent-per-pane invariant

A tmux pane runs one agent at a time. The registry enforces this:

1. **Eviction on register** — `PaneRegistry.Register()` evicts every
   other entry that points at the same `paneId` before storing the new
   one. So `agent-monitor run claude` in pane `%3` immediately replaces
   any previous registration for `%3`.
2. **Periodic GC** — a 15 s ticker drops registrations whose paneId is
   gone from tmux OR whose recorded pid is no longer signalable.
3. **Startup dedupe** — `DedupeByPane()` keeps the newest entry per
   pane, in case pre-eviction state leaked through.
4. **On-demand sweep** — `/api/panes/sweep` runs GC + dedupe on demand.
   `list_agents` calls it before reading so the model never sees stale
   data even within the 15 s GC window.

### Why we don't decay registrations the way we decay sessions

The session store auto-decays entries (running → idle → completed → evict
after ~60 min) so the live UI stays clean. The pane registry **does
not** decay. The reason: pane drivability is a property of the *real
world* (does this tmux pane still exist? is the agent still running?),
not of the daemon's view of recency. We use `kill(pid, 0)` to probe
liveness directly. A long-running agent that hasn't touched its JSONL
in an hour is still drivable; it should keep its registration.

This is why `resolvePane()` looks at the registry only, never the
session store. An aged-out session is still drivable as long as the
underlying tmux pane is alive.

---

## 2. Two flows, one Talk struct: confirm vs reply

`talk_to_agent` has three modes, but lifecycle-wise they collapse to two
end-states:

```
                ┌─ default ──────► delivered (one shot)
                │
talk_to_agent ──┤
                │       ┌─ confirm:true ───► [user clicks Allow] ───► delivered
                │       │                  └─ [user clicks Deny] ───► denied
                └────►  ┤
                        └─ wait_for_reply:true ──► [recipient calls reply_to_talk] ──► replied
                                                └─ [timeout] ───────────────────────► timeout
```

The default path doesn't allocate a Talk struct at all — message goes
straight through `/api/session/send/`. The other two modes both register
a Talk because the sender needs a stable id for the long-poll rendezvous.

### Why Status matters

A single Talk participates in *one* of the two completion mechanisms:

- **`pending`** → resolved by `Respond()` via the user's Allow/Deny click.
  Sender is blocked on `Wait()` (channel: `resp`).
- **`pending_reply`** → resolved by `Reply()` via the recipient's
  `reply_to_talk` call. Sender is blocked on `AwaitReply()` (channel:
  `reply`).

The Status field tells the daemon which branch is in flight so:

1. `Register()` skips the `talk-request` WebSocket broadcast for
   `pending_reply` (no banner — message has already been delivered).
2. `Respond()` refuses to resolve a `pending_reply` talk — clicking
   Allow on a banner that shouldn't exist would corrupt the reply
   channel; we 409 Conflict instead.
3. `Reply()` only fires the `reply` channel, never `resp`.

Two channels per Talk (`resp` and `reply`) lets each path drain
independently without contention. They're both buffered (size 1) so the
sender and the resolver can race without deadlock.

### Why explicit reply rather than polling

An earlier version of `wait_for_reply` polled `tmux capture-pane` until
the buffer stopped changing for 3 s, then returned the diff. This was
brittle:

- Streaming responses (Claude's typewriter rendering) created false
  "stable" moments mid-stream.
- ANSI escape sequences shifted on every cursor move, so even
  byte-identical text triggered `!=` diffs.
- Long reasoning before the answer appeared exceeded the stability
  threshold and we returned the prompt instead of the response.
- We had no signal for "the agent finished but is asking a follow-up."

The reply protocol shifts the problem: instead of inferring "done," we
ask the recipient to *say* it. The recipient parses a known prefix
(`[talk from … · REPLY-EXPECTED · talk_id=… · use reply_to_talk(...)]`),
does the work, then calls `reply_to_talk(talk_id, message)`. The daemon
delivers the reply on the channel and the sender unblocks.

Cost: the recipient must know the protocol. We mitigate by:

- Embedding instructions in the message prefix itself.
- Documenting the recipient-side protocol in the `talk_to_agent` and
  `reply_to_talk` MCP tool descriptions, which models read by default.
- Falling back to fire-and-forget + manual `read_pane` when the
  recipient can't participate.

### Audit log

Every Talk is appended to `~/.agent-monitor/talks.log` (NDJSON) on
creation and again on resolution. Useful for:

- Debugging "did my message get delivered?"
- Reviewing what an agent sent another agent overnight.
- Future replay / reproduction tooling.

---

## 3. MCP server is a separate process by design

The MCP perm server (`mcp_perm.go::runMCPPermServer`) is a **separate
invocation of the same binary** (`agent-monitor mcp-perm-server`) that
the agent CLI spawns. It reads JSON-RPC over stdin/stdout, talks to the
daemon over HTTP. Several reasons for this shape:

1. **Isolation.** A bug in the MCP server can't crash the daemon.
2. **Lifetime.** Codex/Claude expect their MCP server to live as long
   as they do. If the MCP server were inside the daemon, restarting
   the daemon would close the transport for every running agent CLI —
   exactly what you experienced when `pkill -f /agent-monitor/agent-monitor`
   killed the children. Now the MCP child is independent: `agent-monitor
   stop` (which kills only the listen-port owner) doesn't touch them.
3. **Per-agent identity.** Each MCP child knows which pane it's in via
   process-tree descent (`detectMyTmuxPane()`). A daemon-internal MCP
   would have no way to attribute incoming RPCs to a specific pane.

### Process-tree descent for self-identification

Codex strips `$TMUX_PANE` from the env it gives MCP children. We can't
rely on env, so:

```
detectMyTmuxPane() →
  os.Getenv("TMUX_PANE")                  // fast path (Claude propagates)
  ↓ if empty
  ps -A -o pid=,ppid=                     // build pid → parent map
  walk ancestors of os.Getpid() upward
  for each tmux pane: is pane_pid one of our ancestors?
  → return that pane id
```

Cost: one `ps -A` fork (~5 ms) + an O(depth) tree walk. Acceptable on
every MCP call because the answer is cached implicitly by the OS page
cache. We don't memoize because the result is stable per-process and
the MCP child is short-lived per agent CLI session.

### Why the MCP server returns objects, not arrays

MCP `structuredContent` MUST be a JSON object per the spec. Claude's
client validates strictly and rejects bare arrays with
`expected: record received: array`. Codex is lenient. We learned the
hard way and now route every payload through `ensureObject()`, which
wraps slices/scalars as `{items: ...}` if needed. See `mcp_perm.go::ensureObject`.

### Auto-register on `list_agents`

`list_agents` self-heals: if the calling pane has no registration, the
MCP handler synthesizes one (`<tool>:pane-<paneId>`, e.g.
`codex:pane-9`) before returning. This makes the model show up in its
own list with the `← you` marker even when it was launched without the
wrapper.

It also cross-references the registry against `tmux list-panes -a` and
surfaces any unregistered tmux pane that *looks* like an agent (running
`node`, `claude`, `claude.exe`, `codex`, `cursor-agent`, `opencode`),
flagged `status: unregistered`. Otherwise the model wouldn't know its
peers exist.

---

## 4. Network surface

| Trust boundary  | Default       | Override                                      |
| --------------- | ------------- | --------------------------------------------- |
| Listen address  | `127.0.0.1`   | `AGENT_MONITOR_BIND=0.0.0.0` or specific IP   |
| WebSocket origin | accept any   | (intentional — same-host only by default bind) |
| MCP transport   | stdin/stdout | (no network surface)                          |

The default is loopback-only because exposing the daemon externally is
a foot-gun: anyone who can reach the port can drive every registered
tmux pane. We document `AGENT_MONITOR_BIND=100.x.y.z` (a specific
Tailscale IP) as the recommended exposed configuration — that limits
reachability to your tailnet rather than every network you join.

---

## 5. Web UI architecture

The web UI is **vanilla JS, no build step**, served from
`web/index.html` (single file, embedded into the binary via `go:embed`).
Key invariants:

- **Pane bridge lives outside `#detail`.** The detail panel re-renders
  on every WebSocket update; if the bridge were inside it, the live
  viewer (`<pre>`), the textarea, and the poller state would be wiped
  every few seconds. We mount the bridge into a separate
  `#pane-bridge-mount` container that persists across detail re-renders.
- **One poller globally**, not one per session. State lives on
  `state.paneViewer` (sessionId, timer, lastContent, painted, tickCount).
  Switching sessions tears down the old poller and starts a new one;
  the bridge node carries `data-bridge="<sessionId>"` for identity.
- **Persisted preferences** live in `localStorage` with explicit keys:
  `agent-monitor-pane-overrides` (deprecated; from the heuristic-matcher
  era), `agent-monitor-no-auto-register` (per-session opt-out for the
  empty-state auto-pick).

### Mobile responsive

Below 600 px the layout switches to single-pane navigation: list →
detail (with a `← list` back button), and the rail collapses behind a
hamburger. Uses `100dvh` (dynamic viewport height) instead of `100vh`
so iOS Safari's collapsing URL bar doesn't clip content.

---

## 6. Failure modes and what we do about them

| Symptom                                              | Detection                                           | Recovery                                  |
| ---------------------------------------------------- | --------------------------------------------------- | ----------------------------------------- |
| Agent CLI exits                                      | Claude: SessionEnd hook. Others: pid-aliveness GC.  | Registration removed (instant / 15 s).    |
| tmux server restarted (pane IDs renumbered)          | Pane GC sees old paneIds missing.                   | Registrations dropped.                    |
| User force-killed daemon                             | Stop subcommand uses port-owner pid (lsof).         | Survivors are MCP children; restart fine. |
| Same pane re-launched                                | Eviction-on-register.                               | Old registration dropped immediately.     |
| Recipient ignores REPLY-EXPECTED prefix              | Sender's `wait_seconds` timer.                      | Talk goes to `timeout` status.            |
| MCP transport closed (e.g., child killed)            | Codex/Claude show "Transport closed."               | Restart the agent CLI; daemon stays up.   |
| Registered pane vanished (tmux killed it)            | `resolvePane()` returns 410 with `paneId`.          | UI prompts user to forget + relaunch.     |

---

## 7. Subcommand catalogue (and why each exists)

```
agent-monitor                  daemon (default with no args)
agent-monitor install          patches Claude/Codex configs (idempotent)
agent-monitor stop / restart   safe lifecycle (port-owner only)
agent-monitor mcp-perm-server  the spawned-by-agent MCP child
agent-monitor run <agent>      register-then-exec wrapper
agent-monitor send <to> "msg"  CLI counterpart of talk_to_agent
agent-monitor list / list-agents
agent-monitor name <id> <alias>
agent-monitor id               which agent am I (uses $TMUX_PANE)
agent-monitor resolve <addr>   echo canonical agent id
agent-monitor read <to> [n]    capture another pane's buffer
agent-monitor type <to> "txt"  type without Enter
agent-monitor keys <to> <key>  send tmux key names
agent-monitor version
```

The `read / type / keys / id / resolve / list / name / send` set
mirrors the [smux](https://github.com/ShawnPana/smux) CLI surface, by
design — agents can shell out to these from any language without
needing MCP. The web UI + MCP tools are the same operations through
different transports.

---

## 8. Storage layout

```
~/.agent-monitor/
├── panes.json        # PaneRegistration list, persisted on every change
└── talks.log         # NDJSON, one line per Talk creation + resolution
```

`panes.json` survives daemon restarts (the registry rehydrates it on
boot). `talks.log` is append-only; nothing reads it back at runtime.
Both files are atomic-replaced on write (write to `.tmp`, then rename)
so an interrupted write can't leave a corrupt JSON.

Backups: every config patch from `agent-monitor install` writes a
`<file>.bak-YYYY-MM-DD` sibling on first edit per day. So you can roll
back any of the patched files (`~/.claude/settings.json`,
`~/.claude.json`, `~/.codex/config.toml`) by `mv` in the same dir.

---

## 9. Things deliberately NOT done

- **No persistence for the session store.** Sessions live in memory and
  are reconstructed by tailing JSONL files on startup. Restarting the
  daemon loses the in-memory snapshot — but the underlying data is
  still on disk and re-tailed.
- **No authentication on the HTTP API.** The default loopback bind is
  the security boundary; if you bind externally you accept the
  consequences. Adding auth (token, mTLS) is straightforward but
  would compromise the zero-config local-dev experience.
- **No formal tests.** This is a one-author tool that's been tested
  by use, not by suite. A `tests/` directory with at least the talk
  state machine and the registry's GC/dedupe paths covered would be
  worthwhile if this grows beyond solo use.
- **No central message bus.** Talks are point-to-point: sender → daemon
  → recipient pane. We don't try to do publish/subscribe or topic
  routing. If you want broadcast, fan it out client-side.
- **No version negotiation between MCP children and the daemon.** They
  share a binary, so they're always the same version. If the daemon
  binary on disk is updated mid-flight, the running MCP children keep
  running their old code until they exit naturally — that's fine.

---

## 10. Future extension points

If you wanted to extend the system, the cleanest seams:

- **New agent type.** Add `cool_agent.go` mirroring `codex.go` (an
  adapter that tails its JSONL/database and upserts sessions). Add
  `ToolCoolAgent` to `types.go`. The wrapper, talk system, MCP tools
  all work for it for free.
- **New MCP tool.** Append to the `tools/list` response in
  `mcp_perm.go`, add a case in the dispatch switch, write
  `handleX(daemonURL, args, id, writeJSON, logf)`. Use the existing
  HTTP helpers (`httpJSON` / `httpPostJSON`) to talk to the daemon.
- **New web UI panel.** Add markup in `web/index.html` and a render
  function. The WebSocket pushes session/perm/pane/talk events; pick
  what your panel needs and subscribe.
- **Persistent talks (cross-restart).** `talks.log` is already NDJSON;
  add a startup pass that re-loads pending talks (those without a
  resolution line) into the in-memory map. Channels would be re-created
  empty; senders that were waiting would have to retry.
