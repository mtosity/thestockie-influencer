// Package pipeline orchestrates the end-to-end daily job: discover new videos,
// transcribe, extract structured views, store them, then aggregate across all
// influencers and synthesize the daily macro digest.
package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/mtosity/thestockie-influencer/internal/config"
	"github.com/mtosity/thestockie-influencer/internal/convex"
	"github.com/mtosity/thestockie-influencer/internal/llm"
	"github.com/mtosity/thestockie-influencer/internal/models"
	"github.com/mtosity/thestockie-influencer/internal/transcribe"
	"github.com/mtosity/thestockie-influencer/internal/youtube"
)

const perVideoTimeout = 40 * time.Minute

type Options struct {
	Mode           string // daily | manual | aggregate
	OnlyInfluencer string // filter: matches channelId / handle / name
	AggregateOnly  bool
	DryRun         bool
	NoLLM          bool
	SinceMs        int64 // 0 → use MaxVideoAgeDays
}

type Pipeline struct {
	cfg *config.Config
	cx  *convex.Client
	tr  *transcribe.Transcriber
	llm *llm.Service
	yt  *http.Client
	log *slog.Logger
}

func New(cfg *config.Config, cx *convex.Client, tr *transcribe.Transcriber, svc *llm.Service, yt *http.Client, log *slog.Logger) *Pipeline {
	return &Pipeline{cfg: cfg, cx: cx, tr: tr, llm: svc, yt: yt, log: log}
}

type stats struct{ discovered, processed, errored int }

// Run executes a single pass.
func (p *Pipeline) Run(ctx context.Context, opts Options) error {
	var st stats

	var runID string
	if !opts.DryRun {
		if id, err := p.cx.StartRun(ctx, opts.Mode); err != nil {
			p.log.Warn("startRun failed (continuing)", "err", err)
		} else {
			runID = id
		}
	}

	// Drop anything older than the retention window so the dashboard only ever
	// reflects recent videos.
	if !opts.DryRun && p.cfg.RetentionDays > 0 {
		if r, err := p.cx.Purge(ctx, p.cfg.RetentionDays); err != nil {
			p.log.Warn("purge failed", "err", err)
		} else if r.Videos > 0 || r.Sentiment > 0 || r.Digests > 0 {
			p.log.Info("purged outdated", "videos", r.Videos, "mentions", r.Mentions,
				"sentiment", r.Sentiment, "digests", r.Digests, "olderThanDays", p.cfg.RetentionDays)
		}
	}

	// Resolve the scan list. In a dry run we work straight off the config file
	// (so it's useful before anything is seeded); otherwise we mirror config
	// into Convex and read the active list back as the source of truth.
	var scan []models.Influencer
	if !opts.AggregateOnly {
		cfgList := p.resolveConfigList(ctx)
		if opts.DryRun {
			scan = cfgList
		} else {
			for _, inf := range cfgList {
				if err := p.cx.Seed(ctx, inf); err != nil {
					p.log.Warn("seed influencer failed", "name", inf.Name, "err", err)
				}
			}
			active, err := p.cx.ListActive(ctx)
			if err != nil {
				return p.finish(ctx, runID, st, err)
			}
			scan = active
		}
		if opts.OnlyInfluencer != "" {
			scan = filterInfluencers(scan, opts.OnlyInfluencer)
		}

		p.log.Info("scanning influencers", "count", len(scan))
		for _, inf := range scan {
			p.processInfluencer(ctx, inf, opts, &st)
		}
	}

	// Aggregate + synthesize.
	if !opts.DryRun {
		if err := p.aggregateAndDigest(ctx, opts, len(scan)); err != nil {
			p.log.Warn("aggregate/digest failed", "err", err)
		}
	}

	p.log.Info("run complete", "discovered", st.discovered, "processed", st.processed, "errored", st.errored)
	return p.finish(ctx, runID, st, nil)
}

func (p *Pipeline) finish(ctx context.Context, runID string, st stats, runErr error) error {
	if runID != "" {
		status := "success"
		errStr := ""
		if runErr != nil {
			status = "error"
			errStr = runErr.Error()
		}
		if err := p.cx.FinishRun(ctx, convex.FinishRunReq{
			RunID:            runID,
			Status:           status,
			VideosDiscovered: st.discovered,
			VideosProcessed:  st.processed,
			VideosErrored:    st.errored,
			Error:            errStr,
		}); err != nil {
			p.log.Warn("finishRun failed", "err", err)
		}
	}
	return runErr
}

