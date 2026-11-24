package price

import (
	"math"
	"testing"

	"github.com/msidorkevich/mill/internal/budget"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestEstimatePricesEveryBuiltInModelAndAlias(t *testing.T) {
	u := budget.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000, CacheWriteTokens: 1_000_000, CacheReadTokens: 1_000_000}
	cases := []struct {
		model string
		want  float64
	}{
		{"claude-opus-5-5", 4 + 20 + 8 + 0.20},
		{"opus", 4 + 20 + 8 + 0.20},
		{"claude-sonnet-5-5", 2 + 10 + 4 + 0.20},
		{"sonnet", 2 + 10 + 4 + 0.20},
		{"claude-haiku-5-5", 0.10 + 0.50 + 0.20 + 0.01},
		{"haiku", 0.10 + 0.50 + 0.20 + 0.01},
		{"claude-fable-5-1", 10 + 50 + 20 + 0.25},
		{"fable", 10 + 50 + 20 + 0.25},
	}
	for _, c := range cases {
		got, ok := Estimate(c.model, u, nil)
		if !ok || !near(got, c.want) {
			t.Errorf("Estimate(%q) = %v, %v; want %v, true", c.model, got, ok, c.want)
		}
	}
}

func TestEstimatePricesEachTokenKindSeparately(t *testing.T) {
	for _, c := range []struct {
		u    budget.Usage
		want float64
	}{
		{budget.Usage{InputTokens: 500_000}, 2},
		{budget.Usage{OutputTokens: 500_000}, 10},
		{budget.Usage{CacheWriteTokens: 500_000}, 4},
		{budget.Usage{CacheReadTokens: 500_000}, 0.1},
	} {
		got, ok := Estimate("claude-opus-5-5", c.u, nil)
		if !ok || !near(got, c.want) {
			t.Errorf("Estimate(%+v) = %v, %v; want %v", c.u, got, ok, c.want)
		}
	}
}

func TestEstimateResolvesSuffixedModelIDs(t *testing.T) {
	got, ok := Estimate("claude-opus-5-5[1m]", budget.Usage{InputTokens: 1_000_000}, nil)
	if !ok || !near(got, 4) {
		t.Errorf("got %v, %v; want 4, true", got, ok)
	}
}

func TestEstimateHonoursOverridesByAliasOrID(t *testing.T) {
	u := budget.Usage{InputTokens: 1_000_000}
	for _, key := range []string{"opus", "claude-opus-5-5"} {
		for _, model := range []string{"opus", "claude-opus-5-5", "claude-opus-5-5[1m]"} {
			got, ok := Estimate(model, u, map[string]Rates{key: {Input: 99}})
			if !ok || !near(got, 99) {
				t.Errorf("override %q, model %q: got %v, %v; want 99", key, model, got, ok)
			}
		}
	}
	got, _ := Estimate("sonnet", u, map[string]Rates{"opus": {Input: 99}})
	if !near(got, 2) {
		t.Errorf("unrelated override changed sonnet: %v", got)
	}
}

func TestEstimateUnknownModelIsNotOK(t *testing.T) {
	for _, m := range []string{"", "gpt-4", "claude-opus-9"} {
		if got, ok := Estimate(m, budget.Usage{InputTokens: 10}, nil); ok || got != 0 {
			t.Errorf("Estimate(%q) = %v, %v; want 0, false", m, got, ok)
		}
	}
	// An override can supply rates for an otherwise unknown model.
	if got, ok := Estimate("custom", budget.Usage{InputTokens: 1_000_000}, map[string]Rates{"custom": {Input: 1}}); !ok || !near(got, 1) {
		t.Errorf("custom override: %v, %v", got, ok)
	}
}

func TestTokenCostSplitsEightyTwenty(t *testing.T) {
	// opus: 800K input at $4/M + 200K output at $20/M.
	got, ok := TokenCost("opus", 1_000_000, nil)
	if !ok || !near(got, 3.2+4) {
		t.Errorf("TokenCost(opus, 1M) = %v, %v; want 7.2, true", got, ok)
	}
	got, ok = TokenCost("custom", 1_000_000, map[string]Rates{"custom": {Input: 1, Output: 10}})
	if !ok || !near(got, 0.8+2) {
		t.Errorf("TokenCost(custom) = %v, %v; want 2.8, true", got, ok)
	}
	if got, ok := TokenCost("opus", 0, nil); !ok || got != 0 {
		t.Errorf("TokenCost(opus, 0) = %v, %v; want 0, true", got, ok)
	}
}

func TestTokenCostUnknownModelIsNotOK(t *testing.T) {
	if got, ok := TokenCost("gpt-4", 1000, nil); ok || got != 0 {
		t.Errorf("TokenCost(gpt-4) = %v, %v; want 0, false", got, ok)
	}
}
