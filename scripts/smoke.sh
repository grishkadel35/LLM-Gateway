#!/usr/bin/env bash
# Smoke-tests the compose stack end to end: one request to each of the
# gateway's four paid providers, which the mock provider answers, then
# Prometheus, which should be scraping the gateway. CI's compose job runs it
# after `docker compose up -d --build --wait` and scripts/seed.sh.
#
#   SMOKE_KEY=<a tenant's key> scripts/smoke.sh
#
# seed-default's key will do. It is the third column of its line in seed.sh's
# stdout:
#
#   SMOKE_KEY=$(scripts/seed.sh | awk -F'\t' '$1 == "seed-default" { print $3 }') scripts/smoke.sh
#
# The checks run in this order, and the first one that fails ends the script
# with a message that names it, and the HTTP status or the missing field:
#
#   /openai     POST /openai/v1/chat/completions, streamed: 200, and a usage
#               object in one of the stream's events
#   /groq       POST /groq/v1/chat/completions: 200 and a usage object
#   /anthropic  POST /anthropic/v1/messages: 200 and a usage object
#   /gemini     POST /gemini/v1beta/models/...:generateContent: 200 and a
#               usageMetadata object
#   Prometheus  its target gateway:9091 is up, and gateway_requests_total has
#               samples
#
# Variables:
#   GATEWAY_URL     where the gateway listens (default http://127.0.0.1:8080)
#   SMOKE_KEY       a tenant's gateway key (required)
#   PROMETHEUS_URL  where Prometheus listens (default http://127.0.0.1:9092,
#                   where compose publishes it)
#
# Nothing goes to /ollama: it needs Ollama on the host, and CI has none.
#
# Prometheus scrapes every 5 s, so each Prometheus check polls, for up to 30 s.
# Its other target, host.docker.internal:9091, is the gateway on your Mac. It is
# normally down, and is not looked at.
#
# Needs curl and jq. Runs under bash 3.2 (macOS) and bash 5.

set -euo pipefail

GATEWAY_URL=${GATEWAY_URL:-http://127.0.0.1:8080}
GATEWAY_URL=${GATEWAY_URL%/}
PROMETHEUS_URL=${PROMETHEUS_URL:-http://127.0.0.1:9092}
PROMETHEUS_URL=${PROMETHEUS_URL%/}

note() {
  printf 'smoke.sh: %s\n' "$*" >&2
}

die() {
  note "error: $*"
  exit 1
}

for tool in curl jq; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is needed and wasn't found on PATH"
done

[ -n "${SMOKE_KEY:-}" ] || die "SMOKE_KEY isn't set; set it to a tenant's gateway key, such as seed-default's from scripts/seed.sh"

# post PATH BODY [CURL_ARG...]: POSTs the JSON BODY to the gateway, with any
# extra curl arguments, which carry the key in the header the provider's SDK
# would send it in. The body of a 200 reply is left in RESPONSE; anything else
# ends the script with a message.
post() {
  local path=$1 body=$2 out status
  shift 2

  out=$(curl -sS --max-time 30 -H 'Content-Type: application/json' "$@" -d "$body" -w '\n%{http_code}' "$GATEWAY_URL$path") ||
    die "POST $path: couldn't reach the gateway at $GATEWAY_URL"
  # -w put the status on a last line of its own.
  status=${out##*$'\n'}
  RESPONSE=${out%$'\n'*}

  [ "$status" = 200 ] || die "POST $path returned HTTP $status, not 200: $RESPONSE"
}

# The models are in pricing's table: a paid route answers a model it has no
# price for with 400 unknown_model, whatever the tenant's budget. max_tokens is
# small, so the hold a budget puts on a request stays small too. The mock
# accepts any model, and answers with the same usage whatever the request says.
OPENAI='{"model":"gpt-4o-mini","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}'
GROQ='{"model":"openai/gpt-oss-20b","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}'
ANTHROPIC='{"model":"claude-haiku-4-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}'
GEMINI='{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"maxOutputTokens":16}}'

# /openai is the one streamed, because it is the one where the gateway has a
# part in the usage: the mock sends usage in an OpenAI stream only when the
# request asks for it, with stream_options.include_usage, and this request
# doesn't. So a usage object in the events shows that the gateway added the
# option and passed the stream on whole. Groq, Anthropic and Gemini send usage
# in a stream whatever the gateway does. The events are `data: {json}` lines,
# and the last one is `data: [DONE]`.
post /openai/v1/chat/completions "$OPENAI" -H "Authorization: Bearer $SMOKE_KEY"
jq -R -n -e '[inputs | select(startswith("data: ")) | .[6:] | fromjson? | objects | .usage | objects] | length > 0' <<<"$RESPONSE" >/dev/null ||
  die "POST /openai/v1/chat/completions (streamed) returned HTTP 200, but none of its events has a usage object"
note "/openai (streamed): HTTP 200, with usage in the stream"

post /groq/v1/chat/completions "$GROQ" -H "Authorization: Bearer $SMOKE_KEY"
jq -e '.usage | objects' <<<"$RESPONSE" >/dev/null ||
  die "POST /groq/v1/chat/completions returned HTTP 200, but the reply has no usage object: $RESPONSE"
note "/groq: HTTP 200, with usage"

post /anthropic/v1/messages "$ANTHROPIC" -H "x-api-key: $SMOKE_KEY"
jq -e '.usage | objects' <<<"$RESPONSE" >/dev/null ||
  die "POST /anthropic/v1/messages returned HTTP 200, but the reply has no usage object: $RESPONSE"
note "/anthropic: HTTP 200, with usage"

post /gemini/v1beta/models/gemini-2.5-flash:generateContent "$GEMINI" -H "x-goog-api-key: $SMOKE_KEY"
jq -e '.usageMetadata | objects' <<<"$RESPONSE" >/dev/null ||
  die "POST /gemini/v1beta/models/gemini-2.5-flash:generateContent returned HTTP 200, but the reply has no usageMetadata object: $RESPONSE"
note "/gemini: HTTP 200, with usageMetadata"

# wait_prometheus WHAT PATH FILTER [CURL_ARG...]: GETs PATH from Prometheus
# once a second, for up to 30 s, until the jq FILTER prints ok for the reply.
# What FILTER prints instead says what it saw, and ends up in the message if
# 30 s pass without an ok.
wait_prometheus() {
  local what=$1 path=$2 filter=$3 deadline=$((SECONDS + 30)) reply seen
  shift 3

  while :; do
    reply=$(curl -sS --max-time 5 "$@" "$PROMETHEUS_URL$path" 2>/dev/null) || reply=
    seen=$(jq -r "$filter" <<<"$reply" 2>/dev/null) || seen=
    if [ "$seen" = ok ]; then
      return 0
    fi
    [ "$SECONDS" -lt "$deadline" ] || die "$what: not ok after 30 s; last seen: ${seen:-no usable reply from $PROMETHEUS_URL$path}"
    sleep 1
  done
}

wait_prometheus "Prometheus target gateway:9091" /api/v1/targets '
  [.data.activeTargets[]? | select(.labels.instance == "gateway:9091")][0]
  | if . == null then "no such active target"
    elif .health == "up" then "ok"
    else "health \(.health), last error: \(.lastError)" end'
note "Prometheus: target gateway:9091 is up"

wait_prometheus "Prometheus gateway_requests_total" /api/v1/query \
  'if (.data.result | length) > 0 then "ok" else "no samples" end' \
  -G --data-urlencode 'query=gateway_requests_total'
note "Prometheus: gateway_requests_total has samples"

note "ok: /openai (streamed), /groq, /anthropic, /gemini and Prometheus all passed"
