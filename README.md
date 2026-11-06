# Ruptiq

**Find the breaking point. Fix the bottleneck.**

Ruptiq is an intelligent performance testing platform for distributed systems. The goal is to
push a system under test until it breaks, then help engineers understand *why* it broke and
what to fix first.

## Goals

- Run controlled load against a target application and find where it starts to fail.
- Collect telemetry (latency, errors, saturation) from the target during the run.
- Investigate the results to point at the likely bottleneck, not just report raw numbers.
- Keep Ruptiq Core independent of any one target, so other systems can be tested through adapters.

## MVP scope

The first usable version is planned to cover:

1. Running a load workload against a target and recording the run.
2. Reading telemetry from the target while the workload runs.
3. An investigation step that analyzes a run and reports a likely bottleneck.
4. A separate checkout demo application used as the first test target.

Items 1 to 3 do not exist yet; item 4 is in progress (see below). See
[docs/architecture.md](docs/architecture.md) for how these pieces are planned to fit together.

## Current implementation status

Ruptiq Core is only a health-check server. The checkout demo has three services: the Order
Service, the Inventory Service, and the Payment Service. The Order Service can reserve stock by
calling the Inventory Service over HTTP; nothing calls the Payment Service yet. Ruptiq cannot run
load tests yet.

| Item | Status |
| --- | --- |
| Go module and repository layout | Done |
| `ruptiq-api` HTTP server (standard library only) | Done |
| `GET /healthz` health endpoint on `ruptiq-api` | Done (automated test, and checked against a running server) |
| `PORT` setting and server timeouts | Done (`PORT` parsing is unit tested) |
| Graceful shutdown on Ctrl+C / SIGTERM (all three servers) | Implemented, **not yet verified** by a real Ctrl+C run |
| CI workflow (`go test`, `go vet`, `go test -race`) | Added; the race step has **not run yet** (first run happens on Linux CI) |
| Checkout demo: Order Service | Done (in-memory storage, see below) |
| Checkout demo: Inventory Service | Done (in-memory stock and reservations) |
| Checkout demo: Payment Service | Done (simulated payments, in-memory, not yet called by the Order Service) |
| Order-to-inventory integration (`POST /orders/{id}/reserve`) | Done (HTTP/JSON, idempotent by order ID, see below) |
| Order-to-payment integration | Not started |
| Workload running, telemetry, investigation, run storage | Not started |

## Requirements

- Go 1.27 or newer (see the `go` line in `go.mod`).

## Run Ruptiq API locally

From the repository root:

```sh
go run ./cmd/ruptiq-api
```

The server listens on port `8080` by default. To use another port, set `PORT`.

PowerShell:

```powershell
$env:PORT = "9090"
go run ./cmd/ruptiq-api
```

macOS / Linux:

```sh
PORT=9090 go run ./cmd/ruptiq-api
```

Check that it is up (in another terminal):

```sh
curl http://localhost:8080/healthz
```

Expected response:

```json
{"status":"ok","service":"ruptiq-api"}
```

Press `Ctrl+C` to stop. The server is designed to finish in-flight requests (up to 10 seconds)
before exiting. This path has not been verified with a real Ctrl+C run yet and has no automated
test.

## Checkout demo: Order Service

The Order Service is the first piece of the checkout demo application. It is the **system under
test** that Ruptiq will eventually load test. It is **not** part of Ruptiq Core: it lives in
`demo/checkout/order-service/`, never imports Ruptiq Core packages, and is started separately.

**What it does:** it creates orders, returns them by ID, and reserves stock for an order by calling
the Inventory Service over HTTP. Orders are stored **in memory only** and are lost when the service
stops.

**What it does not do:** payment integration is **not implemented**. An order has one of two
statuses:

- `created`: only the order record exists. No stock is held.
- `reserved`: the Inventory Service confirmed a stock reservation, recorded in `reservation_id`.
  This is **not** a paid or completed order. Nothing takes payment, finalizes the order, or
  releases the reservation yet.

