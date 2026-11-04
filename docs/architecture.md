# Ruptiq Architecture

> **Status: planned design.** Only the HTTP server with `GET /healthz` is implemented today.
> Nothing described under "Planned boundaries" exists in code yet, and no interfaces have been
> written. This document records the intended direction so early code does not paint us into a
> corner.

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
2. **The checkout demo is just a target.** It lives apart from Core (its own directory and its
   own entry point when it is built) and is the first system Ruptiq tests. It has no special
   access to Core internals.
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

## What exists today

- `cmd/ruptiq-api`: program entry point.
- `internal/api`: HTTP server with `GET /healthz`.

The four components above, the checkout demo, and any adapters are not implemented.

## Decisions deferred

- Where the checkout demo lives in the repository (it must stay separate from Core).
- Storage technology for `RunStore`.
- How load is generated.
- The adapter mechanism for external targets.
