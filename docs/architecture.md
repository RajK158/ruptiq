# Ruptiq Architecture

> **Status: mostly planned design.** Implemented today: the Ruptiq Core health-check server, and
> three checkout demo services (Order, Inventory, and Payment, described below). Nothing described under
> "Planned boundaries" exists in code yet, and no interfaces have been written. This document
> records the intended direction so early code does not paint us into a corner.

## Core idea: Ruptiq Core is separate from the target

Ruptiq has two sides that must stay apart:

- **Ruptiq Core**: the platform. It generates load, reads telemetry, investigates results, and
  stores run history. It knows nothing about any specific application.
- **Target applications**: the systems being tested. Ruptiq tests them from the outside.

```
+--------------------------------+          +---------------------------+
|          Ruptiq Core           |          |    Target application     |
|                                |          |                           |
|  WorkloadRunner  --- load ---------------->                           |
|  TelemetryReader <-- metrics ------------- |                           |
|  Investigator                  |          +---------------------------+
|  RunStore                      |                      ^
+--------------------------------+                      |
              ^                                         |
              | adapter (per target kind)               |
              +-----------------------------------------+
```

### Rules

1. **Core never imports target code.** No package in Ruptiq Core may import the checkout demo or
   any other target.
2. **The checkout demo is just a target.** It lives apart from Core, under `demo/checkout/`, with
   its own entry points, and is the first system Ruptiq tests. It has no special access to Core
   internals, and it must never import Core packages.
3. **External targets come in through adapters.** To test a real system (for example one that
   exposes Prometheus metrics), we write an adapter that translates between Core's needs and that
   system. Core does not change.
4. **Core talks to targets only through network protocols** (HTTP, metrics endpoints), the same
   way for the demo and for external systems.

## Planned boundaries

These are the four responsibilities we expect Core to have. They are described in words only.
Their Go interfaces will be designed when each one is actually needed, driven by a real
implementation, not guessed up front.

### WorkloadRunner

Responsible for applying load to a target: how many users or requests, for how long, and how load
ramps up. It reports what it did and the client-side results (such as response times and
failures). It must not know what the target does internally.

### TelemetryReader

Responsible for reading the target's own signals during a run, such as CPU, memory, queue depth, or
error counts. This is how Ruptiq sees *inside* the target. Each kind of target can expose telemetry
differently, so this is a main place where adapters plug in.

### Investigator

Responsible for analyzing the results of a run (client-side results plus target telemetry) to find
where the system broke and the likely bottleneck. It works on recorded data and should not drive
load or talk to the target directly, which keeps it easy to test with saved runs.

### RunStore

Responsible for saving and loading runs: configuration, results, and telemetry. It lets us compare
runs over time and re-run an investigation on old data. The storage technology is undecided.

## How the pieces work together (planned)

1. A run is configured against a target.
2. `WorkloadRunner` applies load while `TelemetryReader` collects telemetry.
3. The results are saved through `RunStore`.
4. `Investigator` reads the saved run and produces findings.

## Checkout demo: Order Service

The checkout demo is a small, controlled application that Ruptiq will break on purpose. It is
planned to grow into several services (for example orders, inventory, payment). So far the Order
Service, the Inventory Service, and the Payment Service exist. The Order Service calls the other
two over HTTP; they call nothing.

```
demo/checkout/order-service/
  main.go            entry point: reads ORDER_SERVICE_PORT and the two service URLs, starts the
                     server, shuts down on signal
  internal/httpapi/  HTTP layer: routes, JSON in and out, status codes, server timeouts
  internal/order/    domain layer: Order type, validation rules, ID generation, in-memory Store,
                     reservation and payment use cases, the test price catalog, and the
                     InventoryReserver and PaymentProcessor interfaces
  internal/inventoryclient/  HTTP client for the Inventory Service (implements InventoryReserver)
  internal/paymentclient/    HTTP client for the Payment Service (implements PaymentProcessor)
```

