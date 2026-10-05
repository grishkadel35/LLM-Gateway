package pricing

import (
	"testing"
	"time"

	"github.com/grishkadel/llm-gateway/internal/usage"
)

var oct2026 = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

// Each expected cost below is worked out by hand from the provider's
// published per-million-token prices: tokens × $/MTok, in micro-dollars.
func TestCost(t *testing.T) {
	cases := []struct {
		name      string
		provider  string
		u         usage.Usage
		requested string
		want      int64
	}{
		{
			// Opus 5.5: $4 in, $5 5m write, $8 1h write, $0.20 read, $20 out.
			// 1000×4 + 2000×0.2 + 500×5 + 100×8 + 300×20 = 13,700 micro-$.
			"anthropic, every token kind", "anthropic",
			usage.Usage{Model: "claude-opus-5-5", Input: 1000, CachedInput: 2000, CacheWrite: 500, CacheWrite1h: 100, Output: 300},
			"", 13_700,
		},
		{
			// gpt-6-sol: $2 in, $0.20 cached, $2.50 cache write, $10 out.
			// 20,000×2 + 10,000×0.2 + 4,000×2.5 + 1,000×10 = 62,000.
			"openai with cache writes", "openai",
			usage.Usage{Model: "gpt-6-sol", Input: 20_000, CachedInput: 10_000, CacheWrite: 4_000, Output: 1_000},
			"", 62_000,
		},
		{
			// gemini-3.8-flash until 2027: $0.75 in, $0.075 read, $3.75 out.
			// 15 + 5 cached + 17 out: 15×0.75 + 5×0.075 + 17×3.75 = 75.375 → 75.
			"gemini, mock numbers", "gemini",
			usage.Usage{Model: "gemini-3.8-flash", Input: 15, CachedInput: 5, Output: 17},
			"", 75,
		},
		{
			// gpt-oss-120b: $0.15 in, $0.60 out; no cached price listed, so
			// cached input costs the input rate. 1,000×0.15 + 1,000×0.15 +
			// 500×0.6 = 600.
			"groq", "groq",
			usage.Usage{Model: "openai/gpt-oss-120b", Input: 1_000, CachedInput: 1_000, Output: 500},
			"", 600,
		},
		{
			// A dated snapshot resolves to its alias's price: gpt-4o is
			// $2.50 in, $10 out. 1,000×2.5 + 100×10 = 3,500.
			"dated snapshot", "openai",
			usage.Usage{Model: "gpt-4o-2024-08-06", Input: 1_000, Output: 100},
			"", 3_500,
		},
		{
			// ...unless the snapshot has its own price: gpt-4o-2024-05-13
			// is $5 in, $15 out. 1,000×5 + 100×15 = 6,500.
			"snapshot with its own price", "openai",
			usage.Usage{Model: "gpt-4o-2024-05-13", Input: 1_000, Output: 100},
			"", 6_500,
		},
		{
			// Haiku 4.5's ID is dated; the date is stripped. $1 in, $5 out.
			"anthropic dated ID", "anthropic",
			usage.Usage{Model: "claude-haiku-4-5-20251001", Input: 1_000, Output: 100},
			"", 1_500,
		},
		{
			// The response names a model the table doesn't know; the
			// requested alias is priced instead. Sonnet 5.5: $2 in, $10 out.
			"falls back to the requested model", "anthropic",
			usage.Usage{Model: "claude-sonnet-5-5-experimental", Input: 1_000, Output: 100},
			"claude-sonnet-5-5", 3_000,
		},
		{
			// Above 272K input tokens, gpt-6-sol's long-context prices apply
			// to the whole request: $4 in, $15 out. 300,000×4 + 1,000×15 =
			// 1,215,000.
			"openai long context", "openai",
			usage.Usage{Model: "gpt-6-sol", Input: 300_000, Output: 1_000},
			"", 1_215_000,
		},
		{
			// Exactly at the threshold is still short context: $2, $10.
			"openai at the threshold", "openai",
			usage.Usage{Model: "gpt-6-sol", Input: 272_000, Output: 1_000},
			"", 554_000,
		},
		{
			// The threshold counts every input token, cached ones included:
			// 100K + 200K cached is long context. 100,000×4 + 200,000×0.4 =
			// 480,000.
			"long context counts cached input", "openai",
			usage.Usage{Model: "gpt-6-sol", Input: 100_000, CachedInput: 200_000},
			"", 480_000,
		},
		{
			// Rounds half up: 1 token × $0.50/MTok = 0.5 micro-$ → 1.
			"rounding", "gemini",
			usage.Usage{Model: "gemini-3-flash-preview", Input: 1},
			"", 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Cost(tc.provider, tc.u, tc.requested, oct2026)
			if !ok {
				t.Fatalf("Cost() found no price for %s/%s", tc.provider, tc.u.Model)
			}
			if got != tc.want {
				t.Errorf("Cost() = %d micro-$, want %d", got, tc.want)
			}
		})
	}
}

