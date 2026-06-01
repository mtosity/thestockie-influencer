// Package convex is a thin HTTP client for the influencer endpoints exposed by
// thestockie's Convex deployment (convex/http.ts).
package convex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/mtosity/thestockie-influencer/internal/models"
)

type Client struct {
	BaseURL string // .convex.site origin
	Secret  string
	HTTP    *http.Client
}

func New(baseURL, secret string, hc *http.Client) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Secret: secret, HTTP: hc}
}

type envelope struct {
	OK     bool            `json:"ok"`
	Error  string          `json:"error"`
	Result json.RawMessage `json:"result"`
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Secret)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))

	var env envelope
	_ = json.Unmarshal(raw, &env)

	if resp.StatusCode != http.StatusOK {
		msg := env.Error
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
		}
		return fmt.Errorf("convex %s %s: status %d: %s", method, path, resp.StatusCode, msg)
	}
	if !env.OK {
		return fmt.Errorf("convex %s %s: %s", method, path, env.Error)
	}
	if out != nil && len(env.Result) > 0 && string(env.Result) != "null" {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return fmt.Errorf("convex %s %s: decode result: %w", method, path, err)
		}
	}
	return nil
}

// ── Influencers ──────────────────────────────────────────────────────────────

func (c *Client) ListActive(ctx context.Context) ([]models.Influencer, error) {
	var out []models.Influencer
	if err := c.do(ctx, http.MethodGet, "/influencer/active", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

type seedReq struct {
	Name        string `json:"name"`
	ChannelID   string `json:"channelId"`
	Handle      string `json:"handle,omitempty"`
	YouTubeURL  string `json:"youtubeUrl,omitempty"`
	Avatar      string `json:"avatar,omitempty"`
	Description string `json:"description,omitempty"`
	Active      bool   `json:"active"`
}

func (c *Client) Seed(ctx context.Context, inf models.Influencer) error {
	return c.do(ctx, http.MethodPost, "/influencer/seed", seedReq{
		Name:        inf.Name,
		ChannelID:   inf.ChannelID,
		Handle:      inf.Handle,
		YouTubeURL:  inf.YouTubeURL,
		Avatar:      inf.Avatar,
		Description: inf.Description,
		Active:      true,
	}, nil)
}

// ── Videos ───────────────────────────────────────────────────────────────────

type discoverReq struct {
	ChannelID   string `json:"channelId"`
	VideoID     string `json:"videoId"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	PublishedAt int64  `json:"publishedAt"`
}

type DiscoverResult struct {
	ID           string `json:"id"`
	InfluencerID string `json:"influencerId"`
	IsNew        bool   `json:"isNew"`
	Status       string `json:"status"`
}

func (c *Client) Discover(ctx context.Context, cand models.VideoCandidate) (*DiscoverResult, error) {
	var out DiscoverResult
	err := c.do(ctx, http.MethodPost, "/influencer/video/discover", discoverReq{
		ChannelID:   cand.ChannelID,
		VideoID:     cand.VideoID,
		Title:       cand.Title,
		URL:         cand.URL,
		PublishedAt: cand.PublishedAt,
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) SetStatus(ctx context.Context, videoID, status string) error {
	return c.do(ctx, http.MethodPost, "/influencer/video/status",
		map[string]string{"videoId": videoID, "status": status}, nil)
}

func (c *Client) MarkError(ctx context.Context, videoID, msg string) error {
	return c.do(ctx, http.MethodPost, "/influencer/video/error",
		map[string]string{"videoId": videoID, "error": msg}, nil)
}

type SaveResultReq struct {
	VideoID    string           `json:"videoId"`
	Transcript string           `json:"transcript,omitempty"`
	Summary    string           `json:"summary,omitempty"`
	Mentions   []models.Mention `json:"mentions"`
	Macro      *models.Macro    `json:"macro,omitempty"`
}

func (c *Client) SaveResult(ctx context.Context, req SaveResultReq) error {
	if req.Mentions == nil {
		req.Mentions = []models.Mention{}
	}
	return c.do(ctx, http.MethodPost, "/influencer/video/result", req, nil)
}

// ── Aggregation / digest ─────────────────────────────────────────────────────

func (c *Client) Aggregate(ctx context.Context, date string, windowDays int) (*models.AggregateResult, error) {
	var out models.AggregateResult
	err := c.do(ctx, http.MethodPost, "/influencer/aggregate",
		map[string]any{"date": date, "windowDays": windowDays}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

type SaveDigestReq struct {
	Date               string                     `json:"date"`
	MarketSentiment    string                     `json:"marketSentiment"`
	SentimentLabel     string                     `json:"sentimentLabel,omitempty"`
	KeyThemes          []string                   `json:"keyThemes"`
	SectorRotation     []models.SectorRotation    `json:"sectorRotation"`
	BullishLeaders     []models.Leader            `json:"bullishLeaders"`
	BearishLeaders     []models.Leader            `json:"bearishLeaders"`
	RecommendedActions []models.RecommendedAction `json:"recommendedActions"`
	VideosAnalyzed     int                        `json:"videosAnalyzed"`
	InfluencersCount   int                        `json:"influencersCount"`
	WindowDays         int                        `json:"windowDays"`
}

func (c *Client) SaveDigest(ctx context.Context, req SaveDigestReq) error {
	return c.do(ctx, http.MethodPost, "/influencer/digest", req, nil)
}

// ── Retention ────────────────────────────────────────────────────────────────

type PurgeResult struct {
	Videos    int `json:"videos"`
	Mentions  int `json:"mentions"`
	Macros    int `json:"macros"`
	Sentiment int `json:"sentiment"`
	Digests   int `json:"digests"`
}

func (c *Client) Purge(ctx context.Context, olderThanDays int) (*PurgeResult, error) {
	var out PurgeResult
	err := c.do(ctx, http.MethodPost, "/influencer/purge",
		map[string]int{"olderThanDays": olderThanDays}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ── Run bookkeeping ──────────────────────────────────────────────────────────

func (c *Client) StartRun(ctx context.Context, mode string) (string, error) {
	var runID string
	err := c.do(ctx, http.MethodPost, "/influencer/run/start",
		map[string]string{"mode": mode}, &runID)
	return runID, err
}

type FinishRunReq struct {
	RunID            string `json:"runId"`
	Status           string `json:"status"`
	VideosDiscovered int    `json:"videosDiscovered"`
	VideosProcessed  int    `json:"videosProcessed"`
	VideosErrored    int    `json:"videosErrored"`
	Error            string `json:"error,omitempty"`
}

func (c *Client) FinishRun(ctx context.Context, req FinishRunReq) error {
	return c.do(ctx, http.MethodPost, "/influencer/run/finish", req, nil)
}
