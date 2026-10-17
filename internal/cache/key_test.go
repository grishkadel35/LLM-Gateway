package cache

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/grishkadel/llm-gateway/internal/provider"
)

// call is one call of Key, split into the pieces the tests change.
type call struct {
	tenant, provider string
	path, query      string // as Key sees them, after StripPrefix
	header           http.Header
	body             string
}

// base returns a plain request. It is a new one each time, so a test can
// change it, and each test changes only what it is about.
func base() call {
	return call{
		tenant:   "tn_a",
		provider: "anthropic",
		path:     "/v1/messages",
		query:    "beta=true&alt=json",
		header:   http.Header{"Anthropic-Version": {"2023-06-01"}},
		body:     `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
	}
}

func (c call) key() (string, bool) {
	r := httptest.NewRequest(http.MethodPost, c.path+"?"+c.query, nil)
	r.Header = c.header
	return Key(c.tenant, c.provider, r, []byte(c.body))
}

// mustDigest is key for a call that has to be cacheable. It returns the 64 hex
// digits and leaves off the "cache:{tenant}:" before them, so that comparing
// two of them compares the hashes: a part left out of the hash can't hide
// behind the tenant's name, which is in the key as well.
func (c call) mustDigest(t *testing.T) string {
	t.Helper()
	key, ok := c.key()
	if !ok {
		t.Fatalf("Key(%+v) is not cacheable, want a key", c)
	}
	return key[strings.LastIndex(key, ":")+1:]
}

// withBody and withQuery return changes that set one piece of a call.
func withBody(body string) func(*call)   { return func(c *call) { c.body = body } }
func withQuery(query string) func(*call) { return func(c *call) { c.query = query } }

func TestKeyShape(t *testing.T) {
	key, ok := base().key()
	if !ok || !regexp.MustCompile(`^cache:\{tn_a\}:[0-9a-f]{64}$`).MatchString(key) {
		t.Errorf("Key = %q, %v; want cache:{tn_a}: and 64 lowercase hex digits", key, ok)
	}
	if again, _ := base().key(); again != key {
		t.Errorf("the same request got %q, then %q", key, again)
	}
}

// TestKeyBody: each row sends two bodies in an otherwise identical request, and
// says whether they get the same key.
func TestKeyBody(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b string
		same bool
	}{
		// The same JSON value, written differently.
		{"whitespace", `{"a":1,"b":[1,2]}`, " {\n\t\"a\" : 1 ,\r\n \"b\":[ 1 , 2 ] }\n", true},
		{"key order at the top level", `{"a":1,"b":2}`, `{"b":2,"a":1}`, true},
		{"key order inside messages[]",
			`{"messages":[{"role":"user","content":"hi"}]}`,
			`{"messages":[{"content":"hi","role":"user"}]}`, true},
		{"key order inside generationConfig",
			`{"generationConfig":{"topK":3,"topP":0.5}}`,
			`{"generationConfig":{"topP":0.5,"topK":3}}`, true},
		{"string escapes", `{"a":"<é/"}`, `{"a":"\u003c\u00e9\/"}`, true},
		{"an emoji and its surrogate pair", `{"a":"😀"}`, `{"a":"\ud83d\ude00"}`, true},
		{"duplicate keys: the last one wins", `{"a":1,"a":2}`, `{"a":2}`, true},

		// Different JSON values.
		{"a string", `{"a":"x"}`, `{"a":"y"}`, false},
		{"a nested string", `{"a":{"b":[{"c":"x"}]}}`, `{"a":{"b":[{"c":"y"}]}}`, false},
		{"a number", `{"a":1}`, `{"a":2}`, false},
		{"a boolean", `{"a":true}`, `{"a":false}`, false},
		{"null and no key", `{"a":null}`, `{}`, false},
		{"array order", `[1,2]`, `[2,1]`, false},
		{"duplicate keys: not the first one", `{"a":1,"a":2}`, `{"a":1}`, false},
		{"an escaped backslash is not an escape", `{"a":"\\u003c"}`, `{"a":"<"}`, false},

		// Numbers keep their spelling.
		{"1 and 1.0", `{"a":1}`, `{"a":1.0}`, false},
		{"integers a float64 can't tell apart", `{"a":9007199254740993}`, `{"a":9007199254740992}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := base(), base()
			a.body, b.body = tc.a, tc.b
			if same := a.mustDigest(t) == b.mustDigest(t); same != tc.same {
				t.Errorf("%s and %s: same key = %v, want %v", tc.a, tc.b, same, tc.same)
			}
		})
	}
}

