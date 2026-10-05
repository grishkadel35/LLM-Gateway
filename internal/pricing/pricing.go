// Package pricing turns token usage into cost, in integer micro-dollars
// (millionths of a US dollar). There are no floats anywhere: $2.50 per
// million tokens is 2_500_000 micro-dollars per million tokens.
//
// Prices are copied from each provider's official pricing page, with the
// page and the date checked next to each block. They change: check the
// pages, and add a new Rate with a From date rather than editing the old
// one, so past requests keep the price they were served at.
//
// Not modelled, so usage under them is billed at the standard rates below:
// batch discounts (batch endpoints are refused as unmetered anyway),
// Anthropic fast mode (speed: "fast") and US-only inference (inference_geo,
// 1.1x), OpenAI service tiers other than Standard, Gemini's higher audio
// input prices, and Gemini cache storage per hour.
package pricing

import (
	"regexp"
	"time"

	"github.com/grishkadel/llm-gateway/internal/usage"
)

// Price is a model's prices in micro-dollars per million tokens, one per
// kind of token in usage.Usage. Each is its own number, not a multiplier of
// Input: cache-read discounts differ by model (0.1x on most, 0.05x on Claude
// Opus 5.5, 0.025x on Claude Fable 5.1).
type Price struct {
	Input        int64
	Output       int64
	CacheRead    int64
	CacheWrite   int64 // default-TTL cache writes: Anthropic 5-minute, OpenAI
	CacheWrite1h int64 // Anthropic 1-hour cache writes
}

// Rate is the price of one provider's model from a point in time.
type Rate struct {
	// Provider is the provider's name in config.yaml. A provider renamed
	// there has no prices.
	Provider string
	Model    string
	// From is when the price takes effect; zero means it always has.
	From time.Time
	Price
	// LongContextAbove, if set, is the input size above which the whole
	// request is billed at LongContext instead. Input counts every input
	// token: uncached, cached and cache writes.
	LongContextAbove int64
	LongContext      Price
}

// Constructors that apply each provider's rules for prices its page
// doesn't list.

// openAI: before GPT-5.6, cache writes have no charge of their own and bill
// as ordinary input; pass in for write. A model with no cached-input price
// gets no discount.
func openAI(in, cached, write, out int64) Price {
	return Price{Input: in, Output: out, CacheRead: cached, CacheWrite: write, CacheWrite1h: write}
}

func anthropic(in, write5m, write1h, read, out int64) Price {
	return Price{Input: in, Output: out, CacheRead: read, CacheWrite: write5m, CacheWrite1h: write1h}
}

// gemini: usage reports no cache writes; they would bill as input.
func gemini(in, read, out int64) Price {
	return Price{Input: in, Output: out, CacheRead: read, CacheWrite: in, CacheWrite1h: in}
}

// groq lists no cached-input price, so cached input costs the input rate:
// an overestimate if Groq discounts it, never an underestimate.
func groq(in, out int64) Price {
	return Price{Input: in, Output: out, CacheRead: in, CacheWrite: in, CacheWrite1h: in}
}

var jan2027 = time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)

