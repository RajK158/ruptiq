# Ruptiq Architecture

> **Status: mostly planned design.** Implemented today: the Ruptiq Core health-check server, a
> first `WorkloadRunner` (one HTTP GET workload run with k6, see "WorkloadRunner: k6 adapter"
> below), offline comparison of two saved runs (see "Run comparison"), and three checkout demo
> services (Order, Inventory, and Payment), of which the Inventory Service exposes Prometheus
> metrics (see "Inventory Service telemetry"), and a first `TelemetryReader` that reads one
> snapshot from a Prometheus endpoint (see "TelemetryReader: Prometheus reader"), and an
> experiment coordinator that samples that reader periodically while the k6 workload runs (see
> "Experiment: workload with telemetry sampling"), and offline signal extraction from a saved
> experiment report (see "Signal extraction: analysing a saved experiment"), and a first,
> deterministic `Investigator` that ranks evidence-backed findings from a saved analysis (see
> "Investigator: evidence-driven findings"). It reports observations and hypotheses, not root
> causes. A loopback-only regression lab (`demo/regression-lab/`) is a validation fixture that
> changes CPU work mid-run so the existing pipeline can be checked against a known switch; it is
> not a checkout service and not a root-cause engine. `ruptiq-validate-fix` compares one saved
> baseline analysis with one saved candidate analysis and reports observed server-side
> differences; that is not proof of a cause or of a repeatable fix. `RunStore` does not exist in code yet. This
> document records the intended direction so early code does not paint us into a corner.

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

A first version exists (constant load only, no ramps); see "WorkloadRunner: k6 adapter" below.

### TelemetryReader

Responsible for reading the target's own signals during a run, such as CPU, memory, queue depth, or
error counts. This is how Ruptiq sees *inside* the target. Each kind of target can expose telemetry
differently, so this is a main place where adapters plug in.

