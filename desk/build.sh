#!/usr/bin/env bash
# Build "agent-monitor.app" (the Workspace in its own Mac window). Run it yourself; nothing starts at login.
#   ./desk/build.sh                   compile + assemble agent-monitor.app in the repo root
#   AGENT_MONITOR_URL=http://100.x.y.z:7777 AGENT_MONITOR_TOKEN=amd_... open agent-monitor.app   for a remote daemon
set -euo pipefail
cd "$(dirname "$0")/.."
APP="agent-monitor.app"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
swiftc -O -o "$APP/Contents/MacOS/AgentMonitor" desk/*.swift -framework Cocoa -framework WebKit -framework UserNotifications -framework Carbon
[ -f tv/AppIcon.icns ] && cp tv/AppIcon.icns "$APP/Contents/Resources/AppIcon.icns"
cat > "$APP/Contents/Info.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>agent-monitor</string>
  <key>CFBundleDisplayName</key><string>agent-monitor</string>
  <key>CFBundleIdentifier</key><string>dev.agentmonitor.desk</string>
  <key>CFBundleExecutable</key><string>AgentMonitor</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>0.2</string>
  <key>CFBundleVersion</key><string>1</string>
  <key>LSMinimumSystemVersion</key><string>12.0</string>
  <key>NSHighResolutionCapable</key><true/>
  <key>CFBundleIconFile</key><string>AppIcon</string>
  <key>NSMicrophoneUsageDescription</key><string>The mic button in agent-monitor turns your speech into a prompt for an agent.</string>
  <key>NSSpeechRecognitionUsageDescription</key><string>The mic button in agent-monitor turns your speech into a prompt for an agent.</string>
  <key>NSAppTransportSecurity</key>
  <dict><key>NSAllowsLocalNetworking</key><true/><key>NSAllowsArbitraryLoads</key><true/></dict>
</dict>
</plist>
PLIST
codesign --force --sign - "$APP" >/dev/null 2>&1 || true
echo "built $APP"

# Launchpad shows ~/Applications: refresh that copy when it exists (AM_APP_COPY=0 skips it).
DEST="${AM_APP_DEST:-$HOME/Applications/$APP}"
if [[ "${AM_APP_COPY:-1}" == "1" && -d "$DEST" ]]; then
  rm -rf "$DEST" && ditto "$APP" "$DEST" && echo "copied to $DEST (restart the app to use it)"
fi
