#!/bin/bash
# Post-run Telegram notification for thestockie-influencer.
#
# Reads the latest "run complete" line from the systemd journal and builds
# a one-line summary. Sends EXACTLY ONE Telegram ping per run (post-run only).
#
# If the run had errors that look cookie/auth-related, it triggers
# auto-cookie-refresh.sh to extract fresh cookies from the bridge Chrome
# and restart the job automatically.
set -e

BOT_TOKEN="8673603183:AAHrocYAD5v1xycZFgJFr1vS2GKSRUVgOCk"
CHAT_ID="7020739374"
HOSTNAME_SHORT=$(hostname -s)
SERVICE="thestockie-influencer.service"
AUTO_REFRESH="/opt/thestockie-influencer/auto-cookie-refresh.sh"

# Latest "run complete" log line from this service.
LAST_COMPLETE=$(journalctl -u $SERVICE -n 200 --no-pager 2>/dev/null \
  | grep -E 'msg="run complete"' | tail -1)

if [ -z "$LAST_COMPLETE" ]; then
  # Run ended without a "run complete" line — likely crashed.
  LAST_ERR=$(journalctl -u $SERVICE -n 50 --no-pager -p err 2>/dev/null \
    | tail -3 | sed 's/.*influencer-job\[[0-9]*\]: //')
  MSG="❌ *thestockie-influencer crashed*

🖥️  Host: \`${HOSTNAME_SHORT}\`
📅 $(date '+%Y-%m-%d %H:%M UTC')

\`\`\`
${LAST_ERR:-no error log found}
\`\`\`"
else
  # Parse: msg="run complete" discovered=N processed=M errored=K
  DISCOVERED=$(echo "$LAST_COMPLETE" | grep -oP 'discovered=\K\d+')
  PROCESSED=$(echo "$LAST_COMPLETE"  | grep -oP 'processed=\K\d+')
  ERRORED=$(echo "$LAST_COMPLETE"    | grep -oP 'errored=\K\d+')
  DISCOVERED=${DISCOVERED:-?}
  PROCESSED=${PROCESSED:-?}
  ERRORED=${ERRORED:-0}

  # Wall-clock duration of the most recent run.
  START_LINE=$(journalctl -u $SERVICE -n 200 --no-pager 2>/dev/null \
    | grep -E 'Starting thestockie influencer' | tail -1)
  if [ -n "$START_LINE" ]; then
    START_TS=$(date -d "$(echo "$START_LINE" | awk '{print $1, $2, $3}')" +%s 2>/dev/null || echo 0)
    NOW=$(date +%s)
    if [ "$START_TS" -gt 0 ]; then
      HOURS=$(( (NOW - START_TS) / 3600 ))
      MINS=$(( (((NOW - START_TS) % 3600) + 30) / 60 ))
      RUNTIME_STR=" (${HOURS}h ${MINS}m)"
    else
      RUNTIME_STR=""
    fi
  else
    RUNTIME_STR=""
  fi

  # Surface the most recent error if any.
  LAST_ERR=""
  COOKIE_ERROR=false
  if [ "$ERRORED" -gt 0 ] 2>/dev/null; then
    # Grab all error lines to check for cookie/auth issues
    ALL_ERRORS=$(journalctl -u $SERVICE -n 500 --no-pager 2>/dev/null \
      | grep -E 'level=ERROR')
    LAST_ERR=$(echo "$ALL_ERRORS" | tail -1 \
      | sed -E 's/.*msg="([^"]+)".*err="([^"]+)".*/\1: \2/; s/.*msg="([^"]+)".*/\1/')
    if [ -n "$LAST_ERR" ]; then
      LAST_ERR=$(echo "$LAST_ERR" | head -c 300)
      LAST_ERR="
⚠️  Last error: \`${LAST_ERR}\`"
    fi

    # Check if errors are cookie/auth-related
    COOKIE_ERROR_COUNT=$(echo "$ALL_ERRORS" | grep -ciE "Sign in to confirm|cookie|not a bot|cookies-from-browser|members-only" || true)
    if [ "$COOKIE_ERROR_COUNT" -gt 2 ]; then
      COOKIE_ERROR=true
    fi
  fi

  # Status emoji based on errored count.
  if [ "$ERRORED" = "0" ]; then
    EMOJI="✅"
  elif [ "$COOKIE_ERROR" = "true" ]; then
    EMOJI="🍪"
  else
    EMOJI="⚠️"
  fi

  AUTO_MSG=""
  if [ "$COOKIE_ERROR" = "true" ]; then
    AUTO_MSG="

🔄 *Auto-recovery:* Cookie/auth errors detected. Refreshing cookies from bridge Chrome and restarting..."
  fi

  MSG="${EMOJI} *thestockie-influencer run finished*${RUNTIME_STR}

🖥️  Host: \`${HOSTNAME_SHORT}\`
📅 $(date '+%Y-%m-%d %H:%M UTC')

📊 *Stats*
• Discovered: ${DISCOVERED}
• Processed:  ${PROCESSED}
• Errored:    ${ERRORED}${LAST_ERR}${AUTO_MSG}"
fi

# Send to Telegram.
python3 - "$BOT_TOKEN" "$CHAT_ID" "$MSG" <<'PY'
import json, sys, urllib.request, urllib.parse
bot, chat, text = sys.argv[1], sys.argv[2], sys.argv[3]
data = urllib.parse.urlencode({
    "chat_id": chat,
    "text": text,
    "parse_mode": "Markdown",
    "disable_web_page_preview": "true",
}).encode()
try:
    urllib.request.urlopen(f"https://api.telegram.org/bot{bot}/sendMessage", data, timeout=10).read()
except Exception as e:
    sys.stderr.write(f"telegram send failed: {e}\n")
    sys.exit(1)
PY

# ── Auto-recovery: refresh cookies and restart if cookie errors detected ────
if [ "${COOKIE_ERROR:-false}" = "true" ] && [ -x "$AUTO_REFRESH" ]; then
  # Run in background so notify.sh doesn't block systemd
  nohup "$AUTO_REFRESH" > /tmp/auto-cookie-refresh.log 2>&1 &
fi