The service also has no authentication, no database, and no deliberate slowness or failures yet.

### Endpoints

| Method and path | Success | Description |
| --- | --- | --- |
| `GET /healthz` | `200` | `{"status":"ok","service":"order-service"}` |
| `POST /orders` | `201` | Create an order from `{"sku": "...", "quantity": 1-10}` |
| `GET /orders/{id}` | `200` | Fetch an order by ID |
| `POST /orders/{id}/reserve` | `200` | Reserve the order's stock in the Inventory Service (no request body) |

Rules for `POST /orders`:

- `sku` is required, must not be blank, and can be at most 64 characters. Surrounding whitespace
  is trimmed.
- `quantity` must be a whole number from 1 to 10.
- Unknown fields, malformed JSON, trailing data, and bodies over 4096 bytes are rejected.

Errors are JSON in the form `{"error":"message"}`:

| Status | When |
| --- | --- |
| `400` | Malformed JSON, empty body, unknown field, wrong field type, or trailing data |
| `404` | Order not found, or unknown path |
| `405` | Wrong method for a path (the `Allow` header lists the allowed methods) |
| `413` | Request body larger than 4096 bytes |
| `422` | Valid JSON but invalid values (missing or blank `sku`, `quantity` outside 1 to 10) |

### Reserving inventory: `POST /orders/{id}/reserve`

The Order Service sends the order's `sku` and `quantity` to the Inventory Service's
`POST /reservations`, using the **order ID as the idempotency key** (`order_id`). On confirmation
it stores the reservation ID and sets the status to `reserved`. The updated order is returned and
is visible through `GET /orders/{id}`.

```json
{"id":"ord_6D4ZSGUOXMTIUTGQJVMXDKX5EJ","sku":"LAPTOP-001","quantity":2,"status":"reserved","reservation_id":"res_SF5MH6IFUI54MIGH7QT2FTHZEZ","created_at":"2026-11-06T08:46:45.8032774Z"}
```

| Status | When |
| --- | --- |
| `200` | Reserved now, or already reserved earlier (repeating the call is safe and returns the same order) |
| `404` | Order not found |
| `409` | Not enough stock, or the Inventory Service already holds a different reservation for this order ID |
| `422` | The order's SKU is unknown to the Inventory Service |
| `502` | The Inventory Service answered with something unexpected (malformed or mismatched response) |
| `503` | The Inventory Service could not be reached, failed (5xx), or is not configured |
| `504` | The Inventory Service did not answer within 5 seconds |

On every error the order is **left unchanged** (`created`). For `502`, `503`, and `504` the outcome
in the Inventory Service is unknown: it may or may not have reserved stock. **Retrying the same
call is safe**: the Inventory Service recognizes the order ID and returns the existing reservation
instead of reserving twice. The Order Service never retries on its own.

### Start it

The service listens on port `8081` by default. Set `ORDER_SERVICE_PORT` to change it.

It finds the Inventory Service through `INVENTORY_SERVICE_URL` (default `http://localhost:8082`).
The value must be an absolute `http` or `https` URL with no path, query, or credentials; an
invalid value stops the service at startup. The Order Service starts even if the Inventory Service
is not running; only reserve calls fail (`503`) until it is reachable.

PowerShell:

```powershell
go run ./demo/checkout/order-service
```

```powershell
$env:ORDER_SERVICE_PORT = "9091"
go run ./demo/checkout/order-service
```

Press `Ctrl+C` to stop. Like `ruptiq-api`, graceful shutdown is implemented but has not been
verified with a real Ctrl+C run.

### Try it (PowerShell)

Run these in a second terminal while the service is running.

Health check:

```powershell
Invoke-RestMethod http://localhost:8081/healthz
```

Create an order and keep its ID:

```powershell
$body = @{ sku = "LAPTOP-001"; quantity = 2 } | ConvertTo-Json
$order = Invoke-RestMethod -Method Post -Uri http://localhost:8081/orders -ContentType "application/json" -Body $body
$order
```

Fetch it back:

