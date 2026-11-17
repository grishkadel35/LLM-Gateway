#!/usr/bin/env bash
# Benchmarks the gateway against the mock provider and writes the results to
# bench/results/<YYYY-MM-DD>/ (a -quick run: bench/results/quick-<YYYY-MM-DD>/,
# which .gitignore leaves out):
#
#   make bench                # the full run, about 6 minutes
#   make bench ARGS=-quick    # a smoke run, about 75 seconds
#
# make passes the compose database and Redis URLs and the mock admin key, and
# Postgres and both Redis must be up (make db-up migrate). Run by hand, set
# DATABASE_URL, REDIS_URL, BUDGET_REDIS_URL and GATEWAY_ADMIN_KEY first.
#
# It builds the gateway, the mock provider (-chunk-delay 50ms) and cmd/bench
# into a temp dir, starts the mock and the gateway (config.mock.yaml) and stops
# both when it exits, however it exits. Ports 8080, 9090 and 9091 must be free.
#
# Then:
#   1. hey, on a fresh tenant: the gateway on /openai, cache off, and the mock
#      directly, each at 10, 50, 100 and 200 workers, then the gateway with the
#      cache on, at 50. Each run lasts 30 s (-quick: 5 s) after a short warm-up
#      run that is thrown away. hey's text summary is kept as
#      hey-<target>-c<N>.txt; there is no CSV, which for 30 s of requests would
#      be hundreds of thousands of rows.
#   2. The gateway's /metrics, saved as metrics.txt for gateway_overhead_seconds,
#      and the usage writer's dropped-rows counter, as usage-writer-dropped.txt.
#   3. cmd/bench's ttfb, ratelimit, budget and cache, each as <name>.json.
#   4. env.txt: when, what commit, and on what machine.
#
# It ends with one table: each hey run's requests/s, p50, p99 and number of
# non-200 responses and errors, so a run that was mostly 429s can't pass for a
# fast one (a run with some doesn't fail the script; it is shown), and each
# subcommand's PASS or FAIL. The exit status is 1 if any subcommand failed.
#
# Needs hey (go install github.com/rakyll/hey@v0.1.4), curl and jq. Runs under
# bash 3.2 (macOS) and bash 5.

set -euo pipefail

GATEWAY=http://127.0.0.1:8080
MOCK=http://127.0.0.1:9090
METRICS=http://127.0.0.1:9091

note() {
  printf 'bench.sh: %s\n' "$*" >&2
}

die() {
  note "error: $*"
  exit 1
}

QUICK=0
case "${1:-}" in
  '') ;;
  -quick) QUICK=1 ;;
  *) die "unknown argument '$1'; the only one is -quick" ;;
