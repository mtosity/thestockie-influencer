package models

// SuperInvestor is a tracked 13F filer (config + Convex seed shape).
type SuperInvestor struct {
	Name   string `json:"name"`
	Firm   string `json:"firm"`
	Style  string `json:"style,omitempty"`
	Why    string `json:"why,omitempty"`
	Cik    string `json:"cik"`
	Slug   string `json:"slug"`
	Avatar string `json:"avatar,omitempty"`
}

// Position is one holding with its quarter-over-quarter move classification.
type Position struct {
	Cusip        string   `json:"cusip"`
	Ticker       string   `json:"ticker,omitempty"`
	Name         string   `json:"name"`
	Shares       float64  `json:"shares"`
	Value        float64  `json:"value"`
	PctPortfolio float64  `json:"pctPortfolio"`
	ChangeType   string   `json:"changeType"` // new | added | reduced | sold | hold
	ChangePct    *float64 `json:"changePct,omitempty"`
	PrevShares   *float64 `json:"prevShares,omitempty"`
	IsOption     bool     `json:"isOption"`
}
