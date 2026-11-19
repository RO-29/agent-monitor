#!/bin/bash
# Builds AgentMonitor for iOS and publishes it for an install over the internet (Tailscale HTTPS), for when the
# iPhone is not on the same Wi-Fi or USB. Open the printed link on the iPhone in Safari, then tap Install.
# Needs once: `tailscale serve --bg 7777` (HTTPS on the Mac's ts.net name, forwarded to the daemon).
# Works with a paid Apple team only: iOS 27 downloads a free-team build from the link but does not install it.
set -euo pipefail

# Machine-specific settings (AM_DEFAULT_SERVER, AM_IOS_TEAM) live outside the repository.
[[ -f "$HOME/.config/agent-monitor/build.env" ]] && source "$HOME/.config/agent-monitor/build.env"
REPO="${AM_REPO:-$HOME/agent-monitor}"
TEAM="${AM_IOS_TEAM:-QV5FFU8R79}"
STATE="$HOME/.agent-monitor/ios-resign"
OTA="$HOME/.agent-monitor/ios-ota"
export PATH="/usr/bin:/bin:/usr/sbin:/sbin:/usr/local/bin:/opt/homebrew/bin:$PATH" # /usr/local/bin: the tailscale CLI
mkdir -p "$STATE" "$OTA"
chmod 700 "$OTA"
log() { echo "$(date '+%F %T') $*"; }

HOST=$(tailscale status --json | /usr/bin/python3 -c 'import json,sys; print(json.load(sys.stdin)["Self"]["DNSName"].rstrip("."))')
[[ -n "$HOST" ]] || { log "no Tailscale name"; exit 1; }
# A long random path segment is the only check: iOS fetches these files with no password.
[[ -s "$OTA/token" ]] || { openssl rand -hex 24 > "$OTA/token"; chmod 600 "$OTA/token"; }
TOKEN=$(cat "$OTA/token")
BASE="https://$HOST/ios-ota/$TOKEN"

# The free team's profile lists the registered iPhones, so a generic build installs on them.
log "build $(git -C "$REPO" rev-parse --short HEAD) for team $TEAM"
xcodebuild -project "$REPO/ios/AgentMonitor.xcodeproj" -scheme AgentMonitor -configuration Release \
  -destination 'generic/platform=iOS' -derivedDataPath "$STATE/build" -allowProvisioningUpdates \
  DEVELOPMENT_TEAM="$TEAM" AM_DEFAULT_SERVER="${AM_DEFAULT_SERVER:-}" CODE_SIGN_STYLE=Automatic CURRENT_PROJECT_VERSION="$(git -C "$REPO" rev-list --count HEAD)" MARKETING_VERSION="1.0.$(git -C "$REPO" rev-list --count HEAD)" build -quiet
APP="$STATE/build/Build/Products/Release-iphoneos/AgentMonitor.app"
[[ -d "$APP" ]] || { log "no app at $APP"; exit 1; }

PKG=$(mktemp -d)
mkdir "$PKG/Payload" && cp -R "$APP" "$PKG/Payload/"
(cd "$PKG" && zip -qry "$OTA/AgentMonitor.ipa.tmp" Payload) && mv "$OTA/AgentMonitor.ipa.tmp" "$OTA/AgentMonitor.ipa"
rm -rf "$PKG"

ID=$(/usr/bin/plutil -extract CFBundleIdentifier raw -o - "$APP/Info.plist")
VER=$(/usr/bin/plutil -extract CFBundleVersion raw -o - "$APP/Info.plist")
cat > "$OTA/manifest.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>items</key><array><dict>
  <key>assets</key><array><dict><key>kind</key><string>software-package</string><key>url</key><string>$BASE/AgentMonitor.ipa</string></dict></array>
  <key>metadata</key><dict><key>bundle-identifier</key><string>$ID</string><key>bundle-version</key><string>$VER</string>
  <key>kind</key><string>software</string><key>title</key><string>Agent Monitor</string></dict>
</dict></array></dict></plist>
PLIST
MANIFEST=$(/usr/bin/python3 -c 'import sys,urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "$BASE/manifest.plist")
cat > "$OTA/index.html" <<HTML
<!doctype html><meta name="viewport" content="width=device-width,initial-scale=1"><title>Install Agent Monitor</title>
<body style="font:17px -apple-system;padding:32px;max-width:28em">
<h2>Agent Monitor $(git -C "$REPO" rev-parse --short HEAD)</h2>
<p><a href="itms-services://?action=download-manifest&amp;url=$MANIFEST" style="font-size:20px">Install</a></p>
<p>Tap Install, then confirm. The app keeps your settings.</p></body>
HTML

log "done; profile expires $(security cms -D -i "$APP/embedded.mobileprovision" 2>/dev/null | /usr/bin/plutil -extract ExpirationDate raw -o - - 2>/dev/null || echo unknown)"
echo "Open on the iPhone (Tailscale on): $BASE/"
