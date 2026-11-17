package main

import (
	"flag"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// burst sends n requests made by newReq at once: each goroutine has its
// request ready and waits on a barrier, which opens for all of them
// together. It times each from the barrier opening, and returns them in the
// order made.
func burst(n int, newReq func() *http.Request) ([]stream, error) {
	streams := make([]stream, n)
	errs := make([]error, n)
	release := make(chan struct{})
	var start time.Time
	var wg sync.WaitGroup
	for i := range n {
		req := newReq()
		wg.Go(func() {
			<-release
			streams[i], errs[i] = timeStream(req, start)
		})
	}
	start = time.Now()
	close(release)
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, setupf("burst request: %w", err)
		}
	}
	return streams, nil
}

// burstTiming is when a burst's requests were decided, and when the first
// stream's last event arrived, from the barrier opening. The gateway admits
// or refuses a request before its response headers leave, and settles a
// stream's charge and reservation only once it has passed on the stream's
// last event. So when every request was decided before any stream's last
// event, no settlement could have changed an admission.
type burstTiming struct {
	Decided   float64 `json:"all_decided_ms"`
	FirstLast float64 `json:"first_last_event_ms"`
	Before    bool    `json:"decided_before_any_settlement"`
}

// count tallies streams by status, and times the burst.
func count(streams []stream) (statuses map[int]int, timing burstTiming) {
	statuses = map[int]int{}
	var decided, firstLast time.Duration
	for _, s := range streams {
		statuses[s.status]++
		decided = max(decided, s.header)
		if s.status == http.StatusOK && (firstLast == 0 || s.last < firstLast) {
			firstLast = s.last
		}
	}
	return statuses, burstTiming{ms(decided), ms(firstLast), firstLast == 0 || decided < firstLast}
}

// runRateLimit gives a fresh tenant a rate limit of exactly allow charges of
// the stream request, sends burst of them at once, and passes when exactly
// allow get a 200 and the rest a 429.
func runRateLimit(args []string) (result, error) {
	var c common
	fs := flag.NewFlagSet("ratelimit", flag.ContinueOnError)
	allow := fs.Int("allow", 10, "requests the rate limit fits")
	size := fs.Int("burst", 30, "requests sent at once")
	if err := parse(fs, &c, args); err != nil {
		return result{}, err
	}
	if *allow < 1 || *size < 1 {
		return result{}, setupf("-allow and -burst must be at least 1")
	}

	charge, err := streamCharge()
	if err != nil {
		return result{}, setupError{err}
	}
	limit := int64(*allow) * charge
	t, err := c.newTenant("ratelimit", map[string]int64{"rate_limit_tokens_per_min": limit, "budget_micros": 1_000_000_000})
	if err != nil {
		return result{}, err
	}

	streams, err := burst(*size, func() *http.Request { return c.streamRequest(t.Key) })
	if err != nil {
		return result{}, err
	}
	statuses, timing := count(streams)
	ok, limited := statuses[http.StatusOK], statuses[http.StatusTooManyRequests]
	other := *size - ok - limited
	// The bucket refills continuously at limit tokens a minute: what it
	// regained while the burst was being decided, against one charge.
	refill := float64(limit) * timing.Decided / 60_000

	pass := ok == *allow && limited == *size-*allow
	fmt.Printf("ratelimit: limit %d tokens/min = %d x charge %d; burst of %d\n", limit, *allow, charge, *size)
	fmt.Printf("  200: %d  429: %d  other: %d  (want %d 200s)\n", ok, limited, other, *allow)
	fmt.Printf("  all decided by %.1f ms, first stream's last event at %.1f ms; refill meanwhile %.2f tokens (%.4f of a charge)\n",
		timing.Decided, timing.FirstLast, refill, refill/float64(charge))

	return finish(c.out, result{
		Subcommand: "ratelimit",
		Inputs: map[string]any{
			"allow": *allow, "burst": *size, "gateway": c.gateway, "model": streamModel,
			"charge_tokens": charge, "rate_limit_tokens_per_min": limit, "tenant_id": t.ID,
		},
		Measurements: map[string]any{
			"ok": ok, "rate_limited": limited, "other": other, "timing": timing,
			"refill_while_deciding_tokens": refill,
		},
		Pass: pass,
	})
}