// Gemini 3.8 Flash's price doubles on 1 January 2027 (UTC); the price in
// force when the request was served applies.
func TestCostUsesPriceInForce(t *testing.T) {
	u := usage.Usage{Model: "gemini-3.8-flash", Input: 1_000_000, Output: 1_000_000}

	before, _ := Cost("gemini", u, "", time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC))
	after, _ := Cost("gemini", u, "", time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))

	if before != 4_500_000 {
		t.Errorf("cost on 31 Dec 2026 = %d, want $0.75 + $3.75 = 4500000", before)
	}
	if after != 9_000_000 {
		t.Errorf("cost on 1 Jan 2027 = %d, want $1.50 + $7.50 = 9000000", after)
	}
}

func TestCostUnknown(t *testing.T) {
	cases := []struct{ provider, model, requested string }{
		// Groq lists Llama as "contact sales": no public price to use.
		{"groq", "llama-3.3-70b-versatile", "llama-3.3-70b-versatile"},
		{"openai", "gpt-9", ""},
		// Prices are per provider: the same name elsewhere isn't priced.
		{"groq", "gpt-4o", ""},
		{"my-renamed-openai", "gpt-4o", ""},
	}
	for _, tc := range cases {
		if got, ok := Cost(tc.provider, usage.Usage{Model: tc.model, Input: 10}, tc.requested, oct2026); ok {
			t.Errorf("Cost(%s, %s) = %d, true; want not priced", tc.provider, tc.model, got)
		}
	}
}

// TestRatesAreComplete guards the table against a typo leaving a price at
// zero, which would bill those tokens as free.
func TestRatesAreComplete(t *testing.T) {
	for _, r := range rates {
		prices := []Price{r.Price}
		if r.LongContextAbove > 0 {
			prices = append(prices, r.LongContext)
		}
		for _, p := range prices {
			if p.Input <= 0 || p.Output <= 0 || p.CacheRead <= 0 || p.CacheWrite <= 0 || p.CacheWrite1h <= 0 {
				t.Errorf("%s/%s (from %v): a price is zero: %+v", r.Provider, r.Model, r.From, p)
			}
			if p.CacheRead > p.Input {
				t.Errorf("%s/%s: cache reads cost more than input: %+v", r.Provider, r.Model, p)
			}
		}
	}
}

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"gpt-4o-2024-08-06":         "gpt-4o",
		"claude-sonnet-4-20250514":  "claude-sonnet-4",
		"claude-haiku-4-5-20251001": "claude-haiku-4-5",
		"gemini-3.8-flash":          "gemini-3.8-flash",
		"gpt-4.1-mini":              "gpt-4.1-mini",
		"openai/gpt-oss-120b":       "openai/gpt-oss-120b",
	}
	for in, want := range cases {
		if got := normalize(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
}