// TestKeyQuery: like TestKeyBody, for the query.
func TestKeyQuery(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b string
		same bool
	}{
		{"parameter order", "a=1&b=2", "b=2&a=1", true},
		{"escapes", "a=%41%20b", "a=A+b", true},
		{"a key= parameter", "a=1&key=gw_SECRET", "a=1", true},
		{"a key= parameter with no value", "a=1&key=", "a=1", true},
		{"another key= value", "a=1&key=gw_ONE", "a=1&key=gw_TWO", true},
		{"a repeated parameter's other values stay put", "a=1&b=2&a=3", "b=2&a=1&a=3", true},

		{"a value", "a=1", "a=2", false},
		{"a parameter added", "a=1", "a=1&b=2", false},
		{"a repeated parameter's value order", "a=1&a=2", "a=2&a=1", false},
		{"a name that only starts like a credential's", "a=1&keys=x", "a=1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := base(), base()
			a.query, b.query = tc.a, tc.b
			if same := a.mustDigest(t) == b.mustDigest(t); same != tc.same {
				t.Errorf("?%s and ?%s: same key = %v, want %v", tc.a, tc.b, same, tc.same)
			}
		})
	}
}

// TestKeyDropsCredentialParams ties the key to provider.CredentialParams,
// which Apply strips: a name added to that list is dropped here too.
func TestKeyDropsCredentialParams(t *testing.T) {
	want := base().mustDigest(t)
	for _, name := range provider.CredentialParams {
		c := base()
		c.query += "&" + name + "=gw_SECRET"
		if got := c.mustDigest(t); got != want {
			t.Errorf("?%s: the key changed with %s=, want it dropped", c.query, name)
		}
	}
}

// TestKeyRequest: each row changes one thing in the plain request, and says
// whether the key changes with it.
func TestKeyRequest(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*call)
		same   bool
	}{
		{"headers other than the Anthropic ones", func(c *call) {
			c.header.Set("Authorization", "Bearer gw_SECRET")
			c.header.Set("X-Api-Key", "gw_SECRET")
			c.header.Set("X-Request-Id", "req_1")
		}, true},

		// The tenant is in the hash as well as in the key's name.
		{"the tenant", func(c *call) { c.tenant = "tn_b" }, false},
		{"the provider", func(c *call) { c.provider = "groq" }, false},
		{"the path", func(c *call) { c.path = "/v1/other" }, false},
		// The decoded path is /v1/messages again. The proxy forwards it escaped.
		{"the escaped path", func(c *call) { c.path = "/v1%2Fmessages" }, false},
		{"anthropic-version", func(c *call) { c.header.Set("Anthropic-Version", "2024-01-01") }, false},
		{"anthropic-beta", func(c *call) { c.header.Set("Anthropic-Beta", "tools-2024-04-04") }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := base()
			tc.change(&changed)
			if same := changed.mustDigest(t) == base().mustDigest(t); same != tc.same {
				t.Errorf("same key = %v, want %v", same, tc.same)
			}
		})
	}
}

