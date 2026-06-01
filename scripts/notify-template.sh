#!/bin/bash
# Post-run Telegram notification for thestockie-influencer
# Copy to /opt/thestockie-influencer/notify.sh and set BOT_TOKEN + CHAT_ID

BOT_TOKEN="YOUR_BOT_TOKEN"
CHAT_ID="YOUR_CHAT_ID"

# Get run status from journal
STATUS=$(systemctl status thestockie-influencer.service --no-pager 2>/dev/null | grep "Active:" | head -1)

MSG="🎯 thestockie-influencer run update

📅 $(date '+%Y-%m-%d %H:%M %Z')
🖥️ $(hostname)
📋 ${STATUS:-status unknown}

Check logs: journalctl -u thestockie-influencer.service -f"

curl -s -X POST "https://api.telegram.org/bot${BOT_TOKEN}/sendMessage" \
  -d "chat_id=${CHAT_ID}" \
  -d "text=${MSG}" \
  -d "parse_mode=Markdown" \
  > /dev/null 2>&1
