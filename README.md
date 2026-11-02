# llm-gateway

A reverse proxy that sits between your applications and LLM providers. It routes
each request to the right provider, presents that provider's API key in whatever
header it expects, and logs the result. It enforces each tenant's
tokens-per-minute limit and monthly dollar budget, and answers repeated
requests from a response cache.

Providers speak their own native APIs — the gateway never translates request or
response bodies, so new provider features work the day they ship. Adding a
provider is a block of YAML, not code.

> **⚠️ Keep it on loopback for now.** Every provider route requires a tenant
> key and is held to the tenant's budget, but there is no TLS yet, and
> `/metrics`, on a port of its own, has only the admin key to guard it.
> Prometheus holds that key in a compose secret, and the same key creates
> tenants and mints keys. So the gateway binds to `127.0.0.1` by default, and
> `metrics_port` binds to the same `host`. Binding wider is your call to make
> deliberately. Whether a Prometheus container can reach a gateway on loopback
> depends on the container runtime; see [The dashboard](#the-dashboard).

## Project status

**Last updated:** 2026-11-01
**Stage:** Weeks 1 to 6 of 8 complete; Week 6's checkpoint passed and awaits
its review. Every provider
request is authenticated with a tenant key and held to its tenant's
tokens-per-minute limit, and every request that reaches a provider, streamed or
not, is recorded in Postgres with its tokens and cost, verified live against
Groq and Gemini. A tenant over its limit gets `429` with `Retry-After` before
any provider is called, and each charge is corrected to the tokens the
provider reported. A repeated non-streaming request that sets temperature 0 or
sends `X-Gateway-Cache: on` is answered from Redis, without a charge and without
calling the provider, in about 1.1 ms (median of 20 hits, by curl). A paid
request is also held to its tenant's monthly budget: before the provider is
called, the gateway reserves the most the request could cost, and a tenant whose
budget can't cover that gets `402`. Of 50 requests sent at once against room for
10, exactly 10 reach the provider, and a month lost with Redis is rebuilt from
the usage rows. Each request on a provider route now also leaves one JSON line
that names its tenant, provider and model and carries its tokens and cost.
Prometheus metrics by tenant and model are served on a port of their own,
behind the admin key, `GET /ready` checks Postgres and Redis, and `make obs-up`
starts Prometheus and Grafana with a provisioned dashboard. In a 15-minute
live run against Groq, Gemini and Ollama, every panel of the dashboard showed
data, and the metrics agreed with the responses, the usage rows and the admin
API, cost included, in all 53 comparisons.

### What changed

- **Week 6 checkpoint.** `TestObservabilityCheckpoint` in
  `cmd/gateway/observability_test.go`, through the real router against a real
  Postgres and Redis, sends a fixed mix of requests for two tenants to a mock
  provider whose answers are known. It then checks that the metrics, the usage
  rows and the request lines it captured agree: every counter's samples,
  tokens and cost against the rows' sums by tenant, provider and model, the
  budget gauge against `Ledger.Spent`, each line's tokens and cost against its
  row, and the counts of the overhead and TTFB observations.
  It passed 90 of 90 runs under `-race` with `-cpu 1,2,4`. Of 43 mutations
  that each remove one metric's recording, or a line that feeds one, it fails
  for 41; `TestUsageWriter` and `TestBudgetRecordsTheSpend` fail for the other
  two. In a 15-minute live run against Groq,
  Gemini and Ollama, with Prometheus and Grafana from `make obs-up`, every
  panel showed data, and the metrics' cost, 10,985 µ$, equalled the usage
  rows'. The screenshot is in [The dashboard](#the-dashboard). See
  [Week 6 deliverables](#week-6-deliverables--observability-) and
  [Observability](#observability).
- **Upstream TTFB and the gateway's own overhead.**
  `gateway_upstream_ttfb_seconds{provider}` times each attempt from its
  handoff to the provider's transport, after any wait for a concurrency slot,
  to the response headers: a stream's start, or about the whole generation
  for a non-streaming request. Each attempt that got a response counts under
  its own provider, so a fallback after a `5xx` counts twice.
  `gateway_overhead_seconds{provider}` times the gateway's own work before a
  provider sees the request (auth, the body read, the cache lookup, the
  rate-limit and budget checks), once per request, under the route. Cache
  hits and refused requests have neither, and the wait for a slot is in
  `gateway_concurrency_wait_seconds` alone.
- **Budget gauges per tenant.** `gateway_budget_spent_micros{tenant}` and
  `gateway_budget_micros{tenant}` show each tenant's spend for the month,
  reservations in flight included, and its budget, as the ledger reported
  them to the tenant's latest request to a paid provider: after its
  reservation, admitted or refused, and after its settlement. A tenant
  appears once it sends such a request after the gateway starts, and one
  idle since the month began shows last month's figure until its next
  request. `GET /admin/tenants/{id}/budget` stays the exact figure. To feed
  the gauges, settling now returns the new spend; what it adds, and when,
  doesn't change.
- **Metrics by tenant and model, and four new counters.**
  `gateway_requests_total` and `gateway_tokens_total` gain `tenant` and
  `model` labels, and these are new:
  - `gateway_cost_micros_total{tenant,provider,model}`: what responses cost,
    as their usage rows have it
  - `gateway_cache_requests_total{tenant,provider,result}`: the cache's
    decision, `hit`, `miss`, `bypass` or `error`
  - `gateway_ratelimit_rejections_total{tenant,provider}`: the gateway's own
    `429`s, not a provider's
  - `gateway_budget_rejections_total{tenant,provider,reason}`: `exceeded`
    (`402`), `unknown_model` (`400`) or `unavailable` (`503`)

  `model` is the price table's name, so `gpt-4o-2024-08-06` counts as
  `gpt-4o` and a snapshot with a price of its own keeps its name. Any other
  name, every Ollama model included, is `other`. `tenant` is `none` for a
  request refused before its tenant was known. So clients can't add series:
  tenants come from the admin API, and models from the price table.
  Histograms keep only `provider`, and `gateway_request_duration_seconds`
  now starts at 1 ms, about a cache hit's time. A `1xx` sent ahead of the
  final status no longer becomes the request line's status.
- **Prometheus and Grafana join the compose stack.** `make obs-up` starts
  them and waits until both are healthy, and `make obs-down` stops them.
  `make db-up` still starts only Postgres and Redis.
  - **Prometheus,** on `127.0.0.1:9092`, scrapes the gateway's metrics port
    every 5 seconds with the admin key. Its data lasts until
    `docker compose down`.
  - **Grafana,** on `127.0.0.1:3000`, has the Prometheus datasource and the
    "LLM Gateway" dashboard provisioned from `deployments/grafana/`. Anyone
    who can reach the port may view it without a login. The dashboard can't
    be saved from the UI.
  - **The key** comes from a compose secret filled from `GATEWAY_ADMIN_KEY`,
    or from the mock's key when that is unset, in `obs-up` only. It is never
    in git or in a file in the repo.
- **Each request line says who sent the request, where it went and what it
  cost.** On a provider route, the line adds:
  - `tenant_id`, `provider` (the route), `model` as the client named it, and
    `streamed`
  - once an attempt has ended, its usage: `input_tokens`,
    `cached_input_tokens`, `cache_write_tokens`, `output_tokens`,
    `cost_micros` (`null` when the cost isn't known) and `cached`, plus
    `fallback_provider` when another provider answered. These repeat the
    request's last usage row, which `request_id` joins.

  Each field appears only once it is known, so a `401` names only the route,
  and a `429` or `402` has no usage. Lines for the admin API, `/health` and
  unknown paths don't change.
- **`/metrics` on a port of its own, behind the admin key.** `metrics_port`
  (default 9091, on the gateway's `host`) serves the metrics and nothing else.
  A scrape needs the admin key as a Bearer token: without it, or with a
  tenant's key, it gets `401 invalid_admin_key`, and so does every scrape
  when no admin key is set. Prometheus from `make obs-up` sends the key. The
  main port no longer serves `/metrics`, so it answers `404` there.
  - A `metrics_port` outside 1–65535, equal to `port`, or held by another
    process stops startup with an error that names it.
  - The startup line, `gateway listening`, names both addresses: `addr` and
    `metrics_addr`.
- **`GET /ready`.** It pings Postgres and Redis at the same time, each given
  a second, and answers `200` with `"status":"ready"`, or `503` with
  `"status":"not_ready"` and the failing check `down`.
  - It never calls a provider.
  - An error's text goes only to the log, at Warn, since the endpoint needs no
    key.
  - `/health` doesn't change.
- **go-redis's own lines are JSON.** Its messages, such as `failed to dial`
  while Redis is down, are logged at Warn with `"component":"go-redis"` from
  startup on, not as plain text on stderr.
- **The usage writer's metrics.** `gateway_usage_writer_queue_depth`, the
  rows waiting in the writer's queue, and
  `gateway_usage_writer_dropped_total`, the rows it dropped, are read at each
  scrape.
- **Week 5 checkpoint closed.** Six tests in
  `cmd/gateway/budget_checkpoint_test.go`, through the real router against a
  real Postgres and Redis, show that a tenant with a $1 budget gets its first
  `402` after 18 requests and $0.90 of usage, once its next request could cost
  more than is left, and that the usage rows then sum to the ledger's spend;
  that 50 simultaneous requests against room for 10 put exactly 10 through; and
  that a month lost with Redis is rebuilt from the rows, so the next request is
  admitted or refused exactly as before, and a request in flight when it was
  lost still ends with the spend equal to the rows' sum. A live check against
  Groq and Gemini passed: every `402` was exact,
  `GET /admin/tenants/{id}/budget` matched the usage rows, and the spend
  survived a Redis restart. See
  [Week 5 deliverables](#week-5-deliverables--per-tenant-budgets-) and
  [The budget contract](#the-budget-contract).
- **A client that leaves during the budget check is no longer an error.** When
  the budget check fails and the request's own context is done, the gateway now
  logs "client closed request during budget check" at Info and ends the request
  with 499 and no body, as the proxy does for a client that leaves before the
  provider answers. Until now that was an Error, "budget check failed", and a
  503 that nobody read. A ledger that fails or times out while the client is
  still there still logs the Error and answers `503 budget_unavailable`.
- **A tenant's budget can be read over the admin API.**
  `GET /admin/tenants/{id}/budget` reports the current UTC month in the
  fields a 402 uses: `budget_micros`, `spent_micros`, `remaining_micros`
  (never below 0), `period` and `resets_at`.
  - `spent_micros` is the ledger's: settled costs plus requests still in
    flight. If Redis has lost the month, it is rebuilt from Postgres first.
  - Asking reserves nothing.
  - An unknown tenant gets 404, and Redis is never asked about it. When the
    ledger can't answer, the report gets `503 budget_unavailable`.
- **Each paid attempt is settled at its real cost.** Once an attempt is over,
  the gateway swaps its reservation for what it cost, so a budget now fills
  at the real cost, not the reservation rate.
  - The charge is the cost on the attempt's usage row. The ledger and
    `usage_logs` price a response with the same function, so they agree to the
    micro-dollar.
  - A provider's error, a failed dial, and a request that never got a
    concurrency slot are charged 0, so the whole reservation comes back.
  - A request that may have been billed, but whose usage is unknown (a
    timeout, a stream the client left), keeps its reservation. If the usage
    seen so far costs more, it pays that instead.
  - Settlement runs even when a stream is aborted or the client has gone. If
    it fails, a Warn is logged and the reservation stays in the month's spend.
- **A paid request is refused when its tenant's budget can't cover it.**
  Before each attempt on a paid provider, the gateway reserves the most the
  attempt could cost in the tenant's ledger. If that doesn't fit what is left
  of the month's budget, the attempt is refused. A ledger rebuilt after a
  Redis restart starts again from the real costs in `usage_logs`.
  - A request that doesn't fit gets `402 budget_exceeded` before its
    provider sees it. On a route without a fallback, the rate limiter
    refunds its whole charge.
    - The error carries `budget_micros`, `spent_micros`, `remaining_micros`,
      `reserve_micros`, `period` and `resets_at`.
    - Its message, in dollars, says what would help: waiting for the reset, a
      lower output cap, or, on a $0 budget, a free provider.
  - A model the price table lacks gets `400 unknown_model`, naming the
    provider and the model, and so does a request that names no model.
    - A body that isn't a JSON object has no model to read, so it gets this
      400 too, Gemini's included, though Gemini names its model in the URL.
      Until now the provider rejected it.
  - Budgets fail closed: when Redis or Postgres can't answer, a paid request
    gets `503 budget_unavailable`.
  - Free providers (Ollama) and cache hits never touch the budget. On the
    ollama → groq fallback, only groq's attempt reserves, at groq's price for
    the model the fallback maps to.
  - The first reservation that takes a tenant's spend to 80% of its budget
    logs a Warn. That happens once a month, and again only if the ledger is
    rebuilt.
- **What budgets reserve, and rebuild from.** Budgets reserve the most a
  request could cost before it reaches a paid provider.
  - The body owner now records a second, conservative input estimate for
    them: text at 3 bytes a token instead of the rate limiter's 4. Each image,
    audio or document part still counts as 1,600 tokens.
  - The usage writer now keeps, for each tenant, the cost of rows it has
    queued but not yet inserted. If Redis loses a budget, the budget is
    rebuilt from Postgres, and these rows are counted too.
  - The price table now records each model's maximum output, taken from its
    provider's model page. `pricing.Reserve` prices the most a request could
    cost:
    - **Input:** at the model's dearest input rate, so a prompt the provider
      writes to its prompt cache is covered.
    - **Output:** at the client's own cap. With no cap, or a cap above the
      model's maximum, the model's maximum output, the most it can actually
      answer.
  - `internal/budget` keeps each tenant's spend for the month in Redis, one
    hash per tenant and month.
    - One Lua script checks and reserves atomically, so two requests can't
      both take the last of a budget. Another script settles each reservation
      to the real cost.
    - A missing hash is rebuilt from `usage_logs` plus the writer's queue.
      A request still in flight while that happens is counted exactly once.
  - The dev Redis keeps no snapshots now (`--save ''`). A snapshot restored
    after a crash would bring back an older, lower spend, so every restart
    starts empty, and budgets are rebuilt from Postgres.
- **A request that never reached its provider costs nothing.** When the
  gateway can't connect at all (a DNS failure, a refused connection, a dial
  timeout), the request never left it. Its usage row now costs 0 instead of
  NULL, and the rate limiter refunds its whole charge. Budgets now refund it
  the same way, so a network outage can't spend a budget. Any other
  request that got no answer (a timeout, a connection dropped after sending, a
  client gone) still keeps its charge and leaves a row with no cost, since the
  provider may have billed it.
- **A tenant's limits can be changed over the admin API.**
  `PATCH /admin/tenants/{id}` sets any of `budget_micros`,
  `rate_limit_tokens_per_min` and `default_max_tokens`, and returns the
  tenant. The next request uses the new values. Until now they were set with
  SQL.
  - It refuses with `400 invalid_request`: a field it doesn't know, a `null`,
    anything but a whole number, and a value out of range.
  - Migration 00003 holds a budget between $0 and $1 billion, and the period
    to `monthly`, the only one the gateway enforces. The database itself
    refuses any other value, however it is written.
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
  `GET /metrics` serves Prometheus metrics (since Week 6, on `metrics_port`
  and behind the admin key). A live check against Ollama 0.40.2
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
  tokens, cost in micro-dollars (NULL, with a warning, for a response whose
  usage is incomplete; since Week 5 a paid provider's unpriced model is refused
  before it gets a row), whether it streamed, and latency. A request that got
  no response (timeout, unreachable provider, client gone) still leaves a row,
  with status 499/502/504 and NULL cost: the provider may have billed it. The
  exception is a failed dial (a DNS failure, a refused connection, a dial
  timeout): the request never left the gateway, so its row costs 0. Rows are
  queued and batch-inserted every 100 rows or second, off the response path: a
  full queue drops a row (logged and counted) rather than slow a response down,
  and shutdown drains the queue. This replaces the interim `usage` log line.
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
bucket or the budget. The order on a provider route is Auth → StripPrefix →
RequireMetered → ReadBody → Cache → RateLimit → (Fallback) → Budget → proxy.
For the same reason it is middleware rather than a hook in
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

### Week 5 deliverables — per-tenant budgets ✅

Goal: a paid request is refused, before its provider sees it, when its tenant's
monthly budget can't cover what it could cost, and concurrent requests can't
spend the same dollars twice.

- [x] Integer micro-dollars — prices, reservations, budgets and costs are
  whole µ$, with no floats, and the ledger adds with `HINCRBY`. Budgets and
  reservations stop at 1e15 µ$ ($1 billion), which keeps every number the Lua
  scripts compare exact
- [x] The reservation — `pricing.Reserve` prices the most a request could
  cost: the input estimate (text at 3 bytes a token, 1,600 per media part) at
  the model's dearest input-side rate, plus the output cap, held to the model's
  maximum output, which a request with no cap reserves whole. Each attempt
  reserves at its own provider's prices
- [x] Check-and-reserve in Redis — `internal/budget` keeps a tenant's month in
  one hash, `budget:{<tenant_id>}:<YYYY-MM>`, and one Lua script checks the
  budget and reserves in a single step, so two requests can't take the last of
  it. The hash expires a day after the month ends
- [x] Rebuilding after a Redis loss — a missing hash is rebuilt from the
  month's `usage_logs` rows plus the cost of rows still queued in the usage
  writer, and a request in flight settles its whole cost into the rebuilt
  month. The bounds where overspend is still possible are in the contract
- [x] Settlement at the real cost — once an attempt is over, its reservation
  is swapped for the cost on its usage row, priced by the same function, so the
  ledger and `usage_logs` agree to the micro-dollar. A provider's error, a
  failed dial and a request that got no concurrency slot cost 0. A request that
  may have been billed, with its usage unknown, keeps its reservation
- [x] The refusals — `402 budget_exceeded` carries the numbers behind it, and
  `400 unknown_model` refuses a model with no price, both before the provider
  sees the request
- [x] Free providers and cache hits never touch the budget — Ollama answers on
  a spent budget and with Redis down, and a hit is served before the budget is
  asked
- [x] The 80% warning — one Warn a month for each tenant, and again after a
  rebuild
- [x] The admin API — `PATCH /admin/tenants/{id}` sets a tenant's
  `budget_micros` (and its two limits), and `GET /admin/tenants/{id}/budget`
  reports the month
- [x] Tests — the reservation and the price table; the ledger, against a real
  Redis and Postgres; the middleware and the routes, over a fake ledger; and the
  checkpoint, through the real router against a real Postgres and Redis

[The budget contract](#the-budget-contract) has each of these in full.

The Week 5 checkpoint is "a $1-budget tenant gets 402 after about $1 of usage,
with no overspend under concurrent load or across a Redis restart." Six tests in
`cmd/gateway/budget_checkpoint_test.go` check it through the real router, built
from the parts `main` uses: the tenant store and usage writer on Postgres, and
the cache, the rate limiter and the budget ledger on Redis, on the client
`openRedis` builds. They sit in front of a provider whose every answer reports
20 input and 5,000 output tokens, so a request costs a known amount: 50,050 µ$
on `gpt-4o`, at $2.50 and $10 per million. They need `DATABASE_URL` and
`REDIS_URL`: without them they skip.

`TestBudgetOneDollar` gives a tenant a $1 budget and sends it `gpt-4o` requests
capped at 10,000 output tokens, one after another. Each reserves 100,068 µ$ and
settles at 50,050, so 18 get in, and the 19th finds 99,100 µ$ left, 968 short,
and gets `402` with every field of its error exact, the message aside. The 18
usage rows sum to 900,900 µ$, under the $1, and `spent_micros` from the budget
report equals them. `TestBudgetNoOverspendUnderLoad` sends 50 requests at once
against a budget 1 µ$ short of room for 11 reservations. The provider holds each
request it gets until all 50 have reached it or been refused, so none settles
while the others are checked. Exactly 10 reach the provider and 40 get `402`,
each finding the same spend, 10 reservations, and reporting the same
`reserve_micros`. Once the provider lets go, the rows sum to 500,500 µ$, within
the budget, and the ledger's spend equals them. The test opens its pool's 10
database connections before the requests go and allows no more: 50 cold
connections opening at once under `-race` could hold a Redis call past its
100 ms deadline, failing the test on a Warn or a 503 though no budget went
wrong. That happened in 2 of 40 runs before the cap, and in none of the 90
stress runs since.

`TestBudgetRedisLossAtTheBoundary` spends part of a budget on 5 requests: the
rows of 3 are committed to Postgres, and the rows of 2 are held in the writer's
queue by a table lock, so no batch can commit inside a rebuild and be counted
twice. With the budget 1 µ$ short of one more reservation, the next request is
refused with the same `402` before the loss and after it, from the month that
request rebuilds. With room for exactly one, a month lost and rebuilt by the
budget report holds the spend from before, and then admits one request and
refuses the next. `TestBudgetRedisLossInFlight` loses the month while a request
is held at the provider. The month is rebuilt without that request's
reservation, since it has no usage row yet, and with a new `gen`; once the
request ends, it settles its whole cost into the rebuilt month, and
`spent_micros` comes to the sum of the rows.

`TestBudgetFreeOnASpentBudget` uses up a tenant's budget on one request to groq.
Groq then gets `402`, while ollama, which is free, still answers: its usage row
costs 0, and the spend stays where it was. `TestBudgetFallbackAtGroqsPrice`
sends a request to ollama while ollama is failing, so groq answers. While groq's
attempt is in flight, the spend holds its reservation at groq's price for the
model the fallback maps to (`qwen/qwen3.8-27b`, for the client's `qwen3.5:9b`),
and once it settles, the spend has risen by exactly the cost on groq's usage
row.

Any line the gateway logs at Warn or above that a test doesn't expect fails it,
with a message saying what can cause it: budgets fail closed, so a Redis script
over 100 ms or a rebuild over 2 s refuses a request with
`503 budget_unavailable` where the cache and the rate limiter would let it
through with a Warn, and a settlement over 100 ms leaves a reservation in place
of the charge. The 80% Warn counts too: each test expects it exactly as often
as its reservations should log it.

`go test -race -count=30 -cpu 1,2,4 -run 'Budget' ./cmd/gateway/` passed: each
of the 22 tests it runs, these six and the 16 other `Budget` tests in the
package, passed 90 of 90 runs (30 at each of `-cpu` 1, 2 and 4), with none
failed or skipped and no data race. With `reserve.lua`'s check replaced by
"admit everything", `TestBudgetNoOverspendUnderLoad` fails: 50 of the 50
requests reach the provider, where it wants 10.

The gateway has also been exercised against live providers with budgets on
(2026-10-25 and 2026-10-26, built from `b266bde`, with the compose Postgres and
a Redis of its own, started with `--save ''`): Groq `openai/gpt-oss-120b`
through the `openai` Python SDK (3.37.0) and Gemini through `google-genai`
(2.33.0), each on a tenant whose budget was PATCHed small. The prompt was "Reply
with the single word: ok", and a capped request asks for 256 output tokens.

On Groq, a capped request (`reasoning_effort` low) reserved 161 µ$. With the
budget at 311 µ$, 7 requests were admitted and the 8th got `402` with every
field exact: spent 171, remaining 140, reserve 161.
`GET /admin/tenants/{id}/budget` said 171, the sum of the 7 rows. After a
`docker restart` of the Redis, the same `GET` rebuilt the month with spent 171,
and the next request got the same `402`. Raising the budget to spent + 161,
332 µ$, admitted exactly one more request and refused the next. Ollama, which
is free, answered on the spent budget, and its row cost 0. In all there were 9
rows, costing 197 µ$, equal to `spent_micros`, and one 80% Warn for each hash
the ledger built.

On `gemini-3.8-flash` both `402`s were exact. A capped request reserved 994 µ$
and was refused with 151 µ$ left. An uncapped one reserved 245,782 µ$ (its
65,536 maximum output tokens at $3.75 per million is 245,760, plus the input
estimate) and was refused as more than the whole budget. The requests that were
admitted got Gemini's own `503` "high demand", which gave the whole reservation
back, or timed out as `504`. Each `504` kept its whole reservation, with a
NULL-cost row: 994 µ$ for a capped request, and 245,782 for an uncapped one.
The Redis restart came between the two, and its rebuild counted the capped
request's row as 0, dropping its reservation. Both are documented bounds, seen
live.

`gemini-3.8-flash` answered `503` "high demand" or timed out on both days,
direct calls without the gateway included, so that is Gemini's capacity and not
the gateway's doing. The rest of the check ran on `gemini-3.6-flash`
(2026-10-26), which has the same price row. Gemini names its model in the URL,
so the request body and both reservations are identical. With the budget at
1,994 µ$, 4 capped requests were admitted, each only while 994 µ$ still fit, and
the 5th got `402` with every field exact: spent 1,187, remaining 807, reserve
994. Each row holds Gemini's own token counts, thoughts counted as output (8 in;
87, 62, 93 and 68 out), and costs exactly their price (332, 239, 355 and
261 µ$); `GET /admin/tenants/{id}/budget` said 1,187, the rows' sum. With the
budget then set to spent + 245,782, one uncapped request was admitted. While it
ran, `spent_micros` stood exactly 245,782 above its value before: the
reservation of the model's whole maximum output. When the request ended, that
was swapped for the 359 µ$ on its row, and the next uncapped request got an
exact `402`. In all there were 5 rows, none with a NULL cost, costing 1,546 µ$,
equal to `spent_micros`, and one 80% Warn. An earlier run that day, on another
tenant, had one capped request time out as a `504`, which kept its 994 µ$
reservation, as above.

### Week 6 deliverables — observability ✅

Goal: every request on a provider route leaves one JSON line that says who sent
it, where it went and what it cost, and Prometheus metrics by tenant and model
show the same facts, in a Grafana dashboard.

- [x] One record per request — `middleware.Logging` creates it and the layers
  inside fill in what only they know: the route, the tenant, the model the
  client asked for, the cache's decision, a `429`, a budget refusal and the
  ledger's spend, and the usage with its cost. The request line and the
  route's metrics both read it, so they can't disagree
- [x] The request line — on a provider route it adds `tenant_id`, `provider`,
  `model` and `streamed`, and once an attempt has ended its tokens,
  `cost_micros`, `cached` and `fallback_provider`, each only when known
- [x] Metrics by tenant and model — requests, tokens and the new cost counter
  carry `tenant` and `model`, which is the price table's name or `other`, so
  clients can't add series. The cache's decisions, the gateway's own `429`s
  and the budget's refusals are counters too
- [x] Timing — `gateway_upstream_ttfb_seconds` for each attempt that got a
  response, and `gateway_overhead_seconds` for the gateway's own work before a
  provider sees a request
- [x] Budget gauges — `gateway_budget_spent_micros` and
  `gateway_budget_micros` for each tenant, from the spend the ledger reports to
  each request, which `settle.lua` now returns
- [x] The usage writer's queue depth and its dropped rows, read at each scrape
- [x] `/metrics` on `metrics_port` (9091), behind the admin key; the main port
  answers `404` for it
- [x] `GET /ready` — Postgres and Redis, never a provider
- [x] go-redis's own log lines as JSON, through `slog`
- [x] Prometheus and Grafana in the compose stack — `make obs-up` and
  `make obs-down`, with the admin key as a compose secret
- [x] The dashboard — 13 panels and a row heading, provisioned from
  `deployments/grafana/dashboard.json`
- [x] The checkpoint test and the live run, below

[Observability](#observability) has each of these in full.

The Week 6 checkpoint is "a live Grafana dashboard, with a screenshot in the
README." `TestObservabilityCheckpoint` in `cmd/gateway/observability_test.go`
checks that what the dashboard draws is true. It runs the real router, built
from the parts `main` uses: Postgres for the tenants and the usage rows, and
Redis for the cache, the rate limiter and the budget ledger. The provider is a
mock whose answers are known. The test sends a fixed mix of requests for two
tenants, then reads the metrics, the usage rows and the request lines it
captured, and checks that they agree:

- every counter's samples
- tokens and cost, against the usage rows' sums by tenant, provider and model
- the budget gauge, against `Ledger.Spent`
- each request line's tokens and cost, against its usage row
- the counts of the overhead and TTFB observations

It needs `DATABASE_URL` and `REDIS_URL`: without them it skips.

The first tenant has the default limits. Its requests cover every type of
token, a stream, a dated model, a cache miss, hit and failed lookup, a
provider's own `429`, a fallback, and an unpriced model. The second is held to
one token a minute and 1,000 µ$. It sends a fallback the budget refuses, a
request to a free provider, and one the gateway's `429` refuses. A bad key
completes the mix, along with one more hit after the usage writer has closed,
whose row is dropped.

It passed 90 of 90 runs of `go test -race -count=30 -cpu 1,2,4 -run
'Observability' ./cmd/gateway/`, with no data race. 43 mutations each removed
one metric's recording, or a line that feeds one, from the production code:

- every counter's increment, and the duration, overhead and TTFB histograms'
  observations
- the budget gauges' sets, and the usage writer's queue depth and dropped count
- a token type, and the `none` and `other` labels
- the meter's usage and cost, and both transport wrappers
- each of the cache's decisions, and the rate limit's and the budget's records
- the request's model, route and tenant on the record
- the 1-hour cache writes in the request line and in the usage row
- the fallback's report

Two metrics are outside the mix: `gateway_concurrency_wait_seconds`, since no
route here has a concurrency limit, and `gateway_provider_up`, which only
`run()`'s health checker sets. Their own tests cover them, `TestProviderUp`
among them.

The test fails for 41 of the 43 mutations. It can't see the other two, and
other tests catch them:

- The queue depth reads 0 by the time it looks, once the writer has closed.
  `TestUsageWriter` catches it.
- The spend recorded at admission is recorded again at settlement.
  `TestBudgetRecordsTheSpend` catches it.

The live run uses a binary built from a clean tree and started on
`config.local.yaml`, so Groq, Gemini and, if it's running, Ollama answer.
Prometheus and Grafana come from `make obs-up`. A script sends a mix for about
15 minutes, at a rate inside the free tiers: cache hits, a tenant held to a low
rate limit, and a tenant with a small budget. The run checks that every panel
shows data and that the metrics' cost matches the usage rows, and the
screenshot in [The dashboard](#the-dashboard) is taken headless from the
anonymous dashboard.

The run was on 2026-11-01, from 22:18 to 22:33 (UTC−5), on a binary built from
the clean tree at 42a623e, with Ollama serving `qwen3.5:9b`. Prometheus and
Grafana came from `make obs-up`, with a fresh admin key. A round of requests
every 20 seconds for 15 minutes made 506 requests from three tenants:

- A plain tenant sent Groq `openai/gpt-oss-20b`, plain and streamed; the same
  `openai/gpt-oss-120b` request twice at temperature 0, a cache miss and then a
  hit (45 of each); Gemini `gemini-3.5-flash-lite`, plain and, every third
  round, streamed; and Ollama. Every third round also sent an unpriced model
  (`400 unknown_model`) and a bad key (`401`).
- A tenant held to 300 tokens a minute sent two requests a round. 61 of its 90
  got the gateway's `429`.
- A tenant with a 500 µ$ budget sent, each round, one small request and one it
  could never afford. It got 71 `402`s, and its spend reached 494 µ$.

The Ollama route ran with two settings `config.local.yaml` reads from the
environment: `OLLAMA_FALLBACK_ENABLED=true` and `OLLAMA_TIMEOUT_SECONDS=5`.
Every fourth round asked Ollama for a long answer. Each one timed out and fell
back to Groq's `qwen/qwen3.8-27b`, so the Fallbacks panel has real data. 18
requests fell back: the 11 long ones, and 7 short ones that the local model
didn't answer within 5 seconds. `gemini-3.8-flash` didn't answer within 60 seconds that
evening, so the run used `gemini-3.5-flash-lite`.

Every panel showed data. Each of the dashboard's 23 queries returned series
over the run's window, and the screenshot shows each panel drawn.

The metrics agreed with the responses the script got, the usage rows and the
admin API in all 53 comparisons, with no difference:

- requests by tenant, route and status
- the cache's decisions, the `429`s and the `402`s
- tokens by tenant, provider and type, and cost by tenant, provider and model
- the overhead and TTFB counts, and the fallbacks
- both budget gauges, against `GET /admin/tenants/{id}/budget`, for the three
  tenants

The cost counter came to 10,985 µ$ ($0.011), the same as the usage rows. Each
of the 344 request lines with a usage row repeated that row's tokens and cost,
and every log line was JSON. The usage writer dropped nothing.

The run led to no change to `dashboard.json`. Two things it showed about
reading the dashboard:

- A stat panel such as Provider up reads the last moment of the time range,
  so it says "No data" for a range that ends after the gateway stopped.
- A counter's series appears with its first event, so a rate doesn't count
  that first event, such as the first fallback.

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
make db-down    # stop everything, Prometheus and Grafana too; Postgres data survives in the llm-gateway_pgdata volume
```

Migrations live in `internal/db/migrations` and are embedded into the binary,
so a build always carries the schema its code expects. `make migrate` targets
the compose database; pass `DATABASE_URL=postgres://...` to use another.

Redis holds the rate-limit buckets (Week 3), the cached responses (Week 4) and
the budget ledgers (Week 5). It has no volume and takes no snapshots
(`--save ''`), because all three can be lost: losing the buckets only refills
every tenant, losing a cached response only makes the next identical request a
miss, and a lost ledger is rebuilt from `usage_logs` (see
[The budget contract](#the-budget-contract)). A snapshot would be worse than
none: restored after a crash, it would bring back an older, lower spend. It has
no password either, so binding to loopback is what keeps it private, which
matters more now that cached answers sit in it unencrypted until their TTL ends.

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
metrics_port: 9091  # port that serves /metrics, behind the admin key
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

`port`, `metrics_port`, `host`, `max_request_bytes` (default 32 MiB) and each
provider's `timeout` are optional, and so are the provider settings `free`,
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

`metrics_port` is where `/metrics` is served: a listener of its own, on `host`
like `port`, that serves the metrics and nothing else, behind the admin key.
The default is 9091. A value outside 1–65535 or equal to `port` is a startup
error, and so is a port that another process holds, with its address in the
message. [Metrics](#metrics) has the details.

Budgets are not configuration. A tenant's monthly budget is its `budget_micros`
in Postgres ($50.00 by default), changed with `PATCH /admin/tenants/{id}`; the
only budget setting here is a provider's `free: true`, which exempts it.
[The budget contract](#the-budget-contract) has the details.

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
usage rows, and because a free provider is never budgeted: it is served even
when a tenant's budget is spent. The environment variables are all optional,
and [`.env.example`](.env.example) lists them:

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
Ollama attempt costs 0, and the Groq attempt is priced normally. Budgets follow
the same split: Ollama's attempt reserves nothing, and Groq's reserves at
Groq's prices for the model it maps to. A tenant whose budget can't cover that
gets `402` with `X-Gateway-Provider: groq` and `X-Gateway-Fallback: true`
instead of Groq's answer, and a mapped model with no price gets
`400 unknown_model` (see [The budget contract](#the-budget-contract)).

**Docker.** The gateway has no compose service yet (Week 7). Once it runs in a
container, set `OLLAMA_BASE_URL=http://host.docker.internal:11434` and add
`extra_hosts: ["host.docker.internal:host-gateway"]` to its service. Under
Colima, a container reached a service bound to the Mac's `127.0.0.1` when
tested (see [The dashboard](#the-dashboard)), so Ollama may not need to listen
beyond loopback. An earlier test with Ollama found that a container couldn't
reach it; the cause is unknown, and Ollama's own host checks are one
possibility. Check Ollama's case in Week 7 before setting `OLLAMA_HOST`
(Ollama's own setting) to `0.0.0.0`. That exposes Ollama's unauthenticated API
to the network: bind it to the Colima bridge address or firewall it.

## Endpoints

| Path                              | Behavior                                                                                  |
| --------------------------------- | ----------------------------------------------------------------------------------------- |
| `GET /health`                     | Always `200`: `{"status":"ok"}`, plus provider checks. Handled locally, no key needed.    |
| `GET /ready`                      | `200` with `{"status":"ready","checks":{"postgres":"ok","redis":"ok"}}` while Postgres and Redis both answer within a second; otherwise `503` with `"status":"not_ready"` and the failing check `down`. Handled locally, no key needed, and never calls a provider. See [GET /ready](#get-ready). |
| `GET /metrics`                    | Not on this port: it gets the `404` of the last row. It is served on `metrics_port` (9091 by default), where it needs the admin key. See [Metrics](#metrics). |
| `/<provider>/...`                 | Needs a gateway key (`401` otherwise) and a metered endpoint (`403` otherwise, see below), within the tenant's tokens-per-minute limit (`429` with `Retry-After` otherwise, see [The 429 contract](#the-429-contract)) and, on a paid provider, with room in its monthly budget for what the request could cost (`402` otherwise, see [The budget contract](#the-budget-contract)). Prefix stripped, forwarded with the provider's key. An eligible repeat may be answered from the cache instead, without reaching the limit, the budget or the provider (see [The cache contract](#the-cache-contract)). |
| `POST /admin/tenants`             | Admin key. `{"name": "..."}` → `201` with the tenant and its first key.                   |
| `POST /admin/tenants/{id}/keys`   | Admin key. Issues another key for the tenant → `201`.                                     |
| `PATCH /admin/tenants/{id}`       | Admin key. Any of `budget_micros` (0 to 1e15 µ$; $50.00 by default), `rate_limit_tokens_per_min` (1 to 2^40) and `default_max_tokens` (1 to 2^31 − 1) → `200` with the tenant; the next request uses them. `400` for an unknown field, a `null`, a non-integer or a value out of range; `404` for an unknown tenant. |
| `DELETE /admin/keys/{id}`         | Admin key. Revokes the key → `204`; the row stays so past usage still attributes to it.   |
| `GET /admin/tenants/{id}/usage`   | Admin key. `?from=&to=` (RFC 3339, default this UTC month) → totals and per-model usage.  |
| `GET /admin/tenants/{id}/budget`  | Admin key. This UTC month's `budget_micros`, `spent_micros` (in-flight reservations included) and `remaining_micros` (never below 0), with `period` and `resets_at`; `404` for an unknown tenant, `503 budget_unavailable` if the ledger can't answer. See [The budget contract](#the-budget-contract). |
| anything else                     | `404` with a JSON body listing the configured providers.                                  |

Metered endpoints, the only ones forwarded:

| Format      | Endpoints                                                                          |
| ----------- | ---------------------------------------------------------------------------------- |
| `openai`    | `POST /v1/chat/completions`                                                        |
| `anthropic` | `POST /v1/messages`                                                                |
| `gemini`    | `POST /v1/models/{model}:generateContent`, `:streamGenerateContent` (also `/v1beta/`) |

Every response on this port carries an `X-Request-ID`. Key plaintext appears
only in the `201` that issues it, with `Cache-Control: no-store`. Any `/admin/`
request without the admin key gets `401`, whatever the path or method.

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
  limit or the budget, so it is served even when the tenant's bucket is empty
  or its budget is spent. It leaves a usage row with `cached` true, no tokens
  and cost 0, which the usage report counts as a request. Entries live for
  `cache.ttl` (3,600 seconds by default) from the moment they are stored, and a
  hit doesn't extend that. There is no way to flush a tenant's entries, and a
  hit carries no `Age` header.
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

### The budget contract

Every tenant has a monthly budget, in dollars. Before a request goes to a paid
provider, the gateway reserves the most that request could cost. When the
reservation doesn't fit what is left of the month's budget, the gateway answers
`402` itself and the provider is never called. Here is one (a `gpt-5.5` request
with `max_completion_tokens` 1,000 and a 300-byte body, against a $1.00 budget
with $0.98 spent). The body has this shape for every provider:

```json
{"error":{"budget_micros":1000000,"message":"this request could cost up to $0.0305, more than the $0.02 left of the gateway's $1.00 monthly budget for this tenant; the budget resets at 2026-11-01T00:00:00Z","period":"2026-10","remaining_micros":20000,"reserve_micros":30500,"resets_at":"2026-11-01T00:00:00Z","spent_micros":980000,"type":"budget_exceeded"}}
```

Once an admitted request is over, its reservation is swapped for what it really
cost. Reserving first is what keeps two requests that arrive together from both
taking the last of a budget: the check and the reservation are one Lua script,
which Redis runs atomically.

Money is counted in micro-dollars (µ$): 1,000,000 µ$ is $1. A tenant's budget
is its `budget_micros`: 50,000,000 ($50.00) by default, at most 1e15 ($1
billion), and 0 for free providers only. `PATCH /admin/tenants/{id}` changes it,
and the next request uses the new value against the month's spend as it stands.
The budget belongs to the tenant, so all of its keys, providers and models share
it. Its period is the UTC calendar month in which a request arrived, the same
clock as the request's usage row, and monthly is the only period: the database
refuses any other.

- **What a request reserves.** The most it could cost, estimated before the
  provider has seen it:
  - **Input** is a conservative estimate: the body's text at 3 bytes a token
    (the rate limiter's estimate uses 4), plus 1,600 tokens for each image,
    audio, document or file part, whose payload is left out. It is priced at the
    model's dearest input-side rate: the input rate, or a cache-write rate,
    which is up to twice the input rate (Anthropic's 1-hour writes). So a prompt
    the provider writes to its prompt cache is covered too.
  - **Output** is the client's own cap (`max_tokens`, `max_completion_tokens`,
    Gemini's `maxOutputTokens`) times the choices it asked for (`n`,
    `candidateCount`), but never more than the model's maximum output times the
    choices. A request with no cap reserves that maximum, since the gateway
    never rewrites a request to add one and the answer can be that long: at the
    prices of 2026-10-25, `gpt-5.5` reserves $3.84 of output (128,000 tokens) and
    `gemini-3.8-flash` $0.25 (65,536), plus the input. A lower cap keeps the
    reservation small, and a cap above the maximum is held to it. The maximums
    are the figures on each provider's model pages, kept in `internal/pricing`.
  - **The price** is the one in force when the request arrived. When the input
    estimate is over a model's long-context threshold, the long-context prices
    apply to the whole request. The total is rounded up to a whole micro-dollar
    and stops at 1e15 µ$, the largest budget, which keeps the Lua scripts'
    arithmetic exact: their numbers are doubles, exact only below 2^53.
  - **Each paid attempt** reserves at its own provider's prices. On the
    ollama → groq fallback, Ollama's attempt reserves nothing, and Groq's
    reserves at Groq's prices for the model the fallback maps to.
- **The 402.** `error.type` is `budget_exceeded`, and the body carries six
  fields:
  - `budget_micros`: the tenant's monthly budget.
  - `spent_micros`: the month's spend, which is settled costs plus the
    reservations of requests still in flight. A tenant with big requests open
    can be refused while its settled spend is well under its budget.
  - `remaining_micros`: the budget minus the spend, never below 0. A budget
    lowered under the spend has nothing left.
  - `reserve_micros`: the most this request could cost, the reservation that
    didn't fit.
  - `period`: the UTC month, such as `2026-10`.
  - `resets_at`: when the month ends, which is the next month's start in UTC, in
    RFC 3339.

  The message is in exact dollars ($0.004 is never shown as $0.00) and says what
  would help. An ordinary refusal names what the request could cost, what is
  left and when the budget resets. A request that could cost more than the whole
  budget gets a 402 whatever the spend, since waiting won't help, and its
  message says to set a lower output cap or send a shorter request. An uncapped
  request to an expensive model is such a request on a small budget. On a $0
  budget the message says the tenant can use free providers only.
- **No `Retry-After`.** The reset is weeks away, and the SDKs don't retry a 402.
  A client can tell the gateway's 402 from a provider's own error by its
  `error.type`, a message that names the gateway's monthly budget, and the six
  fields; a provider's own error passes through with its own body. A refused
  request leaves no usage row, and on a route without a fallback the rate
  limiter refunds its whole charge. The gateway logs one Info line for each 402.
- **`400 unknown_model`.** A request can be reserved only for a model that has a
  price. A model the price table lacks gets `400 unknown_model`, naming the
  provider and the model, before anything is reserved. So does a request that
  names no model, and one whose body isn't a JSON object, which has no model to
  read (Gemini's too, though Gemini names its model in the URL). Every tenant has
  a budget, so every tenant gets it, and the gateway logs a Warn, since the fix
  is a row in `internal/pricing/pricing.go`. A model
  can lack a price for want of a public one (the Llama models Groq lists as
  "contact sales") or on purpose (the `-latest` aliases, such as
  `gemini-flash-latest`, which Google moves to newer models, so a reservation
  priced for the old one could fall short). A free provider's models never get
  it.
- **Failing closed.** If the gateway can't check a budget (Redis fails, Postgres
  fails while a missing ledger is rebuilt, or either is too slow), a paid
  request gets `503 budget_unavailable`, the provider is never called and the
  gateway logs an Error. An outage costs availability, never overspend. This is
  the opposite of the rate limiter and the cache, which fail open, because a
  budget guards money. Each Redis script gets 100 ms, and a rebuild 2 s, however
  many times it reads again.
  - A client that leaves while the check runs is the exception. Nobody reads the
    503, so its request ends with status 499 and the gateway logs an Info line,
    as the proxy does for a client that has gone before the provider answers. A
    ledger that fails or times out while the client is still there stays an
    Error and a 503.
  - A reservation that fails after Redis has run it, such as one whose reply was
    lost, stays in the month's spend. That over-counts, never under-counts.
- **Free providers and cache hits never touch the budget.** A provider with
  `free: true` (Ollama) has no reservation, no Redis call and no settlement, so
  it is served when the budget is spent and when Redis is down, and its usage
  rows cost 0. A cache hit is answered before the rate limit and the budget, so
  it reserves nothing, costs 0 and is served on a spent budget.
- **Settlement.** When an admitted attempt is over, the gateway swaps its
  reservation for its charge, by the last usage reported for the attempt:
  - **No report: 0.** Nothing reached the provider, as with a request that got
    no concurrency slot (`503 provider_busy`).
  - **A complete response: its cost,** priced by the function that prices its
    usage row, so the ledger and `usage_logs` agree to the micro-dollar. A
    provider's 4xx or 5xx carries no tokens and costs 0, so the whole
    reservation comes back.
  - **A failed dial: 0.** A DNS failure, a refused connection or a dial timeout
    means the request never left the gateway. It is reported complete with no
    usage, so its row costs 0 and not NULL, and the rate limiter refunds it in
    full. Without this, a network outage would use up budgets at the reservation
    rate.
  - **An incomplete response: the reservation,** or the cost of the usage seen
    so far if that is more. That is a timeout, a stream the client abandoned, or
    a connection dropped after the request was sent. The provider may have
    billed it, so it keeps what it reserved, as the rate limiter keeps its
    charge, and its row's cost is NULL. For an uncapped request, the reservation
    is the model's maximum output.

  Settlement runs once for each attempt, in a `defer`, so it also runs when the
  proxy aborts a stream and when the client has gone. It holds the response's
  last bytes for one Redis call, 100 ms at most. If it fails, the gateway logs a
  Warn and the reservation stays in the month's spend in place of the charge.
- **The 80% warning.** The first reservation in a month that takes a tenant's
  spend (reservations in flight included) to 80% of its budget or more logs one
  Warn, naming the tenant. That is once per tenant and month, and again only if
  the ledger is rebuilt.
- **Reading a budget.** `GET /admin/tenants/{id}/budget` reports the current
  month's `budget_micros`, `spent_micros`, `remaining_micros`, `period` and
  `resets_at`, with the `tenant_id`. `spent_micros` is the ledger's, rebuilt
  first if it is missing, and asking reserves nothing. It can differ from the
  usage report (`GET /admin/tenants/{id}/usage`), which sums the rows' costs: the
  ledger also holds reservations in flight, and those kept by requests cut short,
  whose rows have no cost. An unknown tenant gets 404, and Redis is never asked
  about it. A ledger that can't answer gets `503 budget_unavailable`.
- **Rebuilding after a Redis loss.** Postgres is the record of what a tenant
  spent, and Redis holds a running total of it: one hash for each tenant and
  month, `budget:{<tenant_id>}:<YYYY-MM>`, with `spent` (settled costs plus
  reservations in flight), `gen` and `warned`. It expires a day after the month
  ends, so a request that arrived on the last day still settles into it. When a
  request finds no hash (Redis restarted, or flushed or evicted it, or the month
  is new), the gateway rebuilds it before reserving:
  - It reads, in this order, the month's missed-settle counter (below), the cost
    of the tenant's rows still queued in the usage writer, and `SUM(cost_micros)`
    over the month's rows in `usage_logs`. The queue comes before the sum so that
    a batch that commits between the two reads is counted twice, an over-count,
    and never missed. A row with no cost counts as 0.
  - It writes the total only while the hash is still missing, inside the Lua
    script that reserves, with a new random `gen`. A settle that finds no hash
    writes none: it adds one to the missed counter, which expires with the hash.
    If the counter has moved since the rebuild read it, nothing is written and
    the reads start again, because that settle's row may have been queued after
    the queue was read and committed after the sum.
  - A request in flight when the hash went missing holds a reservation the
    rebuilt hash doesn't have. It carries the old `gen`, so it settles its whole
    cost into the new hash.
  - Concurrent rebuilds of one hash share one set of reads, which gets 2 s and
    runs under `context.WithoutCancel`, so one client leaving can't fail the
    others waiting on it.
- **Where overspend is still possible.** Under concurrent load and across a
  Redis restart, a tenant doesn't spend past its budget, within these bounds:
  - **A reservation below the real cost.** The input estimate and the output
    allowance fall short for media beyond 1,600 tokens a part (PDFs, audio,
    video, large images, `fileData`); text that takes more than a token per 3
    bytes, such as dense non-Latin text; prompt tokens the body doesn't contain,
    such as a chat template's (Groq counted 78 prompt tokens for
    `openai/gpt-oss-120b` on a 145-byte body that the gateway estimates at 49,
    because gpt-oss's template adds about 70, so a request whose answer reaches
    its cap can cost a few µ$ more than it reserved: here 29 tokens at $0.15 per
    million, about 4 µ$); Gemini's tool-use prompt tokens (code
    execution, grounding), which are billed as input but aren't in the request;
    Gemini 4 Argon's long-decode continuations, where each further request pays
    again for the state its encrypted token carries; and prices the table
    doesn't model (Anthropic's fast mode and `inference_geo`, OpenAI's service
    tiers other than Standard, Gemini's audio input rates). Such a request is
    charged its real cost when it settles, which can take the month past its
    budget. The overspend is bounded by how far the requests in flight when the
    budget runs out cost more than they reserved. After them, the next request
    is refused.
  - **A settlement that fails.** The reservation stays in place of the charge. A
    request that cost less than it reserved is over-counted, and one that cost
    more is under-counted by the difference. If the month's hash was missing,
    a settle that failed before Redis ran it added nothing to the missed
    counter, so a rebuild already under way can miss that request's row.
  - **A rebuild.** It counts what this gateway's queue and Postgres hold when it
    reads them. It misses rows queued on other gateway replicas, rows the usage
    writer dropped (a full queue, or Postgres rejecting the row), and the rows a
    gateway crash loses: those queued and not yet flushed, normally the last
    second's. It forgets the reservations that requests cut short keep, since
    their rows have no cost. And it doesn't hold the reservations of requests in
    flight when the hash went missing until those requests settle.

  The other mistakes are over-counts, which refuse a tenant early: a
  reservation whose reply was lost, and a row a rebuild counts twice. They end
  when the month does, or at the next rebuild.
- **Redis without persistence.** The dev stack starts Redis with `--save ''`: no
  snapshots, no append-only file (Redis's default) and no volume. Postgres, not
  Redis, is the record of spend, and a restart is safe only if Redis comes back
  empty. A snapshot restored after a crash would bring back an older, lower
  `spent`, and the gateway would trust it, since a hash that exists is never
  checked against Postgres. Empty, every ledger is rebuilt from `usage_logs` by
  its next request. Production should run the Redis that holds budgets the same
  way, with no snapshots and no append-only file. It should also keep budget
  keys out of eviction: an evicted ledger is rebuilt like a lost one, with the
  bounds above, and a missed counter evicted during a rebuild can hide a settle,
  so that request's cost is counted nowhere. The dev Redis sets no `maxmemory`,
  so nothing is evicted there. Week 7 plans one for the cache's sake, and budget
  keys need a Redis of their own under `noeviction`, or no expiry under a
  `volatile-*` policy, which evicts only keys that have one, with past months
  deleted another way.

## Make targets

| Command         | What it does                                                      |
| --------------- | ----------------------------------------------------------------- |
| `make build`    | Compile to `./bin/gateway`                                        |
| `make run`      | Build, then run                                                   |
| `make mock`     | Run the mock provider on :9090                                    |
| `make run-mock` | Run the gateway against the mock                                  |
| `make db-up`    | Start local Postgres and Redis                                    |
| `make db-down`  | Stop the whole stack: Postgres, Redis, Prometheus, Grafana        |
| `make obs-up`   | Start Prometheus and Grafana; see [The dashboard](#the-dashboard) |
| `make obs-down` | Stop Prometheus and Grafana; Postgres and Redis keep running      |
| `make migrate`  | Apply pending database migrations                                 |
| `make test`     | Run all tests (needs `make db-up`)                                |
| `make vet`      | Run `go vet` (catches suspicious code)                            |
| `make fmt`      | Format all source files                                           |
| `make tidy`     | Sync `go.mod` / `go.sum`                                          |
| `make clean`    | Remove `./bin`                                                    |
| `make all`      | fmt, vet, test, build                                             |

## Layout

```
cmd/gateway/main.go       entry point: loads config, builds routes, serves
cmd/gateway/redislog.go   go-redis's own log lines, routed through slog
internal/config/          YAML config loading and validation
internal/provider/        per-provider auth styles and header handling
internal/proxy/           the httputil.ReverseProxy, its hooks and the fallback
internal/middleware/      request IDs, request logging and the per-request record, tenant key auth, response cache, rate limiting, budgets
internal/health/          GET /health and GET /ready
internal/concurrency/     per-provider request limit and its queue
internal/metrics/         Prometheus metrics
internal/mockprovider/    fake OpenAI/Groq/Anthropic/Gemini APIs for testing
cmd/mockprovider/main.go  serves the mock on 127.0.0.1:9090
internal/db/              Postgres schema: embedded goose migrations
internal/db/dbtest/       throwaway Postgres database for tests
internal/tenant/          tenants and API keys: issue, revoke, look up
internal/usage/           request body owner and estimates, usage parsers and metering, the usage_logs writer and reports
internal/pricing/         per-model prices in micro-dollars, and the most a request could cost
internal/ratelimit/       per-tenant token bucket in Redis (Lua scripts)
internal/budget/          per-tenant monthly spend in Redis (Lua scripts), rebuilt from Postgres
internal/cache/           exact-match response cache: the request key, entries in Redis
internal/redistest/       Redis client for tests
internal/admin/           admin API: create tenants, change their limits, issue and revoke keys, report usage and budgets
internal/apierror/        the one JSON error shape every response uses
cmd/migrate/main.go       applies migrations to DATABASE_URL
deployments/              docker compose dev stack (Postgres, Redis, Prometheus, Grafana), and prometheus.yml
deployments/grafana/      the dashboard and its screenshot, and what provisions them
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
client. That is where usage accounting reads token counts. Returning an error
from it causes `ErrorHandler` to run instead of the response being forwarded,
which is how a fallback replaces a 5xx with another provider's answer.

Rate limiting runs *before* the proxy, as middleware after `ReadBody` in
`main.go`, so a refused request never reaches the provider. It settles on the
way back too: the usage callback, which fires as each response ends (or fails),
reports that request's tokens to it (`middleware.ReportUsage`), and it corrects
the bucket once the request is over.

Budgets run before the proxy too, as `middleware.Budget`
([internal/middleware/budget.go](internal/middleware/budget.go)), not as an
error from `ModifyResponse`: a request over budget is answered there with a
`402`, and its provider never sees it. Budget wraps each provider's own proxy,
inside the rate limit and any fallback, so each attempt is reserved and settled
at the prices of the provider that serves it. It settles through the same usage
callback: `middleware.ReportUsage` also records each report on the attempt's
reservation, and Budget swaps the reservation for the real cost in a `defer`,
which runs even when the proxy aborts a stream.

The response cache runs before rate limiting, as middleware between `ReadBody`
and `RateLimit` in `main.go`
([internal/middleware/cache.go](internal/middleware/cache.go)), not in
`ModifyResponse`: a hit has to skip the rate limit, which runs before the proxy.
A miss learns how its response ended from the same usage callback
(`middleware.ReportUsage`), and is stored once the request is over. The budget
check sits inside the cache too, so a hit is never reserved or charged.

Observability hooks in at the same points. Each request has a `Record`
([internal/middleware/record.go](internal/middleware/record.go)): `Logging`
creates it and puts a pointer to it on the context, and the layers inside fill
in what only they know. `Route` records the route, `Auth` the tenant, and
`RecordRequest`, just inside `ReadBody`, the model and stream flag the client
asked for. `Cache` records its decision, `RateLimit` its own `429`, and
`Budget` a refusal and its reason, and the ledger's spend with the tenant's
budget. The meter in `router()` in `main.go` records the usage last reported,
priced as its usage row is (`usageCost`). `Logging` reads the `Record` for the
request line, and `metrics.Requests`, which wraps each provider route, reads
the same one when the route ends, so the line and the metrics can't disagree.
A new fact is a new field on the `Record`, set by the layer that learns it and
read in those two places. Everything that touches it runs on the goroutine
serving the request, so it needs no lock.

The two timings wrap each provider's transport instead, where the response
headers arrive: `m.UpstreamTTFB` innermost, inside any concurrency limit, and
`m.Overhead` outermost, before it. The usage writer's two metrics are read at
each scrape (`m.UsageWriter`), so the writer knows nothing of metrics.

In Week 7, when the gateway runs in the compose stack, Prometheus is to scrape
`gateway:9091` over the compose network with the same secret, and the metrics
port needn't be published.

## Observability

The gateway reports on itself three ways: one JSON line for each request,
Prometheus metrics on a port of their own, and a Grafana dashboard that draws
the metrics. [`GET /ready`](#get-ready) says whether it can serve at all.

### The request line

Every request produces one JSON line on stdout. This one is a non-streaming
request to Groq, from the Week 6 live run:

```json
{"time":"2026-11-01T22:21:27.958418-05:00","level":"INFO","msg":"request","request_id":"req_a3496c2963c0d0ebcbba12fbe60f2514","method":"POST","path":"/groq/v1/chat/completions","status":200,"duration_ms":224.206,"bytes":883,"remote_addr":"127.0.0.1:63195","tenant_id":"tn_3494fc7bb5f6aae1","provider":"groq","model":"openai/gpt-oss-20b","input_tokens":94,"cached_input_tokens":0,"cache_write_tokens":0,"output_tokens":64,"cost_micros":26,"cached":false,"streamed":false}
```

`request_id` is the `X-Request-ID` returned to the client. When the client
sent its own `X-Request-ID`, it is logged as `client_request_id`.

On a provider route (`/<provider>/...`) the line adds these fields, each only
once it is known:

- `tenant_id`: the tenant whose key the request carried. A `401` has none.
- `provider`: the route the client called, even when another provider
  answered.
- `model`: the model as the client named it, unchanged. The metrics' `model`
  label is the price table's name for it.
- `streamed`: whether the response was streamed. A request refused after its
  body was read has it too, as whether the client asked for a stream.
- Once an attempt has ended, its usage: `input_tokens`, `cached_input_tokens`,
  `cache_write_tokens` (the 5-minute and 1-hour writes together, as the usage
  row stores them), `output_tokens`, `cost_micros` and `cached`. These repeat
  the request's last usage row, which `request_id` joins. `cost_micros` is 0
  for a cache hit and for a free provider, and `null` when the cost isn't
  known, such as for a response cut short.
- `fallback_provider`, when a provider other than the route answered.

The usage fields appear for any attempt that ended: a provider's answer, a
cache hit, or the proxy's `502`, `504` or `499` for an attempt that got no
answer. A request the gateway refused itself has none: a `401` line names only
the route, and a `429` or `402` line has the tenant, provider, model and
`streamed`, but no usage. In two rare fallback cases the line keeps the failed
first attempt's usage, which has no tokens: the client leaves during the
fallback's budget check (`499`), or the fallback finds no concurrency slot
(`503 provider_busy`). Lines for the admin API, `/health`, `/ready` and unknown
paths have none of these fields.

The proxy also logs a line, `upstream response`, for each response a provider
sends: the `provider` that answered, the `status`, the `content_length` (`-1`
when chunked), the `content_type` and whether it is `streaming`. go-redis's own
messages, such as `failed to dial` while Redis is down, are JSON lines too, at
Warn with `"component":"go-redis"`.

### Metrics

The gateway serves Prometheus text format at `GET /metrics` on `metrics_port`
(9091 by default), a listener of its own that serves the metrics and nothing
else: it doesn't look at the path, so with the key every path answers with
them. It listens on `host`, like the main port, which no longer serves
`/metrics` and answers `404` there.

A scrape needs the admin key as a Bearer token:

```sh
curl -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" http://127.0.0.1:9091/metrics
```

Without the key, or with a tenant's key, it gets `401 invalid_admin_key`, as
any `/admin/` request does. The listener writes no request lines, since
Prometheus scrapes every few seconds. The admin key also creates tenants and
mints keys, so whatever holds it to scrape holds that power too: Prometheus
from `make obs-up` does (see [The dashboard](#the-dashboard)).

| Metric                                                    | Type      | What it counts                                                                                                                              |
| --------------------------------------------------------- | --------- | ------------------------------------------------------------------------------------------------------------------------------------------- |
| `gateway_requests_total{tenant,provider,model,status}`    | counter   | Requests on a provider route, when they end, by the status the client got. `401`–`403`, `413`, `429` and `503` are included.                |
| `gateway_request_duration_seconds{provider}`              | histogram | The same requests' whole time, a streamed body included.                                                                                    |
| `gateway_overhead_seconds{provider}`                      | histogram | The gateway's own time before a provider sees a request, once per request handed to a provider's transport. See [Timing](#timing).          |
| `gateway_upstream_ttfb_seconds{provider}`                 | histogram | Each attempt that got a response: from its handoff to the transport until the response headers. See [Timing](#timing).                      |
| `gateway_tokens_total{tenant,provider,model,type}`        | counter   | Tokens the providers reported, by `type`: `input`, `cached_input`, `cache_write`, `cache_write_1h` or `output`. A type of 0 adds no series. |
| `gateway_cost_micros_total{tenant,provider,model}`        | counter   | What responses cost in micro-dollars, as their usage rows have it. A cache hit, a free provider and a cost that isn't known add nothing.    |
| `gateway_cache_requests_total{tenant,provider,result}`    | counter   | The cache's decision: `hit`, `miss`, `bypass`, or `error` for a lookup that failed (the response header says `miss`).                       |
| `gateway_ratelimit_rejections_total{tenant,provider}`     | counter   | The gateway's own `429 rate_limited`; a provider's `429` isn't counted.                                                                     |
| `gateway_budget_rejections_total{tenant,provider,reason}` | counter   | Budget refusals: `reason` is `exceeded` (`402`), `unknown_model` (`400`) or `unavailable` (`503`).                                          |
| `gateway_budget_spent_micros{tenant}`                     | gauge     | The tenant's spend for the month, reservations in flight included. See [Budget gauges](#budget-gauges).                                     |
| `gateway_budget_micros{tenant}`                           | gauge     | The tenant's monthly budget. See [Budget gauges](#budget-gauges).                                                                           |
| `gateway_usage_writer_queue_depth`                        | gauge     | Usage rows waiting in the writer's queue, read at each scrape. The queue holds 10,000; a row that finds it full is dropped.                 |
| `gateway_usage_writer_dropped_total`                      | counter   | Usage rows the writer dropped, read at each scrape.                                                                                         |
| `gateway_concurrency_wait_seconds{provider}`              | histogram | Time spent waiting for a concurrency slot.                                                                                                  |
| `gateway_provider_up{provider}`                           | gauge     | `1` or `0`, from the health check.                                                                                                          |
| `gateway_fallbacks_total{from,to,reason}`                 | counter   | Fallbacks taken; `reason` is `unreachable`, `timeout` or `server_error`.                                                                    |

The histograms' buckets run from 1 ms to 600 s for the request duration, from
0.1 ms to 2.5 s for the overhead, from 25 ms to 600 s for the upstream TTFB,
and from 1 ms to 60 s for the concurrency wait. Tokens are labelled with the
provider that produced them, so a fallback's tokens count under `groq` while
its request counts under `ollama`.

### Metric labels

- **`provider`** is a provider's name in `config.yaml`.
  - On requests, duration, overhead, and the cache's and the rate limit's
    counters, it is the route the client called, even when a fallback answered.
  - On TTFB, tokens, cost and budget refusals, it is the attempt's provider,
    which after a fallback is the fallback's.
- **`tenant`** is the tenant's ID. It is `none` for a request refused before a
  tenant was known: a `401`, or `503 auth_unavailable`.
- **`model`** is the price table's name for the model, or `other`.
  - On requests, it is the model the client asked its route for, looked up in
    that provider's table as given and with any date suffix stripped. So
    `gpt-4o-2024-08-06` is `gpt-4o`, and a snapshot with a price of its own
    keeps its name.
  - On tokens and cost, it is looked up as the price is: the model the response
    names, then the one requested.
  - A free provider's models, every Ollama model included, aren't in the
    table, so they are `other`. So is a request that names no model.
- **`status`** is the status the client received. A `1xx` sent ahead of it
  doesn't count.

`tenant` and `model` are on counters, and `tenant` on the budget gauges. The
histograms keep `provider` alone, since each label value there multiplies about
15 buckets. A client can't add series: tenants come from the admin API, so
their number is the operator's, and a model name becomes a label value only if
the price table has it. A counter's series exists once a request has produced
it, so `gateway_requests_total` holds one for each combination of tenant,
provider, model and status that has occurred, and the series grow with the
number of tenants and the models and providers they use.

### Timing

`gateway_overhead_seconds` is the gateway's own work before a provider sees a
request: auth, the body read, the cache lookup, and the rate-limit and budget
checks. It runs from the request's arrival to the moment its first attempt is
handed to the provider's transport, before any wait for a concurrency slot.
That wait is `gateway_concurrency_wait_seconds`, and a failed first attempt is
a fallback's cost, so neither is overhead. It is observed once per request,
under the route; a cache hit, or a request refused before the handoff, has
none. The response path is not in it. That path only copies bytes through, and
the settlements hold the last bytes for at most a Redis call each; Week 8 is to
measure both against the mock.

`gateway_upstream_ttfb_seconds` times each attempt that got a response, of any
status: from its handoff to the transport, after any concurrency slot, to the
response headers. For a stream that is the time to the stream's start. For a
non-streaming request it is about the whole generation, since providers send
headers when they're done. An attempt that got no response (a failed dial, a
timeout, a client that left) isn't observed, and after a fallback each attempt
counts under its own provider.

### Budget gauges

`gateway_budget_spent_micros{tenant}` and `gateway_budget_micros{tenant}` are
set when a request to a paid provider ends, to the month's spend (settled costs
plus the reservations in flight) and the tenant's budget, as the ledger
reported them to that request: after its reservation, admitted or refused, and
after its settlement. The dashboard divides one by the other.
`GET /admin/tenants/{id}/budget` stays the exact figure. The gauges have these
limits:

- A tenant shows up once one of its requests to a paid provider has reached
  the ledger after the gateway starts. A free provider's request and a cache
  hit never reach it.
- A tenant idle since the month began keeps showing last month's figure until
  its next request.
- A settlement that finds the month missing, because Redis lost it, reports no
  spend. The gauge then keeps the figure its reservation reported, which
  includes that reservation, until the tenant's next request.
- The gauges show whichever of a tenant's requests ended last, not the newest
  figure in the ledger. So a request that began before a budget `PATCH` and
  ends after the next request sets `gateway_budget_micros` back to the old
  budget until the tenant's next request ends. Concurrent requests behave the
  same way.

### GET /ready

`GET /ready` on the main port says whether the gateway can serve. It pings
Postgres and Redis at the same time, and gives them a second together. It needs
no key, like `/health`. When both answer, it is `200`:

```json
{"status":"ready","checks":{"postgres":"ok","redis":"ok"}}
```

When one doesn't answer in time or fails, it is `503`, and names that check:

```json
{"status":"not_ready","checks":{"postgres":"ok","redis":"down"}}
```

The body never carries an error's text, since the endpoint needs no key: each
failure is logged at Warn, as `readiness check failed`, with the check's name
and the error. It never calls a provider, so a provider outage can't make it
fail. `/health` stays a liveness check that always answers `200`. A method other
than `GET` gets `405` with `Allow: GET`.

### The dashboard

`make obs-up` starts Prometheus and Grafana from the compose stack, with
Grafana's datasource and dashboard provisioned, and waits until both are
healthy. Run it in the shell the gateway runs in, so that both hold the same
admin key:

```sh
export GATEWAY_ADMIN_KEY=...   # the key the gateway runs with
make obs-up
```

- **Grafana** is on `127.0.0.1:3000`. Anyone who can reach the port may view it
  without a login, so the "LLM Gateway" dashboard opens at
  `http://127.0.0.1:3000/d/llm-gateway`. Its datasource and the dashboard are
  provisioned from `deployments/grafana/`, so the UI can't save over them. Its
  admin password comes from `GRAFANA_ADMIN_PASSWORD`, `admin` by default.
- **Prometheus** is on `127.0.0.1:9092`, because the mock provider holds 9090.
  It scrapes `host.docker.internal:9091` every 5 seconds, sending the admin key
  as a Bearer token from a compose secret that is filled from
  `GATEWAY_ADMIN_KEY`. The key is never in git or in a file in the repo.
- **The key.** `make obs-up` passes your `GATEWAY_ADMIN_KEY` through. When it is
  unset, it passes the mock's key, the one `make run-mock` uses, in that
  recipe only, so `make run` never gets a default key. With another key than
  the gateway's, the scrape gets a `401` and the target shows as down. Running
  `make obs-up` again with a different key makes Prometheus use it.
- `make obs-down` stops both; Postgres and Redis keep running.

The dashboard (`deployments/grafana/dashboard.json`) refreshes every 10 seconds
and shows the last 30 minutes. Its panels are:

- requests per second, by provider
- error rate by provider: 5xx, and the gateway's own refusals, apart
- latency p50, p95 and p99, by provider
- gateway overhead against upstream TTFB, p50, p95 and p99 of each
- cache hit rate: `hit` over `hit` plus `miss`, with bypasses and failed
  lookups left out
- token throughput, by type and by model
- budget utilization per tenant: spent over budget, with a tenant whose budget
  is 0 left out
- spend per hour by tenant
- a row of gateway health: refusals by kind, the usage writer's queue and
  drops, provider up, and fallbacks

![The LLM Gateway dashboard](deployments/grafana/dashboard.png)

*The Week 6 live run (see [Week 6 deliverables](#week-6-deliverables--observability-)),
from 40 seconds in until 15 seconds after it ended: 2026-11-01, 22:19 to 22:33
(UTC−5). Headless Chrome took it from the anonymous dashboard. The late
start keeps the one-minute rates from reaching back before the run.*

**Reaching a gateway on loopback.** Prometheus runs in a container and scrapes
`host.docker.internal`. The compose file maps that name to `host-gateway` for
it, which Colima doesn't need and Linux Docker does. Whether the container then
reaches a gateway bound to `127.0.0.1` depends on the runtime:

- **Colima:** it does. On 2026-10-31, on Colima's default profile (the macOS
  Virtualization.Framework VM, with no vmnet address), a container fetched from
  one listener on the Mac's `127.0.0.1` and from another on `0.0.0.0`, through
  `host.docker.internal` (192.168.5.2), with and without `host-gateway`. So the
  default `host: 127.0.0.1` works here. Other Colima setups weren't tested.
- **Linux Docker:** it doesn't. `host-gateway` resolves to the bridge IP, so a
  gateway bound to `127.0.0.1` isn't reachable from the Prometheus container.
  The gateway would have to listen on an address the bridge can reach, which
  means a `host` beyond loopback for both ports, since `metrics_port` listens
  on `host` too. Read the warning at the top first.
- In Week 7, when the gateway runs in the compose stack, Prometheus is to
  scrape `gateway:9091` over the compose network.

Some things to know about the stack:

- The first scrape comes about 10 seconds after `make obs-up` returns.
- Grafana doesn't re-read `dashboard.json` while it runs. Edit the file, then
  run `make obs-up`, which recreates both containers in about 10 seconds.
  Prometheus keeps its data: its anonymous volume is reattached.
- Plain `docker compose up` with `GATEWAY_ADMIN_KEY` unset fails, since
  Prometheus's secret needs it. `make db-up`, `docker compose config`, `ps` and
  `stop` don't.
- `docker compose down`, which `make db-down` runs, leaves Prometheus's
  anonymous `/prometheus` volume behind, one for each `down`.
- Grafana preinstalls plugins at every start, which needs the network. One
  download failed once with a `level=error` line. Setting
  `GF_PLUGINS_PREINSTALL_DISABLED=true` would stop it; the compose file doesn't
  set it.