// TestKeyHeaderValues: every request below must get a key of its own. A header
// that is absent, empty, on one line with a comma or on two lines is a
// different request, and so is a value sent in the other Anthropic header.
func TestKeyHeaderValues(t *testing.T) {
	const version, beta = "Anthropic-Version", "Anthropic-Beta"
	headers := []http.Header{
		{},
		{version: {""}},
		{version: {"a"}},
		{version: {"a", "b"}},
		{beta: {""}},
		{beta: {"a"}},
		{beta: {"a,b"}},
		{beta: {"a, b"}},
		{beta: {"a", "b"}},
		{beta: {"b", "a"}},
		{version: {"a"}, beta: {"b"}},
		{version: {"b"}, beta: {"a"}},
		{version: {"a"}, beta: {"a"}},
	}
	seen := map[string]http.Header{}
	for _, h := range headers {
		c := base()
		c.header = h
		digest := c.mustDigest(t)
		if other, ok := seen[digest]; ok {
			t.Errorf("headers %v and %v share a key", other, h)
		}
		seen[digest] = h
	}
}

// TestKeyPartsDontRunTogether: a byte moved from one part to the next makes a
// different request, though both write the same characters in a row.
func TestKeyPartsDontRunTogether(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b func(*call)
	}{
		{"tenant and provider",
			func(c *call) { c.tenant, c.provider = "ab", "c" },
			func(c *call) { c.tenant, c.provider = "a", "bc" }},
		{"provider and path",
			func(c *call) { c.provider, c.path = "anthropic", "/v1/messages" },
			func(c *call) { c.provider, c.path = "anthropic/v1", "/messages" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := base(), base()
			tc.a(&a)
			tc.b(&b)
			if a.mustDigest(t) == b.mustDigest(t) {
				t.Error("the requests share a key")
			}
		})
	}
}

// TestKeyNotCacheable: a request that could share its key with a different one
// gets none.
func TestKeyNotCacheable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*call)
	}{
		{"a bad escape in the query", withQuery("a=%zz")},
		{"a ; in the query", withQuery("a=1;b=2")},

		{"a body that isn't JSON", withBody(`hello`)},
		{"an empty body", withBody(``)},
		{"a body of only whitespace", withBody(" \n")},
		{"a body cut short", withBody(`{"a":`)},
		{"trailing data", withBody(`{} {}`)},
		{"trailing text", withBody(`{}x`)},
		{"a stray } after the value", withBody(`{}}`)},
		{"a stray ] after the value", withBody(`{}]`)},

		{"invalid UTF-8 in a string", withBody("{\"a\":\"\xff\"}")},
		{"invalid UTF-8 in a key", withBody("{\"\xff\":1}")},
		{"invalid UTF-8 outside a string", withBody("{}\xff")},
		{"a cut short UTF-8 character", withBody("{\"a\":\"\xe2\x82\"}")},
		{"a surrogate written in UTF-8", withBody("{\"a\":\"\xed\xa0\x80\"}")},

		{`\ud800 on its own`, withBody(`{"a":"\ud800"}`)},
		{`\udc00 on its own`, withBody(`{"a":"\udc00"}`)},
		{`\ud800 and then a letter`, withBody(`{"a":"\ud800A"}`)},
		{`\ud800 in capitals`, withBody(`{"a":"\uD800"}`)},
		{`\ud800 and then another escape`, withBody(`{"a":"\ud800\u0041"}`)},
		{`\ud800 and then \n`, withBody(`{"a":"\ud800\n"}`)},
		{"a low surrogate before a high one", withBody(`{"a":"\udc00\ud800"}`)},
		{"a high surrogate twice", withBody(`{"a":"\ud800\ud800\udc00"}`)},
		{"a high surrogate before a pair", withBody(`{"a":"\ud800\ud83d\ude00"}`)},
		{"a pair, then a low surrogate", withBody(`{"a":"\ud83d\ude00\udc00"}`)},
		{"a lone surrogate in a key", withBody(`{"\ud800":1}`)},
		{"a lone surrogate after other escapes",
			withBody(`{"a":"\n\ud83d\ude00\\\u0041\udc00"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			tc.change(&c)
			if key, ok := c.key(); ok {
				t.Errorf("Key(%+v) = %q, want not cacheable", c, key)
			}
		})
	}
}

// TestKeyCacheable: requests that look like those above, but are fine.
func TestKeyCacheable(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"an object", `{}`},
		{"an array", `[]`},
		{"a string", `"text"`},
		{"a number", `1`},
		{"null", `null`},
		{"whitespace after the value", "{}\n \t\r\n"},
		{"a pair of surrogates", `{"a":"\ud83d\ude00"}`},
		{"two pairs in a row", `{"a":"\ud83d\ude00\ud83d\ude00"}`},
		{"a pair in capitals", `{"a":"\uD83D\uDE00"}`},
		{`an escaped backslash before "ud800"`, `{"a":"\\ud800"}`},
		{`an escaped backslash before "u"`, `{"a":"\\\\u"}`},
		{"U+FFFD itself", `{"a":"\ufffd"}`},
		{"escapes just outside the surrogates", `{"a":"\ud7ff\ue000"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			c.body = tc.body
			c.mustDigest(t)
		})
	}
}

