# agent-monitor

A single Go daemon that watches every coding-agent CLI you run (Claude Code,
Codex, Cursor, opencode, cursor-agent), gives you one **web dashboard** to see
all of them live, and lets you **drive their tmux panes from the browser** —
including answering permission prompts, sending text, and routing messages
between agents.

## 🌐 &nbsp;[**Visit the website → agent-monitor-5xo.pages.dev**](https://agent-monitor-5xo.pages.dev)

## Demo

Headlines: Claude (one tmux pane) asks Codex (another pane) to code-review
a file via the `talk_to_agent` MCP tool; the same daemon also serves a live
web dashboard with talk-to picker, mobile layout, and per-session detail.

### ▶ &nbsp;[**Live site &amp; interactive demo →**](https://agent-monitor-5xo.pages.dev)

```
┌────────────────────────────┐                ┌────────────────────────────────┐
│  Browser (desktop / phone) │ <───ws/http──> │  agent-monitor daemon (Go)     │
│  ─ session list            │                │  :7777                          │
│  ─ live tmux pane viewer   │                │                                 │
│  ─ Send / Cancel / Keys    │                │  ┌──────────────────────────┐  │
│  ─ Allow / Deny prompts    │                │  │ adapters                 │  │
│  ─ Talk-to picker          │                │  │  Claude  Codex  Cursor   │  │
│  ─ Inbox banners           │                │  │  opencode  cursor-agent  │  │
└────────────────────────────┘                │  └──────────────────────────┘  │
                                                │  ┌──────────────────────────┐  │
┌────────────────────────────┐                │  │ pane registry            │  │
│  CLIs (claude/codex/…)     │                │  │  agentId → paneId        │  │
│  inside tmux panes         │ <──tmux send-> │  │  aliases, persisted to   │  │
│  ─ launched via            │  keys / cap-   │  │  ~/.agent-monitor/       │  │
│    `agent-monitor run X`   │  ture-pane     │  └──────────────────────────┘  │
│  ─ identity recorded by    │                │  ┌──────────────────────────┐  │
│    SessionStart hook OR    │                │  │ talk store               │  │
│    wrapper announcement    │                │  │  request + (allow/deny + │  │
│  ─ MCP client of daemon ◄──┼──── stdio ────►│  │  reply) channels +       │  │
│    (talk_to_agent,         │   JSON-RPC     │  │  audit log               │  │
│     reply_to_talk, …)      │                │  └──────────────────────────┘  │
└────────────────────────────┘                │  ┌──────────────────────────┐  │
                                                │  │ MCP server               │  │
                                                │  │  permission_prompt       │  │
                                                │  │  who_am_i / list_agents  │  │
                                                │  │  read_pane / talk_to_… / │  │
                                                │  │  reply_to_talk           │  │
                                                │  └──────────────────────────┘  │
                                                └────────────────────────────────┘
```

**Three rails of agent → daemon traffic:**

1. *Adapter ingest* (one-way, daemon ← filesystem) — adapters tail JSONL /
   scan chat dirs / read processes to populate the session store.
2. *Pane control* (two-way, daemon ⇄ tmux) — `tmux send-keys` for input and
   `tmux capture-pane` for the live viewer. Routed through pane registry.
3. *MCP* (two-way, daemon ⇄ agent CLI) — stdio JSON-RPC. Claude / Codex
   call the daemon's MCP tools to coordinate with each other; the daemon
   calls back into Claude only for `permission_prompt` (a Claude-specific
   protocol).

---

## Why this exists

Every coding agent leaves a trail:

| Tool          | Trail                                          |
| ------------- | ---------------------------------------------- |
| Claude Code   | `~/.claude/projects/*/<id>.jsonl` + hooks      |
| Codex         | `~/.codex/sessions/*/<id>.jsonl`                |
| opencode      | `~/.local/share/opencode/storage/`              |
| Cursor IDE    | `~/.cursor/chats/<wsHash>/<chatId>/store.db`    |
| cursor-agent  | a `cursor-agent` process scanned via `ps`       |

