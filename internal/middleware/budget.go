package middleware

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/grishkadel/llm-gateway/internal/apierror"
	"github.com/grishkadel/llm-gateway/internal/budget"
	"github.com/grishkadel/llm-gateway/internal/pricing"
	"github.com/grishkadel/llm-gateway/internal/usage"
)

// Ledger keeps each tenant's spend for the month, in micro-dollars (µ$).
// *budget.Ledger satisfies it; tests substitute a fake.
type Ledger interface {
	// Reserve adds amount, the most a request could cost, to the tenant's
	// spend for the month holding at, if that leaves the spend within
	// budget. A refusal reserves nothing.
	Reserve(ctx context.Context, tenantID string, budget, amount int64, at time.Time) (budget.Reservation, error)
	// Settle swaps r's reservation for charge, what its request really
	// cost, once the request is over.
	Settle(ctx context.Context, r budget.Reservation, charge int64) error
}

type reservationKey struct{}

// reservationFrom returns the reservation Budget put on ctx for its attempt,
// or nil if it put none. Like RateLimit's charge, it rides on the context as a
// pointer.
func reservationFrom(ctx context.Context) *budget.Reservation {
	res, _ := ctx.Value(reservationKey{}).(*budget.Reservation)
	return res
}

// Budget returns middleware that reserves the most a request could cost
// (pricing.Reserve, at provider's prices) against its tenant's monthly budget
// in ledger, before provider's proxy sees it:
//   - A free provider is never budgeted: Budget returns next itself, so it
//     works when the budget is spent or Redis is down.
//   - A model with no price gets 400 unknown_model, with nothing reserved.
//   - A reservation the budget can't fit gets 402 budget_exceeded, with the
//     numbers behind it, and goes no further.
//   - An admitted request goes on, with its reservation on the context, so
//     the reservation can be settled once the request's real cost is known.
//     The first reservation to take the month's spend to 80% of the budget
//     is logged with a Warn.
//
// Budgets fail closed: when Reserve fails (Redis or Postgres is down, or
// slower than the ledger's timeouts), the request gets 503
// budget_unavailable. A reserve that fails after Redis ran it, such as one
// whose reply was lost, still leaves its amount in the tenant's spend, which
// over-counts but never under-counts.
//
// It needs the tenant from Auth and the Request from ReadBody, so it runs
// after both. It goes inside any fallback, around each provider's own proxy,
// so each attempt is reserved at the prices of the provider that serves it,
// for the model it asks that provider for.
func Budget(ledger Ledger, provider string, free bool, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if free {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			id := RequestIDFrom(ctx)
			t, _, authed := TenantFrom(ctx)
			req, bodyRead := usage.RequestFrom(ctx)
			if !authed || !bodyRead {
				// Auth and ReadBody run before every provider route, so this
				// is a wiring bug. Budgets guard money, so the request goes
				// no further.
				logger.Error("budget with no tenant or request; request refused", "request_id", id, "provider", provider)
				budgetUnavailable(w)
				return
			}

			// The attempt's own Request: after a fallback, it names the model
			// the fallback asks for.
			amount, priced := pricing.Reserve(provider, req.Model, req.InputReserve, req.OutputCap, req.Choices, req.Received)
			if !priced {
				// A Warn, since the fix is a rate in internal/pricing.
				logger.Warn("model not priced; request refused", "request_id", id, "tenant_id", t.ID, "provider", provider, "model", req.Model)
				msg := fmt.Sprintf("the gateway has no price for model %q on provider %q, so it can't budget for the request", req.Model, provider)
				if req.Model == "" {
					msg = fmt.Sprintf("the request names no model, so the gateway can't price it on provider %q", provider)
				}
				apierror.Write(w, http.StatusBadRequest, "unknown_model", msg, map[string]any{"provider": provider, "model": req.Model})
				return
			}

			// The ledger bounds its own calls: 100 ms a script, 2 s a rebuild.
			// It is asked even for an amount over the whole budget, which it
			// refuses, since the 402 reports the spend it finds.
			res, err := ledger.Reserve(ctx, t.ID, t.BudgetMicros, amount, req.Received)
			switch {
			case err != nil:
				logger.Error("budget check failed; request refused", "request_id", id, "tenant_id", t.ID, "provider", provider, "error", err)
				budgetUnavailable(w)
				return
			case !res.Admitted:
				logger.Info("request over budget",
					"request_id", id,
					"tenant_id", t.ID,
					"provider", provider,
					"model", req.Model,
					"reserve_micros", amount,
					"spent_micros", res.Spent,
					"budget_micros", t.BudgetMicros,
				)
				budgetExceeded(w, t.BudgetMicros, res.Spent, amount, req.Received)
				return
			}

			// The ledger reports it once a month for each tenant, and again
			// only after it rebuilds the month.
			if res.Warn {
				logger.Warn("tenant's spend has reached 80% of its monthly budget",
					"request_id", id,
					"tenant_id", t.ID,
					"spent_micros", res.Spent,
					"budget_micros", t.BudgetMicros,
				)
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, reservationKey{}, &res)))
		})
	}
}

// budgetUnavailable answers a request whose budget can't be checked.
func budgetUnavailable(w http.ResponseWriter) {
	apierror.Write(w, http.StatusServiceUnavailable, "budget_unavailable",
		"the gateway can't check this tenant's budget right now; retry shortly", nil)
}

// budgetExceeded answers a request the ledger refused: it could cost up to
// amount, and its tenant has spent spent of budgetMicros for the month
// holding at, all in µ$. The message, in dollars, says what would help: only
// a free provider when the budget is 0, a smaller request when this one could
// cost more than the whole budget, and otherwise the reset. There is no
// Retry-After: the reset is weeks away, and SDKs don't retry a 402.
func budgetExceeded(w http.ResponseWriter, budgetMicros, spent, amount int64, at time.Time) {
	start, end := budget.Period(at)
	resetsAt := end.Format(time.RFC3339)
	remaining := max(budgetMicros-spent, 0)

	var msg string
	switch {
	case budgetMicros == 0:
		msg = "the gateway's monthly budget for this tenant is $0.00, so it can use free providers only"
	case amount > budgetMicros:
		msg = fmt.Sprintf("this request could cost up to %s, more than the gateway's whole monthly budget for this tenant, %s; "+
			"set a lower output cap or send a shorter request", dollars(amount), dollars(budgetMicros))
	default:
		msg = fmt.Sprintf("this request could cost up to %s, more than the %s left of the gateway's %s monthly budget for this tenant; "+
			"the budget resets at %s", dollars(amount), dollars(remaining), dollars(budgetMicros), resetsAt)
	}
	apierror.Write(w, http.StatusPaymentRequired, "budget_exceeded", msg, map[string]any{
		"budget_micros":    budgetMicros,
		"spent_micros":     spent,
		"remaining_micros": remaining,
		"reserve_micros":   amount,
		"period":           start.Format("2006-01"),
		"resets_at":        resetsAt,
	})
}

// dollars formats micros, an amount in µ$, as exact dollars: whole dollars,
// then the six-digit fraction without its trailing zeros, but at least two
// decimals, such as $50.00, $1.50 or $0.000123. It never rounds, so a message
// never says $0.00 is left when 4,000 µ$ are. micros is not negative.
func dollars(micros int64) string {
	frac := fmt.Sprintf("%06d", micros%1_000_000)
	return fmt.Sprintf("$%d.%s%s", micros/1_000_000, frac[:2], strings.TrimRight(frac[2:], "0"))
}
