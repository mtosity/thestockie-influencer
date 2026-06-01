#!/usr/bin/env bash
#
# One-time provisioning for the thestockie-influencer job on a Debian/Ubuntu VPS.
# Installs ffmpeg + yt-dlp, builds whisper.cpp, downloads the large-v3-turbo
# model, and lays out /opt/thestockie-influencer. Run as root.
#
#   sudo bash scripts/setup-vps.sh
#
set -euo pipefail

APP_DIR="/opt/thestockie-influencer"
APP_USER="thestockie"
WHISPER_SRC="/opt/whisper.cpp"
WHISPER_MODEL="large-v3-turbo"
BUILD_THREADS="$(nproc)"

echo "==> Installing system packages"
export DEBIAN_FRONTEND=noninteractive
apt-get update -y
# python3 + nodejs are needed by yt-dlp (nodejs solves YouTube's JS "n" challenge).
apt-get install -y ffmpeg git build-essential cmake curl ca-certificates python3 nodejs

echo "==> Installing yt-dlp (latest)"
curl -fsSL https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp \
  -o /usr/local/bin/yt-dlp
chmod a+rx /usr/local/bin/yt-dlp
# Keep it current — YouTube regularly breaks old yt-dlp:
/usr/local/bin/yt-dlp -U || true

echo "==> Building whisper.cpp"
if [ ! -d "$WHISPER_SRC/.git" ]; then
  git clone --depth 1 https://github.com/ggml-org/whisper.cpp "$WHISPER_SRC"
fi
cd "$WHISPER_SRC"
git pull --ff-only || true
cmake -B build -DCMAKE_BUILD_TYPE=Release
cmake --build build -j "$BUILD_THREADS" --config Release
install -m 0755 build/bin/whisper-cli /usr/local/bin/whisper-cli
echo "    whisper-cli -> $(command -v whisper-cli)"

echo "==> Downloading whisper model: $WHISPER_MODEL"
bash ./models/download-ggml-model.sh "$WHISPER_MODEL"

echo "==> Creating service user + app dir"
id -u "$APP_USER" >/dev/null 2>&1 || useradd --system --home "$APP_DIR" --shell /usr/sbin/nologin "$APP_USER"
mkdir -p "$APP_DIR/models" "$APP_DIR/config"
cp "$WHISPER_SRC/models/ggml-${WHISPER_MODEL}.bin" "$APP_DIR/models/"
chown -R "$APP_USER:$APP_USER" "$APP_DIR"

cat <<EOF

==> Done. Next steps:

  1. Copy the built binary to the box (from your Mac):
       make build-linux
       scp bin/influencer-job-linux-amd64 root@<host>:$APP_DIR/influencer-job

  2. Copy your config + secrets + YouTube cookies:
       scp .env                      root@<host>:$APP_DIR/.env
       scp config/influencers.json   root@<host>:$APP_DIR/config/influencers.json
       scp cookies.txt               root@<host>:$APP_DIR/cookies.txt   # export from a logged-in browser
     Make sure $APP_DIR/.env has:
       WHISPER_MODEL=$APP_DIR/models/ggml-${WHISPER_MODEL}.bin
       WORK_DIR=$APP_DIR/work
       YTDLP_COOKIES_FILE=$APP_DIR/cookies.txt
     Cookies expire — re-export and re-copy every few weeks. Keep yt-dlp fresh
     with a periodic 'yt-dlp -U'.

  3. Fix ownership + install the timer:
       chown -R $APP_USER:$APP_USER $APP_DIR
       chmod +x $APP_DIR/influencer-job
       cp deploy/thestockie-influencer.service /etc/systemd/system/
       cp deploy/thestockie-influencer.timer   /etc/systemd/system/
       systemctl daemon-reload
       systemctl enable --now thestockie-influencer.timer

  4. Trigger a run by hand any time:
       systemctl start thestockie-influencer.service
       journalctl -u thestockie-influencer.service -f
EOF
