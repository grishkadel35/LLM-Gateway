package usage

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/grishkadel/llm-gateway/internal/provider"
)

// estimateOf decodes body the way ReadBody does, then estimates it.
func estimateOf(format provider.Format, body string) (inputEstimate, outputCap int64) {
	var fields map[string]json.RawMessage
	_ = json.Unmarshal([]byte(body), &fields) // stays nil unless body is a JSON object
	return estimate(format, []byte(body), fields)
}

// ceilQuarter is ceil(n / 4), written the way the estimate's rule words it.
func ceilQuarter(n int) int64 { return int64(math.Ceil(float64(n) / 4)) }

// lenSum is the total length of ss.
func lenSum(ss []string) int {
	n := 0
	for _, s := range ss {
		n += len(s)
	}
	return n
}

// TestEstimateInlineImage: a 1 MiB image arrives as about 1.4 MB of base64,
// which /4 would charge as ~350,000 tokens. It is one media part instead: a
// flat 1,600 tokens, plus only the bytes around its payload / 4.
func TestEstimateInlineImage(t *testing.T) {
	image := base64.StdEncoding.EncodeToString(make([]byte, 1<<20))

	cases := []struct {
		name    string
		format  provider.Format
		body    string
		payload []string // every string inside the image part
	}{
		{"openai data URL", provider.FormatOpenAI,
			`{"model":"gpt-4o","messages":[{"role":"user","content":[{"type":"text","text":"What is in this image?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,` + image + `"}}]}]}`,
			[]string{"image_url", "data:image/png;base64," + image}},
		{"anthropic base64 source", provider.FormatAnthropic,
			`{"model":"claude-sonnet-4","max_tokens":1024,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + image + `"}},{"type":"text","text":"What is in this image?"}]}]}`,
			[]string{"image", "base64", "image/png", image}},
		{"gemini inlineData", provider.FormatGemini,
			`{"contents":[{"role":"user","parts":[{"text":"What is in this image?"},{"inlineData":{"mimeType":"image/png","data":"` + image + `"}}]}]}`,
			[]string{"image/png", image}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := estimateOf(tc.format, tc.body)

			want := 1600 + ceilQuarter(len(tc.body)-lenSum(tc.payload))
			if got != want {
				t.Errorf("InputEstimate = %d, want %d", got, want)
			}
			if whole := ceilQuarter(len(tc.body)); got > whole/100 {
				t.Errorf("InputEstimate = %d, want far below the %d that /4 of the whole body gives", got, whole)
			}
		})
	}
}

