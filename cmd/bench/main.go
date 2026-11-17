// Command bench measures the gateway against cmd/mockprovider: stream latency
// (ttfb), rate-limit admission under a burst (ratelimit), budget admission
// under a burst (budget), and what the response cache saves (cache).
//
//	go run ./cmd/bench <ttfb|ratelimit|budget|cache> [flags] -out result.json
//
// Each subcommand creates a fresh tenant through the admin API, prints a
// short summary, and writes its inputs, measurements and verdict to -out. It
// exits 0 when the check passes, 1 when it fails, and 2 on a setup error.
package main

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/grishkadel/llm-gateway/internal/pricing"
	"github.com/grishkadel/llm-gateway/internal/provider"
	"github.com/grishkadel/llm-gateway/internal/usage"
)

// common holds the flags every subcommand takes.
type common struct {
	gateway  string
	mock     string
	adminKey string
	out      string
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.gateway, "gateway", "http://127.0.0.1:8080", "gateway base URL")
	fs.StringVar(&c.mock, "mock", "http://127.0.0.1:9090", "mock provider base URL")
	fs.StringVar(&c.adminKey, "admin-key", os.Getenv("GATEWAY_ADMIN_KEY"), "gateway admin key")
	fs.StringVar(&c.out, "out", "", "JSON result file (required)")
}

// setupError marks a failure to set the measurement up, as opposed to a
// measurement that failed its check: main exits 2 for it, not 1.
type setupError struct{ err error }

func (e setupError) Error() string { return e.err.Error() }

func setupf(format string, args ...any) error {
	return setupError{fmt.Errorf(format, args...)}
}

// result is what each subcommand writes to -out.
type result struct {
	Subcommand   string    `json:"subcommand"`
	Time         time.Time `json:"time"`
	Inputs       any       `json:"inputs"`
	Measurements any       `json:"measurements"`
	Pass         bool      `json:"pass"`
}

var subcommands = map[string]func(args []string) (result, error){
	"ttfb":      runTTFB,
	"ratelimit": runRateLimit,
	"budget":    runBudget,
	"cache":     runCache,
}

func main() {
	if len(os.Args) < 2 || subcommands[os.Args[1]] == nil {
		fmt.Fprintln(os.Stderr, "usage: bench <ttfb|ratelimit|budget|cache> [flags]")
		os.Exit(2)
	}
	os.Exit(run(os.Args[1], os.Args[2:]))
}

func run(name string, args []string) int {
	res, err := subcommands[name](args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bench %s: %v\n", name, err)
		if errors.As(err, new(setupError)) {
			return 2
		}
		return 1
	}
	if !res.Pass {
		fmt.Println("FAIL")
		return 1
	}
	fmt.Println("PASS")
	return 0
}

// parse parses a subcommand's flags and checks the common ones.
func parse(fs *flag.FlagSet, c *common, args []string) error {
	c.register(fs)
	if err := fs.Parse(args); err != nil {
		return setupError{err}
	}
	if c.adminKey == "" {
		return setupf("no admin key: set GATEWAY_ADMIN_KEY or -admin-key")
	}
	if c.out == "" {
		return setupf("-out is required")
	}
	return nil
}

// finish writes res to path.
func finish(path string, res result) (result, error) {
	res.Time = time.Now().UTC()
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return res, setupError{err}
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return res, setupError{err}
	}
	return res, nil
}

// client is shared by every request the bench makes. Its pool holds enough
// idle connections for the widest burst, so connections are reused rather
// than redialled between phases.
var client = &http.Client{
	Timeout:   30 * time.Second,
	Transport: &http.Transport{MaxIdleConnsPerHost: 200, DisableCompression: true},
}

// tenant is a tenant the bench created, with its first key.
type tenant struct {
	ID  string
	Key string
}

// admin calls the admin API at method and path, and decodes its JSON reply
// into into, unless into is nil.
func (c *common) admin(method, path string, body, into any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.gateway+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.adminKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if into == nil {
		return nil
	}
	return json.Unmarshal(b, into)
}

// newTenant creates a tenant named bench-<name>-<time> and sets limits on
// it: rate_limit_tokens_per_min, budget_micros, as the admin API's PATCH
// takes them.
func (c *common) newTenant(name string, limits map[string]int64) (tenant, error) {
	var created struct {
		Tenant struct {
			ID string `json:"id"`
		} `json:"tenant"`
		Key struct {
			Plaintext string `json:"plaintext"`
		} `json:"key"`
	}
	body := map[string]string{"name": fmt.Sprintf("bench-%s-%d", name, time.Now().UnixNano())}
	if err := c.admin(http.MethodPost, "/admin/tenants", body, &created); err != nil {
		return tenant{}, setupf("creating tenant: %w", err)
	}
	t := tenant{ID: created.Tenant.ID, Key: created.Key.Plaintext}
	if err := c.admin(http.MethodPatch, "/admin/tenants/"+t.ID, limits, nil); err != nil {
		return tenant{}, setupf("setting tenant limits: %w", err)
	}
	return t, nil
}

