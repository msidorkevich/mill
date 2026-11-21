// Package price estimates the USD cost of agent token usage.
//
// claude reports cost only in its final result event, so a run that ends without one has
// known tokens but no cost. Estimate prices those tokens from a built-in rate table, which
// config can override.
package price

import (
	"strings"

	"mill/internal/budget"
)

// Rates are prices in USD per million tokens.
type Rates struct {
	Input        float64 `toml:"input"`
	Output       float64 `toml:"output"`
	CacheWrite5m float64 `toml:"cache_write_5m"`
	CacheWrite1h float64 `toml:"cache_write_1h"`
	CacheRead    float64 `toml:"cache_read"`
}

// table holds the built-in rates (prices as of 2026-10-06).
var table = map[string]Rates{
	"claude-opus-5-5":   {Input: 4, Output: 20, CacheWrite5m: 5, CacheWrite1h: 8, CacheRead: 0.20},
	"claude-sonnet-5-5": {Input: 2, Output: 10, CacheWrite5m: 2.5, CacheWrite1h: 4, CacheRead: 0.20},
	"claude-haiku-5-5":  {Input: 0.10, Output: 0.50, CacheWrite5m: 0.125, CacheWrite1h: 0.20, CacheRead: 0.01},
	"claude-fable-5-1":  {Input: 10, Output: 50, CacheWrite5m: 12.5, CacheWrite1h: 20, CacheRead: 0.25},
}

var aliases = map[string]string{
	"opus":   "claude-opus-5-5",
	"sonnet": "claude-sonnet-5-5",
	"haiku":  "claude-haiku-5-5",
	"fable":  "claude-fable-5-1",
}

// baseID strips a suffix such as "[1m]" from a model id.
func baseID(model string) string {
	model = strings.TrimSpace(model)
	if i := strings.IndexByte(model, '['); i >= 0 {
		model = model[:i]
	}
	return model
}

// Lookup returns the rates for a model alias or id. Overrides win and are matched by alias
// or id: an override keyed "opus" applies to "claude-opus-5-5" and the reverse.
func Lookup(model string, overrides map[string]Rates) (Rates, bool) {
	m := baseID(model)
	if m == "" {
		return Rates{}, false
	}
	id := m
	if a, ok := aliases[m]; ok {
		id = a
	}
	for key, r := range overrides {
		k := baseID(key)
		if a, ok := aliases[k]; ok {
			k = a
		}
		if k == id {
			return r, true
		}
	}
	r, ok := table[id]
	return r, ok
}

// Estimate prices u for model. Input, output and cache reads use their rates; cache writes
// use the 1-hour rate, because Claude Code writes 1-hour cache entries (this errs high).
// Haiku's higher rate for prompts above 100K tokens is not modelled. ok is false when the
// model has no known rates.
func Estimate(model string, u budget.Usage, overrides map[string]Rates) (usd float64, ok bool) {
	r, ok := Lookup(model, overrides)
	if !ok {
		return 0, false
	}
	usd = (float64(u.InputTokens)*r.Input +
		float64(u.OutputTokens)*r.Output +
		float64(u.CacheWriteTokens)*r.CacheWrite1h +
		float64(u.CacheReadTokens)*r.CacheRead) / 1e6
	return usd, true
}
