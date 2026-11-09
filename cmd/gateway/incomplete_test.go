package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/grishkadel/llm-gateway/internal/budget"
	"github.com/grishkadel/llm-gateway/internal/mockprovider"
	"github.com/grishkadel/llm-gateway/internal/pricing"
	"github.com/grishkadel/llm-gateway/internal/tenant"
	"github.com/grishkadel/llm-gateway/internal/usage"
)

// Incomplete usage, through the gateway with main's parts: a paid response
// whose final usage report the gateway never saw costs what its tenant's
// budget held for it, the reservation or what the usage seen costs if that's
// more. Its usage row, its request line, the cost counter and the ledger all
// carry that one figure, and a ledger rebuilt from the rows comes to the same
// spend.

// answerHeader names the answer an answerUpstream gives a request: a key of
// upstreamAnswers. The gateway passes it on, as it does any header but its
// own.
const answerHeader = "X-Test-Answer"

// upstreamAnswer is how an answerUpstream answers. With a status, it writes
// it, contentType and body, and flushes them. Then, with hold, it holds the
// response open until the request ends: with no status, before a word. With
// drop, it closes the connection having written nothing.
type upstreamAnswer struct {
	status      int
	contentType string
	body        string
	hold, drop  bool
}

// sseData is payloads as server-sent events, one each.
func sseData(payloads ...string) string {
	var b strings.Builder
	for _, p := range payloads {
		fmt.Fprintf(&b, "data: %s\n\n", p)
	}
	return b.String()
}

// The chunks the answers below are made of, in each format's own shapes.
const (
	chatChunk       = `{"id":"chatcmpl-x","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}],"usage":null}`
	chatFinishChunk = `{"id":"chatcmpl-x","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":null}`
	blockStart      = `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`
	blockDelta      = `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`
	blockStop       = `{"type":"content_block_stop","index":0}`
	messageStop     = `{"type":"message_stop"}`
	geminiFinish    = `{"candidates":[{"content":{"parts":[{"text":""}],"role":"model"},"finishReason":"STOP","index":0}],"modelVersion":"gemini-3.8-flash"}`
)

// messageStart is Anthropic's message_start, reporting input tokens of input
// beside 5 cache reads, and its placeholder output.
func messageStart(input int) string {
	return fmt.Sprintf(`{"type":"message_start","message":{"id":"msg_x","type":"message","role":"assistant","model":"claude-sonnet-4",`+
		`"content":[],"usage":{"input_tokens":%d,"cache_read_input_tokens":5,"output_tokens":1}}}`, input)
}

// geminiRunning is a Gemini chunk with no finishReason, so its usageMetadata
// is a running total, of candidates output tokens so far. Parsed, it is 15
// input, 5 cache reads, and candidates + 7 output, its thoughts included.
func geminiRunning(candidates int) string {
	return fmt.Sprintf(`{"candidates":[{"content":{"parts":[{"text":"w"}],"role":"model"},"index":0}],"usageMetadata":{"promptTokenCount":20,`+
		`"cachedContentTokenCount":5,"candidatesTokenCount":%d,"thoughtsTokenCount":7},"modelVersion":"gemini-3.8-flash"}`, candidates)
}

