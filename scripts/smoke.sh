#!/usr/bin/env bash
# End-to-end smoke test against a running stack (docker compose up -d --wait).
# Exercises the main flows through the public API only; needs just bash, curl and grep.
#
#   API_URL=http://localhost:8080 PROMETHEUS_URL=http://localhost:9090 scripts/smoke.sh
set -euo pipefail

API="${API_URL:-http://localhost:8080}"
PROM="${PROMETHEUS_URL:-http://localhost:9090}"
MOCK="${MOCK_PROVIDER_URL:-http://localhost:8090}"
TIMEOUT="${SMOKE_TIMEOUT:-90}"
RUN="smoke-$(date +%s)-$RANDOM"

pass() { printf '  \033[32mok\033[0m   %s\n' "$1"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$1" >&2; exit 1; }
json_field() { grep -o "\"$1\":\"[^\"]*\"" | head -1 | cut -d'"' -f4; }
json_number() { grep -o "\"$1\":[0-9.]*" | head -1 | cut -d: -f2; }

# post PATH BODY [HEADER] -> prints "<status> <body>"
post() {
  local extra=()
  [[ $# -ge 3 ]] && extra=(-H "$3")
  local resp
  resp=$(curl -s -w '\n%{http_code}' -H 'Content-Type: application/json' "${extra[@]}" -d "$2" "$API$1")
  echo "${resp##*$'\n'} ${resp%$'\n'*}"
}

echo "smoke test against $API"

for _ in $(seq 1 60); do curl -sf "$API/readyz" >/dev/null && break; sleep 1; done
curl -sf "$API/readyz" >/dev/null || fail "API not ready"
pass "readyz"

# Template + notification from it.
out=$(post /v1/templates "{\"name\":\"$RUN\",\"channel\":\"sms\",\"body\":\"Your code is {{.code}}\"}")
[[ ${out%% *} == 201 ]] || fail "create template: $out"
pass "template created"

body="{\"channel\":\"sms\",\"recipient\":\"+905551234567\",\"priority\":\"high\",\"template\":{\"name\":\"$RUN\",\"variables\":{\"code\":\"4242\"}}}"
out=$(post /v1/notifications "$body" "Idempotency-Key: $RUN")
[[ ${out%% *} == 201 ]] || fail "create notification: $out"
id=$(echo "$out" | json_field id)
echo "$out" | grep -q '"content":"Your code is 4242"' || fail "template not rendered: $out"
pass "notification created from template ($id)"

out=$(post /v1/notifications "$body" "Idempotency-Key: $RUN")
[[ ${out%% *} == 200 ]] && [[ $(echo "$out" | json_field id) == "$id" ]] || fail "idempotent replay: $out"
pass "idempotent replay returns the same notification"

out=$(post /v1/notifications '{"channel":"sms","recipient":"12345","content":""}')
[[ ${out%% *} == 422 ]] || fail "validation: $out"
pass "invalid input rejected with 422"

# A mixed batch of 30.
items=""
for i in $(seq 1 10); do
  items+="{\"channel\":\"sms\",\"recipient\":\"+90555123$(printf '%04d' "$i")\",\"content\":\"batch $i\",\"priority\":\"low\"},"
  items+="{\"channel\":\"email\",\"recipient\":\"user$i@example.com\",\"content\":\"batch $i\"},"
  items+="{\"channel\":\"push\",\"recipient\":\"device-token-$i\",\"content\":\"batch $i\"},"
done
out=$(post /v1/notifications/batch "{\"items\":[${items%,}]}" "Idempotency-Key: $RUN-batch")
[[ ${out%% *} == 201 ]] || fail "create batch: $out"
batch=$(echo "$out" | json_field batch_id)
pass "batch of 30 accepted ($batch)"

# Wait for delivery (the mock provider fails some calls on purpose; retries recover them).
deadline=$((SECONDS + TIMEOUT))
while :; do
  summary=$(curl -s "$API/v1/batches/$batch")
  sent=$(echo "$summary" | json_number sent)
  [[ $sent == 30 ]] && break
  ((SECONDS < deadline)) || fail "batch not delivered in ${TIMEOUT}s: $summary"
  sleep 1
done
pass "all 30 batch notifications sent"

while :; do
  status=$(curl -s "$API/v1/notifications/$id" | json_field status)
  [[ $status == sent ]] && break
  ((SECONDS < deadline)) || fail "notification $id is $status"
  sleep 1
done
pass "template notification sent (with delivery attempts recorded)"

# The rate limit as the provider sees it (when the mock provider is running): 300 SMS at once,
# then the mock's busiest second must stay within 100 requests, across every worker replica.
if curl -sf "$MOCK/healthz" >/dev/null 2>&1; then
  items=""
  for i in $(seq 1 300); do
    items+="{\"channel\":\"sms\",\"recipient\":\"+90556$(printf '%07d' "$i")\",\"content\":\"rate $i\"},"
  done
  out=$(post /v1/notifications/batch "{\"items\":[${items%,}]}")
  [[ ${out%% *} == 201 ]] || fail "create rate batch: $out"
  rbatch=$(echo "$out" | json_field batch_id)
  start=$SECONDS
  deadline=$((SECONDS + TIMEOUT))
  while :; do
    summary=$(curl -s "$API/v1/batches/$rbatch")
    done_count=$(( $(echo "$summary" | json_number sent) + $(echo "$summary" | json_number failed) ))
    ((done_count == 300)) && break
    ((SECONDS < deadline)) || fail "rate batch not finished in ${TIMEOUT}s: $summary"
    sleep 1
  done
  peak=$(curl -s "$MOCK/stats" | grep -o '"sms":[0-9]*' | head -1 | cut -d: -f2)
  [[ -n $peak ]] && ((peak <= 100)) || fail "provider saw ${peak:-?} SMS within one second (limit 100)"
  pass "300 SMS in $((SECONDS - start))s; busiest second at the provider: $peak (limit 100)"
fi

stats=$(curl -s "$API/v1/stats?window=10m")
echo "$stats" | grep -q '"success_rate":[0-9]' || fail "stats: $stats"
pass "GET /v1/stats reports success rates"

curl -s "$API/metrics" | grep -q '^notification_queue_depth{' || fail "queue depth metric missing"
pass "API /metrics exports queue depth"

if curl -sf "$PROM/-/ready" >/dev/null 2>&1; then
  for _ in $(seq 1 30); do
    up=$(curl -s "$PROM/api/v1/query?query=min(up%7Bjob%3D%22worker%22%7D)" | grep -o '"value":\[[^]]*\]' | grep -o '"[01]"' || true)
    [[ $up == '"1"' ]] && break
    sleep 1
  done
  [[ $up == '"1"' ]] || fail "Prometheus is not scraping the workers"
  pass "Prometheus scrapes every worker"
fi

echo "smoke test passed"