`agent-monitor` reads them all into one in-memory store, broadcasts changes
over a WebSocket, and serves a single-page web app that shows the unified
timeline. From there you can drive each agent's tmux pane (view, type, send
keys, cancel) and route messages between agents.

---

## Quick start

```bash
# 1. build
go build -o agent-monitor .

# 2. wire it into Claude Code + Codex (idempotent; backs up edited files)
./agent-monitor install

# 3. run the daemon (foreground)
./agent-monitor

# Or in background, listening on every interface (Tailscale / LAN reachable)
AGENT_MONITOR_BIND=0.0.0.0 nohup ./agent-monitor >> agent-monitor.log 2>&1 &
```

`agent-monitor install` patches three files (each gets a daily `.bak-YYYY-MM-DD`
sibling on first edit):

| File                          | Adds                                                           |
| ----------------------------- | -------------------------------------------------------------- |
| `~/.claude/settings.json`     | SessionStart / Notification / PreToolUse hooks                 |
| `~/.claude.json`              | `mcpServers.agent-monitor` (so Claude exposes the new tools)   |
| `~/.codex/config.toml`        | `[mcp_servers.agent-monitor]` (skipped if Codex isn't installed) |

### Lifecycle commands

```bash
agent-monitor              # foreground daemon
agent-monitor stop         # kill ONLY the daemon (by listen-port owner)
agent-monitor restart      # stop + start
```

> **Don't `pkill -f /agent-monitor/agent-monitor`.** That pattern matches
> every `agent-monitor mcp-perm-server` child too — those are the MCP
> servers that codex/claude spawned and rely on for the `talk_to_agent` /
> `read_pane` / `list_agents` / `reply_to_talk` tools. Killing them
> closes the MCP transport, and codex/claude won't auto-reconnect — you
> have to restart the agent CLI itself. Use `agent-monitor stop` instead.

Open `http://localhost:7777`. To reach the dashboard from another device, use
Tailscale (or a LAN IP). The daemon prints every reachable URL on startup:

```
agent-monitor listening on http://0.0.0.0:7777
⚠ exposed beyond localhost — anyone who can reach 0.0.0.0 on port 7777 controls every registered tmux pane.
  Reachable URLs:
    http://localhost:7777
    http://192.168.1.16:7777
    http://100.68.125.93:7777 (Tailscale)
```

> Don't bind to `0.0.0.0` on untrusted networks — anyone who can reach the
> port can write into your tmux panes. Bind to a specific Tailscale IP
> (e.g. `AGENT_MONITOR_BIND=100.68.125.93`) to expose tailnet-only.

---

## Pane registration: how an agent becomes drivable

Heuristic matching ("which pane is this Claude session in?") is fundamentally
guesswork — multiple Claude panes in similar cwds will collide. The daemon
deletes that approach entirely; instead, every drivable pane is **registered
explicitly** at launch time. There are three paths in:

### 1. Claude — `SessionStart` hook (auto)

`bin/claude-hook.sh` POSTs to `/api/pane/register` whenever Claude fires its
`SessionStart` hook. The hook payload includes `session_id`; the script
appends `$TMUX_PANE` and `$TMUX` from the surrounding shell.

```bash
agent-monitor install-hooks   # patches ~/.claude/settings.json
```

After install, every new Claude session inside tmux is registered
automatically. Sessions started outside tmux simply have no pane bridge.

### 2. Wrapper (any agent) — `agent-monitor run`

For Codex / cursor-agent / opencode (or even Claude if you prefer uniform
launch), wrap the agent:

```bash
tmux new -s work
agent-monitor run codex
agent-monitor run cursor-agent
agent-monitor run opencode
agent-monitor run claude        # also works
```

The wrapper:

1. Reads `$TMUX_PANE`. Errors clearly if not in tmux.
2. POSTs `/api/pane/announce` with `{tool, paneId, cwd, …}`. The daemon
   stores it in a 60-second pending list.
3. `exec`s the real agent — wrapper process vanishes.
4. When a new session of that tool from that cwd appears in the store
   (via the JSONL adapter), the daemon **pairs** it with the pending entry
   and publishes a registration.

### 3. Manual via the UI

If a session was already running before you set up the hook, the bridge
shows a picker of every live tmux pane. Cwd-matches float to the top with a
green badge. **One click registers; if there's exactly one cwd-exact match,
it auto-registers.** The "forget" button removes the registration and
suppresses auto-register for that session until you pick again — so unpinning
actually unpins.

---

## What the bridge gives you

Once a session is registered, the bridge appears above the detail panel:

```
LIVE PANE   ↳ %3 · backend (manual)        tick 12 · 18ms  ☑auto-scroll  ↻ name… talk to →  forget
┌───────────────────────────────────────────────────────────────────────────────────────────┐
│  > Working on the auth refactor…                                                          │
│  ⏵ (tool: Bash)                                                                            │
│   …                                                                                        │
└───────────────────────────────────────────────────────────────────────────────────────────┘
[ Type a prompt to send into the tmux pane running this session…              ]
[ Send + Enter ] [ Send (no Enter) ]                                  [ Cancel (Ctrl-C) ]
quick keys:  [1 ↵] [2 ↵] [3 ↵] [y] [n] [↵] [esc] [↑] [↓] [⇥]
```

- **Live viewer**: polls `tmux capture-pane` ~every 1.2 s, ANSI → HTML.
- **Send + Enter / Send (no Enter)**: types text, optionally presses Enter.
- **Cancel**: sends Ctrl-C.
- **Quick keys**: answers menu prompts (`1 ↵` = "Allow once", `3 ↵` = "Deny",
  arrows / Esc / Tab for navigation). When the session is in
  `awaiting-permission`, the row gets a yellow halo.
- **name…**: assigns a human-friendly alias (used by `agent-monitor send`).
- **talk to →**: opens the agent-to-agent picker (see below).
- **forget**: removes the registration; bridge collapses to the empty-state
  picker. Auto-register is suppressed until you pick manually again.

---

## Agent-to-agent talk

Any agent can message any other registered agent. The recipient's web view
gets a confirmation banner; if the user clicks **✓ Deliver to my pane**, the
message is typed into the recipient's tmux pane wrapped in a fenced block:

```
[from claude/abc123 — agent-monitor talk]
```
hey - what's your status on the auth refactor?
```
```

(Then Enter is pressed so the receiving CLI sees a fresh prompt.)

### From the CLI

```bash
agent-monitor send <recipient> "your message"
```

`<recipient>` accepts any of:

- alias                         (e.g. `backend`)
- exact agent id                (e.g. `claude:abc123-…`)
- tmux pane id                  (e.g. `%3`)
- session-id prefix (≥ 6 chars)

Sender is auto-discovered from `$TMUX_PANE` unless `--from <id>` is given.
The CLI long-polls until the recipient allows / denies / times out, then
prints the outcome and exits with a status code (0 = delivered, 3 = denied,
4 = timed out).

### From the web UI

Click **"talk to →"** on any registered session, pick a recipient from the
list, type your message. Recipient sees the banner the next time they
have their session selected.

---

## smux-style helpers (read / type / keys / id / resolve)

If you've used [smux](https://github.com/ShawnPana/smux), the same surface is
available — backed by the registry instead of pane labels:

```bash
agent-monitor list                            # every registered agent
agent-monitor id                              # who am I (uses $TMUX_PANE)
agent-monitor resolve backend                 # echo canonical agent id
agent-monitor read <recipient> [lines]        # capture-pane content
agent-monitor type <recipient> "text"         # type, no Enter
agent-monitor keys <recipient> Enter Escape   # tmux key names
agent-monitor name <recipient> backend        # set alias
agent-monitor send <recipient> "msg"          # type+Enter, with confirm
```

These talk to the daemon over HTTP — agents written in any language (bash,
Python, …) can shell out and participate.

---

## MCP tools — agents drive each other natively

agent-monitor ships an MCP (stdio JSON-RPC) server that both **Claude Code**
and **Codex** load, exposing six tools. Once `agent-monitor install` has
patched the configs, an agent can call these by name — no `Bash` shell-out.

| Tool                  | What it does                                                      |
| --------------------- | ----------------------------------------------------------------- |
| `who_am_i`            | Returns my agent id, alias, tool, pane, cwd                       |
| `list_agents`         | Lists every other registered agent (alias, tool, cwd)             |
| `read_pane`           | Captures the visible buffer of another agent's pane               |
| `talk_to_agent`       | Types a message into another agent's pane. 3 modes — see below.   |
| `reply_to_talk`       | Recipient-side: send back the answer to a `wait_for_reply` talk   |
| `permission_prompt`   | Routes Claude's tool-use permission prompts to the web UI         |

### `talk_to_agent` — three delivery modes

```
talk_to_agent(recipient, message, confirm=false, wait_for_reply=false, wait_seconds=600)
```

| Mode               | What happens                                                              |
| ------------------ | ------------------------------------------------------------------------- |
| **default**        | Fire-and-forget. Message lands instantly. Returns `delivered`.            |
| **wait_for_reply** | Synchronous request/reply (recommended for "ask another agent a question") — see below |
| **confirm**        | Recipient's web UI shows Allow/Deny banner; sender blocks until click     |

### Recipient-side protocol — what to do when you get REPLY-EXPECTED

When a model in pane B sees a message arrive in its tmux scroll like:

```
[talk from <sender> · REPLY-EXPECTED · talk_id=ab12cd · use reply_to_talk(talk_id="ab12cd", message=...) when done]

<the question>
```

…the model should: (a) actually answer the question by doing whatever work
is asked, and then (b) call the `reply_to_talk` MCP tool with that
exact `talk_id` and its answer. The sender's `talk_to_agent` call is
literally blocked on a Go channel waiting for that one call to fire.

If the model just types its answer normally in its own pane and never
calls `reply_to_talk`, the sender hangs until `wait_seconds` (default
600 s) expires and then sees `status: timeout`.

The MCP tool descriptions for `talk_to_agent` and `reply_to_talk` both
spell this out, so models that read tool docs handle it automatically.
For models that don't (or that haven't picked up the new tool yet),
include a brief instruction in the `message` itself:

