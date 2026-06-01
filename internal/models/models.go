// Package models holds the types shared across the pipeline and the wire
// shapes exchanged with the Convex HTTP endpoints.
package models

import (
	"regexp"
	"strings"
)

// Influencer is a tracked YouTube channel. Loaded from config and/or returned
// by Convex's /influencer/active.
type Influencer struct {
	ID          string `json:"id,omitempty"` // Convex _id (from listActive)
	Name        string `json:"name"`
	ChannelID   string `json:"channelId"`
	Handle      string `json:"handle,omitempty"`
	YouTubeURL  string `json:"youtubeUrl,omitempty"`
	Avatar      string `json:"avatar,omitempty"`
	Description string `json:"description,omitempty"`
}

// VideoCandidate is a video discovered from a channel's RSS feed.
type VideoCandidate struct {
	VideoID     string
	ChannelID   string
	Title       string
	URL         string
	PublishedAt int64 // unix ms
}

// Mention is one influencer's stance on one ticker within a video.
type Mention struct {
	Symbol      string   `json:"symbol"`
	CompanyName string   `json:"companyName,omitempty"`
	Stance      string   `json:"stance"`                // bullish|bearish|neutral
	Conviction  string   `json:"conviction,omitempty"`  // low|medium|high
	Thesis      string   `json:"thesis"`                // why bullish/bearish
	Action      string   `json:"action,omitempty"`      // buy|add|hold|trim|sell|watch
	PriceTarget *float64 `json:"priceTarget,omitempty"` // optional
	Timeframe   string   `json:"timeframe,omitempty"`
}

// SectorView is a stance on a sector.
type SectorView struct {
	Sector string `json:"sector"`
	Stance string `json:"stance"`
	Note   string `json:"note,omitempty"`
}

// Rotation is a described capital rotation (out of X, into Y).
type Rotation struct {
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	Note string `json:"note"`
}

// Macro is a video's macro / sector commentary.
type Macro struct {
	MacroSummary string       `json:"macroSummary"`
	Sentiment    string       `json:"sentiment,omitempty"` // risk_on|neutral|risk_off
	SectorViews  []SectorView `json:"sectorViews"`
	Rotations    []Rotation   `json:"rotations"`
}

// VideoAnalysis is the structured LLM output for a single video.
type VideoAnalysis struct {
	Summary  string    `json:"summary"`
	Mentions []Mention `json:"mentions"`
	Macro    *Macro    `json:"macro,omitempty"`
}

// ── Aggregation / digest wire shapes (from Convex /influencer/aggregate) ──────

type Thesis struct {
	InfluencerID string `json:"influencerId"`
	Stance       string `json:"stance"`
	Thesis       string `json:"thesis"`
}

type RankedSymbol struct {
	Symbol      string   `json:"symbol"`
	CompanyName *string  `json:"companyName"`
	NetScore    float64  `json:"netScore"`
	Bullish     int      `json:"bullish"`
	Bearish     int      `json:"bearish"`
	Neutral     int      `json:"neutral"`
	Mentions    int      `json:"mentions"`
	Consensus   string   `json:"consensus"`
	Theses      []Thesis `json:"theses"`
}

type MacroNote struct {
	InfluencerID string       `json:"influencerId"`
	MacroSummary string       `json:"macroSummary"`
	Sentiment    *string      `json:"sentiment"`
	SectorViews  []SectorView `json:"sectorViews"`
	Rotations    []Rotation   `json:"rotations"`
}

type AggregateResult struct {
	Date           string         `json:"date"`
	WindowDays     int            `json:"windowDays"`
	SymbolCount    int            `json:"symbolCount"`
	BullishLeaders []RankedSymbol `json:"bullishLeaders"`
	BearishLeaders []RankedSymbol `json:"bearishLeaders"`
	MacroNotes     []MacroNote    `json:"macroNotes"`
}

// Leader is a compact symbol ranking entry stored on the digest.
type Leader struct {
	Symbol   string  `json:"symbol"`
	NetScore float64 `json:"netScore"`
	Mentions int     `json:"mentions"`
}

type SectorRotation struct {
	From      string `json:"from,omitempty"`
	To        string `json:"to,omitempty"`
	Rationale string `json:"rationale"`
}

type RecommendedAction struct {
	Symbol    string `json:"symbol,omitempty"`
	Action    string `json:"action"`
	Rationale string `json:"rationale"`
}

// Digest is the LLM-written daily cross-influencer synthesis.
type Digest struct {
	MarketSentiment    string              `json:"marketSentiment"`
	SentimentLabel     string              `json:"sentimentLabel,omitempty"`
	KeyThemes          []string            `json:"keyThemes"`
	SectorRotation     []SectorRotation    `json:"sectorRotation"`
	RecommendedActions []RecommendedAction `json:"recommendedActions"`
}

