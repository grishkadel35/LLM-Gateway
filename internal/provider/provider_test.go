package provider

import (
	"net/http"
	"testing"
)

// TestApplySetsProviderCredential checks each auth style writes the right
// header in the right format.
//
// Go note: this is a table-driven test, the idiomatic Go pattern for testing
// many inputs. t.Run creates a named subtest per case, so a failure names the
// case that broke.
func TestApplySetsProviderCredential(t *testing.T) {
	cases := []struct {
		name       string
		auth       AuthStyle
		wantHeader string
		wantValue  string
	}{
		{"bearer", AuthBearer, "Authorization", "Bearer sk-provider-key"},
		{"anthropic", AuthAPIKey, "X-Api-Key", "sk-provider-key"},
		{"gemini", AuthGoogleKey, "X-Goog-Api-Key", "sk-provider-key"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := Provider{Name: tc.name, Key: "sk-provider-key", Auth: tc.auth}

			h := http.Header{}
			p.Apply(h)

			if got := h.Get(tc.wantHeader); got != tc.wantValue {
				t.Errorf("%s = %q, want %q", tc.wantHeader, got, tc.wantValue)
			}
		})
	}
}

// TestApplyStripsClientCredentials is the security-critical one: whatever key
// the client sent must never survive onto the outbound request, no matter which
// header it arrived in. Otherwise a client's OpenAI key could be forwarded to
// Anthropic simply by calling /anthropic/.
func TestApplyStripsClientCredentials(t *testing.T) {
	for _, tc := range []struct {
		name string
		auth AuthStyle
	}{
		{"bearer", AuthBearer},
		{"anthropic", AuthAPIKey},
		{"gemini", AuthGoogleKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			// The client sends credentials in every shape at once.
			h.Set("Authorization", "Bearer sk-CLIENT-LEAK")
			h.Set("X-Api-Key", "sk-CLIENT-LEAK")
			h.Set("X-Goog-Api-Key", "sk-CLIENT-LEAK")

			p := Provider{Name: tc.name, Key: "sk-gateway-key", Auth: tc.auth}
			p.Apply(h)

			for _, header := range credentialHeaders {
				if got := h.Get(header); got == "sk-CLIENT-LEAK" {
					t.Errorf("%s still carries the client's key %q", header, got)
				}
			}
		})
	}
}

// TestApplySetsStaticHeaders covers Anthropic's required anthropic-version.
func TestApplySetsStaticHeaders(t *testing.T) {
	p := Provider{
		Name:    "anthropic",
		Key:     "sk-ant",
		Auth:    AuthAPIKey,
		Headers: map[string]string{"anthropic-version": "2023-06-01"},
	}

	h := http.Header{}
	p.Apply(h)

	if got := h.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want %q", got, "2023-06-01")
	}
	if got := h.Get("X-Api-Key"); got != "sk-ant" {
		t.Errorf("X-Api-Key = %q, want %q", got, "sk-ant")
	}
}

// TestApplyStaticHeadersCannotOverrideCredential guards against a
// misconfiguration: a static header that would clobber the auth header the
// gateway just set. The credential must win.
func TestApplyStaticHeadersCannotOverrideCredential(t *testing.T) {
	p := Provider{
		Name: "openai",
		Key:  "sk-real",
		Auth: AuthBearer,
		// Someone puts an Authorization header in the provider's config.
		Headers: map[string]string{"authorization": "Bearer sk-WRONG"},
	}

	h := http.Header{}
	p.Apply(h)

	if got := h.Get("Authorization"); got != "Bearer sk-real" {
		t.Errorf("Authorization = %q, want the provider's own key %q", got, "Bearer sk-real")
	}
}

func TestAuthStyleValid(t *testing.T) {
	for _, a := range AuthStyles {
		if !a.Valid() {
			t.Errorf("AuthStyle(%q).Valid() = false, want true", a)
		}
	}
	for _, bad := range []AuthStyle{"", "basic", "Bearer", "api-key"} {
		if bad.Valid() {
			t.Errorf("AuthStyle(%q).Valid() = true, want false", bad)
		}
	}
}