> `"Please use reply_to_talk(talk_id=..., message=...) to send your answer back."`

### Request/reply pattern (the right way for Q&A)

The polling-based approach ("send and watch the pane stabilize") is brittle.
Use the explicit reply protocol instead:

```
1. SENDER calls talk_to_agent(
                  recipient="frontend",
                  message="Need the API contract for /users",
                  wait_for_reply=true)

2. agent-monitor types into recipient's pane:

      [talk from backend · REPLY-EXPECTED · talk_id=ab12cd
       · use reply_to_talk(talk_id="ab12cd", message=...) when done]

      Need the API contract for /users

3. RECIPIENT (frontend agent) reads the prefix, does the work,
   then calls:

      reply_to_talk(talk_id="ab12cd", message="GET /users → [{id, name}]")

4. SENDER's talk_to_agent call unblocks, result contains:
      "Reply from frontend (talk_id=ab12cd):

       GET /users → [{id, name}]"
```

No polling, no stability heuristic — the recipient signals "done" themselves.
`wait_seconds` defaults to 600 (10 min). Multiple in-flight talks coexist
fine — each gets its own talk_id.

A typical model-driven loop:

```
1. who_am_i()        → I'm "backend" (claude:abc…)
2. list_agents()     → "frontend" (codex), "infra" (opencode)
3. talk_to_agent(recipient="frontend",
                 message="API contract for /users?",
                 wait_for_reply=true)         ← blocks until they answer
4. (use the returned reply directly in subsequent reasoning)
```

