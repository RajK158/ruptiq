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

Item 1 has started: Ruptiq can run a short HTTP GET load test with k6, save its results as a JSON
file, and compare two saved results, but has no run history yet. Item 2 has started: the Inventory
Service exposes Prometheus metrics, and Ruptiq Core can read one snapshot of any Prometheus
endpoint or sample one periodically while a k6 load test runs, saving both in one experiment
report, and turn the samples into rates, 5xx shares, and estimated percentiles. Item 3 has started:
`ruptiq-investigate` ranks evidence-backed observations of degradation within one experiment and
abstains when the evidence is inadequate, but it does not identify causes. It has been run on a
controlled live change in `demo/regression-lab/` as well as on synthetic data. Item 4 is in progress
(see below). See
[docs/architecture.md](docs/architecture.md) for how these pieces are planned to fit together.

## Current implementation status

Ruptiq Core has a health-check server; `ruptiq-run`, a command that runs one HTTP GET load test
with [k6](https://k6.io) against any URL and reports what k6 measured; `ruptiq-compare`, which
compares two saved `ruptiq-run` results offline; `ruptiq-telemetry`, which reads one snapshot
of a target's Prometheus metrics; `ruptiq-experiment`, which runs a k6 load test while
sampling a Prometheus endpoint and saves both in one report; `ruptiq-analyze`, which
calculates server-side throughput, 5xx rates, and estimated latency percentiles from a saved
experiment report, offline; and `ruptiq-investigate`, which ranks evidence-backed findings
(observations and hypotheses to test, not root causes) from a saved analysis, offline. The
checkout demo has three
services: the Order Service, the Inventory Service, and the Payment Service. The Order Service
reserves stock by calling the Inventory Service over HTTP, then takes a simulated payment by calling
the Payment Service over HTTP. The Inventory Service exposes Prometheus metrics at `GET /metrics`.

| Item | Status |
| --- | --- |
| Go module and repository layout | Done |
| `ruptiq-api` HTTP server (standard library only) | Done |
| `GET /healthz` health endpoint on `ruptiq-api` | Done (automated test, and checked against a running server) |
| `PORT` setting and server timeouts | Done (`PORT` parsing is unit tested) |
| Graceful shutdown on Ctrl+C / SIGTERM (all three servers) | Implemented, **not yet verified** by a real Ctrl+C run |
| CI workflow (`go test`, `go vet`, `go test -race`) | Done; passing on GitHub Actions (Linux), including the race detector |
| Checkout demo: Order Service | Done (in-memory storage, see below) |
| Checkout demo: Inventory Service | Done (in-memory stock and reservations) |
| Checkout demo: Payment Service | Done (simulated payments, in-memory) |
| Order-to-inventory integration (`POST /orders/{id}/reserve`) | Done (HTTP/JSON, idempotent by order ID, see below) |
| Order-to-payment integration (`POST /orders/{id}/pay`) | Done (HTTP/JSON, simulated payments, idempotent by client key, see below) |
| Docker Compose environment for the checkout demo (`compose.yaml`) | Done; built and run with Docker by CI on GitHub Actions (Docker is not installed on the development machine) |
| Docker checkout integration test in CI (`scripts/checkout-smoke-test.sh`) | Done; passing on GitHub Actions against the three containers |
| Load test runner (`ruptiq-run`, k6 adapter) | Done for one HTTP GET workload; unit tested with a fake k6, and run against the real k6 v2.3.0 (see below) |
| Offline comparison of two saved runs (`ruptiq-compare`) | Done; descriptive only (no significance testing, no diagnosis); unit tested, and run on real saved k6 results (see below) |
| Inventory Service Prometheus metrics (`GET /metrics`) | Done; unit tested and inspected on a running service. Smoke-test checks added; they first run in CI on the pull request (Docker is not installed on the development machine) |
| Prometheus telemetry reader (`ruptiq-telemetry`, one snapshot) | Done; unit tested against local test servers, and run against the real Inventory Service (see below) |
| Experiment: k6 load test with periodic telemetry sampling (`ruptiq-experiment`) | Done for one HTTP GET workload and one Prometheus endpoint; unit tested with fake runners and readers, and run with the real k6 v2.3.0 against the real Inventory Service (see below) |
| Offline performance signals from a saved experiment (`ruptiq-analyze`) | Done for HTTP request counters and duration histograms; unit tested with synthetic experiment reports and the experiment package's saved report. Not yet run on a report from a real Inventory Service experiment |
| Evidence-driven investigation of a saved analysis (`ruptiq-investigate`) | Done for 5xx increases, latency and throughput degradation within one experiment, and abstention; deterministic, unit tested with fake analyses, run offline on the synthetic experiment, and run on a live controlled change from `demo/regression-lab/` (see below) |
| Before/after comparison of two saved analyses (`ruptiq-validate-fix`) | Done for server throughput, 5xx ratio, and median per-window p95; deterministic and unit tested offline, including the synthetic inventory analysis, and run on two live regression-lab pairs (slow then fast, then fast then slow). An observed difference is not a cause, not a software patch, and not a statistical result. Reports stay in gitignored `tmp/` |
| Controlled regression lab (`demo/regression-lab`) | Done as a loopback-only validation fixture (real CPU work, Prometheus metrics, baseline→degraded switch). Unit tested without k6. One live pipeline run is recorded in README.md; generated reports are not in Git |
| Cancellation, compensation, reservation release | Not started |
| Telemetry for the Order and Payment Services | Not started |
| Root-cause diagnosis, run storage, load ramps | Not started |

## Requirements

- Go 1.27 or newer (see the `go` line in `go.mod`).
- Network access the first time you build or test, so Go can download the official Prometheus Go
  libraries (`github.com/prometheus/client_golang`, `client_model`, and `common`). The Inventory
  Service uses the client library to expose metrics; Ruptiq Core uses only the text-format parser
  (`common/expfmt`) and its data model (`client_model`) to read them. `go.sum` pins every module's
  checksum.
- Optional, for the containerized checkout demo: Docker with the Compose plugin (Docker Desktop
  on Windows or macOS, or Docker Engine with `docker compose` on Linux).
- Optional, for load tests with `ruptiq-run` and `ruptiq-experiment`:
  [k6](https://grafana.com/docs/k6/latest/set-up/install-k6/) (developed against v2.3.0). Ruptiq's
  tests do not need it.

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

## Run a load test (`ruptiq-run`)

`ruptiq-run` sends HTTP GET requests to one URL for a fixed time with a fixed number of virtual
users, using k6 as the load generator, and reports what k6 measured. It works against any HTTP
service; it knows nothing about the checkout demo.

### Install k6

k6 is a separate program; Ruptiq starts it and reads its results. Install it with one of:

| Platform | Command |
| --- | --- |
| Windows | `winget install k6 --source winget` or `choco install k6` |
| macOS | `brew install k6` |
| Linux | the package for your distribution, see the [k6 install guide](https://grafana.com/docs/k6/latest/set-up/install-k6/) |
| Any | download a release archive from [github.com/grafana/k6/releases](https://github.com/grafana/k6/releases) and unpack `k6` (`k6.exe`) anywhere |

Check it with `k6 version`. If k6 is not on `PATH`, give its location with `-k6 <path>` or the
`RUPTIQ_K6_PATH` environment variable.

### Example: the Inventory Service

`GET /inventory/{sku}` only reads stock, so load-testing it changes nothing. Start the Inventory
Service (`go run ./demo/checkout/inventory-service`, or `docker compose up -d`), then:

```powershell
go run ./cmd/ruptiq-run -url http://localhost:8082/inventory/LAPTOP-001
go run ./cmd/ruptiq-run -url http://localhost:8082/inventory/LAPTOP-001 -vus 5 -duration 10s -out run.json
go run ./cmd/ruptiq-run -url http://localhost:8082/healthz -k6 "C:\tools\k6\k6.exe"
```

Only point `ruptiq-run` at endpoints that are safe to call thousands of times: it sends GET
requests as fast as the target answers, and nothing stops you from aiming it at a URL that changes
data. Do not run it against systems you do not own.

A real run against the Inventory Service on the development machine (k6 v2.3.0, Windows, service
run as a plain process, defaults of 2 virtual users for 5 seconds):

```
Running 2 virtual users for 5s against http://127.0.0.1:8082/inventory/LAPTOP-001 (timeout 1m5s)...

Ruptiq load test report (k6 v2.3.0)
  Target:            http://127.0.0.1:8082/inventory/LAPTOP-001
  Load:              2 virtual users, 5s configured, 5.00s measured
  HTTP requests:     18200 (3639.2 per second)
  Failed requests:   0 (0.00%)
  Latency (ms):      avg 0.458   p50 0.530   p95 0.744
                     min 0.000   p90 0.637   max 78.091
  Result:            PASSED (failure rate at most 0.00%)
```

Stock read afterwards was unchanged (`available 10, reserved 0`). These numbers describe that one
machine at that moment; they are not a benchmark.

### Options

| Flag | Default | Meaning |
| --- | --- | --- |
| `-url` | (required) | Target URL. Must be absolute `http` or `https`, at most 2048 characters, with no fragment, spaces, or control characters. |
| `-vus` | `2` | Concurrent virtual users, 1 to 1000. Each one sends a request, waits for the answer, and repeats. |
| `-duration` | `5s` | How long to apply load: whole seconds from `1s` to `1h`. |
| `-timeout` | duration + 60s | Hard limit for the whole run. k6 is stopped when it expires. Must be longer than `-duration`. |
| `-k6` | `RUPTIQ_K6_PATH`, else `k6` | The k6 executable, by name (looked up on `PATH`) or path. |
| `-out` | (none) | Also write the results as JSON to this file. |
| `-max-failure-rate` | `0` | Highest share of failed requests (0 to 1) for the run to pass. |

All flags are checked before k6 starts. Ctrl+C stops k6 and the run.

### What the numbers mean

All values come from k6's own summary of the run; Ruptiq computes only requests per second.

- **HTTP requests**: every request k6 sent, including failed ones.
- **Failed requests / failure rate**: requests that got no response (connection refused, DNS
  failure, the 10-second per-request timeout) or a status outside `200`-`399`, as a count and a
  share of all requests.
- **Requests per second**: HTTP requests divided by the measured duration.
- **Measured duration**: how long k6 reports the run took; slightly longer than configured,
  because in-flight requests finish after the configured time.
- **Latency (ms)**: the time from sending a request to receiving the whole response, in
  milliseconds, over all requests (failed requests included, often as `0`). `p50` is the median;
  `p95` means 95% of requests were at least this fast. A `min` of `0.000` comes from requests
  that failed without a response, or, as in the run above (no failures), from very fast local
  requests that k6 timed as 0 ms on Windows.

### Pass, fail, and exit status

k6 itself reports success even when every request fails, so Ruptiq judges the run: it **passes**
only if it made at least one request and its failure rate is at most `-max-failure-rate`.

| Exit status | Meaning |
| --- | --- |
| `0` | The run completed and passed. |
| `1` | The run completed but failed (for example the target refused every connection). The report lists why. |
| `2` | Invalid flags; k6 was not started. |
| `3` | The run could not be completed: k6 missing, k6 failed (for example exit status `107`), the timeout expired, or k6's results were missing or unreadable. No metrics are reported. |

With `-out`, the file is written for every run that started, so an old successful result is never
left behind after a failure. It contains `schema_version`, `run` (the settings, start and finish
times, and the metrics above, using the same names), and `verdict`; or `error` and a failed
`verdict` if the run could not complete.

### Safety

- k6 is started directly, without a shell. The target URL is passed to k6 in an environment
  variable, not on its command line, so it does not appear in process listings.
- The k6 script is a fixed file built into Ruptiq (`internal/workload/k6/scripts/http_get.js`). It
  is written to a fresh temporary directory for each run and deleted afterwards, with k6's summary
  file.
- The target URL is shown and saved with its user name and password, and its **whole query
  string**, each replaced by `xxxxx`: `https://alice:pw@example.com/p?api_key=K&page=2` appears as
  `https://xxxxx@example.com/p?xxxxx`. This applies to everything Ruptiq prints or writes,
  including k6's own output in error messages. k6 itself still requests the complete original URL.
- Inherited `K6_*` environment variables are ignored, so settings such as `K6_VUS` cannot change a
  run behind Ruptiq's back.
- Only the last 4 KiB of k6's output is kept, for error messages, starting at a whole line.

### Limitations

- One workload only: GET requests to one URL at a constant number of virtual users. No ramps, no
  other methods or request bodies, no custom headers, no think time between requests.
- Results are client-side only (what k6 saw). To also sample the target's Prometheus metrics
  during the run, use [`ruptiq-experiment`](#run-an-experiment-ruptiq-experiment). There is no
  investigation and no run history.
- One k6 process on the local machine; no distributed load generation.
- Because the whole query string is hidden, reports cannot tell apart runs that differ only in
  their query. Secrets in the URL path itself (for example `/api/<token>/items`) are not
  recognized and are shown as given.
- The complete URL is in k6's environment while it runs, readable by the same user (and
  administrators) through process inspection tools.
- The JSON results format may still change (`schema_version` will be raised when it does).

## Compare two saved runs (`ruptiq-compare`)

`ruptiq-compare` reads two results files written by `ruptiq-run -out`, a **baseline** and a
**candidate**, and shows how the candidate's measurements differ. It only reads the two files: it
does not run k6 and does not contact the target.

### Create two saved runs

Run the same workload twice with the same settings, saving each result. Typically you change
something in between (a new build of the target, a configuration change) and run again:

```powershell
go run ./cmd/ruptiq-run -url http://localhost:8082/inventory/LAPTOP-001 -out baseline.json
# ... change the target ...
go run ./cmd/ruptiq-run -url http://localhost:8082/inventory/LAPTOP-001 -out candidate.json
```

### Compare them

```powershell
go run ./cmd/ruptiq-compare -baseline baseline.json -candidate candidate.json
go run ./cmd/ruptiq-compare -baseline baseline.json -candidate candidate.json -out comparison.json
```

| Flag | Meaning |
| --- | --- |
| `-baseline` | The saved run to compare against (required). |
| `-candidate` | The saved run being judged (required). |
| `-out` | Also write the comparison as JSON to this file (optional). It may not be one of the inputs. |

### Example: two real runs with nothing changed

These are two real `ruptiq-run` results with the default settings, taken back to back against the
same, unchanged Inventory Service on the development machine (k6 v2.3.0, Windows, service run as a
plain process):

```
Ruptiq run comparison (candidate compared with baseline)
  Target:    http://127.0.0.1:8082/inventory/LAPTOP-001
  Load:      2 virtual users, 5s configured
  Baseline:  baseline.json (k6 v2.3.0, started 2026-11-06T11:36:34Z, measured 5.00s, PASSED)
  Candidate: candidate.json (k6 v2.3.0, started 2026-11-06T11:36:39Z, measured 5.00s, PASSED)

  Metric                 Baseline   Candidate   Change    Change %           Assessment
  Average latency (ms)   0.539      0.434       -0.105    -19.5%             better (lower is better)
  p50 latency (ms)       0.536      0.529       -0.007    -1.4%              better (lower is better)
  p95 latency (ms)       1.057      0.751       -0.305    -28.9%             better (lower is better)
  Requests per second    3173.6     3873.7      +700.0    +22.1%             higher (generally better)
  HTTP failure rate      0.00%      0.00%       0.00 pp   -                  unchanged
  Failed requests        0          0           0         n/a (baseline 0)   not assessed
  Total requests         15870      19370       +3500     +22.1%             not assessed

Caveats:
  - Each side is a single run. Ruptiq does not measure run-to-run variation or statistical significance, so small differences may be noise; repeat both runs before drawing conclusions.
  - Only the target URL, virtual users, and configured duration were checked. Ruptiq cannot tell whether the target's code, data, configuration, or the machines and network changed between runs.
```

Nothing changed between these runs, yet throughput differs by 22% and p95 latency by 29%. On a
busy development machine, sub-millisecond latencies and short runs are dominated by noise. Treat
differences of this size as meaningless unless they repeat across several runs on both sides.

A comparison where the candidate got worse and had failures (from the test fixtures in
`internal/comparison/testdata`, made-up numbers, not a measurement) looks like this:

```
  Metric                 Baseline   Candidate   Change     Change %           Assessment
  Average latency (ms)   2.000      2.500       +0.500     +25.0%             worse (lower is better)
  p50 latency (ms)       1.500      1.500       0.000      0.0%               unchanged
  p95 latency (ms)       4.000      5.000       +1.000     +25.0%             worse (lower is better)
  Requests per second    2000.0     1600.0      -400.0     -20.0%             lower (generally worse)
  HTTP failure rate      0.00%      1.00%       +1.00 pp   -                  worse (lower is better)
  Failed requests        0          80          +80        n/a (baseline 0)   not assessed
  Total requests         10000      8000        -2000      -20.0%             not assessed
```

### How to read it

- **Change** is candidate minus baseline. **Change %** is that change relative to the baseline.
  It is shown as `n/a (baseline 0)` when the baseline is 0, because no relative change exists.
- **Failure rates** are compared in **percentage points** (`pp`): 0.00% to 1.00% is `+1.00 pp`.
  A relative change of a rate is not shown.
- **Assessment** describes the direction only. Lower latency and a lower failure rate are
  `better`. Higher throughput is `higher (generally better)`: with a fixed number of virtual
  users, throughput rises whenever requests finish sooner, including when they fail fast, so it
  is not proof of a better system on its own. Request counts are `not assessed`.
- There is **no overall verdict** and no threshold: the command describes differences, it does
  not decide whether the candidate is acceptable.

### Which comparisons are meaningful

Ruptiq refuses to compare runs whose **target URL, number of virtual users, or configured
duration** differ (exit status `1`), and lists every difference. Runs that completed but
**failed** their pass criteria are accepted, because comparing failure behavior is useful; the
failure is listed under the caveats.

Matching settings do not make two runs equivalent experiments. A comparison is meaningful only if
everything you did not deliberately change stayed the same: the machine running k6, the target's
host and its other load, the target's data, and the network. Ruptiq cannot check any of that, and
the caveats say so on every comparison.

**Redacted URLs.** Saved results contain the target URL with its user information and query string
replaced by `xxxxx`. Two runs against `...?region=eu` and `...?region=us` are both stored as
`...?xxxxx` and look identical, so Ruptiq cannot tell them apart; a caveat appears whenever the
stored URL is redacted. Keep track of which query you used yourself.

### Validation and exit status

Each file must be a schema version 1 `ruptiq-run` result for a **completed** run. A file recording
a run that could not complete (k6 missing, timeout) is rejected, as is any file with missing,
`null`, non-finite, or negative values, unknown fields, latency percentiles out of order, or
derived values that disagree with their inputs (for example a failure rate that is not failed ÷
total requests). Nothing missing is ever treated as zero.

| Exit status | Meaning |
| --- | --- |
| `0` | The runs were compared. |
| `1` | The runs are not comparable; no comparison was made. |
| `2` | Invalid flags. |
| `3` | A file could not be read, or is malformed, unsupported, or records an incomplete run. |

With `-out`, the JSON file is written in every case after the flags are accepted, so an older
comparison is never left behind looking current. It contains `schema_version` and either
`comparison` (both runs, every metric with `baseline`, `candidate`, `change`, `change_percent`, or
`change_percentage_points` for the failure rate, `direction`, `assessment`, and the `caveats`) or
`error` (and `mismatches` when the runs were not comparable). `change_percent` is `null` when it
does not apply, with the reason in `change_percent_note`.

### Limitations

- One run per side. No repeated runs, averages, confidence intervals, or significance tests.
- Only the summary numbers saved by `ruptiq-run` are compared; there are no per-request data or
  time series, and no telemetry from the target.
- No diagnosis: the comparison says what changed, not why.
- Redacted URLs cannot prove that two runs used the same query string or credentials.

## Read a telemetry snapshot (`ruptiq-telemetry`)

`ruptiq-telemetry` reads **one snapshot** of a target's metrics from any HTTP endpoint that serves
the Prometheus text format, prints a short summary, and can save everything as JSON. It works with
any Prometheus endpoint; it does not expect particular metric names. It does not run a load test,
and it does not collect telemetry over time: one command, one request. (`ruptiq-experiment` uses
the same reader to sample during a load test.)

```powershell
go run ./cmd/ruptiq-telemetry -url http://localhost:8082/metrics -out snapshot.json
```

| Flag | Default | Meaning |
| --- | --- | --- |
| `-url` | (required) | The metrics endpoint, `http://` or `https://`. User name and password in the URL are sent as HTTP basic authentication. |
| `-out` | none | Also write the snapshot (or the failure) as JSON to this file. |
| `-timeout` | `10s` | Maximum time for the request and the whole response. |

### Example: the Inventory Service

Start the Inventory Service (`go run ./demo/checkout/inventory-service`, or Docker Compose), make
one reservation and one stock read, then read its metrics. Real output, from the service on port
`18082`, with a user name, password, and query string added to the URL to show how they are
hidden:

```text
Endpoint:    http://xxxxx@127.0.0.1:18082/metrics?xxxxx
Scraped at:  2026-11-06T12:32:33.025Z (took 5.1 ms)
Contents:    5 families, 11 series (1 counter, 3 gauge, 1 histogram)

inventory_http_request_duration_seconds  histogram, 2 series
  {method="GET",route="/inventory/{sku}",status="200"}  count 1, sum 0, 17 buckets
  {method="POST",route="/reservations",status="201"}  count 1, sum 0.0012588, 17 buckets

inventory_http_requests_in_flight  gauge, 1 series
  (no labels)  0

inventory_http_requests_total  counter, 2 series
  {method="GET",route="/inventory/{sku}",status="200"}  1
  {method="POST",route="/reservations",status="201"}  1

inventory_stock_available_units  gauge, 3 series
  {sku="HEADPHONES-01"}  100
  {sku="LAPTOP-001"}  10
  {sku="PHONE-7"}  47

inventory_stock_reserved_units  gauge, 3 series
  {sku="HEADPHONES-01"}  0
  {sku="LAPTOP-001"}  0
  {sku="PHONE-7"}  3

Full snapshot written to ...\after.json
```

The summary shows at most 30 metric families and 8 series per family, shortens long label values,
and quotes names or values containing unusual characters. The JSON file always holds everything.
(The `sum 0` for the stock read is the Windows clock timing a very fast request as 0 seconds.)

### The JSON file

```json
{
  "schema_version": 1,
  "endpoint": "http://127.0.0.1:8082/metrics",
  "snapshot": {
    "endpoint": "http://127.0.0.1:8082/metrics",
    "scraped_at": "2026-11-06T12:32:33.025Z",
    "scrape_duration_seconds": 0.0051,
    "families": [
      {
        "name": "inventory_http_requests_total",
        "help": "HTTP requests completed by the Inventory Service, ...",
        "type": "counter",
        "series": [
          {"labels": {"method": "POST", "route": "/reservations", "status": "201"}, "value": 1}
        ]
      }
    ]
  }
}
```

(The structure, shortened to one family and one series, with values and help text abbreviated; the
real file is indented one field per line.) Families are sorted by name and series by
their labels. Each series has `labels` (omitted when there are none) and one of:

- `value` for counters, gauges, and untyped metrics;
- `histogram`: `count`, `sum`, and `buckets`, each with `upper_bound` and `cumulative_count` (the
  number of observations less than or equal to the bound), including the `+Inf` bucket;
- `summary`: `count`, `sum`, and `quantiles`.

Prometheus allows values that JSON numbers cannot hold; they are written as the strings `"NaN"`,
`"+Inf"`, and `"-Inf"`. `scraped_at` is when the request was sent. If the read fails, the file
holds `error` instead of `snapshot`, so an older snapshot is never left behind.

### Failures and exit status

| Exit status | Meaning |
| --- | --- |
| `0` | The snapshot was read. |
| `1` | It could not be read, or the `-out` file could not be written. The reason is printed, and recorded in the `-out` file. |
| `2` | Invalid flags or URL; no request was sent. |

Reasons are specific: the endpoint is unreachable, the timeout expired, the status was not `200`
(with the first 200 bytes of the response body), the response was larger than 16 MiB, the
`Content-Type` was not `text/plain` (HTML, JSON, OpenMetrics, and protobuf are rejected rather
than misread), or the body is empty or not valid Prometheus text. Examples from the real service:

```text
ruptiq-telemetry: no snapshot: metrics endpoint returned an error status: GET http://127.0.0.1:18082/nope?xxxxx: 404 Not Found: {"error":"not found"}
ruptiq-telemetry: no snapshot: metrics request failed: GET http://127.0.0.1:18082/metrics: dial tcp 127.0.0.1:18082: connectex: No connection could be made because the target machine actively refused it.
```

### Safety

- The user name, password, and whole query string of the URL are replaced by `xxxxx` in
  everything shown or saved, including error messages and echoed response bodies. The request
  itself uses the complete URL.
- The response is read up to 16 MiB after decompression; anything larger is rejected.
- Only the summary's first families and series are printed, so a large or hostile endpoint cannot
  flood the console.

### Limitations

- One snapshot per command. To sample while a load test runs, use
  [`ruptiq-experiment`](#run-an-experiment-ruptiq-experiment).
- Only the Prometheus text format (version 0.0.4). OpenMetrics and protobuf responses are
  rejected. Native histograms are not supported (they are not part of the text format).
- Histogram checks rely on the `+Inf` bucket: without one, bucket counts are not compared with
  the total count. A `le="NaN"` bucket is dropped by the parser rather than reported.
- No TLS options (custom certificates, client certificates) and no bearer tokens; basic
  authentication only, through the URL.
- Values are recorded as exposed. Nothing is converted into rates or percentiles, and counters are
  not compared between snapshots.

## Run an experiment (`ruptiq-experiment`)

`ruptiq-experiment` runs one HTTP GET load test with k6 (exactly as `ruptiq-run` does) and, while
it runs, reads a Prometheus metrics endpoint at a fixed interval (with the same reader as
`ruptiq-telemetry`). It saves k6's client-side results and every timestamped snapshot in one JSON
report. The load target and the metrics endpoint are configured separately; it works with any
HTTP target and any Prometheus text-format endpoint.

### Example: the Inventory Service

Start the Inventory Service (see [Start it](#start-it-1)), install k6 (see
[Install k6](#install-k6)), then:

```sh
go run ./cmd/ruptiq-experiment -url http://localhost:8082/inventory/LAPTOP-001 -metrics-url http://localhost:8082/metrics -vus 2 -duration 5s -interval 1s -out experiment.json
```

Real output from a run against the Inventory Service on port 18082 with the portable k6 v2.3.0
for Windows (`-k6` pointed at it):

```text
Running 2 virtual users for 5s against http://127.0.0.1:18082/inventory/LAPTOP-001,
sampling http://127.0.0.1:18082/metrics every 1s (timeout 1m5s)...

Ruptiq experiment report
  Status:          PASSED
  Target:          http://127.0.0.1:18082/inventory/LAPTOP-001
  Metrics:         http://127.0.0.1:18082/metrics (every 1s)
  Started:         2026-11-06T13:03:52.942Z (took 5.1s)
  Workload:        k6 v2.3.0, 2 virtual users, 5s configured, 5.00s measured
  HTTP requests:   17550 (3509.6 per second), 0 failed (0.00%)
  Latency (ms):    avg 0.482   p50 0.531   p95 0.920   max 25.932
  Workload result: passed (failure rate at most 0.00%)
  Telemetry:       complete: 7 of 7 scrapes succeeded (1 before, 5 during, 1 after the workload)
  Samples:         #1 at +0.0s to #7 at +5.1s (metric families in the last: 5)

Report written to experiment.json
```

The service's own counter `inventory_http_requests_total{method="GET",route="/inventory/{sku}",status="200"}`
in the saved samples, in order: absent before the load, then 3620, 7496, 11433, 14497, and 17352
during it, and 17550 after it, the same number of requests k6 reported. (One more request, under
`route="unmatched"`, was a manual check made before the experiment.) The in-flight gauge read 1 or 2
during the load and 0 after it. The report file was 56 KB.

### Options

| Flag | Default | Meaning |
| --- | --- | --- |
| `-url` | (required) | Target URL that k6 sends GET requests to. |
| `-metrics-url` | (required) | Prometheus metrics endpoint to sample. |
| `-vus` | `2` | Concurrent virtual users, 1 to 1000. |
| `-duration` | `5s` | How long to apply load, whole seconds from `1s` to `1h`. |
| `-interval` | `1s` | Time between scrapes while the load runs, `100ms` to `1m`, and not longer than `-duration`. |
| `-max-samples` | `1000` | Most scrapes in one experiment (3 to 10000), including the one before and the one after the load. Must cover `-duration` / `-interval` + 2; k6 usually runs a little longer than `-duration`, so leave room (see below). |
| `-max-telemetry-bytes` | `33554432` (32 MiB) | Retention budget: the most sample data (measured as JSON) kept from one experiment, 1 KiB to 1 GiB. |
| `-timeout` | duration + 60s | Hard limit for the whole experiment. Must be longer than `-duration`. |
| `-scrape-timeout` | `5s` | Maximum time for one scrape. |
| `-k6` | `RUPTIQ_K6_PATH`, else `k6` | k6 executable name or path. |
| `-max-failure-rate` | `0` | Highest failure rate (0 to 1) for the load test to pass. |
| `-out` | (none) | Write the experiment report as JSON to this file. |

### How sampling works

1. One scrape **before** the workload starts (the baseline).
2. k6 starts; one scrape every `-interval` **during** the workload. Scrapes are made one at a time,
   so they never overlap. Go's ticker keeps at most one pending tick: if a scrape takes longer than
   the interval, the next scrape starts as soon as it finishes, and any further ticks that fell due
   meanwhile are dropped.
3. When k6 returns, sampling stops (a scrape in progress is allowed to finish) and one more scrape
   is made **after** the workload.

Each scrape attempt gets the next sequence number and is recorded exactly once: as a sample, as an
error, or (when the retention budget is reached) as "not retained". Gaps in the samples' sequence
numbers are therefore explained by the other two lists. A scrape interrupted because the experiment
was cancelled or timed out is not counted.

**Limits.** `-max-samples` caps scrape attempts. The check that it covers `-duration` /
`-interval` + 2 assumes the workload runs for exactly `-duration`; k6 takes a little longer (start-up
and shutdown), and a workload that runs much longer than configured can still reach the cap. Then
sampling stops early, keeping the last attempt for the scrape after the workload, and the telemetry
is `partial`. `-max-telemetry-bytes` caps the sample data kept: each successful scrape is measured as
the JSON it adds to the report, and the first one that does not fit in what is left is not kept
(only its sequence, phase, time, and size are recorded). Sampling then stops, no scrape is made after
the workload, the samples already kept stay, and the telemetry is `partial`. The load test is never
stopped or failed by either limit.

**What "timestamped" means.** Every sample has `scraped_at` (when the request to the metrics
endpoint was sent) and `offset_seconds` (since the experiment started). The values in a sample are
the endpoint's values at about that moment. They are not tied to individual k6 requests: k6
measures each request on the client, the service counts it when its handler returns, and scrapes
happen on their own schedule. "During the workload" means while the k6 process was running,
including its start-up and shutdown, so the first and last of those samples can see little load.
Compare samples by their timestamps, not as an exact per-request match.

### The report

`schema_version` 1. Shortened example from the run above, which was made before the retention
budget was added; current reports also have `config.max_telemetry_bytes` and
`telemetry.retained_bytes`, `retention_limit_reached`, and `not_retained` (described below). The
real file has every family in every sample:

```json
{
  "schema_version": 1,
  "status": "passed",
  "started_at": "2026-11-06T13:03:52.9420917Z",
  "finished_at": "2026-11-06T13:03:58.0617214Z",
  "config": {
    "target_url": "http://127.0.0.1:18082/inventory/LAPTOP-001",
    "vus": 2, "duration_seconds": 5, "max_failure_rate": 0,
    "metrics_url": "http://127.0.0.1:18082/metrics",
    "sample_interval_seconds": 1, "max_samples": 1000, "timeout_seconds": 65
  },
  "workload": {
    "result": { "runner": "k6", "runner_version": "2.3.0", "metrics": { "http_requests": 17550, "...": "..." } },
    "verdict": { "passed": true, "max_failure_rate": 0 }
  },
  "telemetry": {
    "completeness": "complete",
    "attempts": 7, "succeeded": 7, "failed": 0, "sample_limit_reached": false,
    "samples": [
      {
        "sequence": 1, "phase": "before_workload",
        "scraped_at": "2026-11-06T13:03:52.9420917Z", "offset_seconds": 0,
        "scrape_duration_seconds": 0.0048,
        "families": [ { "name": "inventory_http_requests_total", "type": "counter", "series": [ "..." ] } ]
      }
    ],
    "errors": []
  }
}
```

- `status` is the overall outcome (see below); `reasons` explains any status other than `passed`.
- `config` holds the settings, with both URLs redacted. Each URL is stored once, not per sample.
- `workload` holds either k6's `result` (the same structure as a `ruptiq-run` report) or an
  `error`, plus the pass/fail `verdict`.
- `telemetry.samples` hold the families exactly as `ruptiq-telemetry` reads them (names, labels,
  values, histogram buckets), with `sequence`, `phase` (`before_workload`, `during_workload`, or
  `after_workload`), `scraped_at`, `offset_seconds`, and `scrape_duration_seconds`.
- `telemetry.errors` hold failed scrapes: `sequence`, `phase`, `attempted_at`, `offset_seconds`,
  and the redacted `error`. No values are recorded for them.
- `telemetry.retained_bytes` is the compact JSON size of the kept samples, counted against
  `config.max_telemetry_bytes`. `telemetry.retention_limit_reached` is `true` when a successful
  scrape did not fit; that scrape is listed in `telemetry.not_retained` (`sequence`, `phase`,
  `scraped_at`, `offset_seconds`, `size_bytes`) without its values.
- Counts: `attempts` = `succeeded` + `failed`, and `succeeded` = kept samples + not retained.
  `samples`, `errors`, and `not_retained` are always arrays.

### Status, partial telemetry, and exit codes

Telemetry is `complete` only if every scrape succeeded and was kept, there is at least one sample
before, during, and after the workload, and neither the sample limit nor the retention budget was
reached. Otherwise it is `partial` (some samples kept) or `none` (no samples kept), and
`completeness_reasons` says why. Telemetry failures and limits never stop the load test; they are
recorded and make the experiment `incomplete`.

| Status | Meaning | Exit |
| --- | --- | --- |
| `passed` | The load test passed and the telemetry is complete. | 0 |
| `failed` | The load test completed but failed its pass criteria (for example, failed requests). | 1 |
| (none) | Invalid arguments; nothing was run and no report was written. | 2 |
| `error` | The load test could not be completed (k6 missing, k6 failed, unreadable results). Also exit 3 if the report file cannot be written. | 3 |
| `incomplete` | The load test passed, but the telemetry is partial or missing. | 4 |
| `cancelled`, `timed_out` | The experiment was stopped by Ctrl+C or by `-timeout`. | 5 |

The status is decided in that order of precedence: a stopped experiment is never judged on its
partial results, and `incomplete` is reported only when the load test itself passed (the
telemetry's completeness is still in the report for the other outcomes). Only `passed` is a pass.

Real examples against the Inventory Service: with `-metrics-url http://127.0.0.1:18082/nope`, the
load test passed but all 4 scrapes failed (`404 Not Found`), so the status was `incomplete`
(exit 4) with 4 recorded errors and no samples. With `-duration 3s -timeout 3001ms`, k6 was stopped
at the deadline: status `timed_out` (exit 5), workload error `k6 run stopped: context deadline
exceeded`, and telemetry `partial` with 3 samples and "no sample after the workload (the experiment
was stopped)". No k6 process or temporary directory was left behind.

The retention budget, against the same service (whose counters had grown from earlier runs): a 5s
experiment with the default budget kept 7 samples, `retained_bytes` 14622, in a 35892-byte report
file (the file is indented, so it is larger than the compact JSON the budget counts). With
`-max-telemetry-bytes 8192`, the load test passed (10606 requests) but the status was `incomplete`
(exit 4):

```text
  Telemetry:       partial: 4 of 4 scrapes succeeded; 3 samples kept (1 before, 2 during, 0 after the workload)
                   - telemetry retention budget of 8192 bytes reached: sample #4 (2317 bytes) did not fit after 6940 bytes were kept; it was not kept and sampling stopped
                   - no sample after the workload (the retention budget was reached)
  Retained:        6940 of 8192 bytes of sample data
```

With `-out`, the report is written for every outcome except invalid arguments, through a temporary
file and a rename, so an old successful report is replaced and never mistaken for the latest
experiment.

### Safety

- The user name, password, and whole query string of both URLs are replaced by `xxxxx` in the
  console output and the report, including error messages. The requests themselves use the
  complete URLs. (Checked with real runs: no credential or query value appeared in the output or
  file.)
- k6 is run by the same adapter as `ruptiq-run`: no shell, the target URL passed through the
  environment, and its temporary directory removed afterwards.
- Memory use is limited: the kept samples are bounded by `-max-telemetry-bytes` (default 32 MiB),
  scrape attempts by `-max-samples`, each response by 16 MiB, and only a short summary is printed (at
  most 5 scrape errors, long messages shortened). The budget counts the samples' JSON size, which
  approximates their memory use; it is not a hard limit on the process's memory (see Limitations).

### Limitations

- One HTTP GET workload and one Prometheus text-format endpoint per experiment.
- Values are recorded as exposed: the report holds cumulative counters and histograms, not rates.
  `ruptiq-analyze` (below) calculates rates and percentile estimates from a saved report, and
  detects counter resets there.
- Sampling is periodic, not continuous: anything shorter than the interval can fall between two
  samples, and the scrapes add a small load of their own to the target. Scrape timing follows the
  local clock and Go's ticker, so intervals are approximate (for example +1.006 s, +2.005 s).
- Timestamps come from the machine running Ruptiq. The target's own clock is not used, so a
  remote target's clock skew does not matter, but network delay to the metrics endpoint does.
- The whole report is kept in memory until the experiment ends. Nothing is stored in a database
  or compared between experiments; `ruptiq-compare` reads only `ruptiq-run` reports.
- The retention budget is an approximation, not a memory guarantee. Go's in-memory form of a
  sample (maps, slices, strings, pointers) is usually larger than its JSON. On top of the kept
  samples, each scrape briefly holds its response (up to 16 MiB) and parsed snapshot, and measuring
  a sample briefly makes one JSON copy of it; writing the report makes one JSON copy of everything.
  The small "not retained" and error records are not counted.
- Once the budget is reached, sampling stops for the rest of the experiment, including the scrape
  after the workload, even if later snapshots would have been smaller.

## Analyze a saved experiment (`ruptiq-analyze`)

`ruptiq-analyze` reads one saved `ruptiq-experiment` report and calculates performance signals
from its telemetry samples: server-side request throughput per method and route, the share of
HTTP 5xx responses, and estimated p95 latency from the duration histogram. It works offline on
the saved file only; it never runs a load test or contacts the target.

```sh
go run ./cmd/ruptiq-analyze -in experiment.json -out analysis.json
```

The output below is from the deterministic test fixture
`internal/analysis/testdata/inventory_experiment.json`: a synthetic Inventory Service
experiment (made-up numbers, real metric names and bucket bounds) with GET 200s and 404s and
POST 201s and 503s. It has not yet been run on a report from a real experiment.

```text
Experiment:   passed, telemetry complete (5 samples, 0 scrape errors, 0 not retained)
  Target:     http://127.0.0.1:8082/inventory/LAPTOP-001
  Metrics:    http://127.0.0.1:8082/metrics
Windows:      4, covering 3.750s (samples #1 to #5); 0 span missed scrapes
Client:       k6 (client side): 9000 requests, 2999.6 req/s, 0.00% failed, p95 0.680 ms (measured by the load generator)

Server signals over all usable windows (inventory_http_requests_total, inventory_http_request_duration_seconds):
  GET /inventory/{sku}
    Throughput: 8040 requests in 3.750s = 2144.0 req/s (windows 1-4)
    5xx:        0 of 8040 (0.00%); 4xx client errors, not counted as server errors: 40
    Latency:    p95 ~0.989 ms (estimate; within the 0.500 ms to 1.000 ms bucket), mean 0.399 ms, 8040 requests
  POST /reservations
    Throughput: 106 requests in 3.750s = 28.3 req/s (windows 1-4)
    5xx:        6 of 106 (5.66%); 4xx client errors, not counted as server errors: 0
    Latency:    p95 ~27.917 ms (estimate; within the 25.000 ms to 50.000 ms bucket), mean 5.094 ms, 106 requests

Windows:
    1  #1->#2    1.002s  GET /inventory/{sku}: 2016.0 req/s, 5xx 0.00%, p95 ~0.989 ms
    1  #1->#2    1.002s  POST /reservations: 41.9 req/s, 5xx 4.76%, p95 ~4.987 ms
    ...
    4  #4->#5    0.250s  GET /inventory/{sku}: 0.0 req/s, 5xx n/a, p95 n/a
    4  #4->#5    0.250s  POST /reservations: 0.0 req/s, 5xx n/a, p95 n/a

Limitations:
  - Rates are averages over each window between two scrapes (for totals, over the windows used); anything shorter than a window is not visible.
  ...
```

The summary shows derived signals only; it is capped at 20 routes, 20 window lines, and 10
warnings, and the JSON file has everything.

### Options

| Flag | Default | Meaning |
| --- | --- | --- |
| `-in` | (required) | Saved `ruptiq-experiment` report. |
| `-out` | (none) | Also write the analysis as JSON to this file. Must not be the input. |
| `-max-input-bytes` | `134217728` (128 MiB) | Largest report accepted, 1 KiB to 1 GiB. A report with the default 32 MiB retention budget is about 80 MiB as written. |
| `-requests-metric` | `inventory_http_requests_total` | Counter of completed HTTP requests. |
| `-duration-metric` | `inventory_http_request_duration_seconds` | Histogram of request durations, in seconds. |
| `-method-label` | `method` | Label with the HTTP method. Empty: do not split by method. |
| `-route-label` | `route` | Label with the route template (not a raw path). Empty: do not split by route. |
| `-status-label` | `status` | Label with the HTTP status code. Empty: 5xx shares are unavailable. |
| `-min-observations` | `20` | Fewest requests in a window (or in the totals) for a p95 estimate. |

The defaults match the Inventory Service. Nothing else about it is assumed, so any Prometheus HTTP
server with a request counter and a duration histogram can be analysed by naming its metrics and
labels.

### Cumulative metrics and rates

Prometheus counters and histogram buckets are cumulative: `inventory_http_requests_total` is the
number of requests since the service started, not a rate. Dividing it by the sampling interval
gives a meaningless number. `ruptiq-analyze` works on **windows** instead: each pair of
consecutive retained samples. For every series it takes the later value minus the earlier one,
then divides by the time that actually passed between the two scrapes, from their timestamps,
not the configured interval (scrapes drift: for example 1.002 s, 1.008 s, 1.490 s).

- **Throughput** is the counter's increase in a window, summed over the route's statuses, divided
  by the window's length.
- **5xx share** is the increase of series whose status label is 500-599, divided by the route's
  total increase. 4xx responses are client errors and are counted separately, never as server
  errors. If any series has no numeric status, the share is unavailable rather than guessed.
- **Totals** add up every window in which a route's signal was available, over the summed length
  of those windows, and list the windows used and excluded.
- **k6's figures** (client side, including network time, over k6's own time span) are copied
  unchanged into `client` and never mixed with the server-side signals.

### Missing and unusable evidence

A value is never invented or assumed to be zero:

- **A series that appears** in a window (present in the later sample, absent from the earlier)
  has an unknown earlier value, so the route's signal for that window is unavailable. On a
  freshly started service this is usual for the first window: Prometheus clients create a series
  at its first request, so it is absent from the baseline sample.
- **A series that disappears**, or **a counter that decreases** (the service probably
  restarted), also makes the route's window unavailable. No negative throughput is ever reported.
- **Failed or unretained scrapes** leave a longer window between the samples around them. Its
  rate is the average over the whole span, and it lists the missed scrapes.
- **A missing metric family** (or one of the wrong type) makes that signal unavailable.

Each case is listed under `warnings` with the window, sample sequence, metric, and labels.

### Why p95 is an estimate

A histogram records how many requests fell in each bucket (for the Inventory Service, bounds from
0.1 ms to 10 s), not the individual durations. For each window, `ruptiq-analyze` subtracts the
earlier cumulative buckets from the later ones, after checking that the bucket bounds are the same
and that no count decreased, and combines a route's statuses. It then finds the bucket containing
the 95th percentile and interpolates linearly within it, as PromQL's `histogram_quantile` does.
The true p95 lies somewhere in that bucket; the report gives the estimate and the bucket's bounds.
p95 is unavailable, with a reason, when the window has fewer than `-min-observations` requests,
when it lies in the `+Inf` bucket (above 10 s), or when the buckets cannot be compared. The mean
(the increase of the histogram's sum divided by its count) is exact apart from rounding.

### The JSON file

```json
{
  "schema_version": 1,
  "input": "experiment.json",
  "analysis": {
    "experiment": { "status": "passed", "telemetry_completeness": "complete", "samples": 5, ... },
    "config": { "requests_counter": "inventory_http_requests_total", ... },
    "client": { "source": "k6 (client side)", "metrics": { ... }, "passed": true },
    "totals": [ { "method": "GET", "route": "/inventory/{sku}", "requests": { ... }, "latency": { ... } } ],
    "windows": [
      {
        "index": 1,
        "from": { "sequence": 1, "phase": "before_workload", "scraped_at": "...", "offset_seconds": 0 },
        "to":   { "sequence": 2, "phase": "during_workload", "scraped_at": "...", "offset_seconds": 1.002 },
        "elapsed_seconds": 1.002,
        "missed_scrapes": [],
        "routes": [
          {
            "method": "GET", "route": "/inventory/{sku}",
            "requests": { "requests": 2020, "elapsed_seconds": 1.002, "per_second": 2015.97,
                          "by_status": [ ... ], "server_errors_5xx": 0, "client_errors_4xx": 20,
                          "unclassified_status": 0, "server_error_ratio": 0,
                          "evidence": { "metric": "inventory_http_requests_total", "series": [ ... ] } },
            "latency": { "observations": 2020, "mean_seconds": 0.0003980,
                         "p95": { "quantile": 0.95, "estimate": true, "seconds": 0.000989,
                                  "bucket_lower_seconds": 0.0005, "bucket_upper_seconds": 0.001 },
                         "buckets": [ ... ], "evidence": { ... } }
          }
        ]
      }
    ],
    "warnings": [],
    "limitations": [ ... ]
  }
}
```

- For each route, exactly one of `requests` and `requests_unavailable` is present, and likewise
  for `latency`; `server_error_ratio`, `mean_seconds`, and `p95.seconds` are each replaced by an
  `..._unavailable` / `unavailable` reason when they cannot be calculated.
- `evidence` names the metric and the exact label sets used; with the window's `from` and `to`
  (sequence numbers and timestamps), every number can be traced back to the experiment report.
- `buckets` is the window's own histogram (cumulative counts of the requests completed in it).
- If the report cannot be analysed, the file has `error` instead of `analysis`, so an older
  analysis cannot be mistaken for the latest one.

### Validation and exit status

The report must be a complete, internally consistent `ruptiq-experiment` report with
`schema_version` 1 and status `passed`, `failed`, or `incomplete`. Anything that cannot be
interpreted safely is rejected, never repaired: unknown or missing fields, a missing or unknown
schema version, scrape counts that disagree with the sample, error, and not-retained lists,
sequence numbers that are missing, repeated, or out of order, timestamps out of order or outside
the experiment, offsets that disagree with timestamps, a completeness or status that the data
does not support, negative or non-finite counters, and histograms whose buckets are not
cumulative or disagree with their count. Reports of `error`, `cancelled`, or `timed_out`
experiments are refused, because their workload did not complete.

| Exit status | Meaning |
| --- | --- |
| 0 | The report was analysed (whatever the signals show). |
| 2 | Invalid arguments. |
| 3 | The report could not be read, is too large, invalid, unsupported, or records an experiment that did not complete; or the analysis could not be written. |

### Safety

- Read-only: only the input file is read. Nothing is sent anywhere.
- URLs are redacted again (user information and query string replaced by `xxxxx`), and so is any
  URL inside a label value or a scrape error, so credentials never reach the summary or the file.
  Validation errors never quote the report's URLs, and a malformed timestamp or number is
  reported without quoting its value.
- The input size is bounded (`-max-input-bytes`), and the summary never prints raw telemetry.

### Limitations

- Signals are averages over windows of about one sampling interval; anything shorter is
  invisible, and an experiment with a few windows says little about variability. No statistical
  significance is calculated.
- p95 values are estimates whose accuracy depends on the bucket bounds; with the Inventory
  Service's buckets, a p95 between 0.5 ms and 1 ms is known only to lie in that range.
- Server-side counters include every client's requests, not only the experiment's workload, and
  are not reconciled with k6's counts (the scrapes and the load run start and stop at different
  moments).
- Latency combines all statuses of a route, including errors and 404s.
- Windows that start before the load or end after it, and the first and last windows during it,
  include time without full load.
- Only one request counter and one duration histogram are analysed; other metrics (such as the
  in-flight gauge or stock levels) are kept in the experiment report but not interpreted.
- These are **signals, not a diagnosis**. A higher p95 or 5xx share in one window says what was
  measured, not why. `ruptiq-investigate` (below) compares the windows and ranks the changes it
  finds, but it does not identify causes. `ruptiq-validate-fix` (below) compares two saved analyses and still does not identify a cause.

## Investigate a saved analysis (`ruptiq-investigate`)

`ruptiq-investigate` reads one analysis saved with `ruptiq-analyze -out` and reports ranked,
evidence-backed findings: for each route, whether the 5xx ratio, the estimated p95, or the request
rate changed materially between the earlier and the later windows of the experiment. It is
deterministic and works offline on the saved file only: no load test, no network, no language
model. **A finding is an observation or a hypothesis to test, never a proven root cause.**

```sh
go run ./cmd/ruptiq-analyze -in experiment.json -out analysis.json
go run ./cmd/ruptiq-investigate -in analysis.json -out investigation.json
```

The output below is from the synthetic test fixture (see
[Analyze a saved experiment](#analyze-a-saved-experiment-ruptiq-analyze)), not a real experiment.
Only windows 2 and 3 of its four were taken entirely during the load, fewer than the four the
default thresholds need, so the investigator abstains:

```text
Source:       passed experiment, telemetry complete, 4 windows (2 comparable: 2-3)
Thresholds:   2+ windows and 100+ requests per side; 5xx ratio +1.00 points and 2x (5+ errors); p95 +50.0%; throughput -30.0%
Outcome:      insufficient_evidence
              No signal of any route could be compared (2 of 4 windows are comparable); the investigation abstains.

Findings (ranked; observations and hypotheses to test, not proven root causes):
  1. insufficient_evidence [abstention] insufficient_evidence-a76631a81bc2
     GET /inventory/{sku}: 3 of 3 signals could not be compared
     throughput: 2 usable window(s) of 2 comparable; at least 4 (2 per side) are needed
     server_error_ratio: 2 usable window(s) of 2 comparable; at least 4 (2 per side) are needed
     p95_latency: 2 usable window(s) of 2 comparable; at least 4 (2 per side) are needed
     Next: Repeat the experiment at least 3 times with the same target and 2 VUs for at least 10s (...)
  2. insufficient_evidence [abstention] insufficient_evidence-1ea60157359d
     POST /reservations: 3 of 3 signals could not be compared
     ...

Excluded windows:
  window 1: spans the workload's start or end (samples #1 before_workload to #2 during_workload), so its load is not steady
  window 4: spans the workload's start or end (samples #4 during_workload to #5 after_workload), so its load is not steady

Not a diagnosis: no finding is a proven root cause, and no statistical significance test is made. See the limitations in the JSON report.
```

With `-min-windows 1` (one window per side, which the report warns is noisy) the same fixture gives
one observation. GET served 3000 requests in window 2's 1.008 s (2976.2/s) and 3020 in window 3's
1.490 s (2026.8/s), a fall of 31.9%, just past the 30% threshold:

```text
  1. throughput_degradation [moderate, observation] throughput_degradation-a76631a81bc2
     GET /inventory/{sku}: completed requests fell from 2976.2 requests/s to 2026.8 requests/s (-31.9%)
     Windows: baseline 2, comparison 3; 2 evidence references (first: /analysis/windows/1/routes/0/requests)
     Contradicting: p95 latency did not rise materially (the p95 estimate changed by -0.0%, short of the +50.0% threshold): a slower service would usually raise latency, so the load generator may simply have sent fewer requests
```

The summary is capped at 10 findings, 10 inconclusive signals, and 10 excluded windows; the JSON
file has everything.

### What it can conclude

| Finding | Designation | Reported when |
| --- | --- | --- |
| `server_error_increase` | observation | The route's pooled 5xx ratio rose by at least 1 percentage point, to at least twice a non-zero baseline, with at least 5 5xx responses in the comparison and 100 requests on each side. |
| `latency_degradation` | observation | The median of the windows' p95 estimates rose by at least 50%, **and** every comparison window's p95 bucket lies above every baseline window's bucket, with at least 100 observations on each side. |
| `throughput_degradation` | observation | The median of the windows' request rates fell by at least 30%, **and** every comparison window's rate is below every baseline window's, with at least 100 requests in the baseline. |
| `combined_degradation` | hypothesis | Both latency and throughput degradation hold for one route over exactly the same windows. That they share a cause is the hypothesis. |
| `insufficient_evidence` | abstention | A route's signal could not be compared: too few usable windows, too little traffic, a zero baseline, or windows in which the signal was unavailable (a counter reset, a series that appeared or disappeared, changed histogram buckets, a missing metric, or a p95 that could not be estimated). |

A change that reaches its threshold but fails a robustness check (p95 estimates in overlapping
buckets, rates whose windows overlap, too few 5xx responses, or a ratio that did not double) is
listed as `inconclusive` in `assessments`, not as a finding. Every compared signal, including those
with no material change, is in `assessments`, so a missing finding can be told from a missing
measurement. All thresholds are flags (see Options).

### What it cannot conclude

- **Causes.** The analysis holds request counts, status codes, and latency histograms only. There
  are no CPU, memory, database, or network measurements, so none of these is ever named as a cause.
  They appear only as untested hypotheses ("Hypothesis (untested): ...") with what would be needed
  to test them. Every finding has `"proven_root_cause": false`.
- **Regressions.** A finding compares earlier and later windows of one run. One run cannot show
  that a version is slower than another; repeat the experiment (see below).
- **Significance.** No statistical significance test is made. "Material" means a change exceeded
  a fixed, configurable threshold, nothing more. There are no confidence percentages.
- **Capacity from errors.** A higher 5xx ratio does not by itself mean lower capacity: errors can
  come from bad input, a failing dependency, or deliberate load shedding. Each finding says so.
- **Client-side numbers.** k6 measures the whole run on the client, including network time; the
  server's signals cover windows between scrapes. They are not compared.
- **Stability from abstention.** An `insufficient_evidence` finding means nothing was concluded,
  not that performance was stable.

### How windows are compared

1. Only windows whose two samples were both taken during the workload, with no missed scrapes
   between them, are comparable. Windows touching the before or after sample include the load
   generator starting or stopping; a window spanning missed scrapes covers a longer, different
   span. Excluded windows are listed with the reason.
2. For each route and signal, the windows in which the signal is available are split in two: the
   earlier half is the baseline and the later half the comparison. With an odd number, the middle
   window is in neither side. Each side needs `-min-windows` windows (default 2).
3. Rates and p95 use the median of the side's windows, so one unusual window cannot decide the
   result; the 5xx ratio pools the side's 5xx responses and requests. Rates come from
   `ruptiq-analyze`, which divides counter increases by each window's measured length.
4. Zero baselines are never divided by: a zero baseline rate or p95 is insufficient evidence, and
   a 5xx increase from 0% has no relative change (`relative_unavailable`).

### How findings are ranked

Findings are sorted by these rules, in order, and numbered from 1. The rule is written into every
finding (`ranking.rule`) with the values used.

1. Observations and hypotheses before abstentions.
2. Severity: `major` (the change is at least twice its threshold) before `moderate`.
3. Type: combined, then server errors, then latency, then throughput.
4. The larger `threshold_multiple` (the change divided by its threshold; for a combined finding,
   the smaller of its two).
5. More windows compared, then more requests or observations.
6. Method, route, and finding ID, so the order is always the same.

Finding IDs are stable: the type and a hash of the method and route, such as
`latency_degradation-a76631a81bc2` for GET `/inventory/{sku}`.

### Evidence references

Each finding lists the analysis measurements it was calculated from, as
[JSON pointers](https://www.rfc-editor.org/rfc/rfc6901) into the analysis file, with the window,
the role (`baseline`, `comparison`, `warning`, or `excluded`), the metric, and the series labels:

```json
{ "pointer": "/analysis/windows/1/routes/0/requests", "window": 2, "role": "baseline",
  "metric": "inventory_http_requests_total", "series": [ { "method": "GET", ... } ] }
```

Array positions in pointers count from 0, window indexes from 1, so window 2 is
`/analysis/windows/1`. An abstention points at the analysis warnings about its route and at the
route in the excluded windows. Through the analysis' own evidence and sample references, every
number can be traced back to the experiment report.

### Running a validation experiment

Every finding has a `next_experiment`. It always starts with repeating the experiment, with the same
target and VUs and long enough for twice the windows a comparison needs, at least 3 times:

```sh
go run ./cmd/ruptiq-experiment -url http://localhost:8082/inventory/LAPTOP-001 -metrics-url http://localhost:8082/metrics -vus 2 -duration 10s -interval 1s -out run1.json
go run ./cmd/ruptiq-analyze -in run1.json -out run1-analysis.json
go run ./cmd/ruptiq-investigate -in run1-analysis.json -out run1-investigation.json
```

A finding that does not recur in most runs was probably noise. One that does recur is still an
observation; the next step depends on its type, for example running at half and twice the VUs to
see whether p95 follows the load, or collecting the service's logs for the comparison windows to see
which 5xx errors were returned. Ruptiq does not run these experiments itself.

### Options

| Flag | Default | Meaning |
| --- | --- | --- |
| `-in` | (required) | Saved `ruptiq-analyze` report. |
| `-out` | (none) | Also write the investigation as JSON to this file. Must not be the input. |
| `-max-input-bytes` | `134217728` (128 MiB) | Largest analysis accepted, 1 KiB to 1 GiB. |
| `-min-windows` | `2` | Fewest usable windows in each of the baseline and the comparison, 1 to 1000. |
| `-min-requests` | `100` | Fewest requests (latency: observations) per side; for throughput, in the baseline only. |
| `-min-server-errors` | `5` | Fewest 5xx responses in the comparison for a server error increase. |
| `-error-ratio-increase` | `0.01` | Smallest rise of the 5xx ratio, as a fraction (0.01 is 1 percentage point). |
| `-error-ratio-factor` | `2` | How many times a non-zero baseline 5xx ratio the comparison's must be. |
| `-latency-increase` | `0.5` | Smallest relative rise of the p95 estimate (+50%). |
| `-throughput-decrease` | `0.3` | Smallest relative fall of the request rate (-30%). |

### The JSON file

```json
{
  "schema_version": 1,
  "input": "analysis.json",
  "investigation": {
    "source": { "experiment_status": "passed", "telemetry_completeness": "complete", "windows": 4, ... },
    "config": { "min_windows_per_side": 2, "min_requests_per_side": 100, ... },
    "outcome": "changes_observed",
    "summary": "...",
    "windows": { "comparable": [2, 3], "excluded": [ { "window": 1, "reason": "...", "pointer": "/analysis/windows/0" } ] },
    "findings": [
      {
        "rank": 1, "id": "throughput_degradation-a76631a81bc2", "type": "throughput_degradation",
        "designation": "observation", "proven_root_cause": false,
        "method": "GET", "route": "/inventory/{sku}", "severity": "moderate", "symptom": "...",
        "changes": [ { "signal": "throughput",
                       "baseline":   { "value": 2976.19, "unit": "requests/s", "statistic": "median of the windows' request rates", ... },
                       "comparison": { "value": 2026.85, "unit": "requests/s", ... },
                       "change": { "absolute": -949.34, "unit": "requests/s", "relative": -0.319, "threshold_multiple": 1.063 } } ],
        "baseline_windows": [2], "comparison_windows": [3],
        "evidence": [ ... ], "explanation": "...", "ranking": { ... },
        "contradictions": [ ... ], "limitations": [ ... ], "possible_explanations": [ ... ],
        "next_experiment": "..."
      }
    ],
    "assessments": [ { "method": "GET", "route": "/inventory/{sku}", "signal": "p95_latency", "status": "no_material_change", ... } ],
    "limitations": [ ... ]
  }
}
```

- `outcome` is `changes_observed` (at least one observation), `no_material_change` (signals were
  compared and none changed materially), or `insufficient_evidence` (nothing could be compared).
- p95 measures are marked `"estimate": true` and carry the lowest and highest bucket bounds of the
  side's windows.
- If the analysis cannot be investigated, the file has `error` instead of `investigation`, written
  over any earlier file, so an older investigation cannot be mistaken for the latest one.

### Validation and exit status

The input must be a `ruptiq-analyze` file with `schema_version` 1 and an `analysis`. A file that
records a failed analysis (`error`) is refused. Anything inconsistent is rejected, never repaired:
unknown fields, window indexes or sample sequences out of order, window lengths that disagree with
their timestamps, rates that are not requests divided by elapsed time, status counts that do not
add up, 5xx ratios that disagree with their counts, p95 estimates outside their bucket or from
fewer observations than the analysis' minimum, a signal both present and unavailable, repeated
routes, and evidence for the wrong metric. Up to 10 problems are listed.

| Exit status | Meaning |
| --- | --- |
| 0 | The analysis was investigated, including when the outcome is `insufficient_evidence`. |
| 2 | Invalid arguments or thresholds. |
| 3 | The analysis could not be read, is too large, invalid, unsupported, or records a failed analysis; or the investigation could not be written. |

### Safety

- Read-only: only the input file is read. Nothing is sent anywhere, and nothing is changed in the
  target.
- URLs are redacted again (user information and query string replaced by `xxxxx`), and so are URLs
  in route names, label values, and a recorded analysis error. Validation errors never quote a
  malformed timestamp or number.
- The input size is bounded (`-max-input-bytes`), copied text is shortened, and the output file is
  replaced atomically (written to a temporary file, then renamed).

### Limitations

- The engine compares only earlier and later windows of one experiment, with the windows the
  analysis provides. With the default thresholds a comparison needs at least 4 windows entirely
  during the load (at a 1s interval, a load of about 5s or more), and more to be useful; shorter
  runs abstain.
- It looks for degradation only: an improvement is reported as no material change.
- Thresholds are fixed rules of thumb, not calibrated to any service. A gradual change that never
  separates the two halves cleanly is reported as inconclusive.
- Latency combines all statuses of a route, as in the analysis.
- Until the regression lab (below), it had been run only on synthetic data. One live run against
  that lab is recorded there; it is still not a comparison between versions or a root-cause
  diagnosis.

## Compare a baseline and a candidate (`ruptiq-validate-fix`)

`ruptiq-validate-fix` reads two analyses saved with `ruptiq-analyze -out` and compares matching
methods and routes. Use it after a proposed fix: one file is the baseline experiment, the other
is a later experiment collected the same way. It works offline on those files only. It does not
change code, and it does not decide whether the fix worked.

Three different claims have to stay separate:

| Claim | What this command does |
| --- | --- |
| An observed difference between one before run and one after run | This is the result `ruptiq-validate-fix` can report. |
| A difference that shows up again when both experiments are repeated | Not shown. One pair of files cannot show that. |
| Proof that a code change caused the difference | Not shown. The tool has no change set, no resource metrics, and no significance test. |

### Collect two comparable experiments

Run the same workload twice, once before the change and once after, and analyse each report with
the same metric names:

```sh
go run ./cmd/ruptiq-experiment -url http://127.0.0.1:8082/inventory/LAPTOP-001 -metrics-url http://127.0.0.1:8082/metrics -vus 2 -duration 20s -interval 1s -out tmp/before-experiment.json
go run ./cmd/ruptiq-analyze -in tmp/before-experiment.json -out tmp/before-analysis.json
go run ./cmd/ruptiq-experiment -url http://127.0.0.1:8082/inventory/LAPTOP-001 -metrics-url http://127.0.0.1:8082/metrics -vus 2 -duration 20s -interval 1s -out tmp/after-experiment.json
go run ./cmd/ruptiq-analyze -in tmp/after-experiment.json -out tmp/after-analysis.json
go run ./cmd/ruptiq-validate-fix -before tmp/before-analysis.json -after tmp/after-analysis.json -out tmp/validation.json
```

`tmp/` is gitignored. The two experiments must use the same VUs, the same configured duration,
the same sampling interval, and the same Prometheus counter, histogram, and label names. The
comparison also checks the target URL that the analysis saved. That saved URL has already had
its user information and query string replaced, so only the scheme, host, and path can be
checked. If either URL still shows a query string or user information, including the redacted
placeholder, the report warns that those parts were not compared. A match does not prove the
original targets or workload parameters were the same.

A route that exists on only one side is reported as missing. It is not treated as zero traffic.
k6's client latency and request rate are copied into the JSON for context and are not mixed into
the server-side numbers.

Matching flags do not make the machines, caches, or background load the same. The report says so.

### What is compared

Only windows taken entirely during the workload, with no missed scrapes, are eligible. A signal
also has to be present in that window. Counter resets, disappeared series, and histogram bucket
changes exclude the window. The default is at least two eligible windows on each side.

- **Throughput** is the total of completed requests in the eligible windows divided by the total
  time those windows actually covered. Per-window rates are not averaged, and cumulative counters
  are not averaged.
- **5xx ratio** pools server errors and completed requests. The change is reported in percentage
  points. 4xx responses stay in their own count. When a side completed no requests, that side's
  ratio is left null and the summary says it is undefined. A printed 0% is a side that completed
  requests and had no 5xx responses.
- **Latency** is the median of the per-window p95 estimates. That is not the p95 of the whole
  experiment. Each estimate keeps its histogram bucket bounds. If the before and after bucket
  ranges overlap, the comparison stays inconclusive. It does not become an improvement, a
  regression, or `no_material_change` on its own.

### Outcomes

| Outcome | Meaning |
| --- | --- |
| `improvement_observed` | Every material change is an improvement. This is still one observation. |
| `regression_observed` | At least one material change is a regression, and none is an improvement. |
| `mixed` | At least one material improvement and one material regression. |
| `no_material_change` | At least one signal was judged, and every judged signal stayed inside its threshold. An inconclusive p95 does not supply that judgment. |
| `insufficient_evidence` | No signal could be judged. That includes too few windows, too few requests, a missing route, and a comparison whose only threshold crossing is an inconclusive p95. |
| `not_comparable` | VUs, duration, interval, the comparable part of the target URL, or metric configuration differ. No route is treated as equivalent. |

The default thresholds are the same kind of fixed rules used by `ruptiq-investigate`: throughput
±30%, 5xx ratio ±1 percentage point and twice the better side when that side is not zero (and at
least 5 server errors on the worse side), and median per-window p95 ±50%. They are not a
significance test. Flags can change them.

### Synthetic examples

These numbers come from the package tests, not from a live service.

A route that does 1000 requests in each of two 1-second windows, with every window p95 estimated
at 20 ms inside the bucket from 10 ms to 25 ms, then 1400 requests per window with every window
p95 at 4 ms inside the bucket from 2 ms to 5 ms:

| Signal | Before | After | Change | Result |
| --- | --- | --- | --- | --- |
| Throughput | 1000.0 requests/s | 1400.0 requests/s | +400 requests/s, +40% | improvement |
| 5xx ratio | 0% | 0% | 0 points | no material change |
| Median window p95 | 20 ms | 4 ms | −80% | improvement, because the buckets do not overlap |

The outcome is `improvement_observed`. The same shape with the numbers reversed is
`regression_observed`. A higher request rate together with a higher 5xx ratio is `mixed`.

A live pair of regression-lab experiments, one with the heavier CPU loop and one with the
lighter loop, is recorded under [Before and after runs](#before-and-after-runs). Those numbers
are one machine's observations. They are not the synthetic table above.

The checked-in inventory analysis compared with itself is `no_material_change`. Its two eligible
windows are windows 2 and 3 (windows 1 and 4 touch the start or the end of the load). For
`GET /inventory/{sku}` those windows contain 3000 and 3020 requests over 1.008 s and 1.490 s, so
the pooled rate is 6020 / 2.498 s = 2410.7 requests/s on both sides, not the average of the two
window rates. The 20 responses counted as 404 stay out of the 5xx ratio, which is 0. POST `/reservations` has 64 requests across those windows, below the default of 100, so that route is
`insufficient_evidence` rather than a claim that nothing changed. k6's 9000 client requests are
not used.

### Validation and exit status

| Exit status | Meaning |
| --- | --- |
| 0 | The analyses were compared, including `not_comparable` and `insufficient_evidence`. |
| 2 | Invalid arguments or thresholds. |
| 3 | An analysis could not be read, is too large, invalid, unsupported, or records a failed analysis; or the validation file could not be written. |

`-out` is written through a temporary file in the same directory and is refused when it names
either input. URLs lose their user information and query string. The input size is bounded.

### Limitations

- One pair of experiments cannot show repeatability or a cause.
- There is no confidence percentage and no significance test.
- Short runs often end as `insufficient_evidence` because two eligible windows per file is the
  default floor, and each judged throughput baseline needs at least 100 requests.
- A p95 that moves inside overlapping buckets is inconclusive even when the estimates differ by
  more than 50%. That signal does not become an improvement, a regression, or `no_material_change`.
  If nothing else could be judged, the route and the overall outcome are `insufficient_evidence`.
  A judged throughput or 5xx result is kept, and the summary says the latency direction was not claimed.
- Different histogram bucket boundaries are reported as incompatible and are not turned into a
  latency verdict.

## Validate the pipeline with a controlled change (`demo/regression-lab`)

`demo/regression-lab/` is a small HTTP target used only to check that the existing pipeline
(k6 → Prometheus samples → `ruptiq-analyze` → `ruptiq-investigate`) reports a change that really
happened. It is **not** a checkout service, not production software, and not a claim that Ruptiq
can name CPU, database, or network bottlenecks. It executes extra integer arithmetic on
`GET /work` after a timer; Prometheus records the real handling time of that work.

It listens on loopback only (`127.0.0.1:8090` by default). There is no HTTP switch for the mode:
the first `GET /work` starts a timer (`-degrade-after`, default 10s), then the handler uses a
heavier CPU loop. `GET /metrics` exposes `regression_lab_degraded` (0 or 1) so the transition can
be checked independently of Ruptiq. `GET /healthz` does not start the timer. The `GET /work` 200
counter and histogram series are created at process start, so the first snapshot is not missing
them.

### Start the lab

From the repository root, in its own terminal:

```sh
go run ./demo/regression-lab -addr 127.0.0.1:8090 -degrade-after 10s
```

The process logs the iteration counts it calibrated on this machine (or the counts you passed
with `-baseline-iters` and `-degraded-iters`). There is no admin port. Stop it with Ctrl+C.

### Run the experiment, analysis, and investigation

Use a portable k6 if `k6` is not on `PATH` (`-k6` or `RUPTIQ_K6_PATH`). Write reports to a local
directory that is not committed (`tmp/` is gitignored):

```sh
mkdir tmp
go run ./cmd/ruptiq-experiment -url http://127.0.0.1:8090/work -metrics-url http://127.0.0.1:8090/metrics -vus 2 -duration 20s -interval 1s -out tmp/experiment.json
go run ./cmd/ruptiq-analyze -in tmp/experiment.json -out tmp/analysis.json -requests-metric regression_lab_http_requests_total -duration-metric regression_lab_http_request_duration_seconds
go run ./cmd/ruptiq-investigate -in tmp/analysis.json -out tmp/investigation.json
```

Leave the investigator's default thresholds unchanged. About 20 seconds at 1s sampling is enough
for several baseline windows and several degraded windows with the default 10s switch.

### Review the evidence

In `tmp/investigation.json`, each finding lists `evidence` JSON pointers into `tmp/analysis.json`
(array indexes count from 0; window numbers from 1). Resolve a pointer such as
`/analysis/windows/1/routes/1/latency/p95` to the p95 object of analysis window 2. Confirm that
`regression_lab_degraded` in `tmp/experiment.json` flips from 0 to 1 near `-degrade-after`, and
that later analysis windows are the slow ones.

### Expected behaviour

After the first `GET /work`, the lab stays in baseline mode until `-degrade-after`. Then
`regression_lab_degraded` becomes 1 and each `/work` does more CPU loop iterations. Server-side
request rates should fall and histogram p95 should move into a later bucket. Windows that touch
the workload's start or end are excluded by the investigator; a window that straddles the switch
may be left out as the middle window of an odd count. `GET /healthz` has almost no traffic during
the load, so that route should abstain.

k6's **whole-run** p95 can stay close to the fast baseline: most of the requests happen in the
first half, so they dominate a single percentile over 20s. The investigator compares earlier
windows with later windows, which is the point of this fixture.

### Observed behaviour (one run)

One run on the development machine (Windows, portable k6 v2.3.0, 2 VUs, 20s, 1s interval, lab
on `127.0.0.1:18090`, `-degrade-after 10s`, calibrated `baseline_iters=157135` and
`degraded_iters=11233031`). Reports were written under the machine's temp directory, not Git.

| Signal | Observed |
| --- | --- |
| k6 | 39379 requests, 1968.0 req/s, 0% failed, avg 0.942 ms, p95 1.461 ms, max 89.335 ms, 20.01s |
| Prometheus snapshots | 22 of 22 scrapes succeeded (1 before, 20 during, 1 after) |
| `regression_lab_degraded` | 0 through sample #11 (+10.0s), 1 from sample #12 (+11.0s) through #22 |
| GET `/work` baseline (windows 2–10) | median 4577.2 req/s, p95 estimate ~0.933 ms (0.500–1.000 ms bucket) |
| GET `/work` degraded (windows 12–20) | median 153.0 req/s, p95 estimate ~24.163 ms (10–25 ms bucket) |
| Transition | window 11 (samples #11–#12): 2026.9 req/s, p95 still ~0.996 ms; left out as the middle window |

`ruptiq-investigate` outcome: `changes_observed`. Ranked findings (default thresholds, none
weakened):

1. `combined_degradation` (major, **hypothesis**) for GET `/work`: p95 +2489.7% and throughput
   −96.7% over the same windows 2–10 vs 12–20. Not a proven cause.
2. `latency_degradation` (major, observation): ~0.933 ms → ~24.163 ms. Evidence starts at
   `/analysis/windows/1/routes/1/latency/p95` (window 2 p95 = 0.000943 s, matching the analysis).
3. `throughput_degradation` (major, observation): 4577.2 → 153.0 req/s. Evidence starts at
   `/analysis/windows/1/routes/1/requests` (window 2 = 1702.5 req/s, 1649 requests).
4. `insufficient_evidence` for GET `/healthz`: no load on that route.

Every evidence pointer in that investigation resolved to an object in the saved analysis. The
output matched the controlled switch: later windows were the slow, low-rate ones, and the
independent `regression_lab_degraded` gauge flipped at 10s. This does **not** show a CPU, database,
or network bottleneck in a real application; the lab added CPU work on purpose.

### Before and after runs

This check uses two separate experiments against the same lab process shape, not one experiment
that changes mode in the middle. The "improvement" is the lab doing less CPU work per `GET /work`.
It is not a source patch, and `ruptiq-validate-fix` is not applying or proving a fix.

Both experiments use one machine, `http://127.0.0.1:8090/work`, `http://127.0.0.1:8090/metrics`,
k6 v2.3.0, 2 VUs, a 20s workload, a 1s scrape interval, and the same analysis metric names.
Iteration counts are passed explicitly (`-baseline-iters 157135`, `-degraded-iters 11233031`)
so the two processes do not recalibrate. Run only one lab at a time. Stop it before starting
the next. Reports stay in gitignored `tmp/`.

#### Expected behavior

With `-degrade-after 0s`, the first `GET /work` enters the heavier loop immediately. Samples
taken while that workload is running should show `regression_lab_degraded` 1 and
`regression_lab_work_iterations` 11233031. The scrape taken before any `/work` can still show
0 and 157135, because the gauges start in baseline until the first `/work`.

With `-degrade-after 1h`, a 20s workload should finish before the timer. Every sample should
show `regression_lab_degraded` 0 and `regression_lab_work_iterations` 157135.

The lighter run should complete more server requests per second, and its median per-window p95
should sit in an earlier histogram bucket than the heavier run. The 5xx ratio should stay 0%
when requests were completed. Default thresholds are left unchanged. `GET /healthz` receives
no load, so that route should not be treated as a performance improvement. Swapping which run
is "before" should swap the direction of the judged changes. Two pairs still do not show
statistical significance or that the result would recur on another machine.

#### Commands

Portable k6 v2.3.0 was used through `-k6`. `RUPTIQ_K6_PATH` or a `k6` on `PATH` is the same
binary if it is that version. In one terminal, start the heavier lab:

```sh
mkdir -p tmp/fix-validation
go run ./demo/regression-lab -addr 127.0.0.1:8090 -degrade-after 0s -baseline-iters 157135 -degraded-iters 11233031
```

In another terminal, from the repository root:

```sh
go run ./cmd/ruptiq-experiment -url http://127.0.0.1:8090/work -metrics-url http://127.0.0.1:8090/metrics -vus 2 -duration 20s -interval 1s -k6 tmp/k6/k6-v2.3.0-windows-amd64/k6.exe -out tmp/fix-validation/before-experiment.json
go run ./cmd/ruptiq-analyze -in tmp/fix-validation/before-experiment.json -out tmp/fix-validation/before-analysis.json -requests-metric regression_lab_http_requests_total -duration-metric regression_lab_http_request_duration_seconds
```

Stop the lab (Ctrl+C) and confirm nothing is still listening on `127.0.0.1:8090`. Start the
lighter lab on the same address:

```sh
go run ./demo/regression-lab -addr 127.0.0.1:8090 -degrade-after 1h -baseline-iters 157135 -degraded-iters 11233031
```

```sh
go run ./cmd/ruptiq-experiment -url http://127.0.0.1:8090/work -metrics-url http://127.0.0.1:8090/metrics -vus 2 -duration 20s -interval 1s -k6 tmp/k6/k6-v2.3.0-windows-amd64/k6.exe -out tmp/fix-validation/after-experiment.json
go run ./cmd/ruptiq-analyze -in tmp/fix-validation/after-experiment.json -out tmp/fix-validation/after-analysis.json -requests-metric regression_lab_http_requests_total -duration-metric regression_lab_http_request_duration_seconds
```

Stop that lab as well, then compare with the default thresholds:

```sh
go run ./cmd/ruptiq-validate-fix -before tmp/fix-validation/before-analysis.json -after tmp/fix-validation/after-analysis.json -out tmp/fix-validation/validation.json
```

The mode check is the saved telemetry, which is the Prometheus text the lab exposed. In each
sample of the experiment JSON, read `regression_lab_degraded` and
`regression_lab_work_iterations`. `GET /metrics` on the running lab shows the same gauges at
that moment.

A second pair ran only after the first lab was stopped. The lighter lab was started first and
the heavier lab second, with new files so the first pair was kept:

```sh
go run ./demo/regression-lab -addr 127.0.0.1:8090 -degrade-after 1h -baseline-iters 157135 -degraded-iters 11233031
go run ./cmd/ruptiq-experiment -url http://127.0.0.1:8090/work -metrics-url http://127.0.0.1:8090/metrics -vus 2 -duration 20s -interval 1s -k6 tmp/k6/k6-v2.3.0-windows-amd64/k6.exe -out tmp/fix-validation/repeat-fast-experiment.json
go run ./cmd/ruptiq-analyze -in tmp/fix-validation/repeat-fast-experiment.json -out tmp/fix-validation/repeat-fast-analysis.json -requests-metric regression_lab_http_requests_total -duration-metric regression_lab_http_request_duration_seconds
```

Stop that process, then:

```sh
go run ./demo/regression-lab -addr 127.0.0.1:8090 -degrade-after 0s -baseline-iters 157135 -degraded-iters 11233031
go run ./cmd/ruptiq-experiment -url http://127.0.0.1:8090/work -metrics-url http://127.0.0.1:8090/metrics -vus 2 -duration 20s -interval 1s -k6 tmp/k6/k6-v2.3.0-windows-amd64/k6.exe -out tmp/fix-validation/repeat-slow-experiment.json
go run ./cmd/ruptiq-analyze -in tmp/fix-validation/repeat-slow-experiment.json -out tmp/fix-validation/repeat-slow-analysis.json -requests-metric regression_lab_http_requests_total -duration-metric regression_lab_http_request_duration_seconds
go run ./cmd/ruptiq-validate-fix -before tmp/fix-validation/repeat-fast-analysis.json -after tmp/fix-validation/repeat-slow-analysis.json -out tmp/fix-validation/repeat-validation.json
```

#### Actually observed results

One development machine, Windows, portable k6 v2.3.0, commit `5b5473e`. The commands above
were run from binaries built from that commit into gitignored `tmp/bin`, with the same flags.
Nothing in the comparison thresholds or the saved JSON was edited afterward. The numbers below
are what this Windows machine measured in these runs. They are not a benchmark.

| | Heavier CPU (`-degrade-after 0s`) | Lighter CPU (`-degrade-after 1h`) |
| --- | --- | --- |
| k6 | 2300 requests, 115.0 req/s, 0% failed, avg 17.299 ms, p95 25.750 ms, max 216.348 ms, 20.01s | 47325 requests, 2366.1 req/s, 0% failed, avg 0.811 ms, p50 0.744 ms, p95 1.232 ms, max 71.328 ms, 20.00s |
| Prometheus | 22 of 22 scrapes (1 before, 20 during, 1 after) | 22 of 22 scrapes (1 before, 20 during, 1 after) |
| `regression_lab_degraded` | 0 on sample #1 (before any `/work`); 1 on samples #2–#22 | 0 on all 22 samples |
| `regression_lab_work_iterations` | 157135 on sample #1; 11233031 on samples #2–#22 | 157135 on all 22 samples |
| Server, all analysis windows, `GET /work` | 2300 requests in 20.202s (113.9 req/s), p95 estimate ~24.571 ms, 0 of 2300 5xx | 47325 requests in 20.160s (2347.5 req/s), p95 estimate ~0.951 ms in the 0.500–1.000 ms bucket, 0 of 47325 5xx |

`ruptiq-validate-fix` called the files comparable. Each side had 21 windows and 19 eligible
windows (2–20). Windows 1 and 21 touch the start or the end of the load and were excluded.
Default thresholds were kept.

For eligible `GET /work` windows the validator measured:

| Signal | Before (heavier) | After (lighter) | Result |
| --- | --- | --- | --- |
| Throughput | 2168 requests / 18.9999s = 114.1 req/s | 45106 requests / 18.9993s = 2374.1 req/s | +1980.6%, improvement |
| 5xx ratio | 0.00% (0 of 2168) | 0.00% (0 of 45106) | no material change |
| Median window p95 | 24.250 ms, bucket union 10 ms–100 ms | 0.951 ms, bucket union 0.500 ms–1.000 ms | −96.1%, improvement |

The outcome was `improvement_observed`. Ranked findings, regressions first:

1. `GET /work` throughput, improvement, +1980.6%.
2. `GET /work` p95, improvement, −96.1%.

The first throughput pointer is `/analysis/windows/1/routes/1/requests` (analysis window 2,
136 requests in the heavier file). The first p95 pointer is
`/analysis/windows/1/routes/1/latency/p95` (0.024221 s, bucket 10–25 ms). All 76 `GET /work`
evidence pointers in the validation report resolved to an object in the file named by that
side. `GET /healthz` had no requests. Its throughput is 0.0 → 0.0 requests/s
(`no_material_change`), its 5xx ratio is undefined on both sides, and its latency has no
eligible windows. The only comparability warning is that the k6 client results differ; they
are not part of the server-side outcome.

The heavier run was noisy. Eligible window 8 estimated p95 at 71.25 ms (50–100 ms bucket,
46 requests) and other windows reached the 25–50 ms bucket, so the before bucket union is
10–100 ms rather than only 10–25 ms. That union still does not overlap 0.500–1.000 ms.

The reversed pair was completed. The lighter run started at 00:06:44Z and the heavier run at
00:07:37Z, after the lighter process had stopped. Their experiment files are separate from the
first pair. The lighter run came first (k6: 50896 requests, 2544.7 req/s, 0 failed, p95 1.138 ms;
22 of 22 scrapes; every sample degraded 0 and 157135 iterations). The heavier run came second
(k6: 2646 requests, 132.2 req/s, 0 failed, p95 17.859 ms; 22 of 22 scrapes; sample #1 was still
baseline, and samples #2–#22 were degraded at 11233031 iterations).

Comparing the lighter analysis as before and the heavier analysis as after was
`regression_observed`. Eligible `GET /work` windows: throughput 48671 requests / 18.9990s =
2561.8 req/s → 2523 requests / 19.0009s = 132.8 req/s (−94.8%); median window p95 0.950 ms →
24.215 ms (+2448.4%), buckets `[0.500 ms, 1.000 ms]` to `[10.000 ms, 25.000 ms]`; 5xx ratio
0.00% on both sides (0 of 48671, then 0 of 2523). Windows 1 and 21 were excluded on both sides.
No `GET /work` signal was inconclusive. Ranked findings were p95 regression first, then
throughput regression. All 76 `GET /work` evidence pointers resolved. The first request pointer,
`/analysis/windows/1/routes/1/requests`, is 2582 requests in the lighter file, and the matching
p95 is 0.000950 s in the 0.500–1.000 ms bucket. The k6 warning is the same as the first pair.
The direction followed the CPU mode. The percentages are not the same size as the first pair.
Two observations are not a significance test and not evidence the same magnitudes would appear
again.

### Options

| Flag | Default | Meaning |
| --- | --- | --- |
| `-addr` | `127.0.0.1:8090` | Listen address. Must be loopback (`127.0.0.1`, `::1`, or `localhost`). |
| `-degrade-after` | `10s` | Delay after the first `GET /work` before heavier CPU work. `0` degrades immediately. |
| `-baseline-iters` | `0` (calibrate to ~250µs) | CPU-loop iterations per `/work` in baseline mode. Must be between 0 and 50,000,000. |
| `-degraded-iters` | `0` (calibrate to ~12ms) | CPU-loop iterations per `/work` in degraded mode. Must be greater than baseline and at most 50,000,000. |

### Tests

`go test ./demo/regression-lab` covers `/work` and `/healthz`, real counters and histograms, the
mode switch, concurrent requests during the switch, bounded metric labels and iteration flags,
loopback binding, and process startup/shutdown. Health and metrics scrapes do not start the
timer. It does not start k6. Set `RUPTIQ_LIVE_K6=1` only to confirm a k6 binary is visible; the
20s pipeline above is the live check.

### Limitations

- Calibration of iteration counts varies by CPU and load; read them from the listening log if you
  need to repeat a run. Other machines will not match the table above.
- Environmental noise (other processes, turbo boost, two VUs sharing cores) moves the exact rates.
  The buckets in this run were far apart; a quieter or busier machine might land differently.
- k6's whole-run percentile is the wrong tool for a mid-run change; use the windowed analysis.
- The lab has no authentication, no persistence, and no checkout behaviour. Do not expose it off
  loopback.

## Checkout demo with Docker Compose

`compose.yaml` at the repository root runs the three checkout services together with one
command. Ruptiq Core is not part of it.

> **Status:** the CI job `Docker checkout integration` builds the images, starts the environment,
> waits for every service to be healthy, and runs the smoke test below on GitHub's Ubuntu runners;
> it passes. Docker is not installed on the development machine, so it has not been run there.

### Automated smoke test

`scripts/checkout-smoke-test.sh` runs real checkout scenarios over HTTP and stops at the first
failed check, printing the request, status, and body. It needs `bash`, `curl`, and `jq`, and a
**freshly started** environment (it asserts the seed stock):

```sh
docker compose up --build -d --wait
bash scripts/checkout-smoke-test.sh
docker compose down
```

It covers a successful checkout, idempotent repeats (no second stock deduction, the same payment
returned, no second approval with a new key), and a decline followed by an approval with a new
key. At the end it reads the Inventory Service's `/metrics` and checks that the stock gauges match
the checkouts, that exactly two reservations were counted as created, that the stock-read count
equals the script's own reads, and that no order, reservation, or payment ID appears. Override `ORDER_URL`, `INVENTORY_URL`, or `PAYMENT_URL` to point it elsewhere. The CI job runs
exactly these steps on every push to `main` and every pull request.

### Prerequisites

- Docker with the Compose plugin (`docker compose version` should print a version).
- Ports `8081`, `8082`, and `8083` free on `127.0.0.1` (stop any `go run` copies of the services).
- Network access on the first build, to pull `golang:1.27-alpine` and `alpine:3.24`.

### Services and ports

| Service | Compose name | Host URL | URL between containers |
| --- | --- | --- | --- |
| Order Service | `order-service` | `http://localhost:8081` | `http://order-service:8081` |
| Inventory Service | `inventory-service` | `http://localhost:8082` | `http://inventory-service:8082` |
| Payment Service | `payment-service` | `http://localhost:8083` | `http://payment-service:8083` |

Ports are published on `127.0.0.1` only, so the services are not reachable from other machines.
Inside the Compose network, the Order Service reaches the others by service name
(`INVENTORY_SERVICE_URL=http://inventory-service:8082`,
`PAYMENT_SERVICE_URL=http://payment-service:8083`); `localhost` inside a container means that
container itself.

### Start, inspect, stop

From the repository root:

```powershell
docker compose config            # validate and print the resolved configuration
docker compose up --build -d     # build the images and start everything in the background
docker compose ps                # each service should show "(healthy)" after a few seconds
docker compose logs -f order-service   # follow one service's logs (Ctrl+C stops following)
docker compose down              # stop and remove the containers and the network
```

Startup order: the Inventory and Payment Services start first; the Order Service starts once both
report healthy. Every service is checked by calling its own `GET /healthz` every 5 seconds.
**Healthy only means the process answers `/healthz`**; it does not prove that a checkout works.
Use the walkthrough below for that.

### Checkout walkthrough (PowerShell)

Once `docker compose ps` shows all three services healthy:

```powershell
$O = "http://localhost:8081"; $I = "http://localhost:8082"; $P = "http://localhost:8083"

Invoke-RestMethod "$I/inventory/LAPTOP-001"                         # available 10, reserved 0

$order = Invoke-RestMethod -Method Post -Uri "$O/orders" -ContentType "application/json" -Body '{"sku":"LAPTOP-001","quantity":2}'
Invoke-RestMethod -Method Post -Uri "$O/orders/$($order.id)/reserve"   # status reserved
Invoke-RestMethod "$I/inventory/LAPTOP-001"                         # available 8, reserved 2

$approve = '{"payment_method":"test_approve","idempotency_key":"checkout-1"}'
$paid = Invoke-RestMethod -Method Post -Uri "$O/orders/$($order.id)/pay" -ContentType "application/json" -Body $approve
$paid.order.status                                                  # paid
Invoke-RestMethod "$P/payments/$($paid.payment.id)"                 # the approved payment

# Repeat both calls with the same identifiers: same reservation, same payment, stock still 8.
Invoke-RestMethod -Method Post -Uri "$O/orders/$($order.id)/reserve"
(Invoke-RestMethod -Method Post -Uri "$O/orders/$($order.id)/pay" -ContentType "application/json" -Body $approve).payment.id
Invoke-RestMethod "$I/inventory/LAPTOP-001"                         # still available 8

# A declined payment leaves a second order reserved.
$phone = Invoke-RestMethod -Method Post -Uri "$O/orders" -ContentType "application/json" -Body '{"sku":"PHONE-7","quantity":1}'
Invoke-RestMethod -Method Post -Uri "$O/orders/$($phone.id)/reserve"
$d = Invoke-RestMethod -Method Post -Uri "$O/orders/$($phone.id)/pay" -ContentType "application/json" -Body '{"payment_method":"test_decline","idempotency_key":"try-1"}'
$d.payment.status                                                   # declined
(Invoke-RestMethod "$O/orders/$($phone.id)").status                 # reserved
```

### State is lost on restart

Every service keeps its data in memory. `docker compose restart`, `docker compose down`, a
rebuild, or a crash **loses** that service's orders, reservations, payments, and idempotency
records; inventory goes back to its seed stock. In particular, after the Payment Service restarts,
a retry with an idempotency key that was already approved creates a **second approved payment**
(this was observed with the same binaries run as plain processes). Nothing here is restart-safe or
persistent. Containers are set to `restart: "no"` so that a crash is visible instead of silently
replaced by an empty service.

### Troubleshooting

- **`docker` is not recognized / cannot connect to the Docker daemon**: install or start Docker
  Desktop, then check `docker version` and `docker compose version`.
- **`Bind for 127.0.0.1:8081 failed: port is already allocated`** (or 8082, 8083): another
  process uses the port, often a `go run` copy of a service. Find it with
  `Get-NetTCPConnection -LocalPort 8081 -State Listen` and stop it, then run `docker compose up -d`
  again.
- **A service stays `(health: starting)` or becomes `(unhealthy)`**: read its logs with
  `docker compose logs <service>`. `docker inspect --format '{{json .State.Health}}' <container>`
  shows the last health check results.
- **`order-service` does not start ("dependency failed to start")**: the Inventory or Payment
  Service did not become healthy; fix that one first.
- **Reserve or pay returns `503`**: the Order Service cannot reach a dependency. Check
  `docker compose ps`, and that `docker compose config` shows the service-name URLs above.
- **Code changes are not picked up**: rebuild with `docker compose up --build -d`.
- **Unexpected data, or a fresh start is needed**: `docker compose down`, then
  `docker compose up -d` (all state is reset).

### Container details

- One multi-stage `demo/checkout/Dockerfile` builds any of the three services, chosen by the
  `SERVICE` build argument. The build stage (`golang:1.27-alpine`) compiles a static Linux binary
  with `CGO_ENABLED=0`; the runtime stage is `alpine:3.24` with only that binary.
- Containers run as a non-root user (UID `10001`), with a read-only root filesystem, all Linux
  capabilities dropped, and `no-new-privileges`.
- The root `.dockerignore` is an allow-list: only `go.mod`, `go.sum`, and `demo/checkout/`
  (without tests) are sent to the build, so Ruptiq Core and `.git` never enter these images. The
  build downloads Go modules (the Prometheus client) in their own cached layer.

## Checkout demo: Order Service

The Order Service is the first piece of the checkout demo application. It is the **system under
test** that Ruptiq will eventually load test. It is **not** part of Ruptiq Core: it lives in
`demo/checkout/order-service/`, never imports Ruptiq Core packages, and is started separately.

**What it does:** it creates orders, returns them by ID, reserves stock for an order by calling the
Inventory Service over HTTP, and pays for a reserved order by calling the Payment Service over HTTP.
Orders are stored **in memory only** and are lost when the service stops.

An order moves forward only, through three statuses:

- `created`: only the order record exists. No stock is held.
- `reserved`: the Inventory Service confirmed a stock reservation, recorded in `reservation_id`.
  No payment is approved yet (a declined attempt leaves the order here).
- `paid`: the Payment Service approved a payment, recorded in `payment_id` with the
  `payment_idempotency_key` that produced it. A paid order never goes back to `reserved`.

**What it does not do:** payments are **simulated** (no card data, no real money). There is no
shipping, cancellation, refund, or reservation release. The service also has no authentication,
no database, and no deliberate slowness or failures yet.

### Endpoints

| Method and path | Success | Description |
| --- | --- | --- |
| `GET /healthz` | `200` | `{"status":"ok","service":"order-service"}` |
| `POST /orders` | `201` | Create an order from `{"sku": "...", "quantity": 1-10}` |
| `GET /orders/{id}` | `200` | Fetch an order by ID |
| `POST /orders/{id}/reserve` | `200` | Reserve the order's stock in the Inventory Service (no request body) |
| `POST /orders/{id}/pay` | `200` | Pay for a reserved order through the Payment Service (simulated) |

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

### Paying for an order: `POST /orders/{id}/pay`

Request body (nothing else is accepted; unknown fields such as `amount_cents` or `card_number`
are rejected with `400`):

```json
{"payment_method":"test_approve","idempotency_key":"checkout-1"}
```

- `payment_method` is a **test switch**, not a payment instrument: `test_approve` or
  `test_decline`.
- `idempotency_key` identifies one payment attempt for this order: required, at most 64
  bytes (UTF-8), no control characters, surrounding whitespace trimmed. The key sent upstream,
  `<order id>:<key>`, is also checked against the Payment Service's 128-byte limit before any call
  (`422` if over). Use a new key for a new
  attempt and the **same key to retry** an attempt whose outcome you did not get.

The **amount is computed by the Order Service**, never taken from the client: quantity times the
unit price from a fixed **test-only price catalog**, in USD integer cents (overflow is checked).

| SKU | Unit price |
| --- | --- |
| `LAPTOP-001` | 129999 cents ($1,299.99) |
| `PHONE-7` | 79999 cents ($799.99) |
| `HEADPHONES-01` | 14999 cents ($149.99) |

The Order Service sends `order_id`, `amount_cents`, `currency` (`USD`), `payment_method`, and the
idempotency key **scoped to the order** (`<order id>:<your key>`, because the Payment Service's
keys are global) to the Payment Service's `POST /payments`. Both approvals and declines return
`200` with the order and the payment:

```json
{"order":{"id":"ord_FKUIXEYPM6BP5TXASNVRGY7BS6","sku":"LAPTOP-001","quantity":2,"status":"paid","reservation_id":"res_YBIE5C6GH37LQDAK5OIS5THMEK","payment_id":"pay_36SVTA54SBK2Y6NVLMRRE7NGU4","payment_idempotency_key":"checkout-1","created_at":"2026-11-06T09:10:59.8865341Z"},"payment":{"id":"pay_36SVTA54SBK2Y6NVLMRRE7NGU4","status":"approved","amount_cents":259998,"currency":"USD"}}
```

- **Approved**: the order moves from `reserved` to `paid` with `payment_id` and
  `payment_idempotency_key`.
- **Declined**: `payment.status` is `declined` and the order **stays `reserved`** (stock is still
  held). A decline is a normal outcome, not an error. Try again with a **new** key; repeating the
  declined key replays the decline.
- **Already paid**: answered by the Order Service without calling the Payment Service. The same
  key and `test_approve` returns the recorded approval again (a safe replay); any other key is
  `409 order is already paid`; the same key with `test_decline` is `409`. A paid order can never be
  paid twice.

| Status | When |
| --- | --- |
| `200` | Approved, declined, or a replay of the order's recorded approval |
| `400` / `413` | Malformed, empty, unknown-field, or oversized body |
| `404` | Order not found |
| `409` | Order not reserved yet; already paid (other key); key reused with another `payment_method`; or the Payment Service already holds an approval for this order that is not recorded here (see below) |
| `422` | Missing or invalid `payment_method` or `idempotency_key`; or the SKU has no catalog price |
| `502` | The Payment Service answered with something unexpected (malformed, or not matching the order, amount, currency, or requested outcome) |
| `503` | The Payment Service could not be reached or failed (5xx), or is not configured |
| `504` | The Payment Service did not answer within 5 seconds |

**On every error the order is left unchanged.** For `502`, `503`, and `504` the payment outcome is
unknown: the Payment Service may have approved it. **Retry with the same `idempotency_key`**: the
Payment Service replays the recorded result, and an approval is then recorded on the order. The
Order Service never retries on its own and never invents a new key.

**Recovering a lost approval.** If an approval was recorded by the Payment Service but its answer
never reached the Order Service, and the client then tries a **different** key, the Payment Service
refuses a second approval and the Order Service returns `409` asking to retry with the key that
produced the approval. The Payment Service cannot be searched by order, so the Order Service cannot
find that payment by itself; the order stays `reserved` until the original key is retried.

### Start it

The service listens on port `8081` by default. Set `ORDER_SERVICE_PORT` to change it.

It finds the Inventory Service through `INVENTORY_SERVICE_URL` (default `http://localhost:8082`)
and the Payment Service through `PAYMENT_SERVICE_URL` (default `http://localhost:8083`). Each value
must be an absolute `http` or `https` URL with no path, query, or credentials; an invalid value
stops the service at startup. The Order Service starts even if the other services are not running;
only reserve or pay calls fail (`503`) until they are reachable.

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

### Run all three services together (PowerShell)

Use four terminals, all in the repository root. Terminals 1 and 2 are the Inventory Service and the
Payment Service:

```powershell
go run ./demo/checkout/inventory-service      # terminal 1, port 8082
```

```powershell
go run ./demo/checkout/payment-service        # terminal 2, port 8083
```

Terminal 3, the Order Service (both URLs are the defaults, shown for clarity):

```powershell
$env:INVENTORY_SERVICE_URL = "http://localhost:8082"
$env:PAYMENT_SERVICE_URL = "http://localhost:8083"
go run ./demo/checkout/order-service
```

Terminal 4, approve a payment, then repeat it:

```powershell
$order = Invoke-RestMethod -Method Post -Uri http://localhost:8081/orders -ContentType "application/json" -Body '{"sku":"LAPTOP-001","quantity":2}'
Invoke-RestMethod -Method Post -Uri "http://localhost:8081/orders/$($order.id)/reserve"   # status reserved

$approve = '{"payment_method":"test_approve","idempotency_key":"checkout-1"}'
$r = Invoke-RestMethod -Method Post -Uri "http://localhost:8081/orders/$($order.id)/pay" -ContentType "application/json" -Body $approve
$r.order.status       # paid
$r.payment            # approved, amount_cents 259998, USD

# Same key again: the same payment comes back and no new payment is created.
(Invoke-RestMethod -Method Post -Uri "http://localhost:8081/orders/$($order.id)/pay" -ContentType "application/json" -Body $approve).payment.id

# The Payment Service holds the same payment.
Invoke-RestMethod "http://localhost:8083/payments/$($r.payment.id)"
```

Decline, then approve with a new key:

```powershell
$phone = Invoke-RestMethod -Method Post -Uri http://localhost:8081/orders -ContentType "application/json" -Body '{"sku":"PHONE-7","quantity":1}'
Invoke-RestMethod -Method Post -Uri "http://localhost:8081/orders/$($phone.id)/reserve"

$d = Invoke-RestMethod -Method Post -Uri "http://localhost:8081/orders/$($phone.id)/pay" -ContentType "application/json" -Body '{"payment_method":"test_decline","idempotency_key":"try-1"}'
$d.payment.status     # declined
$d.order.status       # reserved

$a = Invoke-RestMethod -Method Post -Uri "http://localhost:8081/orders/$($phone.id)/pay" -ContentType "application/json" -Body '{"payment_method":"test_approve","idempotency_key":"try-2"}'
$a.order.status       # paid
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

Payment-specific limitations:

- **A lost approval needs the original key to recover.** If the Payment Service approved an order
  but the answer was lost, the order stays `reserved` until the client retries with that key. A
  different key gets `409`, and no second approval happens. The Payment Service has no lookup by
  order, so the Order Service cannot reconcile on its own.
- **Restarts break the payment guarantees.** If the Order Service restarts, its paid orders are
  gone. If the Payment Service restarts, its idempotency keys and approvals are gone: a retry
  after an ambiguous failure could then approve again, and a `reserved` order could be approved
  twice across the restart.
- **No cancellation or compensation.** A declined or abandoned order keeps its stock reserved.
  Nothing refunds a payment or releases stock.
- **Prices are a fixed test catalog** in the Order Service. Other SKUs cannot be paid (`422`), and
  prices are not shared with the Inventory Service.
- **Error classification depends on message text** for the Payment Service's two `409` cases, as
  for the Inventory Service.

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
| `GET /metrics` | `200` | Prometheus metrics in the text exposition format (see [Metrics](#metrics-prometheus)) |

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

### Metrics (Prometheus)

`GET /metrics` returns the service's own measurements in the Prometheus text format, produced with
the official [Prometheus Go client](https://github.com/prometheus/client_golang). Each running
service has its own metrics registry; nothing is global.

| Metric | Type | Labels | What it measures |
| --- | --- | --- | --- |
| `inventory_http_requests_total` | counter | `method`, `route`, `status` | HTTP requests the service finished handling. |
| `inventory_http_request_duration_seconds` | histogram | `method`, `route`, `status` | How long the service took to handle each request, in seconds. |
| `inventory_http_requests_in_flight` | gauge | none | Requests being handled right now. |
| `inventory_stock_available_units` | gauge | `sku` | Units that can still be reserved. |
| `inventory_stock_reserved_units` | gauge | `sku` | Units held by reservations. |

The duration histogram's buckets run from 100 microseconds to 10 seconds (0.1, 0.25, 0.5, 1, 2.5,
5, 10, 25, 50, 100, 250, and 500 ms, then 1, 2.5, 5, and 10 s), because most requests to this
in-memory service take well under a millisecond.

**Inspect it** while the service runs (directly or in Docker Compose):

```powershell
(Invoke-WebRequest http://localhost:8082/metrics -UseBasicParsing).Content
curl.exe -s http://localhost:8082/metrics
```

An excerpt from a running service after one reservation of 2 laptops (`# HELP` and `# TYPE`
lines, the histogram, and the counters for other requests omitted):

```text
inventory_http_requests_total{method="POST",route="/reservations",status="201"} 1
inventory_http_requests_in_flight 0
inventory_stock_available_units{sku="HEADPHONES-01"} 100
inventory_stock_available_units{sku="LAPTOP-001"} 8
inventory_stock_available_units{sku="PHONE-7"} 50
inventory_stock_reserved_units{sku="HEADPHONES-01"} 0
inventory_stock_reserved_units{sku="LAPTOP-001"} 2
inventory_stock_reserved_units{sku="PHONE-7"} 0
```

**What the numbers mean:**

- A request is counted, and its duration recorded, when its handler returns. The duration starts
  when the request reaches the service's handler (after Go's HTTP server has read the request
  headers) and includes reading the body, the stock update, and writing the response into Go's
  buffers. It does not include network time, waiting for a connection, or the time the operating
  system takes to send the response.
- The stock gauges are not counters kept alongside the stock: every scrape reads the store's
  current state under its lock, all SKUs at once, so they cannot drift out of step with
  `GET /inventory/{sku}`. A reservation shows up completely or not at all. Idempotent replays and
  rejected reservations do not change them.
- Requests to `/metrics` itself are not counted, timed, or shown as in flight, so scraping does not
  look like application traffic. They are still written to the request log.
- All values live in memory and start again from zero (or from the seed stock) when the service
  restarts. Prometheus treats a counter that goes back to zero as a restart.

**Why the labels are bounded:** every distinct combination of label values is a separate time
series, kept in memory by the service and by anything that stores its metrics. Labels copied from
requests would grow without limit (one series per order or per mistyped URL) and could expose IDs
or secrets. So:

- `route` is the route template, never the request path: `/healthz`, `/inventory/{sku}`,
  `/reservations`, or `unmatched` for any other path (including paths Go's router redirects, such
  as `//inventory/LAPTOP-001`). `GET /inventory/ANYTHING` always counts under `/inventory/{sku}`.
- `method` is a standard HTTP method; any other method is counted as `OTHER`.
- `status` is a status code the service itself sends.
- `sku` on the stock gauges comes from the catalog in the store (three SKUs), never from a request.
  An unknown SKU in a request does not create a series.
- Order IDs, reservation IDs, request bodies, query strings, and credentials are never used in
  metrics.

**Server-side versus k6 client-side numbers.** `ruptiq-run` reports what k6 saw from the client:
the time from sending a request to receiving the whole response, including the network, connection
handling, and any queueing before the service's handler starts. The service's histogram covers only
the part inside the service. When k6's latency is much higher than the service's, the extra time is
spent outside the handler. The counts differ too: k6 counts requests that never reached the service
(refused connections, timeouts), and the service counts requests from every client, not just k6.
Neither number replaces the other. On Windows, the clock behind these timings can record very fast
requests as 0 seconds; this was seen when running the service locally.

**Reading it with Ruptiq Core:** `ruptiq-telemetry` reads one snapshot of these metrics (see
[Read a telemetry snapshot](#read-a-telemetry-snapshot-ruptiq-telemetry)), and
`ruptiq-experiment` samples them before, during, and after a k6 load test (see
[Run an experiment](#run-an-experiment-ruptiq-experiment)); `ruptiq-analyze` turns them into
per-window rates and estimated percentiles, and `ruptiq-investigate` ranks the changes between
windows. What Ruptiq Core still needs to investigate this telemetry fully:

- Saving telemetry with each run (`RunStore`), so that comparisons can include it.
- A fuller `Investigator`: `ruptiq-investigate` compares windows within one experiment's server-side
  signals, but does not relate them to the client-side numbers, compare experiments, or measure
  resources such as CPU.
- Metrics from the Order and Payment Services; today only the Inventory Service has them.

### Current limitations

- All state is in memory: a restart resets stock and forgets reservations.
- Reservations are never released, expired, or confirmed, so stock reserved by abandoned orders is
  never returned.
- The idempotency record lives only as long as the process, and one lock guards all SKUs, so
  reservations for different SKUs do not run in parallel. Both are deliberate simplifications.
- No authentication, no database, and no deliberate slowness or failures. It makes no outgoing
  calls; the Order Service calls its `POST /reservations` to reserve stock for orders.
- `/metrics` is unauthenticated like every other endpoint, and shows stock levels and traffic
  counts. Docker Compose publishes the port on `127.0.0.1` only. No Prometheus server or dashboard
  is included.
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

### Called by the Order Service

The Order Service's `POST /orders/{id}/pay` calls `POST /payments` for reserved orders, with the
amount computed from its price catalog and the client's key scoped to the order (see the Order
Service section). What to do when a step fails midway (for example releasing a reservation after
a decline) is not implemented.

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

None of the tests need k6, Docker, or network access: the k6 adapter is tested against a fake k6
program (the test binary itself) and saved k6 summaries, the experiment coordinator against
fake workload runners, fake telemetry readers, a fake clock, and a manually driven ticker, the
analyzer against synthetic experiment reports and the experiment package's saved report, and the
investigator against fake analyses, the saved analysis of the synthetic experiment, and an offline
experiment-to-analysis-to-investigation chain. The before/after comparison is tested against fake
analyses and against that same saved analysis used as both inputs. The regression lab's tests hit its own handler only;
they do not run k6.

`go test -race` turns on Go's race detector, which needs cgo and a 64-bit C compiler. The
GitHub Actions workflow runs it on Linux, where it passes. The development machine (Windows) has only a 32-bit
MinGW compiler, so the race detector cannot run there; run the plain `go test ./...` locally. The
concurrency tests, including 200 simultaneous reservation attempts for 10 units, 200
simultaneous payment attempts for one order, and 100 simultaneous `POST /orders/{id}/pay` calls
for one order (distinct keys and identical keys), run without it.

## Module path

The Go module path is `github.com/RajK158/ruptiq`, matching the GitHub repository.

## Project layout

```
cmd/ruptiq-api/                          Ruptiq Core: program entry point
cmd/ruptiq-run/                          Ruptiq Core: load test command (flags, report, JSON results)
cmd/ruptiq-compare/                      Ruptiq Core: run comparison command (flags, files, exit codes)
cmd/ruptiq-telemetry/                    Ruptiq Core: telemetry snapshot command (flags, JSON file)
cmd/ruptiq-experiment/                   Ruptiq Core: experiment command (flags, exit codes, JSON file)
cmd/ruptiq-analyze/                      Ruptiq Core: offline analysis command (flags, files, exit codes)
cmd/ruptiq-investigate/                  Ruptiq Core: offline investigation command (flags, files, exit codes)
cmd/ruptiq-validate-fix/                 Ruptiq Core: before/after analysis comparison (flags, files, exit codes)
internal/api/                            Ruptiq Core: HTTP server, routes, and handlers
internal/workload/                       Ruptiq Core: workload spec, validation, results, pass/fail
  k6/                                      runs workloads with the k6 program; embedded k6 script
internal/comparison/                     Ruptiq Core: reads and validates saved runs, compares them,
                                         console output
internal/telemetry/                      Ruptiq Core: TelemetryReader boundary, snapshot model, the
                                         Prometheus HTTP reader, console summary
internal/experiment/                     Ruptiq Core: experiment coordinator (workload plus periodic
                                         telemetry sampling), report model, console summary
internal/analysis/                       Ruptiq Core: reads and validates saved experiment reports,
                                         per-window rates, 5xx shares, histogram p95 estimates,
                                         console summary
internal/investigation/                  Ruptiq Core: reads and validates saved analyses, compares
                                         earlier and later windows, ranked findings with evidence
                                         references, console summary
internal/fixvalidation/                  Ruptiq Core: compares a baseline analysis with a candidate
                                         analysis; server throughput, 5xx ratio, and median window
                                         p95, with evidence pointers. Not a cause and not a
                                         significance test
demo/regression-lab/                     Validation fixture (NOT Ruptiq Core, not checkout): loopback
                                         HTTP target with real CPU work and Prometheus metrics
demo/checkout/order-service/             Demo target (NOT Ruptiq Core): Order Service
  main.go                                  entry point, configuration, graceful shutdown
  internal/httpapi/                        HTTP routing, handlers, JSON errors
  internal/order/                          order model, validation, ID generation, in-memory store,
                                           reservation and payment use cases, test price catalog,
                                           and the inventory and payment interfaces they need
  internal/inventoryclient/                HTTP/JSON client for the Inventory Service
  internal/paymentclient/                  HTTP/JSON client for the Payment Service
demo/checkout/inventory-service/         Demo target (NOT Ruptiq Core): Inventory Service
  main.go                                  entry point, configuration, graceful shutdown
  internal/httpapi/                        HTTP routing, handlers, JSON errors, request logging,
                                           Prometheus metrics
  internal/inventory/                      stock and reservation rules, seed data, in-memory store
demo/checkout/payment-service/           Demo target (NOT Ruptiq Core): Payment Service
  main.go                                  entry point, configuration, graceful shutdown
  internal/httpapi/                        HTTP routing, handlers, JSON errors, request logging
  internal/payment/                        payment rules, idempotency, in-memory store
demo/checkout/Dockerfile                 One multi-stage image definition for the three services
compose.yaml                             Runs the three checkout services together (Docker Compose)
scripts/checkout-smoke-test.sh           End-to-end checkout test over HTTP (used by CI)
.dockerignore                            Allow-list for Docker build contexts
docs/                                    Design documents
```
