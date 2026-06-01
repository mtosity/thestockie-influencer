# Deployment

End-to-end guide to run the job in production (daily on a VPS) and what the
Convex/cloud side needs. See [ARCHITECTURE.md](./ARCHITECTURE.md) for how it
works.

## 0. Prerequisites

- A Linux VPS (the reference box: Intel Xeon E3-1270 v6, 4c/8t, 32 GB).
- An [Ollama Cloud](https://ollama.com) API key.
- Access to thestockie's Convex deployment (to deploy functions + set the secret).
- A YouTube account to export cookies from.

## 1. Convex side (once)

The schema, ingest endpoints, and read queries live in the **thestockie** repo
(`convex/`). From there:

```bash
# Pick a long random shared secret and register it on the deployment:
npx convex env set INGEST_SECRET "$(openssl rand -hex 32)"

# Push schema + functions + HTTP routes:
npx convex dev --once     # → dev deployment (exciting-bee-603)
npx convex deploy         # → production deployment, when going live
```

The job's `CONVEX_SITE_URL` is the deployment's **`.convex.site`** origin
(e.g. `https://exciting-bee-603.convex.site`); its `INGEST_SECRET` must match the
value set above. Dev and prod are separate deployments — set the secret and
deploy functions on whichever the production site reads.

## 2. YouTube cookies (required)

YouTube bot-blocks unauthenticated downloads. Export a logged-in `cookies.txt`:

1. Install a "Get cookies.txt LOCALLY" browser extension.
2. Open `youtube.com` while **logged in**.
3. Export → save as `cookies.txt` (Netscape format).
4. Point `YTDLP_COOKIES_FILE` at it.

Cookies **expire** — re-export and re-copy every few weeks. (Alternatively set
`YTDLP_COOKIES_FROM_BROWSER=chrome` to pull from a browser on the same host.)

## 3. Provision the VPS (once, as root)

```bash
sudo bash scripts/setup-vps.sh
```

This installs `ffmpeg`, `python3`, `nodejs` (yt-dlp's JS "n-challenge" solver),
the latest `yt-dlp`, builds `whisper.cpp`, downloads the `ggml-large-v3-turbo`
model, and creates the `thestockie` service user + `/opt/thestockie-influencer`.

## 4. Build & ship

```bash
# From your Mac / CI:
make build-linux                                   # → bin/influencer-job-linux-amd64
scp bin/influencer-job-linux-amd64 root@<host>:/opt/thestockie-influencer/influencer-job
scp .env                    root@<host>:/opt/thestockie-influencer/.env
scp config/influencers.json root@<host>:/opt/thestockie-influencer/config/influencers.json
scp cookies.txt             root@<host>:/opt/thestockie-influencer/cookies.txt
```

On the box, ensure `.env` points at the installed model, a writable work dir,
and the cookies:

```ini
WHISPER_MODEL=/opt/thestockie-influencer/models/ggml-large-v3-turbo.bin
WORK_DIR=/opt/thestockie-influencer/work
YTDLP_COOKIES_FILE=/opt/thestockie-influencer/cookies.txt
```

```bash
chown -R thestockie:thestockie /opt/thestockie-influencer
chmod +x /opt/thestockie-influencer/influencer-job
```

## 5. Schedule (systemd)

```bash
cp deploy/thestockie-influencer.{service,timer} /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now thestockie-influencer.timer    # daily 06:00 + jitter
```

Manual trigger / logs:

```bash
systemctl start thestockie-influencer.service
journalctl -u thestockie-influencer.service -f
```

The unit is `Type=oneshot`, runs as the `thestockie` user, sets a full `PATH`
(so yt-dlp finds ffmpeg/node/whisper), is CPU-niced, and has a 6-hour timeout.

## 6. Environment reference

| Var | Default | Notes |
|---|---|---|
| `CONVEX_SITE_URL` | — | deployment `.convex.site` origin |
| `INGEST_SECRET` | — | must match Convex env var |
| `OLLAMA_HOST` | `https://ollama.com` | |
| `OLLAMA_API_KEY` | — | Ollama Cloud key |
| `OLLAMA_MODEL` | `deepseek-v4-flash` | any Ollama Cloud model |
| `WHISPER_BIN` | `whisper-cli` | |
| `WHISPER_MODEL` | `models/ggml-large-v3-turbo.bin` | |
| `WHISPER_THREADS` | `8` | |
| `YTDLP_BIN` | `yt-dlp` | keep current (`yt-dlp -U`) |
| `YTDLP_COOKIES_FILE` | — | exported `cookies.txt` |
| `YTDLP_COOKIES_FROM_BROWSER` | — | alternative to the file |
| `WINDOW_DAYS` | `60` | sentiment aggregation window |
| `RETENTION_DAYS` | `60` | purge data older than this |
| `MAX_VIDEO_AGE_DAYS` | `62` | discovery cutoff |
| `MAX_VIDEOS_PER_CHANNEL` | `7` | per channel per run |
| `WORK_DIR` | `$TMPDIR/thestockie-influencer` | audio/json scratch |
| `KEEP_AUDIO` | `false` | keep scratch for debugging |
| `INFLUENCERS_FILE` | `config/influencers.json` | watchlist |

## 7. CLI flags

| Flag | Purpose |
|---|---|
| `--mode daily\|manual` | label on the run record |
| `--video <url>` | probe one video (transcribe+extract, print, no Convex) |
| `--dry-run` | discover + log only |
| `--no-llm` | transcribe + store transcript only |
| `--aggregate-only` | recompute `dailySentiment` + digest |
| `--influencer <s>` | only channels matching channelId/handle/name |
| `--since <date>` | only videos on/after `YYYY-MM-DD` |
| `--max-videos` / `--max-age-days` / `--window-days` | overrides |
| `--verbose` | debug logging |

## 8. Operations

- **Monitoring**: each run writes a `jobRuns` row (mode, status, counts). Tail
  `journalctl -u thestockie-influencer.service`.
- **Watchlist**: edit `config/influencers.json` (name + `@handle` or `channelId`);
  the next run seeds it. Channel ids resolve via the canonical channel link.
- **Backfill / fix one channel**: `influencer-job --influencer <channelId> --max-videos 7`.

### Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `Sign in to confirm you're not a bot` | missing/expired cookies, or too much volume from one IP — refresh `cookies.txt`, lower `MAX_VIDEOS_PER_CHANNEL`, or wait out the rate-limit |
| `Requested format is not available` / n-challenge errors | stale yt-dlp — `yt-dlp -U`; ensure `node` is on PATH |
| extraction returns empty | don't send Ollama's `format` schema to reasoning models (already handled in code) |
| a video stuck and never retried | fixed — `transcribing`/`analyzing` are now retried next run |
| wrong channel's videos | handle resolved to a featured/secondary channel — set `channelId` explicitly in config (resolver now prefers the canonical link) |

## 9. Production checklist

- [ ] `npx convex deploy` to the **prod** deployment
- [ ] `INGEST_SECRET` set on prod; same value in the VPS `.env`
- [ ] VPS `.env` `CONVEX_SITE_URL` → prod `.convex.site`
- [ ] `OLLAMA_API_KEY` valid; `OLLAMA_MODEL` reachable
- [ ] `cookies.txt` present + fresh; `yt-dlp -U` recent
- [ ] `whisper-cli` + model present; `WHISPER_MODEL` path correct
- [ ] timer enabled; one manual run green (`jobRuns` row `success`)

## 10. Super Investors (13F) job — `superinvestor-job`

A **second**, much lighter job (no Whisper/ffmpeg/yt-dlp/cookies — just HTTP to
SEC EDGAR + OpenFIGI + Convex). It shares the same `/opt/thestockie-influencer`
dir, `.env`, and Convex deployment as the influencer job.

**Convex:** the 13F functions ship in the same `convex/` dir, so the section-1
`npx convex deploy` already includes them — no extra step.

**Build & ship** (`make build-linux` builds *both* binaries):

```bash
make build-linux
scp bin/superinvestor-job-linux-amd64 root@<host>:/opt/thestockie-influencer/superinvestor-job
scp config/superinvestors.json        root@<host>:/opt/thestockie-influencer/config/superinvestors.json
chmod +x /opt/thestockie-influencer/superinvestor-job
chown thestockie:thestockie /opt/thestockie-influencer/superinvestor-job
```

**Env** (in the same `.env`; `CONVEX_SITE_URL` + `INGEST_SECRET` are already there):

```ini
SUPERINVESTORS_FILE=config/superinvestors.json
EDGAR_USER_AGENT=thestockie you@yourdomain.com   # SEC requires a real contact
# OPENFIGI_API_KEY=...                            # optional, faster CUSIP→ticker
```

**Schedule (every 12h)** — the job is idempotent and **skips any investor whose
latest EDGAR filing is already stored**, so the 12h cadence cheaply catches new
rolling filings:

```bash
cp deploy/superinvestor.{service,timer} /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now superinvestor.timer       # runs 04:00 & 16:00
```

**Manual run / debug:**

```bash
systemctl start superinvestor.service            # full scan now
journalctl -u superinvestor.service -f
# or, locally, probe one investor without writing:
./superinvestor-job --investor 1067983 --dry-run
./superinvestor-job --force                      # reprocess all even if unchanged
```

**Checklist:** `superinvestor-job` shipped + `+x`; `config/superinvestors.json`
present; `EDGAR_USER_AGENT` set to a real contact; `superinvestor.timer` enabled;
one manual run populates `investorConsensus` for the latest period.

### Troubleshooting (13F)

| Symptom | Cause / fix |
|---|---|
| EDGAR `403` | missing/blocked `EDGAR_USER_AGENT` — set a descriptive UA with a real email |
| tickers missing (`—`) | OpenFIGI couldn't map the CUSIP (often foreign issuers) — set `OPENFIGI_API_KEY` for reliability; values still aggregate by CUSIP |
| nothing updates | every investor already up to date — use `--force` to reprocess |
| `401` on `/investor/*` | `.env` `INGEST_SECRET` ≠ the Convex deployment's — re-sync them |
