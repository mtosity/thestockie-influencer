// Package sec13f orchestrates the 13F pipeline: seed investors, scan EDGAR for
// new filings, diff quarter-over-quarter, resolve tickers, and store to Convex.
package sec13f

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"sort"

	"github.com/mtosity/thestockie-influencer/internal/convex"
	"github.com/mtosity/thestockie-influencer/internal/edgar"
	"github.com/mtosity/thestockie-influencer/internal/figi"
	"github.com/mtosity/thestockie-influencer/internal/models"
)

type Service struct {
	ConfigFile string
	CX         *convex.Client
	Edgar      *edgar.Client
	Figi       *figi.Client
	Log        *slog.Logger
}

type Options struct {
	OnlyCik string // process just this CIK
	Force   bool   // process even if EDGAR has nothing newer than stored
	DryRun  bool   // fetch + diff + print, no writes
}

func (s *Service) Run(ctx context.Context, opts Options) error {
	investors, err := loadConfig(s.ConfigFile)
	if err != nil {
		return err
	}

	// Seed config → Convex (skip in dry-run).
	if !opts.DryRun {
		for _, inv := range investors {
			if err := s.CX.SeedInvestor(ctx, inv); err != nil {
				s.Log.Warn("seed investor failed", "name", inv.Name, "err", err)
			}
		}
	}

	// Latest stored filing date per CIK (to skip unchanged investors).
	lastByCik := map[string]int64{}
	if !opts.DryRun {
		refs, err := s.CX.ListActiveInvestors(ctx)
		if err != nil {
			return err
		}
		for _, r := range refs {
			lastByCik[r.Cik] = r.LastFilingDate
		}
	}

	latestPeriod := ""
	processed := 0
	for _, inv := range investors {
		if opts.OnlyCik != "" && inv.Cik != opts.OnlyCik {
			continue
		}
		cur, prior, err := s.Edgar.Latest13F(ctx, inv.Cik)
		if err != nil {
			s.Log.Warn("edgar latest failed", "investor", inv.Name, "err", err)
			continue
		}
		if cur.Period > latestPeriod {
			latestPeriod = cur.Period
		}
		if !opts.Force && !opts.DryRun && cur.FilingDate.UnixMilli() <= lastByCik[inv.Cik] {
			s.Log.Info("up to date, skipping", "investor", inv.Name, "period", cur.Period)
			continue
		}

		curH, err := s.Edgar.Holdings(ctx, inv.Cik, cur)
		if err != nil {
			s.Log.Warn("holdings failed", "investor", inv.Name, "err", err)
			continue
		}
		var priorH []edgar.Holding
		if prior != nil {
			if priorH, err = s.Edgar.Holdings(ctx, inv.Cik, prior); err != nil {
				s.Log.Warn("prior holdings failed (treating all as new)", "investor", inv.Name, "err", err)
			}
		}

		positions, totalValue := diff(curH, priorH)
		s.resolveTickers(ctx, positions, opts.DryRun)

		if opts.DryRun {
			s.printSummary(inv, cur, positions, totalValue)
			processed++
			continue
		}
		if err := s.CX.SaveFiling(ctx, convex.SaveFilingReq{
			Cik:        inv.Cik,
			Period:     cur.Period,
			ReportDate: cur.ReportDate.UnixMilli(),
			FilingDate: cur.FilingDate.UnixMilli(),
			TotalValue: totalValue,
			Positions:  positions,
		}); err != nil {
			s.Log.Error("save filing failed", "investor", inv.Name, "err", err)
			continue
		}
		processed++
		s.Log.Info("saved 13F", "investor", inv.Name, "period", cur.Period, "positions", len(positions))
	}

	if processed > 0 && latestPeriod != "" && !opts.DryRun {
		if err := s.CX.AggregateConsensus(ctx, latestPeriod); err != nil {
			s.Log.Warn("aggregate consensus failed", "period", latestPeriod, "err", err)
		} else {
			s.Log.Info("aggregated consensus", "period", latestPeriod)
		}
	}
	s.Log.Info("13f run complete", "processed", processed)
	return nil
}

// ── QoQ diff ─────────────────────────────────────────────────────────────────

type agg struct {
	name     string
	shares   float64
	value    float64
	isOption bool
}

func aggregateByCusip(hs []edgar.Holding) map[string]*agg {
	m := map[string]*agg{}
	for _, h := range hs {
		a := m[h.Cusip]
		if a == nil {
			a = &agg{name: h.Name}
			m[h.Cusip] = a
		}
		if a.name == "" {
			a.name = h.Name
		}
		a.value += h.Value
		if h.PutCall == "" {
			a.shares += h.Shares
		} else {
			a.isOption = true
		}
	}
	return m
}

