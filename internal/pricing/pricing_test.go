package pricing

import (
	"math"
	"math/big"
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

// TestResultCost: a response's cost is known only when its usage is. A cache
// hit costs 0, whatever its usage says, and so does a free provider's
// response, even one cut short. A response cut short, or read to the end
// without its final usage report, has an unknown cost, never 0. A complete one
// with no tokens, such as a provider's error, cost nothing, and any other
// costs what Cost says: by the requested model when the response's isn't
// priced, at the price in force at at, and unknown for a model with no price.
func TestResultCost(t *testing.T) {
	// gpt-4o: $2.50 in, $1.25 cached, $10 out. 15×2.5 + 5×1.25 + 10×10 =
	// 143.75 → 144.
	gpt4o := usage.Usage{Model: "gpt-4o", Input: 15, CachedInput: 5, Output: 10}
	llama := usage.Usage{Model: "llama-3.3-70b-versatile", Input: 10}
	cases := []struct {
		name      string
		provider  string
		free      bool
		r         usage.Result
		requested string
		at        time.Time
		want      int64
		known     bool
	}{
		{"complete", "openai", false, usage.Result{Usage: gpt4o, Complete: true, UsageComplete: true}, "", oct2026, 144, true},
		// Sonnet 5.5: $2 in, $10 out. 1,000×2 + 100×10 = 3,000.
		{"complete, by the requested model", "anthropic", false,
			usage.Result{Usage: usage.Usage{Model: "claude-sonnet-5-5-experimental", Input: 1_000, Output: 100}, Complete: true, UsageComplete: true}, "claude-sonnet-5-5", oct2026, 3_000, true},
		// gemini-3.8-flash from 2027: $1.50 in, $7.50 out.
		{"complete, at the price in force", "gemini", false,
			usage.Result{Usage: usage.Usage{Model: "gemini-3.8-flash", Input: 1_000_000, Output: 1_000_000}, Complete: true, UsageComplete: true}, "", jan2027, 9_000_000, true},
		{"complete, unpriced", "groq", false, usage.Result{Usage: llama, Complete: true, UsageComplete: true}, "", oct2026, 0, false},
		{"complete, no tokens", "openai", false, usage.Result{Usage: usage.Usage{Model: "gpt-4o"}, Status: 429, Complete: true, UsageComplete: true}, "", oct2026, 0, true},
		{"complete, no tokens, unpriced", "groq", false, usage.Result{Usage: usage.Usage{Model: llama.Model}, Status: 500, Complete: true, UsageComplete: true}, "", oct2026, 0, true},
		{"read to the end without its final report", "openai", false, usage.Result{Usage: gpt4o, Status: 200, Complete: true}, "", oct2026, 0, false},
		{"cut off before usage arrived", "openai", false, usage.Result{Usage: usage.Usage{Model: "gpt-4o"}}, "", oct2026, 0, false},
		{"cut off with partial usage", "openai", false, usage.Result{Usage: gpt4o}, "", oct2026, 0, false},
		{"free, complete", "ollama", true, usage.Result{Usage: usage.Usage{Model: "qwen3.5:9b", Input: 15, Output: 10}, Complete: true, UsageComplete: true}, "", oct2026, 0, true},
		{"free, cut off", "ollama", true, usage.Result{Usage: usage.Usage{Model: "qwen3.5:9b", Input: 15}}, "", oct2026, 0, true},
		{"cached", "openai", false, usage.Result{Usage: gpt4o, Complete: true, UsageComplete: true, Cached: true}, "", oct2026, 0, true},
		{"cached, unpriced", "groq", false, usage.Result{Usage: llama, Complete: true, UsageComplete: true, Cached: true}, "", oct2026, 0, true},
		{"cached, cut off", "openai", false, usage.Result{Usage: gpt4o, Cached: true}, "", oct2026, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, known := ResultCost(tc.provider, tc.free, tc.r, tc.requested, tc.at)
			if got != tc.want || known != tc.known {
				t.Errorf("ResultCost() = %d, %t; want %d, %t", got, known, tc.want, tc.known)
			}
		})
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

// TestRatesHaveMaxOutput guards the table against a model with no maximum
// output, which would reserve nothing for a request's output.
func TestRatesHaveMaxOutput(t *testing.T) {
	for _, r := range rates {
		if r.MaxOutput <= 0 {
			t.Errorf("%s/%s (from %v): MaxOutput is %d", r.Provider, r.Model, r.From, r.MaxOutput)
		}
	}
}

// TestName: a model's name in the table is the first of the names given that
// the provider prices, as given or without its date: the rate Cost prices
// those names by. A snapshot with a rate of its own keeps its name.
func TestName(t *testing.T) {
	cases := []struct {
		provider string
		models   []string
		want     string // "" when not priced
	}{
		{"openai", []string{"gpt-4o"}, "gpt-4o"},
		{"openai", []string{"gpt-4o-2024-08-06"}, "gpt-4o"},
		{"openai", []string{"gpt-4o-2024-05-13"}, "gpt-4o-2024-05-13"},
		{"anthropic", []string{"claude-sonnet-4-20250514"}, "claude-sonnet-4"},
		// The response's model first, then the requested one.
		{"openai", []string{"gpt-4o-mini", "gpt-4o"}, "gpt-4o-mini"},
		{"openai", []string{"gpt-9", "gpt-4o-2024-08-06"}, "gpt-4o"},
		{"openai", []string{"", "gpt-4o"}, "gpt-4o"},
		{"groq", []string{"llama-3.3-70b-versatile"}, ""},
		{"openai", []string{"gpt-9", ""}, ""},
		{"openai", nil, ""},
		// Prices are per provider, and a free one has none.
		{"groq", []string{"gpt-4o"}, ""},
		{"ollama", []string{"qwen3.5:9b"}, ""},
	}
	for _, tc := range cases {
		got, ok := Name(tc.provider, tc.models...)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("Name(%s, %q) = %q, %v; want %q", tc.provider, tc.models, got, ok, tc.want)
		}
		// The name is the model of the rate lookup finds, whenever one is in
		// force.
		if r, priced := lookup(tc.provider, oct2026, tc.models...); priced && r.Model != got {
			t.Errorf("Name(%s, %q) = %q, but the rate in force is %s's", tc.provider, tc.models, got, r.Model)
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

// A reservation is the most a request could cost, so its output is the
// allowance, not a response's actual output. Each expected value below is
// worked out by hand like TestCost's: tokens × $/MTok, in micro-dollars.
func TestReserve(t *testing.T) {
	cases := []struct {
		name      string
		provider  string
		model     string
		input     int64
		outputCap int64
		choices   int64
		want      int64
	}{
		{
			// gpt-5.6-sol: $4 in, $5 cache write, $20 out. Input is priced at
			// the write rate, the dearest: 10,000×5 + 1,000×20 = 70,000
			// (60,000 at the input rate).
			"openai, cache write dearer than input", "openai", "gpt-5.6-sol", 10_000, 1_000, 1, 70_000,
		},
		{
			// Opus 5.5: $4 in, $5 5m write, $8 1h write, $20 out. Input is
			// priced at the 1-hour write rate: 10,000×8 + 1,000×20 = 100,000.
			"anthropic, 1-hour cache write dearest", "anthropic", "claude-opus-5-5", 10_000, 1_000, 1, 100_000,
		},
		{
			// gpt-6-sol: $2 in, $2.50 write, $10 out. At exactly 272K input
			// tokens it is still short context: 272,000×2.5 + 1,000×10 =
			// 690,000.
			"long context, at the threshold", "openai", "gpt-6-sol", 272_000, 1_000, 1, 690_000,
		},
		{
			// One token more and the whole request is long context: $5 write,
			// $15 out. 272,001×5 + 1,000×15 = 1,375,005.
			"long context, over the threshold", "openai", "gpt-6-sol", 272_001, 1_000, 1, 1_375_005,
		},
		{
			// gpt-4.1: $2 in, $2 write, $8 out, at most 32,768 output tokens.
			// The input is 1,000×2 = 2,000. With no cap the output is that
			// maximum: 32,768×8 = 262,144.
			"no cap: the model's maximum output", "openai", "gpt-4.1", 1_000, 0, 1, 264_144,
		},
		{
			// A cap of 1,000 is under the maximum: 2,000 + 1,000×8.
			"a cap below the maximum", "openai", "gpt-4.1", 1_000, 1_000, 1, 10_000,
		},
		{
			// A cap of 100,000 is over it, and the maximum applies instead.
			"a cap above the maximum", "openai", "gpt-4.1", 1_000, 100_000, 1, 264_144,
		},
		{
			// So does a cap of math.MaxInt64.
			"a cap of math.MaxInt64", "openai", "gpt-4.1", 1_000, math.MaxInt64, 1, 264_144,
		},
		{
			// Three choices: the maximum is 3×32,768 = 98,304 tokens, so
			// 2,000 + 98,304×8 = 788,432.
			"3 choices, no cap", "openai", "gpt-4.1", 1_000, 0, 3, 788_432,
		},
		{
			// A cap is already ×choices: 3,000 is 1,000 for each choice.
			// 2,000 + 3,000×8 = 26,000.
			"3 choices, a cap below the maximum", "openai", "gpt-4.1", 1_000, 3_000, 3, 26_000,
		},
		{
			// 200,000 is over 3×32,768, so the maximum applies.
			"3 choices, a cap above the maximum", "openai", "gpt-4.1", 1_000, 200_000, 3, 788_432,
		},
		{
			// A dated snapshot resolves to its alias's rate, as in Cost.
			// Haiku 4.5: $1 in, $1.25 5m write, $2 1h write, $5 out.
			// 1,000×2 + 1,000×5 = 7,000.
			"anthropic dated ID", "anthropic", "claude-haiku-4-5-20251001", 1_000, 1_000, 1, 7_000,
		},
		{
			// gpt-4o: $2.50 in and write, $10 out. 1,000×2.5 + 1,000×10.
			"openai dated snapshot", "openai", "gpt-4o-2024-08-06", 1_000, 1_000, 1, 12_500,
		},
		{
			// ...unless the snapshot has its own rate: $5 in and write, $15
			// out. 1,000×5 + 1,000×15 = 20,000.
			"snapshot with its own rate", "openai", "gpt-4o-2024-05-13", 1_000, 1_000, 1, 20_000,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Reserve(tc.provider, tc.model, tc.input, tc.outputCap, tc.choices, oct2026)
			if !ok {
				t.Fatalf("Reserve() found no price for %s/%s", tc.provider, tc.model)
			}
			if got != tc.want {
				t.Errorf("Reserve() = %d micro-$, want %d", got, tc.want)
			}
		})
	}
}

// Gemini 3.8 Flash's price doubles on 1 January 2027 (UTC); a request
// reserves at the price in force when it arrived.
func TestReserveUsesPriceInForce(t *testing.T) {
	// 100,000 input tokens and, with no cap, the 65,536-token maximum output.
	// Until 2027: 100,000×0.75 + 65,536×3.75 = 320,760. After it, twice that.
	before, _ := Reserve("gemini", "gemini-3.8-flash", 100_000, 0, 1, time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC))
	after, _ := Reserve("gemini", "gemini-3.8-flash", 100_000, 0, 1, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))

	if before != 320_760 {
		t.Errorf("reservation on 31 Dec 2026 = %d, want 320760", before)
	}
	if after != 641_520 {
		t.Errorf("reservation on 1 Jan 2027 = %d, want 641520", after)
	}
}

// A reservation rounds up to a whole micro-dollar, where Cost rounds half up,
// so a request that could cost a fraction of one still reserves it.
func TestReserveRoundsUp(t *testing.T) {
	cases := []struct {
		name      string
		provider  string
		model     string
		input     int64
		outputCap int64
		want      int64
	}{
		{
			// Gemini 2.5 Flash-Lite: $0.40 out. One token is 0.4 micro-$,
			// which Cost would round down to 0.
			"one token under $1 per million", "gemini", "gemini-2.5-flash-lite", 0, 1, 1,
		},
		{
			// gpt-4.1-nano: $0.10 in, $0.40 out. 10,000×0.1 + 5,000×0.4 =
			// 1,000 + 2,000, a whole number, so there's nothing to round.
			"a whole number of micro-dollars", "openai", "gpt-4.1-nano", 10_000, 5_000, 3_000,
		},
		{
			// One more input token makes it 3,000.1.
			"a fraction over", "openai", "gpt-4.1-nano", 10_001, 5_000, 3_001,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Reserve(tc.provider, tc.model, tc.input, tc.outputCap, 1, oct2026)
			if !ok {
				t.Fatalf("Reserve() found no price for %s/%s", tc.provider, tc.model)
			}
			if got != tc.want {
				t.Errorf("Reserve() = %d micro-$, want %d", got, tc.want)
			}
		})
	}
}