Boundaries, and how they are enforced:

- **Separate from Core.** The Order Service imports nothing from `cmd/` or `internal/api`, and
  Core imports nothing from `demo/`. This is a rule we follow and review, not something the
  compiler checks. Both live in the same Go module for now, so a stray import would still compile.
- **Private to the service.** Its packages sit under `order-service/internal/`, so Go itself stops
  any code outside `order-service/` (including Core) from importing them.
- **HTTP is separate from business logic.** `httpapi` translates requests and responses; `order`
  owns what a valid order is and how orders are stored. `order` knows nothing about HTTP.
- **Storage is separate from handlers.** Handlers call `Store.Add` and `Store.Get` (and
  `Store.MarkReserved` and `Store.MarkPaid`, through the use cases). The store is an
  in-memory map guarded by a `sync.RWMutex`. A database can replace it later without touching the
  handlers' logic.
- **Order IDs** come from `crypto/rand` (128 random bits), not from a counter, so they are
  unguessable and do not collide across restarts.

Status `created` means only that the order record exists. Status `reserved` means the Inventory
Service confirmed a stock reservation (see "Order-to-inventory integration" below). Status `paid`
means the Payment Service approved a simulated payment (see "Order-to-payment integration"). An
order only moves forward: `created`, then `reserved`, then `paid`.

The service has no deliberate delays, failures, or bottlenecks yet. When we add them, they will be
explicit and configurable, and documented here.

Ruptiq does not test the Order Service yet: Core has no workload runner, telemetry reader, or
adapter. Today the service is only exercised by its own unit tests and by manual requests.

Some code is duplicated between `cmd/ruptiq-api` and the demo services (port parsing, shutdown
wiring, JSON helpers). That is deliberate: sharing it would make the targets depend on Core, or on
each other.

## Checkout demo: Inventory Service

```
demo/checkout/inventory-service/
  main.go              entry point: reads INVENTORY_SERVICE_PORT (default 8082), JSON logging,
                       graceful shutdown
  internal/httpapi/    HTTP layer: routes, strict JSON decoding, status codes, timeouts,
                       request logging
  internal/inventory/  domain layer: stock, reservations, validation, seed data, in-memory Store
```

It follows the same boundary rules as the Order Service: it imports nothing from Ruptiq Core or
from the Order Service, its packages are private under `inventory-service/internal/`, and `httpapi`
only translates between HTTP and the `inventory` package, which knows nothing about HTTP.

Endpoints: `GET /healthz`, `GET /inventory/{sku}`, and `POST /reservations`. Status codes: `201`
for a new reservation, `200` for an idempotent replay, `400` malformed body, `404` unknown SKU or
path, `405` wrong method, `409` insufficient stock or an `order_id` conflict, `413` oversized body,
`422` invalid values.

### Consistency model

All stock counters and the reservation records live in one `Store` behind one `sync.RWMutex`.
`Store.Reserve` takes the write lock and, while holding it, runs this sequence:

1. Look up a reservation for the `order_id`. If one exists with the same SKU and quantity, return
   it (replay, no change). If it differs, fail with a conflict.
2. Find the SKU. Unknown SKU fails.
3. Check `available >= quantity`. If not, fail.
4. Decrease `available`, increase `reserved`, and store the new reservation.

Steps 1 to 4 are never interleaved with another reservation, so two requests cannot both see the
last unit, and two retries of the same `order_id` cannot both create a reservation. All checks
happen before any change, so a failed request leaves no partial state. `Get` takes the read lock
and sees either the state before or after a reservation, never in between.

The `order_id` is the idempotency key, and the reservation map is keyed by it, so the idempotency
record and the reservation are one piece of data and cannot disagree. A replay is answered from
the stored reservation even if stock has since run out.

Reservation IDs (`res_` plus 128 bits from `crypto/rand`) are generated inside the lock but do not
depend on shared state, so they are unique without coordination.

