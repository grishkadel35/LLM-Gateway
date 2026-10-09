package usage

import (
	"encoding/json"
	"math"

	"github.com/grishkadel/llm-gateway/internal/provider"
)

// mediaPartTokens is what the estimate charges for each media part, however
// big it is. It is just above what a 1024×1024 image costs at default settings
// on current models (OpenAI 1,229, Gemini 3 1,120, Anthropic 1,369). Large
// images on Claude 4.7+ cost up to 4,784, and PDFs, audio and video far more;
// reconciliation charges the difference after the response.
const mediaPartTokens = 1600

// estimate sizes a request before any provider has counted it: the values for
// Request.InputEstimate and Request.OutputCap. fields is the body decoded into
// its top-level keys, nil when the body isn't a JSON object. It only reads the
// body.
func estimate(format provider.Format, body []byte, fields map[string]json.RawMessage) (inputEstimate, outputCap int64) {
	if fields == nil {
		return textTokens(int64(len(body))), 0
	}

	// fields decoded, so the body is valid JSON. The one error left is a number
	// outside float64's range, which leaves the rest of doc decoded.
	var doc any
	_ = json.Unmarshal(body, &doc)
	var found mediaCount
	found.walk(doc, false)

	// payload sums decoded strings, which can outgrow their bytes in the body
	// (invalid UTF-8 decodes to 3-byte U+FFFD), so it can exceed the body.
	textBytes := int64(len(body)) - found.payload
	if textBytes < 0 {
		textBytes = 0
	}
	return textTokens(textBytes) + mediaPartTokens*found.parts, outputLimit(format, fields)
}

// textTokens estimates the tokens in n bytes of text: about 4 bytes each,
// rounded up.
func textTokens(n int64) int64 { return (n + 3) / 4 }

// mediaCount tallies the media parts in a decoded JSON document and the bytes
// of the strings inside them.
type mediaCount struct {
	parts   int64
	payload int64
}

// walk adds everything in v to c, however deep it sits (messages, tool
// results, system arrays...). inPart is true inside a media part: its strings
// are payload, and it holds no further parts.
//
// Go note: a type switch (`switch v := v.(type)`) runs the case that matches
// the type an interface value holds, and v has that type inside the case.
// JSON decoded into `any` only ever holds nil, bool, float64, string, []any
// or map[string]any, so the cases cover strings and the two containers and
// ignore the rest.
func (c *mediaCount) walk(v any, inPart bool) {
	switch v := v.(type) {
	case string:
		if inPart {
			c.payload += int64(len(v))
		}
	case map[string]any:
		if !inPart && isMediaPart(v) {
			c.parts++
			inPart = true
		}
		for _, child := range v {
			c.walk(child, inPart)
		}
	case []any:
		for _, child := range v {
			c.walk(child, inPart)
		}
	}
}

// isMediaPart reports whether obj is an image, audio, document or file part.
// One check serves all three formats, because their request bodies don't
// share these names.
func isMediaPart(obj map[string]any) bool {
	switch obj["type"] {
	case "image_url", "input_audio", "file": // OpenAI
		return true
	case "image", "document": // Anthropic; a document whose source is text counts as text
		source, _ := obj["source"].(map[string]any)
		return source["type"] != "text"
	}
	// Gemini has no type: a part holds its media under one of these keys, in
	// either of proto JSON's spellings.
	for _, key := range []string{"inlineData", "inline_data", "fileData", "file_data"} {
		if _, ok := obj[key]; ok {
			return true
		}
	}
	return false
}

// outputLimit is the output limit the client set, times the number of choices
// it asked for, or 0 when it set none. A value that isn't a positive integer
// counts as unset.
func outputLimit(format provider.Format, fields map[string]json.RawMessage) int64 {
	var maxTokens, choices int64
	switch format {
	case provider.FormatOpenAI:
		maxTokens = positive(fields, "max_completion_tokens", "max_tokens")
		choices = positive(fields, "n")
	case provider.FormatAnthropic:
		maxTokens = positive(fields, "max_tokens")
	case provider.FormatGemini:
		// Proto JSON accepts either spelling of each name.
		raw := fields["generationConfig"]
		if raw == nil {
			raw = fields["generation_config"]
		}
		var config map[string]json.RawMessage
		_ = json.Unmarshal(raw, &config) // anything but an object leaves it nil
		maxTokens = positive(config, "maxOutputTokens", "max_output_tokens")
		choices = positive(config, "candidateCount", "candidate_count")
	}

	if choices == 0 {
		choices = 1
	}
	if maxTokens > math.MaxInt64/choices {
		return math.MaxInt64 // stop at the largest cap rather than wrap around to a negative one
	}
	return maxTokens * choices
}

// positive returns the first of names that obj holds as a positive integer, or
// 0. A string, a fraction, null or a number out of range doesn't count.
func positive(obj map[string]json.RawMessage, names ...string) int64 {
	for _, name := range names {
		var n int64
		if json.Unmarshal(obj[name], &n) == nil && n > 0 {
			return n
		}
	}
	return 0
}