// A client sets its own output cap and choices, and the input estimate scales
// with the body, so each can be up to math.MaxInt64. Reserve stops at
// MaxMicros ($1 billion) rather than overflow: wrapped around to a negative
// number, a reservation would fit any budget.
func TestReserveSaturates(t *testing.T) {
	models := []struct{ provider, model string }{
		{"openai", "gpt-5-nano"},       // the cheapest input
		{"groq", "openai/gpt-oss-20b"}, // the cheapest output
		{"openai", "gpt-6-astra"},      // the dearest, with a long-context tier
		{"anthropic", "claude-fable-5-1"},
		{"gemini", "gemini-2.5-flash-lite"},
	}
	cases := []struct {
		name                      string
		input, outputCap, choices int64
	}{
		{"input", math.MaxInt64, 1, 1},
		// A cap is held to the model's maximum for each choice, so it takes
		// huge choices as well to reach MaxMicros.
		{"cap and choices", 0, math.MaxInt64, math.MaxInt64},
		{"choices, no cap", 0, 0, math.MaxInt64},
		{"all three", math.MaxInt64, math.MaxInt64, math.MaxInt64},
	}

	for _, m := range models {
		for _, tc := range cases {
			t.Run(m.model+"/"+tc.name, func(t *testing.T) {
				got, ok := Reserve(m.provider, m.model, tc.input, tc.outputCap, tc.choices, oct2026)
				if !ok {
					t.Fatalf("Reserve() found no price for %s/%s", m.provider, m.model)
				}
				if got != 1_000_000_000_000_000 {
					t.Errorf("Reserve() = %d micro-$, want 1000000000000000 ($1 billion)", got)
				}
			})
		}
	}
}

