// Package budget defines spending limits (USD and/or tokens) and usage accounting.
//
// A zero value in a limit field means "no limit" for that unit. When both units are
// limited, whichever is reached first counts (BG1).
package budget

import "fmt"

// Count selects which tokens count against a token limit.
type Count string

const (
	// CountBillable counts input, output and cache-write tokens. Default.
	CountBillable Count = "billable"
	// CountAll also counts cache-read tokens.
	CountAll Count = "all"
)

// Valid reports whether c is a known counting mode.
func (c Count) Valid() bool { return c == CountBillable || c == CountAll }

// Limits is a spending limit. Zero fields are unlimited.
type Limits struct {
	USD    float64 `toml:"usd" json:"usd,omitempty"`
	Tokens int64   `toml:"tokens" json:"tokens,omitempty"`
}

// IsZero reports whether no limit is set.
func (l Limits) IsZero() bool { return l.USD == 0 && l.Tokens == 0 }

// Validate rejects negative limits.
func (l Limits) Validate() error {
	if l.USD < 0 {
		return fmt.Errorf("usd must be >= 0, got %v", l.USD)
	}
	if l.Tokens < 0 {
		return fmt.Errorf("tokens must be >= 0, got %d", l.Tokens)
	}
	return nil
}

// Override returns l with every non-zero field of o applied on top.
func (l Limits) Override(o Limits) Limits {
	if o.USD != 0 {
		l.USD = o.USD
	}
	if o.Tokens != 0 {
		l.Tokens = o.Tokens
	}
	return l
}

// Usage is what a worker spent.
type Usage struct {
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

// Add returns the sum of u and o.
func (u Usage) Add(o Usage) Usage {
	return Usage{
		InputTokens:      u.InputTokens + o.InputTokens,
		OutputTokens:     u.OutputTokens + o.OutputTokens,
		CacheWriteTokens: u.CacheWriteTokens + o.CacheWriteTokens,
		CacheReadTokens:  u.CacheReadTokens + o.CacheReadTokens,
		CostUSD:          u.CostUSD + o.CostUSD,
	}
}

// Tokens returns the token total under counting mode c.
func (u Usage) Tokens(c Count) int64 {
	n := u.InputTokens + u.OutputTokens + u.CacheWriteTokens
	if c == CountAll {
		n += u.CacheReadTokens
	}
	return n
}

// Exceeded reports whether u has reached a limit in l, and a reason naming which one.
// Reaching a limit exactly counts as exceeded: the budget is used up.
func Exceeded(u Usage, l Limits, c Count) (bool, string) {
	if l.Tokens > 0 {
		if t := u.Tokens(c); t >= l.Tokens {
			return true, fmt.Sprintf("token limit reached: %d >= %d (%s)", t, l.Tokens, c)
		}
	}
	if l.USD > 0 && u.CostUSD >= l.USD {
		return true, fmt.Sprintf("usd limit reached: %.4f >= %.4f", u.CostUSD, l.USD)
	}
	return false, ""
}

// Covers reports whether the remaining budget (limit minus spent) can cover a request
// for another `need`. An unlimited unit always covers; an unlimited need for a limited
// unit never does (BG2: an attempt starts only if its limit fits the remaining budget).
func Covers(limit Limits, spent Usage, need Limits, c Count) bool {
	if limit.Tokens > 0 {
		if need.Tokens == 0 || spent.Tokens(c)+need.Tokens > limit.Tokens {
			return false
		}
	}
	if limit.USD > 0 {
		if need.USD == 0 || spent.CostUSD+need.USD > limit.USD {
			return false
		}
	}
	return true
}