// upstreamAnswers are the answers an answerUpstream gives, by name: each
// format's 2xx without its final usage report, cut by the client (cut), at
// EOF (eof), or a body (body); errors with no usage; and no answer at all.
var upstreamAnswers = map[string]upstreamAnswer{
	"chat cut":    {status: http.StatusOK, contentType: "text/event-stream", body: sseData(chatChunk), hold: true},
	"chat eof":    {status: http.StatusOK, contentType: "text/event-stream", body: sseData(chatChunk, chatFinishChunk, "[DONE]")},
	"chat body":   {status: http.StatusOK, contentType: "application/json", body: `{"id":"chatcmpl-x","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`},
	"message cut": {status: http.StatusOK, contentType: "text/event-stream", body: sseData(messageStart(100_000)), hold: true},
	// A message_stop with no message_delta before it.
	"message eof":       {status: http.StatusOK, contentType: "text/event-stream", body: sseData(messageStart(15), blockStart, blockDelta, blockStop, messageStop)},
	"message body":      {status: http.StatusOK, contentType: "application/json", body: `{"id":"msg_x","type":"message","role":"assistant","model":"claude-sonnet-4","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`},
	"gemini sse cut":    {status: http.StatusOK, contentType: "text/event-stream", body: sseData(geminiRunning(1)), hold: true},
	"gemini sse eof":    {status: http.StatusOK, contentType: "text/event-stream", body: sseData(geminiRunning(1), geminiRunning(2))},
	"gemini array cut":  {status: http.StatusOK, contentType: "application/json", body: "[" + geminiRunning(1), hold: true},
	"gemini array eof":  {status: http.StatusOK, contentType: "application/json", body: "[" + geminiRunning(1) + ",\n" + geminiFinish + "]\n"},
	"gemini body":       {status: http.StatusOK, contentType: "application/json", body: geminiFinish},
	"a 429":             {status: http.StatusTooManyRequests, contentType: "application/json", body: `{"error":{"message":"Rate limit reached","type":"rate_limit_exceeded"}}`},
	"a 502":             {status: http.StatusBadGateway, contentType: "application/json", body: `{"type":"error","error":{"type":"api_error","message":"bad gateway"}}`},
	"a 504":             {status: http.StatusGatewayTimeout, contentType: "application/json", body: `{"error":{"code":504,"message":"deadline exceeded","status":"DEADLINE_EXCEEDED"}}`},
	"no answer":         {hold: true},
	"a dropped request": {drop: true},
}

// answerUpstream is a provider in every format at once. It gives each request
// the answer its answerHeader names, and one without the mock's.
type answerUpstream struct {
	url string
	// arrivals receives a value as each request held before a word arrives.
	// It has room for more than any test here sends.
	arrivals chan struct{}
}

func newAnswerUpstream(t *testing.T) *answerUpstream {
	t.Helper()
	u := &answerUpstream{arrivals: make(chan struct{}, 64)}
	release := make(chan struct{})
	mock := mockprovider.Handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a, ok := upstreamAnswers[r.Header.Get(answerHeader)]
		switch {
		case !ok:
			mock.ServeHTTP(w, r)
			return
		case a.drop:
			if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
				conn.Close()
			}
			return
		case a.status != 0:
			w.Header().Set("Content-Type", a.contentType)
			w.WriteHeader(a.status)
			_, _ = io.WriteString(w, a.body)
			_ = http.NewResponseController(w).Flush()
		default:
			u.arrivals <- struct{}{}
		}
		if a.hold {
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}
	}))
	// Cleanups run last-registered first, so held requests are let go before
	// Close, which waits for them.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	u.url = srv.URL
	return u
}

// incompleteConfig is a config file with a route for each paid format,
// openai, groq, anthropic and gemini, and local, free, all to the URL it is
// formatted with.
const incompleteConfig = `
providers:
  openai:
    url: %[1]s
    key: ${TEST_MOCK_KEY}
    auth: bearer
    format: openai
  groq:
    url: %[1]s/openai
    key: ${TEST_MOCK_KEY}
    auth: bearer
    format: openai
  anthropic:
    url: %[1]s
    key: ${TEST_MOCK_KEY}
    auth: x-api-key
    format: anthropic
    headers:
      anthropic-version: "2023-06-01"
  gemini:
    url: %[1]s
    key: ${TEST_MOCK_KEY}
    auth: x-goog-api-key
    format: gemini
  local:
    url: %[1]s
    auth: none
    format: openai
    free: true
`

// unansweredConfig is incompleteConfig's paid routes for requests no provider
// answers: openai to a port nothing listens on, and groq, anthropic and gemini
// to the URL it is formatted with, groq giving up after a second without
// response headers.
const unansweredConfig = `
providers:
  openai:
    url: http://127.0.0.1:1
    key: ${TEST_MOCK_KEY}
    auth: bearer
    format: openai
  groq:
    url: %[1]s/openai
    key: ${TEST_MOCK_KEY}
    auth: bearer
    format: openai
    timeout: 1
  anthropic:
    url: %[1]s
    key: ${TEST_MOCK_KEY}
    auth: x-api-key
    format: anthropic
    headers:
      anthropic-version: "2023-06-01"
  gemini:
    url: %[1]s
    key: ${TEST_MOCK_KEY}
    auth: x-goog-api-key
    format: gemini
`

// incompleteCap is the output cap of every request here but the cache's, so
// it is what each reserves for output.
const incompleteCap = 64

