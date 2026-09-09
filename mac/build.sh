#!/usr/bin/env bash
# Build AgentMonitor.app — the full agent-monitor dashboard as a Mac app.
#
#   ./mac/build.sh          compile + assemble AgentMonitor.app in the repo root
#   ./mac/build.sh --run    also launch it
#
# The bundle carries the agent-monitor daemon too, so the app starts its own
# server and nothing has to be run from a terminal. Requires go and swiftc.
#
# Summon/dismiss it from any app with ⌥⌘A. Change that with:
#   AGENT_MONITOR_HOTKEY="ctrl+opt+cmd+a" open AgentMonitor.app
#
# Point it elsewhere (e.g. a Tailscale host) with:
#   AGENT_MONITOR_APP_URL=http://100.x.y.z:7777/ open AgentMonitor.app
set -euo pipefail
cd "$(dirname "$0")/.."

APP="AgentMonitor.app"
BIN="$APP/Contents/MacOS/AgentMonitor"
# The daemon ships beside the app executable. Contents/MacOS is the right place
# for a nested Mach-O: Bundle.url(forAuxiliaryExecutable:) finds it there, and
# codesign treats it as part of the bundle.
DAEMON="$APP/Contents/MacOS/agent-monitor"

for tool in swiftc go; do
  command -v "$tool" >/dev/null 2>&1 || { echo "✗ $tool not found — needed to build the app" >&2; exit 1; }
done

echo "→ compiling app shell…"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
swiftc -O -o "$BIN" mac/main.swift mac/hotkey.swift mac/daemon.swift \
  -framework Cocoa -framework WebKit -framework Carbon

# The daemon embeds web/ via go:embed, so this also refreshes the dashboard
# the app serves.
echo "→ compiling daemon…"
go build -o "$DAEMON" .

# Reuse the AgentTV pulse glyph rather than carrying a second 1.6 MB icon.
cp tv/AppIcon.icns "$APP/Contents/Resources/AppIcon.icns"

# Info.plist — no LSUIElement, so the app gets a Dock icon and a ⌘Tab entry and
# stays reachable even without the hotkey.
cat > "$APP/Contents/Info.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>Agent Monitor</string>
  <key>CFBundleDisplayName</key><string>Agent Monitor</string>
  <key>CFBundleIdentifier</key><string>dev.agentmonitor.app</string>
  <key>CFBundleExecutable</key><string>AgentMonitor</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>0.1</string>
  <key>CFBundleVersion</key><string>1</string>
  <key>LSMinimumSystemVersion</key><string>12.0</string>
  <key>CFBundleIconFile</key><string>AppIcon</string>
  <key>NSHighResolutionCapable</key><true/>
  <!-- Talk to the local daemon over plain HTTP. -->
  <key>NSAppTransportSecurity</key>
  <dict><key>NSAllowsLocalNetworking</key><true/></dict>
</dict>
</plist>
PLIST

# Ad-hoc sign so Gatekeeper/WebKit are happy running it locally. The nested
# daemon is signed first — signing the bundle does not reach inside for it, and
# an unsigned nested binary breaks the outer signature.
codesign --force --sign - "$DAEMON" >/dev/null 2>&1 || true
codesign --force --sign - "$APP" >/dev/null 2>&1 || true

echo "✓ built $APP"
if [[ "${1:-}" == "--run" ]]; then
  echo "→ launching…"; open "$APP"
fi