### Why a single lock

It is the simplest design that is clearly correct, and it keeps all invariants in one place. It
serializes reservations across SKUs, which is acceptable at demo scale. It also gives Ruptiq a
realistic contention point to find later. Finer-grained locking would only be added if measurement
shows a need.

### Testing

Tests cover the store directly and the service over HTTP. The concurrency tests fire 100 to 200
simultaneous reservation attempts at 10 units (through the store and through a real
`httptest` server) and check that exactly 10 succeed, the rest are rejected, and stock never goes
negative. A separate test sends 100 simultaneous retries of one `order_id` and checks that stock is
decreased once. CI runs the whole suite with `go test -race` on Linux; the Windows development
machine cannot run the race detector (no 64-bit C compiler).

### Seed data and limitations

The service starts with a fixed, deterministic catalog (`LAPTOP-001`: 10, `PHONE-7`: 50,
`HEADPHONES-01`: 100). This is test data only.

A reservation means only that stock is held. There is no payment, order finalization, release,
expiry, or confirmation yet, so reserved stock is never returned. State is in memory and lost on
restart. There is no authentication and no deliberate delays or failures. The Order Service calls
`POST /reservations` over HTTP; see "Order-to-inventory integration" below.

## Checkout demo: Payment Service

```
demo/checkout/payment-service/
  main.go            entry point: reads PAYMENT_SERVICE_PORT (default 8083), JSON logging,
                     graceful shutdown
  internal/httpapi/  HTTP layer: routes, strict JSON decoding, status codes, timeouts,
                     request logging
  internal/payment/  domain layer: payment attempts, validation, idempotency, in-memory Store
```

It follows the same boundary rules as the other services: it imports nothing from Ruptiq Core or
from the Order and Inventory Services, its packages are private under `payment-service/internal/`,
and `httpapi` only translates between HTTP and the `payment` package, which knows nothing about
HTTP.

Endpoints: `GET /healthz`, `POST /payments`, and `GET /payments/{id}`. Status codes: `201` for a
new payment attempt (approved or declined), `200` for an idempotent replay, `400` malformed body,
`404` missing payment or path, `405` wrong method, `409` idempotency conflict or order already
paid, `413` oversized body, `422` invalid values.

### It is a simulation

There is no payment provider, credentials, or card data. `payment_method` is a test switch:
`test_approve` yields `approved`, `test_decline` yields `declined`. A decline is a business
outcome, so it is stored and returned like any payment (`201`), not reported as a server error.
The point of the service is to be a correctness-sensitive dependency: duplicate and concurrent
payment attempts are a classic failure area, which makes it a good target for Ruptiq. Sensitive
data is kept out by design: unknown JSON fields (such as a card number) are rejected, and the
service logs identifiers and outcomes only, never request bodies or idempotency keys.

### Consistency model

The store keeps three indexes behind one `sync.RWMutex`: payments by ID, attempts by idempotency
key (the normalized request plus the resulting payment ID), and approved orders (order ID to the ID
of its approved payment). `Store.Create` takes the write lock and, while holding it:

1. Looks up the idempotency key. If it exists with an identical request, return the original
   payment (`200`). If the request differs, fail with a conflict (`409`).
2. Checks whether the order already has an approved payment. If so, fail (`409`).
3. Creates the payment with a `crypto/rand` ID and the outcome the payment method selects.
4. Updates all indexes together: payment by ID, the key's attempt record, and, for an approval,
   the approved-order entry.

Step 1 runs before step 2 so a legitimate retry of the approving request still gets its original
answer after the order is paid. Steps 1 to 4 are one critical section, so:

- Concurrent identical retries: one request creates the payment; every other request sees the
  key and gets the same payment back.
- Concurrent attempts with different keys for one order: one creates the approval; every other
  sees the order is paid and is rejected. There is no window between "is it paid?" and "mark it
  paid" for a second approval to slip through.
