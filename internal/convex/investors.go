package convex

import (
	"context"
	"net/http"

	"github.com/mtosity/thestockie-influencer/internal/models"
)

// ── Super-investor (13F) endpoints ───────────────────────────────────────────

type InvestorRef struct {
	ID             string `json:"id"`
	Cik            string `json:"cik"`
	Name           string `json:"name"`
	Slug           string `json:"slug"`
	LastFilingDate int64  `json:"lastFilingDate"`
	LastPeriod     string `json:"lastPeriod"`
}

func (c *Client) ListActiveInvestors(ctx context.Context) ([]InvestorRef, error) {
	var out []InvestorRef
	if err := c.do(ctx, http.MethodGet, "/investor/active", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

type seedInvestorReq struct {
	Name   string `json:"name"`
	Firm   string `json:"firm"`
	Style  string `json:"style,omitempty"`
	Why    string `json:"why,omitempty"`
	Cik    string `json:"cik"`
	Slug   string `json:"slug"`
	Avatar string `json:"avatar,omitempty"`
	Active bool   `json:"active"`
}

func (c *Client) SeedInvestor(ctx context.Context, inv models.SuperInvestor) error {
	return c.do(ctx, http.MethodPost, "/investor/seed", seedInvestorReq{
		Name: inv.Name, Firm: inv.Firm, Style: inv.Style, Why: inv.Why,
		Cik: inv.Cik, Slug: inv.Slug, Avatar: inv.Avatar, Active: true,
	}, nil)
}

type CusipCacheEntry struct {
	Ticker *string `json:"ticker"`
	Name   *string `json:"name"`
}

// cusipBatch keeps each cusip call under Convex's per-function limits
// (4096 reads, 1024 returned object fields, 8192 array items).
const cusipBatch = 500

func (c *Client) CusipLookup(ctx context.Context, cusips []string) (map[string]CusipCacheEntry, error) {
	out := map[string]CusipCacheEntry{}
	for start := 0; start < len(cusips); start += cusipBatch {
		end := min(start+cusipBatch, len(cusips))
		part := map[string]CusipCacheEntry{}
		if err := c.do(ctx, http.MethodPost, "/investor/cusip/lookup", map[string][]string{"cusips": cusips[start:end]}, &part); err != nil {
			return out, err
		}
		for k, v := range part {
			out[k] = v
		}
	}
	return out, nil
}

type CusipEntry struct {
	Cusip  string `json:"cusip"`
	Ticker string `json:"ticker,omitempty"`
	Name   string `json:"name,omitempty"`
}

func (c *Client) CusipSave(ctx context.Context, entries []CusipEntry) error {
	for start := 0; start < len(entries); start += cusipBatch {
		end := min(start+cusipBatch, len(entries))
		if err := c.do(ctx, http.MethodPost, "/investor/cusip/save", map[string]any{"entries": entries[start:end]}, nil); err != nil {
			return err
		}
	}
	return nil
}

type SaveFilingReq struct {
	Cik        string            `json:"cik"`
	Period     string            `json:"period"`
	ReportDate int64             `json:"reportDate"`
	FilingDate int64             `json:"filingDate"`
	TotalValue float64           `json:"totalValue"`
	Positions  []models.Position `json:"positions"`
}

func (c *Client) SaveFiling(ctx context.Context, req SaveFilingReq) error {
	return c.do(ctx, http.MethodPost, "/investor/filing", req, nil)
}

func (c *Client) AggregateConsensus(ctx context.Context, period string) error {
	return c.do(ctx, http.MethodPost, "/investor/aggregate", map[string]string{"period": period}, nil)
}