// The requests' bodies, each capped at incompleteCap.
func chatBody64(model string, stream bool) string {
	return fmt.Sprintf(`{"model":%q,"stream":%t,"max_tokens":%d,"messages":[{"role":"user","content":"hi"}]}`, model, stream, incompleteCap)
}

func messageBody64(stream bool) string {
	return fmt.Sprintf(`{"model":"claude-sonnet-4","stream":%t,"max_tokens":%d,"messages":[{"role":"user","content":"hi"}]}`, stream, incompleteCap)
}

var geminiBody64 = fmt.Sprintf(`{"contents":[{"parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":%d}}`, incompleteCap)

// The routes' paths.
const (
	chatPath        = "/v1/chat/completions"
	messagesPath    = "/v1/messages"
	geminiBodyPath  = "/v1beta/models/gemini-3.8-flash:generateContent"
	geminiSSEPath   = "/v1beta/models/gemini-3.8-flash:streamGenerateContent?alt=sse"
	geminiArrayPath = "/v1beta/models/gemini-3.8-flash:streamGenerateContent"
)

// incompleteCase is one request, how the client makes it, and how the
// gateway must account for it.
type incompleteCase struct {
	name string
	// route is the provider route, whose prices price it, and model the
	// model it asks for.
	route, model string
	path, body   string
	// answer is the answerUpstream's answer, a key of upstreamAnswers, or ""
	// for the mock's.
	answer string
	// How the client makes the request. cut: through a real server, it reads
	// what the upstream sent and leaves. gone: it leaves once the upstream has
	// the request. twice: it sends it twice, and the second counts. Otherwise
	// it sends it once and reads it all.
	cut, gone, twice bool

	status int
	// complete is the row's usage_complete, and seen the tokens it holds.
	complete bool
	seen     usage.Usage
	// held: the row costs what the budget held, the reservation or what seen
	// costs if that's more. Otherwise it costs 0.
	held bool
}

// incompleteGateway is a lineGateway and the upstream behind it.
type incompleteGateway struct {
	*lineGateway
	up *answerUpstream
}

// newIncompleteGateway builds the gateway from config, a config file's text
// formatted with an answerUpstream's URL, for cases. The gateway may log, for
// each case it holds a paid response's hold for, the Warn that says so, and
// what else warns allows.
func newIncompleteGateway(t *testing.T, config string, cases []incompleteCase, warns map[string]int) incompleteGateway {
	t.Helper()
	up := newAnswerUpstream(t)
	t.Setenv("TEST_MOCK_KEY", "mock")
	for _, tc := range cases {
		if tc.held {
			warns["usage incomplete; charged the budget's hold"]++
		}
	}
	return incompleteGateway{newLineGateway(t, loadConfig(t, fmt.Sprintf(config, up.url)), warns), up}
}

// send makes tc's request as the tenant with key, as tc says, and returns the
// status the client got and the request's ID.
func (g incompleteGateway) send(t *testing.T, key string, tc incompleteCase) (status int, id string) {
	t.Helper()
	request := func(ctx context.Context, url string) *http.Request {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/"+tc.route+tc.path, strings.NewReader(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		if tc.answer != "" {
			req.Header.Set(answerHeader, tc.answer)
		}
		return req
	}

	switch {
	case tc.cut:
		// done closes when the gateway is through with the request, abort
		// included.
		done := make(chan struct{})
		gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			defer close(done)
			g.ServeHTTP(w, req)
		}))
		defer gw.Close()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		resp, err := gw.Client().Do(request(ctx, gw.URL))
		if err != nil {
			t.Fatal(err)
		}
		// What the upstream sent went through the gateway's meter on its way
		// here.
		if _, err := io.ReadFull(resp.Body, make([]byte, len(upstreamAnswers[tc.answer].body))); err != nil {
			t.Fatalf("%s: reading the stream: %v", tc.name, err)
		}
		cancel()
		resp.Body.Close()
		receive(t, done, "the gateway to finish the request")
		return resp.StatusCode, resp.Header.Get("X-Request-ID")

	case tc.gone:
		// Only earlier requests' arrivals, all over by now.
		for len(g.up.arrivals) > 0 {
			<-g.up.arrivals
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := serveAsync(g, request(ctx, ""))
		receive(t, g.up.arrivals, "the request at the provider")
		cancel()
		rec := receive(t, done, "the response")
		return rec.Code, rec.Header().Get("X-Request-ID")
	}

	rec := serve(g, request(context.Background(), ""))
	failedClosed(t, rec)
	return rec.Code, rec.Header().Get("X-Request-ID")
}

