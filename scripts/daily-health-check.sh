#!/bin/bash
# Daily backup check: if the thestockie-influencer.service failed or errored
# in the last 24h, ping MT. Otherwise stay silent. Designed to be cron'd once
# a day via OpenClaw's `cron run` (no agent round-trip needed).

set -e
CHAT_ID="7020739374"
BOT_TOKEN="8673603183:AAHrocYAD5v1xycZFgJFr1vS2GKSRUVgOCk"

# Last 24h of journal for the service.
JOURNAL=$(journalctl -u thestockie-influencer.service --since "24 hours ago" --no-pager 2>/dev/null)

# Trigger conditions (any of these -> ping):
#  1) Service entered "failed" state in the window.
#  2) An ERROR-level log line appeared (the job keeps going on per-video
#     errors, but if it's persistently erroring we want to know).
#  3) The job never started (no "Starting thestockie" line in 24h, despite
#     the daily timer).
START_COUNT=$(echo "$JOURNAL" | grep -c "Starting thestockie influencer" || true)
FAIL_COUNT=$(echo "$JOURNAL" | grep -cE "Failed with result|Service hold-off" || true)
ERR_COUNT=$(echo "$JOURNAL" | grep -cE "level=ERROR" || true)

# Read last run stats if any.
LAST_COMPLETE=$(echo "$JOURNAL" | grep -E 'msg="run complete"' | tail -1)
DISCOVERED=$(echo "$LAST_COMPLETE" | grep -oP 'discovered=\K\d+' || echo 0)
PROCESSED=$(echo "$LAST_COMPLETE"  | grep -oP 'processed=\K\d+'  || echo 0)
ERRORED=$(echo "$LAST_COMPLETE"    | grep -oP 'errored=\K\d+'    || echo 0)
LAST_ERR_LINE=$(echo "$JOURNAL" | grep -E 'level=ERROR' | tail -1 \
  | sed -E 's/.*msg="([^"]+)".*err="([^"]+)".*/\1: \2/; s/.*msg="([^"]+)\".*/\1/')
LAST_ERR_LINE=$(echo "$LAST_ERR_LINE" | head -c 300)

REASON=""
if [ "$START_COUNT" = "0" ]; then
  REASON="⚠️ The thestockie-influencer.timer should have fired in the last 24h but no run was started."
elif [ "$FAIL_COUNT" -gt 0 ]; then
  REASON="❌ The thestockie-influencer.service entered a failed state in the last 24h."
elif [ "$ERR_COUNT" -gt 3 ]; then
  REASON="⚠️ ${ERR_COUNT} ERROR-level log lines in the last 24h."
fi

if [ -z "$REASON" ]; then
  # All clear. Stay silent.
  exit 0
fi

# Build message.
MSG="${REASON}

📊 *Last run stats*
• Discovered: ${DISCOVERED}
• Processed:  ${PROCESSED}
• Errored:    ${ERRORED}

🪵 *Last error*
\`${LAST_ERR_LINE:-none}\`

🔍 \`journalctl -u thestockie-influencer.service -n 50 --no-pager\`"

python3 - "$BOT_TOKEN" "$CHAT_ID" "$MSG" <<'PY'
import json, sys, urllib.request, urllib.parse
bot, chat, text = sys.argv[1], sys.argv[2], sys.argv[3]
data = urllib.parse.urlencode({
    "chat_id": chat, "text": text, "parse_mode": "Markdown",
    "disable_web_page_preview": "true",
}).encode()
try:
    urllib.request.urlopen(f"https://api.telegram.org/bot{bot}/sendMessage", data, timeout=10).read()
except Exception as e:
    sys.stderr.write(f"telegram send failed: {e}\n")
    sys.exit(1)
PY
