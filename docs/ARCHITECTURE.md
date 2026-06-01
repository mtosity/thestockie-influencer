# Architecture

`thestockie-influencer` is a batch job that turns YouTube stock-investing
creators into structured, queryable sentiment data for **thestockie**. It runs
daily (or on demand), and every run is independent and idempotent.

## 1. System overview

```
                          thestockie-influencer (Go, single static binary)
 ┌──────────────┐  RSS    ┌────────────┐  audio   ┌────────────────┐  text   ┌──────────────────┐
 │  YouTube     │ ──────► │ discover   │ ───────► │ yt-dlp + ffmpeg │ ──────► │  Ollama Cloud    │
 │  channels    │         │ new videos │          │ + whisper.cpp   │         │  (deepseek-v4)   │
 └──────────────┘         └────────────┘          │ large-v3-turbo  │         │ extract → JSON   │
       ▲ cookies.txt                              └────────────────┘         └────────┬─────────┘
       │ (auth)                                                                       │ per video:
       │                                                                              │ symbols, stance,
       │                                                                              │ thesis, macro
       ▼                                                                              ▼
 ┌──────────────────────────── Convex (HTTP API, bearer-guarded) ───────────────────────────────┐
 │  influencers · influencerVideos · videoStockMentions · macroNotes · jobRuns                  │
 │             │                                                                                 │
 │             ▼  aggregate (trailing 60-day window, distinct creators)                          │
 │        dailySentiment ──► leaderboard            macroDigest ◄── synthesize (Ollama)          │
 └───────────────────────────────────────────────┬───────────────────────────────────────────────┘
                                                  │ tRPC reads
                                                  ▼
                                    thestockie Next.js app — /influencers page
```

Two repos, one Convex deployment:

- **`thestockie-influencer`** (this repo) — the Go producer job + its config/deploy.
- **`thestockie`** — the Next.js app. The Convex backend (schema, mutations,
  HTTP ingest endpoints, read queries, the `/influencers` page) lives here.

The job never talks to the app directly; it writes to Convex over HTTP, and the
app reads from Convex via tRPC.

## 2. The pipeline (per run)

1. **Purge** — delete videos/mentions/aggregates older than `RETENTION_DAYS`
   (60) so the dashboard only ever reflects the last ~2 months.
2. **Seed** — upsert the configured channel list (`config/influencers.json`)
   into the Convex `influencers` table. Convex is the runtime source of truth;
   the job scans whatever is active there.
3. **Discover** — for each channel, read its public RSS feed
   (`/feeds/videos.xml?channel_id=…` — no API key, no quota), keep videos newer
   than `MAX_VIDEO_AGE_DAYS` (62), newest first, capped at
   `MAX_VIDEOS_PER_CHANNEL` (7). Register each in Convex (idempotent on
   `videoId`); already-`done` videos are skipped.
4. **Transcribe** — `yt-dlp` pulls best audio (with cookies), `ffmpeg`
   downsamples to 16 kHz mono, `whisper.cpp` (`large-v3-turbo`) transcribes.
   Scratch files are cleaned up per video.
5. **Extract** — the transcript goes to Ollama Cloud with a strict JSON shape;
   the model returns `{ summary, mentions[], macro }`. Output is sanitized
   (junk tickers dropped, enums clamped) before storage.
6. **Store** — the per-video result (transcript, summary, mentions, macro note)
   is written to Convex; the video is marked `done`.
7. **Aggregate** — Convex recomputes `dailySentiment` from all mentions in the
   trailing window: per-symbol distinct-creator counts and consensus.
8. **Synthesize** — the ranking + macro notes go back to Ollama for the daily
   `macroDigest` (narrative, themes, sector rotations, recommended actions).

## 3. Go service layout

```
cmd/influencer-job/      entrypoint: flags, wiring, --video probe mode
internal/
  config/                env + .env loading, influencer JSON
  models/                shared types + LLM-output sanitation
  youtube/               RSS discovery + @handle→channelId (canonical/externalId)
  transcribe/            yt-dlp → ffmpeg → whisper.cpp (with cookie + retry)
  llm/                   Ollama Cloud client (retry), per-video extract, synthesis
  convex/                HTTP client for the Convex endpoints
  pipeline/              orchestration (purge, seed, discover, process, aggregate)
```

