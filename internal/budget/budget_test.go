package budget

import (
	"strings"
	"testing"
)

func TestTokensCountModes(t *testing.T) {
	u := Usage{InputTokens: 10, OutputTokens: 20, CacheWriteTokens: 30, CacheReadTokens: 1000}
	if got := u.Tokens(CountBillable); got != 60 {
		t.Fatalf("billable = %d, want 60", got)
	}
	if got := u.Tokens(CountAll); got != 1060 {
		t.Fatalf("all = %d, want 1060", got)
	}
}

func TestExceeded(t *testing.T) {
	tests := []struct {
		name   string
		u      Usage
		l      Limits
		c      Count
		want   bool
		reason string
	}{
		{"no limits", Usage{InputTokens: 1e9, CostUSD: 1e6}, Limits{}, CountBillable, false, ""},
		{"under token limit", Usage{InputTokens: 99}, Limits{Tokens: 100}, CountBillable, false, ""},
		{"token limit reached exactly", Usage{InputTokens: 100}, Limits{Tokens: 100}, CountBillable, true, "token limit"},
		{"cache reads ignored when billable", Usage{CacheReadTokens: 500}, Limits{Tokens: 100}, CountBillable, false, ""},
		{"cache reads count when all", Usage{CacheReadTokens: 500}, Limits{Tokens: 100}, CountAll, true, "token limit"},
		{"usd limit", Usage{CostUSD: 2.5}, Limits{USD: 2}, CountBillable, true, "usd limit"},
		{"both set, usd first", Usage{InputTokens: 1, CostUSD: 3}, Limits{USD: 2, Tokens: 1000}, CountBillable, true, "usd limit"},
		{"both set, tokens first", Usage{InputTokens: 1000, CostUSD: 0.1}, Limits{USD: 2, Tokens: 1000}, CountBillable, true, "token limit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := Exceeded(tt.u, tt.l, tt.c)
			if got != tt.want {
				t.Fatalf("Exceeded = %v (%q), want %v", got, reason, tt.want)
			}
			if !strings.Contains(reason, tt.reason) {
				t.Fatalf("reason %q does not contain %q", reason, tt.reason)
			}
		})
	}
}

func TestOverride(t *testing.T) {
	base := Limits{USD: 2, Tokens: 400_000}
	if got := base.Override(Limits{Tokens: 200_000}); got != (Limits{USD: 2, Tokens: 200_000}) {
		t.Fatalf("got %+v", got)
	}
	if got := base.Override(Limits{}); got != base {
		t.Fatalf("empty override changed limits: %+v", got)
	}
}

func TestValidate(t *testing.T) {
	if err := (Limits{USD: -1}).Validate(); err == nil {
		t.Fatal("negative usd accepted")
	}
	if err := (Limits{Tokens: -1}).Validate(); err == nil {
		t.Fatal("negative tokens accepted")
	}
	if err := (Limits{USD: 1, Tokens: 1}).Validate(); err != nil {
		t.Fatalf("valid limits rejected: %v", err)
	}
}

func TestCovers(t *testing.T) {
	tests := []struct {
		name  string
		limit Limits
		spent Usage
		need  Limits
		want  bool
	}{
		{"unlimited covers anything", Limits{}, Usage{CostUSD: 100}, Limits{}, true},
		{"fits", Limits{USD: 8}, Usage{CostUSD: 5}, Limits{USD: 3}, true},
		{"does not fit", Limits{USD: 8}, Usage{CostUSD: 5.5}, Limits{USD: 3}, false},
		{"unlimited need never fits a limited budget", Limits{Tokens: 1000}, Usage{}, Limits{USD: 1}, false},
		{"tokens fit", Limits{Tokens: 1000}, Usage{InputTokens: 400}, Limits{Tokens: 600}, true},
		{"tokens do not fit", Limits{Tokens: 1000}, Usage{InputTokens: 401}, Limits{Tokens: 600}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Covers(tt.limit, tt.spent, tt.need, CountBillable); got != tt.want {
				t.Fatalf("Covers = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestUsageAdd(t *testing.T) {
	a := Usage{InputTokens: 1, OutputTokens: 2, CacheWriteTokens: 3, CacheReadTokens: 4, CostUSD: 0.5}
	got := a.Add(a)
	want := Usage{InputTokens: 2, OutputTokens: 4, CacheWriteTokens: 6, CacheReadTokens: 8, CostUSD: 1}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}
