// Package provider describes the upstream LLM providers the gateway forwards
// to, and the one thing that genuinely differs between them today: how each
// expects its API key to be presented.
//
// Everything else about a request passes through untouched. The gateway does
// not read or rewrite request or response bodies, so a provider's own API
// schema is never translated — clients speak each provider's native dialect.
package provider

import (
	"net/http"
	"net/url"
	"strings"
	"time"
)

// AuthStyle names the way a provider expects its API key to be presented.
//
// Go note: this is a "defined type" with string as its underlying type. It
// behaves like a string but is a distinct type, so you can't accidentally pass
// a plain string where an AuthStyle is wanted. Coming from Python, it's the
// lightweight version of an enum.
type AuthStyle string

const (
	// AuthBearer sends "Authorization: Bearer <key>" — OpenAI, Groq, Mistral,
	// Together, and most OpenAI-compatible providers.
	AuthBearer AuthStyle = "bearer"
	// AuthAPIKey sends "X-Api-Key: <key>" — Anthropic.
	AuthAPIKey AuthStyle = "x-api-key"
	// AuthGoogleKey sends "X-Goog-Api-Key: <key>" — Google Gemini.
	AuthGoogleKey AuthStyle = "x-goog-api-key"
	// AuthNone sends no key, for a keyless upstream such as a local Ollama.
	// The client's credentials are still stripped.
	AuthNone AuthStyle = "none"
)

// AuthStyles lists every supported style, for validation and error messages.
var AuthStyles = []AuthStyle{AuthBearer, AuthAPIKey, AuthGoogleKey, AuthNone}

// Valid reports whether a is a style the gateway knows how to apply.
func (a AuthStyle) Valid() bool {
	for _, known := range AuthStyles {
		if a == known {
			return true
		}
	}
	return false
}

// Format names the wire shape of a provider's API: which request and
// response bodies it speaks, and so which usage parser reads it. It names a
// shape, not a company: Groq is FormatOpenAI.
type Format string

const (
	// FormatOpenAI is OpenAI's Chat Completions shape, also served by Groq and
	// most OpenAI-compatible providers.
	FormatOpenAI Format = "openai"
	// FormatAnthropic is Anthropic's Messages shape.
	FormatAnthropic Format = "anthropic"
	// FormatGemini is Google Gemini's generateContent shape.
	FormatGemini Format = "gemini"
)

// Formats lists every supported format, for validation and error messages.
var Formats = []Format{FormatOpenAI, FormatAnthropic, FormatGemini}

// Valid reports whether f is a format the gateway can meter.
func (f Format) Valid() bool {
	for _, known := range Formats {
		if f == known {
			return true
		}
	}
	return false
}

// credentialHeaders is every header the gateway treats as carrying a secret.
//
// All of them are removed from an outbound request before the target
// provider's own key is applied. That matters: without it, a client holding an
// OpenAI key could send it to /anthropic/ and leak it to a provider it was
// never meant for.
var credentialHeaders = []string{
	"Authorization",
	"X-Api-Key",
	"X-Goog-Api-Key",
}

// credentialParams is every query parameter the gateway treats as carrying a
// secret. Gemini accepts its key as ?key=, so a client could put a key there
// instead of in a header.
var credentialParams = []string{"key"}

// Provider is one configured upstream.
type Provider struct {
	// Name is the routing prefix: requests to /<Name>/... go here.
	Name string
	// URL is the provider's base URL. It may carry a path (Groq's
	// OpenAI-compatible API lives at https://api.groq.com/openai), which is
	// prefixed onto every forwarded request path.
	URL *url.URL
	// Timeout bounds how long to wait for the provider to start responding.
	Timeout time.Duration
	// Key is the resolved API key — already expanded from its environment
	// variable by the config package, never a literal in config.yaml. It is
	// empty for AuthNone.
	Key string
	// Auth is how Key is presented to this provider.
	Auth AuthStyle
	// Format is the API shape this provider speaks.
	Format Format
	// Headers are static headers this provider requires, such as Anthropic's
	// "anthropic-version".
	Headers map[string]string
	// MaxConcurrency caps the requests in flight to this provider; 0 means no
	// cap. A request beyond it waits up to QueueTimeout for a slot.
	MaxConcurrency int
	QueueTimeout   time.Duration
	// HealthPath, when not empty, is the path on URL that health.Checker GETs
	// to check the provider is up.
	HealthPath string
}

// Apply swaps the client's credential for this provider's own, and adds any
// static headers the provider requires.
//
// Stripping and setting happen together on purpose: they are a single
// security-relevant operation, and splitting them across two call sites would
// make it possible to forget the strip and forward a client's key upstream.
func (p Provider) Apply(req *http.Request) {
	h := req.Header
	for _, name := range credentialHeaders {
		h.Del(name)
	}

	// Re-encoding the query reorders it, so only touch it when there is
	// something to remove.
	q := req.URL.Query()
	stripped := false
	for _, name := range credentialParams {
		if q.Has(name) {
			q.Del(name)
			stripped = true
		}
	}
	if stripped {
		req.URL.RawQuery = q.Encode()
	}

	switch p.Auth {
	case AuthBearer:
		h.Set("Authorization", "Bearer "+p.Key)
	case AuthAPIKey:
		h.Set("X-Api-Key", p.Key)
	case AuthGoogleKey:
		h.Set("X-Goog-Api-Key", p.Key)
	case AuthNone:
		// Nothing to set. The strip above still ran, so a keyless upstream
		// never sees the client's gateway key either.
	}

	// Static headers are set after the credential so a provider can never
	// accidentally be configured to overwrite its own auth header.
	for name, value := range p.Headers {
		if !isCredentialHeader(name) {
			h.Set(name, value)
		}
	}
}

// isCredentialHeader reports whether name is one of the headers Apply manages.
func isCredentialHeader(name string) bool {
	for _, c := range credentialHeaders {
		if strings.EqualFold(name, c) {
			return true
		}
	}
	return false
}