var rates = []Rate{
	// OpenAI, Standard tier. https://developers.openai.com/api/docs/pricing
	// (checked 2026-10-05). Long context is above 272K input tokens. Cache
	// writes cost 1.25x input from GPT-5.6 on and bill as input before it
	// (https://developers.openai.com/api/docs/guides/prompt-caching).
	//                                              in          cached     write       out
	{Provider: "openai", Model: "gpt-6-astra", Price: openAI(10_000_000, 1_000_000, 12_500_000, 50_000_000),
		LongContextAbove: 272_000, LongContext: openAI(20_000_000, 2_000_000, 25_000_000, 75_000_000)},
	{Provider: "openai", Model: "gpt-6.1-sol", Price: openAI(2_000_000, 100_000, 2_500_000, 10_000_000),
		LongContextAbove: 272_000, LongContext: openAI(4_000_000, 200_000, 5_000_000, 15_000_000)},
	{Provider: "openai", Model: "gpt-6-sol", Price: openAI(2_000_000, 200_000, 2_500_000, 10_000_000),
		LongContextAbove: 272_000, LongContext: openAI(4_000_000, 400_000, 5_000_000, 15_000_000)},
	{Provider: "openai", Model: "gpt-6-luna", Price: openAI(100_000, 10_000, 125_000, 500_000),
		LongContextAbove: 272_000, LongContext: openAI(200_000, 20_000, 250_000, 750_000)},
	{Provider: "openai", Model: "gpt-5.6-sol", Price: openAI(4_000_000, 400_000, 5_000_000, 20_000_000),
		LongContextAbove: 272_000, LongContext: openAI(8_000_000, 800_000, 10_000_000, 30_000_000)},
	{Provider: "openai", Model: "gpt-5.6-terra", Price: openAI(2_000_000, 200_000, 2_500_000, 12_000_000),
		LongContextAbove: 272_000, LongContext: openAI(4_000_000, 400_000, 5_000_000, 18_000_000)},
	{Provider: "openai", Model: "gpt-5.6-luna", Price: openAI(200_000, 20_000, 250_000, 1_200_000),
		LongContextAbove: 272_000, LongContext: openAI(400_000, 40_000, 500_000, 1_800_000)},
	{Provider: "openai", Model: "gpt-5.5", Price: openAI(5_000_000, 500_000, 5_000_000, 30_000_000)},
	{Provider: "openai", Model: "gpt-5.4", Price: openAI(2_500_000, 250_000, 2_500_000, 15_000_000),
		LongContextAbove: 272_000, LongContext: openAI(5_000_000, 500_000, 5_000_000, 22_500_000)},
	{Provider: "openai", Model: "gpt-5.4-mini", Price: openAI(750_000, 75_000, 750_000, 4_500_000)},
	{Provider: "openai", Model: "gpt-5.4-nano", Price: openAI(200_000, 20_000, 200_000, 1_250_000)},
	{Provider: "openai", Model: "gpt-5.2", Price: openAI(1_750_000, 175_000, 1_750_000, 14_000_000)},
	{Provider: "openai", Model: "gpt-5.1", Price: openAI(1_250_000, 125_000, 1_250_000, 10_000_000)},
	{Provider: "openai", Model: "gpt-5", Price: openAI(1_250_000, 125_000, 1_250_000, 10_000_000)},
	{Provider: "openai", Model: "gpt-5-mini", Price: openAI(250_000, 25_000, 250_000, 2_000_000)},
	{Provider: "openai", Model: "gpt-5-nano", Price: openAI(50_000, 5_000, 50_000, 400_000)},
	{Provider: "openai", Model: "gpt-4.1", Price: openAI(2_000_000, 500_000, 2_000_000, 8_000_000)},
	{Provider: "openai", Model: "gpt-4.1-mini", Price: openAI(400_000, 100_000, 400_000, 1_600_000)},
	{Provider: "openai", Model: "gpt-4.1-nano", Price: openAI(100_000, 25_000, 100_000, 400_000)},
	{Provider: "openai", Model: "gpt-4o", Price: openAI(2_500_000, 1_250_000, 2_500_000, 10_000_000)},
	// This snapshot has its own price, higher than gpt-4o's, and no cache
	// discount.
	{Provider: "openai", Model: "gpt-4o-2024-05-13", Price: openAI(5_000_000, 5_000_000, 5_000_000, 15_000_000)},
	{Provider: "openai", Model: "gpt-4o-mini", Price: openAI(150_000, 75_000, 150_000, 600_000)},
	{Provider: "openai", Model: "o3", Price: openAI(2_000_000, 500_000, 2_000_000, 8_000_000)},
	{Provider: "openai", Model: "o4-mini", Price: openAI(1_100_000, 275_000, 1_100_000, 4_400_000)},

	// Anthropic. https://platform.claude.com/docs/en/about-claude/pricing
	// (checked 2026-10-05). No long-context tier: Claude 4.6 and later bill
	// the full 1M window at standard prices. IDs from
	// https://platform.claude.com/docs/en/about-claude/models/overview;
	// dateless from the 4.6 generation on, aliases before it.
	//                                                        in          5m write    1h write    read       out
	{Provider: "anthropic", Model: "claude-fable-5-1", Price: anthropic(10_000_000, 12_500_000, 20_000_000, 250_000, 50_000_000)},
	{Provider: "anthropic", Model: "claude-fable-5", Price: anthropic(10_000_000, 12_500_000, 20_000_000, 1_000_000, 50_000_000)},
	{Provider: "anthropic", Model: "claude-opus-5-5", Price: anthropic(4_000_000, 5_000_000, 8_000_000, 200_000, 20_000_000)},
	{Provider: "anthropic", Model: "claude-opus-5", Price: anthropic(5_000_000, 6_250_000, 10_000_000, 500_000, 25_000_000)},
	{Provider: "anthropic", Model: "claude-opus-4-8", Price: anthropic(5_000_000, 6_250_000, 10_000_000, 500_000, 25_000_000)},
	{Provider: "anthropic", Model: "claude-opus-4-7", Price: anthropic(5_000_000, 6_250_000, 10_000_000, 500_000, 25_000_000)},
	{Provider: "anthropic", Model: "claude-opus-4-6", Price: anthropic(5_000_000, 6_250_000, 10_000_000, 500_000, 25_000_000)},
	{Provider: "anthropic", Model: "claude-opus-4-5", Price: anthropic(5_000_000, 6_250_000, 10_000_000, 500_000, 25_000_000)},
	{Provider: "anthropic", Model: "claude-sonnet-5-5", Price: anthropic(2_000_000, 2_500_000, 4_000_000, 200_000, 10_000_000)},
	{Provider: "anthropic", Model: "claude-sonnet-5", Price: anthropic(2_000_000, 2_500_000, 4_000_000, 200_000, 10_000_000)},
	{Provider: "anthropic", Model: "claude-sonnet-4-6", Price: anthropic(3_000_000, 3_750_000, 6_000_000, 300_000, 15_000_000)},
	{Provider: "anthropic", Model: "claude-sonnet-4-5", Price: anthropic(3_000_000, 3_750_000, 6_000_000, 300_000, 15_000_000)},
	{Provider: "anthropic", Model: "claude-sonnet-4", Price: anthropic(3_000_000, 3_750_000, 6_000_000, 300_000, 15_000_000)},
	{Provider: "anthropic", Model: "claude-haiku-4-5", Price: anthropic(1_000_000, 1_250_000, 2_000_000, 100_000, 5_000_000)},

	// Gemini, paid tier, text input. https://ai.google.dev/gemini-api/docs/pricing
	// (checked 2026-10-05). Output prices include thinking tokens. The 3.6 to
	// 3.8 Flash prices double on 1 January 2027. Long context is above 200K
	// input tokens.
	//                                                          in         read     out
	{Provider: "gemini", Model: "gemini-3.8-flash", Price: gemini(750_000, 75_000, 3_750_000)},
	{Provider: "gemini", Model: "gemini-3.8-flash", From: jan2027, Price: gemini(1_500_000, 150_000, 7_500_000)},
	{Provider: "gemini", Model: "gemini-3.7-flash", Price: gemini(750_000, 75_000, 3_750_000)},
	{Provider: "gemini", Model: "gemini-3.7-flash", From: jan2027, Price: gemini(1_500_000, 150_000, 7_500_000)},
	{Provider: "gemini", Model: "gemini-3.6-flash", Price: gemini(750_000, 75_000, 3_750_000)},
	{Provider: "gemini", Model: "gemini-3.6-flash", From: jan2027, Price: gemini(1_500_000, 150_000, 7_500_000)},
	{Provider: "gemini", Model: "gemini-3.5-flash", Price: gemini(1_500_000, 150_000, 9_000_000)},
	{Provider: "gemini", Model: "gemini-3.5-flash-lite", Price: gemini(300_000, 30_000, 2_500_000)},
	{Provider: "gemini", Model: "gemini-3.1-flash-lite", Price: gemini(250_000, 25_000, 1_500_000)},
	{Provider: "gemini", Model: "gemini-3.1-pro-preview", Price: gemini(2_000_000, 200_000, 12_000_000),
		LongContextAbove: 200_000, LongContext: gemini(4_000_000, 400_000, 18_000_000)},
	{Provider: "gemini", Model: "gemini-3-flash-preview", Price: gemini(500_000, 50_000, 3_000_000)},
	{Provider: "gemini", Model: "gemini-2.5-pro", Price: gemini(1_250_000, 125_000, 10_000_000),
		LongContextAbove: 200_000, LongContext: gemini(2_500_000, 250_000, 15_000_000)},
	{Provider: "gemini", Model: "gemini-2.5-flash", Price: gemini(300_000, 30_000, 2_500_000)},
	{Provider: "gemini", Model: "gemini-2.5-flash-lite", Price: gemini(100_000, 10_000, 400_000)},

	// Groq. https://console.groq.com/docs/models (checked 2026-10-05). The
	// Llama models are listed as "Contact Sales", so they have no price here
	// and their usage is logged with no cost.
	//                                                         in       out
	{Provider: "groq", Model: "openai/gpt-oss-120b", Price: groq(150_000, 600_000)},
	{Provider: "groq", Model: "openai/gpt-oss-20b", Price: groq(75_000, 300_000)},
	{Provider: "groq", Model: "qwen/qwen3.8-27b", Price: groq(800_000, 4_000_000)},
}

