#!/bin/bash
# Builds AgentMonitor for iOS, signs it with the Xcode account's team, and installs it on the paired iPhone.
# Usage: ios/resign.sh [--force]. Skips when this build was installed under 20 h ago, so it is safe to run often.
# It renews the signing profile when it ends within 3 days, or when the iPhone asked (ios_renew.go).
# With no reachable iPhone and AM_IOS_OTA=1 (a paid team) it publishes the build over Tailscale and pushes the link.
set -euo pipefail

# Machine-specific settings (AM_DEFAULT_SERVER, AM_IOS_TEAM) live outside the repository.
[[ -f "$HOME/.config/agent-monitor/build.env" ]] && source "$HOME/.config/agent-monitor/build.env"
REPO="${AM_REPO:-$HOME/agent-monitor}"
# Commit count plus 500: the repository restarted on 2026-11-19, and build numbers must keep rising.
BUILD=$(( $(git -C "$REPO" rev-list --count HEAD) + 500 ))
TEAM="${AM_IOS_TEAM:-QV5FFU8R79}"
DEVICE="${AM_IOS_DEVICE:-}"
STATE="$HOME/.agent-monitor/ios-resign"
OUT="$STATE/build"
STAMP="$STATE/last-install"
MIN_AGE=$((20 * 3600))
export PATH="/usr/bin:/bin:/usr/sbin:/sbin:/usr/local/bin:/opt/homebrew/bin:$PATH" # /usr/local/bin: the tailscale CLI
mkdir -p "$STATE"
log() { echo "$(date '+%F %T') $*"; }

HEAD=$(git -C "$REPO" rev-parse --short HEAD)