// resolveConfigList loads the config file and resolves any missing channel ids
// from @handles / URLs. Read-only.
func (p *Pipeline) resolveConfigList(ctx context.Context) []models.Influencer {
	list, err := config.LoadInfluencers(p.cfg.InfluencersFile)
	if err != nil {
		p.log.Warn("load influencers file failed", "path", p.cfg.InfluencersFile, "err", err)
		return nil
	}
	out := make([]models.Influencer, 0, len(list))
	for _, inf := range list {
		if inf.ChannelID == "" {
			ref := inf.Handle
			if ref == "" {
				ref = inf.YouTubeURL
			}
			if ref == "" {
				p.log.Warn("influencer has no channelId/handle/url; skipping", "name", inf.Name)
				continue
			}
			id, err := youtube.ResolveChannelID(ctx, p.yt, ref)
			if err != nil {
				p.log.Warn("resolve channelId failed; skipping", "name", inf.Name, "ref", ref, "err", err)
				continue
			}
			inf.ChannelID = id
			p.log.Info("resolved channelId", "name", inf.Name, "channelId", id)
		}
		out = append(out, inf)
	}
	return out
}

func (p *Pipeline) processInfluencer(ctx context.Context, inf models.Influencer, opts Options, st *stats) {
	cands, err := youtube.DiscoverVideos(ctx, p.yt, inf.ChannelID)
	if err != nil {
		p.log.Warn("discover failed", "influencer", inf.Name, "err", err)
		return
	}

	cutoff := opts.SinceMs
	if cutoff == 0 {
		cutoff = time.Now().AddDate(0, 0, -p.cfg.MaxVideoAgeDays).UnixMilli()
	}
	var fresh []models.VideoCandidate
	for _, c := range cands {
		if c.PublishedAt >= cutoff {
			fresh = append(fresh, c)
		}
	}
	sort.Slice(fresh, func(i, j int) bool { return fresh[i].PublishedAt > fresh[j].PublishedAt })
	if len(fresh) > p.cfg.MaxVideosPerChannel {
		fresh = fresh[:p.cfg.MaxVideosPerChannel]
	}

	for _, cand := range fresh {
		if opts.DryRun {
			p.log.Info("would process", "influencer", inf.Name, "title", cand.Title,
				"published", time.UnixMilli(cand.PublishedAt).Format("2006-01-02"), "url", cand.URL)
			st.discovered++
			continue
		}
		disc, err := p.cx.Discover(ctx, cand)
		if err != nil {
			p.log.Warn("register video failed", "videoId", cand.VideoID, "err", err)
			continue
		}
		if disc.IsNew {
			st.discovered++
		}
		if !shouldProcess(disc.Status) {
			p.log.Debug("skip (already handled)", "videoId", cand.VideoID, "status", disc.Status)
			continue
		}
		p.processVideo(ctx, inf, cand, opts, st)
	}
}

func (p *Pipeline) processVideo(ctx context.Context, inf models.Influencer, cand models.VideoCandidate, opts Options, st *stats) {
	vctx, cancel := context.WithTimeout(ctx, perVideoTimeout)
	defer cancel()

	p.log.Info("processing video", "influencer", inf.Name, "title", cand.Title, "videoId", cand.VideoID)

	_ = p.cx.SetStatus(vctx, cand.VideoID, "transcribing")
	transcript, err := p.tr.Transcribe(vctx, cand.VideoID, cand.URL)
	if err != nil {
		p.log.Error("transcribe failed", "videoId", cand.VideoID, "err", err)
		_ = p.cx.MarkError(ctx, cand.VideoID, "transcribe: "+err.Error())
		st.errored++
		return
	}

	if opts.NoLLM {
		if err := p.cx.SaveResult(vctx, convex.SaveResultReq{VideoID: cand.VideoID, Transcript: transcript}); err != nil {
			p.log.Error("save (no-llm) failed", "videoId", cand.VideoID, "err", err)
			st.errored++
			return
		}
		p.log.Info("transcript stored (no-llm)", "videoId", cand.VideoID, "chars", len(transcript))
		st.processed++
		return
	}

	_ = p.cx.SetStatus(vctx, cand.VideoID, "analyzing")
	analysis, err := p.llm.ExtractVideo(vctx, inf.Name, cand.Title, cand.PublishedAt, transcript)
	if err != nil {
		p.log.Error("extract failed", "videoId", cand.VideoID, "err", err)
		_ = p.cx.MarkError(ctx, cand.VideoID, "extract: "+err.Error())
		st.errored++
		return
	}

	if err := p.cx.SaveResult(vctx, convex.SaveResultReq{
		VideoID:    cand.VideoID,
		Transcript: transcript,
		Summary:    analysis.Summary,
		Mentions:   analysis.Mentions,
		Macro:      analysis.Macro,
	}); err != nil {
		p.log.Error("save result failed", "videoId", cand.VideoID, "err", err)
		_ = p.cx.MarkError(ctx, cand.VideoID, "save: "+err.Error())
		st.errored++
		return
	}
	p.log.Info("video done", "videoId", cand.VideoID, "mentions", len(analysis.Mentions), "hasMacro", analysis.Macro != nil)
	st.processed++
}