```powershell
Invoke-RestMethod "http://localhost:8081/orders/$($order.id)"
```

See the status code and raw body (`Invoke-RestMethod` hides the status code):

```powershell
$r = Invoke-WebRequest -Method Post -Uri http://localhost:8081/orders -ContentType "application/json" -Body $body -UseBasicParsing
$r.StatusCode
$r.Content
```

See an error response. In Windows PowerShell, errors are thrown, so catch them:

```powershell
try {
    Invoke-WebRequest -Method Post -Uri http://localhost:8081/orders -ContentType "application/json" -Body '{"sku":"LAPTOP-001","quantity":11}' -UseBasicParsing
} catch {
    $_.Exception.Response.StatusCode.value__   # 422
    $_.ErrorDetails.Message                    # {"error":"quantity must be between 1 and 10"}
}
```

Example order response (`id` and `created_at` differ on every request):

```json
{"id":"ord_2JUUQNEIZFTQOZDHSM5OSFVOCA","sku":"LAPTOP-001","quantity":2,"status":"created","created_at":"2026-11-04T23:55:39.3056743Z"}
```

### Run Order Service and Inventory Service together (PowerShell)

Use three terminals, all in the repository root.

Terminal 1, the Inventory Service (port `8082`):

```powershell
go run ./demo/checkout/inventory-service
```

Terminal 2, the Order Service (port `8081`). Setting `INVENTORY_SERVICE_URL` is optional here
because this is the default:

```powershell
$env:INVENTORY_SERVICE_URL = "http://localhost:8082"
go run ./demo/checkout/order-service
```

Terminal 3, the walkthrough. Check stock, create an order, reserve it, and inspect both sides:

```powershell
Invoke-RestMethod http://localhost:8082/inventory/LAPTOP-001          # available 10, reserved 0

$order = Invoke-RestMethod -Method Post -Uri http://localhost:8081/orders -ContentType "application/json" -Body '{"sku":"LAPTOP-001","quantity":2}'
$order                                                                # status created

Invoke-RestMethod -Method Post -Uri "http://localhost:8081/orders/$($order.id)/reserve"   # status reserved, reservation_id set
Invoke-RestMethod "http://localhost:8081/orders/$($order.id)"                             # same, stored
Invoke-RestMethod http://localhost:8082/inventory/LAPTOP-001          # available 8, reserved 2
```

Reserve the same order again. It returns the same reservation and stock stays at 8:

```powershell
Invoke-RestMethod -Method Post -Uri "http://localhost:8081/orders/$($order.id)/reserve"
Invoke-RestMethod http://localhost:8082/inventory/LAPTOP-001          # still available 8
```

See a failure (here: not enough stock left for 9 laptops):

```powershell
$big = Invoke-RestMethod -Method Post -Uri http://localhost:8081/orders -ContentType "application/json" -Body '{"sku":"LAPTOP-001","quantity":9}'
try {
    Invoke-WebRequest -Method Post -Uri "http://localhost:8081/orders/$($big.id)/reserve" -UseBasicParsing
} catch {
    $_.Exception.Response.StatusCode.value__   # 409
    $_.ErrorDetails.Message                    # {"error":"insufficient stock"}
}
```

### Integration limitations

- **Restarts lose state on either side.** Both services keep everything in memory. If the
  Inventory Service restarts, orders already marked `reserved` keep a `reservation_id` that the
  Inventory Service no longer knows about, and its stock is back to the seed values. If the Order
  Service restarts, its orders are gone but their reservations remain held in the Inventory Service.
- **Reservations are never released.** A failed or abandoned checkout keeps its stock held; there
  is no cancellation or expiry.
- **Already-reserved orders are answered locally.** Once an order is `reserved`, repeating the
  reserve call does not contact the Inventory Service again, so it does not detect the restart case
  above.
- **Error classification depends on message text.** The Inventory Service's errors have no
  machine-readable codes, so the Order Service tells "insufficient stock" from other `409`s, and
  "unknown sku" from other `404`s, by the documented message.
