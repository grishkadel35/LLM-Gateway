package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/grishkadel/llm-gateway/internal/db"
	"github.com/grishkadel/llm-gateway/internal/db/dbtest"
	"github.com/grishkadel/llm-gateway/internal/tenant"
)

// fakeKeys is a KeyLookup over a fixed set of keys. err, when set, is
// returned for every lookup, standing in for a database that is down.
type fakeKeys struct {
	keys map[string]tenant.APIKey
	err  error
}

func (f fakeKeys) Lookup(ctx context.Context, key string) (tenant.Tenant, tenant.APIKey, error) {
	if f.err != nil {
		return tenant.Tenant{}, tenant.APIKey{}, f.err
	}
	k, ok := f.keys[key]
	if !ok {
		return tenant.Tenant{}, tenant.APIKey{}, tenant.ErrNotFound
	}
	return tenant.Tenant{ID: k.TenantID}, k, nil
}

var testKeys = fakeKeys{keys: map[string]tenant.APIKey{
	"gw_good": {ID: 7, TenantID: "tn_acme"},
}}

// serveAuth runs req through Auth in front of a handler that records whether
// it was reached and what tenant it saw.
func serveAuth(t *testing.T, keys KeyLookup, req *http.Request) (rec *httptest.ResponseRecorder, reached bool, got tenant.Tenant, gotKey tenant.APIKey) {
	t.Helper()

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		var ok bool
		got, gotKey, ok = TenantFrom(r.Context())
		if !ok {
			t.Error("TenantFrom() found no tenant in an authenticated request")
		}
	})

	rec = httptest.NewRecorder()
	Auth(keys, slog.New(slog.NewTextHandler(io.Discard, nil)))(next).ServeHTTP(rec, req)
	return rec, reached, got, gotKey
}

// TestAuthAcceptsEachNativeCredentialHeader: clients keep their provider's
// SDK and only swap in a gateway key, so the key arrives in whichever header
// that SDK uses.
func TestAuthAcceptsEachNativeCredentialHeader(t *testing.T) {
	cases := []struct {
		sdk    string
		header string
		value  string
	}{
		{"openai", "Authorization", "Bearer gw_good"},
		{"openai lowercase scheme", "Authorization", "bearer gw_good"},
		// RFC 6750: "Bearer" 1*SP b64token, so any run of spaces is valid.
		{"several spaces after the scheme", "Authorization", "Bearer   gw_good"},
		{"anthropic", "X-Api-Key", "gw_good"},
		{"gemini", "X-Goog-Api-Key", "gw_good"},
	}

	for _, tc := range cases {
		t.Run(tc.sdk, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			req.Header.Set(tc.header, tc.value)

			rec, reached, got, gotKey := serveAuth(t, testKeys, req)

			if !reached {
				t.Fatalf("request was not forwarded: status %d, body %s", rec.Code, rec.Body)
			}
			if got.ID != "tn_acme" {
				t.Errorf("tenant ID = %q, want %q", got.ID, "tn_acme")
			}
			if gotKey.ID != 7 {
				t.Errorf("key ID = %d, want 7", gotKey.ID)
			}
		})
	}
}

// TestAuthRejectsWithoutForwarding: a request that fails authentication must
// never reach the provider, where it would spend the gateway's keys.
func TestAuthRejectsWithoutForwarding(t *testing.T) {
	cases := []struct {
		name   string
		header string
		value  string
	}{
		{"no credential", "", ""},
		{"unknown key", "Authorization", "Bearer gw_unknown"},
		{"empty bearer", "Authorization", "Bearer "},
		// Only the Bearer scheme carries a key; anything else isn't one.
		{"basic scheme", "Authorization", "Basic gw_good"},
		{"empty x-api-key", "X-Api-Key", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			if tc.header != "" {
				req.Header.Set(tc.header, tc.value)
			}

			rec, reached, _, _ := serveAuth(t, testKeys, req)

			if reached {
				t.Error("unauthenticated request was forwarded")
			}
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
			}
			if rec.Header().Get("WWW-Authenticate") == "" {
				t.Error("401 has no WWW-Authenticate header")
			}
			assertErrorType(t, rec, "invalid_api_key")
		})
	}
}

// TestAuthDoesNotAcceptQueryKey: Gemini also takes ?key=, but a gateway key
// in a URL ends up in every proxy and load balancer log on the way.
func TestAuthDoesNotAcceptQueryKey(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/x:generateContent?key=gw_good", nil)

	rec, reached, _, _ := serveAuth(t, testKeys, req)

	if reached || rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, forwarded = %v; want 401, not forwarded", rec.Code, reached)
	}
}

// TestAuthLookupFailureIsNot401: when the key store is down, telling the
// client its key is invalid would send them off rotating a working key.
func TestAuthLookupFailureIsNot401(t *testing.T) {
	keys := fakeKeys{err: errors.New("connection refused")}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer gw_good")

	rec, reached, _, _ := serveAuth(t, keys, req)

	if reached {
		t.Error("request was forwarded without a successful lookup")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	assertErrorType(t, rec, "auth_unavailable")
}

func TestTenantFromUnauthenticatedContext(t *testing.T) {
	if _, _, ok := TenantFrom(context.Background()); ok {
		t.Error("TenantFrom() found a tenant in a context Auth never touched")
	}
}

// TestAuthWithTenantStore runs Auth against the real store, so the rules the
// fake encodes (revoked keys are not found) are checked against Postgres.
func TestAuthWithTenantStore(t *testing.T) {
	conn := dbtest.New(t)
	ctx := context.Background()
	if _, err := db.Migrate(ctx, conn); err != nil {
		t.Fatalf("Migrate() returned error: %v", err)
	}

	store := tenant.NewStore(conn)
	created, key, err := store.Create(ctx, "acme")
	if err != nil {
		t.Fatalf("Create() returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("X-Api-Key", key.Plaintext)
	rec, reached, got, gotKey := serveAuth(t, store, req)
	if !reached {
		t.Fatalf("valid key rejected: status %d, body %s", rec.Code, rec.Body)
	}
	if got.ID != created.ID || gotKey.ID != key.ID {
		t.Errorf("context holds tenant %q key %d, want tenant %q key %d", got.ID, gotKey.ID, created.ID, key.ID)
	}

	if err := store.RevokeKey(ctx, key.ID); err != nil {
		t.Fatalf("RevokeKey() returned error: %v", err)
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("X-Api-Key", key.Plaintext)
	rec, reached, _, _ = serveAuth(t, store, req)
	if reached || rec.Code != http.StatusUnauthorized {
		t.Errorf("revoked key: status = %d, forwarded = %v; want 401, not forwarded", rec.Code, reached)
	}
}

// assertErrorType checks the JSON error body's type field.
func assertErrorType(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var body struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %q is not JSON: %v", rec.Body, err)
	}
	if body.Error.Type != want {
		t.Errorf("error type = %q, want %q", body.Error.Type, want)
	}
}