- All checks happen before any write, so a rejected request leaves no partial state, and rejected
  attempts are not recorded (their keys stay free).

Key scope: keys are global, not per order. A key reused with a different order is a conflict.
Requests are compared after whitespace trimming, on `order_id`, `amount_cents`, `currency`, and
`payment_method`.

A decline never marks the order as paid, so declines can be followed by new attempts. Once an
order is paid, even a decline attempt with a new key is rejected, because the order is settled.

### Scope of the guarantees

These guarantees hold **within one running process only**. State is in memory, so a crash or
restart forgets every key and approval, and a retry afterwards could approve the order again. Two
replicas behind a load balancer would each hold separate state and could each approve the same
order. Real protection across restarts and replicas needs shared durable storage with a uniqueness
constraint (for example on the idempotency key and on one approval per order), which is outside
the current scope. One lock also means payments for different orders do not run in parallel, which
is acceptable at demo scale.

### Testing

Tests cover the store directly (including its internal indexes after each scenario) and the
service over HTTP. Concurrency tests run 100 to 200 simultaneous requests: identical retries
(exactly one creation, the rest replays returning the same payment), distinct keys for one order
(exactly one approval, no duplicate approved records), many simultaneous declines (all recorded),
and a mixed workload across several orders that checks the indexes stay consistent. Tests assert on
counts and relationships, not on fixed IDs or exact timestamps. CI runs them with `go test -race`
on Linux.

### Called by the Order Service

The Order Service pays for reserved orders through `POST /payments` (see "Order-to-payment
integration" below). Failure handling across the three services (for example releasing a
reservation after a decline, or recovering when the Order Service crashes midway) is a design
problem for a later task and does not exist today.

## Order-to-inventory integration

`POST /orders/{id}/reserve` on the Order Service reserves the order's stock by calling the
Inventory Service's public `POST /reservations` API over HTTP/JSON. This is the first call between
checkout services.

```
client ── POST /orders/{id}/reserve ──> Order Service ── POST /reservations ──> Inventory Service
                                         {order_id, sku, quantity}
        <── 200 order (status reserved, <── 201/200 reservation (id, status reserved)
            reservation_id)
```

### Independence

The services stay independently deployable: the Order Service imports no Inventory Service
package and knows it only by URL (`INVENTORY_SERVICE_URL`, default `http://localhost:8082`). Its
client mirrors the few JSON fields it needs. Either service can be started, stopped, or replaced
alone. Core is untouched and still depends on nothing in `demo/`.

### Layers inside the Order Service

```
internal/httpapi/          POST /orders/{id}/reserve: maps results and errors to status codes
internal/order/            Service.Reserve: the use case, plus the InventoryReserver interface
                           and the error values it understands. No HTTP.
internal/inventoryclient/  Client: implements InventoryReserver with net/http and JSON
```

`order.InventoryReserver` is a one-method interface (`Reserve(ctx, req) (reservationID, error)`),
defined in the package that uses it. Business logic depends on the interface; only `main.go`
knows the HTTP implementation. Tests substitute fakes at both levels.

### Reservation flow and safety rules

1. Load the order. Missing: `404`, no upstream call.
2. If it is already `reserved`, return it as is, with no upstream call.
3. Call the Inventory Service with `order_id` = the order ID. **No lock is held** during this call.
4. Only on a confirmed success (`201` or `200` with a body whose `order_id`, `sku`, `quantity`, and
   `status` match the request), record the reservation with `Store.MarkReserved`.

`MarkReserved` is a compare-and-set under the store's lock: it moves the order from `created` to
`reserved` only if it is still `created`. So when concurrent requests for one order all finish,
exactly one records the reservation, the others leave it alone, and nothing can move an order back
to `created` or replace its reservation ID. Concurrent requests may each call the Inventory
Service, but they all carry the same order ID, so the Inventory Service creates one reservation and
returns it to all of them.

