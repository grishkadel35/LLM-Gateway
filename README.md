# llm-gateway

A reverse proxy that sits between your applications and LLM providers. It routes
each request to the right provider, presents that provider's API key in whatever
header it expects, and logs the result. It enforces each tenant's
tokens-per-minute limit and answers repeated requests from a response cache;
budget enforcement plugs in later.

Providers speak their own native APIs — the gateway never translates request or
response bodies, so new provider features work the day they ship. Adding a
provider is a block of YAML, not code.

> **⚠️ Keep it on loopback for now.** Every provider route requires a tenant
> key, but there is no TLS or budget enforcement yet, so the gateway binds to
> `127.0.0.1` by default. Binding wider is your call to make deliberately.

## Project status

**Last updated:** 2026-10-17
**Stage:** Weeks 1 to 4 of 8 complete, checkpoints closed. Every provider
request is authenticated with a tenant key and held to its tenant's
tokens-per-minute limit, and every request that reaches a provider, streamed or
not, is recorded in Postgres with its tokens and cost, verified live against
Groq and Gemini. A tenant over its limit gets `429` with `Retry-After` before
any provider is called, and each charge is corrected to the tokens the
provider reported. A repeated non-streaming request that sets temperature 0 or
sends `X-Gateway-Cache: on` is answered from Redis, without a charge and without
calling the provider, in about 1.1 ms (median of 20 hits, by curl).

### What changed

- **Gemini rows get a provider request ID.** Gemini sends no request-ID
  header, so `provider_request_id` was always empty on its usage rows. The
  gateway now takes the `responseId` that every Gemini response body carries,
  streamed or not. OpenAI and Groq still supply theirs as `x-request-id`, and
  Anthropic as `request-id`.
