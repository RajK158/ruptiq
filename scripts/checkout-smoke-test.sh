#!/usr/bin/env bash
# End-to-end smoke test for the checkout demo (Order, Inventory, and Payment
# Services) over real HTTP. It expects a freshly started environment, for
# example:
#
#   docker compose up --build -d --wait
#   bash scripts/checkout-smoke-test.sh
#
# Requires bash, curl, and jq. Service URLs can be overridden:
#   ORDER_URL (default http://localhost:8081)
#   INVENTORY_URL (default http://localhost:8082)
#   PAYMENT_URL (default http://localhost:8083)
#   HEALTH_TIMEOUT_SECONDS (default 60): how long to wait for /healthz
#
# Every check compares the actual HTTP status and response body; the first
# failed check stops the script with a nonzero exit status.

set -euo pipefail

ORDER_URL="${ORDER_URL:-http://localhost:8081}"
INVENTORY_URL="${INVENTORY_URL:-http://localhost:8082}"
PAYMENT_URL="${PAYMENT_URL:-http://localhost:8083}"
HEALTH_TIMEOUT_SECONDS="${HEALTH_TIMEOUT_SECONDS:-60}"

for tool in curl jq; do
  command -v "$tool" >/dev/null 2>&1 || { echo "FAIL: $tool is required" >&2; exit 1; }
done

body_file="$(mktemp)"
trap 'rm -f "$body_file"' EXIT

# Set by request: the last response's status code and body.
STATUS=""
BODY=""

log() { printf '%s\n' "$*"; }

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  if [[ -n "${LAST_REQUEST:-}" ]]; then
    printf '  last request: %s\n  status: %s\n  body: %s\n' "$LAST_REQUEST" "$STATUS" "$BODY" >&2
  fi
  exit 1
}

# request METHOD URL [JSON_BODY]
request() {
  local method="$1" url="$2" data="${3:-}"
  local args=(--silent --show-error --max-time 10 -o "$body_file" -w '%{http_code}' -X "$method")
  if [[ -n "$data" ]]; then
    args+=(-H 'Content-Type: application/json' --data "$data")
  fi
  LAST_REQUEST="$method $url${data:+ $data}"
  if ! STATUS="$(curl "${args[@]}" "$url")"; then
    BODY="$(cat "$body_file" 2>/dev/null || true)"
    fail "HTTP request could not be completed"
  fi
  BODY="$(cat "$body_file")"
  log "  $method $url -> $STATUS"
}

expect_status() {
  [[ "$STATUS" == "$1" ]] || fail "expected HTTP status $1, got $STATUS"
}

# field JQ_FILTER: prints the value from BODY; fails if missing or null.
# Callers use x="$(field ...)" as a plain assignment (never inside if or ||),
# so a failure in the subshell stops the script through set -e.
field() {
  local value
  if ! value="$(jq -er "$1" <<<"$BODY" 2>/dev/null)"; then
    fail "response has no value for $1"
  fi
  printf '%s' "$value"
}

# expect_field JQ_FILTER EXPECTED
expect_field() {
  local actual
  actual="$(field "$1")"
  [[ "$actual" == "$2" ]] || fail "expected $1 = '$2', got '$actual'"
}

# expect_no_field JQ_FILTER: the field must be absent, null, or empty.
expect_no_field() {
  local actual
  if ! actual="$(jq -r "$1 // \"\"" <<<"$BODY" 2>/dev/null)"; then
    fail "response is not valid JSON"
  fi
  [[ -z "$actual" ]] || fail "expected $1 to be empty, got '$actual'"
}

pay_body() {
  jq -nc --arg m "$1" --arg k "$2" '{payment_method: $m, idempotency_key: $k}'
}

wait_for_health() {
  local name="$1" url="$2" deadline=$((SECONDS + HEALTH_TIMEOUT_SECONDS))
  until [[ "$(curl --silent --max-time 2 -o /dev/null -w '%{http_code}' "$url/healthz" || true)" == "200" ]]; do
    if (( SECONDS >= deadline )); then
      LAST_REQUEST="GET $url/healthz"
      fail "$name did not become healthy within ${HEALTH_TIMEOUT_SECONDS}s"
    fi
    sleep 1
  done
  log "  $name healthy"
}