**Ambiguous failures never mark an order reserved.** If the connection fails, times out, or the
response is malformed, the Inventory Service may or may not have reserved stock, but the Order
Service has no confirmation, so the order stays `created`. Recovery is a retry of the same call:
because the order ID is the idempotency key, the Inventory Service returns the existing
reservation (`200`) instead of reserving again, and the order then becomes `reserved`. The Order
Service never retries automatically, and never generates a new key for a retry.

### HTTP client behavior

- `net/http` client with a 5-second overall timeout; the incoming request's context is passed
  through, so a client disconnect or server shutdown also cancels the upstream call.
- Redirects are not followed, the response body is always closed, and at most 64 KiB is read.
- The base URL is validated at startup (absolute `http`/`https`, host, no path, query, fragment,
  or credentials). A bad value stops the service instead of failing on every request.

| Inventory Service outcome | Order Service error | Order Service response |
| --- | --- | --- |
| `201`/`200` with a matching reservation | (success) | `200`, order `reserved` |
| `409` `insufficient stock` | `ErrInsufficientStock` | `409` |
| `409`, any other message | `ErrInventoryConflict` | `409` |
| `404` `unknown sku` | `ErrUnknownSKU` | `422` |
| Connection refused or reset, `5xx`, cancelled | `ErrInventoryUnavailable` | `503` |
| Timeout | `ErrInventoryTimeout` | `504` |
| Anything else (malformed or mismatched body, other statuses, bare `404`) | `ErrInventoryBadResponse` | `502` |

Business errors (stock, SKU, conflict) are definite answers. The last three rows are failures to
get an answer, and the response says the order is unchanged and the call can be retried.

### Testing

- `inventoryclient`: an `httptest` server checks the exact request sent and every response
  mapping, including malformed and mismatched bodies, a truncated response, a server that never
  answers (timeout), a caller deadline, and a closed port (connection refused).
- `order`: the use case with an in-memory fake, including failures leaving the order unchanged,
  100 concurrent reserve calls producing one reservation, and the compare-and-set never letting a
  later writer replace the first reservation.
- `httpapi`: the full Order Service handler using the real client against an `httptest` fake of
  the Inventory Service that keeps stock and deduplicates by `order_id`. It covers success, stored
  reservation details, identical retries, 100 concurrent retries (one reservation, stock decreased
  once), each failure mode, and recovery from a dropped connection after the Inventory Service had
  already reserved (order stays `created`, then a retry returns the original reservation without
  reserving again).

### Limitations

- Both services keep state in memory. If the Inventory Service restarts, `reserved` orders keep a
  reservation ID it no longer knows and its stock is reset. If the Order Service restarts, its
  orders are lost but their reservations stay held.
- Reservations are never released or expired, including for abandoned orders.
- An already-`reserved` order is answered locally, so a repeat call does not detect a lost
  reservation after an Inventory Service restart.
- The Inventory Service has no machine-readable error codes, so the client distinguishes
  "insufficient stock" and "unknown sku" by their documented messages. A wording change there would
  turn those into `409` conflict and `502` bad response respectively.
- One fixed timeout, no automatic retry, no circuit breaker or rate limiting, and a single
  configured URL.

## Order-to-payment integration

`POST /orders/{id}/pay` on the Order Service takes a simulated payment for a reserved order by
calling the Payment Service's public `POST /payments` API over HTTP/JSON.

```
client ── POST /orders/{id}/pay ──────> Order Service ── POST /payments ─────────> Payment Service
          {payment_method,                {order_id, amount_cents (computed),
           idempotency_key}                currency USD, payment_method,
                                           idempotency_key "<order id>:<key>"}
        <── 200 {order, payment}  <────────────────────── 201 new / 200 replay {id, status, ...}
```

The same independence rules as for inventory apply: no Payment Service package is imported, the
service is known only by `PAYMENT_SERVICE_URL` (default `http://localhost:8083`), and Core is
untouched. `order.PaymentProcessor` is a one-method interface defined in `order`;
`paymentclient.Client` implements it, and `order.Payer` is the use case.

