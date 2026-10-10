package usage

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"

	"github.com/grishkadel/llm-gateway/internal/provider"
)

// mediaPartTokens is what the estimate charges for each media part, however
// big it is. It is just above what a 1024×1024 image costs at default settings
// on current models (OpenAI 1,229, Gemini 3 1,120, Anthropic 1,369). Large
// images on Claude 4.7+ cost up to 4,784, and PDFs, audio and video far more;
// reconciliation charges the difference after the response.
const mediaPartTokens = 1600

// estimate sizes a request before any provider has counted it: the values for
// Request.InputEstimate, Request.OutputCap and Request.Choices. fields is the
// body decoded into its top-level keys, nil when the body isn't a JSON object.
// It only reads the body.
func estimate(format provider.Format, body []byte, fields map[string]json.RawMessage) (inputEstimate, outputCap, choices int64) {
	if fields == nil {
		return textTokens(int64(len(body))), 0, 1
	}

	// fields decoded, so the body is valid JSON nested at most 10,000 deep,
	// which is all scanMedia needs. The payloads it finds are separate pieces
	// of the body, so they never add up to more than the body.
	found := scanMedia(format, body)
	outputCap, choices = outputLimit(format, fields)
	return textTokens(int64(len(body))-found.payload) + mediaPartTokens*found.parts, outputCap, choices
}

// textTokens estimates the tokens in n bytes of text: about 4 bytes each,
// rounded up.
func textTokens(n int64) int64 { return (n + 3) / 4 }

// mediaCount tallies media parts and the bytes of their payloads.
type mediaCount struct {
	parts   int64
	payload int64
}

// scanMedia counts the media parts in body, a JSON object, wherever they sit
// (messages, tool results, system arrays...). A part's payload is the value
// under its payload key, measured in the body's own bytes from the value's
// first byte to its last, escapes included. body must be valid JSON nested at
// most 10,000 deep, as json.Unmarshal checks.
func scanMedia(format provider.Format, body []byte) mediaCount {
	s := scanner{format: format, keys: payloadKeys[format], b: body}
	s.space()
	found, _ := s.object()
	return found
}

// payloadKeys are the keys under which each format holds a part's media. An
// object keeps a slot for each, so a format can have 4 at most.
var payloadKeys = map[provider.Format][]string{
	provider.FormatOpenAI:    {"image_url", "input_audio", "file"},
	provider.FormatAnthropic: {"source"},
	provider.FormatGemini:    {"inlineData", "inline_data", "fileData", "file_data"},
}

// isPart reports whether an object whose "type" is typ, holding value under
// the payload key key, is a media part. Only the format's own shapes count,
// since JSON the client defines (tool inputs, schemas) can look like any of
// them.
func isPart(format provider.Format, typ []byte, key string, value summary) bool {
	switch format {
	case provider.FormatOpenAI:
		// {"type":"image_url","image_url":{"url":...}}: the type names the
		// key, whatever it holds.
		return string(typ) == key
	case provider.FormatAnthropic:
		// {"type":"image","source":{"type":"base64",...}}. A document whose
		// source is text or content is text, and any image in it is a part
		// of its own.
		return (string(typ) == "image" || string(typ) == "document") &&
			value.object && string(value.typ) != "text" && string(value.typ) != "content"
	case provider.FormatGemini:
		// A part has no type: {"inlineData":{"mimeType":...,"data":...}} or
		// {"fileData":{"mimeType":...,"fileUri":...}}, in either of proto
		// JSON's spellings.
		if key == "inlineData" || key == "inline_data" {
			return value.object && value.data
		}
		return value.object && value.fileURI
	}
	return false
}

// summary is what isPart needs to know about a value. Only an object's has
// anything set.
type summary struct {
	object  bool   // whether the value is an object
	typ     []byte // its "type", if that's a string: the bytes between the quotes
	data    bool   // it has a "data" key
	fileURI bool   // it has a "fileUri" or "file_uri" key
}

// scanner reads a JSON body's bytes in place, without decoding or copying
// them, so it allocates nothing however the body is built.
type scanner struct {
	format provider.Format
	keys   []string // payloadKeys[format]
	b      []byte
	i      int // the next byte to read
}

// value reads the value at s.i, returning the media parts in it and, for an
// object, its summary.
//
// Go note: value, object and array call each other once per level of nesting.
// A goroutine's stack starts small and grows as needed, so that is safe even
// at the decoder's limit of 10,000 levels.
func (s *scanner) value() (mediaCount, summary) {
	switch s.b[s.i] {
	case '{':
		return s.object()
	case '[':
		return s.array(), summary{}
	case '"':
		s.str()
	default:
		s.literal()
	}
	return mediaCount{}, summary{}
}

// slot is the last value an object holds under one of the payload keys.
type slot struct {
	seen   bool
	size   int64      // its bytes in the body
	nested mediaCount // the parts inside it
	value  summary
}