# Successful stock reads made by this script. Nothing else reads stock, so the
# Inventory Service's request counter must match it exactly.
STOCK_READS=0

expect_stock() {
  request GET "$INVENTORY_URL/inventory/$1"
  expect_status 200
  STOCK_READS=$((STOCK_READS + 1))
  expect_field .sku "$1"
  expect_field .available "$2"
  expect_field .reserved "$3"
}

# expect_metric SERIES EXPECTED: SERIES is the exact text before the value on
# one line of a Prometheus exposition in BODY, such as
# 'inventory_stock_available_units{sku="PHONE-7"}'.
expect_metric() {
  local actual
  if ! actual="$(awk -v s="$1" '$1 == s { print $2; found = 1 } END { exit !found }' <<<"$BODY")"; then
    fail "metrics have no series $1"
  fi
  [[ "$actual" == "$2" ]] || fail "expected $1 = $2, got $actual"
}

# expect_absent TEXT: BODY must not contain TEXT.
expect_absent() {
  [[ "$BODY" != *"$1"* ]] || fail "response unexpectedly contains '$1'"
}

reserve_order() {
  request POST "$ORDER_URL/orders/$1/reserve"
  expect_status 200
  expect_field .id "$1"
  expect_field .status reserved
}

log "== Waiting for services"
wait_for_health "order-service" "$ORDER_URL"
wait_for_health "inventory-service" "$INVENTORY_URL"
wait_for_health "payment-service" "$PAYMENT_URL"

log "== Test A: successful checkout"
log "A1: LAPTOP-001 starts with 10 available (requires a fresh environment)"
expect_stock LAPTOP-001 10 0

log "A2: create an order for 2 x LAPTOP-001"
request POST "$ORDER_URL/orders" '{"sku":"LAPTOP-001","quantity":2}'
expect_status 201
expect_field .status created
expect_field .sku LAPTOP-001
expect_field .quantity 2
order_a="$(field .id)"
log "  order $order_a"

log "A3: reserve inventory"
reserve_order "$order_a"
reservation_a="$(field .reservation_id)"

log "A4: availability is 8"
expect_stock LAPTOP-001 8 2

log "A5: approve a simulated payment"
request POST "$ORDER_URL/orders/$order_a/pay" "$(pay_body test_approve checkout-a-1)"
expect_status 200
expect_field .payment.status approved
expect_field .payment.amount_cents 259998
expect_field .payment.currency USD
expect_field .order.status paid
payment_a="$(field .payment.id)"
expect_field .order.payment_id "$payment_a"
log "  payment $payment_a"

log "A6: the order is stored as paid"
request GET "$ORDER_URL/orders/$order_a"
expect_status 200
expect_field .status paid
expect_field .payment_id "$payment_a"
expect_field .payment_idempotency_key checkout-a-1
expect_field .reservation_id "$reservation_a"

log "A7: the Payment Service holds the approval"
request GET "$PAYMENT_URL/payments/$payment_a"
expect_status 200
expect_field .id "$payment_a"
expect_field .order_id "$order_a"
expect_field .status approved
expect_field .amount_cents 259998
expect_field .currency USD

log "== Test B: idempotency"
log "B1: repeat the reservation (the paid order must not move back to reserved)"
request POST "$ORDER_URL/orders/$order_a/reserve"
expect_status 200
expect_field .status paid
expect_field .reservation_id "$reservation_a"
expect_field .payment_id "$payment_a"

log "B2: availability is still 8"
expect_stock LAPTOP-001 8 2

log "B3: repeat the approved payment with the same key"
request POST "$ORDER_URL/orders/$order_a/pay" "$(pay_body test_approve checkout-a-1)"
expect_status 200
expect_field .payment.id "$payment_a"
expect_field .payment.status approved
expect_field .order.status paid

