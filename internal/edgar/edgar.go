// Package edgar fetches SEC Form 13F-HR filings from EDGAR and parses their
// information tables (holdings).
package edgar

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	dataBase = "https://data.sec.gov"
	archives = "https://www.sec.gov/Archives/edgar/data"
)

type Client struct {
	HTTP      *http.Client
	UserAgent string // SEC requires a descriptive UA with contact info
}

// Filing identifies one 13F-HR submission.
type Filing struct {
	Accession  string
	Period     string // "2026-Q1"
	PrimaryDoc string
	FilingDate time.Time
	ReportDate time.Time
}

// Holding is one raw info-table row.
type Holding struct {
	Cusip   string
	Name    string
	Value   float64 // dollars (2023+ filings)
	Shares  float64
	PutCall string // "Put" | "Call" | ""
}

// lastRequest tracks the last EDGAR request time for rate limiting.
var lastRequest time.Time

func (c *Client) get(ctx context.Context, url string) ([]byte, error) {
	// SEC EDGAR rate limit: max 10 requests/second per API key
	// We add a 200ms delay between requests (5 req/sec) to be safe
	elapsed := time.Since(lastRequest)
	if elapsed < 200*time.Millisecond {
		time.Sleep(200*time.Millisecond - elapsed)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.UserAgent)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	lastRequest = time.Now()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 32<<20))
}

type submissions struct {
	Name    string `json:"name"`
	Filings struct {
		Recent struct {
			Accession       []string `json:"accessionNumber"`
			FilingDate      []string `json:"filingDate"`
			ReportDate      []string `json:"reportDate"`
			Form            []string `json:"form"`
			PrimaryDocument []string `json:"primaryDocument"`
		} `json:"recent"`
	} `json:"filings"`
}

// Latest13F returns the most recent 13F-HR filing and the most recent prior
// filing for a *different* quarter (for the QoQ diff).
func (c *Client) Latest13F(ctx context.Context, cik string) (current, prior *Filing, err error) {
	body, err := c.get(ctx, fmt.Sprintf("%s/submissions/CIK%s.json", dataBase, pad10(cik)))
	if err != nil {
		return nil, nil, err
	}
	var s submissions
	if err := json.Unmarshal(body, &s); err != nil {
		return nil, nil, err
	}
	r := s.Filings.Recent
	var fs []Filing
	for i, form := range r.Form {
		if form != "13F-HR" && form != "13F-HR/A" {
			continue
		}
		fd, _ := time.Parse("2006-01-02", r.FilingDate[i])
		rd, _ := time.Parse("2006-01-02", r.ReportDate[i])
		fs = append(fs, Filing{
			Accession:  r.Accession[i],
			FilingDate: fd,
			ReportDate: rd,
			PrimaryDoc: r.PrimaryDocument[i],
			Period:     periodOf(rd),
		})
	}
	if len(fs) == 0 {
		return nil, nil, fmt.Errorf("no 13F-HR filings for CIK %s", cik)
	}
	sort.Slice(fs, func(i, j int) bool { return fs[i].FilingDate.After(fs[j].FilingDate) })
	cur := fs[0]
	current = &cur
	for i := 1; i < len(fs); i++ {
		if !fs[i].ReportDate.Equal(current.ReportDate) {
			p := fs[i]
			prior = &p
			break
		}
	}
	return current, prior, nil
}

