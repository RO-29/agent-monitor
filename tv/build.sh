#!/usr/bin/env bash
# Build AgentTV.app — the always-on-top glance widget for agent-monitor.
#
#   ./tv/build.sh          compile + assemble AgentTV.app in the repo root
#   ./tv/build.sh --run    also launch it
#
# Point it elsewhere (e.g. a Tailscale host) with:
#   AGENT_TV_URL=http://100.x.y.z:7777/tv open AgentTV.app
set -euo pipefail
cd "$(dirname "$0")/.."

APP="AgentTV.app"
BIN="$APP/Contents/MacOS/AgentTV"

echo "→ compiling…"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
swiftc -O -o "$BIN" tv/main.swift -framework Cocoa -framework WebKit

# Branded Dock/app icon (the pulse glyph).
cp tv/AppIcon.icns "$APP/Contents/Resources/AppIcon.icns"

# Info.plist — shows a Dock icon + ⌘Tab entry (no LSUIElement) so the widget is
# always launchable, even when the menu-bar status item hides behind the notch.
cat > "$APP/Contents/Info.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>AgentTV</string>
  <key>CFBundleDisplayName</key><string>AgentTV</string>
  <key>CFBundleIdentifier</key><string>dev.agentmonitor.tv</string>
  <key>CFBundleExecutable</key><string>AgentTV</string>
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

# Ad-hoc sign so Gatekeeper/WebKit are happy running it locally.
codesign --force --sign - "$APP" >/dev/null 2>&1 || true

echo "✓ built $APP"
if [[ "${1:-}" == "--run" ]]; then
  echo "→ launching…"; open "$APP"
fi
