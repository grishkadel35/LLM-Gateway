# llm-gateway

A reverse proxy that sits between your applications and LLM providers. It routes
each request to the right provider, presents that provider's API key in whatever
header it expects, and logs the result. Rate limiting, response caching, and
budget enforcement plug in later.

Providers speak their own native APIs — the gateway never translates request or
response bodies, so new provider features work the day they ship. Adding a
provider is a block of YAML, not code.

> **⚠️ Not safe to expose yet.** The gateway holds real provider API keys but has
> no tenant authentication (that arrives in Week 2). Anything that can reach the
> port can spend those keys, so it binds to `127.0.0.1` by default. Don't change
> that until auth exists.

## Project status

**Last updated:** 2026-10-05
**Stage:** Week 1 of 8 complete and checkpoint closed, plus multi-provider
routing. Week 2 in progress: the mock provider, the dev stack (Postgres,
migrations, CI), tenant key management, the auth middleware and request IDs
are done. The gateway itself doesn't use the database yet.

### What changed

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
  context; anything else gets a 401 and never reaches the provider. A failed
  lookup (database down) is a 503, not a 401. Gemini's `?key=` is not
  accepted: a key in a URL ends up in proxy logs. It isn't wired into the
  gateway yet: that lands with the admin routes, so there is never a gateway
  that demands keys with no way to issue one.
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

### In progress: Week 2 — API keys and per-tenant tracking

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
  request context (wired into the gateway together with the admin routes)
- [x] Request IDs — an `X-Request-ID` per request, returned to the client and
  logged (stored with its usage row, next to the provider's own ID, once usage
  logging lands)
- [ ] A `format` field per provider (`openai`, `anthropic`, `gemini`), with a
  usage parser for each
- [ ] `internal/usage` — reads token counts from the response as it streams past,
  without buffering it, and writes `usage_logs` rows in async batches
- [ ] `internal/pricing` — per-model prices in integer micro-dollars
- [ ] Admin routes behind a separate admin key: `POST /admin/tenants`,
  issuing and revoking keys, `GET /admin/tenants/{id}/usage`

This is the first week the gateway stops being dependency-free: it adds
Postgres and the `pgx` driver. Auth middleware runs *before* the proxy, so a
request with a bad key never reaches the provider. Usage is counted by
wrapping the response body in `ModifyResponse`, so streaming responses are
metered too; otherwise `stream: true` would bypass every future limit.

## Quick start

Export a key for each provider in `config.yaml`, then run:

```sh
export OPENAI_API_KEY=sk-...
export ANTHROPIC_API_KEY=sk-ant-...
export GEMINI_API_KEY=...
export GROQ_API_KEY=gsk_...

make run
```

A key that isn't set is a startup error, not a surprise 401 on the first
request. To run with fewer providers, delete the ones you don't need from
`config.yaml`.

Then point your client at `/<provider>` and use that provider's own API. You
send **no API key** — the gateway attaches its own:

```sh
curl http://127.0.0.1:8080/openai/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}'

curl http://127.0.0.1:8080/anthropic/v1/messages \
  -H "Content-Type: application/json" \
  -d '{"model":"claude-sonnet-4","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}'
```

With an SDK, override the base URL:

```python
from openai import OpenAI

# The api_key argument is required by the SDK but ignored by the gateway,
# which strips it and substitutes its own.
client = OpenAI(base_url="http://127.0.0.1:8080/openai/v1", api_key="unused")
```

### Without API keys

`cmd/mockprovider` serves fake OpenAI, Groq, Anthropic and Gemini APIs on one port,
and `config.mock.yaml` points the gateway's providers at it. Nothing leaves the
machine and nothing costs money:

```sh
make mock       # terminal 1: mock provider on 127.0.0.1:9090
make run-mock   # terminal 2: gateway on 127.0.0.1:8080
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

Postgres runs in Docker (any runtime works; on macOS, `brew install colima
docker docker-compose` then `colima start` is enough):

```sh
make db-up      # Postgres 18 on 127.0.0.1:5432, waits until healthy
make migrate    # apply pending migrations
make db-down    # stop it; data survives in the llm-gateway_pgdata volume
```

Migrations live in `internal/db/migrations` and are embedded into the binary,
so a build always carries the schema its code expects. `make migrate` targets
the compose database; pass `DATABASE_URL=postgres://...` to use another.

The Postgres tests only run when `DATABASE_URL` is set, and each creates and
drops its own throwaway database, so they never touch dev data:

```sh
DATABASE_URL='postgres://gateway:gateway@127.0.0.1:5432/gateway?sslmode=disable' go test -race ./...
```

Without it, `go test ./...` skips them and needs nothing running.

## Configuration

`config.yaml`:

```yaml
port: 8080          # port the gateway listens on
host: 127.0.0.1     # bind address — loopback until tenant auth exists

providers:
  openai:
    url: https://api.openai.com
    timeout: 600                 # seconds to wait for response headers
    key: ${OPENAI_API_KEY}       # environment reference, never a literal
    auth: bearer                 # bearer | x-api-key | x-goog-api-key

  anthropic:
    url: https://api.anthropic.com
    key: ${ANTHROPIC_API_KEY}
    auth: x-api-key
    headers:                     # static headers this provider requires
      anthropic-version: "2023-06-01"
```

`port`, `host` and each provider's `timeout` are optional. `url`, `key` and
`auth` are required per provider.

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
— same `auth: bearer` as OpenAI, different base URL:

```yaml
  groq:
    url: https://api.groq.com/openai   # base path is preserved
    key: ${GROQ_API_KEY}
    auth: bearer
```

Providers needing request signing rather than a static header — AWS Bedrock's
SigV4, Vertex AI's OAuth — can't be expressed this way and would need code.

## Endpoints

| Path              | Behavior                                                        |
| ----------------- | --------------------------------------------------------------- |
| `/health`         | Returns `{"status":"ok"}`. Handled locally.                     |
| `/<provider>/...` | Prefix stripped, forwarded to that provider with its own key.   |
| anything else     | `404` with a JSON body listing the configured providers.        |

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
| `make db-up`    | Start local Postgres (docker compose)  |
| `make db-down`  | Stop local Postgres                    |
| `make migrate`  | Apply pending database migrations      |
| `make test`     | Run all tests                          |
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
cmd/migrate/main.go       applies migrations to DATABASE_URL
deployments/              docker compose dev stack (Postgres)
.github/workflows/ci.yml  vet + race-enabled tests against Postgres
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