// TestEstimateMediaParts: what counts as a media part, in each format and
// wherever it sits in the JSON. Each costs 1,600 tokens, and every string
// inside it is payload that /4 leaves out.
func TestEstimateMediaParts(t *testing.T) {
	cases := []struct {
		name    string
		format  provider.Format
		body    string
		parts   int64
		payload []string // every string inside the media parts
	}{
		{"openai image by URL", provider.FormatOpenAI,
			`{"model":"gpt-4o","messages":[{"role":"user","content":[{"type":"text","text":"Describe it."},{"type":"image_url","image_url":{"url":"https://example.com/cat.png","detail":"high"}}]}]}`,
			1, []string{"image_url", "https://example.com/cat.png", "high"}},
		{"openai two images", provider.FormatOpenAI,
			`{"model":"gpt-4o","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}},{"type":"image_url","image_url":{"url":"https://example.com/b.png"}}]}]}`,
			2, []string{"image_url", "https://example.com/a.png", "image_url", "https://example.com/b.png"}},
		{"openai audio", provider.FormatOpenAI,
			`{"model":"gpt-4o-audio-preview","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"UklGRiQAAABXQVZF","format":"wav"}}]}]}`,
			1, []string{"input_audio", "UklGRiQAAABXQVZF", "wav"}},
		{"openai file", provider.FormatOpenAI,
			`{"model":"gpt-4o","messages":[{"role":"user","content":[{"type":"file","file":{"filename":"report.pdf","file_data":"data:application/pdf;base64,JVBERi0xLjQ="}},{"type":"text","text":"Summarize."}]}]}`,
			1, []string{"file", "report.pdf", "data:application/pdf;base64,JVBERi0xLjQ="}},
		{"anthropic image by URL", provider.FormatAnthropic,
			`{"model":"claude-sonnet-4","max_tokens":1024,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://example.com/cat.png"}},{"type":"text","text":"Describe it."}]}]}`,
			1, []string{"image", "url", "https://example.com/cat.png"}},
		{"anthropic image in a tool_result", provider.FormatAnthropic,
			`{"model":"claude-sonnet-4","max_tokens":1024,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01","content":[{"type":"text","text":"Screenshot taken."},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}}]}]}]}`,
			1, []string{"image", "base64", "image/png", "iVBORw0KGgo="}},
		{"anthropic PDF document", provider.FormatAnthropic,
			`{"model":"claude-sonnet-4","max_tokens":1024,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0xLjQ="}},{"type":"text","text":"Summarize."}]}]}`,
			1, []string{"document", "base64", "application/pdf", "JVBERi0xLjQ="}},
		// Its text is text, however long: only a source that isn't text is media.
		{"anthropic document with a text source", provider.FormatAnthropic,
			`{"model":"claude-sonnet-4","max_tokens":1024,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"text","media_type":"text/plain","data":"The quick brown fox jumps over the lazy dog."}}]}]}`,
			0, nil},
		// One part, with everything inside it as payload: the image in it isn't a second part.
		{"anthropic document holding an image", provider.FormatAnthropic,
			`{"model":"claude-sonnet-4","max_tokens":1024,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"content","content":[{"type":"text","text":"Chapter 1"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}}]}}]}]}`,
			1, []string{"document", "content", "text", "Chapter 1", "image", "base64", "image/png", "iVBORw0KGgo="}},
		{"gemini inline_data", provider.FormatGemini,
			`{"contents":[{"role":"user","parts":[{"text":"Describe it."},{"inline_data":{"mime_type":"image/png","data":"iVBORw0KGgo="}}]}]}`,
			1, []string{"image/png", "iVBORw0KGgo="}},
		{"gemini fileData", provider.FormatGemini,
			`{"contents":[{"role":"user","parts":[{"fileData":{"mimeType":"video/mp4","fileUri":"https://generativelanguage.googleapis.com/v1beta/files/abc123"}},{"text":"Summarize."}]}]}`,
			1, []string{"video/mp4", "https://generativelanguage.googleapis.com/v1beta/files/abc123"}},
		{"gemini file_data", provider.FormatGemini,
			`{"contents":[{"parts":[{"file_data":{"mime_type":"application/pdf","file_uri":"gs://bucket/report.pdf"}}]}]}`,
			1, []string{"application/pdf", "gs://bucket/report.pdf"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := estimateOf(tc.format, tc.body)

			want := 1600*tc.parts + ceilQuarter(len(tc.body)-lenSum(tc.payload))
			if got != want {
				t.Errorf("InputEstimate = %d, want %d (%d media parts)", got, want, tc.parts)
			}
		})
	}
}