// ── Validation / sanitation ──────────────────────────────────────────────────
//
// Convex validators are strict unions; the LLM occasionally drifts off-enum or
// emits junk tickers. We normalise here so a bad field never fails the whole
// write.

var (
	stanceSet     = set("bullish", "bearish", "neutral")
	convictionSet = set("low", "medium", "high")
	actionSet     = set("buy", "add", "hold", "trim", "sell", "watch")
	sentimentSet  = set("risk_on", "neutral", "risk_off")
	symbolRe      = regexp.MustCompile(`^[A-Z][A-Z0-9.\-]{0,6}$`)
)

func set(xs ...string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

func norm(s string, allowed map[string]bool, fallback string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if allowed[s] {
		return s
	}
	return fallback
}

func cleanSymbol(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "$")
	if symbolRe.MatchString(s) {
		return s
	}
	return ""
}

// Sanitize normalises a VideoAnalysis in place: drops junk tickers, forces
// enum fields onto their allowed sets, dedupes by symbol, and drops an empty
// macro block. Returns the receiver for chaining.
func (a *VideoAnalysis) Sanitize() *VideoAnalysis {
	seen := map[string]bool{}
	cleaned := a.Mentions[:0]
	for _, m := range a.Mentions {
		sym := cleanSymbol(m.Symbol)
		if sym == "" || seen[sym] {
			continue
		}
		seen[sym] = true
		m.Symbol = sym
		m.Stance = norm(m.Stance, stanceSet, "neutral")
		m.Conviction = norm(m.Conviction, convictionSet, "")
		m.Action = norm(m.Action, actionSet, "")
		m.Thesis = strings.TrimSpace(m.Thesis)
		if m.Thesis == "" {
			m.Thesis = "(no explicit thesis given)"
		}
		cleaned = append(cleaned, m)
	}
	a.Mentions = cleaned
	a.Summary = strings.TrimSpace(a.Summary)

	if a.Macro != nil {
		a.Macro.Sentiment = norm(a.Macro.Sentiment, sentimentSet, "")
		sv := a.Macro.SectorViews[:0]
		for _, s := range a.Macro.SectorViews {
			s.Sector = strings.TrimSpace(s.Sector)
			if s.Sector == "" {
				continue
			}
			s.Stance = norm(s.Stance, stanceSet, "neutral")
			sv = append(sv, s)
		}
		a.Macro.SectorViews = sv
		rot := a.Macro.Rotations[:0]
		for _, r := range a.Macro.Rotations {
			r.Note = strings.TrimSpace(r.Note)
			if r.Note == "" {
				continue
			}
			rot = append(rot, r)
		}
		a.Macro.Rotations = rot
		a.Macro.MacroSummary = strings.TrimSpace(a.Macro.MacroSummary)
		if a.Macro.MacroSummary == "" && len(a.Macro.SectorViews) == 0 && len(a.Macro.Rotations) == 0 {
			a.Macro = nil
		} else if a.Macro.MacroSummary == "" {
			a.Macro.MacroSummary = "(no macro summary)"
		}
	}
	// Convex v.array rejects null — never emit nil slices.
	if a.Mentions == nil {
		a.Mentions = []Mention{}
	}
	if a.Macro != nil {
		if a.Macro.SectorViews == nil {
			a.Macro.SectorViews = []SectorView{}
		}
		if a.Macro.Rotations == nil {
			a.Macro.Rotations = []Rotation{}
		}
	}
	return a
}

// Sanitize normalises a Digest in place.
func (d *Digest) Sanitize() *Digest {
	d.MarketSentiment = strings.TrimSpace(d.MarketSentiment)
	d.SentimentLabel = norm(d.SentimentLabel, sentimentSet, "")

	themes := d.KeyThemes[:0]
	for _, t := range d.KeyThemes {
		if t = strings.TrimSpace(t); t != "" {
			themes = append(themes, t)
		}
	}
	d.KeyThemes = themes

	rot := d.SectorRotation[:0]
	for _, r := range d.SectorRotation {
		r.Rationale = strings.TrimSpace(r.Rationale)
		if r.Rationale == "" {
			continue
		}
		rot = append(rot, r)
	}
	d.SectorRotation = rot

	acts := d.RecommendedActions[:0]
	for _, a := range d.RecommendedActions {
		a.Action = strings.TrimSpace(a.Action)
		a.Rationale = strings.TrimSpace(a.Rationale)
		a.Symbol = cleanSymbol(a.Symbol) // "" if not a ticker — fine, action can be market-wide
		if a.Action == "" {
			continue
		}
		acts = append(acts, a)
	}
	d.RecommendedActions = acts

	// Convex v.array rejects null — never emit nil slices.
	if d.KeyThemes == nil {
		d.KeyThemes = []string{}
	}
	if d.SectorRotation == nil {
		d.SectorRotation = []SectorRotation{}
	}
	if d.RecommendedActions == nil {
		d.RecommendedActions = []RecommendedAction{}
	}
	return d
}