### When the recipient can't run `reply_to_talk`

Some recipients can't call back: a stripped-down agent without MCP, an
older session that hasn't picked up the new tool, or a model that
ignores the protocol prefix. There is no automatic polling fallback —
use `read_pane(recipient=...)` after a fire-and-forget `talk_to_agent`
to capture their output manually.

### What `agent-monitor install` writes

```jsonc
// ~/.claude.json
{
  "mcpServers": {
    "agent-monitor": {
      "command": "/Users/you/agent-monitor/agent-monitor",
      "args":    ["mcp-perm-server"]
    }
  }
}
```

```toml
# ~/.codex/config.toml
[mcp_servers.agent-monitor]
command = "/Users/you/agent-monitor/agent-monitor"
args    = ["mcp-perm-server"]
```

To use the web-UI permission flow with Claude, also pass:

```bash
claude --permission-prompt-tool mcp__agent-monitor__permission_prompt …
```

For Codex / opencode there's no permission-prompt-tool flag, so the bridge's
quick-key buttons (`1 ↵` / `y` / etc.) are the answer path. The
awaiting-permission state is detected from JSONL and the row is highlighted
to point you to the right buttons.

---

## HTTP + WebSocket API

Mostly internal, but useful if you're scripting:

| Method  | Path                                 | Notes                            |
| ------- | ------------------------------------ | -------------------------------- |
| GET     | `/api/sessions`                      | snapshot of every session        |
| GET     | `/api/session/{id}/full`             | full detail (messages, tools)    |
| GET     | `/api/health`                        | liveness                         |
| WS      | `/ws`                                | session/perm/pane/talk events    |
| POST    | `/api/session/send/{id}`             | `{text, enter}` → tmux           |
| POST    | `/api/session/keys/{id}`             | `{keys: ["1","Enter"]}`          |
| POST    | `/api/session/cancel/{id}`           | sends Ctrl-C                     |
| GET     | `/api/session/pane-view/{id}`        | `?lines=200` → captured buffer   |
| GET     | `/api/session/pane/{id}`             | registration info                |
| POST    | `/api/pane/register`                 | hook / wrapper / manual          |
| POST    | `/api/pane/announce`                 | wrapper-pending                  |
| GET     | `/api/panes`                         | every registered pane            |
| GET     | `/api/tmux/panes`                    | every live tmux pane on system   |
| DELETE  | `/api/pane/registration/{id}`        | forget                           |
| POST    | `/api/pane/name/{id}`                | `{alias}` set / clear            |
| POST    | `/api/talk/request`                  | `{fromAgent, toAgent, message}`           |
| POST    | `/api/talk/{id}/respond`             | `{behavior: "allow"\|"deny"}` (confirm flow) |
| GET     | `/api/talk/{id}/await`               | sender long-poll (confirm flow)           |
| POST    | `/api/talk/{id}/reply`               | `{message}` — recipient's reply           |
| GET     | `/api/talk/{id}/await-reply`         | sender long-poll (reply flow)             |
| POST    | `/api/permission/request`            | MCP server uses this             |
| POST    | `/api/permission/{id}/respond`       | UI uses this                     |

