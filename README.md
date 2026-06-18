# thestockie-influencer

A daily (or manually-triggered) Go job that scans a list of YouTube stock-portfolio
influencers, transcribes their new videos, extracts each creator's stock views and
macro commentary, and aggregates everything into a cross-influencer sentiment ranking
and a daily macro digest — all stored in **thestockie's Convex** deployment.

📖 **Docs:** [Architecture](docs/ARCHITECTURE.md) · [Deployment](docs/DEPLOYMENT.md)

```
                          thestockie-influencer (Go, single binary, runs on a VPS)
 ┌──────────────┐  RSS   ┌───────────────┐  audio  ┌───────────────┐  text  ┌──────────────────┐
 │  YouTube     │ ─────► │  discover     │ ──────► │  yt-dlp +      │ ─────► │  Ollama Cloud    │
 │  channels    │        │  new videos   │         │  whisper.cpp   │        │  (gpt-oss:120b)  │
 └──────────────┘        └───────────────┘         │  large-v3-turbo│        │  extract → JSON  │
                                                   └───────────────┘        └────────┬─────────┘
                                                                                     │ per-video:
                                                                                     │ symbols, stance,
                                                                                     │ thesis, macro
                                                                                     ▼
 ┌───────────────────────────── Convex (exciting-bee-603) ──────────────────────────────────────┐
 │  influencers · influencerVideos · videoStockMentions · macroNotes                            │
 │            │                                                                                  │
 │            ▼  aggregate (trailing window)                 ▼  synthesize (Ollama)              │
 │       dailySentiment  ◄── per-symbol bull/bear ranking    macroDigest ◄── narrative + actions │
 └───────────────────────────────────────────────────────────────────────────────────────────────┘
```

## What it produces

- **Per influencer**: every analyzed video's stock mentions — `symbol`, `stance`
  (bullish/bearish/neutral), `conviction`, the `thesis` (why), suggested `action`,
  optional price target — plus per-video macro/sector/rotation notes.
- **Across all influencers**: `dailySentiment` — for each ticker, the bullish/bearish
  counts, a conviction-weighted `netScore`, a `consensus` label, and the strongest
  theses, computed over a trailing window (default 7 days).
- **Daily macro digest**: an LLM-written market narrative, key themes, sector
  rotations, the bullish/bearish leaders, and a prioritized list of recommended actions.

## Repository layout

```
cmd/influencer-job/      CLI entrypoint (flags, wiring)
internal/
  config/                env + .env loading, influencer JSON
  models/                shared types + LLM-output sanitation
  youtube/               RSS discovery + @handle → channelId resolution
  transcribe/            yt-dlp → ffmpeg (16k mono) → whisper.cpp
  llm/                   Ollama Cloud client, per-video extract, daily synthesis
  convex/                HTTP client for the Convex endpoints
  pipeline/              orchestration
config/influencers.example.json
deploy/                  systemd service + timer
scripts/setup-vps.sh     one-time VPS provisioning
```

The Convex side lives in the **thestockie** repo: `convex/schema.ts` (tables),
`convex/influencer.ts` (queries/mutations/aggregation), `convex/http.ts` (the HTTP
endpoints this job calls).

## Prerequisites

- Go 1.25+
- `yt-dlp` and `ffmpeg` on PATH
- `whisper-cli` (whisper.cpp) + a `ggml-large-v3-turbo.bin` model
  - macOS dev: `brew install whisper-cpp ffmpeg yt-dlp`
- **Deno** — YouTube now requires a JS runtime for yt-dlp extraction
  - VPS: see `scripts/setup-vps.sh`