func (p *Pipeline) aggregateAndDigest(ctx context.Context, opts Options, influencersCount int) error {
	date := time.Now().UTC().Format("2006-01-02")
	agg, err := p.cx.Aggregate(ctx, date, p.cfg.WindowDays)
	if err != nil {
		return err
	}
	p.log.Info("aggregated sentiment", "date", date, "symbols", agg.SymbolCount, "windowDays", agg.WindowDays)

	if opts.NoLLM {
		return nil
	}
	digest, err := p.llm.Synthesize(ctx, agg)
	if err != nil {
		return err
	}
	if digest == nil {
		p.log.Info("no data to synthesize; skipping digest")
		return nil
	}
	return p.cx.SaveDigest(ctx, convex.SaveDigestReq{
		Date:               date,
		MarketSentiment:    digest.MarketSentiment,
		SentimentLabel:     digest.SentimentLabel,
		KeyThemes:          digest.KeyThemes,
		SectorRotation:     digest.SectorRotation,
		BullishLeaders:     leadersFromRanked(agg.BullishLeaders),
		BearishLeaders:     leadersFromRanked(agg.BearishLeaders),
		RecommendedActions: digest.RecommendedActions,
		VideosAnalyzed:     len(agg.MacroNotes),
		InfluencersCount:   influencersCount,
		WindowDays:         agg.WindowDays,
	})
}

// Probe runs transcription (+ optional extraction) for a single video URL and
// prints the result to stdout. It does not touch Convex — handy for testing
// the toolchain on one video before wiring up the full pipeline.
func (p *Pipeline) Probe(ctx context.Context, videoURL string, noLLM bool) error {
	videoID := youtube.VideoIDFromURL(videoURL)
	if videoID == "" {
		videoID = "probe"
	}
	p.log.Info("probe: transcribing", "videoId", videoID, "url", videoURL)

	transcript, err := p.tr.Transcribe(ctx, videoID, videoURL)
	if err != nil {
		return fmt.Errorf("transcribe: %w", err)
	}
	p.log.Info("probe: transcript ready", "chars", len(transcript), "words", len(strings.Fields(transcript)))
	fmt.Printf("\n──── TRANSCRIPT (first 1500 chars of %d) ────\n%s\n", len(transcript), preview(transcript, 1500))

	if noLLM {
		return nil
	}
	p.log.Info("probe: extracting via LLM")
	analysis, err := p.llm.ExtractVideo(ctx, "(probe)", videoID, time.Now().UnixMilli(), transcript)
	if err != nil {
		return fmt.Errorf("extract: %w", err)
	}
	out, _ := json.MarshalIndent(analysis, "", "  ")
	fmt.Printf("\n──── EXTRACTION ────\n%s\n", out)
	p.log.Info("probe: done", "mentions", len(analysis.Mentions), "hasMacro", analysis.Macro != nil)
	return nil
}

func preview(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// shouldProcess decides whether to (re)transcribe a registered video. Anything
// not finished is fair game — including "transcribing"/"analyzing", which an
// interrupted run can leave stale (runs are sequential, so there's no genuine
// in-flight video to clobber).
func shouldProcess(status string) bool {
	return status != "done" && status != "skipped"
}

func filterInfluencers(in []models.Influencer, needle string) []models.Influencer {
	needle = strings.ToLower(needle)
	var out []models.Influencer
	for _, inf := range in {
		if strings.Contains(strings.ToLower(inf.ChannelID), needle) ||
			strings.Contains(strings.ToLower(inf.Handle), needle) ||
			strings.Contains(strings.ToLower(inf.Name), needle) {
			out = append(out, inf)
		}
	}
	return out
}

func leadersFromRanked(rs []models.RankedSymbol) []models.Leader {
	out := make([]models.Leader, 0, len(rs))
	for _, r := range rs {
		out = append(out, models.Leader{Symbol: r.Symbol, NetScore: r.NetScore, Mentions: r.Mentions})
	}
	return out
}
