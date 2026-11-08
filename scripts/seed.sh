#!/usr/bin/env bash
# Creates three test tenants through the gateway's admin API and prints a key
# for each, so there is something to send requests with:
#
#   seed-default   the column defaults
#   seed-limited   a rate limit low enough that a few small requests get a 429
#   seed-budget    a budget small enough that a request without max_tokens gets
#                  a 402, while one with a small max_tokens gets a 200
#
# Each tenant is created with POST /admin/tenants, which also issues its key,
# and given its limits with PATCH /admin/tenants/{id} (seed-default keeps the
# defaults, so it has none).
#
# stdout is one line per tenant, name<TAB>tenant_id<TAB>key, and nothing else,
# so a script can `cut -f3` the key out. Progress, notes and an example curl go
# to stderr.
#
# Variables:
#   GATEWAY_URL        where the gateway listens (default http://127.0.0.1:8080)
#   GATEWAY_ADMIN_KEY  the admin key (default: the one `make run-mock` uses, with
#                      a note on stderr)
#
# It waits up to 60 s for GET /ready to answer 200. It exits 1, saying why,
# when the gateway never does, when the admin key is rejected (401), and on any
# other non-2xx reply, whose body it shows.
#
# It is not idempotent: tenant names aren't unique and there is no list
# endpoint, so a second run makes three more tenants.
#
# Needs curl and jq. Runs under bash 3.2 (macOS) and bash 5.

set -euo pipefail

# The limits below are sized for the mock provider (`make mock`), which reports
# 30 tokens for every request (20 in, 10 out), and for gpt-4o-mini on /openai.
#
# seed-limited, 100 tokens a minute: the bucket holds 100. A request is charged
# its estimate up front (its input plus its max_tokens, or default_max_tokens
# when it sets none, capped at the limit), then refunded down to the 30 tokens
# the mock reports. With max_tokens 16 the estimate is about 37, so three
# requests pass (70, 40, then 10 tokens left) and the fourth gets a 429. With
# no max_tokens the estimate is the whole limit, which needs a full bucket, so
# the second request gets the 429.
LIMITED='{"rate_limit_tokens_per_min": 100}'

# seed-budget, 1000 micro-dollars ($0.001) a month: a request is reserved at
# its worst case. With no max_tokens that is gpt-4o-mini's whole 16,384-token
# output, about 9,834 micro-dollars, ten times the budget, so it gets a 402.
# With max_tokens 16 it is about 14, and the request settles at the 9 it cost,
# so the budget lasts about a hundred of them.
BUDGET='{"budget_micros": 1000}'

GATEWAY_URL=${GATEWAY_URL:-http://127.0.0.1:8080}
GATEWAY_URL=${GATEWAY_URL%/}

note() {
  printf 'seed.sh: %s\n' "$*" >&2
}

die() {
  note "error: $*"
  exit 1
}

for tool in curl jq; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is needed and wasn't found on PATH"
done

if [ -z "${GATEWAY_ADMIN_KEY:-}" ]; then
  GATEWAY_ADMIN_KEY=mock-admin-key-for-local-testing-only
  note "GATEWAY_ADMIN_KEY isn't set; using the mock setup's admin key"
fi

# Waits up to 60 s for GET /ready to answer 200.
wait_ready() {
  local deadline=$((SECONDS + 60)) status
  note "waiting up to 60 s for $GATEWAY_URL/ready"
  while :; do
    # curl prints 000 when nothing answers, so `|| true` only keeps set -e
    # from ending the script on its non-zero exit.
    status=$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 "$GATEWAY_URL/ready" || true)
    if [ "$status" = 200 ]; then
      return 0
    fi
    if [ "$SECONDS" -ge "$deadline" ]; then
      if [ "$status" = 000 ]; then
        status="no answer at all"
      else
        status="HTTP $status"
      fi
      die "GET $GATEWAY_URL/ready never answered 200 in 60 s (last try: $status). Is the gateway running?"
    fi
    sleep 1
  done
}

# admin METHOD PATH [JSON]: calls the admin API. The body of a 2xx reply is left
# in RESPONSE; anything else ends the script with a message.
admin() {
  local method=$1 path=$2 body=${3:-} out status
  local args=(-sS --max-time 30 -X "$method" -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" -w '\n%{http_code}')
  if [ -n "$body" ]; then
    args+=(-H 'Content-Type: application/json' -d "$body")
  fi

  out=$(curl "${args[@]}" "$GATEWAY_URL$path") || die "$method $path: couldn't reach the gateway at $GATEWAY_URL"
  # -w put the status on a last line of its own.
  status=${out##*$'\n'}
  RESPONSE=${out%$'\n'*}

  case $status in
    2??) ;;
    401) die "the gateway rejected the admin key (HTTP 401 on $method $path); set GATEWAY_ADMIN_KEY to the key the gateway runs with" ;;
    *) die "$method $path returned HTTP $status: $RESPONSE" ;;
  esac
}

# seed_tenant NAME [LIMITS]: creates the tenant, with its key, applies LIMITS
# (a PATCH body) if there are any, and prints its stdout line.
seed_tenant() {
  local name=$1 limits=${2:-} payload id key

  payload=$(jq -nc --arg name "$name" '{name: $name}')
  admin POST /admin/tenants "$payload"
  id=$(jq -er '.tenant.id' <<<"$RESPONSE") || die "no tenant ID in the reply to POST /admin/tenants"
  key=$(jq -er '.key.plaintext' <<<"$RESPONSE") || die "no key in the reply to POST /admin/tenants"
  note "$name: tenant $id created, with its key"

  if [ -n "$limits" ]; then
    admin PATCH "/admin/tenants/$id" "$limits"
    note "$name: limits set: $limits"
  fi

  printf '%s\t%s\t%s\n' "$name" "$id" "$key"
}

wait_ready
seed_tenant seed-default
seed_tenant seed-limited "$LIMITED"
seed_tenant seed-budget "$BUDGET"

cat >&2 <<EOF
seed.sh: done. Try a key (KEY is the third column of a line above):

  curl -sS $GATEWAY_URL/openai/v1/chat/completions \\
    -H "Authorization: Bearer \$KEY" \\
    -H 'Content-Type: application/json' \\
    -d '{"model":"gpt-4o-mini","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}'
EOF