// TestReserveMatchesExactArithmetic checks Reserve against the same sum worked
// out with no way to overflow, for every rate and for sizes from nothing up
// to math.MaxInt64, the long-context threshold and one token over it among
// them: it is that sum rounded up, or MaxMicros when the sum is over it.
//
// Go note: big.Int does integer arithmetic on numbers of any size, so it
// can't overflow. Here it is the reference the saturating arithmetic must
// agree with.
func TestReserveMatchesExactArithmetic(t *testing.T) {
	sizes := []int64{0, 1, 999_999, 1_000_000, 1_000_001, 1e9, 1e12, 1e14, 1e15, 1e18, math.MaxInt64 - 1, math.MaxInt64}
	choices := []int64{1, 3, 1e12, math.MaxInt64}

	for _, r := range rates {
		inputs := append([]int64{r.LongContextAbove, r.LongContextAbove + 1}, sizes...)
		for _, input := range inputs {
			for _, outputCap := range sizes {
				for _, n := range choices {
					want := exactReserve(r, input, outputCap, n)
					got, ok := Reserve(r.Provider, r.Model, input, outputCap, n, priceTime(r))
					if !ok || got != want {
						t.Fatalf("%s/%s: Reserve(input %d, cap %d, choices %d) = %d, %t; want %d",
							r.Provider, r.Model, input, outputCap, n, got, ok, want)
					}
				}
			}
		}
	}
}