- One fixed 5-second timeout, no automatic retries, no circuit breaker, and no service discovery
  beyond a single URL.

## Checkout demo: Inventory Service

The Inventory Service is the second piece of the checkout demo, and like the Order Service it is
part of the **system under test**, not Ruptiq Core. It lives in `demo/checkout/inventory-service/`,
never imports Ruptiq Core or the Order Service, and is started separately.

**What it does:** it reports stock per SKU and reserves stock for an order. Stock and reservations
are kept **in memory only** and are lost when the service stops.

**What it does not do:** a reservation is **not a completed purchase**. There is no payment, no
order finalization, and no way to release, expire, or confirm a reservation. The Order Service
calls `POST /reservations` to reserve stock for its orders (see the Order Service section).

**Seed data (testing only):** the service always starts with the same small catalog. It exists so
tests and demos begin from a known state; it is not real product data.

| SKU | Starting units |
| --- | --- |
| `LAPTOP-001` | 10 |
| `PHONE-7` | 50 |
| `HEADPHONES-01` | 100 |

Restart the service to reset stock to these values.

### Endpoints

| Method and path | Success | Description |
| --- | --- | --- |
| `GET /healthz` | `200` | `{"status":"ok","service":"inventory-service"}` |
| `GET /inventory/{sku}` | `200` | `{"sku":"...","available":N,"reserved":N}` |
| `POST /reservations` | `201` or `200` | Reserve stock: `{"order_id":"...","sku":"...","quantity":N}` |

Reservation behavior for `POST /reservations`:

| Situation | Response |
| --- | --- |
| New `order_id`, enough stock | `201` with the reservation (`id`, `order_id`, `sku`, `quantity`, `status: "reserved"`, `created_at`). Stock is decreased. |
| Same `order_id` and identical `sku` and `quantity` (a retry) | `200` with the **existing** reservation. Stock is not decreased again. |
| Same `order_id` with a different `sku` or `quantity` | `409`. Nothing changes. |
| Not enough available stock | `409`. Inventory is not changed. |
| Unknown `sku` | `404` |

Validation rules: `order_id` and `sku` are required, non-blank, at most 64 characters, and have
surrounding whitespace trimmed. `quantity` must be a whole number from 1 to 1000. Unknown fields,
malformed JSON, trailing data, and bodies over 4096 bytes are rejected.

Errors are JSON in the form `{"error":"message"}`:

| Status | When |
| --- | --- |
| `400` | Malformed JSON, empty body, unknown field, wrong field type, or trailing data |
| `404` | Unknown SKU (on either endpoint), or unknown path |
| `405` | Wrong method for a path (the `Allow` header lists the allowed methods) |
| `409` | Insufficient stock, or `order_id` reused with different details |
| `413` | Request body larger than 4096 bytes |
| `422` | Valid JSON but invalid values (missing or blank `order_id` or `sku`, `quantity` outside 1 to 1000, values too long) |

### Consistency guarantees

- **No overselling.** Checking stock, decreasing it, and recording the reservation happen as one
  step under a single lock. Under concurrent requests, the number of units reserved can never
  exceed the units that exist, and `available` never goes below zero.
- **Idempotent reservations.** `order_id` is the idempotency key. Retrying the same request, even
  many times at once, decreases stock exactly once and always returns the same reservation ID.
  A retry still returns `200` if the stock has since run out, because the reservation exists.
- **Failures change nothing.** Rejected requests (validation, unknown SKU, conflict, insufficient
  stock) leave stock and reservations untouched.
- **Unique reservation IDs.** IDs look like `res_...` and come from `crypto/rand` (128 random
  bits).
- `available + reserved` for a SKU is constant while the service runs.

### Start it

The service listens on port `8082` by default. Set `INVENTORY_SERVICE_PORT` to change it. Logs are
JSON lines on standard output.

PowerShell:

```powershell
go run ./demo/checkout/inventory-service
```

```powershell
$env:INVENTORY_SERVICE_PORT = "9092"
go run ./demo/checkout/inventory-service
```

