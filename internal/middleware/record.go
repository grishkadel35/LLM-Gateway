package middleware

import (
	"context"
	"net/http"
	"time"

	"github.com/grishkadel/llm-gateway/internal/usage"
)

// Record is what the gateway learns about one request on its way through:
// the one channel from the layers that learn each fact to the code that
// reports them, the request line and the route's metrics, so the two can't
// disagree.
//
// Logging creates it as the request arrives, and reads it once the request is
// over. Each layer in between records what only it knows:
//   - Route, outside Auth: the provider route the request is on
//   - Auth: the tenant
//   - RecordRequest, just inside ReadBody: the client's Request
//   - Cache: its decision
//   - RateLimit: its 429
//   - Budget: a refusal, and why, and the tenant's spend and budget
//   - cmd/gateway's meter: the usage last reported, and what it cost
//
// A fact no layer learned stays at its zero value: a request no provider route
// touched leaves the whole Record empty. A layer that finds no Record on the
// context records nothing, so each works without Logging too.
//
// It needs no lock, for the reason charge gives: everything that touches it
// runs on the goroutine serving the request.
//
// Go note: a context's values can't be changed, and r.WithContext makes a new
// request that the handlers outside never see, so nothing an inner handler
// puts on its context reaches an outer one. A pointer does: Logging puts a
// *Record on the context before calling the handlers inside it, they find
// that same Record (RecordFrom) and fill in its fields, and Logging reads them
// once next.ServeHTTP is done. Python would pass a mutable dict down the chain.
type Record struct {
	// Start is when Logging created the Record, as the request arrived.
	Start time.Time
	// Provider is the route the request is on: the provider the client
	// called, whichever provider answered. "" off the provider routes.
	Provider string
	// TenantID is the tenant Auth found the request's key for. "" until Auth
	// admitted the request.
	TenantID string
	// Request is ReadBody's Request as the client sent it, before any fallback
	// asked another model: the model the client named, raw, and whether it
	// asked for a stream. nil until ReadBody had read the body.
	Request *usage.Request
	// Cache is the response cache's decision: hit, miss, bypass, or error for
	// a lookup that failed, though its response says miss. "" when the cache
	// made none: there is no cache, the request never reached it, or it
	// refused a bad X-Gateway-Cache.
	Cache string
	// RateLimited reports that RateLimit refused the request with its own 429
	// rate_limited. A provider's 429 isn't the gateway's.
	RateLimited bool
	// BudgetRefusal is Budget's refusal of one of the request's attempts; nil
	// if it refused none. A client gone during the budget check (499) isn't a
	// refusal.
	BudgetRefusal *BudgetRefusal
	// BudgetSpend is the tenant's spend for the month as the ledger last
	// reported it to Budget, after a reservation, admitted or refused, or a
	// settlement, with the tenant's budget. After a fallback, the fallback's
	// attempt reports last. nil if the ledger reported none: no attempt
	// reached it, or it failed.
	BudgetSpend *BudgetSpend
	// Usage is the usage last reported for the request: the response the
	// client got, from a provider or the cache, or the proxy's report of a
	// provider that never answered (502, 504, or 499 for a client gone
	// first). On a route with a fallback, the fallback's attempt reports after
	// the one that failed. It is nil if nothing was reported. When Budget
	// refused a fallback's attempt, Usage is the failed attempt's, and the
	// client got the refusal.
	Usage *UsageReport
}

// BudgetRefusal is Budget's refusal of one attempt.
type BudgetRefusal struct {
	// Provider is the attempt's provider: after a fallback, the fallback's.
	Provider string
	// Reason is why: exceeded (402 budget_exceeded), unknown_model (400) or
	// unavailable (503 budget_unavailable).
	Reason string
}

// BudgetSpend is a tenant's spend for the month and its monthly budget.
type BudgetSpend struct {
	// SpentMicros is the spend in µ$: the month's settled costs, plus the
	// reservations in flight.
	SpentMicros int64
	// BudgetMicros is the budget in µ$.
	BudgetMicros int64
}

// UsageReport is one report of a response's usage, with what it cost.
type UsageReport struct {
	// Provider is the provider that produced the response: the route's own,
	// or after a fallback the fallback's. A cache hit names its route's.
	Provider string
	usage.Result
	// CostMicros is what the response cost in µ$, priced as its usage row is:
	// nil when that isn't known.
	CostMicros *int64
}

type recordKey struct{}

// RecordFrom returns the Record Logging put on ctx, or nil for a request that
// never passed through Logging.
func RecordFrom(ctx context.Context) *Record {
	rec, _ := ctx.Value(recordKey{}).(*Record)
	return rec
}

// Route returns middleware that records provider as the route of each request
// on its Record. It goes outside Auth on provider's route, so a request Auth
// refuses still names its route.
func Route(provider string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if rec := RecordFrom(r.Context()); rec != nil {
				rec.Provider = provider
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RecordRequest is middleware that records ReadBody's Request on the
// request's Record. It goes just inside ReadBody, so every request whose body
// was read has its model and stream flag recorded, whatever refuses it next.
func RecordRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rec := RecordFrom(r.Context()); rec != nil {
			rec.Request, _ = usage.RequestFrom(r.Context())
		}
		next.ServeHTTP(w, r)
	})
}

// attrs appends the request line's fields from r to attrs, each only when it
// is known, so a request no provider route touched adds none:
//   - tenant_id, provider, and model as the client named it
//   - from the usage last reported, while it describes the response the
//     client got: fallback_provider when the provider that produced it isn't
//     the route, its tokens, cost_micros (null when the cost isn't known) and
//     cached
//   - streamed, once the body was read: the report's when there is one, so it
//     is the usage row's, else whether the client asked for a stream
func (r *Record) attrs(attrs []any) []any {
	if r.TenantID != "" {
		attrs = append(attrs, "tenant_id", r.TenantID)
	}
	if r.Provider != "" {
		attrs = append(attrs, "provider", r.Provider)
	}
	if r.Request != nil && r.Request.Model != "" {
		attrs = append(attrs, "model", r.Request.Model)
	}

	// A refused fallback's attempt answered the client itself, after the
	// failed attempt reported.
	u := r.Usage
	if r.BudgetRefusal != nil {
		u = nil
	}
	if u != nil {
		if u.Provider != r.Provider {
			attrs = append(attrs, "fallback_provider", u.Provider)
		}
		attrs = append(attrs,
			"input_tokens", u.Input,
			"cached_input_tokens", u.CachedInput,
			// One figure for both TTLs, as the usage row stores them.
			"cache_write_tokens", u.CacheWrite+u.CacheWrite1h,
			"output_tokens", u.Output,
			// A nil *int64 is JSON null.
			"cost_micros", u.CostMicros,
			"cached", u.Cached,
		)
	}

	if r.Request != nil {
		streamed := r.Request.Stream
		if u != nil {
			streamed = u.Streamed
		}
		attrs = append(attrs, "streamed", streamed)
	}
	return attrs
}