### Inputs the Order Service trusts, and those it does not

- The client chooses only the **test outcome** (`test_approve`/`test_decline`) and an
  **idempotency key** per attempt. The strict decoder rejects any other field, so no card data or
  amount can be submitted.
- The **amount** is always computed server-side: quantity times a unit price from a fixed
  test-only catalog in `order/pricing.go`, in USD integer cents, with overflow and unknown-SKU
  checks. Prices live in the Order Service because the Inventory Service has none; they are demo
  data, not a pricing system.
- The **upstream key** is `<order id>:<client key>`. Payment Service keys are global, so scoping
  stops two orders that both use `attempt-1` from colliding, and stops one order's key from
  replaying another order's payment. Order IDs contain no `:`, so the mapping is unambiguous.
  The Payment Service limits keys to 128 bytes, so `Pay` checks the length of the whole combined
  key, not just the client's part (at most 64 bytes), and rejects an over-long key with `422`
  before any upstream call. With today's 30-byte order IDs the combined key is at most 95 bytes.

### Payment flow and safety rules

1. Validate `payment_method` and `idempotency_key` (`422`). Load the order (`404`).
2. `created`: `409`, no upstream call. Only reserved orders can pay.
3. Compute the amount (`422` if the SKU has no price).
4. `paid`: answer locally, with no upstream call. The same key with `test_approve` returns the
   recorded approval; another key is `ErrOrderAlreadyPaid` (`409`); the same key with
   `test_decline` is a key conflict (`409`). So a paid order never produces a new payment attempt.
5. `reserved`: call the Payment Service. **No lock is held** during this call.
6. The client accepts only `201` or `200` whose body has an ID and matches the request's order,
   amount, currency, and the outcome the method selects. Anything else is an error.
7. **Declined**: return the decline with the current order (still `reserved`). It is not an error.
8. **Approved**: `Store.MarkPaid`, a compare-and-set under the store lock that moves `reserved` to
   `paid` and stores the payment ID and client key. It never overwrites an existing approval and
   never moves a `paid` order back. If the order turns out to be paid by a *different* payment
   (only possible if the Payment Service lost its state), the recorded payment is kept, the event
   is logged, and the request fails with `409` (`ErrPaymentRecordConflict`).

**Concurrency.** Concurrent requests for one order may each reach the Payment Service, which
allows one approval per order and deduplicates identical keys, so:

