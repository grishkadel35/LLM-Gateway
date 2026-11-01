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
	"github.com/grishkadel/llm-gateway/internal/proxy"
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
	// cost, once the request is over. r must be admitted, and the request's
	// usage row already queued: a ledger that finds the month missing counts
	// the request from that row when it rebuilds.
	Settle(ctx context.Context, r budget.Reservation, charge int64) error
}

// reservation is what Budget reserved for one attempt. It rides on the
// attempt's context as a pointer, so the attempt's usage can be reported onto
// it (ReportUsage), and Budget settles the reservation by that usage once the
// attempt is over.
//
// It needs no lock, for the reason charge gives: the reports are made, and
// the settlement reads them, on the goroutine serving the request.
type reservation struct {
	budget.Reservation
	// result is the usage last reported for the attempt; nil if none was.
	result *usage.Result
}

type reservationKey struct{}

// reservationFrom returns the reservation Budget put on ctx for its attempt,
// or nil if it put none.
func reservationFrom(ctx context.Context) *reservation {
	res, _ := ctx.Value(reservationKey{}).(*reservation)
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
//   - An admitted request goes on, with its reservation on the context for
//     its usage to be reported onto (ReportUsage). The first reservation to
//     take the month's spend to 80% of the budget is logged with a Warn.
//
// Once an admitted attempt is over, its reservation is settled: swapped for
// what the attempt cost, by the usage last reported for it (see
// settleReservation). By then the attempt's usage row is queued, as
// Ledger.Settle requires: cmd/gateway's usage callback reports the usage
// (ReportUsage), then queues the row (recordUsage), and the proxy makes that
// callback before it returns or aborts.
//
// Budgets fail closed: when Reserve fails (Redis or Postgres is down, or
// slower than the ledger's timeouts), the request gets 503
// budget_unavailable. The exception is a client that has left, told by the
// request's own context being done: nobody reads a response, so it is logged
// at Info, not Error, and ends with proxy.StatusClientClosedRequest, as the
// proxy ends a request whose client leaves before the provider answers.
// Either way there is no reservation to settle, and a reserve that fails
// after Redis ran it, such as one whose reply was lost, still leaves its
// amount in the tenant's spend, which over-counts but never under-counts. A
// Settle that fails is logged with a Warn, and leaves the reservation in the
// spend in place of the charge.
//
// Each refusal, the 400, the 402 and the 503, is recorded on the request's
// Record with its reason and provider. The 499 isn't a refusal.
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
				recordRefusal(ctx, provider, "unavailable")
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
				recordRefusal(ctx, provider, "unknown_model")
				apierror.Write(w, http.StatusBadRequest, "unknown_model", msg, map[string]any{"provider": provider, "model": req.Model})
				return
			}

			// The ledger bounds its own calls: 100 ms a script, 2 s a rebuild.
			// It is asked even for an amount over the whole budget, which it
			// refuses, since the 402 reports the spend it finds.
			res, err := ledger.Reserve(ctx, t.ID, t.BudgetMicros, amount, req.Received)
			switch {
			case err != nil:
				// The client has left if the request's own context is done. Ask
				// the context, not err: the ledger's timeouts wrap
				// context.DeadlineExceeded with the request still live, and stay
				// a 503. As the proxy does for a client that has gone, answer 499
				// with no body, at Info. No usage is reported, so RateLimit
				// refunds the whole charge.
				if ctx.Err() != nil {
					logger.Info("client closed request during budget check", "request_id", id, "tenant_id", t.ID, "provider", provider)
					w.WriteHeader(proxy.StatusClientClosedRequest)
					return
				}
				logger.Error("budget check failed; request refused", "request_id", id, "tenant_id", t.ID, "provider", provider, "error", err)
				recordRefusal(ctx, provider, "unavailable")
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
				recordRefusal(ctx, provider, "exceeded")
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

			// Deferred, so the attempt is settled even when ReverseProxy
			// aborts a stream with panic(http.ErrAbortHandler). It gets the
			// pointer, and reads the last report only when it runs. Like
			// RateLimit's settle, it holds the response's last bytes for one
			// Redis call.
			held := &reservation{Reservation: res}
			defer settleReservation(ctx, ledger, logger, provider, t.ID, req, held)
			next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, reservationKey{}, held)))
		})
	}
}

// settleReservation swaps res's reservation for what its attempt cost, once
// the attempt is over, by the usage last reported for it:
//   - None: nothing reached the provider (no concurrency slot came free), so
//     the attempt is charged 0.
//   - A cost pricing.ResultCost knows: that cost, the one the attempt's usage
//     row records.
//   - Unknown (the client left mid-stream, the provider never answered): the
//     reservation, or what the usage seen so far costs if that's more. The
//     attempt keeps what it reserved, as RateLimit keeps its charge.
//
// req is the attempt's own Request, which its row is priced by too: after a
// fallback, it names the model the fallback asked for.
//
// Settle is called even when the charge is the reservation, since the ledger
// may need the whole charge (the month was rebuilt without the reservation)
// or a miss counted (the month is missing). ctx is the attempt's context; a
// client that has left must still be settled, so Settle gets it without its
// cancellation, and the ledger bounds the call. A failed Settle is logged and
// left: the response is already written.
func settleReservation(ctx context.Context, ledger Ledger, logger *slog.Logger, provider, tenantID string, req *usage.Request, res *reservation) {
	var charged int64
	if r := res.result; r != nil {
		var known bool
		// Budget never runs for a free provider.
		charged, known = pricing.ResultCost(provider, false, *r, req.Model, req.Received)
		if !known {
			charged = res.Amount
			if seen, priced := pricing.Cost(provider, r.Usage, req.Model, req.Received); priced {
				charged = max(charged, seen)
			}
		}
	}

	if err := ledger.Settle(context.WithoutCancel(ctx), res.Reservation, charged); err != nil {
		logger.Warn("budget settlement failed",
			"request_id", RequestIDFrom(ctx),
			"tenant_id", tenantID,
			"provider", provider,
			"charge_micros", charged,
			"error", err,
		)
	}
}

// recordRefusal records on the Record of the request ctx belongs to that
// Budget refused its attempt on provider, for reason.
func recordRefusal(ctx context.Context, provider, reason string) {
	if rec := RecordFrom(ctx); rec != nil {
		rec.BudgetRefusal = &BudgetRefusal{Provider: provider, Reason: reason}
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
