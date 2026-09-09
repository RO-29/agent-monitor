# AgentMonitor.app — the dashboard as a Mac app

The full agent-monitor dashboard in a real macOS window, so you never have to
hunt for a browser tab again. Press **⌥⌘A** from any app to bring it forward.
Press it again to put it away.

It is a thin native shell (`main.swift`, AppKit + WKWebView) around the daemon's
`/` page — all the UI lives in `web/index.html`, so changing the dashboard only
needs a daemon rebuild, not a recompile.

## Build & run

```bash
./mac/build.sh --run       # compile → AgentMonitor.app → launch
# or
./mac/build.sh && open AgentMonitor.app
```

No terminal needed after the build: the bundle carries the agent-monitor daemon
and starts it for you.

- **Standard window**: resizable, minimizable, and its size and position are
  remembered across launches.
- **Global shortcut**: ⌥⌘A shows it from anywhere, including over fullscreen
  apps. Press again to hide.
- **Dock + ⌘Tab**: it is a normal app, so it is reachable even without the
  shortcut.
- **Menu-bar icon**: left-click toggles, right-click opens a menu (Show/Hide,
  Reload, Quit).
- **⌘R** reload · **⌘W** hide · **⌘Q** quit.
- Links pointing outside the daemon open in your default browser.

No Accessibility or Input Monitoring permission is needed. The shortcut uses
Carbon's `RegisterEventHotKey`, which does not require one, so macOS never shows
a privacy prompt.

## The bundled daemon

The dashboard is served by the agent-monitor Go daemon, so `mac/build.sh` builds
that binary into `AgentMonitor.app/Contents/MacOS/agent-monitor` and the app
manages it.

On launch the app checks `http://127.0.0.1:7777/api/health`:

- **Something is already serving** — the app adopts it and never touches it,
  including on quit. A daemon you started by hand usually has its own flags, and
  it owns the MCP permission servers the agent CLIs talk to; stopping it would
  close those transports and the CLIs do not reconnect.
- **Nothing is serving** — the app starts the bundled daemon, and stops it with
  SIGTERM on quit so the session history is flushed to SQLite. If it exits
  unexpectedly the app retries three times, then stops and says where to look.

SIGTERM and SIGINT are routed through the normal quit path, so `kill`, a
`pkill`, and logout all shut the daemon down cleanly rather than orphaning it.

Menu items (app menu and the menu-bar icon):

- **Restart Daemon** (⇧⌘R) — restarts a daemon the app owns. On an adopted one
  it only re-checks, since stopping someone else's daemon is not the app's call.
- **Open Daemon Log** — `~/Library/Logs/AgentMonitor/daemon.log`, truncated when
  it passes 5 MB.
- **Install Agent Hooks…** — runs `agent-monitor install`, which adds hooks and
  MCP entries to `~/.claude/settings.json`, `~/.claude.json`, and
  `~/.codex/config.toml`. It asks first and shows the output. This is the one
  remaining setup step, and it is not run automatically: writing to another
  tool's config file is not something an app should do unasked. It is idempotent
  and backs up what it edits.

### Reaching it from other devices

The app starts the daemon on **loopback only**, and drops `AGENT_MONITOR_BIND`
however it reached the environment — a value exported for a shell session must
not make the app publish a pane-driving API to the network.

To expose it anyway, use the separately named `AGENT_MONITOR_APP_BIND`:

```bash
launchctl setenv AGENT_MONITOR_APP_BIND 0.0.0.0   # then relaunch the app
```

Anyone who can reach that address controls every registered tmux pane, so
prefer a specific Tailscale IP over `0.0.0.0`.

Setting environment variables for a GUI app is awkward, so if you need LAN or
Tailscale reach the simpler path is to keep starting the daemon yourself —

```bash
AGENT_MONITOR_BIND=0.0.0.0 nohup ./agent-monitor >> agent-monitor.log 2>&1 &
```

— and let the app adopt it. That is also what the iPhone client needs.

## Change the shortcut

```bash
AGENT_MONITOR_HOTKEY="ctrl+opt+cmd+a" open AgentMonitor.app
```

Accepted modifiers: `cmd`, `opt` (or `alt`), `ctrl`, `shift`. At least one
modifier is required. The key can be a letter, a digit, or `space`, `return`,
`tab`, `escape`.

If the combination is unparseable the app still starts and says so on stderr —
use the menu-bar icon instead.

Conflicts are not detected. macOS lets two apps register the same combination
and then picks who receives it, so if the shortcut does nothing, something else
is taking it: set `AGENT_MONITOR_HOTKEY` to a different combination. The default
⌥⌘A was picked for being unclaimed by macOS itself.

## Point it at another host (e.g. over Tailscale)

```bash
AGENT_MONITOR_APP_URL=http://100.68.125.93:7777/ open AgentMonitor.app
```

## AgentTV vs AgentMonitor

Two apps, two jobs. Run both at once if you like.

| | `AgentTV.app` (`tv/`) | `AgentMonitor.app` (`mac/`) |
|---|---|---|
| Page | `/tv` glance board | `/` full dashboard |
| Window | small, borderless, always on top, all Spaces | standard, resizable, remembered |
| For | watching out of the corner of your eye | actually working in |
| Toggle | menu-bar click | ⌥⌘A, menu-bar click, Dock, ⌘Tab |

## Notes

- No LaunchAgent / login item — the app starts the daemon as its own child
  process while it runs, and stops it on quit. Nothing is installed to launch at
  login; opening the app is still a deliberate act.
- Desktop notifications do not fire yet. WKWebView does not implement the Web
  Notification API that `web/index.html` uses, so the dashboard degrades to
  silence inside this shell. Sound alerts still work. Use a browser tab if you
  need system notifications today.
- Just the browser? `open http://localhost:7777` works too — this shell exists
  so the dashboard has its own window, its own icon, and one keystroke.
