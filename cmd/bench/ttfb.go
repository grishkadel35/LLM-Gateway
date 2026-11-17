package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// stream is one streamed response, timed from just before the request was
// sent.
type stream struct {
	status int
	// header is when the response headers arrived.
	header time.Duration
	// first and last are when the first and the last SSE "data:" lines
	// arrived: 0 when none did.
	first, last time.Duration
	// end is when the last byte had been read.
	end time.Duration
}

// timeStream sends req and reads its response to the end, timing it from
// start.
func timeStream(req *http.Request, start time.Time) (stream, error) {
	resp, err := client.Do(req)
	if err != nil {
		return stream{}, err
	}
	defer resp.Body.Close()
	s := stream{status: resp.StatusCode, header: time.Since(start)}
	br := bufio.NewReader(resp.Body)
	for {
		line, err := br.ReadString('\n')
		if strings.HasPrefix(line, "data:") {
			s.last = time.Since(start)
			if s.first == 0 {
				s.first = s.last
			}
		}
		if err == io.EOF {
			s.end = time.Since(start)
			return s, nil
		}
		if err != nil {
			return s, err
		}
	}
}

type latency struct {
	P50 float64 `json:"p50_ms"`
	P95 float64 `json:"p95_ms"`
	P99 float64 `json:"p99_ms"`
}

func latencyOf(xs []float64) latency {
	return latency{percentile(xs, 50), percentile(xs, 95), percentile(xs, 99)}
}

type ttfbSide struct {
	TTFB  latency `json:"ttfb"`
	Total latency `json:"total"`
}

// runTTFB sends n streams one after another through the gateway, then n
// straight to the mock, and passes when the gateway's p99 time to the first
// event is under half the mock's p50 time to the end: a gateway that
// buffered the stream would hold its first event until about the end.
func runTTFB(args []string) (result, error) {
	var c common
	fs := flag.NewFlagSet("ttfb", flag.ContinueOnError)
	n := fs.Int("n", 50, "streams through the gateway, and again straight to the mock")
	chunkDelay := fs.Duration("chunk-delay", 50*time.Millisecond, "the -chunk-delay the mock runs with, for the record")
	if err := parse(fs, &c, args); err != nil {
		return result{}, err
	}
	if *n < 1 {
		return result{}, setupf("-n must be at least 1")
	}

	t, err := c.newTenant("ttfb", map[string]int64{"rate_limit_tokens_per_min": 1_000_000_000, "budget_micros": 1_000_000_000})
	if err != nil {
		return result{}, err
	}

	measure := func(key string) (ttfbSide, error) {
		var first, end []float64
		for range *n {
			start := time.Now()
			s, err := timeStream(c.streamRequest(key), start)
			if err != nil {
				return ttfbSide{}, setupf("stream: %w", err)
			}
			if s.status != http.StatusOK {
				return ttfbSide{}, setupf("stream: HTTP %d", s.status)
			}
			if s.first == 0 {
				return ttfbSide{}, setupf("stream: no data line")
			}
			first = append(first, ms(s.first))
			end = append(end, ms(s.end))
		}
		return ttfbSide{latencyOf(first), latencyOf(end)}, nil
	}
	gateway, err := measure(t.Key)
	if err != nil {
		return result{}, err
	}
	direct, err := measure("")
	if err != nil {
		return result{}, err
	}

	limit := direct.Total.P50 / 2
	pass := gateway.TTFB.P99 < limit
	fmt.Printf("ttfb: %d streams each way, mock chunk delay %s\n", *n, *chunkDelay)
	fmt.Printf("  gateway  ttfb p50/p95/p99 %.2f/%.2f/%.2f ms  total p50/p95/p99 %.1f/%.1f/%.1f ms\n",
		gateway.TTFB.P50, gateway.TTFB.P95, gateway.TTFB.P99, gateway.Total.P50, gateway.Total.P95, gateway.Total.P99)
	fmt.Printf("  direct   ttfb p50/p95/p99 %.2f/%.2f/%.2f ms  total p50/p95/p99 %.1f/%.1f/%.1f ms\n",
		direct.TTFB.P50, direct.TTFB.P95, direct.TTFB.P99, direct.Total.P50, direct.Total.P95, direct.Total.P99)
	fmt.Printf("  check: gateway ttfb p99 %.2f ms < direct total p50 / 2 = %.2f ms\n", gateway.TTFB.P99, limit)

	return finish(c.out, result{
		Subcommand: "ttfb",
		Inputs: map[string]any{
			"n": *n, "chunk_delay_ms": ms(*chunkDelay), "gateway": c.gateway, "mock": c.mock,
			"model": streamModel, "tenant_id": t.ID,
		},
		Measurements: map[string]any{"gateway": gateway, "direct": direct, "limit_ms": limit},
		Pass:         pass,
	})
}
