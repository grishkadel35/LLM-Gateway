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
	// Adjust settles a charge once the request's real cost is known: a
	// positive delta refunds tokens, a negative one charges more.
	Adjust(ctx context.Context, tenantID string, limit, delta int64) error
}

// allowTimeout bounds each call to the Limiter, Allow and Adjust alike.
// Redis answers in about a millisecond; one that takes 100 ms is treated as
// down.
const allowTimeout = 100 * time.Millisecond

// charge is what RateLimit charged a request: whose bucket, at what limit,
// and how many tokens. It rides on the request context as a pointer, so the
// request's usage can be reported onto it (ReportUsage), and RateLimit
// settles the charge by that usage once the request is over.
//
// It needs no lock: everything that touches it runs on the goroutine serving
// the request. ReverseProxy calls ModifyResponse and ErrorHandler there, and
// reads and closes the response body there, closing it in a defer when it
// aborts a stream. So every report is made before its ServeHTTP returns, or
// while it unwinds.
type charge struct {
	tenantID string
	limit    int64
	cost     int64
	// result is the usage last reported for the request; nil if none was.
	result *usage.Result
}

type chargeKey struct{}

// chargeFrom returns the charge RateLimit put on ctx, or nil if it put none.
func chargeFrom(ctx context.Context) *charge {
	c, _ := ctx.Value(chargeKey{}).(*charge)
	return c
}

// ReportUsage records r as the usage of the request ctx belongs to, for
// RateLimit to settle its charge by. The last report wins: on a route with a
// fallback, the attempt that answered reports after the one that failed. A
// request RateLimit didn't admit has no charge, and its report is dropped.
func ReportUsage(ctx context.Context, r usage.Result) {
	if c := chargeFrom(ctx); c != nil {
		c.result = &r
	}
}

// RateLimit returns middleware that charges each request's estimated tokens
// (see cost) to its tenant's bucket before any provider sees it. A request
// the bucket can't cover gets a 429 rate_limited with Retry-After, and goes
// no further.
//
// Once an admitted request is over, its charge is settled by the usage
// reported for it (see settle): the bucket gets back what the request didn't
// use, or pays for what it used beyond the charge.
//
// Rate limiting fails open: when Allow fails (Redis is down, or slower than
// allowTimeout), the request goes ahead with a Warn log. It is still settled
// like any other, since a script that timed out may have run.
//
// It needs the tenant from Auth and the estimate from ReadBody, so it runs
// after both. It goes outside any fallback, so a request is charged and
// settled once however many providers try it.
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

			// Deferred, so the charge is settled even when ReverseProxy aborts
			// a stream with panic(http.ErrAbortHandler). It runs before
			// net/http finishes the response, so the response's last bytes
			// wait on one Redis call (about a millisecond, at most
			// allowTimeout). In return, a client that has read its whole
			// response finds its refund already made, and its next request
			// isn't refused for tokens that were about to come back.
			defer settle(ctx, limiter, logger, c)
			next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, chargeKey{}, c)))
		})
	}
}

// settle corrects the bucket for charge c, once its request is over, by the
// usage last reported for it:
//   - None: nothing reached a provider (no concurrency slot came free), so
//     the whole charge is refunded.
//   - Complete: the bucket gets back the charge less the tokens used, or
//     pays the difference when they came to more.
//   - Incomplete (the client left mid-stream, the provider never answered):
//     the real count is unknown, so nothing is refunded, but tokens already
//     seen beyond the charge are charged.
//
// ctx is the request's context. A failed Adjust is logged and left: the
// response is already written.
func settle(ctx context.Context, limiter Limiter, logger *slog.Logger, c *charge) {
	delta := c.cost
	if r := c.result; r != nil {
		delta = c.cost - r.Tokens()
		if !r.Complete {
			delta = min(delta, 0)
		}
	}
	if delta == 0 {
		return
	}

	// Go note: context.WithoutCancel returns a context that keeps ctx's
	// values (the request ID) but is never cancelled, even once ctx is. A
	// client that went away has already cancelled its request's context,
	// and its charge must still be settled.
	adjustCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), allowTimeout)
	defer cancel()
	if err := limiter.Adjust(adjustCtx, c.tenantID, c.limit, delta); err != nil {
		logger.Warn("rate limit settlement failed",
			"request_id", RequestIDFrom(ctx),
			"tenant_id", c.tenantID,
			"delta", delta,
			"error", err,
		)
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
