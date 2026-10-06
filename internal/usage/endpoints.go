// Package usage reads token usage from provider traffic.
//
// It also decides which endpoints tenants may reach at all: only those whose
// usage it can read. An endpoint is added to the allowlist together with its
// parser, never before.
package usage

import (
	"net/http"
	"regexp"

	"github.com/grishkadel/llm-gateway/internal/apierror"
	"github.com/grishkadel/llm-gateway/internal/provider"
)

// geminiGenerate matches Gemini's two metered endpoints, under the stable and
// beta API versions. The model is a single path segment: tuned models
// (tunedModels/...) and other resources are not metered.
var geminiGenerate = regexp.MustCompile(`^/(v1|v1beta)/models/[^/:]+:(generateContent|streamGenerateContent)$`)

// Metered reports whether a request to path, relative to the provider's base
// URL, is one whose token usage the gateway can read for this format.
//
// Everything else is refused. An endpoint the parser can't read (OpenAI's
// /v1/responses, embeddings, batches, files, Gemini's Interactions API...)
// would otherwise let a tenant spend without being metered, around every
// rate limit and budget.
func Metered(format provider.Format, method, path string) bool {
	if method != http.MethodPost {
		return false
	}
	switch format {
	case provider.FormatOpenAI:
		return path == "/v1/chat/completions"
	case provider.FormatAnthropic:
		return path == "/v1/messages"
	case provider.FormatGemini:
		return geminiGenerate.MatchString(path)
	}
	return false
}

// RequireMetered returns middleware that answers 403 unmetered_endpoint to
// any request Metered refuses, without forwarding it. It must see the path
// after the provider prefix is stripped.
func RequireMetered(format provider.Format) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !Metered(format, r.Method, r.URL.Path) {
				apierror.Write(w, http.StatusForbidden, "unmetered_endpoint",
					r.Method+" "+r.URL.Path+" is not a metered endpoint for "+string(format)+
						"-format providers; the gateway only forwards endpoints whose token usage it can record", nil)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
