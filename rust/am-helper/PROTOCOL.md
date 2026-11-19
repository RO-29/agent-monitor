# am-helper protocol

am-helper is the only implementation of the helper modes (`run`, `__pty-leader`, `mcp-perm-server`,
`hook claude|codex`). The Go binary keeps the protocol types (`host_proto.go`) and execs am-helper for
these modes. Go test fixtures in `*_sim_test.go` play the other side in tests.

## Switch

- `am-helper version` prints `am-helper 0.3.0 (rust)` and `protocol 1`. `helperFor` runs it once per
  path, size and mtime (2 s limit) and uses the helper when the protocol equals `rustHelperProtocol`.
  A probe that times out counts as a match and is not cached. Bump both numbers together when a
  mode's contract changes.
- `agent-monitor mcp-perm-server|run|__pty-leader ...` execs `am-helper` from the directory of the Go
  binary with the same arguments (`helper_exec.go`). With no matching helper it prints the build
  command and exits 1.
- Direct launchers call `helperFor(bin)`: the daemon's terminal start (`exec <helper> run`) and the
  orchestrator MCP server command. `bin/shell-functions.zsh` runs `${AGENT_MONITOR_BIN:h}/am-helper`,
  and runs the agent directly with a warning when the helper is missing.
- MCP configs written by `agent-monitor install` name the Go binary, which execs the helper.
- `bin/claude-hook.sh` and `bin/codex-hook.sh` exec `<script dir>/../am-helper hook claude|codex` when
  it is an executable file, `AGENT_MONITOR_RUST_HELPERS` is not `0` and `AGENT_MONITOR_URL` is empty or
  starts with `http://`; otherwise the shell code below the shim runs. `~/.claude/settings.json`
  and `~/.codex/hooks.json` keep naming the scripts.

## Environment

| Variable | Used by | Meaning |
|---|---|---|
| `AGENT_MONITOR_PORT` | all | daemon port, default 7777, base URL `http://127.0.0.1:<port>` |
| `AM_DAEMON_URL` | mcp | full daemon URL, trailing `/` removed; wins over the port |
| `AM_ORCH_THREAD`, `AM_ORCH_DEPTH` | mcp | driven thread id and depth; the thread enables the delegate tools |
| `TMUX_PANE`, `TMUX` | mcp, run | caller's pane; `run` announces it |
| `AGENT_MONITOR_HOME` | run | state dir, default `~/.agent-monitor`; sockets in `<home>/hosts/` |
| `AM_HOST_ID_ASSIGN` | run | host id if it matches `^[A-Za-z0-9_-]{4,64}$`, else 16 random hex |
| `AM_HOST_ID` | run | set for the agent to the host id; both markers are removed from the inherited env |

At start every mode sets `LC_CTYPE=UTF-8` when no locale is set and appends `/opt/homebrew/bin` and
`/usr/local/bin` to `PATH` when they exist.

## run

- No arguments: usage on stderr, exit 2. With `TMUX_PANE`: `POST /api/pane/announce`
  `{"cwd","paneId","pid","tmuxSocket","tool"}`, 1 s timeout, errors ignored.
- stdin or stdout not a terminal: exec the agent (argv[0] as given) without the host markers;
  not found exits 127, exec failure exits 1.
- Terminal: look up the agent (127 if missing), open a pty at the terminal's size (80x24 if unknown),
  start `<self> __pty-leader -- <agent path> <args>` as session leader of the pty with fd 3 as a pipe,
  read the agent pid from it (2 s limit), put the terminal in raw mode, relay both ways, keep the last
  256 KiB of output.