- distinct keys: one approval, and every other request gets `409` (answered locally once the order
  is paid, or from the Payment Service's "order already has an approved payment");
- identical keys: one payment; every request gets `200` with the same payment (replays).

**Ambiguous failures never mark an order paid.** Timeouts, dropped connections, `5xx`, and
malformed or mismatched responses leave the order `reserved` and return `504`, `503`, or `502`
saying the outcome is unknown and the same key can be retried. Retrying with the same key makes the
Payment Service replay the recorded result, which is then recorded on the order.

**Unrecorded upstream approval.** If the approval's response was lost and the client then uses a
*different* key, the Payment Service refuses a second approval (`409 order already has an approved
payment`). The Order Service first re-reads the order: a concurrent request may just have recorded
the approval, in which case it answers as for a paid order. Otherwise it cannot recover the
payment itself, because the Payment Service has no lookup by order. It returns `409` stating that
an approval exists and must be recovered by retrying with the key that produced it, and the order
stays `reserved`. This is the documented "reconciliation cannot be done safely" case; no approval
is invented and none is duplicated.

### Payment client behavior

Same hygiene as the inventory client: 5-second timeout, caller's context propagated, redirects not
followed, at most 64 KiB read, base URL validated at startup, no automatic retries.

| Payment Service outcome | Order Service error | Order Service response |
| --- | --- | --- |
| `201`/`200`, matching, approved | (success) | `200`, order `paid` |
| `201`/`200`, matching, declined | (success) | `200`, order still `reserved` |
| `409` `order already has an approved payment` | `ErrPaymentApprovedElsewhere` | `409` (or a local replay if the order is now paid) |
| `409` `idempotency_key was already used with different payment details` | `ErrPaymentKeyConflict` | `409` |
| Connection refused or reset, `5xx`, cancelled, truncated body | `ErrPaymentUnavailable` | `503` |
| Timeout | `ErrPaymentTimeout` | `504` |
| Anything else (malformed or mismatched body, other `409`, `4xx`, unexpected `2xx`) | `ErrPaymentBadResponse` | `502` |

### Testing

- `order`: pricing and overflow; approval, decline, decline then approval with a new key; local
  replays and conflicts for paid orders (with no upstream call); rejections before any upstream
  call; every failure leaving the order unchanged; recovery of an approval whose response was lost;
  the record-conflict path; `MarkPaid` compare-and-set; 100 concurrent distinct-key and 100
  identical-key payments (one approval each).
- `paymentclient`: an `httptest` server checks the exact request and every response mapping,
  including mismatched order, amount, currency, and outcome, oversized and truncated bodies,
  redirects, timeout, caller deadline, and connection refused.
- `httpapi`: the full handler with the real inventory and payment clients against `httptest` fakes
  of both services (the payment fake has global keys and one approval per order). It covers
  approval and stored state, decline then approval, identical replay without upstream traffic,
  conflicting attempts, per-order key scoping, 100 concurrent distinct-key calls (one `200`, 99
  `409`, one upstream approval), 100 concurrent identical-key calls (all `200`, one payment),
  timeout, service down, not configured, a dropped connection after approval followed by a
  rejected different key and a successful same-key recovery, malformed and mismatched responses,
  and request validation (including `card_number` and `amount_cents` rejected).

### Limitations

- A lost approval is recovered only when the client retries with the original key (see above).
- Restarts: Payment Service state is in memory, so after it restarts a retry could approve again
  and a `reserved` order could be approved twice across the restart. Order Service restarts lose
  paid orders.
- No cancellation, compensation, refund, or reservation release.
- The price catalog is fixed test data inside the Order Service.
- The two Payment Service `409` cases are told apart by their documented messages.

## Checkout demo containers (Docker Compose)

`compose.yaml` (repository root) runs the three checkout services as one environment. It packages
the existing binaries unchanged; no business logic was modified for containers.

```
host (127.0.0.1 only)          Compose network "ruptiq-checkout_default"
  :8081 ──> order-service ──── http://inventory-service:8082 ──> inventory-service
                          └─── http://payment-service:8083 ────> payment-service
  :8082 ──────────────────────────────────────────────────────> inventory-service
  :8083 ──────────────────────────────────────────────────────> payment-service
```

- **Images.** One multi-stage `demo/checkout/Dockerfile`, parameterized by the `SERVICE` build
  argument (the service's directory name). Build stage: `golang:1.27-alpine`, matching `go.mod`,
  compiling a static Linux binary (`CGO_ENABLED=0`, `-trimpath`). Runtime stage: `alpine:3.24`
  plus the binary. Alpine was chosen over a shell-less image so that its busybox `wget` can run the
  health checks without adding code or packages to the services.
- **Independence from Core.** The Dockerfile lives under `demo/checkout/`, and the root
  `.dockerignore` is an allow-list that sends only `go.mod` and `demo/checkout/` (minus tests) to
  the builder. Core code cannot enter these images, and Core has no Docker setup.
- **Service discovery.** The Order Service gets `INVENTORY_SERVICE_URL` and `PAYMENT_SERVICE_URL`
  set to Compose service names. The existing URL validation accepts them unchanged.
- **Health and startup.** Each service is checked with `GET /healthz` (every 5 s, 3 s timeout, 5
  retries, 10 s start period). The Order Service `depends_on` the other two with
  `condition: service_healthy`. This only orders startup: a healthy check means the process
  answers HTTP, not that checkout works end to end, and it does not keep watching dependencies
  after startup.
- **Hardening.** Non-root user (UID 10001), read-only root filesystem, all capabilities dropped,
  `no-new-privileges`, host ports bound to `127.0.0.1`. The services write no files, so the
  read-only filesystem needs no exceptions. `stop_grace_period` is 15 s, above the services' 10 s
  graceful shutdown; the binary is PID 1 (exec-form `ENTRYPOINT`), so it receives SIGTERM directly.
- **No automatic restart** (`restart: "no"`). All state is in memory, so an automatic restart would
  silently replace a crashed service with an empty one; a stopped container is easier to notice.

### State and restarts

Nothing is persisted. Restarting or recreating any container loses its state: orders, inventory
reservations (stock returns to seed values), payments, and idempotency records. The cross-service
guarantees from the integration sections hold only while all three processes keep running. After a
Payment Service restart, retrying an already-approved key creates a second approved payment; this
was demonstrated with the same binaries run as plain processes (approved `201`, replay `200`,
restart, same request `201` with a new payment ID). Durable storage is out of scope for now.

### Verification status

The Compose file was loaded and schema-validated with `compose-spec/compose-go` v2.16.2 (the
library behind `docker compose config`), and the build step was reproduced by cross-compiling all
three services for Linux from a copy of exactly the files the `.dockerignore` admits. The images
have **not** been built and the environment has **not** been run with Docker yet, because Docker
was unavailable on the development machine.

Real container verification is delegated to CI. The `docker-checkout` job in
`.github/workflows/ci.yml` runs on `ubuntu-latest`, in parallel with the Go job:

1. Print Docker, Compose, curl, and jq versions (fails if any is missing).
2. `docker compose config --quiet`, then `docker compose build`.
3. `docker compose up --detach --wait --wait-timeout 120`: Compose polls the health checks and
   fails if any service exits or is not healthy within 120 s. No fixed sleeps.
4. `bash scripts/checkout-smoke-test.sh`: real HTTP checkout scenarios with exact status and body
   assertions (successful checkout, idempotent repeats, decline then approval).
5. On failure: `docker compose ps --all`, each container's health check history, and all logs.
6. Always: `docker compose down --volumes --remove-orphans --rmi all`.

The workflow keeps `permissions: contents: read`, and the job has a 20-minute timeout. A passing
health check only means each process answers `/healthz`; the smoke test is what shows that a
checkout works across the three containers.

## What exists today

- `cmd/ruptiq-api`: Ruptiq Core program entry point.
- `internal/api`: Ruptiq Core HTTP server with `GET /healthz`.
- `demo/checkout/order-service`: the Order Service (`GET /healthz`, `POST /orders`,
  `GET /orders/{id}`, `POST /orders/{id}/reserve`, `POST /orders/{id}/pay`), with in-memory
  storage. It reserves stock by calling the Inventory Service and takes simulated payments by
  calling the Payment Service, both over HTTP.
- `demo/checkout/inventory-service`: the Inventory Service (`GET /healthz`,
  `GET /inventory/{sku}`, `POST /reservations`), with in-memory stock and idempotent,
  concurrency-safe reservations.
- `demo/checkout/payment-service`: the Payment Service (`GET /healthz`, `POST /payments`,
  `GET /payments/{id}`), with simulated, idempotent payments and in-memory storage.

- `compose.yaml` and `demo/checkout/Dockerfile`: a Docker Compose environment for the three
  checkout services (configuration validated, not yet run with Docker).

The four Core components above, cancellation and compensation across services, persistence, and
any adapters are not implemented.

## Decisions deferred

- Where each further checkout demo service lives (under `demo/checkout/`, separate from Core).
- Whether the demo eventually gets its own Go module (it shares Core's module for now).
- Storage technology for `RunStore`.
- How load is generated.
- The adapter mechanism for external targets.
