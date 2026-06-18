#!/usr/bin/env bash
#
# Helper to refresh YouTube cookies when the server IP gets flagged.
# YouTube requires authenticated cookies for reliable downloads.
# Cookies expire and IP addresses can get flagged after too many requests.
#
# Usage:
#   1. On your local machine (Mac), export fresh cookies from Chrome:
#      Install "Get cookies.txt LOCALLY" extension
#      Open youtube.com while logged in
#      Export as Netscape format → cookies.txt
#
#   2. Copy to server:
#      scp cookies.txt root@your-server:/opt/thestockie-influencer/cookies.txt
#
#   3. Or run this script on the server if Chrome is installed:
#      bash scripts/cookie-refresh.sh
#

COOKIES_FILE="${1:-/opt/thestockie-influencer/cookies.txt}"

echo "=== YouTube Cookie Refresh ==="
echo ""
echo "YouTube frequently flags server IPs. When you see:"
echo '  "Sign in to confirm you\'re not a bot"'
echo "you need fresh cookies."
echo ""

# Try cookies-from-browser if Chrome is available
if command -v google-chrome &>/dev/null || command -v chromium &>/dev/null || [ -d "$HOME/.config/google-chrome" ]; then
  echo "Chrome/Chromium found. Attempting to pull cookies from browser..."
  yt-dlp --cookies-from-browser chrome --no-download "https://www.youtube.com/watch?v=dQw4w9WgXcQ" 2>/dev/null
  if [ $? -eq 0 ]; then
    echo "✅ Browser cookies work! Consider setting YTDLP_COOKIES_FROM_BROWSER=chrome in .env"
    exit 0
  fi
fi

echo "Manual cookie export required:"
echo ""
echo "1. Install 'Get cookies.txt LOCALLY' Chrome extension"
echo "2. Go to youtube.com (make sure you're logged in)"
echo "3. Click extension → Export → Netscape format"
echo "4. Save as cookies.txt"
echo "5. Copy to server:"
echo "   scp cookies.txt root@YOUR_SERVER:/opt/thestockie-influencer/cookies.txt"
echo ""
echo "Current cookie file: $COOKIES_FILE"
if [ -f "$COOKIES_FILE" ]; then
  echo "   Exists: yes ($(wc -l < "$COOKIES_FILE") lines)"
  # Check expiry of VISITOR_INFO1_LIVE
  EXPIRY=$(grep "VISITOR_INFO1_LIVE" "$COOKIES_FILE" 2>/dev/null | awk '{print $5}' || echo "unknown")
  if [ "$EXPIRY" != "unknown" ] && [ -n "$EXPIRY" ]; then
    EXPIRY_DATE=$(date -d "@${EXPIRY}" 2>/dev/null || python3 -c "import datetime; print(datetime.datetime.fromtimestamp($EXPIRY).strftime('%Y-%m-%d %H:%M'))" 2>/dev/null)
    echo "   Session cookie expires: $EXPIRY_DATE"
  fi
else
  echo "   Exists: NO"
fi

# ═══════════════════════════════════════════════════════════════════════════
# NEW: Automated CDP-based cookie extraction (June 2026)
# ═══════════════════════════════════════════════════════════════════════════
# If you have a bridge Chrome running (via noVNC) that's logged into YouTube,
# you can use auto-cookie-refresh.sh to extract cookies automatically via CDP:
#
#   ./scripts/auto-cookie-refresh.sh
#
# This connects to Chrome's remote debugging port (9222), pulls all YouTube
# cookies, saves them to cookies.txt + cookies-fresh.txt + ~/.youtube-cookies.txt,
# pushes to 1Password, and restarts the influencer service.
#
# notify.sh will also trigger this automatically when cookie errors are detected.
