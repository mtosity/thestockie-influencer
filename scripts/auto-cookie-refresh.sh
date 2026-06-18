#!/bin/bash
# auto-cookie-refresh.sh — Extract fresh YouTube cookies from a running bridge
# Chrome (opened via noVNC) and restart the influencer job.
#
# Triggered automatically by notify.sh when cookie/auth errors are detected,
# or can be run manually.
#
# Requires:
#   - Bridge Chrome running with --remote-debugging-port=9222
#   - Chrome logged into a YouTube/Google account
#   - Python 3 with aiohttp + websockets
#
# Usage:
#   ./scripts/auto-cookie-refresh.sh
set -euo pipefail

LOG_PREFIX="[auto-cookie-refresh]"
BRIDGE_CDP_URL="http://localhost:9222"
COOKIES_FILE="${YTDLP_COOKIES_FILE:-/opt/thestockie-influencer/cookies.txt}"
COOKIES_FRESH="$(dirname "$COOKIES_FILE")/cookies-fresh.txt"
OP_ITEM_ID="${OP_ITEM_ID:-2x4prtonnfis54poidrfjdvu6y}"
SERVICE="thestockie-influencer.service"

log() { echo "$LOG_PREFIX $(date -u '+%H:%M:%S') $*" >&2; }

# ── 1. Check if bridge Chrome is running ────────────────────────────────────
if ! curl -sf "$BRIDGE_CDP_URL/json" >/dev/null 2>&1; then
  log "❌ Bridge Chrome not running on $BRIDGE_CDP_URL — cannot extract cookies."
  log "   Start it: DISPLAY=:99 python3 scripts/yt-bridge.py &"
  exit 1
fi

# ── 2. Extract cookies via CDP ─────────────────────────────────────────────
log "Extracting cookies from bridge Chrome via CDP..."
python3 << 'PYEOF'
import asyncio, json, time, os

async def main():
    import aiohttp, websockets

    async with aiohttp.ClientSession() as session:
        async with session.get("http://localhost:9222/json") as r:
            targets = await r.json()

    page_target = next((t for t in targets if t.get("type") == "page"), None)
    if not page_target:
        print("No page target found", flush=True)
        return False

    ws_url = page_target["webSocketDebuggerUrl"]
    async with websockets.connect(ws_url, max_size=10_000_000) as ws:
        await ws.send(json.dumps({"id": 1, "method": "Network.enable"}))
        await ws.recv()
        await ws.send(json.dumps({"id": 2, "method": "Network.getAllCookies"}))
        result = json.loads(await ws.recv())
        cookies = result.get("result", {}).get("cookies", [])

    yt_cookies = [c for c in cookies if any(
        d in c.get("domain", "").lower()
        for d in [".youtube.com", "youtube.com", ".google.com", "google.com"]
    )]

    login_names = {"SID", "HSID", "SSID", "APISID", "SAPISID",
                   "__Secure-1PSID", "__Secure-3PSID", "LOGIN_INFO", "LOGININFO"}
    login_found = {c["name"] for c in yt_cookies if c["name"] in login_names}
    print(f"YouTube/Google cookies: {len(yt_cookies)}, login cookies: {len(login_found)}", flush=True)

    if not login_found:
        print("ERROR: No login cookies found — Chrome may not be logged in!", flush=True)
        return False

    # Save Netscape format
    netscape = "# Netscape HTTP Cookie File\n"
    netscape += f"# Generated {time.strftime('%Y-%m-%d %H:%M:%S UTC')}\n\n"
    for c in yt_cookies:
        http_only = "#HttpOnly_" if c.get("httpOnly") else ""
        secure = "TRUE" if c.get("secure") else "FALSE"
        expires = str(int(c.get("expires", 0)))
        domain = c["domain"]
        if not domain.startswith("."):
            domain = "." + domain
        netscape += f"{http_only}{domain}\tTRUE\t{c.get('path', '/')}\t{secure}\t{expires}\t{c['name']}\t{c['value']}\n"

    cookies_file = os.environ.get("YTDLP_COOKIES_FILE", "/opt/thestockie-influencer/cookies.txt")
    cookies_fresh = os.path.join(os.path.dirname(cookies_file), "cookies-fresh.txt")
    home_cookies = os.path.expanduser("~/.youtube-cookies.txt")

    for path in [cookies_file, cookies_fresh, home_cookies]:
        with open(path, "w") as f:
            f.write(netscape)
        os.chmod(path, 0o600)

    # Save JSON for 1Password
    cookie_json = [{
        "domain": c["domain"],
        "expirationDate": c.get("expires", -1) or -1,
        "hostOnly": not c["domain"].startswith("."),
        "httpOnly": c.get("httpOnly", False),
        "name": c["name"],
        "path": c.get("path", "/"),
        "sameSite": (c.get("sameSite") or "lax").lower(),
        "secure": c.get("secure", False),
        "session": (c.get("expires", -1) or 0) <= 0,
        "storeId": "0",
        "value": c["value"],
    } for c in yt_cookies]

    with open("/tmp/yt-cookies.json", "w") as f:
        json.dump(cookie_json, f, indent=2)

    print(f"Saved {len(yt_cookies)} cookies to all locations", flush=True)
    return True

success = asyncio.run(main())
exit(0 if success else 1)
PYEOF

if [ $? -ne 0 ]; then
  log "❌ Cookie extraction failed"
  exit 1
fi
log "✅ Cookies extracted and saved"

# ── 3. Push to 1Password (best-effort) ─────────────────────────────────────
if command -v op >/dev/null 2>&1 && op whoami >/dev/null 2>&1; then
  log "Pushing cookies to 1Password..."
  COOKIE_STR=$(cat /tmp/yt-cookies.json)
  op item edit "$OP_ITEM_ID" \
    cookies="$COOKIE_STR" \
    "notesPlain=Auto-extracted via CDP $(date -u '+%Y-%m-%d %H:%M UTC') | refreshed by auto-cookie-refresh" 2>&1 | head -3
  log "✅ 1Password updated"
else
  log "⚠️ 1Password not signed in — skipping 1P sync"
fi

# ── 4. Restart the influencer service ──────────────────────────────────────
sleep 2
log "Restarting $SERVICE with fresh cookies..."
systemctl restart "$SERVICE" 2>/dev/null || true

sleep 3
if systemctl is-active "$SERVICE" >/dev/null 2>&1; then
  log "✅ Service restarted successfully"
else
  log "⚠️ Service may not have started — check journalctl"
fi