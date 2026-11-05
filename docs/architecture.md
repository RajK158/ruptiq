# Ruptiq Architecture

> **Status: mostly planned design.** Implemented today: the Ruptiq Core health-check server, and
> two checkout demo services (the Order Service and the Inventory Service, described below). Nothing described under
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
Service and the Inventory Service exist; they do not call each other.

```
demo/checkout/order-service/
  main.go            entry point: reads ORDER_SERVICE_PORT, starts the server, shuts down on signal
  internal/httpapi/  HTTP layer: routes, JSON in and out, status codes, server timeouts
  internal/order/    domain layer: Order type, validation rules, ID generation, in-memory Store
```

Boundaries, and how they are enforced:

- **Separate from Core.** The Order Service imports nothing from `cmd/` or `internal/api`, and
  Core imports nothing from `demo/`. This is a rule we follow and review, not something the
  compiler checks. Both live in the same Go module for now, so a stray import would still compile.
- **Private to the service.** Its packages sit under `order-service/internal/`, so Go itself stops
  any code outside `order-service/` (including Core) from importing them.
- **HTTP is separate from business logic.** `httpapi` translates requests and responses; `order`
  owns what a valid order is and how orders are stored. `order` knows nothing about HTTP.
- **Storage is separate from handlers.** Handlers call `Store.Add` and `Store.Get`. The store is an
  in-memory map guarded by a `sync.RWMutex`. A database can replace it later without touching the
  handlers' logic.
- **Order IDs** come from `crypto/rand` (128 random bits), not from a counter, so they are
  unguessable and do not collide across restarts.

Status `created` means only that the order record exists. It says nothing about inventory or
payment, because neither is implemented. Those services, and the calls to them, do not exist yet.

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
restart. There is no authentication and no deliberate delays or failures. The Order Service does
not call the Inventory Service; that integration is a later step.

## What exists today

- `cmd/ruptiq-api`: Ruptiq Core program entry point.
- `internal/api`: Ruptiq Core HTTP server with `GET /healthz`.
- `demo/checkout/order-service`: the Order Service (`GET /healthz`, `POST /orders`,
  `GET /orders/{id}`), with in-memory storage.
- `demo/checkout/inventory-service`: the Inventory Service (`GET /healthz`,
  `GET /inventory/{sku}`, `POST /reservations`), with in-memory stock and idempotent,
  concurrency-safe reservations.

The four Core components above, the other checkout demo services, and any adapters are not
implemented.

## Decisions deferred

- Where each further checkout demo service lives (under `demo/checkout/`, separate from Core).
- Whether the demo eventually gets its own Go module (it shares Core's module for now).
- Storage technology for `RunStore`.
- How load is generated.
- The adapter mechanism for external targets.
