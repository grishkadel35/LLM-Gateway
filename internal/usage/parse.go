package usage

import (
	"bytes"
	"encoding/json"

	"github.com/grishkadel/llm-gateway/internal/provider"
)

// Usage is the token usage of one response, in one convention for every
// format: each field is a disjoint group of tokens with its own price, so a
// response's cost is each field times its price, summed.
type Usage struct {
	// Model is the model the response says served it, often a dated
	// snapshot of the requested alias. "" if the response didn't say.
	Model string
	// Input is input tokens billed at the base input rate: cache reads and
	// cache writes are not included. Providers disagree on this (OpenAI's and
	// Gemini's prompt counts include cache reads, Anthropic's doesn't), so
	// each parser converts.
	Input int64
	// CachedInput is input tokens read from the provider's prompt cache.
	CachedInput int64
	// CacheWrite is input tokens written to the provider's prompt cache at
	// its default TTL: Anthropic's 5-minute writes, and OpenAI's (one TTL,
	// billed at its own rate from GPT-5.6 on). CacheWrite1h is Anthropic's
	// 1-hour writes, which cost more.
	CacheWrite   int64
	CacheWrite1h int64
	// Output is output tokens, reasoning and thinking included: they are
	// billed as output.
	Output int64
}

// Tokens is the total of every kind of token.
func (u Usage) Tokens() int64 {
	return u.Input + u.CachedInput + u.CacheWrite + u.CacheWrite1h + u.Output
}

// parser reads one response's usage, fed either SSE event payloads or one
// complete body.
type parser interface {
	// event handles the data of one server-sent event.
	event(data []byte)
	// body handles a complete, non-SSE response body.
	body(b []byte)
	usage() Usage
}

// newParser returns the parser for format, or nil if there is none.
func newParser(format provider.Format) parser {
	switch format {
	case provider.FormatOpenAI:
		return &openAIParser{}
	case provider.FormatAnthropic:
		return &anthropicParser{}
	case provider.FormatGemini:
		return &geminiParser{}
	}
	return nil
}

// openAIParser reads the OpenAI Chat Completions shape, which Groq shares.
// Streams carry usage on one chunk near the end (the include_usage chunk;
// Groq also on the finish chunk). The last usage seen wins, never a sum:
// Groq reports the same numbers twice.
type openAIParser struct {
	u Usage
}