// costCounted returns what gateway_cost_micros_total has counted for the
// tenant tenantID, over every provider and model.
func (g incompleteGateway) costCounted(t *testing.T, tenantID string) float64 {
	t.Helper()
	var sum float64
	for labels, v := range samples(t, scrapeMetrics(t, g.metrics), "gateway_cost_micros_total", "tenant", "provider", "model") {
		if strings.HasPrefix(labels, tenantID+",") {
			sum += v
		}
	}
	return sum
}

// adminGet sends GET path to the admin API, and decodes its answer into v.
func (g incompleteGateway) adminGet(t *testing.T, path string, v any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+testAdminKey)
	rec := serve(g, req)
	failedClosed(t, rec)
	if err := json.Unmarshal(rec.Body.Bytes(), v); rec.Code != http.StatusOK || err != nil {
		t.Fatalf("GET %s: status %d, body %s (%v); want 200 and JSON", path, rec.Code, rec.Body, err)
	}
}

// spent returns the tenant's spend for the month, as the admin API's budget
// report gives it: the ledger's, rebuilt first if its hash is missing.
func (g incompleteGateway) spent(t *testing.T, tenantID string) int64 {
	t.Helper()
	var report struct {
		SpentMicros int64 `json:"spent_micros"`
	}
	g.adminGet(t, "/admin/tenants/"+tenantID+"/budget", &report)
	return report.SpentMicros
}

// loseMonth deletes the tenant's hash for this month and its missed counter,
// as a Redis restart would, and fails the test if there was no hash.
func (g incompleteGateway) loseMonth(t *testing.T, tenantID string) {
	t.Helper()
	start, _ := budget.Period(time.Now())
	hash := "budget:{" + tenantID + "}:" + start.Format("2006-01")
	if n, err := g.rdb.Del(context.Background(), hash, hash+":missed").Result(); err != nil || n == 0 {
		t.Fatalf("deleting %s: %d keys deleted (%v); want the hash at least", hash, n, err)
	}
}

// check sends each of cases as a tenant of its own, and then checks how the
// gateway accounted for each:
//   - its row: the status the client got, usage_complete, the tokens seen,
//     and its cost
//   - its request line, whose usage fields repeat the row's
//   - the cost counter, whose increase is the row's cost
//   - the ledger, whose spend is the tenant's rows', and is again once the
//     month is lost and rebuilt from them
//   - the admin usage report, whose incomplete_requests counts the tenant's
//     rows with incomplete usage
func (g incompleteGateway) check(t *testing.T, cases []incompleteCase) {
	type sent struct {
		tenantID, id string
		at           time.Time
		counted      float64 // the cost counter's increase
	}
	sends := make([]sent, len(cases))
	for i, tc := range cases {
		tn, key := g.newTenant(t, tc.name, tenant.Update{})
		if tc.twice {
			first := tc
			first.twice = false
			g.send(t, key, first)
		}
		before := g.costCounted(t, tn)
		at := time.Now()
		status, id := g.send(t, key, tc)
		if status != tc.status {
			t.Fatalf("%s: status %d, want %d", tc.name, status, tc.status)
		}
		sends[i] = sent{tn, id, at, g.costCounted(t, tn) - before}
	}
	// The writer inserts its rows in batches, off the request path: Close
	// inserts the rest.
	if err := g.writer.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	for i, tc := range cases {
		s := sends[i]
		t.Run(tc.name, func(t *testing.T) {
			var want int64
			if tc.held {
				reserve, reserved := pricing.Reserve(tc.route, tc.model, reserveTokens(tc.body), incompleteCap, 1, s.at)
				seen, priced := pricing.Cost(tc.route, tc.seen, tc.model, s.at)
				if !reserved || !priced {
					t.Fatalf("%s/%s should be priced", tc.route, tc.model)
				}
				want = max(reserve, seen)
			}
			var status int
			if err := g.conn.QueryRow(`SELECT status FROM usage_logs WHERE request_id = $1`, s.id).Scan(&status); err != nil {
				t.Fatal(err)
			}
			row := g.row(t, s.id, tc.route)
			if status != tc.status || row.UsageComplete != tc.complete || row.Input != tc.seen.Input || row.CachedInput != tc.seen.CachedInput ||
				row.CacheWrite != tc.seen.CacheWrite+tc.seen.CacheWrite1h || row.Output != tc.seen.Output || !row.Cost.Valid || row.Cost.Int64 != want {
				t.Errorf("row: status %d, %+v; want status %d, usage_complete %v, the tokens of %+v, at cost %d",
					status, row, tc.status, tc.complete, tc.seen, want)
			}

			wantLine := with(row.fields(), map[string]any{"tenant_id": s.tenantID, "provider": tc.route, "model": tc.model})
			if got := fromRecord(g.line(t, s.id)); !reflect.DeepEqual(got, wantLine) {
				t.Errorf("fields from the Record = %v\nwant %v", got, wantLine)
			}
			if s.counted != float64(row.Cost.Int64) {
				t.Errorf("gateway_cost_micros_total rose by %v, want the row's cost, %d", s.counted, row.Cost.Int64)
			}

			var rows, sum, incomplete int64
			err := g.conn.QueryRow(`SELECT count(*), COALESCE(SUM(cost_micros), 0), count(*) FILTER (WHERE NOT usage_complete)
				FROM usage_logs WHERE tenant_id = $1`, s.tenantID).Scan(&rows, &sum, &incomplete)
			if err != nil {
				t.Fatal(err)
			}
			if got := g.spent(t, s.tenantID); got != sum {
				t.Errorf("spent %d µ$, want the tenant's rows', %d", got, sum)
			}
			g.loseMonth(t, s.tenantID)
			if got := g.spent(t, s.tenantID); got != sum {
				t.Errorf("rebuilt, spent %d µ$, want the tenant's rows', %d", got, sum)
			}
			var report struct {
				Total usage.Totals `json:"total"`
			}
			g.adminGet(t, "/admin/tenants/"+s.tenantID+"/usage", &report)
			if tot := report.Total; tot.Requests != rows || tot.CostMicros != sum || tot.IncompleteRequests != incomplete {
				t.Errorf("usage report total = %+v, want %d requests, %d µ$, %d incomplete", tot, rows, sum, incomplete)
			}
		})
	}
}

