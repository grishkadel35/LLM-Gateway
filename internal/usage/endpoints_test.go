package usage

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/grishkadel/llm-gateway/internal/provider"
)

func TestMetered(t *testing.T) {
	cases := []struct {
		format       provider.Format
		method, path string
		want         bool
	}{
		{provider.FormatOpenAI, http.MethodPost, "/v1/chat/completions", true},
		{provider.FormatAnthropic, http.MethodPost, "/v1/messages", true},
		{provider.FormatGemini, http.MethodPost, "/v1beta/models/gemini-3.8-flash:generateContent", true},
		{provider.FormatGemini, http.MethodPost, "/v1beta/models/gemini-3.8-flash:streamGenerateContent", true},
		{provider.FormatGemini, http.MethodPost, "/v1/models/gemini-3.8-flash:generateContent", true},
		{provider.FormatGemini, http.MethodPost, "/v1/models/gemini-3.8-flash:streamGenerateContent", true},

		// Endpoints whose usage no parser reads yet: forwarding them would
		// let a tenant spend around every limit.
		{provider.FormatOpenAI, http.MethodPost, "/v1/responses", false},
		{provider.FormatOpenAI, http.MethodPost, "/v1/embeddings", false},
		{provider.FormatOpenAI, http.MethodPost, "/v1/completions", false},
		{provider.FormatOpenAI, http.MethodGet, "/v1/models", false},
		{provider.FormatAnthropic, http.MethodPost, "/v1/messages/count_tokens", false},
		{provider.FormatAnthropic, http.MethodPost, "/v1/messages/batches", false},
		{provider.FormatGemini, http.MethodPost, "/v1beta/models/gemini-3.8-flash:countTokens", false},
		{provider.FormatGemini, http.MethodPost, "/v1beta/models/gemini-3.8-flash:embedContent", false},
		{provider.FormatGemini, http.MethodPost, "/v1beta/models/gemini-3.8-flash:batchGenerateContent", false},
		{provider.FormatGemini, http.MethodPost, "/v1beta/interactions", false},
		{provider.FormatGemini, http.MethodPost, "/v1beta/tunedModels/x:generateContent", false},
		{provider.FormatGemini, http.MethodPost, "/v1alpha/models/x:generateContent", false},
		{provider.FormatGemini, http.MethodPost, "/v1beta/models/:generateContent", false},
		{provider.FormatGemini, http.MethodPost, "/v1beta/models/a/b:generateContent", false},

		// Right path, wrong method or shape.
		{provider.FormatOpenAI, http.MethodGet, "/v1/chat/completions", false},
		{provider.FormatOpenAI, http.MethodPost, "/v1/chat/completions/", false},
		{provider.FormatAnthropic, http.MethodGet, "/v1/messages", false},
		{provider.FormatGemini, http.MethodGet, "/v1beta/models/x:generateContent", false},

		// A path is only metered for the format whose parser reads it.
		{provider.FormatAnthropic, http.MethodPost, "/v1/chat/completions", false},
		{provider.FormatOpenAI, http.MethodPost, "/v1/messages", false},
		{provider.FormatOpenAI, http.MethodPost, "/v1beta/models/x:generateContent", false},
	}

	for _, tc := range cases {
		if got := Metered(tc.format, tc.method, tc.path); got != tc.want {
			t.Errorf("Metered(%s, %s %s) = %v, want %v", tc.format, tc.method, tc.path, got, tc.want)
		}
	}
}

func TestRequireMeteredRejectsWithoutForwarding(t *testing.T) {
	reached := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true })

	rec := httptest.NewRecorder()
	RequireMetered(provider.FormatOpenAI)(next).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/embeddings", nil))

	if reached {
		t.Error("unmetered request was forwarded")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	var body struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q is not JSON: %v", rec.Body, err)
	}
	if body.Error.Type != "unmetered_endpoint" || !strings.Contains(body.Error.Message, "POST /v1/embeddings") {
		t.Errorf("error = %+v, want type unmetered_endpoint naming POST /v1/embeddings", body.Error)
	}
}

func TestRequireMeteredForwardsMetered(t *testing.T) {
	reached := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true })

	RequireMetered(provider.FormatAnthropic)(next).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", nil))

	if !reached {
		t.Error("metered request was not forwarded")
	}
}