type openAIChunk struct {
	Model string `json:"model"`
	Usage *struct {
		PromptTokens        int64 `json:"prompt_tokens"`
		CompletionTokens    int64 `json:"completion_tokens"`
		PromptTokensDetails struct {
			CachedTokens     int64 `json:"cached_tokens"`
			CacheWriteTokens int64 `json:"cache_write_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

func (p *openAIParser) event(data []byte) {
	var c openAIChunk
	if json.Unmarshal(data, &c) != nil {
		// "[DONE]", or anything else that isn't a chunk.
		return
	}
	if c.Model != "" {
		p.u.Model = c.Model
	}
	if c.Usage != nil {
		// Both are parts of prompt_tokens (OpenAI's prompt caching guide:
		// ordinary input = input - cached - cache writes).
		d := c.Usage.PromptTokensDetails
		p.u.Input = c.Usage.PromptTokens - d.CachedTokens - d.CacheWriteTokens
		p.u.CachedInput = d.CachedTokens
		p.u.CacheWrite = d.CacheWriteTokens
		p.u.Output = c.Usage.CompletionTokens
	}
}

// A non-streaming body is one chunk-shaped object.
func (p *openAIParser) body(b []byte) { p.event(b) }

func (p *openAIParser) usage() Usage { return p.u }

// anthropicParser reads Anthropic's Messages shape. input_tokens already
// excludes cache reads and writes, so it is Input as is.
//
// Streams carry input usage in message_start, whose output_tokens is only a
// placeholder, and the final, cumulative usage in message_delta. The delta's
// fields win when present; a stream cut before it reports no output.
type anthropicParser struct {
	u Usage
}

// anthropicUsage uses pointers to tell a field that is absent from one that
// is zero: message_delta restates only some fields.
type anthropicUsage struct {
	InputTokens              *int64 `json:"input_tokens"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
	CacheCreation            *struct {
		Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
		Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
	OutputTokens *int64 `json:"output_tokens"`
}

func (p *anthropicParser) event(data []byte) {
	var e struct {
		Type    string `json:"type"`
		Message struct {
			Model string          `json:"model"`
			Usage *anthropicUsage `json:"usage"`
		} `json:"message"`
		Usage *anthropicUsage `json:"usage"`
	}
	if json.Unmarshal(data, &e) != nil {
		return
	}
	switch e.Type {
	case "message_start":
		if e.Message.Model != "" {
			p.u.Model = e.Message.Model
		}
		p.apply(e.Message.Usage, false)
	case "message_delta":
		p.apply(e.Usage, true)
	}
}

func (p *anthropicParser) body(b []byte) {
	var m struct {
		Model string          `json:"model"`
		Usage *anthropicUsage `json:"usage"`
	}
	if json.Unmarshal(b, &m) != nil {
		return
	}
	p.u.Model = m.Model
	p.apply(m.Usage, true)
}

// apply copies the fields u states. withOutput is false for message_start,
// whose output count is a placeholder.
func (p *anthropicParser) apply(u *anthropicUsage, withOutput bool) {
	if u == nil {
		return
	}
	if u.InputTokens != nil {
		p.u.Input = *u.InputTokens
	}
	if u.CacheReadInputTokens != nil {
		p.u.CachedInput = *u.CacheReadInputTokens
	}
	switch {
	case u.CacheCreation != nil:
		p.u.CacheWrite = u.CacheCreation.Ephemeral5m
		p.u.CacheWrite1h = u.CacheCreation.Ephemeral1h
	case u.CacheCreationInputTokens != nil && *u.CacheCreationInputTokens != p.u.CacheWrite+p.u.CacheWrite1h:
		// A total with no TTL breakdown: all writes are the default 5-minute
		// TTL. A total that matches what is already known keeps the
		// breakdown message_start gave.
		p.u.CacheWrite = *u.CacheCreationInputTokens
		p.u.CacheWrite1h = 0
	}
	if withOutput && u.OutputTokens != nil {
		p.u.Output = *u.OutputTokens
	}
}

func (p *anthropicParser) usage() Usage { return p.u }

// geminiParser reads Gemini's generateContent shape. promptTokenCount
// includes cache reads, so they are subtracted for Input. Tool use (code
// execution, grounding) adds input reported in toolUsePromptTokenCount,
// outside promptTokenCount, and billed as input, so it is added to Input.
// Thinking is billed as output but reported in thoughtsTokenCount, outside
// candidatesTokenCount, so Output is their sum. (Both verified live:
// totalTokenCount is the sum of prompt, tool use, candidates and thoughts.)
//
// Every streamed chunk carries usageMetadata as a running total, so the last
// one wins. Without ?alt=sse the stream is one JSON array of chunks.
type geminiParser struct {
	u Usage
}

type geminiChunk struct {
	ModelVersion  string `json:"modelVersion"`
	UsageMetadata *struct {
		PromptTokenCount        int64 `json:"promptTokenCount"`
		CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
		ToolUsePromptTokenCount int64 `json:"toolUsePromptTokenCount"`
		CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
		ThoughtsTokenCount      int64 `json:"thoughtsTokenCount"`
	} `json:"usageMetadata"`
}

func (p *geminiParser) event(data []byte) {
	var c geminiChunk
	if json.Unmarshal(data, &c) == nil {
		p.chunk(c)
	}
}

func (p *geminiParser) body(b []byte) {
	if t := bytes.TrimSpace(b); len(t) > 0 && t[0] == '[' {
		var chunks []geminiChunk
		if json.Unmarshal(t, &chunks) != nil {
			return
		}
		for _, c := range chunks {
			p.chunk(c)
		}
		return
	}
	p.event(b)
}

func (p *geminiParser) chunk(c geminiChunk) {
	if c.ModelVersion != "" {
		p.u.Model = c.ModelVersion
	}
	if m := c.UsageMetadata; m != nil {
		p.u.Input = m.PromptTokenCount - m.CachedContentTokenCount + m.ToolUsePromptTokenCount
		p.u.CachedInput = m.CachedContentTokenCount
		p.u.Output = m.CandidatesTokenCount + m.ThoughtsTokenCount
	}
}

func (p *geminiParser) usage() Usage { return p.u }
