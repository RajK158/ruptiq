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

Ruptiq Core is only a health-check server. The checkout demo has two independent services, the
Order Service and the Inventory Service, which do not call each other yet. Ruptiq cannot run load
tests yet.

| Item | Status |
| --- | --- |
| Go module and repository layout | Done |
| `ruptiq-api` HTTP server (standard library only) | Done |
| `GET /healthz` health endpoint on `ruptiq-api` | Done (automated test, and checked against a running server) |
| `PORT` setting and server timeouts | Done (`PORT` parsing is unit tested) |
| Graceful shutdown on Ctrl+C / SIGTERM (all three servers) | Implemented, **not yet verified** by a real Ctrl+C run |
| CI workflow (`go test`, `go vet`, `go test -race`) | Added; the race step has **not run yet** (first run happens on Linux CI) |
| Checkout demo: Order Service | Done (in-memory storage, see below) |
| Checkout demo: Inventory Service | Done (in-memory stock and reservations, not yet called by the Order Service) |
| Checkout demo: Payment Service | Not started |
| Order-to-inventory integration | Not started |
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

**What it does:** it creates orders and returns them by ID. Orders are stored **in memory only**
and are lost when the service stops.

**What it does not do:** payment and inventory integration are **not implemented**. There is no
Payment Service, and the Order Service does not call the Inventory Service. An order with status `created` means *only* that the
record was created. It does not mean stock was reserved, payment succeeded, or checkout completed.
The service also has no authentication, no database, and no deliberate slowness or failures yet.

### Endpoints

| Method and path | Success | Description |
| --- | --- | --- |
| `GET /healthz` | `200` | `{"status":"ok","service":"order-service"}` |
| `POST /orders` | `201` | Create an order from `{"sku": "...", "quantity": 1-10}` |
| `GET /orders/{id}` | `200` | Fetch an order by ID |

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

### Start it

The service listens on port `8081` by default. Set `ORDER_SERVICE_PORT` to change it.

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

## Checkout demo: Inventory Service

The Inventory Service is the second piece of the checkout demo, and like the Order Service it is
part of the **system under test**, not Ruptiq Core. It lives in `demo/checkout/inventory-service/`,
never imports Ruptiq Core or the Order Service, and is started separately.

**What it does:** it reports stock per SKU and reserves stock for an order. Stock and reservations
are kept **in memory only** and are lost when the service stops.

**What it does not do:** a reservation is **not a completed purchase**. There is no payment, no
order finalization, and no way to release, expire, or confirm a reservation. The Order Service does
not call the Inventory Service yet.

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
- No authentication, no database, no deliberate slowness or failures, and no calls to or from any
  other service.
- The seed catalog is fixed and cannot be changed at runtime.

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
concurrency tests, including 200 simultaneous reservation attempts for 10 units, run without it.

## Module path

The Go module path is `github.com/RajK158/ruptiq`, matching the GitHub repository.

## Project layout

```
cmd/ruptiq-api/                          Ruptiq Core: program entry point
internal/api/                            Ruptiq Core: HTTP server, routes, and handlers
demo/checkout/order-service/             Demo target (NOT Ruptiq Core): Order Service
  main.go                                  entry point, configuration, graceful shutdown
  internal/httpapi/                        HTTP routing, handlers, JSON errors
  internal/order/                          order model, validation, ID generation, in-memory store
demo/checkout/inventory-service/         Demo target (NOT Ruptiq Core): Inventory Service
  main.go                                  entry point, configuration, graceful shutdown
  internal/httpapi/                        HTTP routing, handlers, JSON errors, request logging
  internal/inventory/                      stock and reservation rules, seed data, in-memory store
docs/                                    Design documents
```
