package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/grishkadel/llm-gateway/internal/tenant"
)

// KeyLookup finds the tenant a gateway key belongs to. *tenant.Store
// satisfies it; tests substitute a fake.
type KeyLookup interface {
	Lookup(ctx context.Context, key string) (tenant.Tenant, tenant.APIKey, error)
}

// Go note: context keys use an unexported type so no other package can
// collide with them, even by using the same underlying value.
type ctxKey struct{}

type principal struct {
	tenant tenant.Tenant
	key    tenant.APIKey
}

// TenantFrom returns the tenant and key Auth attached to ctx. ok is false for
// a request that never passed through Auth.
func TenantFrom(ctx context.Context) (t tenant.Tenant, key tenant.APIKey, ok bool) {
	p, ok := ctx.Value(ctxKey{}).(principal)
	return p.tenant, p.key, ok
}

// Auth returns middleware that admits only requests carrying a valid gateway
// key, and attaches the key's tenant to the request context.
//
// The key is read from whichever credential header the client's SDK sends:
// Authorization (OpenAI and compatibles), X-Api-Key (Anthropic) or
// X-Goog-Api-Key (Gemini). Gemini's ?key= query parameter is not accepted:
// a key in a URL lands in every proxy and load balancer log on the way. The
// header still reaches the proxy, whose provider.Apply strips it before
// forwarding.
func Auth(keys KeyLookup, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := credential(r.Header)
			if key == "" {
				unauthorized(w, "no gateway API key: send it where your SDK sends its API key")
				return
			}

			t, k, err := keys.Lookup(r.Context(), key)
			if errors.Is(err, tenant.ErrNotFound) {
				unauthorized(w, "invalid or revoked gateway API key")
				return
			}
			if err != nil {
				// Not a 401: the key may be fine, and the client shouldn't go
				// rotating it because the database is down.
				logger.Error("key lookup failed", "error", err)
				writeError(w, http.StatusServiceUnavailable, "auth_unavailable", "could not verify the API key; retry shortly")
				return
			}

			ctx := context.WithValue(r.Context(), ctxKey{}, principal{tenant: t, key: k})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// credential returns the key from the first credential header present, or ""
// if there is none.
func credential(h http.Header) string {
	if v := h.Get("Authorization"); v != "" {
		// The auth scheme is case-insensitive (RFC 9110 §11.1). Anything but
		// Bearer, such as Basic, doesn't carry a key.
		scheme, token, ok := strings.Cut(v, " ")
		if ok && strings.EqualFold(scheme, "Bearer") {
			return strings.TrimSpace(token)
		}
		return ""
	}
	if v := h.Get("X-Api-Key"); v != "" {
		return v
	}
	return h.Get("X-Goog-Api-Key")
}

func unauthorized(w http.ResponseWriter, message string) {
	// A 401 must say how to authenticate (RFC 9110 §15.5.2).
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeError(w, http.StatusUnauthorized, "invalid_api_key", message)
}

func writeError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"type": errType, "message": message},
	})
}