// object reads the object at s.i. When it is a media part, its payload value
// counts whole, in place of any parts inside it, so payloads never overlap.
// Its type can come after its payload, so that is decided at the closing
// brace. Like a decoder, it goes by the last value of a repeated key.
func (s *scanner) object() (mediaCount, summary) {
	var found mediaCount
	obj := summary{object: true}
	var slots [4]slot // one for each of s.keys

	s.i++ // {
	s.space()
	for s.b[s.i] != '}' {
		key := s.str()
		s.space()
		s.i++ // :
		s.space()
		start := s.i
		nested, value := s.value()
		found.parts += nested.parts
		found.payload += nested.payload

		// Go note: converting a []byte to a string normally copies it. Where
		// the string is only compared, as in this switch, the loop below and
		// isPart, the compiler compares the bytes in place, so the scan
		// allocates nothing.
		switch string(key) {
		case "type":
			obj.typ = nil
			if s.b[start] == '"' {
				obj.typ = s.b[start+1 : s.i-1]
			}
		case "data":
			obj.data = true
		case "fileUri", "file_uri":
			obj.fileURI = true
		}
		for n, k := range s.keys {
			if string(key) == k {
				slots[n] = slot{seen: true, size: int64(s.i - start), nested: nested, value: value}
			}
		}

		s.space()
		if s.b[s.i] == ',' {
			s.i++
			s.space()
		}
	}
	s.i++ // }

	for n, k := range s.keys {
		if p := &slots[n]; p.seen && isPart(s.format, obj.typ, k, p.value) {
			found.parts += 1 - p.nested.parts
			found.payload += p.size - p.nested.payload
			break // one part per object, however many of its keys qualify
		}
	}
	return found, obj
}

// array reads the array at s.i, returning the media parts in it.
func (s *scanner) array() mediaCount {
	var found mediaCount
	s.i++ // [
	s.space()
	for s.b[s.i] != ']' {
		nested, _ := s.value()
		found.parts += nested.parts
		found.payload += nested.payload
		s.space()
		if s.b[s.i] == ',' {
			s.i++
			s.space()
		}
	}
	s.i++ // ]
	return found
}

// str reads the string at s.i, returning its bytes between the quotes as they
// are in the body, escapes and all.
func (s *scanner) str() []byte {
	s.i++ // "
	start := s.i
	for {
		end := s.i + bytes.IndexByte(s.b[s.i:], '"')
		s.i = end + 1
		// The quote ends the string unless an odd number of backslashes
		// escape it. The opening quote stops the count.
		backslashes := 0
		for s.b[end-1-backslashes] == '\\' {
			backslashes++
		}
		if backslashes%2 == 0 {
			return s.b[start:end]
		}
	}
}

// literal skips the number, true, false or null at s.i.
func (s *scanner) literal() {
	for {
		switch s.b[s.i] {
		case ',', '}', ']', ' ', '\t', '\n', '\r':
			return
		}
		s.i++
	}
}

// space skips the whitespace at s.i, if any.
func (s *scanner) space() {
	for {
		switch s.b[s.i] {
		case ' ', '\t', '\n', '\r':
			s.i++
		default:
			return
		}
	}
}

// outputLimit reads what the client asked for: choices is the number of
// choices, at least 1, and outputCap the output limit it set times choices, or
// 0 when it set none. One function returns both, so a request's cap and its
// count can't come from different readings of the body, and Gemini's config
// is decoded once. A value that isn't a positive integer counts as unset.
func outputLimit(format provider.Format, fields map[string]json.RawMessage) (outputCap, choices int64) {
	var maxTokens int64
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
		maxTokens = positiveProto(config, "maxOutputTokens", "max_output_tokens")
		choices = positiveProto(config, "candidateCount", "candidate_count")
	}

	if choices == 0 {
		choices = 1
	}
	if maxTokens > math.MaxInt64/choices {
		return math.MaxInt64, choices // stop at the largest cap rather than wrap around to a negative one
	}
	return maxTokens * choices, choices
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

// positiveProto is positive for Gemini, whose API reads the request as proto3
// JSON. There an int32 field takes a number with no fractional part (8192,
// 8192.0, 8.192e3) or a string holding one ("8192"), up to math.MaxInt32.
// Anything else doesn't count: a fraction, null, a bool, an array, an object, a
// number out of range, or a string that isn't wholly a number ("abc", " 8192").
// OpenAI's and Anthropic's APIs refuse these forms, so they keep using positive.
//
// Go note: a json.Number holds a number's digits as written, unread.
// encoding/json fills one from a JSON number, or from a JSON string only if the
// whole string is a valid JSON number, and fails on anything else. strconv then
// reads it: a float64 holds every int32 exactly, and reads exponent forms. (A
// fraction too small for a float64 to see beside the integer, like 1e-7 near
// 2^31, is lost, which is fine for an estimate.)
func positiveProto(obj map[string]json.RawMessage, names ...string) int64 {
	for _, name := range names {
		var num json.Number
		if json.Unmarshal(obj[name], &num) != nil {
			continue
		}
		// null leaves num empty, which ParseFloat refuses.
		f, err := strconv.ParseFloat(string(num), 64)
		if err == nil && f > 0 && f <= math.MaxInt32 && f == math.Trunc(f) {
			return int64(f)
		}
	}
	return 0
}