log "B4: a new key cannot create a second approval (Order Service)"
request POST "$ORDER_URL/orders/$order_a/pay" "$(pay_body test_approve checkout-a-2)"
expect_status 409
expect_field .error "order is already paid"

log "B5: a new key cannot create a second approval (Payment Service directly)"
request POST "$PAYMENT_URL/payments" "$(jq -nc --arg o "$order_a" \
  '{order_id: $o, amount_cents: 259998, currency: "USD", payment_method: "test_approve", idempotency_key: "smoke-direct-1"}')"
expect_status 409
expect_field .error "order already has an approved payment"

log "B6: the order still has the original payment"
request GET "$ORDER_URL/orders/$order_a"
expect_status 200
expect_field .status paid
expect_field .payment_id "$payment_a"
expect_field .payment_idempotency_key checkout-a-1

log "== Test C: declined payment, then approval with a new key"
log "C1: create and reserve an order for 1 x PHONE-7"
expect_stock PHONE-7 50 0
request POST "$ORDER_URL/orders" '{"sku":"PHONE-7","quantity":1}'
expect_status 201
order_c="$(field .id)"
log "  order $order_c"
reserve_order "$order_c"
expect_stock PHONE-7 49 1

log "C2: decline"
request POST "$ORDER_URL/orders/$order_c/pay" "$(pay_body test_decline checkout-c-1)"
expect_status 200
expect_field .payment.status declined
expect_field .payment.amount_cents 79999
expect_field .order.status reserved
expect_no_field .order.payment_id
declined_c="$(field .payment.id)"

log "C3: the order remains reserved"
request GET "$ORDER_URL/orders/$order_c"
expect_status 200
expect_field .status reserved
expect_no_field .payment_id
request GET "$PAYMENT_URL/payments/$declined_c"
expect_status 200
expect_field .status declined
expect_field .order_id "$order_c"

log "C4: approve with a new key"
request POST "$ORDER_URL/orders/$order_c/pay" "$(pay_body test_approve checkout-c-2)"
expect_status 200
expect_field .payment.status approved
expect_field .order.status paid
approved_c="$(field .payment.id)"
[[ "$approved_c" != "$declined_c" ]] || fail "approval reused the declined payment ID $declined_c"
expect_field .order.payment_id "$approved_c"

log "C5: the order is stored as paid; stock was deducted once"
request GET "$ORDER_URL/orders/$order_c"
expect_status 200
expect_field .status paid
expect_field .payment_id "$approved_c"
expect_field .payment_idempotency_key checkout-c-2
expect_stock PHONE-7 49 1

log "== Test D: Inventory Service metrics reflect the checkouts"
log "D1: stock gauges match the reservations above"
request GET "$INVENTORY_URL/metrics"
expect_status 200
expect_metric 'inventory_stock_available_units{sku="LAPTOP-001"}' 8
expect_metric 'inventory_stock_reserved_units{sku="LAPTOP-001"}' 2
expect_metric 'inventory_stock_available_units{sku="PHONE-7"}' 49
expect_metric 'inventory_stock_reserved_units{sku="PHONE-7"}' 1
expect_metric 'inventory_stock_available_units{sku="HEADPHONES-01"}' 100
expect_metric 'inventory_stock_reserved_units{sku="HEADPHONES-01"}' 0

log "D2: request metrics count each reservation once, by route template"
expect_metric 'inventory_http_requests_total{method="POST",route="/reservations",status="201"}' 2
expect_metric 'inventory_http_request_duration_seconds_count{method="POST",route="/reservations",status="201"}' 2
expect_metric 'inventory_http_requests_total{method="GET",route="/inventory/{sku}",status="200"}' "$STOCK_READS"

log "D3: no order, reservation, or payment IDs in labels; scrapes are not counted"
for id in "$order_a" "$order_c" "$reservation_a" "$payment_a"; do
  expect_absent "$id"
done
expect_absent 'route="/metrics"'
expect_absent 'route="/inventory/LAPTOP-001"'

log "PASS: all checkout smoke tests passed"