// usageTotals is the part of GET /admin/tenants/{id}/usage the bench reads.
type usageTotals struct {
	Requests           int64 `json:"requests"`
	CostMicros         int64 `json:"cost_micros"`
	UnpricedRequests   int64 `json:"unpriced_requests"`
	IncompleteRequests int64 `json:"incomplete_requests"`
}

func (c *common) usage(tenantID string) (usageTotals, error) {
	var reply struct {
		Total usageTotals `json:"total"`
	}
	err := c.admin(http.MethodGet, "/admin/tenants/"+tenantID+"/usage", nil, &reply)
	return reply.Total, err
}

// waitUsage polls the usage of each tenant until every one shows want
// requests, or timeout passes. The gateway writes usage rows in batches off
// the response path, so they land some time after the responses. It returns
// the last totals read, in the order of ids.
func (c *common) waitUsage(want int64, timeout time.Duration, ids ...string) ([]usageTotals, error) {
	deadline := time.Now().Add(timeout)
	for {
		totals := make([]usageTotals, len(ids))
		done := true
		for i, id := range ids {
			u, err := c.usage(id)
			if err != nil {
				return nil, setupf("reading usage: %w", err)
			}
			totals[i] = u
			done = done && u.Requests == want
		}
		if done || time.Now().After(deadline) {
			return totals, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// The request ttfb, ratelimit and budget send: an OpenAI chat completion,
// streamed, on the gateway's openai route. max_tokens is set, so the rate
// limiter charges it rather than the tenant's default_max_tokens, and the
// budget reserves it rather than the model's whole output limit. It is well
// above the mock's real output, so each settlement refunds most of the
// charge and the reservation: a refund that landed mid-burst would show.
const (
	streamProvider = "openai"
	streamModel    = "gpt-4o-mini"
	streamPath     = "/v1/chat/completions"
	streamBody     = `{"model":"` + streamModel + `","max_tokens":256,"stream":true,"stream_options":{"include_usage":true},` +
		`"messages":[{"role":"user","content":"Reply with the mock's deterministic sentence."}]}`
)

// streamRequest builds the stream request, to the gateway's openai route
// with key, or straight to the mock when key is "".
func (c *common) streamRequest(key string) *http.Request {
	url := c.mock + streamPath
	if key != "" {
		url = c.gateway + "/" + streamProvider + streamPath
	}
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(streamBody))
	req.Header.Set("Content-Type", "application/json")
	// The mock only checks that a credential is present.
	req.Header.Set("Authorization", "Bearer "+cmp.Or(key, "mock"))
	return req
}

// estimate is what the gateway reads from a request body, got by running
// the body through the gateway's own usage.ReadBody for its format.
func estimate(format provider.Format, path, body string) (usage.Request, error) {
	var got *usage.Request
	h := usage.ReadBody(format, 1<<20)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, _ = usage.RequestFrom(r.Context())
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	if got == nil {
		return usage.Request{}, fmt.Errorf("usage.ReadBody refused the request")
	}
	return *got, nil
}

// streamCharge is the rate limiter's charge for the stream request: its
// input estimate plus its output cap (internal/middleware's cost, for a
// request that sets max_tokens below the tenant's limit).
func streamCharge() (int64, error) {
	r, err := estimate(provider.FormatOpenAI, streamPath, streamBody)
	if err != nil {
		return 0, err
	}
	if r.OutputCap == 0 {
		return 0, fmt.Errorf("the stream request sets no max_tokens")
	}
	return r.InputEstimate + r.OutputCap, nil
}

// streamReservation is the budget's reservation for the stream request at
// the time at, as internal/middleware's Budget makes it.
func streamReservation(at time.Time) (int64, error) {
	r, err := estimate(provider.FormatOpenAI, streamPath, streamBody)
	if err != nil {
		return 0, err
	}
	amount, ok := pricing.Reserve(streamProvider, r.Model, r.InputReserve, r.OutputCap, r.Choices, at)
	if !ok {
		return 0, fmt.Errorf("model %q is not priced on %q", r.Model, streamProvider)
	}
	return amount, nil
}

// percentile returns the p-th percentile of xs by the nearest-rank method:
// the smallest value with at least p% of xs at or below it. xs is not
// changed. It returns 0 for no values.
func percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	rank := int(math.Ceil(p / 100 * float64(len(s))))
	return s[min(max(rank, 1), len(s))-1]
}

// ms is d in milliseconds, for results.
func ms(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}