A first version exists: it reads one snapshot from any Prometheus text-format endpoint, such as
the Inventory Service's `/metrics` (see "TelemetryReader: Prometheus reader" below). The
experiment coordinator calls it periodically while a workload runs (see "Experiment: workload with
telemetry sampling").

### Investigator

Responsible for analyzing the results of a run (client-side results plus target telemetry) to find
where the system broke and the likely bottleneck. It works on recorded data and should not drive
load or talk to the target directly, which keeps it easy to test with saved runs.

Its input is signal extraction (package `internal/analysis`), which turns a saved experiment's
cumulative metrics into per-window rates, error shares, and latency estimates, with references to
the samples they came from (see "Signal extraction: analysing a saved experiment"). Extraction
describes measurements; deciding what they mean is the Investigator's job.

A first version exists (package `internal/investigation`, see "Investigator: evidence-driven
findings"). It compares earlier and later windows of one experiment and reports ranked
observations of degradation, each with evidence references, limitations, and a validation
experiment, or abstains. Locating "the likely bottleneck" needs measurements Ruptiq does not
collect yet (resources such as CPU, or dependencies), so it does not name causes.

### RunStore

Responsible for saving and loading runs: configuration, results, and telemetry. It lets us compare
runs over time and re-run an investigation on old data. The storage technology is undecided.

## How the pieces work together (planned)

1. A run is configured against a target.
2. `WorkloadRunner` applies load while `TelemetryReader` collects telemetry. (Implemented by the
   experiment coordinator for one workload and one Prometheus endpoint.)
3. The results are saved through `RunStore`. (Today: one JSON report file per experiment.)
4. `Investigator` reads the saved run and produces findings. (Today: `ruptiq-analyze` extracts
   signals from a saved experiment report, and `ruptiq-investigate` ranks the changes between its
   earlier and later windows as observations and hypotheses to test. `ruptiq-diagnose` runs those
   stages, in order, for one experiment. It does not add a detection rule.)

## WorkloadRunner: k6 adapter

The first real Core capability: run one HTTP GET workload with [k6](https://k6.io) as an external
process and report its client-side results. It does not import or know about the checkout demo.

```
cmd/ruptiq-run/          CLI: flags, validation, timeout and Ctrl+C, report, JSON results file
internal/workload/       Spec (URL, VUs, duration) and Validate; Result and Metrics; the Runner
                         interface; Evaluate (pass/fail); Report (the JSON file); URL redaction
internal/workload/k6/    Runner: implements workload.Runner by running the k6 program;
                         scripts/http_get.js (embedded); parser for k6's summary
```

`workload.Runner` is one method, `Run(ctx, spec) (Result, error)`, defined with its first
implementation rather than up front. A `Result` holds only measured values; a run that could not
finish, or whose results cannot be trusted, is an error, never a partial `Result`.

### How a run works

1. `Spec.Validate` checks everything before any process starts: an absolute `http`/`https` URL
   with a host, valid port, no fragment, no spaces or control characters, at most 2048 characters;
   1 to 1000 VUs; 1 s to 1 h in whole seconds.
2. `exec.LookPath` finds k6. Not found maps to `k6.ErrNotInstalled` with an install hint.
3. A fresh `os.MkdirTemp` directory receives the embedded script; it is removed when `Run`
   returns, together with k6's summary file.
4. k6 is started with `exec.CommandContext`, without a shell, and every value as a separate
   argument:
   `run --quiet --no-color --no-usage-report --summary-mode compact --new-machine-readable-summary
   --summary-export <dir>/summary.json --include-system-env-vars=true --vus N --duration Ns
   <dir>/http_get.js`.
   The target URL goes in the `RUPTIQ_TARGET_URL` environment variable, so it is not in the
   process list. Inherited `K6_*` variables are removed: k6 reads options from them, and for
   example `K6_SUMMARY_MODE=disabled` silently stops k6 from writing the summary file.
5. k6's stdout and stderr go to one buffer that keeps the last 4 KiB (a failing run prints a
   warning per request: 8,162 lines in a 2-second test against a closed port). Once output has
   been dropped, the first kept line is dropped as well, because it starts at an arbitrary byte,
   possibly inside a URL. The rest is passed through `workload.RedactText` before it is used in
   an error (see "Redaction" below).
6. When the context ends (Ruptiq's timeout or Ctrl+C), k6 is killed; `WaitDelay` (5 s) bounds the
   wait for its output pipes. The error wraps `context.DeadlineExceeded` or `context.Canceled`.
7. A nonzero k6 exit becomes `*k6.ExitError` with the status and output tail (for example `107`
   for a script error). On exit status 0, the summary file is read (at most 8 MiB) and parsed.

### Reading k6's results

Ruptiq reads k6's machine-readable summary (format `1.x`, from `--new-machine-readable-summary`),
not the older `--summary-export` format: the newer one is versioned, includes the measured test
duration, and reports failures as plain `matches`/`total` counts. The parser requires, and never
defaults:

- `config.duration` > 0 (seconds), which is the measured run time;
- `http_reqs` (counter) `count`: a whole number. Zero, or no HTTP metrics at all (k6 omits them
  when no request was sent), is `k6.ErrNoRequests`;
- `http_req_failed` (rate) `matches`, `total`, `rate`, consistent with each other;
- `http_req_duration` (trend, time) `avg`, `min`, `med`, `p90`, `p95`, `max` in milliseconds,
  non-negative.

Missing values, JSON `null`, wrong types, non-integer counts, duplicate metrics, and unknown format
versions are `k6.ErrSummary`. Requests per second is the only derived value
(`count / config.duration`), because this format reports no counter rates.

### Judging the run

k6 exits 0 even when every request fails (seen with a closed port: 4,078 requests, all failed,
exit 0), so its exit status is not a verdict. `workload.Evaluate` passes a run only if it made at
least one request and its failure rate is at most the allowed maximum (default 0). The CLI exits
`0` passed, `1` failed, `2` invalid flags, `3` the run could not complete.

### Testing

No test needs k6, Docker, or a network. The k6 package's tests run the test binary itself as a
fake k6 (`TestMain` switches on an environment variable), which records its arguments,
environment, script, and working directory, and then writes a summary, exits with an error, writes
nothing, writes malformed JSON, floods output, echoes the URL, or sleeps. Tests cover argument
construction, environment filtering, success, every error type, output bounding, credential
redaction (user information, `api_key`, `token`, and several query parameters, in k6's real
output format and at every possible output truncation point), a missing executable,
cancellation, timeout, and removal of the temporary directory
after every outcome. The parser is tested against unedited k6 v2.3.0 summaries (healthy, all
requests failed, no requests) and 23 corrupted or invalid variants. The CLI is tested with a fake
`workload.Runner`: flag validation, defaults, report text, exit codes, the JSON file, and that a
failed run replaces an earlier successful results file.

The real k6 v2.3.0 was also run on the development machine through the CLI (a portable copy, not
installed): against the Inventory Service's read-only `GET /inventory/LAPTOP-001` (18,200
requests, 0 failed, exit `0`, stock unchanged), a closed port (all failed, exit `1`), a `404` path
(all failed, exit `1`, with `K6_SUMMARY_MODE=disabled` set to confirm the filter), a missing k6
(exit `3`), and a listener that never answers with a 4-second timeout (k6 stopped at 4 s, exit
`3`, no k6 process or temporary directory left).

### Redaction

The complete target URL exists in exactly two places: the `Spec`, and k6's `RUPTIQ_TARGET_URL`
environment variable (never k6's command line). Everything displayed or written uses a redacted
form:

- `workload.RedactURL` replaces the user information (user name and password) and the **whole**
  query string with `xxxxx`. Redacting the entire query, instead of a list of "sensitive"
  parameter names, cannot miss a parameter. It is used for the CLI's progress line, the report,
  and `Result.TargetURL`; the CLI sets the reported URL from the spec itself, not from the runner.
- `workload.RedactText` cleans free text (k6's output, error messages): it replaces the target's
  user information and query string verbatim and in `\"`-escaped form, then the user information
  and query of every `http(s)://` URL in the text. The second step is needed because k6 re-prints
  URLs: for a failed request it logs `http://alice:***@host/p?api_key=...`, hiding only the
  password. The CLI applies it to every run error before printing it or saving it.

Not covered: secrets in the URL path, and the process environment of the running k6.

### Limitations

Constant load from one local k6 process, GET only, one URL, no headers or bodies, no ramps. Results
are client-side only and are not stored.

## Run comparison

`ruptiq-compare` compares two saved run reports (the `workload.Report` JSON that
`ruptiq-run -out` writes): a baseline and a candidate. It is offline and descriptive: it reads two
files, never runs a workload or contacts a target, and makes no pass/fail or significance claim.

```
cmd/ruptiq-compare/      CLI: flags, reading files (1 MiB limit), exit codes, atomic JSON output
internal/comparison/
  report.go              DecodeReport: strict decoding and validation of one saved report
  compare.go             Compare: comparability check, per-metric changes, caveats; JSON model
  render.go              WriteText: the console table
```

The package does no file I/O: `DecodeReport` takes bytes, `WriteText` takes an `io.Writer`.
It reads the existing report model rather than defining a second one, and does not change it.

### Reading a saved report

`workload.Report` decodes missing numbers as zero, which would turn an incomplete file into
invented measurements. `DecodeReport` therefore decodes into a mirror of the report with pointer
fields and rejects unknown fields and trailing data, then requires:

- `schema_version` 1 (`ErrUnsupportedSchema` otherwise);
- a completed run: a report holding `error` is `ErrIncompleteRun`; one with both `run` and `error`
  is invalid;
- every field present and not `null`; every number finite and non-negative; at least one request;
  whole-second configured duration within the runner's limits; `finished_at` not before
  `started_at`; an absolute http(s) target URL;
- consistency: `failed_requests <= http_requests`; `failure_rate` equal to failed ÷ total and
  `requests_per_second` equal to total ÷ measured duration (relative tolerance 1e-9, enough for
  floating-point rounding only); `min <= p50 <= p90 <= p95 <= max` and `min <= avg <= max`; and
  `verdict.passed` equal to what `workload.Evaluate` gives for the metrics and the stored
  `max_failure_rate`.

All problems in a report are listed together (`ErrInvalidReport`). A completed run whose verdict is
failed is valid input.

### Comparability

`Compare` returns `*IncomparableError`, listing every difference, unless both runs have the same
stored target URL, virtual users, and configured duration. Stored URLs are compared exactly; since
`ruptiq-run` stores them redacted (user information and query replaced by `xxxxx`), two runs that
used different query strings or credentials are indistinguishable. This is reported as a caveat on
every comparison whose URL is redacted; it cannot be checked.

Passing this check does not make the runs equivalent experiments. The target's build, data, and
host, the load generator's machine, and the network are not recorded, so they cannot be checked.
A caveat says so on every comparison.

### Metrics and calculations

| Key | Unit | Preference | Change shown as |
| --- | --- | --- | --- |
| `latency_avg_ms`, `latency_p50_ms`, `latency_p95_ms` | ms | lower is better | absolute and % |
| `requests_per_second` | requests/s | higher is generally better | absolute and % |
| `failure_rate` | ratio (0–1) | lower is better | percentage points |
| `failed_requests`, `http_requests` | requests | not assessed | absolute and % |

- `change = candidate - baseline`.
- `change_percent = (candidate - baseline) × 100 ÷ baseline`, only when the baseline is not 0;
  otherwise `null` with the note `baseline is 0` (including 0 to 0). Multiplying before dividing
  keeps results exact for exactly representable inputs.
- For the failure rate, `change_percentage_points = candidate × 100 - baseline × 100`, and no
  relative change is reported.
- `direction` is `increase`, `decrease`, or `unchanged` (exact equality). `assessment` follows the
  preference: `better`/`worse` for latency and failure rate, `generally_better`/`generally_worse`
  for throughput, `unchanged`, or `not_assessed` for counts.

Throughput is only "generally" better because the load model is closed: each virtual user sends
its next request when the previous one ends, so throughput rises whenever requests end sooner,
including when they fail fast. When throughput and failure rate rise together, a caveat says so.

### Caveats

Always: each side is a single run (no variation or significance measured), and only URL, virtual
users, and duration were checked. When they apply: redacted URL, different runner or runner
version, candidate started before baseline, nonzero failure rate (latency includes failed
requests, which k6 may time as 0 ms), throughput rising with failures, different pass criteria,
and each run that failed its pass criteria (with its recorded reasons).

### Output

The console table shows baseline, candidate, change, change %, and assessment per metric, then
the caveats. The optional JSON file is a `comparison.Report` (`schema_version` 1) with either
`comparison` or `error` (plus `mismatches` when the runs were not comparable), written through a
temporary file and rename. It is written for every failed comparison too, so an earlier result is
never left looking current, and `-out` may not name an input file.

### Testing

No k6 or Docker is needed. Deterministic fixtures in `internal/comparison/testdata` use exactly
representable values, so the expected changes are asserted exactly (for example `+0.5 ms` and
`+25%`, `-400 requests/s` and `-20%`, `+1 pp`, and `n/a` from a zero baseline). One fixture is an
unedited real `ruptiq-run` report. Tests cover improvement, regression, throughput up and down,
increased failure rate, zero baselines (including 0 to 0), different URLs (including a difference
hidden by redaction), VUs, and durations, 35 missing, invalid, or inconsistent fields and 6
malformed files (empty, truncated, trailing data, oversized, and others), unsupported
schema versions, incomplete runs, completed but failed runs, every caveat, the exact console
output, the JSON fields, and the CLI's exit codes and output file.

### Limitations

One run per side; no repetition, aggregation, confidence intervals, or significance testing. Only
the saved summary numbers are compared (no per-request data or time series, no target
telemetry). No diagnosis of causes. Redacted URLs limit what the comparability check can prove.

## TelemetryReader: Prometheus reader

```
internal/telemetry/      the Reader interface; Snapshot, Family, Series, and the JSON Report;
                         PrometheusReader (HTTP fetch, limits, parsing, redacted errors);
                         WriteSummary (bounded console summary)
cmd/ruptiq-telemetry/    flags, Ctrl+C, summary output, atomic JSON file, exit codes
```

The interface is what the one implementation needs, nothing more:

```go
type Reader interface {
	Read(ctx context.Context) (Snapshot, error)
}
```

`Read` returns a whole `Snapshot` or an error, never a partial snapshot. The endpoint and limits
are configuration of the implementation (`PrometheusConfig`), not of the interface, so a reader for
another kind of target (a Prometheus server's query API, cloud monitoring) can fit the same shape.
Repeated sampling during a run will call `Read` on a schedule; that is not part of this boundary
yet.

### The snapshot model

A `Snapshot` holds the redacted endpoint, `ScrapedAt` (when the request was sent, UTC), how long
the request took, and the metric families sorted by name. Each `Family` keeps the target's own name,
help text, and type (`counter`, `gauge`, `histogram`, `summary`, or `untyped`). Each `Series` keeps
its labels as a map and exactly one of: a value (counters, gauges, untyped), a histogram (count,
sum, and buckets with upper bound and cumulative count, including `+Inf`), or a summary (count,
sum, quantiles). A timestamp is kept only if the target attached one to the sample.

Nothing is renamed, derived, or assumed: Core does not know which metrics a target exposes, so no
names such as `inventory_*` appear in Core code. Values that JSON numbers cannot represent (`NaN`,
`+Inf`, `-Inf`, all legal in Prometheus) are written as strings by the `Float` type and read back by
it.

### How a read works

1. `NewPrometheusReader` validates the URL (`http`/`https`, a host, a valid port, no fragment, no
   spaces, at most 2048 characters), the timeout, and the size limit, without contacting the
   endpoint.
2. `Read` derives a context with the timeout (default 10 s) from the caller's, so both the timeout
   and the caller's cancellation (Ctrl+C in the CLI) stop the request and the body read.
3. The request asks for the text format (`Accept: text/plain;version=0.0.4`). User information in
   the URL becomes HTTP basic authentication, as Go's client does by default.
4. The status must be `200`. Otherwise the error carries the status and the first 200 bytes of the
   body, on one line.
5. `Content-Type` must be `text/plain` (or absent). HTML, JSON, OpenMetrics, and protobuf are
   rejected instead of being fed to the wrong parser.
6. The body is read through a limit (default 16 MiB, applied after Go's transparent gzip
   decompression); one byte over is an error.
7. The body is parsed with `expfmt.TextParser` from `github.com/prometheus/common`, the parser
   Prometheus's own libraries use. An empty body, a body with no samples, and parse errors (with
   their line number) are errors.
8. Classic histograms are checked: whole-number counts, no duplicate bounds, cumulative counts
   that never decrease once buckets are sorted by bound, and a `+Inf` bucket, when present, equal
   to the count. Anything else is reported as invalid rather than stored. Known gaps: a histogram
   without a `+Inf` bucket is accepted without comparing its buckets to the count, and the text
   parser itself drops a `le="NaN"` bucket without an error.

Errors wrap one of five sentinels for `errors.Is`: `ErrInvalidEndpoint`, `ErrFetch` (no response:
unreachable, timed out, cancelled; it also wraps `context.DeadlineExceeded` or `context.Canceled`
and the network error), `ErrHTTPStatus`, `ErrResponseTooLarge`, and `ErrInvalidMetrics`.

### Redaction

The endpoint is shown and stored through `workload.RedactURL` (user information and the whole
query string become `xxxxx`), the same rule `ruptiq-run` uses. Error text is redacted twice: Go's
`*url.Error`, which repeats the URL with only the password masked, is unwrapped and replaced by
the redacted endpoint, and every message (including echoed response bodies) also goes through
`workload.RedactText`. Unwrapping was checked to be redundant with the second layer, so either
alone hides the secrets. The redaction helpers live in package `workload` because `ruptiq-run`
introduced them; they are not specific to workloads.

### Output

`WriteSummary` prints the endpoint, time, totals by type, and then at most 30 families and 8 series
per family. Names that are not plain Prometheus identifiers, and all label values, are quoted with
control characters escaped, and long ones are shortened, so a large or hostile endpoint cannot
flood or garble the console. The JSON file (`Report`, schema version 1) holds the full snapshot, or
the error if the read failed; it is written atomically through a temporary file.

### Testing

All tests use `httptest` servers and fixtures in `internal/telemetry/testdata`; none need the
Inventory Service, k6, Docker, or network access. `valid.prom` is a synthetic exposition (made-up
values) with every family type, labels needing escapes, a negative gauge, `NaN`, a summary, and a
sample timestamp. Tests cover:

- Exact names, help, types, labels, values, buckets, sums, counts, and quantiles from that fixture,
  and the `Accept` header sent.
- Golden files for the JSON report and the console summary, and a JSON round trip.
- Invalid bodies: empty, whitespace, comments only, syntax errors, bad values, decreasing buckets,
  a `+Inf` bucket that disagrees with the count, fractional counts, and wrong content types.
- Status errors with and without bodies (long ones cut), an unreachable port, a server that never
  answers and one that stalls mid-body (timeout), cancellation by the caller, the size limit at and
  one byte over, and a gzip body that is small compressed but too large decompressed.
- Credentials and query strings reaching the server but absent from the snapshot and from status,
  network, timeout, and configuration errors, including a server that echoes them back.
- The summary's limits with 40 families of 20 series and hostile names and label values.
- The CLI: summary and JSON, failures recorded in the file (replacing an older snapshot), redaction
  in stdout, stderr, and the file, usage errors, and cancellation.

The CLI was also run against the real Inventory Service before and after a reservation (see
README.md): it captured the seed stock, then the reservation in the counters, histogram, and stock
gauges.

### Limitations

One snapshot per call; sampling over time is the experiment coordinator's job. Only the Prometheus
text format; no OpenMetrics, protobuf, or native histograms. Basic authentication through the URL
only; no bearer tokens or TLS settings. No conversion to rates or percentiles, and no comparison of
snapshots.

## Experiment: workload with telemetry sampling

`internal/experiment` runs one workload while sampling telemetry, and `cmd/ruptiq-experiment` is
its command. It is the first place where `WorkloadRunner` and `TelemetryReader` work together. It
depends only on their interfaces (`workload.Runner`, `telemetry.Reader`); the command plugs in the
k6 runner and the Prometheus reader. It does not change or duplicate either: k6 is run exactly as
`ruptiq-run` runs it.

```text
internal/experiment/
  experiment.go     Config, defaults and validation, the Report model, statuses
  coordinator.go    Coordinator: baseline scrape, sampler goroutine, final scrape, judging
  render.go         WriteSummary (console)
cmd/ruptiq-experiment/
  main.go           flags, exit codes, atomic JSON file
```

### How an experiment runs

`Coordinator.Run(ctx, cfg)` validates the config (after defaults), then:

1. Applies the experiment timeout to `ctx`.
2. Makes one scrape **before** the workload.
3. Starts one sampler goroutine, which waits for a tick (every `Interval`), scrapes, and repeats.
   The same goroutine makes every scrape, so scrapes never overlap. A `time.Ticker` keeps at most
   one pending tick: after a scrape longer than the interval, the next scrape starts as soon as it
   finishes, and further ticks that fell due meanwhile are dropped.
4. Runs the workload on the calling goroutine. Telemetry failures and limits never stop it.
5. When the workload returns, closes a stop channel and waits for the sampler to exit. A scrape in
   progress finishes (bounded by the reader's timeout); a tick and stop arriving together resolve
   to stop. The ticker is stopped by the sampler on its way out.
6. Unless the context is done or the retention budget was reached, makes one scrape **after** the
   workload.
7. Builds the report and judges it.

`Run` returns only after the sampler goroutine has exited, so nothing outlives it. The collected
data has one owner at a time: the calling goroutine before step 3 and after step 5, the sampler in
between, with the goroutine start and the channel close as the hand-offs, so no lock is needed.

**Bounds.** `MaxSamples` (default 1000, at most 10000) bounds scrape attempts, successful or not.
The sampler stops one short of it, keeping the last attempt for the scrape after the workload, and
records `sample_limit_reached`. Validation rejects a config whose expected count
(`duration / interval + 2`) exceeds `MaxSamples`, but that count assumes the workload runs for
exactly its configured duration. k6 runs a little longer (start-up and shutdown), and a workload
that runs much longer than configured can still reach the limit; the telemetry is then `partial`.
Each response is bounded by the reader (16 MiB), and the console shows at most 5 scrape errors.

**Retention budget.** `MaxTelemetryBytes` (default 32 MiB, 1 KiB to 1 GiB) bounds the sample data
kept, and it is enforced as each scrape completes, not when the report is written. Each successful
scrape is measured as the compact JSON of the `Sample` it would add (families, labels, values,
buckets, and its timestamps), so the budget counts actual metric data, not a number of snapshots.
If the sample fits in what is left, it is kept and its size added to `retained_bytes`. If it does
not, including a single snapshot larger than the whole budget, it is recorded in `not_retained`
(sequence, phase, time, and size, without values), `retention_limit_reached` is set, the sampler
exits at its next check, and no scrape is made after the workload, because nothing more could be
kept. Samples already kept stay, and the workload is unaffected. The telemetry becomes `partial` (or
`none` if nothing was kept), with a reason naming the budget, the sample, and the bytes kept, so the
experiment can be at best `incomplete`.

The budget is an approximation of memory, not a guarantee. A sample's Go representation (maps,
slices, strings, pointers) is usually larger than its JSON. Beyond the kept samples, each scrape
transiently holds its response body (up to 16 MiB), the parsed snapshot, and one JSON copy for
measuring it, and writing the report makes one (indented, so larger) JSON copy of everything. The
`errors` and `not_retained` records are small and bounded by `MaxSamples`, and are not counted.

**Scrape accounting.** Every attempt gets the next sequence number and becomes exactly one sample,
one error, or one not-retained record, so `attempts = succeeded + failed` and
`succeeded = len(samples) + len(not_retained)`. A scrape that fails because the experiment's
context ended (cancellation or timeout) is not counted: that is not the endpoint's failure. Errors
are redacted again with the metrics URL before they are stored, in addition to the reader's own
redaction.

### Partial-failure policy

Telemetry completeness:

- `complete`: every attempt succeeded and was kept; at least one sample before, during, and after
  the workload; neither the sample limit nor the retention budget was reached.
- `partial`: at least one sample kept, but one of those conditions fails. `completeness_reasons`
  lists each one (for example "2 of 5 scrapes failed", "no sample while the workload was running",
  or "telemetry retention budget of 8192 bytes reached: sample #4 (2317 bytes) did not fit after
  6940 bytes were kept; it was not kept and sampling stopped").
- `none`: no sample kept.

Experiment status, decided in this order:

1. `timed_out` or `cancelled` if the experiment's context ended (by timeout or by the caller).
   Partial results of a stopped experiment are never judged.
2. `error` if the workload returned an error (no result).
3. `failed` if the workload's verdict failed (`workload.Evaluate`, as in `ruptiq-run`).
4. `incomplete` if the workload passed but the telemetry is not complete.
5. `passed` otherwise.

So a workload that passed with partial telemetry is never reported as passed, and telemetry
problems do not hide a workload failure. The report keeps the telemetry, with its completeness, in
every case.

### Report model

`Report` (`schema_version` 1) holds `status` and `reasons`, start and finish times, the config
(both URLs redacted with `workload.RedactURL`), the workload outcome (`workload.Result` or an
error, plus `workload.Verdict`), and the telemetry outcome. Each `Sample` stores the
`telemetry.Family` list from the snapshot with its sequence, phase, `scraped_at`, offset from the
start, and scrape duration; the endpoint is not repeated per sample. The workload result's URL is
replaced by the config's redacted one, so the report does not depend on a runner redacting it.
The telemetry outcome also records `retained_bytes`, `retention_limit_reached`, and `not_retained`,
and the config records `max_telemetry_bytes`. `samples`, `errors`, and `not_retained` are always
arrays, never `null`, so consumers need no special cases.

The samples are aligned to the run only by time. k6 measures requests on the client, the target
counts them when its handler finishes, and scrapes run on their own ticker; nothing ties a sample
to particular requests. "During" includes k6 starting and stopping.

### Testing

Coordinator tests use fake runners and readers, a fake clock, and a ticker the test drives by
hand, so they need no sleeps for ordering. The fake runner sends a tick and waits for the scrape it
causes before sending the next. They cover a passing experiment (phases, sequences, exact offsets,
values kept as read), early workload completion, sampling stopping when the workload returns, no
overlapping scrapes (ticks refused while a scrape is blocked), cancellation (an interrupted scrape
not counted, no scrape after), timeouts, workload errors, HTTP failures, an unavailable endpoint,
intermittent failures (sequence gaps), the sample limit, the retention budget (reached after two
samples by one that fits alone but not in what is left; and a baseline snapshot larger than the
whole budget, which leaves the report with no sample data), config validation, a golden JSON report
and summary, a JSON round trip, and redaction of URLs echoed in runner and reader errors. Every
coordinator test also checks the accounting: each sequence number appears exactly once, the counts
agree, and `retained_bytes` equals the JSON size of the kept samples. One test uses the real clock
and ticker with a slow reader. Mutating the code to skip waiting for the sampler, to count
interrupted scrapes, to ignore the sample limit, or to ignore the retention budget makes tests fail
(the last by the budget test waiting forever for sampling to stop, caught by the test timeout).

Command tests use a fake runner and the real Prometheus reader against `httptest` servers: every
exit code, cancellation and timeout, a failed run replacing an older report, a report that cannot
be written, redaction in stdout, stderr, and the file, usage errors, the k6 path, and the retention
budget reached with real scrapes (exit 4, no scrapes after it).

A real experiment against the Inventory Service with the portable k6 v2.3.0 is in README.md: 7
samples, and the service's request counter rising in each, ending at the same number k6 reported.

### Limitations

One HTTP GET workload and one Prometheus endpoint per experiment. Sampling is periodic, so short
events can fall between samples, and the scrapes themselves add a little load. Timestamps come from
Ruptiq's machine. Values are stored as exposed; rates, differences, percentile estimates, and
counter-reset detection are left to signal extraction, which reads the saved report afterwards.
The report is held in memory until the end and written to one file; there is no run
history, and `ruptiq-compare` does not read experiment reports. The retention budget approximates
memory use rather than capping it, and once it is reached nothing more is sampled, even if later
snapshots would be smaller; no attempt is made to keep, for example, the scrape after the workload
by dropping an earlier one.

## Signal extraction: analysing a saved experiment

Package `internal/analysis` and the `ruptiq-analyze` command read one saved experiment report
and calculate performance signals from its telemetry samples. This is the first step of
evidence analysis, kept separate from the `Investigator`: it turns raw, cumulative
measurements into numbers with stated uncertainty and exact references to their source, and
draws no conclusions. It works offline on the file only and imports neither the checkout demo nor
any target: the metric family names and the method, route, and status label keys are
configuration (`analysis.Config`), with defaults for the Inventory Service. It depends on
`internal/experiment` (the report model), `internal/telemetry` (the snapshot model), and
`internal/workload` (client metrics and URL redaction).

### Reading a saved report

`DecodeExperiment` first reads only `schema_version`, so a newer report is reported as
unsupported rather than as having unknown fields. It then decodes strictly (unknown fields and
trailing data are errors) into `experiment.Report`, and separately checks that every required
field is present and not `null`, which the typed decode cannot tell from a zero value. Finally it
checks the report's meaning and collects every problem (at most 10 quoted):

- status is `passed`, `failed`, or `incomplete`; `error`, `cancelled`, and `timed_out` are
  refused (`ErrNotAnalyzable`), because their workload did not complete;
- the status agrees with the workload verdict and the telemetry completeness, and the
  completeness agrees with the samples, errors, and not-retained records (recalculated);
- counts agree (`attempts = succeeded + failed`, `succeeded = samples + not_retained`,
  `failed = errors`), and the sequence numbers are exactly 1 to `attempts`, each once, in order
  within each list, in time order, with the baseline first and the final scrape last;
- every timestamp lies within the experiment, each `offset_seconds` agrees with its timestamp to
  within 5 ms (which also catches a wall-clock change during the experiment), and sample
  timestamps strictly increase;
- families are unique per sample with known types; each series has the one value field its type
  needs and unique labels; counters are finite and non-negative; histogram bounds strictly
  increase, counts never decrease or exceed the count, and the `+Inf` bucket equals the count;
- the client metrics are finite and non-negative, with failed requests at most the total.

Anything else is rejected rather than repaired (`ErrInvalidReport`). Errors name fields and
positions but never quote the report's URLs.

### Windows

The analysis works on windows: each pair of consecutive retained samples. A window's length is
the difference between the two samples' `scraped_at` timestamps, not the configured interval.
Scrape attempts between the two samples (failed, or not retained) are listed in the window as
missed scrapes; the window is then longer, its rates are averages over the whole span, and a
`scrape_gap` warning is raised. With fewer than two samples there are no windows.

For each window and each series of the two configured families, the series is looked up by its
full label set in both samples:

- present in both, not decreased: its increase (later minus earlier) is used;
- present only in the later sample (`series_appeared`): its earlier value is unknown, and it is
  **not** treated as zero;
- present only in the earlier sample (`series_disappeared`);
- decreased (`counter_reset`, or `histogram_reset` for a count or bucket): the target probably
  restarted.

In the last three cases the route's signal for that window is unavailable, with the reason;
nothing is partly counted. A family missing from a sample, or of the wrong type, makes that signal
unavailable in the windows that need the sample. Series are grouped into routes by the method and
route labels (either can be disabled), so a route's signal combines its statuses.

### Signals

- **Requests**: the sum of the route's counter increases, `per_second` = that / window length,
  and a breakdown by status label value.
- **5xx share**: increases of series whose status is 500-599, over the route's total. 4xx are
  counted separately as client errors. The share is unavailable when there were no requests, no
  status label is configured, or any series has a missing or non-numeric status.
- **Interval histogram**: for each series, later cumulative buckets minus earlier ones, after
  checking that the bounds are identical and no bucket or count decreased; the result must still
  be cumulative and not exceed the count's increase. The route's series are added together, which
  needs identical bounds.
- **Mean latency**: the increase of the histogram sum divided by the increase of its count; exact
  apart from rounding. Unavailable with no requests or a non-finite or negative sum increase.
- **p95**: estimated from the interval histogram like PromQL's `histogram_quantile`: find the
  first bucket whose cumulative count reaches rank 0.95 × total, and interpolate linearly between
  its lower bound (the previous bucket's bound, or 0) and its upper bound. It is marked
  `estimate: true` and comes with the bucket's bounds. It is unavailable with fewer than
  `MinObservations` (default 20) requests, in the `+Inf` bucket, or above the largest bound of a
  histogram without `+Inf`.
- **Totals** add, per route, the windows in which the signal was available: request increases
  over the summed length of those windows, and interval histograms (if every window has the same
  bounds) for a whole-run p95 estimate. They list the windows used and excluded.

Counter values are float64. Above 2^53 whole numbers are no longer exact, so a `precision`
warning is raised; a sum or rate that is not finite makes the signal unavailable with an
`overflow` warning, so the JSON never contains NaN or infinities. Histogram counts are unsigned
integers and are compared before subtracting.

k6's client-side results are copied unchanged into `client`. They are measured elsewhere (on the
client, including network time) over a different time span, so they are not combined with or
checked against the server-side signals.

### Report model

`analysis.Report` (schema version 1) has the input path and either `analysis` or `error`, so a
failed analysis replaces an older file. `Analysis` has the experiment's identity (status,
timestamps, redacted URLs, telemetry counts), the config used, `client`, `totals`, `windows`,
`warnings`, and `limitations`. Every unavailable value is replaced by a reason string instead of
a zero or null number. Evidence references name the metric and the exact label sets used; with the
window's `from` and `to` (sequence, phase, timestamp, offset), each number can be traced back to
two samples of the experiment report. Lists are always arrays, never `null`, and output is
deterministic (routes, series, and statuses are sorted).

### Redaction

URLs from the report are redacted again with `workload.RedactURL`, and label values, reasons, and
scrape errors with `workload.RedactText`, so a credential in a hand-edited report (or a URL in a
label value) never reaches the summary or the file. A report written by `ruptiq-experiment` is
already redacted. Decode errors that would quote a rejected value (a malformed timestamp or
number) are replaced by a generic message, and unknown enumeration values are redacted with
`workload.RedactText` before being shortened and quoted.

### Testing

Tests build experiment reports from a compact fixture builder (samples, failed scrapes, and
unretained scrapes at given offsets, with counter and histogram series) and always round-trip them
through `DecodeExperiment`, so the validation is exercised everywhere. They cover counter
increases and rates over unequal intervals, multiple routes and statuses (4xx not counted as
server errors), zero traffic, series appearing and disappearing, counter resets, failed and
unretained scrapes, interval histograms combined across statuses with a hand-calculated p95,
quantile edge cases (first bucket, rank on a bound, `+Inf` bucket, too few observations, no
buckets), changed bucket bounds, histogram resets, non-cumulative increases, wrong family types,
missing families, empty and single-sample telemetry, numeric edge cases (above 2^53, an
overflowing rate, fractional counters, negative or NaN sums), configurable names for a different
service, redaction, determinism, and bounded console output. Decode tests cover every accepted
status, unsupported and malformed schemas, missing fields, and 38 kinds of inconsistent report.
A golden analysis and summary of a synthetic Inventory Service experiment
(`testdata/inventory_experiment.json`) record the full output, and the experiment package's own
saved report (written by the real coordinator) is analysed with a generic metric name. Mutating
the code to divide by a fixed one second, to count an appearing series from zero, or to accept a
decreasing counter makes tests fail.

Command tests cover the summary and JSON file, custom metric names, every bad-report class (each
writes an error report), a missing input, usage errors, an unwritable output, and redaction in
stdout, stderr, and the file. No test needs k6, Docker, or a running service.

### Limitations

Signals are averages over windows of about one sampling interval, so short events are invisible,
and a run with a few windows says little about variability; no significance is tested. p95 is
only as precise as the bucket bounds. Server-side counters include all clients' traffic and are
not reconciled with k6's counts. Latency combines a route's statuses. Windows at the start and
end of the run include time without full load, and on a freshly started service the first window
is usually unavailable because series appear at their first request. Only one request counter
and one duration histogram are interpreted. The whole report is held in memory (bounded by
`-max-input-bytes`, default 128 MiB). Signal extraction is not diagnosis: it does not rank
causes, explain changes, or compare experiments. The Investigator (below) interprets its output.

## Investigator: evidence-driven findings

Package `internal/investigation` and the `ruptiq-investigate` command read one saved analysis
(written by `ruptiq-analyze -out`) and report ranked findings. This is the first `Investigator`.
It is a fixed set of deterministic rules over the analysis' signals: no language model, no
generated text beyond templates, no statistical significance tests, and no confidence
percentages. It does not repeat signal extraction: it uses the analysis' per-window rates, 5xx
ratios, and p95 estimates as they are, through the `internal/analysis` types.

```
analysis.json --ReadAnalysis--> analysis.Analysis --Investigate--> Investigation --WriteSummary--> console
 (strict decode,                  (select windows, assess           (findings, assessments,     \--> investigation.json
  validation)                      each route and signal, rank)      windows, limitations)
```

### Reading a saved analysis

`DecodeAnalysis` reads the `schema_version` first (missing or non-integer: invalid; any value
other than `analysis.SchemaVersion`: unsupported), then decodes strictly (unknown fields and
trailing data rejected). A file that records a failed analysis is refused with
`ErrNotInvestigable`. Then every consistency rule that a later step relies on is checked, and every
problem collected (up to 10 listed): window indexes from 1, sample sequences increasing and
chained (each window starts at the sample the previous one ends at), known phases, positive window
lengths equal to their timestamps' difference, missed scrapes inside their window, exactly one of
each signal and its "unavailable" reason, finite and non-negative counts, rates equal to requests
divided by elapsed time, status counts adding up, 5xx ratios equal to their counts, cumulative
buckets, p95 estimates marked as estimates, within their bucket, and from at least the analysis'
minimum observations, evidence for the configured metrics, and no repeated routes. A file that
passes cannot make the engine divide by zero or compare values that contradict each other.

### Windows and sides

Only windows whose two samples have phase `during_workload` and that span no missed scrapes are
comparable; the others are listed in `windows.excluded` with a reason and a JSON pointer. For each
route and each of three signals (throughput, server error ratio, p95 latency), the comparable
windows in which the signal is available are its usable windows; the others become `gaps` with the
analysis' reason ("the route has no series in this window", a reset, changed buckets, a p95 that
could not be estimated, and so on). The earlier half of the usable windows is the baseline and the
later half the comparison; with an odd count the middle window is left out. Each signal has its
own sides, so a gap in one signal does not discard the others.

### Assessments

Each route and signal gets one `Assessment`, with status `material_change`,
`no_material_change`, `inconclusive`, or `insufficient_evidence`:

| Signal | Side value | Insufficient when | Material when | Inconclusive when |
| --- | --- | --- | --- | --- |
| throughput | median of window rates (requests/s) | fewer than 2 x `MinWindows` usable windows; baseline below `MinRequests` requests; zero baseline median | fall of at least `ThroughputDecrease`, and every comparison window below every baseline window | the fall reaches the threshold but the windows overlap |
| server error ratio | pooled 5xx / requests (ratio) | too few windows; either side below `MinRequests` | rise of at least `ErrorRatioIncrease` (absolute), at least `MinServerErrors` 5xx in the comparison, and (non-zero baseline) at least `ErrorRatioFactor` times the baseline | the rise reaches the threshold but there are too few 5xx or it is less than the factor |
| p95 latency | median of window p95 estimates (seconds) | too few windows; either side below `MinRequests` observations; zero baseline median | rise of at least `LatencyIncrease`, and the comparison's lowest bucket lower bound at or above the baseline's highest bucket upper bound | the rise reaches the threshold but the buckets overlap |

Medians keep one unusual window from deciding a rate or a p95; the error ratio is pooled because
5xx counts per window are small. The separation checks are the noise and resolution safeguards:
within one histogram bucket a p95 estimate is a linear interpolation, so a rise inside overlapping
buckets may not be real, and rates whose windows overlap could be noise. Throughput needs
`MinRequests` in the baseline only, so a collapse to almost no traffic is still detected. A zero
baseline is never divided by: rates and p95 abstain, and a 5xx rise from 0% has no relative change.
Each `Change` records the absolute and relative change and its `threshold_multiple` (the change
divided by its threshold, in the direction of degradation).

### Findings

A material change becomes an observation: `server_error_increase`, `latency_degradation`, or
`throughput_degradation`. When latency and throughput both degrade for one route over identical
sides, a `combined_degradation` finding is added, designated a **hypothesis**, which links the two
(`related`); with different sides they are not combined and each says so. A route with any
insufficient signal gets one `insufficient_evidence` finding (an **abstention**), listing each
signal's reason and gaps. Inconclusive assessments are not findings. Every finding carries:

- a stable ID (type plus the first 12 hex digits of a SHA-256 of method and route), type,
  designation (`observation`, `hypothesis`, or `abstention`), and `proven_root_cause: false`;
- the method and route, a one-line symptom, and the compared values with units (`requests/s`,
  `ratio`, `seconds`), the statistic used, the requests or observations, and for p95 the bucket
  range;
- the baseline and comparison windows, and evidence references: an RFC 6901 JSON pointer into the
  analysis file for every window used (for example `/analysis/windows/1/routes/0/latency/p95`),
  with its role, metric, and series labels; abstentions point at the analysis warnings about the
  route and at the route in excluded windows;
- an explanation (which rule fired, over which windows), contradictions found in the same data,
  limitations, untested hypotheses, and a concrete next validation experiment.

Contradictions are measurements that weaken a finding: a latency rise while the request rate
also rose by at least `ThroughputDecrease` (more load), the mean falling while p95 rose, a
throughput fall without a latency rise (the load generator may have sent less), a 5xx rise without
a latency rise, and comparison windows whose 5xx ratio did not rise.

### Avoiding false attribution

The analysis has request counts, status codes, and latency histograms. The engine therefore never
states a cause: CPU, locks, connection pools, dependencies, or databases appear only in
`possible_explanations`, each prefixed "Hypothesis (untested):" with the measurement that is
missing. Fixed limitations on every finding say that it is a change within one run, not a
regression, and that no significance test was made; error findings say a higher 5xx ratio does not
by itself mean lower capacity; latency findings give the bucket ranges and say client- and
server-side latency are not compared; combined findings say correlation does not show causation and
that with fixed VUs higher latency lowers the request rate by itself. The investigation's own
limitations repeat what is not measured.

### Ranking

Findings are sorted by: observations and hypotheses before abstentions; severity (`major` when the
threshold multiple is at least 2, else `moderate`); type (combined, server errors, latency,
throughput); larger threshold multiple; more windows, then more requests compared; then method,
route, and ID. The rule and the values are written into each finding's `ranking`. The order does
not depend on the order of routes in the analysis.

### Outcome

`changes_observed` if any finding is an observation; otherwise `no_material_change` if any signal
was compared; otherwise `insufficient_evidence` (the investigation abstains, also when the analysis
has no routes or windows). A `no_material_change` outcome says this run showed no material change,
not that there is none.

### Report model and command

`Report{schema_version, input, investigation | error}`; exactly one of `investigation` and `error`
is set. `Investigation` holds the source (experiment status, completeness, redacted URLs, VUs,
duration, interval, metrics), the config, the outcome and a one-sentence summary, the window
selection, the ranked findings, every assessment, and the limitations. The command's exit codes
are 0 (investigated, including abstention), 2 (usage), and 3 (bad input or write failure); a
failure is written to `-out` too, replacing any earlier file atomically. The console summary is
capped at 10 findings, 10 inconclusive signals, and 10 excluded windows, with long lines
shortened.

### Redaction

Source URLs go through `workload.RedactURL`; methods, routes, labels, reasons, and metric names
copied from the analysis go through `workload.RedactText`, control characters are replaced, and
length is capped. A recorded analysis error is redacted before it is quoted. Decode errors that
would quote a rejected value (a malformed timestamp or number) are replaced by a generic message.

### Testing

Engine tests build analyses from a compact builder (windows of given lengths and phases, routes
with request counts by status class, p95 estimates with bucket bounds, and unavailable signals)
and always round-trip them through `DecodeAnalysis`. They cover every finding type with
hand-calculated values (p95 0.8 ms to 2 ms is +150%, three times the threshold; 1000 to 600
requests/s is -40%, 1.33 times; 0% to 6% 5xx is six times), p95 rises inside one bucket
(inconclusive), noisy and unequal-length windows, zero baselines, error spikes without latency
increases, 4xx-only increases (no finding), non-robust error increases (factor, too few errors),
multiple routes and their ranking, determinism under shuffled route order, stable IDs, excluded
start, end, and gap windows with partial telemetry, counter resets with warning references,
changed buckets, too few observations, a p95 in the `+Inf` bucket, odd window counts, too few
windows, no routes or windows, contradictions, credential redaction, and a check that no output
claims a cause, significance, or confidence. Every evidence pointer in the latency and throughput
tests is resolved in the analysis file and checked against its value. Decode tests use the
analysis golden file and 27 kinds of inconsistency. Mutating the engine to skip the bucket
separation check or to compare windows that span the workload's start or end makes tests fail.

Command tests include an offline end-to-end run: the synthetic experiment fixture is analysed with
`analysis.ReadExperiment` and `analysis.Analyze` (checked to equal the analysis golden file),
saved as `ruptiq-analyze` saves it, and investigated. With the defaults every route abstains (two
comparable windows); with `-min-windows 1` GET shows a throughput degradation from 2976.2 to
2026.8 requests/s, and its evidence pointers are resolved to windows 2 and 3 of the saved analysis
(3000 and 3020 requests). Other command tests cover the JSON schema (decoded with unknown fields
disallowed), every bad-input class with the error report replacing an older file, usage errors,
an unwritable output, and redaction. No test needs k6, Docker, or a running service.

### Limitations

Only earlier and later windows of one experiment are compared, so short experiments abstain and
this command cannot show a change between two versions. Only degradation is looked for. The thresholds are
conservative rules of thumb, not calibrated to any service, and a gradual change that never
separates the halves is reported as inconclusive. There are no resource, dependency, or network
measurements, so causes are never identified, and client-side measurements are not used. The
engine has only been run on synthetic data in unit tests. One live run against
`demo/regression-lab/` (see below) used the same commands and default thresholds. Comparing a
baseline experiment with a later candidate is a separate step, `ruptiq-validate-fix` (see below),
and that step still does not identify a cause.

## Single-run diagnosis

`internal/diagnosis` and `ruptiq-diagnose` are the one-command path through the three stages
above. The package calls `experiment.Coordinator.Run`, then `analysis.Analyze` on the saved
experiment report, then `investigation.Investigate` on the saved analysis. It does not estimate
quantiles, compare windows, or rank findings itself. `ruptiq-validate-fix` is unchanged and
still compares two analysis files. `ruptiq-validate-runs` compares two saved diagnosis directories
by calling `fixvalidation.Compare`.

```
flags + runner + reader
        |
        v
experiment.Coordinator.Run --> experiment.json
        |
        |  only when the workload completed (passed, failed, incomplete)
        v
analysis.DecodeExperiment + analysis.Analyze --> analysis.json
        |
        v
investigation.DecodeAnalysis + investigation.Investigate --> investigation.json
        |
        v
manifest.json (outcome, stage statuses, schema versions, relative names, limitations)
```

The output directory is created if needed. Ownership is an exclusive
`.ruptiq-diagnose-reservation` file (`O_EXCL`), taken before the workload starts and removed
when the run finishes, fails, or is cancelled. A second process fails while that file exists
and does not delete a reservation because of its age. After the reservation is held, an output
directory that already contains one of the four report names is refused. Each report is renamed
into place from a temporary file in that directory. Artifact names
are fixed (`experiment.json`, `analysis.json`, `investigation.json`, `manifest.json`); a name
that would leave the directory is rejected. The manifest stores those names, not absolute paths,
and stores URLs only after `workload.RedactURL`. Stage errors pass through `workload.RedactText`.

`analysis.json` is an `analysis.Report` and `investigation.json` is an `investigation.Report`,
with `input` set to the previous artifact's file name, so the existing decoders accept them.
Evidence pointers in the investigation are JSON pointers into `analysis.json`.

Outcome `completed` means each stage that the experiment data allowed wrote its file. An
investigation outcome of `insufficient_evidence` is still `completed`: the investigator abstained.
Outcome `failed` means a stage failed or was skipped. Skipped analysis and investigation do not
get placeholder files. The experiment file is kept whenever the coordinator returned a report,
including when the run was cancelled or timed out. Those statuses are not analysed, matching
`analysis.DecodeExperiment`.

The command's exit codes are 0 (completed, including abstention), 2 (usage, invalid
configuration, or an output collision), and 3 (a stage failed or the workload did not complete).
`go run` reports a non-zero status as `exit status N` and then exits 1. A path given to `-k6` is
resolved against the current directory first, because the k6 adapter runs k6 in a temporary
directory. A bare executable name is left for `PATH`.

One live run used `demo/regression-lab` at `127.0.0.1:8090` with `-degrade-after 10s`,
`-baseline-iters 157135`, and `-degraded-iters 11233031`, then:

```
go run ./cmd/ruptiq-diagnose -url http://127.0.0.1:8090/work -metrics-url http://127.0.0.1:8090/metrics -vus 2 -duration 20s -interval 1s -k6 tmp/k6/k6-v2.3.0-windows-amd64/k6.exe -requests-metric regression_lab_http_requests_total -duration-metric regression_lab_http_request_duration_seconds -out-dir tmp/diagnosis-live-11a
```

The command exited 0. The experiment passed. k6 v2.3.0 recorded 33354 requests, 1667.0701975500676
requests/s, failure rate 0, and p95 1.9575 ms over 20.0075558 s. Telemetry was complete (22 of 22
scrapes). `regression_lab_degraded` stayed 0 with 157135 iterations through the sample at offset
10.011 s and was 1 with 11233031 iterations from the sample at offset 11.012 s. The investigator,
at its default thresholds, reported `changes_observed`: combined, latency (about 0.938 ms to about
24.216 ms), and throughput (2992.9/s to 135.9/s) findings for `GET /work`, plus an abstention for
`GET /healthz`. Windows 1 and 21 were excluded. Pointers `/analysis/windows/1/routes/1/latency/p95`,
`/analysis/windows/2/routes/1/latency/p95`, and `/analysis/windows/3/routes/1/latency/p95` resolved
in `analysis.json` to 0.000958008008008008 s, 0.0009505194218608852 s, and 0.0009376345788473716 s.
A second run with the same output directory exited 2 and did not change the four files. These
numbers are that one run. They are not a benchmark, a significance result, or a software fix.
Reports stayed under gitignored `tmp/`.

Tests use a fake `workload.Runner` and `telemetry.Reader`. They do not start k6, Docker, or an
HTTP server. They cover a completed run through all three stages, an abstention, missing telemetry,
a workload error, cancellation, timeout, invalid configuration, output collisions, atomic writes,
a failed later stage that leaves the experiment file valid, secret redaction, the manifest, schema
decoding through `analysis.DecodeExperiment` and `investigation.DecodeAnalysis`, and resolution of
the investigation's evidence pointers in the saved analysis.

## Comparison of two saved diagnosis runs

`internal/runvalidation` and `ruptiq-validate-runs` compare two directories produced by
`ruptiq-diagnose`. The package checks each manifest and analysis, then calls
`fixvalidation.Compare`. It does not reimplement throughput, 5xx ratios, histogram quantiles, or
ranking. It does not start k6 or modify either input directory. The user runs the two diagnoses
separately; this command does not run them and does not apply a code or deployment change.

```
before-dir/manifest.json + analysis.json + experiment.json
after-dir/manifest.json  + analysis.json + experiment.json
        |
        |  schema, artifact names, analysis stage succeeded,
        |  analysis.json matches analysis.Analyze(experiment.json)
        v
fixvalidation.Compare
        |
        v
out-dir/validation.json   existing fixvalidation.Report
out-dir/manifest.json     run-validation schema 1
```

An input is refused when its diagnosis schema is unsupported, its analysis stage did not succeed,
`analysis.json` is missing or is not the analysis of the saved experiment, the manifest names a
different artifact, or `.ruptiq-diagnose-reservation` is present. That reservation is not deleted.
An investigation outcome of `insufficient_evidence` is accepted. An experiment status of `failed`
or incomplete telemetry is accepted when the saved analysis is one `fixvalidation.Compare` can
judge; the status is kept on the manifest as a limitation. `fixvalidation.Compare` remains the
authority for `improvement_observed`, `regression_observed`, `mixed`, `no_material_change`,
`insufficient_evidence`, and `not_comparable`. The last two are successful comparisons.

Paths are resolved with `EvalSymlinks` before the directories are accepted, so a symlink or
junction cannot make the two inputs the same directory or put the output inside an input.
When the two analysis files have the same SHA-256, the outcome from `fixvalidation.Compare`
is kept and the manifest records that the files are one saved analysis, not independent
evidence of a change.

The output directory is owned by an exclusive `.ruptiq-validate-runs-reservation` file, created
with `O_EXCL` after the inputs have been accepted and removed when the command returns. A second
process fails while it exists. A reservation left by a crash is not deleted because of its age.
`validation.json` and `manifest.json` are renamed into place from temporary files, and an output
directory that already contains either name is refused. Stored labels are `before` and `after`.
Each input records the SHA-256 of the `analysis.json` bytes that were compared. Evidence pointers
keep the `side` and JSON pointer produced by fix-validation, so a pointer into the before analysis
is read from that directory's `analysis.json`.

Exit status is 0 when a comparison was written, 2 for invalid arguments or an output collision,
and 3 when an input cannot be read or a result cannot be written. Tests build diagnosis
directories from synthetic experiments and do not start k6.

## Guided before/after workflow

`internal/pairworkflow` and `ruptiq-pair` run the diagnosis and the run-validation comparison
as three explicit invocations: `before`, `after`, and `compare`. The process exits at the end
of each one. It does not apply a change, restart a service, or start the next stage.

```
-stage before  + runner + reader
        |
        v
diagnosis.Run --> pair-dir/before/   (experiment, analysis, investigation, manifest)
        |
        |  process exits; the developer changes the service outside Ruptiq
        v
-stage after   (same workload settings; rejected before k6 if they differ)
        |
        v
diagnosis.Run --> pair-dir/after/
        |
        |  process exits
        v
-stage compare
        |
        v
runvalidation.Run --> pair-dir/comparison/validation.json
                      pair-dir/comparison/manifest.json
pair-dir/pair-manifest.json records the workflow state
```

`before` and `after` call `diagnosis.Run`. `compare` calls `runvalidation.Run`, which calls
`fixvalidation.Compare`. This package does not estimate quantiles, rank findings, or duplicate
the comparison. The pair manifest stores a redacted configuration fingerprint and the SHA-256
of each original target and metrics URL. Those hashes are consistency fingerprints. They are
not a secure substitute for protecting a short or guessable secret. The raw user information
and query string are not stored, and a hash is not computed from the redacted URL. The after
stage compares that fingerprint, including virtual users, duration, scrape interval, sampling
limits, metric families, labels, and minimum histogram observations, and refuses to start a
workload when they differ. If the original URL hashes are missing, the after workload is
refused as well. Two saved diagnoses can still be compared: `compare` calls `runvalidation.Run`
when `comparison/` is empty. A manifest that is only behind a finished `comparison/` is updated
from those files and does not run the comparison again. That reuse is allowed only when
`validation.json` is a successful fix-validation report of the supported schema, its outcome
and comparability match `comparison/manifest.json`, and that manifest identifies
`before/analysis.json` and `after/analysis.json` with SHA-256 values of the current files.
A corrupt validation file, an error report, a disagreement, an unsupported schema, or hashes
from another pair are refused. Those comparison files are left in place and the comparison is
not recomputed. `fixvalidation.Compare` remains the authority for whether the two saved runs
are comparable and for the six outcomes.

The states `baseline_captured`, `candidate_captured`, and `comparison_completed` are written
only when the corresponding diagnosis or comparison files are complete. A failed diagnosis, a
partial directory, or a cancellation is `failed` or `interrupted`. The pair manifest is updated
after the stage reports are written. It is not a transaction across those files. Replacing it
writes a complete temporary file in the pair directory and renames that file onto
`pair-manifest.json`. The previous manifest is not deleted first, including on Windows. If the
replacement fails, the previous manifest stays, the temporary file is removed, and the pair
reservation stays until the invocation returns. On resume the stage directories are read
again; a manifest that claims a file which is missing or whose analysis hash differs is
rejected and is not repaired.

The pair directory is owned by an exclusive `.ruptiq-pair-reservation` file, created with
`O_EXCL` and removed when the invocation returns. A reservation left by a crash is not deleted
because of its age. Diagnosis and run validation keep their own reservations inside `before/`,
`after/`, and `comparison/`. Those files are not deleted by the pair workflow. Stage paths are
resolved with `Readlink` and `EvalSymlinks`, so a symlink or junction cannot point a stage at
another stage or outside the pair directory. Completed reports are not replaced.

`not_comparable` and `insufficient_evidence` are successful compare invocations. An
investigation abstention is a completed diagnosis stage. Neither one is an observed
improvement. One pair is not a cause and not a statistical significance test.

Tests use a fake runner and reader, or synthetic diagnosis directories. They do not start k6.

## Fix validation: before and after

`internal/fixvalidation` compares two saved analyses. The command is `ruptiq-validate-fix`. It is
offline and deterministic. It does not import the investigator's ranking rules and does not change
them. The command reuses `investigation.ReadAnalysis` so a file is accepted only when it is a
strict `ruptiq-analyze` report.

For each method and route present in either file it compares three server-side signals, using
only windows whose samples are both `during_workload` and which have no missed scrapes:

- Throughput is total completed requests divided by total elapsed time. A window of three seconds
  counts three times a window of one second. The mean of the per-window rates is not used.
- The 5xx ratio pools `server_errors_5xx` and completed requests. 4xx counts are reported beside
  it and do not enter the ratio. A side with no completed requests stores a null ratio and
  `value_unavailable`; the request count, 5xx count, and window evidence stay. A ratio of 0 is
  stored only when that side completed requests.
- Latency is the median of the eligible windows' p95 estimates. An even count uses the mean of
  the two central estimates. The value is labeled as that median, not as the experiment-wide p95.
  The union of the windows' bucket bounds is kept. Overlapping ranges stay `inconclusive`: they
  do not become an improvement, a regression, or `no_material_change`. If no other signal can be
  judged, the route and the overall outcome are `insufficient_evidence`. A judged signal on the
  same comparison is kept, and the summary names the inconclusive latency comparison. Histogram
  boundaries that differ between the files stay incompatible.

A route that is absent from one file stays absent. It is not filled with zeros. Fewer than two
eligible windows on a side, or fewer completed requests than the configured floor, is
`insufficient_evidence` for that signal. Other signals of the same route are still judged.

The experiments are `not_comparable` when VUs, configured duration, sampling interval, the
scheme, host, and path of the target URL, metric family names, label names, or the analysis
minimum observation count differ. User information and query strings are already redacted in a
saved analysis, so they are not compared. When either URL still has those parts, including the
redacted placeholder, the result warns that a URL match does not prove the original targets were
the same. Status,
telemetry completeness, and the metrics URL may differ; those are warnings, not a silent
mismatch. k6's client counters are copied onto each side and are not inputs.

Outcomes are `improvement_observed`, `regression_observed`, `mixed`, `no_material_change`,
`insufficient_evidence`, and `not_comparable`. Thresholds default to ±30% throughput, ±1
percentage point of 5xx ratio (and a factor of two when the better side is not zero, with at
least five 5xx responses on the worse side), and ±50% on the median window p95. Every material
row points at `/analysis/windows/{i}/routes/{j}/requests` or `.../latency/p95` in the file it
came from. The report asks for the pair to be repeated. One pair is an observation only.

The synthetic inventory analysis compared with itself yields `no_material_change`: GET
`/inventory/{sku}` pools to 6020 requests over 2.498s (2410.7 requests/s) in windows 2 and 3,
while POST `/reservations` has too few requests to judge. A unit fixture with 1000 requests/s
and a 20 ms median window p95, then 1400 requests/s and a 4 ms median in a non-overlapping
bucket, yields `improvement_observed` (+40% throughput, −80% median p95).

A live check on the regression lab used the same default thresholds and did not change them.
One lab process with `-degrade-after 0s` and fixed iteration counts (`-baseline-iters 157135`,
`-degraded-iters 11233031`) was the heavier run. After that process was stopped, a second
process on `127.0.0.1:8090` with `-degrade-after 1h` and the same counts was the lighter run.
Both used k6 v2.3.0, 2 VUs, 20s, and a 1s interval. The saved gauges matched the mode: the
heavier run was degraded at 11233031 iterations during the workload, and the lighter run stayed
at 0 and 157135. Eligible `GET /work` windows compared as 114.1 → 2374.1 requests/s (+1980.6%)
and median window p95 24.250 ms → 0.951 ms (−96.1%), bucket unions 10–100 ms and 0.500–1.000 ms,
5xx ratio 0% on both sides. The outcome was `improvement_observed`. A second pair, saved in
separate files and run lighter-then-heavier, was `regression_observed` (2561.8 → 132.8
requests/s, −94.8% throughput; 0.950 → 24.215 ms, +2448.4% median window p95; 5xx ratio 0% on
both sides). Its evidence pointers resolved. The exact commands and the split between expected
and observed results are in README.md. This is less CPU work in a fixture, not a source patch,
and two pairs are not a significance test. The numbers are this Windows machine's measurements,
not a benchmark. The 10–100 ms union came from noisy heavier windows,
including one p95 estimate in the 50–100 ms bucket; those bounds still did not overlap the
lighter bucket.

## Regression lab: a known mid-run change

`demo/regression-lab/` is a validation fixture, not Ruptiq Core and not the checkout demo. It
serves `GET /work`, `GET /healthz`, and `GET /metrics` on loopback. `GET /work` runs a tight
integer loop (FNV-1a mixing); the iteration count is the baseline until `-degrade-after` after
the first `/work`, then a larger count. Iteration flags and calibration are capped at 50 million
loops per request. Prometheus histograms observe the real handler duration.
The `GET /work` 200 series is registered at start. `regression_lab_degraded` is 0 then 1, so the
switch can be checked without trusting Ruptiq.

There is no HTTP control plane. Non-loopback listen addresses are rejected. Core packages do not
import it; it does not import Core. Commands to run `ruptiq-experiment`, `ruptiq-analyze` (with
this lab's metric names), and `ruptiq-investigate` (default thresholds) are in README.md, with
one recorded live run. Generated reports stay out of Git. The lab does not prove a CPU bottleneck
in any real application: the extra work is the fixture.

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

Ruptiq does not test the Order Service yet: its endpoints that matter all change state, and the
workload runner only sends GET requests, and the Order Service exposes no metrics for Core's
telemetry reader. Today the service is
exercised by its own unit tests, the Docker smoke test, and manual requests.

Some code is duplicated between `cmd/ruptiq-api` and the demo services (port parsing, shutdown
wiring, JSON helpers). That is deliberate: sharing it would make the targets depend on Core, or on
each other.

## Checkout demo: Inventory Service

```
demo/checkout/inventory-service/
  main.go              entry point: reads INVENTORY_SERVICE_PORT (default 8082), JSON logging,
                       graceful shutdown
  internal/httpapi/    HTTP layer: routes, strict JSON decoding, status codes, timeouts,
                       request logging; Prometheus metrics (metrics.go)
  internal/inventory/  domain layer: stock, reservations, validation, seed data, in-memory Store
```

It follows the same boundary rules as the Order Service: it imports nothing from Ruptiq Core or
from the Order Service, its packages are private under `inventory-service/internal/`, and `httpapi`
only translates between HTTP and the `inventory` package, which knows nothing about HTTP.

Endpoints: `GET /healthz`, `GET /inventory/{sku}`, `POST /reservations`, and `GET /metrics`
(Prometheus). Status codes: `201`
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

### Inventory Service telemetry

The service exposes server-side measurements at `GET /metrics` in the Prometheus text format, using
the official Prometheus Go client (`client_golang`). This is the only checkout service with
metrics; Ruptiq Core does not import `client_golang` (its reader uses only the text parser). README.md
lists the metrics and shows how to inspect them.

| Metric | Type | Labels |
| --- | --- | --- |
| `inventory_http_requests_total` | counter | `method`, `route`, `status` |
| `inventory_http_request_duration_seconds` | histogram (100 µs to 10 s) | `method`, `route`, `status` |
| `inventory_http_requests_in_flight` | gauge | none |
| `inventory_stock_available_units` | gauge | `sku` |
| `inventory_stock_reserved_units` | gauge | `sku` |

**Registry.** `NewHandler` creates a fresh `prometheus.Registry` for each handler. Nothing is
registered in the client library's global registry, so tests and multiple handlers in one process
never share metrics, and the output holds only these five metrics (no Go runtime or process
metrics yet).

**HTTP metrics.** One middleware, `observeRequests`, wraps the router. It logs every request as
before, and for every request except those to `/metrics` it increments the in-flight gauge, runs
the router, then records the count and duration. The route label comes from `r.Pattern`, which
`ServeMux` sets on the request it was given, so the middleware sees the matched pattern after the
router returns. `routeLabel` strips the method from the pattern (`GET /inventory/{sku}` becomes
`/inventory/{sku}`) and accepts only the registered templates; anything else, including the
catch-all `/` pattern and redirects that `ServeMux` issues itself (whose pattern can be a raw path),
becomes `unmatched`. Methods outside the standard set become `OTHER`. Requests to `/metrics` skip
the in-flight gauge and the counters entirely, so a scrape never sees itself.

The status comes from a `statusRecorder` around the response writer. It keeps the first final
(non-1xx) status, treats a body written without `WriteHeader` as `200`, and implements `Unwrap`,
so `http.ResponseController` can still reach the underlying writer's flushing and deadlines.

**Stock gauges.** A custom collector, `inventoryCollector`, keeps no numbers of its own. On each
scrape it calls `Store.Snapshot`, which copies every SKU's `available` and `reserved` under one
read lock and releases it before returning. The collector then builds constant metrics from the
copy. Consequences:

- The gauges cannot drift from the store, because there is nothing separate to update. A
  successful reservation shows up on the next scrape; replays and rejected requests change
  nothing in the store, so they change nothing in the gauges.
- A scrape sees each reservation either completely or not at all, across all SKUs.
- The store lock is held only to copy a few integers; no lock is held while metrics are encoded or
  the HTTP response is written.
- The `sku` label set is the catalog in the store, never a value taken from a request.

**Bounded labels.** Every label value comes from a fixed set: the route templates plus
`unmatched`, the standard methods plus `OTHER`, status codes the service sends, and the catalog's
SKUs. Order IDs, reservation IDs, request paths, query strings, bodies, and credentials never
become labels, because each new value would be a new time series held in memory forever, and could
expose identifiers.

**Server-side versus client-side.** The histogram measures from the moment the handler chain
receives the request (headers already read) until the handler returns. k6's latency in
`ruptiq-run` adds the network, connection handling, and anything queued before the handler, so the
gap between the two is time spent outside the service's handler. Counts differ as well: k6 counts
requests that never reached the service; the service counts requests from every client.

**Testing.** `metrics_test.go` scrapes `/metrics` through the handler and parses every response with
the Prometheus text parser (`expfmt`), so invalid exposition fails the test. It checks:

- The five metric families, their types, and HELP text; independent registries per handler.
- Exact request counts per method, route, and status for a mix of successful, replayed, rejected,
  invalid, wrong-method, and unknown-path requests, with one histogram observation each.
- Histogram buckets, using fixed durations passed to the recorder, so no assertion depends on real
  latency.
- The in-flight gauge while three requests are held inside a handler (`/metrics` scrapes during
  that time do not count themselves), then back to zero.
- Stock gauges at start, after a reservation, after idempotent replays, and after rejected
  reservations (insufficient stock, conflict, unknown SKU, invalid, malformed).
- 150 concurrent reservations for 10 units over a real `httptest` server while 30 scrapes run, with
  every scrape internally consistent and exact final counts.
- 150+ requests with IDs, secrets, and odd methods in paths, queries, and bodies: none appear in the
  output, every route and method label is from the allowed set, and the total count excludes
  scrapes.
- The status recorder's edge cases and that flushing still works through it.

`Store.Snapshot` has its own tests, including snapshots taken during concurrent reservations. The
Docker smoke test reads `/metrics` after the checkout scenarios and checks the stock gauges, the
number of created reservations, the stock-read count, and that no IDs appear.

**What Core still needs** to use this automatically: Core's `TelemetryReader` can read this
endpoint, and the experiment coordinator samples it before, during, and after a k6 run, with the
metrics address configured separately from the URL under load (see "Experiment: workload with
telemetry sampling"). Counters and histograms are turned into per-window rates and estimated
percentiles, with counter resets detected (see "Signal extraction: analysing a saved
experiment"), and a first `Investigator` ranks changes between windows (see "Investigator:
evidence-driven findings"). Still missing are storage of telemetry with each run (`RunStore`);
relating client-side and server-side numbers; and metrics from the Order and Payment Services.

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
  `.dockerignore` is an allow-list that sends only `go.mod`, `go.sum`, and `demo/checkout/` (minus
  tests) to the builder. Core code cannot enter these images, and Core has no Docker setup. Go
  modules (the Prometheus client used by the Inventory Service) are downloaded in a separate,
  cached layer, verified against `go.sum`.
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

The `docker-checkout` CI job builds the images, runs the environment, and passes the smoke test on
GitHub Actions (first passing on the pull request that added it and on the resulting `main`
push). Docker is not installed on the development machine, so container runs happen only in CI;
locally, the Compose file was schema-validated with `compose-spec/compose-go` v2.16.2 and the
build was reproduced by cross-compiling for Linux.

The `docker-checkout` job in `.github/workflows/ci.yml` runs on `ubuntu-latest`, in parallel with
the Go job:

1. Print Docker, Compose, curl, and jq versions (fails if any is missing).
2. `docker compose config --quiet`, then `docker compose build`.
3. `docker compose up --detach --wait --wait-timeout 120`: Compose polls the health checks and
   fails if any service exits or is not healthy within 120 s. No fixed sleeps.
4. `bash scripts/checkout-smoke-test.sh`: real HTTP checkout scenarios with exact status and body
   assertions (successful checkout, idempotent repeats, decline then approval), then checks of the
   Inventory Service's `/metrics` against those scenarios.
5. On failure: `docker compose ps --all`, each container's health check history, and all logs.
6. Always: `docker compose down --volumes --remove-orphans --rmi all`.

The workflow keeps `permissions: contents: read`, and the job has a 20-minute timeout. A passing
health check only means each process answers `/healthz`; the smoke test is what shows that a
checkout works across the three containers.

## What exists today

- `cmd/ruptiq-api`: Ruptiq Core program entry point.
- `internal/api`: Ruptiq Core HTTP server with `GET /healthz`.
- `cmd/ruptiq-run`, `internal/workload`, `internal/workload/k6`: the first `WorkloadRunner`, one
  HTTP GET workload run with k6, with a CLI report and JSON results.
- `cmd/ruptiq-compare`, `internal/comparison`: offline, descriptive comparison of two saved run
  reports.
- `cmd/ruptiq-telemetry`, `internal/telemetry`: the first `TelemetryReader`, reading one snapshot
  from a Prometheus text-format endpoint, with a console summary and JSON output.
- `cmd/ruptiq-experiment`, `internal/experiment`: one k6 workload with periodic Prometheus
  sampling before, during, and after it, combined in one versioned JSON report.
- `cmd/ruptiq-analyze`, `internal/analysis`: offline signal extraction from a saved experiment
  report: per-window request rates, 5xx shares, and histogram p95 estimates, with evidence
  references, warnings for missing or reset series, and a versioned JSON report.
- `cmd/ruptiq-investigate`, `internal/investigation`: the first `Investigator`, offline and
  deterministic: ranked observations of 5xx increases and latency and throughput degradation
  between earlier and later windows of one experiment, with evidence references, contradictions,
  limitations, and validation experiments, or an explicit abstention. Run on synthetic analyses
  and on one live controlled change from the regression lab.
- `cmd/ruptiq-diagnose`, `internal/diagnosis`: one command that runs an experiment, analyses the
  saved report, and investigates that analysis. Orchestration only. Abstention is a completed
  investigation. Before/after comparison of two analysis files remains `ruptiq-validate-fix`. Unit tested with fake
  runners and readers, and run once against the regression lab (see above).
- `cmd/ruptiq-validate-fix`, `internal/fixvalidation`: offline before/after comparison of two
  saved analyses. Server throughput, 5xx ratio, and the median of per-window p95 estimates, with
  evidence pointers. Run on synthetic analyses and on two live regression-lab pairs. An observed
  improvement is not a proven or repeatable fix.
- `cmd/ruptiq-validate-runs`, `internal/runvalidation`: offline comparison of two saved
  diagnosis directories. It calls `fixvalidation.Compare`. It does not run a workload or apply
  a change.
- `cmd/ruptiq-pair`, `internal/pairworkflow`: three explicit invocations that capture a baseline
  diagnosis, stop for a change applied outside Ruptiq, capture a candidate, and compare the two
  saved diagnoses through `runvalidation.Run`. Unit tested with fake runners and synthetic
  diagnoses. One observed improvement is not a cause.
- `demo/regression-lab`: loopback HTTP fixture with real CPU work, Prometheus HTTP metrics, and a
  timed baseline→degraded switch, used to validate the experiment/analysis/investigation pipeline
  and a two-run before/after comparison.
- `demo/checkout/order-service`: the Order Service (`GET /healthz`, `POST /orders`,
  `GET /orders/{id}`, `POST /orders/{id}/reserve`, `POST /orders/{id}/pay`), with in-memory
  storage. It reserves stock by calling the Inventory Service and takes simulated payments by
  calling the Payment Service, both over HTTP.
- `demo/checkout/inventory-service`: the Inventory Service (`GET /healthz`,
  `GET /inventory/{sku}`, `POST /reservations`), with in-memory stock and idempotent,
  concurrency-safe reservations, and Prometheus metrics at `GET /metrics`.
- `demo/checkout/payment-service`: the Payment Service (`GET /healthz`, `POST /payments`,
  `GET /payments/{id}`), with simulated, idempotent payments and in-memory storage.

- `compose.yaml` and `demo/checkout/Dockerfile`: a Docker Compose environment for the three
  checkout services, run and smoke-tested by CI.

Root-cause diagnosis, `RunStore` (run history), load ramps, cancellation and compensation across services, persistence,
target adapters, metrics for the Order and Payment Services, a Prometheus server, dashboards, and
tracing are not implemented.

## Decisions deferred

- Where each further checkout demo service lives (under `demo/checkout/`, separate from Core).
- Whether the demo eventually gets its own Go module (it shares Core's module for now). Core
  imports only the Prometheus text parser and data model (`common/expfmt`, `client_model`); the
  client library (`client_golang`) is in `go.mod` for the Inventory Service and the regression lab.
- Storage technology for `RunStore`.
- Whether load generation stays with k6 for ramps, other request types, and distributed load.
- The adapter mechanism for external targets.