// exactReserve is Reserve's rule worked out with big.Int.
func exactReserve(r Rate, input, outputCap, choices int64) int64 {
	p := r.Price
	if r.LongContextAbove > 0 && input > r.LongContextAbove {
		p = r.LongContext
	}
	inputPrice := max(p.Input, p.CacheWrite, p.CacheWrite1h)

	output := new(big.Int).Mul(big.NewInt(r.MaxOutput), big.NewInt(choices))
	if c := big.NewInt(outputCap); outputCap > 0 && c.Cmp(output) < 0 {
		output = c
	}

	sum := new(big.Int).Mul(big.NewInt(input), big.NewInt(inputPrice))
	sum.Add(sum, new(big.Int).Mul(output, big.NewInt(p.Output)))
	sum.Add(sum, big.NewInt(999_999))
	sum.Div(sum, big.NewInt(1_000_000))
	if sum.Cmp(big.NewInt(1_000_000_000_000_000)) > 0 {
		return 1_000_000_000_000_000
	}
	return sum.Int64()
}

// priceTime is a time at which r is the rate in force: its own start date for
// a rate that takes effect later, and any time before that for the rate it
// replaces.
func priceTime(r Rate) time.Time {
	if r.From.IsZero() {
		return oct2026
	}
	return r.From
}