---

## Storage layout

Persistent state lives in `~/.agent-monitor/`:

| File          | Purpose                                                  |
| ------------- | -------------------------------------------------------- |
| `panes.json`  | pane registrations (so they survive daemon restart)      |
| `talks.log`   | append-only NDJSON of every talk request + outcome       |

In-memory only:

| Store          | Backed by                                                  |
| -------------- | ----------------------------------------------------------- |
| Sessions       | Tail / scan adapters (see `claude.go`, `codex.go`, …)       |
| Permissions    | MCP server registers; UI responds; channel delivers result  |

**Registry vs. session store — they're decoupled on purpose.** The session
store auto-decays entries (running → idle after a few minutes, idle →
completed after an hour) so the live UI stays clean. The pane registry
doesn't decay — once a registration exists, it stays drivable until the
underlying tmux pane disappears OR the user explicitly forgets it. So you
can `talk_to_agent` an agent whose session has aged out of the live view
as long as its tmux pane is still alive; the bridge / send / keys / talk
endpoints all consult the registry directly, never the session store.

---

## Mobile

The web UI collapses to single-pane navigation on phones (≤ 600 px). The
top bar gets ☰ (slide-in tools rail) and ← back (return to list). Tap any
session to drill into the detail + bridge view.

Tested on iOS Safari. Uses `100dvh` for viewport-height so content doesn't
get clipped behind the URL bar.

---

## Layout

