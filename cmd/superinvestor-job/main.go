// Command superinvestor-job scans SEC EDGAR for the tracked investors' latest
// 13F-HR filings, diffs them quarter-over-quarter, resolves tickers, and stores
// the moves + cross-investor consensus in Convex. Single-shot; schedule it with
// a systemd timer (every ~12h) to catch new filings.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mtosity/thestockie-influencer/internal/config"
	"github.com/mtosity/thestockie-influencer/internal/convex"
	"github.com/mtosity/thestockie-influencer/internal/edgar"
	"github.com/mtosity/thestockie-influencer/internal/figi"
	"github.com/mtosity/thestockie-influencer/internal/sec13f"
)

func main() {
	var (
		envPath    = flag.String("env", ".env", "path to .env file (optional)")
		only       = flag.String("investor", "", "only process this CIK")
		force      = flag.Bool("force", false, "process even if EDGAR has nothing newer than stored")
		dryRun     = flag.Bool("dry-run", false, "fetch + diff + print, no Convex writes")
		configFile = flag.String("config", "", "override path to superinvestors.json")
		verbose    = flag.Bool("verbose", false, "debug logging")
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
	if *configFile != "" {
		cfg.SuperInvestorsFile = *configFile
	}
	if !*dryRun && (cfg.ConvexSiteURL == "" || cfg.IngestSecret == "") {
		log.Error("missing CONVEX_SITE_URL / INGEST_SECRET")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	svc := &sec13f.Service{
		ConfigFile: cfg.SuperInvestorsFile,
		CX:         convex.New(cfg.ConvexSiteURL, cfg.IngestSecret, &http.Client{Timeout: 90 * time.Second}),
		Edgar:      &edgar.Client{HTTP: &http.Client{Timeout: 60 * time.Second}, UserAgent: cfg.EdgarUserAgent},
		Figi:       &figi.Client{HTTP: &http.Client{Timeout: 60 * time.Second}, APIKey: cfg.OpenFIGIKey},
		Log:        log,
	}

	log.Info("superinvestor-job starting", "config", cfg.SuperInvestorsFile, "dryRun", *dryRun, "force", *force)
	if err := svc.Run(ctx, sec13f.Options{OnlyCik: *only, Force: *force, DryRun: *dryRun}); err != nil {
		log.Error("run failed", "err", err)
		os.Exit(1)
	}
}
