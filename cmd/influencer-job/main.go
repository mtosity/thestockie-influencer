// Command influencer-job runs one pass of the thestockie influencer pipeline:
// discover new influencer videos, transcribe (yt-dlp + whisper.cpp), extract
// structured views (Ollama Cloud), store them in Convex, then aggregate
// cross-influencer sentiment and write the daily macro digest.
//
// It is single-shot by design — scheduling is handled externally (systemd
// timer). Run it by hand for a manual trigger.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/mtosity/thestockie-influencer/internal/config"
	"github.com/mtosity/thestockie-influencer/internal/convex"
	"github.com/mtosity/thestockie-influencer/internal/llm"
	"github.com/mtosity/thestockie-influencer/internal/pipeline"
	"github.com/mtosity/thestockie-influencer/internal/transcribe"
)

func main() {
	var (
		envPath         = flag.String("env", ".env", "path to .env file (optional)")
		modeFlag        = flag.String("mode", "manual", "run mode label: daily|manual")
		only            = flag.String("influencer", "", "only this influencer (matches channelId/handle/name)")
		influencersFile = flag.String("influencers", "", "override path to influencers JSON")
		aggregateOnly   = flag.Bool("aggregate-only", false, "skip discovery; only recompute aggregate + digest")
		dryRun          = flag.Bool("dry-run", false, "discover + log only; no transcription, no writes")
		noLLM           = flag.Bool("no-llm", false, "transcribe + store only; skip LLM extraction/synthesis")
		maxAge          = flag.Int("max-age-days", 0, "override MAX_VIDEO_AGE_DAYS")
		maxVideos       = flag.Int("max-videos", 0, "override MAX_VIDEOS_PER_CHANNEL")
		windowDays      = flag.Int("window-days", 0, "override WINDOW_DAYS")
		since           = flag.String("since", "", "only videos on/after this date (YYYY-MM-DD or RFC3339)")
		video           = flag.String("video", "", "probe ONE video URL: transcribe (+extract) and print; no Convex writes")
		verbose         = flag.Bool("verbose", false, "debug logging")
	)
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if err := config.LoadDotEnv(*envPath); err != nil {
		log.Warn("load .env", "path", *envPath, "err", err)
	}
	cfg := config.Load()
	if *influencersFile != "" {
		cfg.InfluencersFile = *influencersFile
	}
	if *maxAge > 0 {
		cfg.MaxVideoAgeDays = *maxAge
	}
	if *maxVideos > 0 {
		cfg.MaxVideosPerChannel = *maxVideos
	}
	if *windowDays > 0 {
		cfg.WindowDays = *windowDays
	}

	// Probe mode: transcribe (+extract) a single video, no Convex involved.
	if *video != "" {
		if _, err := os.Stat(cfg.WhisperModel); err != nil {
			log.Error("whisper model not found (set WHISPER_MODEL)", "path", cfg.WhisperModel, "err", err)
			os.Exit(2)
		}
		if !*noLLM && cfg.OllamaAPIKey == "" {
			log.Error("OLLAMA_API_KEY required for extraction; set it or pass --no-llm")
			os.Exit(2)
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		ollama := &llm.Ollama{Host: cfg.OllamaHost, APIKey: cfg.OllamaAPIKey, Model: cfg.OllamaModel, HTTP: &http.Client{Timeout: 8 * time.Minute}}
		tr := &transcribe.Transcriber{
			YtDlp: cfg.YtDlpBin, Ffmpeg: cfg.FfmpegBin, WhisperBin: cfg.WhisperBin,
			CookiesFromBrowser: cfg.CookiesFromBrowser, CookiesFile: cfg.CookiesFile,
			WhisperModel: cfg.WhisperModel, Threads: cfg.WhisperThreads, Lang: cfg.WhisperLang,
			WorkDir: cfg.WorkDir, KeepAudio: cfg.KeepAudio, Log: log,
		}
		p := pipeline.New(cfg, nil, tr, llm.New(ollama, 0, log), &http.Client{Timeout: 30 * time.Second}, log)
		log.Info("probe mode", "model", cfg.OllamaModel, "whisperModel", filepath.Base(cfg.WhisperModel), "noLLM", *noLLM)
		if err := p.Probe(ctx, *video, *noLLM); err != nil {
			log.Error("probe failed", "err", err)
			os.Exit(1)
		}
		return
	}

	needLLM := !*noLLM && !*dryRun
	needTranscribe := !*dryRun && !*aggregateOnly
	if err := cfg.Validate(needLLM, needTranscribe); err != nil {
		log.Error("invalid configuration", "err", err)
		os.Exit(2)
	}

	var sinceMs int64
	if *since != "" {
		t, err := parseSince(*since)
		if err != nil {
			log.Error("invalid --since (use YYYY-MM-DD or RFC3339)", "value", *since, "err", err)
			os.Exit(2)
		}
		sinceMs = t.UnixMilli()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cx := convex.New(cfg.ConvexSiteURL, cfg.IngestSecret, &http.Client{Timeout: 90 * time.Second})
	ollama := &llm.Ollama{
		Host:   cfg.OllamaHost,
		APIKey: cfg.OllamaAPIKey,
		Model:  cfg.OllamaModel,
		HTTP:   &http.Client{Timeout: 8 * time.Minute},
	}
	svc := llm.New(ollama, 0, log)
	tr := &transcribe.Transcriber{
		YtDlp:              cfg.YtDlpBin,
		Ffmpeg:             cfg.FfmpegBin,
		CookiesFromBrowser: cfg.CookiesFromBrowser,
		CookiesFile:        cfg.CookiesFile,
		WhisperBin:         cfg.WhisperBin,
		WhisperModel:       cfg.WhisperModel,
		Threads:            cfg.WhisperThreads,
		Lang:               cfg.WhisperLang,
		WorkDir:            cfg.WorkDir,
		KeepAudio:          cfg.KeepAudio,
		Log:                log,
	}
	discover := &http.Client{Timeout: 30 * time.Second}

	runMode := *modeFlag
	if *aggregateOnly {
		runMode = "aggregate"
	}
	opts := pipeline.Options{
		Mode:           runMode,
		OnlyInfluencer: *only,
		AggregateOnly:  *aggregateOnly,
		DryRun:         *dryRun,
		NoLLM:          *noLLM,
		SinceMs:        sinceMs,
	}

	log.Info("thestockie-influencer starting",
		"mode", runMode,
		"model", cfg.OllamaModel,
		"whisperModel", filepath.Base(cfg.WhisperModel),
		"windowDays", cfg.WindowDays,
		"maxAgeDays", cfg.MaxVideoAgeDays,
		"maxVideosPerChannel", cfg.MaxVideosPerChannel,
		"dryRun", *dryRun, "noLLM", *noLLM, "aggregateOnly", *aggregateOnly)

	p := pipeline.New(cfg, cx, tr, svc, discover, log)
	if err := p.Run(ctx, opts); err != nil {
		log.Error("run failed", "err", err)
		os.Exit(1)
	}
}

func parseSince(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02", s)
}