- **Week 4 checkpoint closed.** Tests through the real router, against a real
  Postgres and Redis, show that on each of the four mock routes the same
  temperature-0 request sent twice makes one provider call, gets a
  byte-identical `hit` the second time, and charges the tenant once; that the
  median of 20 hits takes 0.67–0.72 ms under `-race`; and that two tenants
  never get each other's entries. A live check against Groq and Gemini passed:
  a hit came back in about 1.1 ms through the real server. See
  [Week 4 deliverables](#week-4-deliverables--response-caching-) and
  [The cache contract](#the-cache-contract).
- **Response cache, wired in.** A repeated request is now answered from
  Redis, and neither the rate limit nor the provider sees it. A
  non-streaming request is eligible when it sets temperature to exactly 0 or
  sends `X-Gateway-Cache: on`. `off` opts it out, and any other value gets
  `400 invalid_request`. Every response that reaches the cache carries
  `X-Gateway-Cache: hit`, `miss` or `bypass`, in place of any value a
  provider sent. A miss's response is stored before it finishes, and only
  when it is a complete 2xx from the route's own provider: never a stream, a
  fallback's answer, or a body over 1 MiB. Redis failing never fails a
  request: a failed lookup is a miss, logged with a Warn, and isn't stored.
  A hit leaves a usage row of its own, marked `cached`, with no tokens and
  cost 0. On a route with a fallback, a hit carries `X-Gateway-Provider`
  (the route's provider) and `X-Gateway-Fallback: false`, like every other
  response there.
- **How the cache keys and stores.** The body owner records whether a
  request asks for temperature exactly 0: a top-level `temperature` number
  for OpenAI and Anthropic, and `generationConfig.temperature` for Gemini,
  where a numeric string counts too. The body is never changed for this.
  `config.yaml` has a `cache.ttl` in seconds (default 3,600, at least 1), and
  the proxy removes every `X-Gateway-*` request header, such as
  `X-Gateway-Cache`, before forwarding, so the client's instructions to the
  gateway never reach a provider.
  `internal/cache` names each request by a SHA-256 over its tenant, provider,
  path, query (without `key=`), Anthropic version headers, and its body as
  canonical JSON: whitespace and key order don't matter, while `1` and `1.0`
  stay different. A request that two different bodies or queries could
  share, such as one whose body isn't a single valid JSON value, gets no key
  and bypasses the cache. So does a request body over 4 MiB: building its key
  costs about six times the body in memory, and the cache runs before the
  rate limiter. Entries live in Redis, each written with its TTL in one
  `SET … EX`: a 2xx status, Content-Type, model and body (at most 1 MiB), the
  body kept byte for byte.
- **Week 3 checkpoint closed.** Tests through the real router against a real
  Redis show a 100,000 tokens/min tenant refused exactly at its threshold, let
  back in by the refill and by its refunds, and never over-admitted by 50
  simultaneous requests. A live check against Groq and Gemini passed. See
  [Week 3 deliverables](#week-3-deliverables--token-aware-rate-limiting-) and
  [The 429 contract](#the-429-contract).
- **Rate limiting is on.** Every provider request is charged against its
  tenant's tokens-per-minute bucket before it is forwarded. The charge is the
  token estimate plus the output the client allowed: its own cap, or the
  tenant's `default_max_tokens` for each choice when it set none. The output
  part is capped at the tenant's limit. A tenant over its limit gets
  `429 rate_limited` with `Retry-After` in whole seconds, and the provider is
  never called. The body names the gateway, so a client can tell it from a
  provider's own 429, and carries `retry_after_seconds`,
  `limit_tokens_per_min` and `estimated_tokens`. When the response is over,
  the charge is corrected to the tokens the provider reported: the unused
  part comes back, and a request that used more pays the difference. A
  response cut short (the client left, or the provider never answered)
  refunds nothing, since its real cost is unknown, but tokens already seen
  beyond the charge are still charged. A request that never reached a
  provider (`503 provider_busy`) gets its whole charge back, and one that fell
  back to another provider is settled once, by the provider that answered.
  Each Redis call gives up after 100 ms; when Redis fails, the request goes
  through and the gateway logs a warning: rate limiting fails open. The
  gateway now needs `REDIS_URL` to start, and pings Redis at startup. Set
  limits with SQL for now (`tenants.rate_limit_tokens_per_min`).
- **Local Ollama.** `config.yaml` now has an `ollama` provider: a model on this
  machine, served at `/ollama/` and metered like the hosted ones. It is keyless
  (`auth: none`; clients still need a gateway key) and `free: true`, so every
  usage row costs 0 with its tokens still recorded. Provider settings such as
  `url` and `timeout` accept `${VAR:-default}`. `max_concurrency` queues
  requests for a busy provider and answers `503 provider_busy` when
  `queue_timeout` runs out; a background check of `health_path` shows in
  `/health`; and an optional `fallback` retries a failed non-streaming request
  on another provider (Groq), off by default because it spends real money.
  `GET /metrics` serves Prometheus metrics. A live check against Ollama 0.40.2
  with `qwen3.5:9b` showed that it thinks by default, which is slow and counts
  as output tokens, and that a model that isn't loaded takes about 30 s to
  load. See [Running with Ollama](#running-with-ollama).
- **Token bucket in Redis.** `internal/ratelimit` keeps each
  tenant's tokens-per-minute bucket in Redis. Admission and refunds each run as
  one Lua script on Redis's clock, so concurrent requests can't spend the same
  tokens and replicas agree on time. A request bigger than a whole minute's
  limit gets in on a full bucket and leaves the tenant in debt. A bucket's key
  expires only when it would be full again at its limit, so expiry never
  forgives debt, with one exception: after a limit is lowered, an idle
  tenant's key can still expire when its bucket would have been full at the
  old limit.
- **Token estimate per request.** Before forwarding, the gateway estimates a
  request's tokens from its body: one per 4 bytes of text (system prompt, tools
  and every other field included), plus a flat 1,600 per image, audio,
  document or file part, whose base64 payload is left out. Counted as text, a
  1 MB image would come to ~350,000 tokens. It also reads the output limit the
  client set (`max_completion_tokens` or `max_tokens` × `n`, Anthropic
  `max_tokens`, Gemini `maxOutputTokens` × `candidateCount`). The body is never
  changed for this. The Week 3 rate limiter admits requests against the
  estimate, then corrects it to the provider's own count.
- **Usage reports.** `GET /admin/tenants/{id}/usage?from=&to=` sums a
  tenant's usage over a period (RFC 3339, default: the current UTC month):
  totals plus a breakdown by provider and model, with `unpriced_requests`
  counting rows that had no cost, so a partial total isn't mistaken for a
  complete one.
- **Usage is recorded in Postgres.** Every request that reaches a provider
  now leaves one `usage_logs` row: the `X-Request-ID` the client got, the
  provider's own request ID, tenant, key, provider, model, endpoint, status,
  tokens, cost in micro-dollars (NULL, with a warning, for an unpriced model or
  a response whose usage is incomplete),
  whether it streamed, and latency. A request that got no response (timeout,
  unreachable provider, client gone) still leaves a row, with status
  499/502/504 and NULL cost: the provider may have billed it. Rows are queued and batch-inserted every
  100 rows or second, off the response path: a full queue drops a row (logged
  and counted) rather than slow a response down, and shutdown drains the
  queue. This replaces the interim `usage` log line.
- **Pricing.** `internal/pricing` holds per-model prices for all four
  providers in integer micro-dollars, copied from each provider's pricing
  page on 2026-10-05 (source and date noted per block). It handles dated
  snapshots (`gpt-4o-2024-08-06` → `gpt-4o`), long-context tiers (OpenAI above
  272K, Gemini Pro above 200K input tokens), prices that change on a date
  (Gemini 3.6–3.8 Flash double on 1 January 2027), Anthropic's 5-minute vs
  1-hour cache writes, and OpenAI cache writes (1.25× input from GPT-5.6 on).
  A model without a public price, such as Groq's Llama models ("contact
  sales"), gets no cost rather than a guess.
- **Usage metering, OpenAI format first.** Each provider response's body is
  wrapped as it passes through: bytes reach the client untouched and
  unbuffered while the gateway reads token usage from them (SSE event by event,
  or the JSON body at its end). OpenAI and Groq responses, streamed or not, now
  produce a `usage` log line with tenant, key, provider, model, token counts,
  the provider's own request ID, and whether the stream completed. Anthropic
  is metered too, cache writes split by TTL (5-minute and 1-hour writes cost
  different amounts). Gemini is metered in all three response shapes (JSON,
  SSE, and the default JSON-array stream), with thinking tokens counted as
  output, as they are billed. Verified through the openai, anthropic and
  google-genai SDKs against the mock. A
  client that disconnects mid-stream still gets what was seen recorded; since
  its full usage is unknown, its cost is NULL, never 0. `input_tokens` means
  tokens billed at the input rate: cache reads and writes are counted
  separately.
- **One owner for the request body.** The gateway reads each body once (up to
  `max_request_bytes`, default 32 MiB; `413` beyond that), and forwards the
  client's exact bytes unless it has to rewrite something. For OpenAI-format
  streams (OpenAI, Groq) it now always sets `stream_options.include_usage`,
  since that is the only way the stream reports token usage. **Clients see one
  extra chunk** at the end of every OpenAI-format stream: `choices` is empty and
  `usage` is set. The OpenAI SDKs handle it; a hand-written client that reads
  `choices[0]` on every chunk must skip it.
- **Only metered endpoints are forwarded.** Tenants can reach exactly the
  endpoints whose token usage the gateway can read: OpenAI-format
  `POST /v1/chat/completions`, Anthropic `POST /v1/messages`, and Gemini
  `POST /{v1,v1beta}/models/{model}:generateContent` / `:streamGenerateContent`.
  Anything else (`/v1/responses`, embeddings, batches, `count_tokens`,
  `countTokens`...) gets `403 unmetered_endpoint`, since an endpoint the
  gateway can't meter would get around every future limit. Endpoints join the
  list only with a parser for their usage.
- **Tenant auth is live.** Every provider route needs a gateway key (`gw_…`),
  sent where the client's SDK sends its API key; anything else gets a 401 and
  never reaches the provider. The gateway now needs Postgres and an admin key
  to start: `DATABASE_URL` and `GATEWAY_ADMIN_KEY` (32+ characters, not a
  `gw_` key, no stray whitespace). Both are checked at startup.
- **Admin API.** `POST /admin/tenants` creates a tenant and returns its first
  key, `POST /admin/tenants/{id}/keys` issues another (for rotation), and
  `DELETE /admin/keys/{id}` revokes one. The admin key goes in
  `Authorization: Bearer`, compared in constant time; a key's plaintext appears
  once, in the response that issues it. See [Endpoints](#endpoints).
- **Request IDs.** Every response, including 401s, 404s and proxy errors,
  carries a gateway-generated `X-Request-ID`, which also goes in the request's
  log line. A client-sent `X-Request-ID` is logged as `client_request_id` but
  never reused: nothing makes it unique. OpenAI and Groq send an
  `x-request-id` of their own; clients now see the gateway's in its place (the
  OpenAI SDK's `_request_id` included), and the provider's will be stored as
  `provider_request_id` once usage logging lands.
- **Auth middleware.** `middleware.Auth` reads the gateway key from whichever
  header the client's SDK sends (`Authorization: Bearer`, `X-Api-Key`,
  `X-Goog-Api-Key`), looks it up, and puts the tenant and key on the request
  context. A failed lookup (database down) is a 503, not a 401. Gemini's
  `?key=` is not accepted: a key in a URL ends up in proxy logs.
- **Tenant keys.** `internal/tenant` creates tenants and issues, revokes and
  looks up their `gw_` keys. Only a SHA-256 hash of each key is stored; the
  plaintext is returned once, at issue.
- **Schema reworked before any code uses it.** API keys moved out of `tenants`
  into their own `api_keys` table, so a tenant can hold several keys and rotate
  or revoke one without downtime. `usage_logs` gained `request_id`,
  `provider_request_id`, `api_key_id` and `cache_write_tokens` (Anthropic
  cache writes cost more than plain input).
- **Gemini thinking tokens.** A live check showed Gemini reports thinking in
  `thoughtsTokenCount`, outside `candidatesTokenCount`, though it is billed as
  output; the mock now reproduces it (see [Without API keys](#without-api-keys)).
- **Dev stack.** Postgres 18 runs locally via docker compose (`make db-up`),
  and `make migrate` applies embedded goose migrations creating `tenants`,
  `api_keys` and `usage_logs`. CI runs `go vet` and `go test -race` against a Postgres service
  container on every push. See [Database](#database).
- **Mock provider covers Groq.** A live check showed Groq reports streaming
  usage differently from OpenAI despite the shared format (see
  [Without API keys](#without-api-keys)), so the mock now serves that shape at
  Groq's `/openai` base path and `config.mock.yaml` routes `groq` to it.
- **Multi-provider routing.** A single `upstream` became a map of providers,
  each served at its own `/<name>/` prefix. Ships with OpenAI, Anthropic, Gemini
  and Groq configured.
- **New `internal/provider` package** holding the one thing that genuinely
  differs between providers today: how each expects its API key presented
  (`bearer`, `x-api-key`, `x-goog-api-key`) plus any static headers.
- **The gateway now holds the keys.** Provider keys load from environment
  variables at startup; whatever credential a client sends is stripped before
  forwarding, so a key meant for one provider can never leak to another.
- **Binds to loopback by default**, because of the point above.

### Week 1 deliverables

- [x] `cmd/gateway/main.go` — starts the HTTP server
- [x] `internal/proxy/proxy.go` — reverse proxy with Rewrite, ModifyResponse,
      and ErrorHandler
- [x] `internal/config/config.go` — YAML config (port, host, providers)
- [x] `internal/middleware/logging.go` — structured JSON request logging
- [x] `internal/health/health.go` — `GET /health` → `{"status":"ok"}`
- [x] `Makefile` — build, run, test targets
- [x] Tests: proxy forwarding, health endpoint, logging doesn't break responses
- [x] `go build`, `go vet ./...`, `go test ./...` all pass

The Week 1 checkpoint is "point any OpenAI client at `localhost:8080` and it
proxies transparently." That is verified with `curl` against four local fake
providers: each prefix routes to its own upstream with the prefix stripped, the
right credential header is applied per provider, the client's own credential is
stripped, Groq's base path is preserved, an unknown prefix returns 404 without
contacting anything, and `/health` is served locally.

It has also been exercised against live providers through their official SDKs
(2026-09-27): Groq via the `openai` Python SDK and Gemini via `google-genai`,
one non-streaming and one streaming request each. Each client sent a bogus API
key, so a successful reply shows the gateway substituted its own. Streams
arrived incrementally rather than as one buffered block: Groq delivered 79
chunks and Gemini 5, spread across the generation.

OpenAI and Anthropic have not been tried with live keys. OpenAI uses the same
`bearer` path that Groq exercised; Anthropic's `x-api-key` style is covered by
tests only.

### Week 2 deliverables — API keys and per-tenant tracking ✅

Goal: every request is authenticated, and its token usage and dollar cost are
logged — including streamed responses.

- [x] `cmd/mockprovider` — fake OpenAI, Anthropic and Gemini upstreams, streaming
  and non-streaming, so nothing below costs money to test
- [x] Postgres (via docker-compose) with `goose` migrations: `tenants`,
  `api_keys` and `usage_logs` tables; CI running `go vet` and `go test -race`
- [x] `internal/tenant` — `gw_`-prefixed random keys, stored as SHA-256 for an
  indexed lookup; several per tenant, so keys rotate without downtime
- [x] `internal/middleware/auth.go` — reads the gateway key from the client SDK's
  native credential header, rejects revoked keys, and attaches the tenant to the
  request context
- [x] Request IDs — an `X-Request-ID` per request, returned to the client,
  logged, and stored with its usage row next to the provider's own ID
- [x] A `format` field per provider (`openai`, `anthropic`, `gemini`)
- [x] A usage parser for each format (OpenAI/Groq, Anthropic, Gemini)
- [x] `internal/usage` — reads token counts from the response as it streams past,
  without buffering it, and writes `usage_logs` rows in async batches
- [x] `internal/pricing` — per-model prices in integer micro-dollars
- [x] Admin routes behind a separate admin key: `POST /admin/tenants`,
  issuing and revoking keys, `GET /admin/tenants/{id}/usage`

This is the first week the gateway stops being dependency-free: it adds
Postgres and the `pgx` driver. Auth middleware runs *before* the proxy, so a
request with a bad key never reaches the provider. Usage is counted by
wrapping the response body in `ModifyResponse`, so streaming responses are
metered too; otherwise `stream: true` would bypass every future limit.

### Week 3 deliverables — token-aware rate limiting ✅

Goal: per-tenant limits in tokens per minute, enforced atomically in Redis.

- [x] Redis in the dev stack and CI — `redis:8-alpine` in docker-compose, bound
  to loopback with no volume (the buckets are disposable) and started by
  `make db-up`, and a service container in CI
- [x] `internal/ratelimit` — one token bucket per tenant in Redis, run as two
  Lua scripts (`Allow`, `Adjust`) on Redis's own clock, so concurrent requests
  can't spend the same tokens and replicas agree on the time. A request bigger
  than a whole minute's limit gets in on a full bucket and leaves the tenant in
  debt; a bucket's key expires when it would be full again
- [x] Tenant limits valid by construction — migration `00002` puts
  `CHECK (> 0)` on `rate_limit_tokens_per_min` and `default_max_tokens`
- [x] The request estimate, computed where the body is read — one token per 4
  bytes with media payloads left out, plus 1,600 per image, audio, document or
  file part; the output cap the client set is read, never rewritten
- [x] `internal/middleware/ratelimit.go` — charges the estimate plus the output
  the request allows before the provider is called; over the limit is a
  `429 rate_limited` with `Retry-After`; when Redis fails, the request is
  admitted with a warning (fail open)
- [x] Settlement from the usage callback — when a response ends, the charge is
  corrected to the tokens the provider reported: the unused part comes back,
  and an overshoot is charged
- [x] Tests: the checkpoint through the real router, against a real Redis and
  an upstream that holds its responses open — a 100,000 tokens/min tenant is
  refused at the threshold and recovers after refill, and 50 simultaneous
  requests of 10,000 tokens admit exactly 10

This is the first week the gateway depends on Redis too: it adds `go-redis`,
and `REDIS_URL` joins `DATABASE_URL` as a setting the gateway won't start
without. `RateLimit` runs *after* `ReadBody`, which supplies the estimate, and
*before* the proxy, so a refused request never reaches the provider. It wraps a
route's fallback (Ollama → Groq), so a request is charged once and settled
once, by the provider that answered. Settlement rides on the usage callback
that already meters each response, so a streamed response is corrected when its
stream ends.

The Week 3 checkpoint is "a 100K tokens/min tenant is blocked at the right
threshold, recovers after refill, and concurrent requests can't over-admit."
Two tests in `cmd/gateway` check it through the real router, against a real
Redis, in front of a provider that holds each request until the test lets it
go, so no admitted request gives tokens back early. On a 100,000 tokens/min
tenant, three 30,000-token requests get in and a fourth gets `429` with
`Retry-After: 12`. Once the three end, each having used 1,000 tokens, the
29,000 apiece they didn't use come back, and a fifth gets in at once. After a
drain, a 1,700-token request is refused, then gets in 1.5 s later on the
refill alone (100,000 a minute is about 1,667 a second, so 1.02 s would do;
the rest rides out a step back of Redis's clock, which Colima's VM makes about
every 20 s). Of 50 requests of 10,000 tokens sent at once, exactly 10 reach
the provider and 40 get `429`. Both tests need `REDIS_URL` and skip without it.

The gateway has also been exercised against live providers with rate limiting
on (2026-10-12, built from `a36fa30`): Groq `openai/gpt-oss-120b` through the
`openai` Python SDK (3.29) and Gemini `gemini-3.8-flash` through `google-genai`
(2.29) still answer, streamed and not. Gemini answered some calls with its own
`503` "high demand", direct calls without the gateway included, so that is its
capacity, not the gateway's doing. A 503 carries no tokens, so its whole charge
came back.

With the tenant's limit lowered to 1,000 a minute in SQL, the first request got
in on the full bucket and was charged 1,027: an input estimate of 27 tokens plus
an output allowance clamped at the limit. It was settled to the 117 tokens it
really used, and the bucket read 888, where the charge alone would have left it
at −27. The next request, sent with `max_retries=0`, got `429 rate_limited` with
`Retry-After: 7` (⌈(1,000 − 888) × 60 / 1,000⌉) and `estimated_tokens` 1,027.
With the SDK's default retries it waited the 7 s and got 200, 7.42 s in all. The
refused requests left no usage row.

### Week 4 deliverables — response caching ✅

Goal: a repeated request is answered from Redis, without calling the provider or
charging the tenant.

- [x] The exact-match key — `cache.Key` is a SHA-256 over the tenant, the
  provider, the escaped path, the query (without `key=`), the
  `anthropic-version` and `anthropic-beta` headers, and the body as canonical
  JSON, its parts kept apart so no two requests can run together into one key.
  Whitespace and key order don't matter, and a number keeps its spelling, so `1`
  and `1.0` get different keys: that can cost a hit, never serve a wrong one. A
  request that can't be keyed safely (a body that isn't one JSON value, invalid
  UTF-8, a query that doesn't parse, a body over 4 MiB) bypasses the cache
- [x] Per-tenant scoping — the tenant is in the hash and in the entry's Redis
  key (`cache:{<tenant_id>}:<hex>`), so one tenant never reads another's
  entries. A tenant's API keys share its entries
- [x] Eligibility and the header — a non-streaming request is cached when it
  sets temperature to exactly 0 or sends `X-Gateway-Cache: on`. `off` opts a
  request out, and any other value gets `400 invalid_request`. The proxy removes
  every `X-Gateway-*` request header before forwarding, so none reaches a
  provider
- [x] The TTL — `cache.ttl` in `config.yaml`, in seconds (default 3,600, at
  least 1). Each entry is written with its TTL in one `SET … EX`, so none is
  ever stored without an expiry
- [x] The store rules — only a complete 2xx answer from the route's own provider
  is stored, never a stream, a fallback's answer, a response with a
  `Content-Encoding`, or a body over 1 MiB. It is stored before the response
  finishes. An entry holds the status, Content-Type, model and body, and no
  provider headers
- [x] Before the rate limit, with hit rows at cost 0 — `middleware.Cache` runs
  between `ReadBody` and `RateLimit`, so a hit is never rate limited or charged.
  It leaves a usage row of its own, marked `cached`, with no tokens and cost 0
- [x] `X-Gateway-Cache: hit|miss|bypass` on every response that reaches the
  cache, set as the headers go out, so it replaces a provider's own value and
  survives a 1xx
- [x] Tests — the key; the store, against a real Redis; the middleware and the
  routes, over a fake cache; and the checkpoint, through the real router against
  a real Postgres and Redis
- [ ] Semantic cache (stretch) — deferred: it needs pgvector, an embedding model
  and a measured false-positive rate before it may answer anyone, and a wrong
  semantic hit is a wrong answer, which the exact cache never gives

The cache runs after `ReadBody` and before `RateLimit`. It needs the tenant from
Auth and the body from `ReadBody`, and a hit must never touch the rate limit's
bucket or, from Week 5, the budget. The order on a provider route is Auth →
StripPrefix → RequireMetered → ReadBody → Cache → RateLimit → (Fallback) →
proxy. For the same reason it is middleware rather than a hook in
`ModifyResponse`, which runs inside the proxy, after `RateLimit` has charged the
request. Like the rate limiter, it comes in two pieces: `internal/cache` owns
the key and Redis, and `middleware.Cache` is the HTTP side, over a small
interface (`Get` and `Set`), so routing tests need no Redis. A miss learns how
its response ended from the usage callback (`middleware.ReportUsage`) that
already meters it, and is stored once the handler returns normally, so a
response the proxy aborts is never stored.

The Week 4 checkpoint is "the same request twice → one provider call, the second
response in under 5 ms, the tenant charged once." Three tests in `cmd/gateway`
check it through the real router, built from the parts `main` uses: the tenant
store and usage writer on Postgres, and the cache and the rate limiter on Redis,
on the client `openRedis` builds. They sit in front of the mock provider,
wrapped to count the requests that reach it, and need `DATABASE_URL` and
`REDIS_URL`: without them they skip.

`TestCacheCheckpoint` sends the same temperature-0 request twice on each mock
route (openai, groq, anthropic, gemini). The provider gets one request per
route, and the second response is a `hit` with the first's status, Content-Type
and body, byte for byte. A counting wrapper over the real limiter sees one
`Allow` per route, and `usage_logs` holds 8 rows: each route's miss, priced at
the price in force when the test runs, and its hit, marked `cached`, with no
tokens and cost 0. `TestCacheHitLatency` times 20 hits through the router, each
around `ServeHTTP`, and their median must be under 5 ms; under `-race` it
measured 0.67–0.72 ms across 15 runs at `-cpu 1,2,4`.
`TestCacheKeepsTenantsApart` has two tenants send the same request, taking
turns. Each misses once and then hits its own entry, never the other's: the
upstream numbers its answers, so a tenant served the other's entry would get the
other's number, and Redis holds exactly one entry under each tenant's
`cache:{<tenant ID>}:` prefix.

Any Warn fails the tests, with a message saying why: the cache and the limiter
fail open after 100 ms, so a slow Redis would otherwise look like a cache that
never hits. They passed 30 of 30 runs with `-race`
across `-cpu 1,2,4`, and they fail when the middleware skips the lookup, so
every request misses: each route's provider then gets 2 requests and the limiter
2 `Allow` calls, the second row is priced and not cached, no repeat is a hit,
and each tenant gets a fresh answer (3 and 4).

The gateway has also been exercised against live providers with the response
cache on (2026-10-17, built from `fe497d7`, which holds all of the cache code):
Groq `openai/gpt-oss-120b` through the `openai` Python SDK (3.34) and Gemini
`gemini-3.8-flash` through `google-genai` (2.31). On both, the same
temperature-0 request sent twice got `miss`, then `hit` with the same answer:
Groq's hit body was byte-identical to the miss's (671 bytes), and Gemini's
parsed to the same response. `X-Gateway-Cache: on` at the default temperature
got `miss`, then `hit`. `off` on a temperature-0 request already stored got
`bypass` both times: the provider answered both, and both were priced. A
temperature-0 stream got `bypass` both times, priced and marked streamed. On
Groq, `X-Gateway-Cache: yes` got `400 invalid_request`, with no cache header and
no usage row. The usage rows for those requests came to 16, 8 per provider: 12
priced, 4 of them streams, and 4 hits, each `cached`, with no tokens, cost 0 and
no provider request ID. A hand-written curl body for the same Groq request hit
the entry the SDK had stored.

Hit latency through the real server, by curl, had a median of 1.14 ms on Groq
(0.96–2.06 ms) and 1.11 ms on Gemini (1.02–1.82 ms), over 20 hits each. For
comparison, calls the provider answered took 0.31–0.45 s on Groq and 2.6–7.8 s
on Gemini (non-streamed misses and bypasses, from `latency_ms`). Gemini answered
every call this time, with no 503s, and the gateway logged no Warn.

## Quick start

Start Postgres and Redis and create the schema (see [Database](#database)):

```sh
make db-up migrate
```

Export a key for each provider in `config.yaml`, plus an admin key of your
own, then run:

```sh
export OPENAI_API_KEY=sk-...
export ANTHROPIC_API_KEY=sk-ant-...
export GEMINI_API_KEY=...
export GROQ_API_KEY=gsk_...
export GATEWAY_ADMIN_KEY=$(openssl rand -hex 32)

make run
```

A provider key that isn't set, a missing or weak admin key, or an unreachable
database or Redis is a startup error, not a surprise on the first request. To
run with fewer providers, delete the ones you don't need from `config.yaml`.

Create a tenant with the admin key. The response holds the tenant's first
gateway key (`key.plaintext`); it is shown once, so keep it:

```sh
curl -X POST http://127.0.0.1:8080/admin/tenants \
  -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" \
  -d '{"name":"my-app"}'
```

Then point your client at `/<provider>` and use that provider's own API, with
the **gateway key** in place of the provider's. The gateway checks it, strips
it, and attaches the provider key it holds:

```sh
curl http://127.0.0.1:8080/openai/v1/chat/completions \
  -H "Authorization: Bearer gw_..." \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}'

curl http://127.0.0.1:8080/anthropic/v1/messages \
  -H "X-Api-Key: gw_..." \
  -H "Content-Type: application/json" \
  -d '{"model":"claude-sonnet-4","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}'
```

With an SDK, override the base URL and pass the gateway key as the API key:

```python
from openai import OpenAI

client = OpenAI(base_url="http://127.0.0.1:8080/openai/v1", api_key="gw_...")
```

### Without API keys

`cmd/mockprovider` serves fake OpenAI, Groq, Anthropic and Gemini APIs on one port,
and `config.mock.yaml` points the gateway's providers at it. Nothing leaves the
machine and nothing costs money:

```sh
make db-up migrate   # once: tenant keys live in Postgres
make mock            # terminal 1: mock provider on 127.0.0.1:9090
make run-mock        # terminal 2: gateway on 127.0.0.1:8080
```

`run-mock` sets a fixed admin key, `mock-admin-key-for-local-testing-only`, so
creating a tenant needs no setup:

```sh
curl -X POST http://127.0.0.1:8080/admin/tenants \
  -H "Authorization: Bearer mock-admin-key-for-local-testing-only" \
  -d '{"name":"dev"}'
```

Client base URLs are the same as against the real providers. Every reply is the
same fixed text with the same usage numbers (20 prompt tokens, 5 of them
cached, 10 output), so tests can assert exact counts. The mock reproduces the
details a usage parser has to get right:

- **OpenAI** streams report usage only when the request sets
  `stream_options.include_usage`.
- **Groq** puts usage on the finish chunk (top-level `usage` and again in
  `x_groq.usage`) even without `include_usage`; with it, OpenAI's extra chunk
  repeats the same numbers. A parser must keep the last usage, not sum them.
- **Anthropic** puts input usage in `message_start` with a placeholder output
  count; the real output count arrives in `message_delta`. `input_tokens`
  excludes cache reads (it reports 15, plus 5 in `cache_read_input_tokens`).
- **Gemini** sends `usageMetadata` on every chunk as a running total, not a
  delta. It streams as SSE with `?alt=sse` (what the SDKs use) and as one JSON
  array without it. Thinking is reported in `thoughtsTokenCount` (7), outside
  `candidatesTokenCount` but billed as output, so Gemini's output total is
  10 + 7 = 17. A parser that reads only `candidatesTokenCount` undercounts.

It also checks each provider's credential header is present (and Anthropic's
`anthropic-version`), so a provider configured with the wrong `auth` style
gets a 401, as it would from the real API.

The handlers live in `internal/mockprovider`, so Go tests can run them
in-process with `httptest.NewServer(mockprovider.Handler())`.

### Database

Postgres and Redis run in Docker (any runtime works; on macOS, `brew install
colima docker docker-compose` then `colima start` is enough):

```sh
make db-up      # Postgres 18 on 127.0.0.1:5432 and Redis 8 on 127.0.0.1:6379, waits until healthy
make migrate    # apply pending migrations
make db-down    # stop both; Postgres data survives in the llm-gateway_pgdata volume
```

Migrations live in `internal/db/migrations` and are embedded into the binary,
so a build always carries the schema its code expects. `make migrate` targets
the compose database; pass `DATABASE_URL=postgres://...` to use another.

Redis holds the rate-limit buckets (Week 3) and the cached responses (Week 4).
It has no volume, because both are disposable: losing the buckets only refills
every tenant, and losing a cached response only makes the next identical request
a miss. It has no password either, so binding to loopback is what keeps it
private, which matters more now that cached answers sit in it unencrypted until
their TTL ends.

The gateway finds it through `REDIS_URL`: `make run` and `make run-mock` point
it at this one (pass `REDIS_URL=redis://...` to use another). The gateway pings
it at startup, so a missing or unreachable Redis stops it there instead of
leaving every tenant unlimited.

The Postgres tests only run when `DATABASE_URL` is set, and each creates and
drops its own throwaway database, so they never touch dev data. The Redis
tests likewise need `REDIS_URL`; they share one Redis, each under its own
random tenant ID:

```sh
DATABASE_URL='postgres://gateway:gateway@127.0.0.1:5432/gateway?sslmode=disable' \
REDIS_URL='redis://127.0.0.1:6379/0' go test -race ./...
```

Without them, `go test ./...` skips those tests and needs nothing running.

## Configuration

`config.yaml`:

```yaml
port: 8080          # port the gateway listens on
host: 127.0.0.1     # bind address — loopback by default; see the note at the top

cache:
  ttl: 3600         # seconds to serve a stored response

providers:
  openai:
    url: https://api.openai.com
    timeout: 600                 # seconds to wait for response headers
    key: ${OPENAI_API_KEY}       # environment reference, never a literal
    auth: bearer                 # bearer | x-api-key | x-goog-api-key | none
    format: openai               # openai | anthropic | gemini

  anthropic:
    url: https://api.anthropic.com
    key: ${ANTHROPIC_API_KEY}
    auth: x-api-key
    format: anthropic
    headers:                     # static headers this provider requires
      anthropic-version: "2023-06-01"
```

`port`, `host`, `max_request_bytes` (default 32 MiB) and each provider's
`timeout` are optional, and so are the provider settings `free`,
`max_concurrency`, `queue_timeout`, `health_path` and `fallback`, described in
[Running with Ollama](#running-with-ollama). `url`, `auth` and `format` are
required per provider, and so is `key` unless `auth` is `none`.

`format` is the shape of the provider's API, which decides how the gateway
reads token usage from its responses. It names a wire shape, not a company:
Groq and other OpenAI-compatible providers are `openai`.

`auth: none` is for an upstream that takes no key, such as a local Ollama. No
credential is sent upstream, and `key` must be left out: it would be silently
ignored, so it is a startup error. Whatever credential the client sends is still
stripped, and clients still need a gateway key.

**Keys are always environment references.** `config.yaml` is committed to git;
a literal key there would be published, so `key` must be exactly one `${VAR}`
and anything else is a startup error. That includes `${VAR:-default}`: a default
would be a literal key committed to git. Unknown fields are startup errors too,
so a typo can't silently fall back to a default.

A few other fields may be environment references, so a machine can override
them without editing the file: `url`, `timeout`, `max_concurrency`,
`queue_timeout` and `fallback.enabled`. Each takes a whole-value `${VAR}` or
`${VAR:-default}`. As in the shell, an unset or empty variable uses the
default; with no default, it is a startup error naming the variable and the
line. Expansion happens only on these fields and on `key`, so other values
containing `$` are left alone.

Point `GATEWAY_CONFIG` at a different file to use one:

```sh
GATEWAY_CONFIG=config.local.yaml make run
```

`timeout` bounds how long we wait for a provider to *start* replying, not total
request time. Streaming completions hold the connection open for minutes, and a
total-request cap would cut them off mid-generation. A non-streaming reply
sends no headers until the whole generation is done, which is why the default
is 600 seconds, the same as the official SDKs. A timeout returns `504`; an
unreachable provider returns `502`; a request that waits longer than
`queue_timeout` for a free slot returns `503` with error type `provider_busy`
and `Retry-After: 1`, and that wait doesn't count against `timeout`.

`cache.ttl` is how long, in seconds, the gateway serves a stored response
before it asks the provider again. The default is 3,600 and the minimum is 1,
and the whole `cache:` block may be left out. A response is stored for a
non-streaming request with temperature 0 or the header `X-Gateway-Cache: on`;
`X-Gateway-Cache: off` opts a request out. A typo under `cache:`, or a `ttl`
below 1, is a startup error. [The cache contract](#the-cache-contract) has the
details.

### Adding a provider

Anything OpenAI-compatible is pure config. Groq is in the default file as proof
— same `auth: bearer` and `format: openai` as OpenAI, different base URL:

```yaml
  groq:
    url: https://api.groq.com/openai   # base path is preserved
    key: ${GROQ_API_KEY}
    auth: bearer
    format: openai
```

Providers needing request signing rather than a static header — AWS Bedrock's
SigV4, Vertex AI's OAuth — can't be expressed this way and would need code.

## Running with Ollama

Ollama runs models on your own machine: no API key and no per-token bill. The
default `config.yaml` already has an `ollama` provider, so once Ollama is
installed the gateway serves it at `/ollama/`, metered like any other provider.

```sh
brew install ollama
brew services start ollama   # runs in the background, restarts at login
ollama pull qwen3.5:9b       # 7.6 GB on disk
```

Ollama listens on `127.0.0.1:11434`, and `brew services stop ollama` stops it.
The provider block:

```yaml
  ollama:
    url: ${OLLAMA_BASE_URL:-http://127.0.0.1:11434}
    timeout: ${OLLAMA_TIMEOUT_SECONDS:-120}
    auth: none
    format: openai
    # Every usage row costs exactly 0; its tokens are still recorded.
    free: true
    max_concurrency: ${OLLAMA_MAX_CONCURRENCY:-1}
    queue_timeout: ${OLLAMA_QUEUE_TIMEOUT_SECONDS:-30}
    health_path: /api/tags
    fallback:
      enabled: ${OLLAMA_FALLBACK_ENABLED:-false}
      provider: groq
      models:
        "qwen3.5:9b": qwen/qwen3.8-27b
```

`auth: none` because Ollama takes no key (see [Configuration](#configuration));
`free: true` because it costs nothing, though its tokens still belong in the
usage rows. The environment variables are all optional, and
[`.env.example`](.env.example) lists them:

| Variable                       | Default                  | Meaning                                                           |
| ------------------------------ | ------------------------ | ----------------------------------------------------------------- |
| `OLLAMA_BASE_URL`              | `http://127.0.0.1:11434` | Where Ollama listens.                                             |
| `OLLAMA_TIMEOUT_SECONDS`       | `120`                    | Seconds to wait for Ollama's response headers.                    |
| `OLLAMA_MAX_CONCURRENCY`       | `1`                      | Requests sent to Ollama at once; `0` is unlimited.                |
| `OLLAMA_QUEUE_TIMEOUT_SECONDS` | `30`                     | Seconds a request waits for a free slot before it gets a `503`.   |
| `OLLAMA_FALLBACK_ENABLED`      | `false`                  | `true` retries failed requests on Groq, which costs real money.   |

Call it like any provider, with a gateway key:

```sh
curl http://127.0.0.1:8080/ollama/v1/chat/completions \
  -H "Authorization: Bearer gw_..." \
  -H "Content-Type: application/json" \
  -d '{"model":"qwen3.5:9b","reasoning_effort":"none","messages":[{"role":"user","content":"hi"}]}'
```

With the OpenAI SDK, set `base_url` to `http://127.0.0.1:8080/ollama/v1`, as in
[Quick start](#quick-start).

**Thinking.** qwen3.5 thinks before it answers by default. That is slower, and
the thinking counts as output tokens: a one-word answer took 362 completion
tokens with thinking and 1 without. `"reasoning_effort": "none"` in the request
turns it off. With thinking on, the thinking text arrives in the message's
`reasoning` field.

**Cold start.** A request that finds the model unloaded loads it first, which
took about 30 s on a 16 GB Mac. Ollama unloads an idle model after 5 minutes by
default (its own `OLLAMA_KEEP_ALIVE`). A non-streaming reply sends no headers
until it is done, so it has to finish within `timeout` (120 s by default), the
load included.

**Usage.** Ollama's streams carry token usage only when the request sets
`stream_options.include_usage`, which the gateway always does, as for OpenAI and
Groq. `cached_tokens` is part of `prompt_tokens`, as at OpenAI.

**Concurrency.** `max_concurrency` caps how many requests go to a provider at
once; `0`, the default when the field is left out, means unlimited. The
`ollama` block allows 1: a local model has one machine to run on, so extra
requests wait their turn instead of competing. Waiting requests are served in
arrival order, and one that waits longer than `queue_timeout` seconds (default
30) gets `503` with `{"error":{"type":"provider_busy",...}}` and
`Retry-After: 1`. The wait doesn't count against `timeout`.
[`gateway_concurrency_wait_seconds`](#metrics) shows how long requests wait.

**Health.** Every 15 s the gateway sends a `GET` to `url` plus `health_path`
(`/api/tags`, Ollama's list of installed models), with a 5 s timeout. The result
appears in [`/health`](#endpoints) and as `gateway_provider_up`.

**Fallback.** Off by default: `OLLAMA_FALLBACK_ENABLED=true` turns it on. A
fallback **spends real money** at the target provider (Groq here), which is why
it is opt-in. A non-streaming request whose model is listed under
`fallback.models` is retried on `fallback.provider` when Ollama is unreachable,
times out or answers 5xx; a streaming request is not. Only `"model"` is
rewritten, to the name on the right (`qwen3.5:9b` becomes `qwen/qwen3.8-27b`).
It never happens on a 4xx, a client disconnect or a queue timeout. Responses on
a route with fallback enabled say who answered:

- `X-Gateway-Provider: <provider that answered>`
- `X-Gateway-Fallback: true|false`

Each attempt leaves its own usage row under the same request ID: the failed
Ollama attempt costs 0, and the Groq attempt is priced normally.

**Docker.** The gateway has no compose service yet (Week 7). Once it runs in a
container, set `OLLAMA_BASE_URL=http://host.docker.internal:11434` and add
`extra_hosts: ["host.docker.internal:host-gateway"]` to its service. Under
Colima (see [Database](#database)), containers can't reach services bound to
the Mac's `127.0.0.1`, so Ollama has to listen beyond loopback (its own
`OLLAMA_HOST` setting). Binding `0.0.0.0` exposes Ollama's unauthenticated API
to the network: bind it to the Colima bridge address or firewall it.

## Endpoints

| Path                              | Behavior                                                                                  |
| --------------------------------- | ----------------------------------------------------------------------------------------- |
| `GET /health`                     | Always `200`: `{"status":"ok"}`, plus provider checks. Handled locally, no key needed.    |
| `GET /metrics`                    | Prometheus text format. Handled locally, no key needed. See [Metrics](#metrics).          |
| `/<provider>/...`                 | Needs a gateway key (`401` otherwise) and a metered endpoint (`403` otherwise, see below), within the tenant's tokens-per-minute limit (`429` with `Retry-After` otherwise, see [The 429 contract](#the-429-contract)). Prefix stripped, forwarded with the provider's key. An eligible repeat may be answered from the cache instead, without reaching the limit or the provider (see [The cache contract](#the-cache-contract)). |
| `POST /admin/tenants`             | Admin key. `{"name": "..."}` → `201` with the tenant and its first key.                   |
| `POST /admin/tenants/{id}/keys`   | Admin key. Issues another key for the tenant → `201`.                                     |
| `DELETE /admin/keys/{id}`         | Admin key. Revokes the key → `204`; the row stays so past usage still attributes to it.   |
| `GET /admin/tenants/{id}/usage`   | Admin key. `?from=&to=` (RFC 3339, default this UTC month) → totals and per-model usage.  |
| anything else                     | `404` with a JSON body listing the configured providers.                                  |

Metered endpoints, the only ones forwarded:

| Format      | Endpoints                                                                          |
| ----------- | ---------------------------------------------------------------------------------- |
| `openai`    | `POST /v1/chat/completions`                                                        |
| `anthropic` | `POST /v1/messages`                                                                |
| `gemini`    | `POST /v1/models/{model}:generateContent`, `:streamGenerateContent` (also `/v1beta/`) |

Every response carries an `X-Request-ID`. Key plaintext appears only in the
`201` that issues it, with `Cache-Control: no-store`. Any `/admin/` request
without the admin key gets `401`, whatever the path or method.

There is deliberately **no default provider**. A request must name one, so a
typo in a base URL can never silently send traffic — and spend — somewhere
unintended.

`/health` answers "is this process alive and serving?", which is what a load
balancer or container orchestrator needs. It always answers `200` and never
calls a provider itself — otherwise one provider's outage would take the gateway
out of rotation even though it is working fine.

What it adds is the last result of the gateway's background check of each
provider that has a `health_path`:

```json
{"status":"ok","providers":{"ollama":{"status":"up","checked_at":"2026-10-10T16:30:00Z"}}}
```

A provider's `status` is `up` or `down`, or `unknown` before its first check has
run; `checked_at` is an RFC 3339 time. A `down` provider is information for
whoever reads the body or `gateway_provider_up`; it never changes the `200`.
With no checked provider the body is exactly `{"status":"ok"}`.

### The 429 contract

Before a request is forwarded, the gateway charges its tenant's
tokens-per-minute bucket an estimate: the input estimate plus the output the
request allows, which is its own cap (`max_tokens`, `max_completion_tokens` or
Gemini's `maxOutputTokens`) or, when it set none, the tenant's
`default_max_tokens` for each choice. The bucket belongs to the tenant, so all
of its keys, providers and models share it. When it can't cover the charge, the
gateway answers `429` itself, with `Retry-After: 12` and a body like this one (a
30,000-token request with 10,000 left in a 100,000-per-minute bucket). The body
has this shape for every provider, and the provider is never called:

```json
{"error":{"estimated_tokens":30000,"limit_tokens_per_min":100000,"message":"this tenant is over the gateway's token rate limit; retry in 12 s","retry_after_seconds":12,"type":"rate_limited"}}
```

- **`Retry-After`** is in whole seconds, rounded up, and at least 1: how long
  until the bucket could admit this request if the tenant sent nothing else
  meanwhile. It is never more than 60 unless the tenant is in debt. Debt comes
  from a request bigger than a whole minute's limit, which gets in on a full
  bucket and is charged in full, and from settlement charging a request that
  used more tokens than it was charged up front.
- **Telling it from a provider's 429.** The gateway's 429 has `error.type`
  `rate_limited`, a message that names the gateway's token rate limit, and
  `retry_after_seconds`, `limit_tokens_per_min` and `estimated_tokens` beside
  them. A provider's own 429 passes through with its own body.
- **SDK retries.** The openai Python SDK treats a 429 as retryable: it waits
  for `Retry-After` and retries twice by default, so a tenant a little over its
  limit sees a short wait instead of an error. With `max_retries=0` the 429
  surfaces as a `RateLimitError`, with `e.type == "rate_limited"` and the wait
  in `e.response.headers["retry-after"]`. (openai 3.29 doesn't retry at all
  when `Retry-After` is above two minutes, which only a tenant in debt gets.)
- **The estimate is corrected afterwards.** `estimated_tokens` is a charge made
  up front, not a bill. When the response ends, the gateway corrects the bucket
  to the tokens the provider reported: the unused part comes back, and a request
  that used more pays the difference, so the bucket soon reflects real usage. A
  response cut short (the client left, the provider never answered) is not
  refunded, since its real cost is unknown. A request that sets a tighter
  `max_tokens` is cheaper to admit.

A refused request leaves no usage row. The gateway logs it as one
`request rate limited` line carrying the `X-Request-ID` the client got.

### The cache contract

The gateway keeps some responses in Redis and answers an identical request from
there, without calling the provider. Entries belong to a tenant: no other tenant
can read one.

- **Which requests are cached.** A request is eligible when it isn't a stream
  and either sets `temperature` to exactly 0 (at the top level for OpenAI-format
  and Anthropic requests, in `generationConfig` for Gemini) or sends
  `X-Gateway-Cache: on`, unless it sends `off`. Everything else is a bypass: it
  goes straight on, with no lookup and no store. That covers a stream
  (`"stream": true`, or Gemini's `streamGenerateContent`), whether or not it
  sends `on`; a request with no temperature, or one other than 0, and no `on`; a
  request with `off`; a request the cache can't key safely (a body that isn't
  exactly one JSON value or isn't valid UTF-8, or a query string that doesn't
  parse); and a request body over 4 MiB.
- **`X-Gateway-Cache` in a request** is `on` or `off`, in any case, sent once.
  `on` makes a non-streaming request eligible at any temperature. `off` keeps it
  out of the cache, whatever its temperature. Any other value, or the header
  sent twice, gets `400 invalid_request` and goes no further, so a typo like
  `true` can't silently do nothing. The header never reaches a provider: the
  gateway removes it, like every `X-Gateway-*` request header.
- **`X-Gateway-Cache` in a response** is `hit`, `miss` or `bypass`, on every
  response that reaches the cache, in place of any value a provider sent. It
  says what the cache did, not how the request turned out. After a lookup that
  finds nothing, or fails, every response says `miss`, a `429` from the rate
  limit or a `503 provider_busy` included. A bypassed request says `bypass`
  whatever comes back. A request refused before the cache (`401`, `403`, `413`,
  or `400` for a body that can't be read) or by it (`400` for a bad
  `X-Gateway-Cache`) carries no cache header.
- **What counts as the same request.** The tenant, the provider route, the
  path, the query string (without `key=`), the `anthropic-version` and
  `anthropic-beta` headers, and the body; no other header counts. Whitespace,
  the order of an object's keys at any depth, and how a string is escaped don't
  matter. A number counts as written, so `1` and `1.0` are different requests:
  that can cost a hit, but never serves a wrong answer. All of a tenant's API
  keys share its entries.
- **What is stored.** A miss is stored when its response is a complete 2xx from
  the route's own provider. Never stored: a stream, a fallback's answer, a
  response with a `Content-Encoding`, or a body over 1 MiB. An entry holds the
  status, the Content-Type and the body, exactly as the client got them, and no
  provider headers.
- **A hit** replays the status, the Content-Type and the body, with
  `X-Gateway-Cache: hit`. It carries its own `X-Request-ID` and none of the
  provider's headers, whose request IDs, rate-limit headers and cookies would be
  stale or wrong. On a route with a fallback enabled it adds
  `X-Gateway-Provider` (the route's provider) and `X-Gateway-Fallback: false`,
  as every other response there does.
- **What a hit costs, and how long it lasts.** A hit never reaches the rate
  limit, so it is served even when the tenant's bucket is empty. It leaves a
  usage row with `cached` true, no tokens and cost 0, which the usage report
  counts as a request. Entries live for `cache.ttl` (3,600 seconds by default)
  from the moment they are stored, and a hit doesn't extend that. There is no
  way to flush a tenant's entries, and a hit carries no `Age` header.
- **Opting out.** Send `X-Gateway-Cache: off`. A "regenerate" button that
  resends a temperature-0 request would otherwise get the same answer back for
  the whole TTL, and so would an eval that runs one prompt several times.
- **Concurrent requests.** There is no request coalescing. Two identical
  requests that miss at once both reach the provider and both are charged, and
  the later store wins.
- **When Redis is down,** the cache fails open. A lookup that fails, or takes
  more than 100 ms, makes the request a `miss` that goes to the provider as it
  would without a cache; the gateway logs a Warn, and the response isn't stored.
  A store that fails is a Warn too, and changes nothing the client sees.
- **Not prompt caching.** This is the gateway's own response cache, and a hit
  never reaches the provider. The providers' prompt caching is another thing: it
  still runs the model, bills the input tokens it has seen before at a discount,
  and is metered as before (`cached_input_tokens` and `cache_write_tokens`).

## Make targets

| Command         | What it does                           |
| --------------- | -------------------------------------- |
| `make build`    | Compile to `./bin/gateway`             |
| `make run`      | Build, then run                        |
| `make mock`     | Run the mock provider on :9090         |
| `make run-mock` | Run the gateway against the mock       |
| `make db-up`    | Start local Postgres and Redis         |
| `make db-down`  | Stop local Postgres and Redis          |
| `make migrate`  | Apply pending database migrations      |
| `make test`     | Run all tests (needs `make db-up`)     |
| `make vet`      | Run `go vet` (catches suspicious code) |
| `make fmt`      | Format all source files                |
| `make tidy`     | Sync `go.mod` / `go.sum`               |
| `make clean`    | Remove `./bin`                         |
| `make all`      | fmt, vet, test, build                  |

## Layout

```
cmd/gateway/main.go       entry point: loads config, builds routes, serves
internal/config/          YAML config loading and validation
internal/provider/        per-provider auth styles and header handling
internal/proxy/           the httputil.ReverseProxy, its hooks and the fallback
internal/middleware/      request IDs, request logging, tenant key auth, response cache, rate limiting
internal/health/          GET /health
internal/concurrency/     per-provider request limit and its queue
internal/metrics/         Prometheus metrics
internal/mockprovider/    fake OpenAI/Groq/Anthropic/Gemini APIs for testing
cmd/mockprovider/main.go  serves the mock on 127.0.0.1:9090
internal/db/              Postgres schema: embedded goose migrations
internal/db/dbtest/       throwaway Postgres database for tests
internal/tenant/          tenants and API keys: issue, revoke, look up
internal/ratelimit/       per-tenant token bucket in Redis (Lua scripts)
internal/cache/           exact-match response cache: the request key, entries in Redis
internal/redistest/       Redis client for tests
internal/admin/           admin API: create tenants, issue and revoke keys
internal/apierror/        the one JSON error shape every response uses
cmd/migrate/main.go       applies migrations to DATABASE_URL
deployments/              docker compose dev stack (Postgres, Redis)
.github/workflows/ci.yml  vet + race-enabled tests against Postgres and Redis
```

Routing costs no custom code: each provider is registered on `ServeMux` as
`/<name>/` wrapped in `http.StripPrefix`, both standard library. Routes are
built by looping over the config map at startup, so nothing is hardcoded.

Code under `internal/` can only be imported from inside this module — the Go
toolchain enforces that, which makes it the right home for implementation
details you don't want other projects depending on.

## Where the next features hook in

`ModifyResponse` in [internal/proxy/proxy.go](internal/proxy/proxy.go) runs
after the provider responds and before the response is copied back to the
client. That is where usage accounting reads token counts.
Returning an error from it causes `ErrorHandler` to run instead of the response
being forwarded — that's the mechanism for rejecting a request over budget.

Rate limiting runs *before* the proxy, as middleware after `ReadBody` in
`main.go`, so a refused request never reaches the provider. It settles on the
way back too: the usage callback, which fires as each response ends (or fails),
reports that request's tokens to it (`middleware.ReportUsage`), and it corrects
the bucket once the request is over. Budget checks belong next to it.

The response cache runs before rate limiting, as middleware between `ReadBody`
and `RateLimit` in `main.go`
([internal/middleware/cache.go](internal/middleware/cache.go)), not in
`ModifyResponse`: a hit has to skip the rate limit, which runs before the proxy.
A miss learns how its response ended from the same usage callback
(`middleware.ReportUsage`), and is stored once the request is over. Week 5's
budget check goes inside the cache too, so a hit is never charged.

## Observability

Each request produces one JSON line on stdout:

```json
{"time":"2026-09-20T18:20:00Z","level":"INFO","msg":"request","request_id":"req_2d10cec6dc4299d10f08dbf141ebffb9","method":"POST","path":"/openai/v1/chat/completions","status":200,"duration_ms":842.11,"bytes":1204,"remote_addr":"127.0.0.1:52233"}
```

`request_id` is the `X-Request-ID` returned to the client. When the client
sent its own `X-Request-ID`, it is logged as `client_request_id`.

Plus one line per upstream response, recording which provider answered, the
status, content length, and whether the response was streamed.

### Metrics

`GET /metrics` serves Prometheus text format and needs no key. The gateway binds
to loopback by default, so only this machine can read it; Week 6 decides between
admin auth and a separate port. No metric has a tenant or model label.

| Metric                                       | Meaning                                                                                 |
| -------------------------------------------- | --------------------------------------------------------------------------------------- |
| `gateway_requests_total{provider,status}`    | Requests per route and HTTP status, `401`, `403`, `413`, `429` and `503` included.      |
| `gateway_request_duration_seconds{provider}` | Histogram of the whole response, streaming included.                                    |
| `gateway_tokens_total{provider,type}`        | Tokens by `type`: `input`, `cached_input`, `cache_write`, `cache_write_1h` or `output`. |
| `gateway_concurrency_wait_seconds{provider}` | Histogram of time spent waiting for a slot.                                             |
| `gateway_provider_up{provider}`              | `1` or `0`, from the health check.                                                      |
| `gateway_fallbacks_total{from,to,reason}`    | Fallbacks taken; `reason` is `unreachable`, `timeout` or `server_error`.                |

Tokens are labelled with the provider that produced them, so a fallback's tokens
count under `groq` while its request counts under `ollama`.