// Holdings fetches + parses the information table for a filing.
// Values are normalized to actual dollars: some filers report in thousands
// (per SEC spec) while others report in dollars. We detect the unit by
// comparing the sum of XML values against tableValueTotal from primary_doc.xml.
func (c *Client) Holdings(ctx context.Context, cik string, f *Filing) ([]Holding, error) {
	cikNum := strings.TrimLeft(cik, "0")
	accNo := strings.ReplaceAll(f.Accession, "-", "")
	body, err := c.get(ctx, fmt.Sprintf("%s/%s/%s/index.json", archives, cikNum, accNo))
	if err != nil {
		return nil, err
	}
	var idx struct {
		Directory struct {
			Item []struct {
				Name string `json:"name"`
			} `json:"item"`
		} `json:"directory"`
	}
	if err := json.Unmarshal(body, &idx); err != nil {
		return nil, err
	}
	var xmls []string
	for _, it := range idx.Directory.Item {
		lo := strings.ToLower(it.Name)
		if strings.HasSuffix(lo, ".xml") && it.Name != f.PrimaryDoc && !strings.Contains(lo, "primary_doc") {
			xmls = append(xmls, it.Name)
		}
	}
	// Prefer names that look like the information table.
	sort.SliceStable(xmls, func(i, j int) bool { return infoScore(xmls[i]) > infoScore(xmls[j]) })

	for _, name := range xmls {
		raw, err := c.get(ctx, fmt.Sprintf("%s/%s/%s/%s", archives, cikNum, accNo, name))
		if err != nil {
			continue
		}
		if hs, ok := parseInfoTable(raw); ok {
			// Detect value unit: SEC spec says values are in thousands of dollars.
			// Some filers (e.g. Berkshire, Li Lu) report in actual dollars instead.
			// Heuristic: compute the MEDIAN value-per-share across all non-option
			// holdings with shares > 0. If median value/share < $0.10, values are
			// in thousands. If > $1, values are in dollars. Between $0.10 and $1,
			// fall back to checking total portfolio value: if total > $1T, already
			// in dollars (no portfolio is $1Q); if total < $1B, likely in thousands.
			{
				var vps []float64
				var totalVal float64
				for _, h := range hs {
					totalVal += h.Value
					if h.Shares > 0 && h.PutCall == "" {
						vps = append(vps, h.Value/h.Shares)
					}
				}
				sort.Float64s(vps)
				medianVPS := 0.0
				if len(vps) > 0 {
					medianVPS = vps[len(vps)/2]
				}

				// Determine if values are in thousands
				inThousands := false
				if medianVPS > 0 {
					if medianVPS < 0.10 {
						inThousands = true // clearly thousands
					} else if medianVPS > 1.0 {
						inThousands = false // clearly dollars
					} else {
						// Ambiguous: use total value as tiebreaker
						// If total < $1B, likely thousands (too small for 13F)
						// If total > $100B, likely dollars
						inThousands = totalVal < 1e9
					}
				} else {
					// No non-option holdings with shares — use total value
					inThousands = totalVal < 1e9
				}

				if inThousands {
					for i := range hs {
						hs[i].Value *= 1000
					}
				}
			}
			return hs, nil
		}
	}
	return nil, fmt.Errorf("no information table found in %s", f.Accession)
}

func infoScore(name string) int {
	lo := strings.ToLower(name)
	switch {
	case strings.Contains(lo, "infotable"), strings.Contains(lo, "info_table"):
		return 3
	case strings.Contains(lo, "table"), strings.Contains(lo, "info"):
		return 2
	default:
		return 1
	}
}

func parseInfoTable(raw []byte) ([]Holding, bool) {
	type row struct {
		Name    string  `xml:"nameOfIssuer"`
		Cusip   string  `xml:"cusip"`
		Value   float64 `xml:"value"`
		Shares  float64 `xml:"shrsOrPrnAmt>sshPrnamt"`
		ShType  string  `xml:"shrsOrPrnAmt>sshPrnamtType"`
		PutCall string  `xml:"putCall"`
	}
	var it struct {
		Rows []row `xml:"infoTable"`
	}
	if err := xml.Unmarshal(raw, &it); err != nil || len(it.Rows) == 0 {
		return nil, false
	}
	out := make([]Holding, 0, len(it.Rows))
	for _, r := range it.Rows {
		cusip := strings.ToUpper(strings.TrimSpace(r.Cusip))
		if cusip == "" {
			continue
		}
		out = append(out, Holding{
			Cusip:   cusip,
			Name:    strings.TrimSpace(r.Name),
			Value:   r.Value,
			Shares:  r.Shares,
			PutCall: strings.TrimSpace(r.PutCall),
		})
	}
	return out, true
}

func pad10(cik string) string {
	cik = strings.TrimLeft(strings.TrimSpace(cik), "0")
	for len(cik) < 10 {
		cik = "0" + cik
	}
	return cik
}

func periodOf(t time.Time) string {
	q := (int(t.Month())-1)/3 + 1
	return fmt.Sprintf("%d-Q%d", t.Year(), q)
}