func diff(cur, prior []edgar.Holding) ([]models.Position, float64) {
	curAgg := aggregateByCusip(cur)
	priorAgg := aggregateByCusip(prior)

	var total float64
	for _, a := range curAgg {
		total += a.value
	}

	var positions []models.Position
	for cusip, c := range curAgg {
		p := models.Position{
			Cusip:    cusip,
			Name:     c.name,
			Shares:   c.shares,
			Value:    c.value,
			IsOption: c.isOption,
		}
		if total > 0 {
			p.PctPortfolio = c.value / total * 100
		}
		if pr, ok := priorAgg[cusip]; ok && pr.shares > 0 {
			ch := (c.shares - pr.shares) / pr.shares * 100
			ps := pr.shares
			p.PrevShares = &ps
			switch {
			case c.shares > pr.shares*1.005:
				p.ChangeType, p.ChangePct = "added", &ch
			case c.shares < pr.shares*0.995:
				p.ChangeType, p.ChangePct = "reduced", &ch
			default:
				p.ChangeType = "hold"
			}
		} else if _, ok := priorAgg[cusip]; ok {
			p.ChangeType = "hold"
		} else {
			p.ChangeType = "new"
		}
		positions = append(positions, p)
	}
	// Exits: present in prior, gone in current.
	for cusip, pr := range priorAgg {
		if _, ok := curAgg[cusip]; ok {
			continue
		}
		neg := -100.0
		ps := pr.shares
		positions = append(positions, models.Position{
			Cusip:      cusip,
			Name:       pr.name,
			Shares:     0,
			Value:      pr.value, // prior stake size, for display ranking
			ChangeType: "sold",
			ChangePct:  &neg,
			PrevShares: &ps,
			IsOption:   pr.isOption,
		})
	}
	return positions, total
}

// ── Ticker resolution (Convex cache → OpenFIGI) ──────────────────────────────

func (s *Service) resolveTickers(ctx context.Context, positions []models.Position, dryRun bool) {
	seen := map[string]bool{}
	var cusips []string
	for _, p := range positions {
		if !seen[p.Cusip] {
			seen[p.Cusip] = true
			cusips = append(cusips, p.Cusip)
		}
	}

	tickers := map[string]string{}
	var misses []string
	if dryRun {
		misses = cusips
	} else {
		cached, err := s.CX.CusipLookup(ctx, cusips)
		if err != nil {
			s.Log.Warn("cusip cache lookup failed", "err", err)
		}
		for _, c := range cusips {
			if e, ok := cached[c]; ok {
				if e.Ticker != nil {
					tickers[c] = *e.Ticker
				}
			} else {
				misses = append(misses, c)
			}
		}
	}

	if len(misses) > 0 {
		resolved, err := s.Figi.Map(ctx, misses)
		if err != nil {
			s.Log.Warn("openfigi map failed", "err", err)
		}
		var entries []convex.CusipEntry
		for _, c := range misses {
			r := resolved[c]
			if r.Ticker != "" {
				tickers[c] = r.Ticker
			}
			entries = append(entries, convex.CusipEntry{Cusip: c, Ticker: r.Ticker, Name: r.Name}) // negative-cache misses too
		}
		if !dryRun {
			if err := s.CX.CusipSave(ctx, entries); err != nil {
				s.Log.Warn("cusip cache save failed", "err", err)
			}
		}
	}

	for i := range positions {
		positions[i].Ticker = tickers[positions[i].Cusip]
	}
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func loadConfig(path string) ([]models.SuperInvestor, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var list []models.SuperInvestor
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	return list, nil
}

func (s *Service) printSummary(inv models.SuperInvestor, f *edgar.Filing, ps []models.Position, total float64) {
	counts := map[string]int{}
	for _, p := range ps {
		counts[p.ChangeType]++
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].Value > ps[j].Value })
	s.Log.Info("DRY 13F",
		"investor", inv.Name, "period", f.Period,
		"holdings", counts["new"]+counts["added"]+counts["reduced"]+counts["hold"],
		"new", counts["new"], "added", counts["added"], "reduced", counts["reduced"], "sold", counts["sold"],
		"portfolio$", int64(total))
	top := ps
	if len(top) > 8 {
		top = top[:8]
	}
	for _, p := range top {
		s.Log.Info("  position", "ticker", orDash(p.Ticker), "name", trunc(p.Name, 28), "move", p.ChangeType, "pct", int(p.PctPortfolio))
	}
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