// TestIncompleteUsageKeepsItsHold: for each format, a paid 2xx that never
// carried its final usage report to the gateway costs its hold, whether the
// client cut it after the headers, it ended at EOF, or it was a body without
// usage. Its row has usage_complete false and the tokens seen. A provider's
// error with no usage is complete and costs 0. A free provider's cut stream
// costs 0 too, with usage_complete false, and a cache hit costs 0, complete.
func TestIncompleteUsageKeepsItsHold(t *testing.T) {
	// The tokens what each answer reports come to, by the format's parser.
	messageSeen := func(input int64) usage.Usage { return usage.Usage{Input: input, CachedInput: 5} }
	geminiSeen := func(candidates int64) usage.Usage {
		return usage.Usage{Input: 15, CachedInput: 5, Output: candidates + 7}
	}
	const gpt4o, oss, sonnet, flash = "gpt-4o", "openai/gpt-oss-120b", "claude-sonnet-4", "gemini-3.8-flash"
	cases := []incompleteCase{
		// Cut by the client after the headers and what the upstream sent with
		// them.
		{name: "openai stream, cut", route: "openai", model: gpt4o, path: chatPath, body: chatBody64(gpt4o, true), answer: "chat cut", cut: true,
			status: http.StatusOK, held: true},
		{name: "groq stream, cut", route: "groq", model: oss, path: chatPath, body: chatBody64(oss, true), answer: "chat cut", cut: true,
			status: http.StatusOK, held: true},
		// message_start's input alone costs more than the reservation.
		{name: "anthropic stream, cut", route: "anthropic", model: sonnet, path: messagesPath, body: messageBody64(true), answer: "message cut", cut: true,
			status: http.StatusOK, seen: messageSeen(100_000), held: true},
		{name: "gemini sse, cut", route: "gemini", model: flash, path: geminiSSEPath, body: geminiBody64, answer: "gemini sse cut", cut: true,
			status: http.StatusOK, seen: geminiSeen(1), held: true},
		// Half an array, which doesn't parse.
		{name: "gemini json array, cut", route: "gemini", model: flash, path: geminiArrayPath, body: geminiBody64, answer: "gemini array cut", cut: true,
			status: http.StatusOK, held: true},

		// Ended at EOF without the report.
		{name: "openai stream without its report", route: "openai", model: gpt4o, path: chatPath, body: chatBody64(gpt4o, true), answer: "chat eof",
			status: http.StatusOK, held: true},
		{name: "groq stream without its report", route: "groq", model: oss, path: chatPath, body: chatBody64(oss, true), answer: "chat eof",
			status: http.StatusOK, held: true},
		{name: "anthropic stream without its report", route: "anthropic", model: sonnet, path: messagesPath, body: messageBody64(true), answer: "message eof",
			status: http.StatusOK, seen: messageSeen(15), held: true},
		{name: "gemini sse without its report", route: "gemini", model: flash, path: geminiSSEPath, body: geminiBody64, answer: "gemini sse eof",
			status: http.StatusOK, seen: geminiSeen(2), held: true},
		{name: "gemini json array without its report", route: "gemini", model: flash, path: geminiArrayPath, body: geminiBody64, answer: "gemini array eof",
			status: http.StatusOK, seen: geminiSeen(1), held: true},

		// Bodies without usage.
		{name: "openai body without usage", route: "openai", model: gpt4o, path: chatPath, body: chatBody64(gpt4o, false), answer: "chat body",
			status: http.StatusOK, held: true},
		{name: "groq body without usage", route: "groq", model: oss, path: chatPath, body: chatBody64(oss, false), answer: "chat body",
			status: http.StatusOK, held: true},
		{name: "anthropic body without usage", route: "anthropic", model: sonnet, path: messagesPath, body: messageBody64(false), answer: "message body",
			status: http.StatusOK, held: true},
		{name: "gemini body without usage", route: "gemini", model: flash, path: geminiBodyPath, body: geminiBody64, answer: "gemini body",
			status: http.StatusOK, held: true},

		// A provider's errors report no usage, and are complete.
		{name: "a provider's 429", route: "openai", model: gpt4o, path: chatPath, body: chatBody64(gpt4o, false), answer: "a 429",
			status: http.StatusTooManyRequests, complete: true},
		{name: "a provider's 502", route: "anthropic", model: sonnet, path: messagesPath, body: messageBody64(false), answer: "a 502",
			status: http.StatusBadGateway, complete: true},
		{name: "a provider's 504", route: "gemini", model: flash, path: geminiBodyPath, body: geminiBody64, answer: "a 504",
			status: http.StatusGatewayTimeout, complete: true},

		{name: "a free provider's stream, cut", route: "local", model: "qwen3.5:9b", path: chatPath, body: chatBody64("qwen3.5:9b", true),
			answer: "chat cut", cut: true, status: http.StatusOK},
		// The first is the miss, from the mock; the second the hit.
		{name: "a cache hit", route: "openai", model: gpt4o, path: chatPath, body: zeroTemperature, twice: true,
			status: http.StatusOK, complete: true},
	}
	g := newIncompleteGateway(t, incompleteConfig, cases, map[string]int{})
	g.check(t, cases)
}

