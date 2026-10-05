package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

const testAdminKey = "admin-0123456789abcdef0123456789abcdef"

// serveAdmin runs a request with the given Authorization header through
// AdminAuth and reports whether it reached the handler behind it.
func serveAdmin(adminKey, authorization string) (rec *httptest.ResponseRecorder, reached bool) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true })

	req := httptest.NewRequest(http.MethodPost, "/admin/tenants", nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec = httptest.NewRecorder()
	AdminAuth(adminKey)(next).ServeHTTP(rec, req)
	return rec, reached
}

func TestAdminAuthAcceptsAdminKey(t *testing.T) {
	// The auth scheme is case-insensitive (RFC 9110 §11.1).
	for _, scheme := range []string{"Bearer", "bearer"} {
		rec, reached := serveAdmin(testAdminKey, scheme+" "+testAdminKey)
		if !reached {
			t.Errorf("%s admin key rejected: status %d, body %s", scheme, rec.Code, rec.Body)
		}
	}
}

// TestAdminAuthRejects: anything but the exact admin key is turned away
// before the admin handler runs. That includes a valid tenant key: tenants
// must never manage tenants.
func TestAdminAuthRejects(t *testing.T) {
	cases := []struct {
		name          string
		authorization string
	}{
		{"no credential", ""},
		{"wrong key", "Bearer admin-wrong"},
		{"tenant key", "Bearer gw_" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
		{"prefix of the key", "Bearer " + testAdminKey[:len(testAdminKey)-1]},
		{"key plus suffix", "Bearer " + testAdminKey + "x"},
		// Exact means exact: no trimming of what was presented.
		{"key plus non-breaking space", "Bearer " + testAdminKey + "\u00a0"},
		{"extra space before key", "Bearer  " + testAdminKey},
		{"other scheme", "Basic " + testAdminKey},
		{"empty bearer", "Bearer "},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, reached := serveAdmin(testAdminKey, tc.authorization)

			if reached {
				t.Error("request reached the admin handler")
			}
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
			}
			if rec.Header().Get("WWW-Authenticate") == "" {
				t.Error("401 has no WWW-Authenticate header")
			}
			assertErrorType(t, rec, "invalid_admin_key")
		})
	}
}

// TestAdminAuthReadsOnlyAuthorization: the admin key travels only as a Bearer
// token. Tenant SDKs put keys in X-Api-Key and X-Goog-Api-Key too; neither
// header is ever an admin credential.
func TestAdminAuthReadsOnlyAuthorization(t *testing.T) {
	for _, header := range []string{"X-Api-Key", "X-Goog-Api-Key"} {
		req := httptest.NewRequest(http.MethodPost, "/admin/tenants", nil)
		req.Header.Set(header, testAdminKey)
		reached := false
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true })

		rec := httptest.NewRecorder()
		AdminAuth(testAdminKey)(next).ServeHTTP(rec, req)

		if reached || rec.Code != http.StatusUnauthorized {
			t.Errorf("admin key in %s: status = %d, admitted = %v; want 401", header, rec.Code, reached)
		}
	}
}

// TestAdminAuthFailsClosed: an unset admin key must lock admin out, not let
// an empty credential match it; and an admin key that is a tenant key would
// make that tenant an admin, so it locks admin out too.
func TestAdminAuthFailsClosed(t *testing.T) {
	tenantKey := "gw_" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	cases := []struct {
		name, adminKey, authorization string
	}{
		{"no key, no credential", "", ""},
		{"no key, empty bearer", "", "Bearer "},
		{"no key, some bearer", "", "Bearer x"},
		{"tenant key as admin key", tenantKey, "Bearer " + tenantKey},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, reached := serveAdmin(tc.adminKey, tc.authorization)
			if reached || rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, admitted = %v; want 401", rec.Code, reached)
			}
			assertErrorType(t, rec, "invalid_admin_key")
		})
	}
}
