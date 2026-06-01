// Package figi maps CUSIPs to tickers via the free OpenFIGI API.
package figi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const endpoint = "https://api.openfigi.com/v3/mapping"

type Client struct {
	HTTP   *http.Client
	APIKey string // optional; higher rate limits when set
}

type Result struct {
	Ticker string
	Name   string
}

type mapResp struct {
	Data []struct {
		Ticker   string `json:"ticker"`
		Name     string `json:"name"`
		ExchCode string `json:"exchCode"`
	} `json:"data"`
	Warning string `json:"warning"`
}

// Map resolves CUSIP→ticker for the given cusips (deduped by caller). Returns a
// map keyed by CUSIP; unresolved CUSIPs are simply absent.
func (c *Client) Map(ctx context.Context, cusips []string) (map[string]Result, error) {
	out := map[string]Result{}
	batch := 10
	pause := 1500 * time.Millisecond // unauth: ~25 req/min
	if c.APIKey != "" {
		batch = 100
		pause = 300 * time.Millisecond
	}
	for i := 0; i < len(cusips); i += batch {
		end := i + batch
		if end > len(cusips) {
			end = len(cusips)
		}
		chunk := cusips[i:end]
		results, err := c.mapChunk(ctx, chunk)
		if err != nil {
			return out, err
		}
		for j, r := range results {
			if t := pickTicker(r); t != "" {
				out[chunk[j]] = Result{Ticker: t, Name: pickName(r)}
			}
		}
		if end < len(cusips) {
			select {
			case <-ctx.Done():
				return out, ctx.Err()
			case <-time.After(pause):
			}
		}
	}
	return out, nil
}

func (c *Client) mapChunk(ctx context.Context, cusips []string) ([]mapResp, error) {
	jobs := make([]map[string]string, 0, len(cusips))
	for _, c := range cusips {
		jobs = append(jobs, map[string]string{"idType": "ID_CUSIP", "idValue": c})
	}
	buf, _ := json.Marshal(jobs)

	for attempt := 1; attempt <= 4; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(buf))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		if c.APIKey != "" {
			req.Header.Set("X-OPENFIGI-APIKEY", c.APIKey)
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return nil, err
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 6 * time.Second):
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("openfigi: status %d: %s", resp.StatusCode, string(body[:min(len(body), 200)]))
		}
		var out []mapResp
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, err
		}
		return out, nil
	}
	return nil, fmt.Errorf("openfigi: rate-limited after retries")
}

func pickTicker(r mapResp) string {
	// Prefer a US listing.
	for _, d := range r.Data {
		if d.ExchCode == "US" && d.Ticker != "" {
			return d.Ticker
		}
	}
	if len(r.Data) > 0 {
		return r.Data[0].Ticker
	}
	return ""
}

func pickName(r mapResp) string {
	if len(r.Data) > 0 {
		return r.Data[0].Name
	}
	return ""
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
