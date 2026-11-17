package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"slices"
	"sync"
	"time"
)

// cacheModels are the priced models the cache mix spreads over, two on each
// of the openai, anthropic and gemini routes. The mock answers any model.
var cacheModels = []struct{ provider, model string }{
	{"openai", "gpt-4o-mini"},
	{"openai", "gpt-4.1-mini"},
	{"anthropic", "claude-haiku-4-5"},
	{"anthropic", "claude-sonnet-4-5"},
	{"gemini", "gemini-2.5-flash"},
	{"gemini", "gemini-2.5-flash-lite"},
}

// zipfS is the Zipf exponent of the cache mix's prompt popularity.
const zipfS = 1.2

// mix returns n prompt numbers in [0, prompts), drawn Zipf(zipfS) from seed:
// prompt 0 the most often, and prompt k about (k+1)^-zipfS as often as it.
// The same seed gives the same mix.
func mix(seed uint64, n, prompts int) []int {
	z := rand.NewZipf(rand.New(rand.NewPCG(seed, seed)), zipfS, 1, uint64(prompts-1))
	out := make([]int, n)
	for i := range out {
		out[i] = int(z.Uint64())
	}
	return out
}

// cacheRequest builds the request for prompt p, with key and the
// X-Gateway-Cache header set to mode. Each prompt has one model, so the
// prompts are the distinct requests in the mix: they take the models in turn.
func (c *common) cacheRequest(p int, key, mode string) *http.Request {
	m := cacheModels[p%len(cacheModels)]
	text := fmt.Sprintf("Benchmark prompt %d: say something about caching.", p)
	messages := []map[string]string{{"role": "user", "content": text}}
	var path, header string
	var body any
	switch m.provider {
	case "openai":
		path, header = "/openai/v1/chat/completions", "Authorization"
		key = "Bearer " + key
		body = map[string]any{"model": m.model, "max_tokens": 16, "messages": messages}
	case "anthropic":
		path, header = "/anthropic/v1/messages", "X-Api-Key"
		body = map[string]any{"model": m.model, "max_tokens": 16, "messages": messages}
	case "gemini":
		path, header = "/gemini/v1beta/models/"+m.model+":generateContent", "X-Goog-Api-Key"
		body = map[string]any{
			"contents":         []any{map[string]any{"role": "user", "parts": []any{map[string]string{"text": text}}}},
			"generationConfig": map[string]int{"maxOutputTokens": 16},
		}
	}
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, c.gateway+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(header, key)
	req.Header.Set("X-Gateway-Cache", mode)
	return req
}

// cacheRun is one pass of the mix: the responses by status, and by their
// X-Gateway-Cache header.
type cacheRun struct {
	Statuses map[int]int    `json:"statuses"`
	Cache    map[string]int `json:"cache"`
	Seconds  float64        `json:"seconds"`
}

// send sends the mix to the tenant with key, workers at a time.
func (c *common) send(prompts []int, key, mode string, workers int) (cacheRun, error) {
	run := cacheRun{Statuses: map[int]int{}, Cache: map[string]int{}}
	var mu sync.Mutex
	var firstErr error
	next := make(chan int)
	var wg sync.WaitGroup
	start := time.Now()
	for range workers {
		wg.Go(func() {
			for p := range next {
				status, cache, err := do(c.cacheRequest(p, key, mode))
				mu.Lock()
				if err != nil && firstErr == nil {
					firstErr = err
				}
				run.Statuses[status]++
				run.Cache[cache]++
				mu.Unlock()
			}
		})
	}
	for _, p := range prompts {
		next <- p
	}
	close(next)
	wg.Wait()
	run.Seconds = time.Since(start).Seconds()
	if firstErr != nil {
		return run, setupf("cache request: %w", firstErr)
	}
	return run, nil
}

// do sends req, reads its response to the end, and returns its status and
// X-Gateway-Cache header.
func do(req *http.Request) (status int, cache string, err error) {
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	_, err = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, resp.Header.Get("X-Gateway-Cache"), err
}

// runCache sends one seeded mix to a tenant with the cache on, then the same
// mix to a tenant with it off, and compares what each was billed. It passes
// when every response was a 200 and both tenants' usage shows every request.
func runCache(args []string) (result, error) {
	var c common
	fs := flag.NewFlagSet("cache", flag.ContinueOnError)
	seed := fs.Uint64("seed", 1, "seed of the request mix")
	n := fs.Int("n", 1000, "requests in the mix")
	prompts := fs.Int("prompts", 200, "distinct prompts the mix draws from")
	workers := fs.Int("c", 20, "requests in flight at once")
	if err := parse(fs, &c, args); err != nil {
		return result{}, err
	}
	if *n < 1 || *prompts < 2 || *workers < 1 {
		return result{}, setupf("-n and -c must be at least 1, and -prompts at least 2")
	}

	limits := map[string]int64{"rate_limit_tokens_per_min": 1_000_000_000, "budget_micros": 1_000_000_000}
	on, err := c.newTenant("cache-on", limits)
	if err != nil {
		return result{}, err
	}
	off, err := c.newTenant("cache-off", limits)
	if err != nil {
		return result{}, err
	}

	ps := mix(*seed, *n, *prompts)
	onRun, err := c.send(ps, on.Key, "on", *workers)
	if err != nil {
		return result{}, err
	}
	offRun, err := c.send(ps, off.Key, "off", *workers)
	if err != nil {
		return result{}, err
	}
	totals, err := c.waitUsage(int64(*n), 30*time.Second, on.ID, off.ID)
	if err != nil {
		return result{}, err
	}
	onUsage, offUsage := totals[0], totals[1]

	var saving float64
	if offUsage.CostMicros > 0 {
		saving = 100 * float64(offUsage.CostMicros-onUsage.CostMicros) / float64(offUsage.CostMicros)
	}
	hits := onRun.Cache["hit"]
	// At most n - distinct can be hits: each distinct request misses once, and
	// again for each copy sent while its first was still in flight.
	distinct := len(slices.Compact(slices.Sorted(slices.Values(ps))))
	pass := onUsage.Requests == int64(*n) && offUsage.Requests == int64(*n) &&
		onRun.Statuses[http.StatusOK] == *n && offRun.Statuses[http.StatusOK] == *n
	fmt.Printf("cache: %d requests, Zipf(%.1f) over %d prompts, seed %d, %d in flight\n", *n, zipfS, *prompts, *seed, *workers)
	fmt.Printf("  cache on:  200s %d, hits %d of at most %d, usage rows %d, cost %d µ$ (%.1f s)\n",
		onRun.Statuses[http.StatusOK], hits, *n-distinct, onUsage.Requests, onUsage.CostMicros, onRun.Seconds)
	fmt.Printf("  cache off: 200s %d, usage rows %d, cost %d µ$ (%.1f s)\n",
		offRun.Statuses[http.StatusOK], offUsage.Requests, offUsage.CostMicros, offRun.Seconds)
	fmt.Printf("  saving %.1f%%\n", saving)

	return finish(c.out, result{
		Subcommand: "cache",
		Inputs: map[string]any{
			"seed": *seed, "n": *n, "prompts": *prompts, "c": *workers, "zipf_s": zipfS,
			"gateway": c.gateway, "tenant_on": on.ID, "tenant_off": off.ID,
		},
		Measurements: map[string]any{
			"on": onRun, "off": offRun, "hits": hits, "distinct_prompts": distinct,
			"usage_on": onUsage, "usage_off": offUsage, "saving_percent": saving,
		},
		Pass: pass,
	})
}
