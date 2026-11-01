package middleware

import (
	"bytes"
	"context"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"testing"

	"github.com/grishkadel/llm-gateway/internal/provider"
	"github.com/grishkadel/llm-gateway/internal/usage"
)

// withRecord returns req with a Record on its context, as Logging puts one
// there, and the Record.
func withRecord(req *http.Request) (*http.Request, *Record) {
	rec := &Record{}
	return req.WithContext(context.WithValue(req.Context(), recordKey{}, rec)), rec
}

// lineFields are the request line's fields that come from the Record.
var lineFields = []string{"tenant_id", "provider", "model", "fallback_provider", "input_tokens", "cached_input_tokens",
	"cache_write_tokens", "output_tokens", "cost_micros", "cached", "streamed"}

// recordedFields returns the fields of entry, a request line, that come from
// the Record. A field logged as null is there, with a nil value.
func recordedFields(entry map[string]any) map[string]any {
	got := map[string]any{}
	for _, k := range lineFields {
		if v, ok := entry[k]; ok {
			got[k] = v
		}
	}
	return got
}

// TestLoggingLineFromRecord: what the layers inside record goes on the
// request line, each field only when it is known. The usage fields come from
// the usage last reported, unless Budget then refused a fallback's attempt
// itself. A request nothing recorded anything for gets the line it always
// did.
func TestLoggingLineFromRecord(t *testing.T) {
	cost, zero := int64(144), int64(0)
	answered := &UsageReport{Provider: "openai", Result: usage.Result{
		Usage:  usage.Usage{Model: "gpt-4o-2024-08-06", Input: 15, CachedInput: 5, CacheWrite: 3, CacheWrite1h: 4, Output: 10},
		Status: http.StatusOK, Complete: true,
	}, CostMicros: &cost}
	// with returns a copy of answered, changed by change.
	with := func(change func(*UsageReport)) *UsageReport {
		u := *answered
		change(&u)
		return &u
	}
	asked := &usage.Request{Model: "gpt-4o"}
	// The fields every request with an answer gets, from answered.
	usageFields := map[string]any{"input_tokens": 15.0, "cached_input_tokens": 5.0, "cache_write_tokens": 7.0,
		"output_tokens": 10.0, "cost_micros": 144.0, "cached": false}

	cases := []struct {
		name   string
		record Record
		want   map[string]any
	}{
		{"nothing recorded", Record{}, map[string]any{}},
		{"refused by Auth", Record{Provider: "openai"}, map[string]any{"provider": "openai"}},
		{"refused before the body was read", Record{Provider: "openai", TenantID: "tn_acme"},
			map[string]any{"provider": "openai", "tenant_id": "tn_acme"}},
		{"refused by the gateway after reading the body", Record{Provider: "openai", TenantID: "tn_acme", Request: asked, RateLimited: true},
			map[string]any{"provider": "openai", "tenant_id": "tn_acme", "model": "gpt-4o", "streamed": false}},
		{"no model named", Record{Provider: "openai", TenantID: "tn_acme", Request: &usage.Request{Stream: true}},
			map[string]any{"provider": "openai", "tenant_id": "tn_acme", "streamed": true}},
		{"answered", Record{Provider: "openai", TenantID: "tn_acme", Request: asked, Usage: answered},
			union(usageFields, map[string]any{"provider": "openai", "tenant_id": "tn_acme", "model": "gpt-4o", "streamed": false})},
		// The report's, so the usage row's: an upstream may stream what the
		// client didn't ask to have streamed.
		{"streamed, as the report says", Record{Provider: "openai", TenantID: "tn_acme", Request: asked,
			Usage: with(func(u *UsageReport) { u.Streamed = true })},
			union(usageFields, map[string]any{"provider": "openai", "tenant_id": "tn_acme", "model": "gpt-4o", "streamed": true})},
		{"cost not known", Record{Provider: "openai", TenantID: "tn_acme", Request: asked,
			Usage: with(func(u *UsageReport) { u.CostMicros = nil })},
			union(usageFields, map[string]any{"provider": "openai", "tenant_id": "tn_acme", "model": "gpt-4o", "streamed": false,
				"cost_micros": nil})},
		{"answered by the fallback", Record{Provider: "ollama", TenantID: "tn_acme", Request: asked,
			Usage: with(func(u *UsageReport) { u.Provider = "groq" })},
			union(usageFields, map[string]any{"provider": "ollama", "tenant_id": "tn_acme", "model": "gpt-4o", "streamed": false,
				"fallback_provider": "groq"})},
		{"a cache hit", Record{Provider: "openai", TenantID: "tn_acme", Request: asked, Cache: "hit",
			Usage: &UsageReport{Provider: "openai", Result: usage.Result{Status: http.StatusOK, Complete: true, Cached: true}, CostMicros: &zero}},
			map[string]any{"provider": "openai", "tenant_id": "tn_acme", "model": "gpt-4o", "streamed": false, "input_tokens": 0.0,
				"cached_input_tokens": 0.0, "cache_write_tokens": 0.0, "output_tokens": 0.0, "cost_micros": 0.0, "cached": true}},
		// The client got the fallback's 402, not the failed attempt's answer.
		{"a fallback's attempt refused by Budget", Record{Provider: "ollama", TenantID: "tn_acme", Request: asked,
			Usage:         &UsageReport{Provider: "ollama", Result: usage.Result{Status: http.StatusInternalServerError, Complete: true, Streamed: true}, CostMicros: &zero},
			BudgetRefusal: &BudgetRefusal{Provider: "groq", Reason: "exceeded"}},
			map[string]any{"provider": "ollama", "tenant_id": "tn_acme", "model": "gpt-4o", "streamed": false}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rec := RecordFrom(r.Context())
				if rec == nil {
					t.Fatal("RecordFrom() found no Record inside Logging")
				}
				start := rec.Start
				*rec = tc.record
				rec.Start = start
			})
			Logging(slog.New(slog.NewJSONHandler(&logs, nil)))(inner).ServeHTTP(httptest.NewRecorder(),
				httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", nil))

			entry := logLine(t, &logs)
			if got := recordedFields(entry); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("fields from the Record = %v\nwant %v", got, tc.want)
			}
			// The fields every line had before the Record, and only those.
			if len(tc.want) == 0 {
				want := []string{"bytes", "duration_ms", "level", "method", "msg", "path", "remote_addr", "request_id", "status", "time"}
				if got := slices.Sorted(maps.Keys(entry)); !slices.Equal(got, want) {
					t.Errorf("line fields = %q, want %q", got, want)
				}
			}
		})
	}
}

