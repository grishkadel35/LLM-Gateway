# llm-gateway

A reverse proxy that sits between your applications and LLM providers. It routes
each request to the right provider, presents that provider's API key in whatever
header it expects, and logs the result. Rate limiting, response caching, and
budget enforcement plug in later.

Providers speak their own native APIs — the gateway never translates request or
response bodies, so new provider features work the day they ship. Adding a
provider is a block of YAML, not code.

> **⚠️ Keep it on loopback for now.** Every provider route requires a tenant
> key, but there is no TLS, rate limiting or budget enforcement yet, so the
> gateway binds to `127.0.0.1` by default. Binding wider is your call to make
> deliberately.

## Project status

**Last updated:** 2026-10-05
**Stage:** Weeks 1 and 2 of 8 complete, checkpoints closed. Every provider
request is authenticated with a tenant key, and every request that reaches a
provider, streamed or not, is recorded in Postgres with its tokens and cost,
verified live against Groq and Gemini. Week 3 (token-aware rate limiting) is
next.

### What changed

- **Token bucket in Redis (not wired in yet).** `internal/ratelimit` keeps each
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
database is a startup error, not a surprise on the first request. To run with
fewer providers, delete the ones you don't need from `config.yaml`.

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

Redis holds the rate-limit buckets (Week 3). It has no volume, because the
buckets are disposable: losing them only refills every tenant. It has no
password either, so binding to loopback is what keeps it private.

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

providers:
  openai:
    url: https://api.openai.com
    timeout: 600                 # seconds to wait for response headers
    key: ${OPENAI_API_KEY}       # environment reference, never a literal
    auth: bearer                 # bearer | x-api-key | x-goog-api-key
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
`timeout` are optional. `url`, `key`,
`auth` and `format` are required per provider.

`format` is the shape of the provider's API, which decides how the gateway
reads token usage from its responses. It names a wire shape, not a company:
Groq and other OpenAI-compatible providers are `openai`.

**Keys are always environment references.** `config.yaml` is committed to git;
a literal key there would be published, so `key` must be exactly one `${VAR}`
and anything else is a startup error. Expansion happens only on the `key`
field, so other values containing `$` are left alone. Unknown fields are
startup errors too, so a typo can't silently fall back to a default.

Point `GATEWAY_CONFIG` at a different file to use one:

```sh
GATEWAY_CONFIG=config.local.yaml make run
```

`timeout` bounds how long we wait for a provider to *start* replying, not total
request time. Streaming completions hold the connection open for minutes, and a
total-request cap would cut them off mid-generation. A non-streaming reply
sends no headers until the whole generation is done, which is why the default
is 600 seconds, the same as the official SDKs. A timeout returns `504`; an
unreachable provider returns `502`.

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

## Endpoints

| Path                              | Behavior                                                                                  |
| --------------------------------- | ----------------------------------------------------------------------------------------- |
| `GET /health`                     | Returns `{"status":"ok"}`. Handled locally, no key needed.                                |
| `/<provider>/...`                 | Needs a gateway key (`401` otherwise) and a metered endpoint (`403` otherwise, see below). Prefix stripped, forwarded with the provider's key. |
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
balancer or container orchestrator needs. It deliberately does not check the
providers — otherwise one provider's outage would take the gateway out of
rotation even though it is working fine.

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
internal/proxy/           the httputil.ReverseProxy and its hooks
internal/middleware/      request IDs, request logging, tenant key auth
internal/health/          GET /health
internal/mockprovider/    fake OpenAI/Groq/Anthropic/Gemini APIs for testing
cmd/mockprovider/main.go  serves the mock on 127.0.0.1:9090
internal/db/              Postgres schema: embedded goose migrations
internal/db/dbtest/       throwaway Postgres database for tests
internal/tenant/          tenants and API keys: issue, revoke, look up
internal/ratelimit/       per-tenant token bucket in Redis (Lua scripts)
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
client. That is where usage accounting and caching will read token counts.
Returning an error from it causes `ErrorHandler` to run instead of the response
being forwarded — that's the mechanism for rejecting a request over budget.

Rate limiting and budget checks belong *before* the proxy, as middleware
alongside `Logging` in `main.go`, so a rejected request never reaches the
provider.

## Observability

Each request produces one JSON line on stdout:

```json
{"time":"2026-09-20T18:20:00Z","level":"INFO","msg":"request","request_id":"req_2d10cec6dc4299d10f08dbf141ebffb9","method":"POST","path":"/openai/v1/chat/completions","status":200,"duration_ms":842.11,"bytes":1204,"remote_addr":"127.0.0.1:52233"}
```

`request_id` is the `X-Request-ID` returned to the client. When the client
sent its own `X-Request-ID`, it is logged as `client_request_id`.

Plus one line per upstream response, recording which provider answered, the
status, content length, and whether the response was streamed.