Zero third-party Go dependencies — standard library only. The influencer list
is plain JSON; secrets come from the environment / `.env`.

## 4. Convex data model

Defined in `thestockie/convex/schema.ts`:

| Table | Purpose | Key fields |
|---|---|---|
| `influencers` | tracked channels (watchlist) | `name`, `channelId`, `handle`, `avatar`, `active` |
| `influencerVideos` | per-video processing ledger | `videoId`, `status`, `transcript`, `publishedAt` |
| `videoStockMentions` | one row per (video, ticker) | `symbol`, `stance`, `conviction`, `thesis`, `action` |
| `macroNotes` | per-video macro/sector commentary | `macroSummary`, `sectorViews`, `rotations` |
| `dailySentiment` | aggregated per-symbol consensus | `bullishCount`, `bearishCount`, `*Creators[]`, `consensus` |
| `macroDigest` | daily LLM-written synthesis | `marketSentiment`, `keyThemes`, `recommendedActions` |
| `jobRuns` | one row per execution (observability) | `mode`, `status`, counts |

Convex functions (`thestockie/convex/`):

- `influencer.ts` — **internal** mutations/queries: `discoverVideo`,
  `saveVideoResult`, `aggregate`, `saveDigest`, `purgeOld`, run bookkeeping,
  `deleteInfluencerByChannelId`.
- `http.ts` — **public** `httpAction` endpoints the Go job calls, guarded by
  `Authorization: Bearer ${INGEST_SECRET}` (fail-closed if unset).
- `influencerReads.ts` — **public** read queries for the app
  (`latestDigest`, `sentimentRanking`, `influencers`, `recentVideos`,
  `videosByChannel`, `latestRun`).

## 5. Sentiment model

- **Counts are distinct creators per stance** — a creator who posts several
  videos on a ticker counts once. (Stored as `bullishCreators[]` etc. for the
  hover tooltips; the count is the set size.)
- **Consensus** is derived from the bullish-vs-bearish creator ratio
  (`strong_bullish … mixed … strong_bearish`).
- **Leaderboard** (`sentimentRanking`): a stock appears only on the side more
  creators lean toward; within a side it's sorted by that side's creator count
  (top votes first). Every stock with a real consensus (>2 creators) is shown;
  if a side has fewer than 12, it's filled with lighter (1–2 creator) names.
- `netScore` (conviction-weighted) is still stored but no longer drives the UI —
  hyperbolic language shouldn't inflate a ranking.

## 6. Reliability

- **Idempotent**: re-running only transcribes unseen/failed videos
  (`status != done`). `--aggregate-only` recomputes the rollup without touching
  YouTube/Whisper.
- **Retries**: Ollama calls retry on network/5xx (4× backoff); yt-dlp retries
  3× (YouTube intermittently bot-challenges even authenticated requests);
  interrupted videos left in `transcribing`/`analyzing` are retried on the next
  run.
- **Auth**: YouTube increasingly requires a logged-in `cookies.txt` and a
  current `yt-dlp` (stale versions fail the JS "n-challenge"). See
  [DEPLOYMENT.md](./DEPLOYMENT.md).

## 7. Key decisions

| Decision | Why |
|---|---|
| YouTube **RSS** for discovery | free, no API key/quota |
| **whisper.cpp** (not Python whisper) | single native binary, no Python/torch runtime; pairs with a Go service |
| `large-v3-turbo` | near-large accuracy, runs on CPU (Xeon) at ~real-time |
| **Ollama Cloud** for the LLM | open models, no local GPU; structured extraction + synthesis |
| Schema in the **thestockie** repo | Convex is the app's backend; one deployment, visible to the dashboard |
| Embed JSON shape in the prompt (no `format`) | reasoning models route answers to a `thinking` field and return empty `content` when `format`-constrained |
| 60-day **retention/window** | dashboard reflects current views, not stale calls |