// union returns a map with the entries of a and b.
func union(a, b map[string]any) map[string]any {
	m := maps.Clone(a)
	maps.Copy(m, b)
	return m
}

// TestLoggingStartsTheRecord: the Record a request gets starts as it arrives,
// empty, and is its own.
func TestLoggingStartsTheRecord(t *testing.T) {
	var records []*Record
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		records = append(records, RecordFrom(r.Context()))
	})
	h := Logging(discard)(inner)
	for range 2 {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}

	if len(records) != 2 || records[0] == nil || records[0] == records[1] {
		t.Fatalf("Records = %v, want one of its own for each request", records)
	}
	if rec := *records[0]; rec.Start.IsZero() || !reflect.DeepEqual(rec, Record{Start: rec.Start}) {
		t.Errorf("Record = %+v, want empty but for its start", rec)
	}
}

// TestLoggingLineAfterAbort: a report made while ReverseProxy aborts a stream,
// from its deferred Close, still reaches the line, which is written once
// everything inside Logging has unwound.
func TestLoggingLineAfterAbort(t *testing.T) {
	var logs bytes.Buffer
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			RecordFrom(r.Context()).Usage = &UsageReport{Provider: "openai", Result: usage.Result{Usage: usage.Usage{Input: 1000}, Streamed: true}}
		}()
		panic(http.ErrAbortHandler)
	})

	func() {
		defer func() { _ = recover() }()
		Logging(slog.New(slog.NewJSONHandler(&logs, nil)))(Route("openai")(inner)).ServeHTTP(httptest.NewRecorder(),
			httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", nil))
	}()

	want := map[string]any{"provider": "openai", "input_tokens": 1000.0, "cached_input_tokens": 0.0, "cache_write_tokens": 0.0,
		"output_tokens": 0.0, "cost_micros": nil, "cached": false}
	if got := recordedFields(logLine(t, &logs)); !reflect.DeepEqual(got, want) {
		t.Errorf("fields from the Record = %v\nwant %v", got, want)
	}
}

// TestRouteAndRecordRequest: Route records its route, and RecordRequest the
// Request ReadBody made, on the Record. Without one, each just goes on.
func TestRouteAndRecordRequest(t *testing.T) {
	var seen *usage.Request
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = usage.RequestFrom(r.Context())
		w.WriteHeader(http.StatusTeapot)
	})
	h := Route("openai")(usage.ReadBody(provider.FormatOpenAI, 1<<20)(RecordRequest(next)))

	req, rec := withRecord(chatPost(`{"model":"gpt-4o","stream":true,"messages":[]}`))
	h.ServeHTTP(httptest.NewRecorder(), req)
	if rec.Provider != "openai" || rec.Request == nil || rec.Request != seen || rec.Request.Model != "gpt-4o" || !rec.Request.Stream {
		t.Errorf("Record = %+v with Request %+v, want route openai and ReadBody's Request, for gpt-4o, streamed", rec, rec.Request)
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, chatPost(`{"model":"gpt-4o","messages":[]}`))
	if w.Code != http.StatusTeapot {
		t.Errorf("without a Record: status = %d, want %d", w.Code, http.StatusTeapot)
	}
}
