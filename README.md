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

**Last updated:** 2026-09-20
**Stage:** Week 1 of 8 complete, plus multi-provider routing. No external
service dependencies yet.

### What changed

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
- [x] `internal/proxy/proxy.go` — reverse proxy with Director, ModifyResponse,
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

It has **not** yet been exercised against a real provider with a live API key,
or with a provider SDK rather than `curl`. That's the one remaining step to
close the checkpoint.

### Next: Week 2 — API keys and per-tenant tracking

Goal: every request is authenticated, and its token usage and dollar cost are
logged — including streamed responses.

- `cmd/mockprovider` — fake OpenAI, Anthropic and Gemini upstreams, streaming
  and non-streaming, so nothing below costs money to test
- Postgres (via docker-compose) with `goose` migrations: `tenants` and
  `usage_logs` tables; CI running `go vet` and `go test`
- `internal/tenant` — `gw_`-prefixed random keys, stored as SHA-256 for an
  indexed lookup
- `internal/middleware/auth.go` — reads the gateway key from the client SDK's
  native credential header and attaches the tenant to the request context
- A `format` field per provider (`openai`, `anthropic`, `gemini`), with a
  usage parser for each
- `internal/usage` — reads token counts from the response as it streams past,
  without buffering it, and writes `usage_logs` rows in async batches
- `internal/pricing` — per-model prices in integer micro-dollars
- Admin routes behind a separate admin key: `POST /admin/tenants`,
  `GET /admin/tenants/{id}/usage`

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

## Configuration

`config.yaml`:

```yaml
port: 8080          # port the gateway listens on
host: 127.0.0.1     # bind address — loopback until tenant auth exists

providers:
  openai:
    url: https://api.openai.com
    timeout: 30                  # seconds to wait for response headers
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
a literal key there would be published. Expansion happens only on the `key`
field, so other values containing `$` are left alone.

Point `GATEWAY_CONFIG` at a different file to use one:

```sh
GATEWAY_CONFIG=config.local.yaml make run
```

`timeout` bounds how long we wait for a provider to *start* replying, not total
request time. Streaming completions hold the connection open for minutes, and a
total-request cap would cut them off mid-generation.

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

| Command      | What it does                          |
| ------------ | ------------------------------------- |
| `make build` | Compile to `./bin/gateway`            |
| `make run`   | Build, then run                       |
| `make test`  | Run all tests                         |
| `make vet`   | Run `go vet` (catches suspicious code)|
| `make fmt`   | Format all source files               |
| `make tidy`  | Sync `go.mod` / `go.sum`              |
| `make clean` | Remove `./bin`                        |
| `make all`   | fmt, vet, test, build                 |

## Layout

```
cmd/gateway/main.go       entry point: loads config, builds routes, serves
internal/config/          YAML config loading and validation
internal/provider/        per-provider auth styles and header handling
internal/proxy/           the httputil.ReverseProxy and its hooks
internal/middleware/      request logging
internal/health/          GET /health
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
{"time":"2026-09-20T18:20:00Z","level":"INFO","msg":"request","method":"POST","path":"/openai/v1/chat/completions","status":200,"duration_ms":842.11,"bytes":1204,"remote_addr":"127.0.0.1:52233"}
```

Plus one line per upstream response, recording which provider answered, the
status, content length, and whether the response was streamed.