// TestKeyBodySizeLimit: a body of MaxRequestBody bytes gets a key, and one
// byte more doesn't, however valid it is.
func TestKeyBodySizeLimit(t *testing.T) {
	c := base()
	c.body = jsonOfSize(MaxRequestBody)
	if _, ok := c.key(); !ok {
		t.Errorf("a body of MaxRequestBody (%d) bytes is not cacheable, want a key", MaxRequestBody)
	}
	c.body = jsonOfSize(MaxRequestBody + 1)
	if _, ok := c.key(); ok {
		t.Errorf("a body of MaxRequestBody+1 (%d) bytes got a key, want not cacheable", MaxRequestBody+1)
	}
}

// jsonOfSize returns a JSON object of exactly n bytes, for n of at least 8.
func jsonOfSize(n int) string {
	return `{"a":"` + strings.Repeat("x", n-8) + `"}`
}

// chatBody returns a chat request of about size bytes: many short messages,
// so decoding it builds thousands of small maps and strings.
func chatBody(size int) []byte {
	var sb strings.Builder
	sb.WriteString(`{"model":"gpt-5","temperature":0,"messages":[`)
	for i := 0; sb.Len() < size; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, `{"role":"user","content":"Message %d: the quick brown fox jumps over the lazy dog, then does it all again."}`, i)
	}
	sb.WriteString(`]}`)
	return []byte(sb.String())
}

// imageBody returns a chat request of about size bytes, nearly all of it one
// base64 image in a data URL, the way OpenAI's image_url takes it.
func imageBody(size int) []byte {
	raw := make([]byte, size/4*3)
	rand.Read(raw)
	return []byte(`{"model":"gpt-5","temperature":0,"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"What is in this image?"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,` +
		base64.StdEncoding.EncodeToString(raw) + `"}}]}]}`)
}

// BenchmarkKey times Key on the two bodies that cost it most: a 100 KB chat,
// and a 5 MB image.
//
// Go note: go test runs a function named BenchmarkXxx(b *testing.B) only with
// -bench. for b.Loop() runs its body as often as it takes to get a steady
// timing, and doesn't count the setup before the loop. b.Run makes a
// sub-benchmark, as t.Run makes a subtest. b.ReportAllocs adds the bytes and
// allocations per call to the output, and b.SetBytes(n) says a call handles n
// bytes, which adds the throughput in MB/s.
func BenchmarkKey(b *testing.B) {
	for _, bench := range []struct {
		name string
		body []byte
	}{
		{"chat-100KB", chatBody(100 << 10)},
		{"image-5MB", imageBody(5 << 20)},
	} {
		b.Run(bench.name, func(b *testing.B) {
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			b.ReportAllocs()
			b.SetBytes(int64(len(bench.body)))
			for b.Loop() {
				if _, ok := Key("tn_bench", "openai", r, bench.body); !ok {
					b.Fatal("not cacheable")
				}
			}
		})
	}
}
