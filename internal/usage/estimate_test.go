package usage

import (
	"bytes"
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
// flat 1,600 tokens, plus only the bytes around its payload / 4. The payload
// is measured in the body's own bytes, escapes included, so the image costs
// the same however the client's JSON encoder escaped it.
func TestEstimateInlineImage(t *testing.T) {
	raw := make([]byte, 1<<20)
	for i := range raw {
		raw[i] = byte(i)
	}
	image := base64.StdEncoding.EncodeToString(raw) // every base64 character, + and / included

	cases := []struct {
		name    string
		format  provider.Format
		body    string
		payload string // the raw value under the image part's payload key
	}{
		{"openai data URL", provider.FormatOpenAI,
			`{"model":"gpt-4o","messages":[{"role":"user","content":[{"type":"text","text":"What is in this image?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,` + image + `"}}]}]}`,
			`{"url":"data:image/png;base64,` + image + `"}`},
		{"anthropic base64 source", provider.FormatAnthropic,
			`{"model":"claude-sonnet-4","max_tokens":1024,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + image + `"}},{"type":"text","text":"What is in this image?"}]}]}`,
			`{"type":"base64","media_type":"image/png","data":"` + image + `"}`},
		{"gemini inlineData", provider.FormatGemini,
			`{"contents":[{"role":"user","parts":[{"text":"What is in this image?"},{"inlineData":{"mimeType":"image/png","data":"` + image + `"}}]}]}`,
			`{"mimeType":"image/png","data":"` + image + `"}`},
	}

	for _, tc := range cases {
		want := 1600 + ceilQuarter(len(tc.body)-len(tc.payload))
		// PHP's json_encode writes / as \/, and .NET's default encoder writes +
		// as a six-byte escape: a backslash, then u002B. Outside the payload,
		// these bodies have neither.
		encodings := []struct{ name, body string }{
			{"compact", tc.body},
			{"php", strings.ReplaceAll(tc.body, "/", `\/`)},
			{"dotnet", strings.ReplaceAll(tc.body, "+", `\`+"u002B")},
		}
		for _, enc := range encodings {
			t.Run(tc.name+" "+enc.name, func(t *testing.T) {
				if enc.name != "compact" && len(enc.body) <= len(tc.body) {
					t.Fatalf("%s escaping changed nothing; want a payload it escapes", enc.name)
				}
				got, _ := estimateOf(tc.format, enc.body)
				if got != want {
					t.Errorf("InputEstimate = %d, want %d", got, want)
				}
				if whole := ceilQuarter(len(enc.body)); got > whole/100 {
					t.Errorf("InputEstimate = %d, want far below the %d that /4 of the whole body gives", got, whole)
				}
			})
		}
	}
}

// mediaPartCases hold media parts in each format, wherever they sit in the
// JSON. A part's payload is the raw value under the key that holds its media,
// from its first byte to its last.
var mediaPartCases = []struct {
	name    string
	format  provider.Format
	body    string
	parts   int64
	payload []string // the raw value under each part's payload key
}{
	{"openai image by URL", provider.FormatOpenAI,
		`{"model":"gpt-4o","messages":[{"role":"user","content":[{"type":"text","text":"Describe it."},{"type":"image_url","image_url":{"url":"https://example.com/cat.png","detail":"high"}}]}]}`,
		1, []string{`{"url":"https://example.com/cat.png","detail":"high"}`}},
	{"openai two images", provider.FormatOpenAI,
		`{"model":"gpt-4o","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}},{"type":"image_url","image_url":{"url":"https://example.com/b.png"}}]}]}`,
		2, []string{`{"url":"https://example.com/a.png"}`, `{"url":"https://example.com/b.png"}`}},
	{"openai audio", provider.FormatOpenAI,
		`{"model":"gpt-4o-audio-preview","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"UklGRiQAAABXQVZF","format":"wav"}}]}]}`,
		1, []string{`{"data":"UklGRiQAAABXQVZF","format":"wav"}`}},
	{"openai file", provider.FormatOpenAI,
		`{"model":"gpt-4o","messages":[{"role":"user","content":[{"type":"file","file":{"filename":"report.pdf","file_data":"data:application/pdf;base64,JVBERi0xLjQ="}},{"type":"text","text":"Summarize."}]}]}`,
		1, []string{`{"filename":"report.pdf","file_data":"data:application/pdf;base64,JVBERi0xLjQ="}`}},
	// The type can come after the payload, and the spaces around the payload
	// aren't part of it.
	{"openai type after the image", provider.FormatOpenAI,
		`{"model":"gpt-4o","messages":[{"role":"user","content":[{"image_url" : {"url":"https://example.com/cat.png"} , "type":"image_url"}]}]}`,
		1, []string{`{"url":"https://example.com/cat.png"}`}},
	// A repeated key counts by its last value, as a decoder reads it.
	{"openai repeated keys", provider.FormatOpenAI,
		`{"model":"gpt-4o","messages":[{"role":"user","content":[{"type":"text","image_url":{"url":"https://example.com/a.png"},"type":"image_url","image_url":{"url":"https://example.com/b.png"}}]}]}`,
		1, []string{`{"url":"https://example.com/b.png"}`}},
	// A part inside a payload is payload, not a part of its own.
	{"openai part inside a payload", provider.FormatOpenAI,
		`{"model":"gpt-4o","messages":[{"role":"user","content":[{"type":"file","file":{"file_data":"JVBERi0xLjQ=","preview":{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}}}]}]}`,
		1, []string{`{"file_data":"JVBERi0xLjQ=","preview":{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}}`}},
	{"anthropic image by URL", provider.FormatAnthropic,
		`{"model":"claude-sonnet-4","max_tokens":1024,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://example.com/cat.png"}},{"type":"text","text":"Describe it."}]}]}`,
		1, []string{`{"type":"url","url":"https://example.com/cat.png"}`}},
	{"anthropic image in a tool_result", provider.FormatAnthropic,
		`{"model":"claude-sonnet-4","max_tokens":1024,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01","content":[{"type":"text","text":"Screenshot taken."},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}}]}]}]}`,
		1, []string{`{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}`}},
	{"anthropic PDF document", provider.FormatAnthropic,
		`{"model":"claude-sonnet-4","max_tokens":1024,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0xLjQ="}},{"type":"text","text":"Summarize."}]}]}`,
		1, []string{`{"type":"base64","media_type":"application/pdf","data":"JVBERi0xLjQ="}`}},
	// A source with no type still holds media.
	{"anthropic source with no type", provider.FormatAnthropic,
		`{"model":"claude-sonnet-4","max_tokens":1024,"messages":[{"role":"user","content":[{"source":{"media_type":"image/png","data":"iVBORw0KGgo="},"type":"image"}]}]}`,
		1, []string{`{"media_type":"image/png","data":"iVBORw0KGgo="}`}},
	// Its text is text, however long: only a source that isn't text is media.
	{"anthropic document with a text source", provider.FormatAnthropic,
		`{"model":"claude-sonnet-4","max_tokens":1024,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"text","media_type":"text/plain","data":"The quick brown fox jumps over the lazy dog."}}]}]}`,
		0, nil},
	// A content document is text too, read like any other JSON: the image in it
	// is a part of its own, and its text is text.
	{"anthropic content document holding an image", provider.FormatAnthropic,
		`{"model":"claude-sonnet-4","max_tokens":1024,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"content","content":[{"type":"text","text":"Chapter 1"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}}]}}]}]}`,
		1, []string{`{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}`}},
	{"gemini inline_data", provider.FormatGemini,
		`{"contents":[{"role":"user","parts":[{"text":"Describe it."},{"inline_data":{"mime_type":"image/png","data":"iVBORw0KGgo="}}]}]}`,
		1, []string{`{"mime_type":"image/png","data":"iVBORw0KGgo="}`}},
	{"gemini fileData", provider.FormatGemini,
		`{"contents":[{"role":"user","parts":[{"fileData":{"mimeType":"video/mp4","fileUri":"https://generativelanguage.googleapis.com/v1beta/files/abc123"}},{"text":"Summarize."}]}]}`,
		1, []string{`{"mimeType":"video/mp4","fileUri":"https://generativelanguage.googleapis.com/v1beta/files/abc123"}`}},
	{"gemini file_data", provider.FormatGemini,
		`{"contents":[{"parts":[{"file_data":{"mime_type":"application/pdf","file_uri":"gs://bucket/report.pdf"}}]}]}`,
		1, []string{`{"mime_type":"application/pdf","file_uri":"gs://bucket/report.pdf"}`}},
	// An object is one part however many media keys it has. Its payload is
	// the first in payloadKeys' order.
	{"gemini part with two media keys", provider.FormatGemini,
		`{"contents":[{"parts":[{"fileData":{"fileUri":"gs://bucket/a.pdf"},"inlineData":{"mimeType":"image/png","data":"iVBORw0KGgo="}}]}]}`,
		1, []string{`{"mimeType":"image/png","data":"iVBORw0KGgo="}`}},
}

// TestEstimateMediaParts: each media part costs 1,600 tokens, and its payload
// is left out of the /4.
func TestEstimateMediaParts(t *testing.T) {
	for _, tc := range mediaPartCases {
		t.Run(tc.name, func(t *testing.T) {
			want := mediaCount{parts: tc.parts, payload: int64(lenSum(tc.payload))}
			if found := scanMedia(tc.format, []byte(tc.body)); found != want {
				t.Errorf("found %+v, want %+v", found, want)
			}

			wantEstimate := 1600*tc.parts + ceilQuarter(len(tc.body)-lenSum(tc.payload))
			if got, _ := estimateOf(tc.format, tc.body); got != wantEstimate {
				t.Errorf("InputEstimate = %d, want %d (%d media parts)", got, wantEstimate, tc.parts)
			}
		})
	}
}

// TestEstimatePartsOfOtherFormats: a format's part shapes mean nothing in
// another format's body, so there they are text like the rest.
func TestEstimatePartsOfOtherFormats(t *testing.T) {
	for _, tc := range mediaPartCases {
		for _, format := range provider.Formats {
			if format == tc.format {
				continue
			}
			t.Run(tc.name+" as "+string(format), func(t *testing.T) {
				if got, _ := estimateOf(format, tc.body); got != ceilQuarter(len(tc.body)) {
					t.Errorf("InputEstimate = %d, want ceil(%d / 4) = %d", got, len(tc.body), ceilQuarter(len(tc.body)))
				}
			})
		}
	}
}

// textBodyCases hold no media: system prompts, tool definitions and every
// other field are text. Words and schema names that only look like media
// don't make a part, and nor does JSON the client defines, like a tool's
// input or schema, whatever shape it has.
var textBodyCases = []struct {
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
	{"anthropic tool_use input", provider.FormatAnthropic,
		`{"model":"claude-sonnet-4","max_tokens":1024,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_01","name":"review","input":{"files":[{"type":"file","path":"src/main.go"},{"type":"file","path":"src/util.go"}],"cover":{"type":"image","caption":"The build pipeline, drawn as boxes."},"logo":{"type":"image","source":"logo.png"}}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01","content":"Reviewed 2 files."}]}]}`},
	{"openai tool schema", provider.FormatOpenAI,
		`{"model":"gpt-4o","messages":[{"role":"user","content":"Upload the report."}],"tools":[{"type":"function","function":{"name":"upload","description":"Upload a file.","parameters":{"type":"object","properties":{"fileData":{"type":"string","description":"The file, base64."},"file":{"type":"string"}},"required":["fileData"]}}}]}`},
	{"gemini function declaration", provider.FormatGemini,
		`{"contents":[{"role":"user","parts":[{"text":"Attach the report."}]}],"tools":[{"functionDeclarations":[{"name":"attach","description":"Attach a file.","parameters":{"type":"OBJECT","properties":{"file_data":{"type":"STRING"},"inlineData":{"type":"OBJECT","properties":{"data":{"type":"STRING"}}}}}}]}]}`},
	// An OpenAI part's type names the key that holds its media.
	{"openai image_url with no image_url key", provider.FormatOpenAI,
		`{"model":"gpt-4o","messages":[{"role":"user","content":[{"type":"image_url","url":"https://example.com/cat.png"},{"type":"text","text":"Describe it."}]}]}`},
	// A content document is text, whichever form its content takes.
	{"anthropic content document", provider.FormatAnthropic,
		`{"model":"claude-sonnet-4","max_tokens":1024,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"content","content":[{"type":"text","text":"Chapter 1. It was a bright cold day in April."},{"type":"text","text":"Chapter 2. The clocks were striking thirteen."}]},"title":"A novel","citations":{"enabled":true}},{"type":"text","text":"Summarize the chapters."}]}]}`},
	{"anthropic content document with string content", provider.FormatAnthropic,
		`{"model":"claude-sonnet-4","max_tokens":1024,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"content","content":"It was a bright cold day in April, and the clocks were striking thirteen."}},{"type":"text","text":"Summarize it."}]}]}`},
}

// TestEstimateTextBody: in a body with no media, every byte counts.
func TestEstimateTextBody(t *testing.T) {
	for _, tc := range textBodyCases {
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

// TestEstimateNumberOutsideFloat64: a number too big for a float64 is still
// valid JSON, and one a decoder into float64 would fail on. The scan skips
// numbers without decoding them, so it still finds the media after one.
func TestEstimateNumberOutsideFloat64(t *testing.T) {
	body := `{"temperature":1e999,"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/cat.png"}}]}]}`

	got, _ := estimateOf(provider.FormatOpenAI, body)

	want := 1600 + ceilQuarter(len(body)-len(`{"url":"https://example.com/cat.png"}`))
	if got != want {
		t.Errorf("InputEstimate = %d, want %d", got, want)
	}
}

// TestEstimateInvalidUTF8Payload: decoding would turn each invalid byte into a
// 3-byte U+FFFD, but the payload is measured in the body's own bytes. A
// payload full of them leaves the real text around it counted in full.
func TestEstimateInvalidUTF8Payload(t *testing.T) {
	text := strings.Repeat("Describe this image in detail. ", 100)
	payload := `{"url":"` + strings.Repeat("\xff", 1000) + `"}`
	body := `{"messages":[{"role":"user","content":[{"type":"text","text":"` + text + `"},{"type":"image_url","image_url":` + payload + `}]}]}`

	got, _ := estimateOf(provider.FormatOpenAI, body)

	if want := 1600 + ceilQuarter(len(body)-len(payload)); got != want {
		t.Errorf("InputEstimate = %d, want %d", got, want)
	}
}

// TestScanMediaAllocatesNothing: the scan reads the body in place, so even a
// body that is nearly all structure costs no memory beyond its own bytes.
// Decoding one into `any` took about 50 times its size.
//
// Go note: testing.AllocsPerRun calls a function several times and returns
// the average number of heap allocations each call made.
func TestScanMediaAllocatesNothing(t *testing.T) {
	image := base64.StdEncoding.EncodeToString(make([]byte, 1<<20))
	bodies := []struct {
		name string
		body []byte
	}{
		{"structure", []byte(`{"model":"m","messages":[` + strings.Repeat(`1,[true,null],{"type":"image","source":{"type":"text"},"inlineData":{"data":"x"},"k\"ey":-2.5e3},`, 1<<15) + `1]}`)},
		{"images", []byte(`{"messages":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + image + `"}},{"type":"image","source":{"type":"base64","data":"` + image + `"}},{"inlineData":{"mimeType":"image/png","data":"` + image + `"}}]}`)},
	}

	for _, tc := range bodies {
		for _, format := range provider.Formats {
			if n := testing.AllocsPerRun(5, func() { scanMedia(format, tc.body) }); n != 0 {
				t.Errorf("scanning %s as %s: %v allocations, want 0", tc.name, format, n)
			}
		}
	}
}

// FuzzScanMedia: whatever JSON object a client sends, the payloads stay within
// the body, and whitespace never changes what is a part.
//
// Go note: a fuzz test runs as an ordinary test over the inputs given to f.Add.
// `go test -fuzz=FuzzScanMedia` then keeps making new inputs from them, looking
// for one that fails, and saves any it finds under testdata/fuzz, where plain
// `go test` reruns it from then on.
func FuzzScanMedia(f *testing.F) {
	for _, tc := range mediaPartCases {
		f.Add([]byte(tc.body))
	}
	for _, tc := range textBodyCases {
		f.Add([]byte(tc.body))
	}

	f.Fuzz(func(t *testing.T, body []byte) {
		// Only a JSON object is scanned, as in ReadBody.
		var fields map[string]json.RawMessage
		if json.Unmarshal(body, &fields) != nil || fields == nil {
			return
		}
		var indented bytes.Buffer
		if err := json.Indent(&indented, body, "", "  "); err != nil {
			t.Fatalf("json.Indent: %v", err)
		}

		for _, format := range provider.Formats {
			found := scanMedia(format, body)
			if found.payload < 0 || found.payload > int64(len(body)) {
				t.Errorf("as %s: payload = %d bytes, want 0 to the body's %d", format, found.payload, len(body))
			}
			if again := scanMedia(format, indented.Bytes()); again.parts != found.parts {
				t.Errorf("as %s: %d parts, but %d once indented", format, found.parts, again.parts)
			}
		}
	})
}

// FuzzScanMediaStrings: escaped, a string or a key can hold anything: quotes,
// backslashes, brackets, another part's JSON. Wherever a client puts them, the
// scan finds exactly the part around them, and exactly its payload.
func FuzzScanMediaStrings(f *testing.F) {
	f.Add("What is in this image?", "iVBORw0KGgo=")
	f.Add(`"}]},{"type":"image_url","image_url":{"url":"`, `\"\\`)
	f.Add("type", "text")
	f.Add("source", `{"type":"content"}`)
	f.Add("fileData", "inline_data")

	// $a and $b are the two strings, $payload the expanded payload.
	cases := []struct {
		format        provider.Format
		body, payload string
	}{
		{provider.FormatOpenAI,
			`{"model":$a,"messages":[{"role":"user","content":[{"type":"text","text":$b},{$a:$b,"type":"image_url","image_url":$payload}]}],$a:$b}`,
			`{$a:$b,"url":$b}`},
		{provider.FormatAnthropic,
			`{"model":$a,"system":$b,"messages":[{"role":"user","content":[{"type":"text","text":$a},{$a:$b,"type":"image","source":$payload}]}]}`,
			`{$a:$b,"type":"base64","media_type":"image/png","data":$b}`},
		{provider.FormatGemini,
			`{"systemInstruction":{"parts":[{"text":$b}]},"contents":[{"role":"user","parts":[{"text":$a},{$a:$b,"inlineData":$payload}]}]}`,
			`{$a:$b,"mimeType":"image/png","data":$b}`},
	}

	f.Fuzz(func(t *testing.T, a, b string) {
		qa, _ := json.Marshal(a)
		qb, _ := json.Marshal(b)
		for _, tc := range cases {
			payload := strings.NewReplacer("$a", string(qa), "$b", string(qb)).Replace(tc.payload)
			body := strings.NewReplacer("$a", string(qa), "$b", string(qb), "$payload", payload).Replace(tc.body)
			if !json.Valid([]byte(body)) {
				t.Fatalf("as %s: built invalid JSON %s", tc.format, body)
			}

			want := mediaCount{parts: 1, payload: int64(len(payload))}
			if found := scanMedia(tc.format, []byte(body)); found != want {
				t.Errorf("as %s: found %+v in %s, want %+v", tc.format, found, body, want)
			}
		}
	})
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