Press `Ctrl+C` to stop. Graceful shutdown is implemented (in-flight requests get up to 10 seconds)
but, like the other services, has not been verified with a real Ctrl+C run.

### Try it (PowerShell)

Run these in a second terminal while the service is running.

```powershell
Invoke-RestMethod http://localhost:8082/healthz
Invoke-RestMethod http://localhost:8082/inventory/LAPTOP-001
```

Reserve 2 laptops (`201`):

```powershell
$body = @{ order_id = "ord_example123"; sku = "LAPTOP-001"; quantity = 2 } | ConvertTo-Json
$res = Invoke-RestMethod -Method Post -Uri http://localhost:8082/reservations -ContentType "application/json" -Body $body
$res
```

Send the identical request again. It returns the same reservation (`200`) and stock is not reduced
a second time:

```powershell
$r = Invoke-WebRequest -Method Post -Uri http://localhost:8082/reservations -ContentType "application/json" -Body $body -UseBasicParsing
$r.StatusCode   # 200
$r.Content
Invoke-RestMethod http://localhost:8082/inventory/LAPTOP-001   # available 8, reserved 2
```

See an error response (here: asking for more than is available, `409`). In Windows PowerShell,
errors are thrown, so catch them:

```powershell
try {
    Invoke-WebRequest -Method Post -Uri http://localhost:8082/reservations -ContentType "application/json" -Body '{"order_id":"ord_too_big","sku":"LAPTOP-001","quantity":9}' -UseBasicParsing
} catch {
    $_.Exception.Response.StatusCode.value__   # 409
    $_.ErrorDetails.Message                    # {"error":"insufficient stock"}
}
```

Example responses:

```json
{"sku":"LAPTOP-001","available":10,"reserved":0}
{"id":"res_LUJ6DSWPQGE3KBA7YLNQCXEOQ4","order_id":"ord_example123","sku":"LAPTOP-001","quantity":2,"status":"reserved","created_at":"2026-11-05T00:20:33.9115372Z"}
{"error":"insufficient stock"}
```

### Current limitations

- All state is in memory: a restart resets stock and forgets reservations.
- Reservations are never released, expired, or confirmed, so stock reserved by abandoned orders is
  never returned.
- The idempotency record lives only as long as the process, and one lock guards all SKUs, so
  reservations for different SKUs do not run in parallel. Both are deliberate simplifications.
- No authentication, no database, and no deliberate slowness or failures. It makes no outgoing
  calls; the Order Service calls its `POST /reservations` to reserve stock for orders.
- The seed catalog is fixed and cannot be changed at runtime.

## Checkout demo: Payment Service

The Payment Service is the third piece of the checkout demo. Like the others it is part of the
**system under test**, not Ruptiq Core. It lives in `demo/checkout/payment-service/`, never imports
Ruptiq Core or the other checkout services, and is started separately.

**What it does:** it records simulated payment attempts and returns them by ID. Each attempt is
either `approved` or `declined`, and the caller chooses which by picking a test payment method.

**Why it is a simulation:** there is no payment provider, no credentials, and no card numbers. The
service exists so Ruptiq has a realistic, correctness-sensitive dependency to test: retries,
duplicate submissions, and concurrent attempts for the same order are exactly where real payment
systems go wrong. Requests containing unknown fields (for example a `card_number`) are rejected,
and request bodies are never logged.

**What it does not do:** it does not move money, call the Order Service, or check inventory. An
`approved` payment means only that the simulation approved it.

### Endpoints

| Method and path | Success | Description |
| --- | --- | --- |
| `GET /healthz` | `200` | `{"status":"ok","service":"payment-service"}` |
| `POST /payments` | `201` or `200` | Create a simulated payment attempt |
| `GET /payments/{id}` | `200` | Fetch a payment attempt by ID |

Request body for `POST /payments`:

```json
{
  "order_id": "ord_example123",
  "amount_cents": 24999,
  "currency": "USD",
  "payment_method": "test_approve",
  "idempotency_key": "checkout-attempt-001"
}
```

