package middleware

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/grishkadel/llm-gateway/internal/apierror"
	"github.com/grishkadel/llm-gateway/internal/ratelimit"
	"github.com/grishkadel/llm-gateway/internal/tenant"
	"github.com/grishkadel/llm-gateway/internal/usage"
)

// Limiter charges requests to their tenant's token bucket.
// *ratelimit.Limiter satisfies it; tests substitute a fake.
type Limiter interface {
	// Allow charges a request of cost tokens to the bucket of a tenant
	// allowed limit tokens a minute. It reports whether the request may go
	// ahead and, if not, how long until it could.
	Allow(ctx context.Context, tenantID string, limit, cost int64) (allowed bool, retryAfter time.Duration, err error)
}

// allowTimeout bounds each Allow. Redis answers in about a millisecond; one
// that takes 100 ms is treated as down, and the request goes ahead
// unlimited.
const allowTimeout = 100 * time.Millisecond

// charge is what RateLimit charged a request: whose bucket, at what limit,
// and how many tokens. It rides on the request context, so the charge can
// be settled once the request's real usage is known.
type charge struct {
	tenantID string
	limit    int64
	cost     int64
}

type chargeKey struct{}

// chargeFrom returns the charge RateLimit put on ctx, or nil if it put none.
func chargeFrom(ctx context.Context) *charge {
	c, _ := ctx.Value(chargeKey{}).(*charge)
	return c
}

// RateLimit returns middleware that charges each request's estimated tokens
// (see cost) to its tenant's bucket before any provider sees it. A request
// the bucket can't cover gets a 429 rate_limited with Retry-After, and goes
// no further.
//
// Rate limiting fails open: when Allow fails (Redis is down, or slower than
// allowTimeout), the request goes ahead with a Warn log. An admitted request
// carries its charge on the context either way, since a script that timed
// out may still have run.
//
// It needs the tenant from Auth and the estimate from ReadBody, so it runs
// after both. It goes outside any fallback, so a request is charged once
// however many providers try it.
func RateLimit(limiter Limiter, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			id := RequestIDFrom(ctx)
			t, _, authed := TenantFrom(ctx)
			req, bodyRead := usage.RequestFrom(ctx)
			if !authed || !bodyRead {
				// Auth and ReadBody run before every provider route, so this
				// is a wiring bug. The request still goes ahead.
				logger.Error("rate limit with no tenant or request; request not limited", "request_id", id)
				next.ServeHTTP(w, r)
				return
			}

			c := &charge{tenantID: t.ID, limit: t.RateLimitTokensPerMin, cost: cost(req, t)}

			// Go note: context.WithTimeout returns a child of ctx that is done
			// once the timeout passes (or as soon as ctx is), and a cancel func
			// that releases its timer early. A call given the child gives up
			// when it is done, so a hung Redis costs a request 100 ms rather
			// than hanging it. go-redis honours the deadline only with
			// ContextTimeoutEnabled, which cmd/gateway sets.
			allowCtx, cancel := context.WithTimeout(ctx, allowTimeout)
			allowed, wait, err := limiter.Allow(allowCtx, c.tenantID, c.limit, c.cost)
			cancel()

			switch {
			case err != nil:
				logger.Warn("rate limit check failed; request admitted", "request_id", id, "tenant_id", t.ID, "error", err)
			case !allowed:
				secs := retryAfterSeconds(wait)
				logger.Info("request rate limited",
					"request_id", id,
					"tenant_id", t.ID,
					"estimated_tokens", c.cost,
					"retry_after_seconds", secs,
				)
				w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
				apierror.Write(w, http.StatusTooManyRequests, "rate_limited",
					fmt.Sprintf("this tenant is over the gateway's token rate limit; retry in %d s", secs),
					map[string]any{
						"retry_after_seconds":  secs,
						"limit_tokens_per_min": c.limit,
						"estimated_tokens":     c.cost,
					})
				return
			}

			next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, chargeKey{}, c)))
		})
	}
}

// cost is what a request is charged up front: its input estimate plus an
// output allowance. The allowance is the client's own output cap or, when it
// set none, the tenant's default_max_tokens for each choice it asked for.
//
// The allowance never exceeds the tenant's limit. A cost over the limit
// already needs a full bucket to get in, so the clamp changes no admission,
// but it bounds the debt a request leaves if it is never settled.
//
// The sum stops at ratelimit.MaxCost, where Allow would clamp it, so the
// charge recorded is exactly the one Allow made. No step can overflow:
// OutputCap and Choices can each be up to math.MaxInt64.
func cost(req *usage.Request, t tenant.Tenant) int64 {
	output := req.OutputCap
	if output == 0 {
		perChoice := int64(t.DefaultMaxTokens)
		// Stop at the largest allowance rather than wrap around to a
		// negative one.
		if perChoice > 0 && req.Choices > math.MaxInt64/perChoice {
			output = math.MaxInt64
		} else {
			output = perChoice * req.Choices
		}
	}
	output = min(output, t.RateLimitTokensPerMin)
	return min(min(req.InputEstimate, ratelimit.MaxCost)+min(output, ratelimit.MaxCost), ratelimit.MaxCost)
}

// retryAfterSeconds is wait in whole seconds for Retry-After: rounded up, so
// the client doesn't come back before the bucket can admit it, and at least
// 1.
func retryAfterSeconds(wait time.Duration) int64 {
	secs := int64(wait / time.Second)
	if wait%time.Second > 0 {
		secs++
	}
	return max(secs, 1)
}
