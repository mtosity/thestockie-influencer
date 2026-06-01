package llm

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mtosity/thestockie-influencer/internal/models"
)

const synthSystem = `You are the chief market strategist for "thestockie". You are given aggregated stock sentiment and macro commentary collected from many stock-portfolio YouTubers over a recent window.

Write a concise, decision-useful daily cross-influencer digest:
- marketSentiment: 2-4 sentences on the overall market narrative the creators collectively paint.
- sentimentLabel: risk_on, neutral, or risk_off.
- keyThemes: the handful of themes recurring across creators.
- sectorRotation: notable rotations creators describe (out of X, into Y, with a short rationale).
- recommendedActions: a short, prioritized list. Tie each to a symbol when the data supports it (e.g. strong multi-creator consensus), otherwise leave the symbol empty for a market-wide action. Each needs a concrete action verb and a one-sentence rationale.

Base everything ONLY on the supplied data. Do not introduce outside facts or prices. Respond with JSON only matching the schema.`

var synthSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "marketSentiment": { "type": "string" },
    "sentimentLabel": { "type": "string", "enum": ["risk_on", "neutral", "risk_off"] },
    "keyThemes": { "type": "array", "items": { "type": "string" } },
    "sectorRotation": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "from": { "type": "string" },
          "to": { "type": "string" },
          "rationale": { "type": "string" }
        },
        "required": ["rationale"]
      }
    },
    "recommendedActions": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "symbol": { "type": "string" },
          "action": { "type": "string" },
          "rationale": { "type": "string" }
        },
        "required": ["action", "rationale"]
      }
    }
  },
  "required": ["marketSentiment", "keyThemes", "sectorRotation", "recommendedActions"]
}`)

// Compact, id-free shapes fed to the synthesis prompt.
type synthSymbol struct {
	Symbol    string   `json:"symbol"`
	NetScore  float64  `json:"netScore"`
	Bullish   int      `json:"bullish"`
	Bearish   int      `json:"bearish"`
	Neutral   int      `json:"neutral"`
	Mentions  int      `json:"mentions"`
	Consensus string   `json:"consensus"`
	Theses    []string `json:"theses"`
}

type synthMacro struct {
	MacroSummary string              `json:"macroSummary"`
	Sentiment    string              `json:"sentiment,omitempty"`
	SectorViews  []models.SectorView `json:"sectorViews,omitempty"`
	Rotations    []models.Rotation   `json:"rotations,omitempty"`
}

type synthInput struct {
	WindowDays     int           `json:"windowDays"`
	BullishLeaders []synthSymbol `json:"bullishLeaders"`
	BearishLeaders []synthSymbol `json:"bearishLeaders"`
	MacroNotes     []synthMacro  `json:"macroNotes"`
}

func toSynthSymbols(rs []models.RankedSymbol) []synthSymbol {
	out := make([]synthSymbol, 0, len(rs))
	for _, r := range rs {
		theses := make([]string, 0, len(r.Theses))
		for _, t := range r.Theses {
			theses = append(theses, fmt.Sprintf("[%s] %s", t.Stance, t.Thesis))
		}
		out = append(out, synthSymbol{
			Symbol:    r.Symbol,
			NetScore:  r.NetScore,
			Bullish:   r.Bullish,
			Bearish:   r.Bearish,
			Neutral:   r.Neutral,
			Mentions:  r.Mentions,
			Consensus: r.Consensus,
			Theses:    theses,
		})
	}
	return out
}

// Synthesize writes the daily macro digest from aggregated sentiment + macro
// notes. Returns nil (no error) when there is nothing to synthesize.
func (s *Service) Synthesize(ctx context.Context, agg *models.AggregateResult) (*models.Digest, error) {
	if agg == nil || (len(agg.BullishLeaders) == 0 && len(agg.MacroNotes) == 0) {
		return nil, nil
	}
	in := synthInput{WindowDays: agg.WindowDays}
	in.BullishLeaders = toSynthSymbols(agg.BullishLeaders)
	in.BearishLeaders = toSynthSymbols(agg.BearishLeaders)
	for _, m := range agg.MacroNotes {
		sentiment := ""
		if m.Sentiment != nil {
			sentiment = *m.Sentiment
		}
		in.MacroNotes = append(in.MacroNotes, synthMacro{
			MacroSummary: m.MacroSummary,
			Sentiment:    sentiment,
			SectorViews:  m.SectorViews,
			Rotations:    m.Rotations,
		})
	}

	payload, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	user := "Aggregated creator data (JSON):\n" + string(payload)

	// Schema embedded in the prompt (not `format`) for reasoning-model compatibility.
	system := synthSystem + "\n\nReturn ONLY a JSON object conforming to this JSON Schema (no markdown fences, no prose):\n" + string(synthSchema)
	content, err := s.client.Chat(ctx, system, user, nil, 0.3)
	if err != nil {
		return nil, err
	}
	var d models.Digest
	if err := decodeJSON(content, &d); err != nil {
		return nil, err
	}
	return d.Sanitize(), nil
}