// TestUnansweredUsageKeepsItsHold: a paid request the provider got and never
// answered may have been billed, so it costs its hold, as an incomplete
// response does: a 504 when the provider takes too long, a 499 when the client
// leaves first, and a 502 when the connection drops. A request whose dial
// failed never left the gateway: it costs 0, complete.
func TestUnansweredUsageKeepsItsHold(t *testing.T) {
	cases := []incompleteCase{
		{name: "a 504 after the request was sent", route: "groq", model: "openai/gpt-oss-120b", path: chatPath,
			body: chatBody64("openai/gpt-oss-120b", false), answer: "no answer", status: http.StatusGatewayTimeout, held: true},
		{name: "a 499 after the request was sent", route: "anthropic", model: "claude-sonnet-4", path: messagesPath,
			body: messageBody64(true), answer: "no answer", gone: true, status: 499, held: true},
		{name: "a 502 after the request was sent", route: "gemini", model: "gemini-3.8-flash", path: geminiBodyPath,
			body: geminiBody64, answer: "a dropped request", status: http.StatusBadGateway, held: true},
		{name: "a failed dial", route: "openai", model: "gpt-4o", path: chatPath,
			body: chatBody64("gpt-4o", false), status: http.StatusBadGateway, complete: true},
	}
	g := newIncompleteGateway(t, unansweredConfig, cases, map[string]int{"upstream request failed": 3})
	g.check(t, cases)
}