| Field | Rules |
| --- | --- |
| `order_id` | Required, non-blank, at most 64 characters, no control characters. Whitespace is trimmed. |
| `amount_cents` | Whole number from 1 to 100,000,000 (that is, up to 1,000,000.00 USD). The limit is a demo sanity check. |
| `currency` | Must be exactly `USD`. No other currency is supported in the MVP. |
| `payment_method` | `test_approve` (always approved) or `test_decline` (always declined). |
| `idempotency_key` | Required, non-blank, at most 128 characters, no control characters. Whitespace is trimmed. |

Unknown fields, malformed JSON, trailing data, and bodies over 4096 bytes are rejected.

Response (`id` and `created_at` differ on every new payment; `status` is `approved` or `declined`):

```json
{"id":"pay_...","order_id":"ord_example123","amount_cents":24999,"currency":"USD","status":"approved","created_at":"2026-11-05T00:20:33.9115372Z"}
```

A decline is a normal business outcome: it is returned with `201` like an approval, with
`"status":"declined"`. It is not an HTTP error.

### Idempotency model

The client supplies an `idempotency_key` with each attempt. The key identifies one logical attempt,
so a client that is unsure whether its request arrived can safely send it again.

| Situation | Response |
| --- | --- |
| New key | `201`, a new payment is recorded |
| Same key, identical `order_id`, `amount_cents`, `currency`, and `payment_method` | `200` with the **original** payment. Nothing new is created, even for a declined attempt, even if sent many times at once. |
| Same key, any of those fields different | `409` |

Keys are global to the service, not scoped per order. Rejected requests (`409`, `422`) are not
recorded, so their keys stay unused.

### Duplicate-approval protection

An order can have **at most one approved payment**.

- If the order already has an approved payment, any new attempt for it with a different key gets
  `409` (approve or decline).
- A declined payment does not block the order. A new attempt with a new key can follow it.
- The idempotency check runs **first**, so retrying the key that produced the approval still
  returns `200` with that approval, rather than `409`.
- Concurrent attempts for one order with different keys result in exactly one approval. The check
  and the creation are one atomic step (see the architecture document).

### Start it

The service listens on port `8083` by default. Set `PAYMENT_SERVICE_PORT` to change it. Logs are
JSON lines on standard output.

PowerShell:

```powershell
go run ./demo/checkout/payment-service
```

```powershell
$env:PAYMENT_SERVICE_PORT = "9093"
go run ./demo/checkout/payment-service
```

Press `Ctrl+C` to stop. Graceful shutdown is implemented but, like the other services, has not
been verified with a real Ctrl+C run.

### Try it (PowerShell)

Run these in a second terminal while the service is running.

```powershell
Invoke-RestMethod http://localhost:8083/healthz
```

Create an approved payment (`201`) and fetch it back:

```powershell
$body = @{ order_id = "ord_example123"; amount_cents = 24999; currency = "USD"; payment_method = "test_approve"; idempotency_key = "checkout-attempt-001" } | ConvertTo-Json
$pay = Invoke-RestMethod -Method Post -Uri http://localhost:8083/payments -ContentType "application/json" -Body $body
$pay
Invoke-RestMethod "http://localhost:8083/payments/$($pay.id)"
```

Send the identical request again. It returns the same payment with status `200`:

```powershell
$r = Invoke-WebRequest -Method Post -Uri http://localhost:8083/payments -ContentType "application/json" -Body $body -UseBasicParsing
$r.StatusCode   # 200
$r.Content
```

Try to pay the same order again with a new key (`409`). In Windows PowerShell, errors are thrown,
so catch them:

```powershell
$second = @{ order_id = "ord_example123"; amount_cents = 24999; currency = "USD"; payment_method = "test_approve"; idempotency_key = "checkout-attempt-002" } | ConvertTo-Json
try {
    Invoke-WebRequest -Method Post -Uri http://localhost:8083/payments -ContentType "application/json" -Body $second -UseBasicParsing
} catch {
    $_.Exception.Response.StatusCode.value__   # 409
    $_.ErrorDetails.Message                    # {"error":"order already has an approved payment"}
}
```