- An [Ollama Cloud](https://ollama.com) API key
- Access to thestockie's Convex deployment

## 1. Convex setup (one time)

From the **thestockie** repo (the new files are already added):

```bash
# Choose a long random shared secret and register it with Convex:
npx convex env set INGEST_SECRET "$(openssl rand -hex 32)"

# Push schema + functions + HTTP routes to the single deployment (exciting-bee-603):
npx convex dev --once
```

thestockie uses **one Convex deployment, `exciting-bee-603`, for both local/preview
and production**. Your HTTP base URL is its **`.convex.site`** origin:
`https://exciting-bee-603.convex.site`. Put the same `INGEST_SECRET` in this job's `.env`.

## 2. Configure

```bash
cp .env.example .env                       # fill in secrets
cp config/influencers.example.json config/influencers.json
```

Edit `config/influencers.json` — list the channels you want. Each entry can use a
`@handle`/`youtubeUrl` (the job resolves the `UC…` channel id on first run) or an
explicit `channelId`. The list is mirrored into Convex's `influencers` table each run.

## 3. Build & run locally

```bash
make build

# See what it WOULD do — RSS discovery only, no transcription, no writes:
./bin/influencer-job --dry-run --verbose

# One real manual pass:
./bin/influencer-job --mode manual

# Just one creator (matches channelId/handle/name), capped to 1 newest video:
./bin/influencer-job --influencer "Joseph Carlson" --max-videos 1

# Re-run only the aggregation + digest (no discovery/transcription):
./bin/influencer-job --aggregate-only
```

### Useful flags

| Flag | Purpose |
|------|---------|
| `--mode daily\|manual` | label stored on the run record |
| `--dry-run` | discover + log only; no transcription, no DB writes |
| `--no-llm` | transcribe + store transcript only; skip extraction/synthesis |
| `--aggregate-only` | skip discovery; recompute `dailySentiment` + digest |
| `--influencer <s>` | only influencers matching channelId/handle/name |
| `--since <date>` | only videos on/after `YYYY-MM-DD` (overrides max-age) |
| `--max-videos <n>` | per-channel cap this run |
| `--max-age-days <n>` | discovery age cutoff |
| `--window-days <n>` | aggregation trailing window |
| `--verbose` | debug logging |

## 4. Deploy to the VPS (systemd)

```bash
# On the box (Debian/Ubuntu, as root): installs ffmpeg, yt-dlp, whisper.cpp + model
sudo bash scripts/setup-vps.sh

# From your Mac: cross-compile and ship the binary + config
make build-linux
scp bin/influencer-job-linux-amd64 root@<host>:/opt/thestockie-influencer/influencer-job
scp .env                    root@<host>:/opt/thestockie-influencer/.env
scp config/influencers.json root@<host>:/opt/thestockie-influencer/config/influencers.json
scp cookies.txt             root@<host>:/opt/thestockie-influencer/cookies.txt
```

On the box, point `.env` at the installed model, a writable work dir, and the cookies:

```
WHISPER_MODEL=/opt/thestockie-influencer/models/ggml-large-v3-turbo.bin
WORK_DIR=/opt/thestockie-influencer/work
YTDLP_COOKIES_FILE=/opt/thestockie-influencer/cookies.txt
```

> **YouTube auth:** downloads need a logged-in `cookies.txt` (Netscape format,
> exported from a browser) or YouTube will bot-block them. Cookies **expire** —
> re-export every few weeks. Also keep yt-dlp current (`yt-dlp -U`); stale
> versions fail YouTube's "n-challenge".

Install the timer:

```bash
cp deploy/thestockie-influencer.{service,timer} /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now thestockie-influencer.timer   # daily at 06:00 (+jitter)
```

**Manual trigger** any time:

```bash
systemctl start thestockie-influencer.service
journalctl -u thestockie-influencer.service -f
```

## How it works

1. **Discover** — for each influencer, fetch `https://www.youtube.com/feeds/videos.xml?channel_id=…`
   (free, no API key/quota), keep videos newer than `MAX_VIDEO_AGE_DAYS`, newest first,
   capped at `MAX_VIDEOS_PER_CHANNEL`. Each video is registered in Convex; already-processed
   ones are skipped (idempotent on `videoId`).
2. **Transcribe** — `yt-dlp` pulls bestaudio, ffmpeg downsamples to 16 kHz mono WAV,
   `whisper.cpp` (large-v3-turbo) transcribes. Audio/JSON scratch files are cleaned up.
3. **Extract** — the transcript goes to Ollama Cloud with a strict JSON schema; the model
   returns symbols + stances + theses + macro commentary. Output is sanitized (junk tickers
   dropped, enums clamped) before writing.
4. **Aggregate** — Convex recomputes `dailySentiment` from all mentions in the trailing
   window: per-symbol bull/bear counts and a conviction-weighted net score (high=3, med=2,
   low=1).
5. **Synthesize** — the ranking + macro notes go back to Ollama for the daily `macroDigest`
   (narrative, themes, rotations, recommended actions).

## Cost / performance notes

- **Transcription** is the heavy step. On the Xeon E3-1270 v6 (4c/8t), large-v3-turbo runs
  roughly around real-time; a handful of 10–30 min videos per night is comfortable. Tune
  `MAX_VIDEOS_PER_CHANNEL` / `MAX_VIDEO_AGE_DAYS` to bound it. Videos are processed
  sequentially so whisper's 8 threads aren't oversubscribed.
- **LLM**: two kinds of calls — one extraction per new video, one synthesis per run.
  Swap models via `OLLAMA_MODEL` (e.g. `gpt-oss:20b` for cheaper/faster, larger for quality).
- Reruns are cheap: only unseen videos are transcribed; `--aggregate-only` recomputes the
  rollup without touching YouTube/Whisper.
```