- Control socket `<home>/hosts/<id>.sock`, dir 0700, socket 0600. One JSON object per line in, one per
  line out, 30 s deadline per request, 1 MiB line limit, trailing `\r` ignored.
  Request `{"op","data","keys","lines"}` (keys match without case). Response, `ok` always and the rest
  only when not empty, in this order: `ok, error, text, cols, rows, hostId, agent, agentPid, outBytes, idleMs`.
  - `ping`: hostId, agent (base name of argv[0]), agentPid, outBytes (total), idleMs (since last output)
  - `input`: writes `data` to the pty
  - `keys`: tmux key names (Enter, Escape, Tab, BTab, Up, Down, Left, Right, BSpace, Space, Home, End,
    PageUp/PPage, PageDown/NPage, DC/Delete, C-a..C-z, one character), 25 ms apart; an unknown name
    fails the list with `unknown key "<k>"`, an empty list with `keys required`
  - `screen`: rendered text of the buffer (port of `host_screen.go`), last `lines` lines (default 50,
    max 2000), with cols and rows
  - `size`: pty cols and rows (80x24 when unknown)
  - other: `unknown op <op>`; bad JSON or wrong field types: `bad json`; after the agent exits:
    `agent exited`
- Registration: `POST /api/hosts` `{"hostId","pid","agentPid","agent","cwd","tty"?,"tmuxPane"?,"socket"}`,
  1 s timeout; retried after 0.5, 1, 2, 4, 8 s on error, then every 10 s. At exit
  `DELETE /api/hosts/<id>` with a 300 ms timeout.
- Signals: SIGWINCH copies the terminal size to the pty. SIGINT, SIGTERM, SIGHUP go to the agent's process
  group; after SIGHUP the agent is killed if it lives 10 s more.
- Job control: when the leader stops and a shell can resume the host (session id differs from the
  process group), the host restores the terminal, stops itself with SIGTSTP, and on resume sets raw
  mode and the size again. Then it sends SIGCONT to the leader.
- Exit: the agent's status, or 128+signal. Output drains for at most 500 ms; the socket file is
  removed and the terminal restored.

## __pty-leader

`-- <cmd> [args]`; no command prints `agent-monitor __pty-leader: agent required` and exits 2. Ignores
SIGTTOU, starts the command in its own process group as the terminal's foreground group, writes
`<pid>\n` to fd 3 when open, forwards SIGINT/SIGTERM/SIGHUP to the group. When the agent stops, the
leader stops itself with SIGSTOP; on SIGCONT it gives the terminal back to the agent and continues it.
Start failure exits 126; otherwise the agent's status or 128+signal.

## mcp-perm-server

Newline-delimited JSON-RPC 2.0 on stdin and stdout, handled one at a time; logs on stderr with the
prefix `[mcp-perm]`. Output is encoded as Go's `encoding/json` does it (sorted map keys, struct field
order, `<>&` and U+2028/2029 escaped, Go number format), so the bytes match the Go server.

- `initialize` returns protocol `2025-06-18`, server `agent-monitor 0.3.0`, `tools` capability.
- `notifications/*`, a missing method, bad JSON: no answer. `ping`: `{}`. Other methods: error -32601.
- `tools/list`: permission_prompt, who_am_i, list_agents, register_self, read_pane, talk_to_agent,
  reply_to_talk, search_sessions, session_context; with `AM_ORCH_THREAD` also wait_tasks, task_status and,
  below depth 2, delegate_task. An unknown tool name gives error -32602.
- `permission_prompt`: `POST /api/permission/request` `{"cwd","input","toolName","toolUseId"}`, then
  `GET /api/permission/wait/<id>?timeout=600` (11 min limit). The result is one text block holding
  `{"behavior","message"?,"updatedInput"?}`; an allow without updatedInput echoes the input. Any failure denies.
- Agent tools call `/api/panes`, `/api/panes/sweep`, `/api/tmux/panes`, `/api/pane/register`,
  `/api/session/pane-view/<id>`, `/api/session/send/<id>`, `/api/talk/request`, `/api/talk/<id>/await`,
  `/api/talk/<id>/await-reply`, `/api/talk/<id>/reply`. Search tools call `/api/search`,
  `/api/v3/threads`, `/api/session/<id>/items` (paged with `before`) and `/api/orch/threads/<id>`.
  Delegate tools call `/api/orch/threads/<thread>/delegate` and `/tasks`.
- Text results are one text block plus `structuredContent` where Go sends it.

## hook claude

Port of `bin/claude-hook.sh`. Reads the payload from stdin as `$(cat)` does (NUL bytes dropped,
trailing newlines removed). Prints nothing and always exits 0. HTTP limits: 0.5 s connect, 5 s total
per request (the script set none).

