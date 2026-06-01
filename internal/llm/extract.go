package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/mtosity/thestockie-influencer/internal/models"
)

// Service performs the LLM steps: per-video extraction and daily synthesis.
type Service struct {
	client             *Ollama
	maxTranscriptChars int
	log                *slog.Logger
}

func New(client *Ollama, maxTranscriptChars int, log *slog.Logger) *Service {
	if maxTranscriptChars <= 0 {
		maxTranscriptChars = 100_000
	}
	return &Service{client: client, maxTranscriptChars: maxTranscriptChars, log: log}
}

const extractSystem = `You are a meticulous equity research analyst. You read a transcript of a stock-market YouTube video and extract, strictly and faithfully, the creator's views.

Rules:
- Only include companies the creator actually discusses as investments. Use official, mostly US-listed ticker symbols in uppercase (e.g. NVDA, not "Nvidia"). Skip indexes/ETFs unless the creator treats them as a position.
- For each stock: stance is bullish, bearish, or neutral from the creator's point of view; thesis is 1-3 sentences explaining WHY, in your own words but faithful to theirs; conviction reflects how strongly they express it; action is what they say they are doing or recommend.
- Capture macro/market commentary (rates, inflation, growth, risk appetite) and any sector views or capital rotations (out of X, into Y).
- If the video has no stock or macro content, return empty arrays and an empty macroSummary.
- Respond with JSON only, matching the provided schema. Never invent data.`

var extractSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "summary": { "type": "string" },
    "mentions": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "symbol": { "type": "string" },
          "companyName": { "type": "string" },
          "stance": { "type": "string", "enum": ["bullish", "bearish", "neutral"] },
          "conviction": { "type": "string", "enum": ["low", "medium", "high"] },
          "thesis": { "type": "string" },
          "action": { "type": "string", "enum": ["buy", "add", "hold", "trim", "sell", "watch"] },
          "priceTarget": { "type": "number" },
          "timeframe": { "type": "string" }
        },
        "required": ["symbol", "stance", "thesis"]
      }
    },
    "macro": {
      "type": "object",
      "properties": {
        "macroSummary": { "type": "string" },
        "sentiment": { "type": "string", "enum": ["risk_on", "neutral", "risk_off"] },
        "sectorViews": {
          "type": "array",
          "items": {
            "type": "object",
            "properties": {
              "sector": { "type": "string" },
              "stance": { "type": "string", "enum": ["bullish", "bearish", "neutral"] },
              "note": { "type": "string" }
            },
            "required": ["sector", "stance"]
          }
        },
        "rotations": {
          "type": "array",
          "items": {
            "type": "object",
            "properties": {
              "from": { "type": "string" },
              "to": { "type": "string" },
              "note": { "type": "string" }
            },
            "required": ["note"]
          }
        }
      },
      "required": ["macroSummary", "sectorViews", "rotations"]
    }
  },
  "required": ["summary", "mentions", "macro"]
}`)

// ExtractVideo runs structured extraction over a single transcript.
func (s *Service) ExtractVideo(ctx context.Context, influencerName, title string, publishedMs int64, transcript string) (*models.VideoAnalysis, error) {
	if len(transcript) > s.maxTranscriptChars {
		transcript = transcript[:s.maxTranscriptChars] + "\n[transcript truncated]"
	}
	published := ""
	if publishedMs > 0 {
		published = time.UnixMilli(publishedMs).UTC().Format("2006-01-02")
	}
	user := fmt.Sprintf("Influencer: %s\nVideo title: %s\nPublished: %s\n\nTranscript:\n\"\"\"\n%s\n\"\"\"",
		influencerName, title, published, transcript)

	// We embed the schema in the prompt and parse defensively rather than using
	// Ollama's `format` structured-output constraint: reasoning models (e.g.
	// deepseek) emit their answer into the separate `thinking` field and return
	// empty `content` when format-constrained.
	system := extractSystem + "\n\nReturn ONLY a JSON object conforming to this JSON Schema (no markdown fences, no prose):\n" + string(extractSchema)
	content, err := s.client.Chat(ctx, system, user, nil, 0.2)
	if err != nil {
		return nil, err
	}
	var va models.VideoAnalysis
	if err := decodeJSON(content, &va); err != nil {
		return nil, err
	}
	return va.Sanitize(), nil
}