esac
[ $# -le 1 ] || die "too many arguments; the only one is -quick"

# hey installs into GOPATH/bin, which is often not on PATH.
PATH=$PATH:$(go env GOPATH 2>/dev/null || true)/bin

for tool in curl jq go; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is needed and wasn't found on PATH"
done
command -v hey >/dev/null 2>&1 ||
  die "hey is needed and wasn't found on PATH; install it with: go install github.com/rakyll/hey@v0.1.4"

for v in DATABASE_URL REDIS_URL BUDGET_REDIS_URL GATEWAY_ADMIN_KEY; do
  [ -n "${!v:-}" ] || die "$v isn't set; run this as make bench, which sets it"
done
export DATABASE_URL REDIS_URL BUDGET_REDIS_URL GATEWAY_ADMIN_KEY
export MOCK_API_KEY=mock

# Before anything is started, so that failing here leaves nothing behind.
for port in 8080 9090 9091; do
  if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
    die "port $port is already in use; stop what is listening there (lsof -nP -iTCP:$port -sTCP:LISTEN)"
  fi
done

cd "$(dirname "$0")/.."

DATE=$(date +%Y-%m-%d)
if [ "$QUICK" = 1 ]; then
  OUT=bench/results/quick-$DATE
  HEY_SECS=5
  WARM_SECS=1
  TTFB_N=10
  CACHE_FLAGS="-n 200 -prompts 50"
else
  OUT=bench/results/$DATE
  HEY_SECS=30
  WARM_SECS=2
  TTFB_N=50
  CACHE_FLAGS="-n 1000 -prompts 200"
fi
mkdir -p "$OUT"

TMP=$(mktemp -d)
PIDS=""
cleanup() {
  local pid
  for pid in $PIDS; do
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  done
  rm -rf "$TMP"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

note "building into $TMP"
go build -o "$TMP/gateway" ./cmd/gateway
go build -o "$TMP/mockprovider" ./cmd/mockprovider
go build -o "$TMP/bench" ./cmd/bench

# start NAME CMD...: runs CMD in the background, its output in $TMP/NAME.log.
start() {
  local name=$1
  shift
  "$@" >"$TMP/$name.log" 2>&1 &
  PIDS="$PIDS $!"
  LAST_PID=$!
}

start mock "$TMP/mockprovider" -chunk-delay 50ms
MOCK_PID=$LAST_PID
start gateway env GATEWAY_CONFIG=config.mock.yaml "$TMP/gateway"
GATEWAY_PID=$LAST_PID

# The gateway exits when Postgres or Redis can't be reached, and answers /ready
# with an error when one goes away after it started.
note "waiting for the gateway's /ready"
ready=0
for _ in $(seq 1 60); do
  kill -0 "$GATEWAY_PID" 2>/dev/null || break
  kill -0 "$MOCK_PID" 2>/dev/null || break
  if curl -fsS -o /dev/null "$GATEWAY/ready" 2>/dev/null; then
    ready=1
    break
  fi
  sleep 0.5
done
if [ "$ready" != 1 ]; then
  tail -n 20 "$TMP/gateway.log" "$TMP/mock.log" >&2 || true
  die "the gateway didn't become ready: are Postgres and both Redis up and migrated? (make db-up migrate)"
fi

# The worst case sets the tenant's limits: no run sends more than CEIL
# requests a second, and each costs at most REQ_TOKENS tokens against the rate
# limit (its input estimate plus max_tokens 16) and REQ_MICROS against the
# budget (gpt-4o-mini's output price is 0.6 micro-dollars a token). The
# hey runs last the warm-ups and the runs, plus one second to spare each.
CEIL=50000
REQ_TOKENS=200
REQ_MICROS=200
GATEWAY_RUNS=5
SECS=$((GATEWAY_RUNS * (HEY_SECS + WARM_SECS + 1)))
RATE=$((CEIL * REQ_TOKENS * 60))
BUDGET=$((CEIL * SECS * REQ_MICROS))
note "bench tenant: rate_limit_tokens_per_min=$RATE (at most $CEIL requests/s of $REQ_TOKENS tokens), budget_micros=$BUDGET (at most $CEIL requests/s for ${SECS}s, $REQ_MICROS micros each)"

admin() {
  curl -fsS -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" -H 'Content-Type: application/json' "$@"
}
created=$(admin -d "{\"name\":\"bench-hey-$(date +%s)\"}" "$GATEWAY/admin/tenants") ||
  die "creating the bench tenant failed"
TENANT_ID=$(jq -er .tenant.id <<<"$created")
KEY=$(jq -er .key.plaintext <<<"$created")
admin -X PATCH -d "{\"rate_limit_tokens_per_min\":$RATE,\"budget_micros\":$BUDGET}" \
  "$GATEWAY/admin/tenants/$TENANT_ID" >/dev/null || die "setting the bench tenant's limits failed"

BODY='{"model":"gpt-4o-mini","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}'

# hey_run LABEL C SECS URL [HEY_ARG...]: one run, its summary in
# $OUT/hey-LABEL-cC.txt. A SECS of $WARM_SECS is a warm-up: the file is
# removed.
hey_run() {
  local label=$1 c=$2 secs=$3 url=$4 file
  shift 4
  file=$OUT/hey-$label-c$c.txt
  hey -z "${secs}s" -c "$c" -m POST -T application/json -d "$BODY" "$@" "$url" >"$file" 2>&1 ||
    die "hey failed; see $file"
}

# hey_row LABEL C: adds a row of the table: requests/s, p50 and p99 in ms, and
# the responses that weren't 200 plus the errors, from hey's summary.
ROWS=$TMP/rows
hey_row() {
  local label=$1 c=$2
  awk -v name="$label c=$c" '
    /^  Requests\/sec:/ { rps = $2 }
    /^  50% in/ { p50 = $3 }
    /^  99% in/ { p99 = $3 }
    /^Status code distribution:/ { sec = "s"; next }
    /^Error distribution:/ { sec = "e"; next }
    /^$/ { sec = "" }
    sec == "s" && /^ *\[/ { if ($1 != "[200]") bad += $2 }
    sec == "e" && /^ *\[/ { n = $1; gsub(/[^0-9]/, "", n); bad += n }
    END { printf "%s\t%.0f\t%.2f\t%.2f\t%d\n", name, rps, p50 * 1000, p99 * 1000, bad }
  ' "$OUT/hey-$label-c$c.txt" >>"$ROWS"
}

AUTH="Authorization: Bearer $KEY"
CONCURRENCIES="10 50 100 200"

note "warming up"
hey_run warmup 10 "$WARM_SECS" "$GATEWAY/openai/v1/chat/completions" -H "$AUTH" -H 'X-Gateway-Cache: off'
hey_run warmup 10 "$WARM_SECS" "$MOCK/v1/chat/completions" -H 'Authorization: Bearer mock'
rm -f "$OUT/hey-warmup-c10.txt"

for c in $CONCURRENCIES; do
  note "hey: the gateway, cache off, $c workers, ${HEY_SECS}s"
  hey_run gateway "$c" "$HEY_SECS" "$GATEWAY/openai/v1/chat/completions" -H "$AUTH" -H 'X-Gateway-Cache: off'
  hey_row gateway "$c"
done

for c in $CONCURRENCIES; do
  note "hey: the mock directly, $c workers, ${HEY_SECS}s"
  hey_run mock "$c" "$HEY_SECS" "$MOCK/v1/chat/completions" -H 'Authorization: Bearer mock'
  hey_row mock "$c"
done

# The first request stores the answer; the second one has to be a hit, or the
# run below would measure misses.
note "hey: the gateway, cache on, 50 workers, ${HEY_SECS}s"
curl -fsS -o /dev/null -H "$AUTH" -H 'Content-Type: application/json' -H 'X-Gateway-Cache: on' \
  -d "$BODY" "$GATEWAY/openai/v1/chat/completions" || die "the cache warm-up request failed"
curl -fsS -o /dev/null -D "$TMP/cache-headers" -H "$AUTH" -H 'Content-Type: application/json' \
  -H 'X-Gateway-Cache: on' -d "$BODY" "$GATEWAY/openai/v1/chat/completions" ||
  die "the cache warm-up request failed"
grep -qi '^x-gateway-cache: hit' "$TMP/cache-headers" ||
  die "the second identical request wasn't a cache hit; the cache-hit run would measure misses"
hey_run gateway-cache 50 "$HEY_SECS" "$GATEWAY/openai/v1/chat/completions" -H "$AUTH" -H 'X-Gateway-Cache: on'
hey_row gateway-cache 50

# The histogram counts every request the gateway has served, the warm-ups
# included. The dropped counter is the usage rows the writer had no room for.
note "scraping $METRICS/metrics"
curl -fsS -H "Authorization: Bearer $GATEWAY_ADMIN_KEY" "$METRICS/metrics" >"$OUT/metrics.txt" ||
  die "scraping $METRICS/metrics failed"
DROPPED=$(awk '$1 == "gateway_usage_writer_dropped_total" { print $2 }' "$OUT/metrics.txt")
[ -n "$DROPPED" ] || die "gateway_usage_writer_dropped_total isn't in the metrics"
echo "$DROPPED" >"$OUT/usage-writer-dropped.txt"

# One subcommand's failing doesn't stop the others; the status is kept for the
# table. 1 is a failed check, 2 a setup error.
RESULTS=$TMP/results
bench() {
  local name=$1 status=0
  shift
  note "bench $name"
  "$TMP/bench" "$name" "$@" -out "$OUT/$name.json" || status=$?
  case $status in
    0) echo "$name PASS" >>"$RESULTS" ;;
    1) echo "$name FAIL" >>"$RESULTS" ;;
    *) echo "$name ERROR" >>"$RESULTS" ;;
  esac
}
# CACHE_FLAGS is left unquoted to be split into flags.
bench ttfb -n "$TTFB_N" -chunk-delay 50ms
bench ratelimit -allow 10 -burst 30
bench budget -room 10 -n 50
# shellcheck disable=SC2086
bench cache $CACHE_FLAGS -c 20 -seed 1

{
  echo "date: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
  echo "commit: $(git rev-parse HEAD)"
  # The run's own results don't make the tree dirty.
  if [ -n "$(git status --porcelain -- . ':!bench/results')" ]; then echo "dirty tree: yes"; else echo "dirty tree: no"; fi
  echo "go: $(go version)"
  echo "cpu: $(sysctl -n machdep.cpu.brand_string 2>/dev/null || echo unknown)"
  echo "cores: $(sysctl -n hw.ncpu 2>/dev/null || echo unknown)"
  echo "memory: $(($(sysctl -n hw.memsize 2>/dev/null || echo 0) / 1073741824)) GiB"
  echo "macOS: $(sw_vers -productVersion 2>/dev/null || echo unknown)"
  echo "colima vm: $(docker info --format '{{.NCPU}} CPUs, {{.MemTotal}} bytes of memory' 2>/dev/null || echo unknown)"
  echo "hey: $(go version -m "$(command -v hey)" 2>/dev/null | awk '$1 == "mod" { print $2, $3 }')"
  echo "mode: $([ "$QUICK" = 1 ] && echo quick || echo full)"
} >"$OUT/env.txt"

echo
echo "Results in $OUT (hey ${HEY_SECS}s runs)"
echo
printf '%-22s %9s %9s %9s %10s\n' run req/s p50_ms p99_ms 'non-200+err'
awk -F'\t' '{ printf "%-22s %9s %9s %9s %10s\n", $1, $2, $3, $4, $5 }' "$ROWS"
echo
echo "usage rows dropped by the writer: $DROPPED"
echo
printf '%-12s %s\n' subcommand result
awk '{ printf "%-12s %s\n", $1, $2 }' "$RESULTS"

if grep -qv ' PASS$' "$RESULTS"; then
  exit 1
fi