// TestEstimateTextBody: in a body with no media, every byte counts: system
// prompts, tool definitions and every other field. Words and schema names that
// only look like media don't make a part.
func TestEstimateTextBody(t *testing.T) {
	cases := []struct {
		name   string
		format provider.Format
		body   string
	}{
		{"openai with a system prompt and tools", provider.FormatOpenAI,
			`{"model":"gpt-4o","max_tokens":512,"messages":[{"role":"system","content":"You are a terse assistant."},{"role":"user","content":"What's the weather in Oslo?"}],"tools":[{"type":"function","function":{"name":"get_weather","description":"Current weather for a city.","parameters":{"type":"object","properties":{"city":{"type":"string"},"image":{"type":"string","description":"Optional photo of the sky."},"type":{"type":"string"}},"required":["city"]}}}]}`},
		{"anthropic with a system prompt and tools", provider.FormatAnthropic,
			`{"model":"claude-sonnet-4","max_tokens":1024,"system":[{"type":"text","text":"You are a terse assistant."}],"tools":[{"name":"get_weather","description":"Current weather for a city.","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}],"messages":[{"role":"user","content":[{"type":"text","text":"What's the weather in Oslo?"}]},{"role":"assistant","content":[{"type":"tool_use","id":"toolu_01","name":"get_weather","input":{"city":"Oslo"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01","content":"3 degrees, clear."}]}]}`},
		{"gemini with a system instruction and tools", provider.FormatGemini,
			`{"systemInstruction":{"parts":[{"text":"You are a terse assistant."}]},"tools":[{"functionDeclarations":[{"name":"get_weather","description":"Current weather for a city.","parameters":{"type":"OBJECT","properties":{"city":{"type":"STRING"}}}}]}],"contents":[{"role":"user","parts":[{"text":"What's the weather in Oslo?"}]}],"generationConfig":{"maxOutputTokens":256}}`},
		{"media named in text", provider.FormatOpenAI,
			`{"model":"gpt-4o","messages":[{"role":"user","content":"Explain {\"type\":\"image_url\",\"image_url\":{\"url\":\"data:image/png;base64,AAAA\"}} and inlineData."}]}`},
		{"empty object", provider.FormatAnthropic, `{}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := estimateOf(tc.format, tc.body)
			if want := ceilQuarter(len(tc.body)); got != want {
				t.Errorf("InputEstimate = %d, want ceil(%d / 4) = %d", got, len(tc.body), want)
			}
		})
	}
}

// TestEstimateNonObjectBody: a body that isn't a JSON object has no fields to
// read and no parts to find. It is estimated as plain text, with no output cap
// even when it looks like it set one.
func TestEstimateNonObjectBody(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"truncated JSON", `{"model":"gpt-4o","max_tokens":100,"messages":[`},
		{"array", `[{"type":"image","source":{"type":"base64","data":"AAAA"}}]`},
		{"string", `"hello"`},
		{"null", `null`},
		{"not JSON", `model=gpt-4o&max_tokens=100`},
		{"empty", ``},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input, outputCap := estimateOf(provider.FormatOpenAI, tc.body)
			if want := ceilQuarter(len(tc.body)); input != want {
				t.Errorf("InputEstimate = %d, want ceil(%d / 4) = %d", input, len(tc.body), want)
			}
			if outputCap != 0 {
				t.Errorf("OutputCap = %d, want 0", outputCap)
			}
		})
	}
}

// TestEstimateNumberOutsideFloat64: the walk decodes numbers as float64, and
// one that doesn't fit is an error from the decoder. It must not turn media
// detection off for the rest of the body.
func TestEstimateNumberOutsideFloat64(t *testing.T) {
	body := `{"temperature":1e999,"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/cat.png"}}]}]}`

	got, _ := estimateOf(provider.FormatOpenAI, body)

	want := 1600 + ceilQuarter(len(body)-len("image_url")-len("https://example.com/cat.png"))
	if got != want {
		t.Errorf("InputEstimate = %d, want %d", got, want)
	}
}

// TestEstimateInvalidUTF8Payload: payload is measured on decoded strings, and
// each invalid byte decodes to a 3-byte U+FFFD. A part full of them must not
// push the text below zero, or the estimate with it.
func TestEstimateInvalidUTF8Payload(t *testing.T) {
	body := `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"` + strings.Repeat("\xff", 1000) + `"}}]}]}`

	got, _ := estimateOf(provider.FormatOpenAI, body)

	if got != 1600 {
		t.Errorf("InputEstimate = %d, want just the media part's 1600", got)
	}
}

// TestOutputCap: the client's own output limit, from the field each format
// keeps it in, times the number of choices it asked for.
func TestOutputCap(t *testing.T) {
	cases := []struct {
		name   string
		format provider.Format
		body   string
		want   int64
	}{
		{"openai max_completion_tokens", provider.FormatOpenAI, `{"max_completion_tokens":2000}`, 2000},
		{"openai max_tokens fallback", provider.FormatOpenAI, `{"max_tokens":1500}`, 1500},
		{"openai max_completion_tokens wins", provider.FormatOpenAI, `{"max_tokens":1500,"max_completion_tokens":2000}`, 2000},
		{"openai null max_completion_tokens falls back", provider.FormatOpenAI, `{"max_completion_tokens":null,"max_tokens":1500}`, 1500},
		{"openai n multiplies max_tokens", provider.FormatOpenAI, `{"max_tokens":100,"n":3}`, 300},
		{"openai n multiplies max_completion_tokens", provider.FormatOpenAI, `{"max_completion_tokens":100,"n":3,"max_tokens":9}`, 300},
		{"openai n alone sets no cap", provider.FormatOpenAI, `{"n":3}`, 0},
		{"openai no limit", provider.FormatOpenAI, `{"model":"gpt-4o"}`, 0},
		{"openai doesn't read Gemini's names", provider.FormatOpenAI, `{"generationConfig":{"maxOutputTokens":800}}`, 0},

		{"anthropic max_tokens", provider.FormatAnthropic, `{"max_tokens":1024}`, 1024},
		{"anthropic no limit", provider.FormatAnthropic, `{"model":"claude-sonnet-4"}`, 0},
		{"anthropic has no n", provider.FormatAnthropic, `{"max_tokens":1024,"n":2}`, 1024},
		{"anthropic doesn't read OpenAI's max_completion_tokens", provider.FormatAnthropic, `{"max_completion_tokens":1024}`, 0},

		{"gemini camelCase", provider.FormatGemini, `{"generationConfig":{"maxOutputTokens":800}}`, 800},
		{"gemini snake_case", provider.FormatGemini, `{"generation_config":{"max_output_tokens":800}}`, 800},
		{"gemini camelCase config, snake_case name", provider.FormatGemini, `{"generationConfig":{"max_output_tokens":800}}`, 800},
		{"gemini snake_case config, camelCase name", provider.FormatGemini, `{"generation_config":{"maxOutputTokens":800}}`, 800},
		{"gemini candidateCount multiplies", provider.FormatGemini, `{"generationConfig":{"maxOutputTokens":800,"candidateCount":2}}`, 1600},
		{"gemini candidate_count multiplies", provider.FormatGemini, `{"generation_config":{"max_output_tokens":800,"candidate_count":3}}`, 2400},
		{"gemini candidateCount alone sets no cap", provider.FormatGemini, `{"generationConfig":{"candidateCount":2}}`, 0},
		{"gemini limit outside the config", provider.FormatGemini, `{"maxOutputTokens":800,"max_tokens":800}`, 0},
		{"gemini no config", provider.FormatGemini, `{"contents":[]}`, 0},
		{"gemini config is a string", provider.FormatGemini, `{"generationConfig":"maxOutputTokens"}`, 0},
		{"gemini config is null", provider.FormatGemini, `{"generationConfig":null}`, 0},
		{"gemini config is an array", provider.FormatGemini, `{"generationConfig":[800]}`, 0},

		// A cap and a count that overflow int64 together must not wrap around
		// to a negative cap.
		{"product stops at the largest int64", provider.FormatOpenAI, `{"max_tokens":9223372036854775807,"n":2}`, math.MaxInt64},
		{"largest int64 alone", provider.FormatAnthropic, `{"max_tokens":9223372036854775807}`, math.MaxInt64},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, got := estimateOf(tc.format, tc.body); got != tc.want {
				t.Errorf("OutputCap = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestOutputCapIgnoresWrongTypes: a limit or a choice count that isn't a
// positive integer counts as unset: no cap, and one choice.
func TestOutputCapIgnoresWrongTypes(t *testing.T) {
	values := []string{`"100"`, `1.5`, `0`, `-5`, `null`, `true`, `[100]`, `{"v":100}`, `9223372036854775808`}

	cases := []struct {
		name   string
		format provider.Format
		body   string // %s is the value under test
		want   int64
	}{
		{"openai max_completion_tokens", provider.FormatOpenAI, `{"max_completion_tokens":%s}`, 0},
		{"openai max_tokens", provider.FormatOpenAI, `{"max_tokens":%s}`, 0},
		{"openai n", provider.FormatOpenAI, `{"max_tokens":100,"n":%s}`, 100},
		{"anthropic max_tokens", provider.FormatAnthropic, `{"max_tokens":%s}`, 0},
		{"gemini maxOutputTokens", provider.FormatGemini, `{"generationConfig":{"maxOutputTokens":%s}}`, 0},
		{"gemini max_output_tokens", provider.FormatGemini, `{"generation_config":{"max_output_tokens":%s}}`, 0},
		{"gemini candidateCount", provider.FormatGemini, `{"generationConfig":{"maxOutputTokens":100,"candidateCount":%s}}`, 100},
		{"gemini candidate_count", provider.FormatGemini, `{"generation_config":{"max_output_tokens":100,"candidate_count":%s}}`, 100},
	}

	for _, tc := range cases {
		for _, value := range values {
			body := fmt.Sprintf(tc.body, value)
			t.Run(tc.name+" = "+value, func(t *testing.T) {
				if _, got := estimateOf(tc.format, body); got != tc.want {
					t.Errorf("OutputCap of %s = %d, want %d", body, got, tc.want)
				}
			})
		}
	}
}
