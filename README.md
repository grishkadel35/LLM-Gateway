# llm-gateway

A reverse proxy that sits between your applications and LLM providers (OpenAI,
Anthropic). Right now it forwards requests transparently and logs them. Rate
limiting, response caching, and budget enforcement plug in later.

## Project status

**Last updated:** 2026-09-19
**Stage:** Week 1 of 8 complete — transparent proxy working, no external
dependencies yet.

### What changed

- Tests for all four internal packages: proxy forwarding and header handling,
  the health endpoint and its routing, the logging middleware, and config
  loading and validation.
- Generated `go.sum` for the `gopkg.in/yaml.v3` dependency.
- Removed `llm-gateway/go.mod`, a nested duplicate of the root module file.
- Added this README and `.gitignore`.

### Week 1 deliverables

- [x] `cmd/gateway/main.go` — starts the HTTP server
- [x] `internal/proxy/proxy.go` — reverse proxy with Director, ModifyResponse,
      and ErrorHandler
- [x] `internal/config/config.go` — YAML config (port, upstream URL, timeout)
- [x] `internal/middleware/logging.go` — structured JSON request logging
- [x] `internal/health/health.go` — `GET /health` → `{"status":"ok"}`
- [x] `Makefile` — build, run, test targets
- [x] Tests: proxy forwarding, health endpoint, logging doesn't break responses
- [x] `go build`, `go vet ./...`, `go test ./...` all pass

The Week 1 checkpoint is "point any OpenAI client at `localhost:8080` and it
proxies transparently." That is verified with `curl` against a local fake
upstream — the `Authorization` header passes through untouched, the Host is
rewritten, `X-Forwarded-For` is set once, and `/health` is served locally
without touching the upstream. It has **not** yet been exercised against a real
provider with a live API key, or with a provider SDK rather than `curl`.

### Next: Week 2 — API keys and per-tenant tracking

Goal: every request is authenticated, and its token usage and dollar cost are
logged.

- Postgres schema: `tenants` and `usage_logs` tables
- `internal/tenant/tenant.go` — API key generation (`crypto/rand` + bcrypt)
- `internal/middleware/auth.go` — authenticate, attach the tenant to the
  request context
- `internal/tenant/usage.go` — parse the provider's `usage` field, compute cost
  from a pricing table, write to `usage_logs`
- `internal/pricing/pricing.go` — per-model pricing lookup
- `GET /admin/tenants/{id}/usage` — usage summary endpoint

This is the first week the gateway stops being dependency-free: it adds
Postgres and the `pgx` driver. Auth middleware runs *before* the proxy, so a
request with a bad key never reaches the provider; usage logging hooks into
`ModifyResponse`, where the token counts arrive.

## Quick start

```sh
make run
```

The gateway reads `config.yaml`, listens on port 8080, and forwards everything
to `https://api.openai.com`.

Send it a request the same way you'd send one to the provider, but point the
base URL at the gateway and keep your own API key:

```sh
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}'
```

The gateway does not hold credentials. It passes your `Authorization` header
through untouched, so any provider SDK works by overriding its base URL.

## Configuration

`config.yaml`:

```yaml
port: 8080                        # port the gateway listens on
upstream:
  url: https://api.openai.com     # provider to forward to
  timeout: 30                     # seconds to wait for response headers
```

Every key is optional; anything you leave out falls back to the default shown
above. Point `GATEWAY_CONFIG` at a different file to use one:

```sh
GATEWAY_CONFIG=config.local.yaml make run
```

Note that `timeout` bounds how long we wait for the upstream to *start*
replying, not the total request time. Streaming completions hold the connection
open for minutes, and a total-request cap would cut them off mid-generation.

## Endpoints

| Path      | Behavior                                              |
| --------- | ----------------------------------------------------- |
| `/health` | Returns `{"status":"ok"}`. Handled locally.           |
| `/*`      | Forwarded to the configured upstream.                 |

`/health` answers "is this process alive and serving?", which is what a load
balancer or container orchestrator needs. It deliberately does not check the
upstream — otherwise a provider outage would take the gateway out of rotation
even though it is working fine.

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
cmd/gateway/main.go       entry point: loads config, wires handlers, serves
internal/config/          YAML config loading and validation
internal/proxy/           the httputil.ReverseProxy and its hooks
internal/middleware/      request logging
internal/health/          GET /health
```

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
{"time":"2026-09-19T18:20:00Z","level":"INFO","msg":"request","method":"POST","path":"/v1/chat/completions","status":200,"duration_ms":842.11,"bytes":1204,"remote_addr":"127.0.0.1:52233"}
```

Plus one line per upstream response, recording the status, content length, and
whether the response was streamed.