- Base URL: `AGENT_MONITOR_URL`, else `http://127.0.0.1:${AGENT_MONITOR_PORT:-7777}`.
- `POST <base>/events/claude` with the payload, `Content-Type: application/json`. When `AM_HOST_ID` is
  non-empty and only `[A-Za-z0-9_-]`, the query is `?host=<id>&ppid=<ppid>`.
- `hook_event_name`, `session_id` and `cwd` are read as the script's
  `grep -o '"k"[[:space:]]*:[[:space:]]*"[^"]*"' | head -1 | sed` does, quirks included (first match
  on any line, no JSON unescaping). A missing `hook_event_name` or `session_id`, or a missing `cwd` on
  SessionStart in tmux, ends the hook there; the script exits 1 at that point (set -e), am-helper exits 0.
- SessionStart with `TMUX_PANE` and a session id: `POST <base>/api/pane/register`
  `{"tool":"claude","sessionId":"<sid>","paneId":"<TMUX_PANE>","tmuxSocket":"<TMUX>","cwd":"<cwd>","source":"hook"}`,
  values inserted raw as the script's printf does.
- SessionEnd with a session id: `DELETE <base>/api/pane/registration/claude:<sid>`.
- URLs are sent as curl sends them: fragment cut, dot segments removed, bytes >= 0x80 as lowercase
  `%xx`. A URL curl refuses (space, control bytes) or would glob (`{}[]`) is not sent.

## hook codex

Port of `bin/codex-hook.sh`, which used `/usr/bin/python3` for JSON: `pyjson.rs` follows Python's
`json.loads`, `json.dumps(separators=(",", ":"))` (ASCII escapes, key order, float repr) and `str()`.
Always exits 0.

- `hook_event_name` and `session_id` as `print(json.load(stdin).get(k, ""))` gives them; "" when the
  payload is not a JSON object.
- `POST <base>/events/codex[?host=&ppid=]` with the payload, 0.2 s connect, 0.5 s total. For
  PermissionRequest the body is the script's Notification rewrite:
  `{"session_id","transcript_path","cwd","hook_event_name":"Notification","notification_type":"permission_prompt","message","tool_name"}`,
  message `Permission required: <command|description|json(tool_input)>` (whitespace squashed, 200
  code points with `...`) or `Permission required for <tool_name|tool>`.
- SessionStart, UserPromptSubmit, Stop, PermissionRequest with a session id: the pane is the stdout of
  `CODEX_THREAD_ID=<sid> /bin/sh /Users/rohit/bin/tmux-pane-id.sh <ppid>` on success, else the one pane
  of `tmux list-panes -a -F '#{pane_id}'` whose `capture-pane -p -S -100` contains the session id
  (none or several: no pane). With a pane: `POST <base>/api/pane/register`
  `{"tool":"codex","sessionId","paneId","tmuxSocket","cwd","source":"hook"}`.
- SessionEnd with a session id: `DELETE <base>/api/pane/registration/codex:<sid>`.
- PermissionRequest: hold = `int(holdSeconds or 120)` from
  `${AGENT_MONITOR_HOME:-$HOME/.agent-monitor}/approval-settings.json`, 120 when that fails or is not a
  non-negative integer. `POST <base>/api/approvals/hook/codex` with the original payload, 0.5 s connect,
  hold+5 s total. Only a complete HTTP 200 body that is
  `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","decision":{"behavior":"allow"|"deny"}}}`
  (other keys kept) is printed, re-encoded as Python prints it, plus a newline. Timeout, refused
  connection, other status or shape: nothing, so Codex falls back to its own prompt.

## ppid

Both agents run a hook command through a shell (`sh -c '<command>'` for Claude Code's shell-form
hooks, the same for Codex). For a single command `/bin/sh`, `bash` and `zsh` exec it, so the hook's
`$PPID` is the agent process itself. The shim then execs am-helper, which keeps the pid, so
`getppid()` in am-helper equals the script's `$PPID`. The daemon (`LinkFromHook` in
`host_registry.go`) accepts the link when the host's agent pid is within four parents of that pid, so
either the agent or an intermediate shell works. `hook_rust_test.go` runs the hooks through
`/bin/sh -c` and checks that `ppid=` is the runner's pid on both sides.