// TestReserveCoversCost: usage that fits inside a reservation never costs more
// than it. For every rate in the table, the input tokens reserved for are used
// as each kind in turn and as a mix, the output is at the allowance, and the
// reservation is checked against Cost.
func TestReserveCoversCost(t *testing.T) {
	for _, r := range rates {
		at := priceTime(r)
		inputs := []int64{0, 1, 1_000, 100_000, 5_000_000}
		if r.LongContextAbove > 0 {
			inputs = append(inputs, r.LongContextAbove, r.LongContextAbove+1)
		}

		for _, input := range inputs {
			for _, choices := range []int64{1, 3} {
				for _, outputCap := range []int64{0, 1, r.MaxOutput, 10 * r.MaxOutput} {
					reserved, ok := Reserve(r.Provider, r.Model, input, outputCap, choices, at)
					if !ok {
						t.Fatalf("Reserve() found no price for %s/%s", r.Provider, r.Model)
					}
					allowance := r.MaxOutput * choices
					if outputCap > 0 {
						allowance = min(allowance, outputCap)
					}

					for _, u := range usagesWithin(input, allowance) {
						u.Model = r.Model
						cost, ok := Cost(r.Provider, u, "", at)
						if !ok {
							t.Fatalf("Cost() found no price for %s/%s", r.Provider, r.Model)
						}
						if cost > reserved {
							t.Errorf("%s/%s: Cost(%+v) = %d is over Reserve(input %d, cap %d, choices %d) = %d",
								r.Provider, r.Model, u, cost, input, outputCap, choices, reserved)
						}
					}
				}
			}
		}
	}
}

// usagesWithin returns usages that fit in a reservation for input input
// tokens and output output tokens: the input as each kind in turn and as a
// mix, at the full count and one token short of it. One short of a
// long-context threshold is billed at the short-context prices.
func usagesWithin(input, output int64) []usage.Usage {
	var us []usage.Usage
	for _, n := range []int64{input, input - 1} {
		if n < 0 {
			continue
		}
		us = append(us,
			usage.Usage{Input: n, Output: output},
			usage.Usage{CachedInput: n, Output: output},
			usage.Usage{CacheWrite: n, Output: output},
			usage.Usage{CacheWrite1h: n, Output: output},
			usage.Usage{Input: n / 4, CachedInput: n / 4, CacheWrite: n / 4, CacheWrite1h: n - 3*(n/4), Output: output},
		)
	}
	return us
}

func TestReserveUnknown(t *testing.T) {
	cases := []struct{ provider, model string }{
		// Groq lists Llama as "contact sales": no public price to use.
		{"groq", "llama-3.3-70b-versatile"},
		{"openai", "gpt-9"},
		// Prices are per provider: the same name elsewhere isn't priced.
		{"groq", "gpt-4o"},
		{"my-renamed-openai", "gpt-4o"},
		// A body that named no model.
		{"openai", ""},
	}
	for _, tc := range cases {
		if got, ok := Reserve(tc.provider, tc.model, 10, 10, 1, oct2026); ok || got != 0 {
			t.Errorf("Reserve(%s, %q) = %d, %t; want 0, false", tc.provider, tc.model, got, ok)
		}
	}
}