```
agent-monitor/
├── main.go               # HTTP server, subcommand routing
├── store.go              # in-memory session store + WS broadcasts
├── types.go              # Tool / State / Session / ServerEvent
├── claude.go             # Claude hook ingest + JSONL tail
├── claude_detail.go      # Per-session detail builder for Claude
├── codex.go              # Codex JSONL adapter
├── codex_detail.go
├── cursor.go             # Cursor IDE chat scanner
├── cursor_detail.go
├── cursoragent.go        # cursor-agent process scanner
├── opencode.go           # opencode JSONL adapter
├── opencode_detail.go
├── tmux.go               # listTmuxPanes, send-keys helpers, capture-pane
├── pane_registry.go      # pane registration + aliases + persistence
├── talk.go               # talk store + delivery formatting
├── perm.go               # permission request store + long-poll
├── mcp_perm.go           # MCP stdio server (Claude permission prompt)
├── install_hooks.go      # patches ~/.claude/settings.json
├── tail.go               # tiny line-by-line file tailer
├── detail.go             # session-detail dispatcher
├── util.go               # path helpers
├── wrapper.go            # `agent-monitor run / send / list / read / type / keys / id / name / resolve`
├── bin/
│   └── claude-hook.sh    # SessionStart hook script
└── web/
    └── index.html        # single-page web app (vanilla JS, no build step)
```

---

## Troubleshooting

### "Transport closed" inside Claude / Codex when calling MCP tools

Something killed the `agent-monitor mcp-perm-server` child process the
agent CLI spawned at startup. That child is what hosts the MCP tools;
once it dies the agent CLI doesn't reconnect.

Most common cause: someone ran `pkill -f /agent-monitor/agent-monitor`
to restart the daemon — that pattern matches the MCP child too. Use
`agent-monitor stop` (kills only the listen-port owner), or restart
the agent CLI itself if the MCP child has already died.

### `list_agents` (Claude only) returns "expected: record received: array"

You're on an MCP server build before commit `db93449`. Restart the
Claude CLI inside tmux (`exit` then `agent-monitor run claude`) to
pick up the schema-fixed binary. Codex was lenient and accepted the
old shape, which is why the bug was Claude-only.

### `talk_to_agent` returns 404 "session not found"

The pane is registered but the underlying session has aged out of the
live store (~60 min idle). Pre-`1f72fa4` builds gated the send
endpoints on the session store; new builds consult the registry only.
Restart the daemon to load the fix:

```
agent-monitor stop
AGENT_MONITOR_BIND=0.0.0.0 nohup ./agent-monitor >> agent-monitor.log 2>&1 &
```

### Sender's `talk_to_agent(wait_for_reply=true)` hangs forever

The recipient never called `reply_to_talk`. Three possible reasons:

1. The recipient agent's MCP server pre-dates `reply_to_talk` (commit
   `d0b86d0`). Restart the recipient's CLI inside tmux.
2. The recipient model didn't recognize the REPLY-EXPECTED prefix and
   just answered in its own pane. Add an instruction to the `message`
   itself: `"Use reply_to_talk(talk_id=..., message=...) to send back."`
3. The recipient is genuinely still working. Bump `wait_seconds`.

For recipients that simply can't run `reply_to_talk`, fall back to a
fire-and-forget `talk_to_agent` followed by `read_pane` to capture
their output manually.

### Bridge in the web UI shows no pane after the agent was already running

The agent was launched before the SessionStart hook deployed. Use the
empty-state picker in the bridge — it lists every live tmux pane with
cwd-matches at the top. One click registers; if there's exactly one
cwd-exact match, it auto-registers. `forget` cleans up a wrong pick.

### Daemon won't start: "bind: address already in use"

Another daemon is holding the port. `pkill` may not have killed it
(slow signal, or you matched the wrong process). Use:

```bash
lsof -ti :7777 -sTCP:LISTEN | xargs kill
```

…or just `agent-monitor stop`.

---

## Design notes

For the *why* behind these decisions — registered-not-inferred panes,
the two-channel Talk struct, the separate-process MCP server, the
failure-mode catalogue — see [DESIGN.md](DESIGN.md). It's the
companion document to this one: README answers "what / how to use,"
DESIGN answers "why is it shaped this way."

---

## License

MIT.
