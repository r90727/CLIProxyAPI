package localusage

import (
	"regexp"
	"strings"
)

// Rates are USD per million tokens, verified against the linked provider pages.
// These are standard, short-context rates, not historical invoices or plan fees.
const pricingChecked = "2026-09-18"

type APIRate struct {
	Input      float64 `json:"input"`
	Cached     float64 `json:"cached"`
	CacheWrite float64 `json:"cache_write"`
	Output     float64 `json:"output"`
}

var apiRates = map[string]APIRate{
	"gpt-6-astra":       {10, 1, 12.5, 50},
	"gpt-5.6-sol":       {4, .4, 5, 20},
	"gpt-5.6-terra":     {2, .2, 2.5, 12},
	"gpt-5.6-luna":      {.2, .02, .25, 1.2},
	"claude-fable-5-1":  {10, .25, 12.5, 50},
	"claude-mythos-5-1": {10, .25, 12.5, 50},
	"claude-fable-5":    {10, 1, 12.5, 50},
	"claude-mythos-5":   {10, 1, 12.5, 50},
	"claude-opus-5":     {5, .5, 6.25, 25},
	"claude-opus-4-8":   {5, .5, 6.25, 25},
	"claude-opus-4-7":   {5, .5, 6.25, 25},
	"claude-opus-4-6":   {5, .5, 6.25, 25},
	"claude-opus-4-5":   {5, .5, 6.25, 25},
	"claude-opus-4-1":   {15, 1.5, 18.75, 75},
	"claude-opus-4":     {15, 1.5, 18.75, 75},
	"claude-sonnet-5":   {2, .2, 2.5, 10},
	"claude-sonnet-4-6": {3, .3, 3.75, 15},
	"claude-sonnet-4-5": {3, .3, 3.75, 15},
	"claude-sonnet-4":   {3, .3, 3.75, 15},
	"claude-haiku-4-5":  {1, .1, 1.25, 5},
	"claude-haiku-3-5":  {.8, .08, 1, 4},
}

var modelDateSuffix = regexp.MustCompile(`-(?:\d{8}|\d{4}-\d{2}-\d{2})$`)

func rateFor(model string) (APIRate, bool) {
	name := modelDateSuffix.ReplaceAllString(strings.ToLower(strings.TrimSpace(model)), "")
	rate, ok := apiRates[name]
	return rate, ok
}

type CostEstimate struct {
	USD            float64 `json:"usd"`
	InputUSD       float64 `json:"input_usd"`
	OutputUSD      float64 `json:"output_usd"`
	CacheReadUSD   float64 `json:"cache_read_usd"`
	CacheWriteUSD  float64 `json:"cache_write_usd"`
	PricedEvents   int64   `json:"priced_events"`
	UnpricedEvents int64   `json:"unpriced_events"`
}

func estimateCost(model string, v Totals) CostEstimate {
	r, ok := rateFor(model)
	if !ok || v.Cached+v.CacheWrite > v.Input {
		return CostEstimate{UnpricedEvents: v.Events}
	}
	c := CostEstimate{
		InputUSD:      float64(v.Input-v.Cached-v.CacheWrite) * r.Input / 1e6,
		OutputUSD:     float64(v.Output) * r.Output / 1e6,
		CacheReadUSD:  float64(v.Cached) * r.Cached / 1e6,
		CacheWriteUSD: float64(v.CacheWrite) * r.CacheWrite / 1e6,
		PricedEvents:  v.Events,
	}
	// Reasoning is already included in output; cache is already included in input.
	c.USD = c.InputUSD + c.OutputUSD + c.CacheReadUSD + c.CacheWriteUSD
	return c
}

func (c *CostEstimate) add(v CostEstimate) {
	c.USD += v.USD
	c.InputUSD += v.InputUSD
	c.OutputUSD += v.OutputUSD
	c.CacheReadUSD += v.CacheReadUSD
	c.CacheWriteUSD += v.CacheWriteUSD
	c.PricedEvents += v.PricedEvents
	c.UnpricedEvents += v.UnpricedEvents
}
