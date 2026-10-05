package middleware

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/grishkadel/llm-gateway/internal/tenant"
)

// AdminAuth returns middleware that admits only requests presenting adminKey
// as "Authorization: Bearer <key>". Tenant keys are not admin keys: a tenant
// never manages tenants.
//
// The comparison is between SHA-256 digests, with
// subtle.ConstantTimeCompare. A plain == on the keys returns at the first
// differing byte, so response timing would reveal how much of a guess was
// right. ConstantTimeCompare alone still returns early when the lengths
// differ; hashing first makes both sides 32 bytes, so the key's length
// doesn't leak either.
//
// An empty adminKey admits nobody, and so does one that is a tenant key
// ("gw_..."): that tenant could otherwise manage every tenant. Startup
// validation should reject both, but the middleware fails closed on its own.
//
// Wrap the whole /admin/ subtree with it, not each admin route: per-route
// patterns like "POST /admin/tenants" would let ServeMux answer an
// unauthenticated GET with 405 and an Allow header before AdminAuth runs.
func AdminAuth(adminKey string) func(http.Handler) http.Handler {
	want := sha256.Sum256([]byte(adminKey))
	usable := adminKey != "" && !strings.HasPrefix(adminKey, tenant.KeyPrefix)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got := sha256.Sum256([]byte(bearerToken(r.Header.Get("Authorization"))))
			if !usable || subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
				unauthorizedAs(w, "invalid_admin_key", "admin routes need the gateway admin key as a Bearer token")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