// index maps provider, then model, to its rates in table order, which lists
// each model's rates oldest first.
var index = func() map[string]map[string][]Rate {
	m := map[string]map[string][]Rate{}
	for _, r := range rates {
		if m[r.Provider] == nil {
			m[r.Provider] = map[string][]Rate{}
		}
		m[r.Provider][r.Model] = append(m[r.Provider][r.Model], r)
	}
	return m
}()

// Cost returns what u cost in micro-dollars, at the prices in force at the
// time at. ok is false when the model isn't priced; the caller records no
// cost rather than a guess.
//
// The model is looked up as the response named it, then with any date suffix
// stripped (responses name dated snapshots such as gpt-4o-2024-08-06), then
// as the request named it, likewise.
func Cost(provider string, u usage.Usage, requested string, at time.Time) (cost int64, ok bool) {
	r, ok := lookup(provider, at, u.Model, requested)
	if !ok {
		return 0, false
	}

	p := r.Price
	input := u.Input + u.CachedInput + u.CacheWrite + u.CacheWrite1h
	if r.LongContextAbove > 0 && input > r.LongContextAbove {
		p = r.LongContext
	}

	// Sum before dividing, so rounding happens once per request. With prices
	// under $1,000/MTok and requests under 10M tokens, the sum stays far
	// below int64's limit.
	sum := u.Input*p.Input + u.CachedInput*p.CacheRead + u.CacheWrite*p.CacheWrite +
		u.CacheWrite1h*p.CacheWrite1h + u.Output*p.Output
	return (sum + 500_000) / 1_000_000, true
}

func lookup(provider string, at time.Time, models ...string) (Rate, bool) {
	byModel := index[provider]
	for _, model := range models {
		for _, name := range []string{model, normalize(model)} {
			if name == "" {
				continue
			}
			if r, ok := inForce(byModel[name], at); ok {
				return r, true
			}
		}
	}
	return Rate{}, false
}

// inForce returns the latest rate that took effect at or before at.
func inForce(rs []Rate, at time.Time) (Rate, bool) {
	var best Rate
	found := false
	for _, r := range rs {
		if !r.From.After(at) && (!found || r.From.After(best.From)) {
			best, found = r, true
		}
	}
	return best, found
}

// dateSuffix matches a snapshot date at the end of a model name:
// -2024-08-06 (OpenAI) or -20250514 (Anthropic).
var dateSuffix = regexp.MustCompile(`-(\d{4}-\d{2}-\d{2}|\d{8})$`)

func normalize(model string) string {
	return dateSuffix.ReplaceAllString(model, "")
}