A simulated decline (`201`, status `declined`) for a different order:

```powershell
$decline = @{ order_id = "ord_declined1"; amount_cents = 1500; currency = "USD"; payment_method = "test_decline"; idempotency_key = "checkout-attempt-003" } | ConvertTo-Json
Invoke-RestMethod -Method Post -Uri http://localhost:8083/payments -ContentType "application/json" -Body $decline
```

### Errors

Errors are JSON in the form `{"error":"message"}`:

| Status | When |
| --- | --- |
| `400` | Malformed JSON, empty body, unknown field, wrong field type, or trailing data |
| `404` | Payment not found, or unknown path |
| `405` | Wrong method for a path (the `Allow` header lists the allowed methods) |
| `409` | Idempotency key reused with different details, or the order already has an approved payment |
| `413` | Request body larger than 4096 bytes |
| `422` | Valid JSON but invalid values (missing or blank identifiers, bad amount, unsupported currency or payment method) |

### Current limitations

- **The guarantees hold only within one running process.** Idempotency keys, payments, and the
  approved-order record live in memory. After a crash or restart they are gone, so a retry could
  create a second approval. Two replicas of the service would each have separate state and could
  each approve the same order. Fixing this needs shared durable storage (such as a database
  constraint), which does not exist yet.
- No payment is ever refunded, captured, voided, or expired.
- Idempotency keys are never forgotten or expired while the process runs.
- One lock guards all state, so payments for different orders do not run in parallel.
- No authentication, and no deliberate slowness or failures.

### Future integration

Nothing calls the Payment Service yet. The planned checkout flow is: the Order Service creates an
order, reserves stock with the Inventory Service, then asks the Payment Service to pay, using a
stable idempotency key per checkout attempt so its own retries are safe. Only then would the order
be finalized. Those calls, and what to do when a step fails midway (for example releasing a
reservation after a decline), are not implemented.

## Test and check

```sh
gofmt -l .
go build ./...
go test ./...
go vet ./...
go test -race ./...
```

`gofmt -l .` prints the names of files that are not formatted. No output means everything is
formatted.

`go test -race` turns on Go's race detector, which needs cgo and a 64-bit C compiler. The
GitHub Actions workflow runs it on Linux. The development machine (Windows) has only a 32-bit
MinGW compiler, so the race detector cannot run there; run the plain `go test ./...` locally. The
concurrency tests, including 200 simultaneous reservation attempts for 10 units and 200
simultaneous payment attempts for one order, run without it.

## Module path

The Go module path is `github.com/RajK158/ruptiq`, matching the GitHub repository.

## Project layout

```
cmd/ruptiq-api/                          Ruptiq Core: program entry point
internal/api/                            Ruptiq Core: HTTP server, routes, and handlers
demo/checkout/order-service/             Demo target (NOT Ruptiq Core): Order Service
  main.go                                  entry point, configuration, graceful shutdown
  internal/httpapi/                        HTTP routing, handlers, JSON errors
  internal/order/                          order model, validation, ID generation, in-memory store,
                                           reservation use case and the inventory interface it needs
  internal/inventoryclient/                HTTP/JSON client for the Inventory Service
demo/checkout/inventory-service/         Demo target (NOT Ruptiq Core): Inventory Service
  main.go                                  entry point, configuration, graceful shutdown
  internal/httpapi/                        HTTP routing, handlers, JSON errors, request logging
  internal/inventory/                      stock and reservation rules, seed data, in-memory store
demo/checkout/payment-service/           Demo target (NOT Ruptiq Core): Payment Service
  main.go                                  entry point, configuration, graceful shutdown
  internal/httpapi/                        HTTP routing, handlers, JSON errors, request logging
  internal/payment/                        payment rules, idempotency, in-memory store
docs/                                    Design documents
```
