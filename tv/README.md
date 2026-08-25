# AgentTV — always-on-top glance widget

A tiny "peek-a-boo" desktop window that floats above every app and Space,
showing your **live** agent sessions at a glance: what each one is doing right
now, a recent-activity sparkline, and a red pulse when one needs you
(awaiting permission).

It's a thin native shell (`main.swift`, AppKit + WKWebView) around the daemon's
`/tv` page — all the UI lives in `web/tv.html`, so tweaking the look only needs
a daemon rebuild, not a recompile.

## Build & run

```bash
./tv/build.sh --run       # compile → AgentTV.app → launch
# or
./tv/build.sh && open AgentTV.app
```

The daemon must be running (`./agent-monitor`) — the widget retries until it's up.

- **Dockless**: no Dock icon or app-switcher entry (`LSUIElement`).
- **Always on top / all Spaces**: floats over fullscreen apps too.
- **Drag** it anywhere by its background; resize from any edge.
- **Click a tile** → opens that session's full trace view in your browser.
- **⌘R** reload · **⌘Q** quit (menu is hidden but shortcuts work).

## Point it at another host (e.g. over Tailscale)

```bash
AGENT_TV_URL=http://100.68.125.93:7777/tv open AgentTV.app
```

## Notes

- No LaunchAgent / login item by design — run it by hand (matches the daemon).
- Just the browser? `open http://localhost:7777/tv` works too, but a normal
  browser window can't stay on top — that's the whole reason for the native shell.