# Free-team profiles last 7 days, and Xcode reuses its cached one, so a new install alone never moves the end.
PROFILES="$HOME/Library/Developer/Xcode/UserData/Provisioning Profiles"
OLD="$STATE/old-profiles"
RENEW="$STATE/renew" # written by the daemon when the iPhone asks (POST /api/ios/renew): its profile end, Unix ms
RENEW_WITHIN=$((3 * 86400))
profile_end() { security cms -D -i "$1" 2>/dev/null | plutil -extract ExpirationDate raw -o - - 2>/dev/null; }
epoch() { date -j -u -f '%Y-%m-%dT%H:%M:%SZ' "$1" +%s 2>/dev/null || echo 0; }
# Cached profiles for this app that end within RENEW_WITHIN.
stale_profiles() {
  local f end
  for f in "$PROFILES"/*.mobileprovision; do
    [[ -f "$f" ]] || continue
    security cms -D -i "$f" 2>/dev/null | plutil -extract Entitlements.application-identifier raw -o - - 2>/dev/null |
      grep -q "\.com\.rohit\.agentmonitor$" || continue
    end=$(epoch "$(profile_end "$f")")
    (( end - $(date +%s) < RENEW_WITHIN )) && echo "$f"
  done
  return 0
}
STALE=$(stale_profiles)
RENEWING=0
[[ -n "$STALE" || -f "$RENEW" ]] && RENEWING=1

# Skip a recent install of this same build; a new commit or a profile renewal installs at once.
if [[ "${1:-}" != "--force" && "$RENEWING" == 0 && -f "$STAMP" ]]; then
  age=$(( $(date +%s) - $(stat -f %m "$STAMP") ))
  if (( age < MIN_AGE )) && [[ "$(cat "$STAMP")" == "$HEAD" ]]; then log "skip: $HEAD installed $((age / 3600)) h ago"; exit 0; fi
fi

# Away from the Mac's Wi-Fi: publish the build for an install over Tailscale and push its link, once per build
# and again after 4 days (the free profile lasts 7). Installing still needs one tap on the iPhone.
ota_fallback() {
  local stamp="$STATE/last-ota" age=999999
  [[ -f "$stamp" ]] && age=$(( $(date +%s) - $(stat -f %m "$stamp") ))
  if [[ "$(cat "$stamp" 2>/dev/null)" == "$HEAD" ]] && (( age < 4 * 86400 )); then log "skip: $HEAD already offered over Tailscale"; return 0; fi
  local link
  link=$(bash "$REPO/ios/ota.sh" | tee /dev/stderr | sed -n 's/^Open on the iPhone (Tailscale on): //p')
  [[ -n "$link" ]] || { log "ota.sh printed no link"; return 1; }
  curl -sf -X POST -H 'Content-Type: application/json' "http://127.0.0.1:${AGENT_MONITOR_PORT:-7777}/api/notify/install" \
    -d "{\"build\":\"$HEAD\",\"url\":\"$link\"}" >/dev/null || { log "could not send the install notification"; return 1; }
  echo "$HEAD" > "$stamp"
  log "offered $HEAD over Tailscale"
}

# Pick the named device, else the first connected iPhone that is reachable now.
json="$STATE/devices.json"
xcrun devicectl list devices --json-output "$json" >/dev/null 2>&1 || { log "devicectl failed"; exit 1; }
DEVICE=$(/usr/bin/python3 - "$json" "$DEVICE" <<'PY'
import json, sys
want = sys.argv[2]
devs = json.load(open(sys.argv[1])).get("result", {}).get("devices", [])
def ok(d):
    cp = d.get("connectionProperties", {})
    return cp.get("tunnelState") in ("connected", "disconnected") and cp.get("pairingState") == "paired" and d.get("hardwareProperties", {}).get("platform") == "iOS"
for d in devs:
    ident, name = d.get("identifier", ""), d.get("deviceProperties", {}).get("name", "")
    if want and want not in (ident, name):
        continue
    if ok(d) and d.get("connectionProperties", {}).get("transportType") in ("wired", "localNetwork"):
        print(ident, d.get("hardwareProperties", {}).get("udid", "")); break
PY
)
read -r DEVICE UDID <<<"$DEVICE"
if [[ -z "$DEVICE" ]]; then
  log "no reachable paired iPhone (same Wi-Fi or USB needed)"
  # iOS downloads but does not install a free-team build over the internet; set AM_IOS_OTA=1 with a paid team.
  [[ "${AM_IOS_OTA:-0}" == "1" ]] && ota_fallback
  exit 0
fi
# A free team signs only for registered devices, so build for this phone and let Xcode register it.
DEST='generic/platform=iOS'
[[ -n "${UDID:-}" ]] && DEST="platform=iOS,id=$UDID"

# Move the ending profile aside so Xcode asks Apple for a new one; put it back if the build fails.
moved=0
if [[ -n "$STALE" ]]; then
  mkdir -p "$OLD"
  while IFS= read -r f; do mv "$f" "$OLD/" && moved=1; done <<<"$STALE"
  log "renew: moved the ending profile aside"
fi
restore() { (( moved )) && mv "$OLD"/*.mobileprovision "$PROFILES/" 2>/dev/null; return 0; }

log "build $HEAD for team $TEAM"
if ! xcodebuild -project "$REPO/ios/AgentMonitor.xcodeproj" -scheme AgentMonitor -configuration Release \
  -destination "$DEST" -derivedDataPath "$OUT" -allowProvisioningUpdates -allowProvisioningDeviceRegistration \
  DEVELOPMENT_TEAM="$TEAM" AM_DEFAULT_SERVER="${AM_DEFAULT_SERVER:-}" CODE_SIGN_STYLE=Automatic CURRENT_PROJECT_VERSION="$BUILD" MARKETING_VERSION="1.0.$BUILD" build -quiet; then
  restore; log "build failed"; exit 1
fi
APP="$OUT/Build/Products/Release-iphoneos/AgentMonitor.app"
[[ -d "$APP" ]] || { restore; log "no app at $APP"; exit 1; }
END=$(profile_end "$APP/embedded.mobileprovision")
rm -f "$OLD"/*.mobileprovision
# The iPhone already has this end date: a reinstall would only restart the app, so try again later.
if [[ -f "$RENEW" ]] && (( $(epoch "$END") * 1000 <= $(tr -dc 0-9 <"$RENEW") )) && [[ "$(cat "$STAMP" 2>/dev/null)" == "$HEAD" ]]; then
  log "renew: Apple gave the same profile (ends $END); will try again"; exit 0
fi

log "install on $DEVICE"
xcrun devicectl device install app --device "$DEVICE" "$APP"
echo "$HEAD" > "$STAMP"
if [[ -f "$RENEW" ]] && (( $(epoch "$END") * 1000 > $(tr -dc 0-9 <"$RENEW") )); then rm -f "$RENEW"; log "renew: done"; fi
log "done; profile expires ${END:-unknown}"
