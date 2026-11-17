package main

import (
	"flag"
	"fmt"
	"net/http"
	"time"
)

// runBudget gives a fresh tenant a budget of exactly room reservations of the
// stream request, sends n of them at once, and reads the tenant's budget once
// every request has settled. It passes when the spend is within the budget,
// and so is the cost of the 200s at what each settled at.
func runBudget(args []string) (result, error) {
	var c common
	fs := flag.NewFlagSet("budget", flag.ContinueOnError)
	room := fs.Int("room", 10, "reservations the budget fits")
	n := fs.Int("n", 50, "requests sent at once")
	if err := parse(fs, &c, args); err != nil {
		return result{}, err
	}
	if *room < 1 || *n < 1 {
		return result{}, setupf("-room and -n must be at least 1")
	}

	reservation, err := streamReservation(time.Now())
	if err != nil {
		return result{}, setupError{err}
	}
	budgetMicros := int64(*room) * reservation
	t, err := c.newTenant("budget", map[string]int64{"rate_limit_tokens_per_min": 1_000_000_000, "budget_micros": budgetMicros})
	if err != nil {
		return result{}, err
	}

	streams, err := burst(*n, func() *http.Request { return c.streamRequest(t.Key) })
	if err != nil {
		return result{}, err
	}
	statuses, timing := count(streams)
	ok, over := statuses[http.StatusOK], statuses[http.StatusPaymentRequired]
	other := *n - ok - over

	// Every response has been read to its end, and the gateway settles a
	// reservation before it ends the response, so the ledger is final. The
	// usage rows are written in batches after that: wait for one per 200, the
	// only responses that write one, for the cost each settled at.
	totals, err := c.waitUsage(int64(ok), 30*time.Second, t.ID)
	if err != nil {
		return result{}, err
	}
	u := totals[0]
	var b struct {
		BudgetMicros int64 `json:"budget_micros"`
		SpentMicros  int64 `json:"spent_micros"`
	}
	if err := c.admin(http.MethodGet, "/admin/tenants/"+t.ID+"/budget", nil, &b); err != nil {
		return result{}, setupf("reading budget: %w", err)
	}
	var settled int64
	if u.Requests > 0 {
		settled = u.CostMicros / u.Requests
	}

	pass := u.Requests == int64(ok) && u.UnpricedRequests == 0 &&
		b.SpentMicros <= b.BudgetMicros && int64(ok)*settled <= b.BudgetMicros
	fmt.Printf("budget: %d µ$ = %d x reservation %d µ$; %d requests at once\n", budgetMicros, *room, reservation, *n)
	fmt.Printf("  200: %d  402: %d  other: %d\n", ok, over, other)
	fmt.Printf("  usage rows %d, cost %d µ$ (%d µ$ each); ledger spent %d of %d µ$\n",
		u.Requests, u.CostMicros, settled, b.SpentMicros, b.BudgetMicros)
	fmt.Printf("  check: spent %d <= budget %d, and 200s x settled cost %d <= budget\n", b.SpentMicros, b.BudgetMicros, int64(ok)*settled)

	return finish(c.out, result{
		Subcommand: "budget",
		Inputs: map[string]any{
			"room": *room, "n": *n, "gateway": c.gateway, "model": streamModel,
			"reservation_micros": reservation, "budget_micros": budgetMicros, "tenant_id": t.ID,
		},
		Measurements: map[string]any{
			"ok": ok, "budget_exceeded": over, "other": other, "timing": timing,
			"usage_rows": u.Requests, "usage_cost_micros": u.CostMicros, "settled_cost_micros": settled,
			"unpriced_rows": u.UnpricedRequests, "incomplete_rows": u.IncompleteRequests,
			"spent_micros": b.SpentMicros, "ledger_budget_micros": b.BudgetMicros,
		},
		Pass: pass,
	})
}
