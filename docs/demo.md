# Ruptiq — Guided Performance Regression Demo

This is a local demonstration of the guided workflow. You start a small service, capture a
baseline, stop that service, start it in a different CPU mode, capture a candidate, stop it
again, and compare the two saved diagnoses. Ruptiq does not restart the service and does
not apply a fix.

Run the commands from the repository root. `tmp/` is gitignored. The reports this demo
writes stay on your machine.

## Who does what

| Step | You | Ruptiq |
| --- | --- | --- |
| 1 | Start `demo/regression-lab` in heavy CPU mode | Nothing |
| 2 | Run `ruptiq-pair -stage before` | Runs k6, scrapes Prometheus, writes the baseline diagnosis, then exits |
| 3 | Stop the lab | Nothing |
| 4 | Start the lab again in lighter CPU mode | Nothing |
| 5 | Run `ruptiq-pair -stage after` with the same workload settings | Runs k6, scrapes Prometheus, writes the candidate diagnosis, then exits |
| 6 | Stop the lab | Nothing |
| 7 | Run `ruptiq-pair -stage compare` with no lab process | Reads the two saved diagnoses and writes the comparison |
| 8 | Open the JSON files | Nothing |

The `before` command prints:

> Baseline captured. Apply your change outside Ruptiq, then explicitly run the after stage. Ruptiq does not apply or verify a code change.

The `after` command prints:

> Candidate captured. Run the compare stage to judge the two saved diagnoses. Ruptiq does not apply or verify a code change.

## What you need

- Go 1.27 or newer, so `go run` can build `demo/regression-lab` and `cmd/ruptiq-pair` from this checkout. The module version is in `go.mod`.
- k6 on `PATH`, installed separately. This repository was developed against k6 v2.3.0. Ruptiq does not download it.
- A free loopback port. The commands below use `127.0.0.1:8090`.
- Two terminals, both at the repository root: one for the lab, one for Ruptiq.

A prebuilt `ruptiq-pair` can replace `go run ./cmd/ruptiq-pair`. The lab is source in this
repository, so the demo still needs Go. See the install section in `README.md` for the
unsigned development archives. Compare does not need k6. Before and after do.

## Use a new pair directory

Completed artifacts are protected against overwrite. A second `compare` of the same
directory is refused: the built command exits 2, and `go run` prints `exit status 2` and
returns 1. The files are left as they were.

Use `tmp/demo-pair` only when that directory does not already contain a finished pair. For
another demonstration, pick a new name such as `tmp/demo-pair-2` and use it in every
command below. Do not delete unrelated directories or reports.

## 1. Start the lab in heavy CPU mode

In the lab terminal:

```powershell
go run ./demo/regression-lab -addr 127.0.0.1:8090 -degrade-after 0s -baseline-iters 157135 -degraded-iters 11233031
```

```sh
go run ./demo/regression-lab -addr 127.0.0.1:8090 -degrade-after 0s -baseline-iters 157135 -degraded-iters 11233031
```

Leave that process running. `-degrade-after 0s` selects the larger iteration count for
`GET /work`. The address must be loopback.

In the other terminal, confirm the lab is up. The body is JSON with `"status":"ok"` and
`"service":"regression-lab"`.

```powershell
(Invoke-WebRequest -UseBasicParsing http://127.0.0.1:8090/healthz).Content
```

```sh
curl -fsS http://127.0.0.1:8090/healthz
```

If something else already answers on 8090, stop here. Do not stop a process you do not
own. Pick another loopback port, change `-addr`, and use that same host and port in both
URLs below.

## 2. Capture the baseline

```powershell
go run ./cmd/ruptiq-pair -stage before -pair-dir tmp/demo-pair -url http://127.0.0.1:8090/work -metrics-url http://127.0.0.1:8090/metrics -vus 2 -duration 20s -interval 1s -k6 k6 -requests-metric regression_lab_http_requests_total -duration-metric regression_lab_http_request_duration_seconds
```

```sh
go run ./cmd/ruptiq-pair -stage before -pair-dir tmp/demo-pair -url http://127.0.0.1:8090/work -metrics-url http://127.0.0.1:8090/metrics -vus 2 -duration 20s -interval 1s -k6 k6 -requests-metric regression_lab_http_requests_total -duration-metric regression_lab_http_request_duration_seconds
```

`-k6 k6` uses the `k6` executable on `PATH`. On Windows that is usually `k6.exe`. If k6
lives somewhere else, pass that path instead of `k6`.

A finished baseline exits 0 and writes:

```text
tmp/demo-pair/before/experiment.json
tmp/demo-pair/before/analysis.json
tmp/demo-pair/before/investigation.json
tmp/demo-pair/before/manifest.json
tmp/demo-pair/pair-manifest.json
```

`pair-manifest.json` field `state` is `baseline_captured`. There is no `after/` directory
yet. Ruptiq has exited. It will not start the next stage.

## 3. Stop the lab

In the lab terminal, press Ctrl+C. On Windows, `go run` may then print
`exit status 0xffffffff`. That is the lab process being stopped. It is not a failed
workload, and it does not undo the baseline that already finished.

Wait until the health URL no longer answers before you start the lab again. Do not start
the light lab while the heavy process is still bound to the port.

## 4. Start the lab in lighter CPU mode

Same binary, same iteration counts, and a delay that is longer than the 20 second
workload. The lab stays on the smaller iteration count for this run.

```powershell
go run ./demo/regression-lab -addr 127.0.0.1:8090 -degrade-after 1h -baseline-iters 157135 -degraded-iters 11233031
```

```sh
go run ./demo/regression-lab -addr 127.0.0.1:8090 -degrade-after 1h -baseline-iters 157135 -degraded-iters 11233031
```

Check `/healthz` again, the same way as in step 1.

## 5. Capture the candidate

The material settings must match the baseline: pair directory, both URLs, virtual users,
duration, scrape interval, k6 path, and the two metric names. Only `-stage` changes.

```powershell
go run ./cmd/ruptiq-pair -stage after -pair-dir tmp/demo-pair -url http://127.0.0.1:8090/work -metrics-url http://127.0.0.1:8090/metrics -vus 2 -duration 20s -interval 1s -k6 k6 -requests-metric regression_lab_http_requests_total -duration-metric regression_lab_http_request_duration_seconds
```

```sh
go run ./cmd/ruptiq-pair -stage after -pair-dir tmp/demo-pair -url http://127.0.0.1:8090/work -metrics-url http://127.0.0.1:8090/metrics -vus 2 -duration 20s -interval 1s -k6 k6 -requests-metric regression_lab_http_requests_total -duration-metric regression_lab_http_request_duration_seconds
```

If a setting does not match, the command exits before the workload. A built binary exits
2. `go run` prints `exit status 2` and returns 1. It does not create `after/`.

A finished candidate exits 0. `state` becomes `candidate_captured`, and these files exist:

```text
tmp/demo-pair/after/experiment.json
tmp/demo-pair/after/analysis.json
tmp/demo-pair/after/investigation.json
tmp/demo-pair/after/manifest.json
```

## 6. Stop the lab again

Press Ctrl+C in the lab terminal. The same Windows `exit status 0xffffffff` note from
step 3 applies. Confirm `/healthz` no longer answers. Compare does not want a live lab.

## 7. Compare the saved diagnoses

```powershell
go run ./cmd/ruptiq-pair -stage compare -pair-dir tmp/demo-pair
```

```sh
go run ./cmd/ruptiq-pair -stage compare -pair-dir tmp/demo-pair
```

This command does not start k6 and does not contact the lab. It reads `before/` and
`after/`. A finished comparison exits 0, sets `state` to `comparison_completed`, and writes:

```text
tmp/demo-pair/comparison/validation.json
tmp/demo-pair/comparison/manifest.json
```

`pair-manifest.json` is updated in the same directory. Request counts, latency, and the
comparison outcome depend on the machine. They are not a fixed demo script.

## 8. Inspect the reports

These names are the schema fields. Open the file your command names. Do not expect the
numbers from the recorded run in the next section.

### Pair state

`tmp/demo-pair/pair-manifest.json`

- `state` is the workflow state: `baseline_captured`, `candidate_captured`, or `comparison_completed`.
- `before.analysis_sha256` and `after.analysis_sha256` are the SHA-256 hashes of the raw `analysis.json` bytes.
- `comparison.outcome` is the before/after judgment.
- `comparison.comparable` is whether the two runs were accepted as the same test.
- `comparison.before_analysis_sha256` and `comparison.after_analysis_sha256` repeat those analysis hashes on the completed comparison.

```powershell
$pair = Get-Content -Raw tmp\demo-pair\pair-manifest.json | ConvertFrom-Json
$pair.state
$pair.comparison.outcome
$pair.comparison.comparable
$pair.comparison.before_analysis_sha256
$pair.comparison.after_analysis_sha256
```

```sh
python3 -c 'import json; p=json.load(open("tmp/demo-pair/pair-manifest.json")); c=p["comparison"]; print(p["state"]); print(c["outcome"]); print(c["comparable"]); print(c["before_analysis_sha256"]); print(c["after_analysis_sha256"])'
```

If your Python command is `python` rather than `python3`, use that name. The hash check
below uses a different tool on macOS than on Linux.

### One diagnosis

Each of `before/` and `after/` has the same four files.

`experiment.json`

- `status` is the experiment status. A finished workload that met its checks is `passed`.
- `telemetry.completeness` is `complete` when every planned scrape succeeded.
- `telemetry.attempts`, `telemetry.succeeded`, and `telemetry.failed` are the scrape counts.
- `workload.result.metrics.http_requests` and `workload.result.metrics.failed_requests` are k6's client counts. They are not the server-side comparison.

`manifest.json` for that stage

- `outcome` is the diagnosis outcome. A finished diagnosis is `completed`. That is not the investigator's finding.
- `stages` lists each stage. The experiment stage has `experiment_status`. The investigation stage has `investigation_outcome`.

`investigation.json`

- `outcome` is the single-run finding: `changes_observed`, `no_material_change`, or `insufficient_evidence`.
- `summary` is one sentence for that one run.

A stable CPU mode often produces `no_material_change` inside one run. That does not say
the two runs match each other. The comparison in the next files is the cross-run result.

```powershell
$exp = Get-Content -Raw tmp\demo-pair\before\experiment.json | ConvertFrom-Json
$exp.status
$exp.telemetry.completeness
$exp.telemetry.attempts
$exp.telemetry.succeeded
$exp.telemetry.failed
$inv = Get-Content -Raw tmp\demo-pair\before\investigation.json | ConvertFrom-Json
$inv.outcome
```

```sh
python3 -c 'import json; e=json.load(open("tmp/demo-pair/before/experiment.json")); i=json.load(open("tmp/demo-pair/before/investigation.json")); t=e["telemetry"]; print(e["status"]); print(t["completeness"], t["attempts"], t["succeeded"], t["failed"]); print(i["outcome"])'
```

Repeat with `after` in place of `before`.

### The comparison

`comparison/validation.json`

- `validation.outcome` is one of `improvement_observed`, `regression_observed`, `mixed`, `no_material_change`, `insufficient_evidence`, or `not_comparable`.
- `validation.comparability.comparable` is true when the runs were accepted as the same test. `not_comparable` is a finished result, not a crashed command.
- `validation.routes[]` has `method`, `route`, `throughput`, and `latency`.
- For `GET` and `/work`, `throughput.before.value` and `throughput.after.value` are server request rates. `unit` is `requests/s`. `relative` is the relative change.
- `latency.before.value` and `latency.after.value` are the median of the per-window p95 estimates. `unit` is `seconds`. `estimate` is true. This is not an exact percentile.
- `throughput.evidence[]` and `latency.evidence[]` have `side` (`before` or `after`) and `pointer`. The pointer is a JSON pointer into that side's `analysis.json`, not into `validation.json`.

`comparison/manifest.json`

- `outcome` and `comparable` match the validation result.
- `before.analysis_sha256` and `after.analysis_sha256` are the same raw-file hashes recorded on the pair manifest.

```powershell
$v = (Get-Content -Raw tmp\demo-pair\comparison\validation.json | ConvertFrom-Json).validation
$v.outcome
$v.comparability.comparable
$work = $v.routes | Where-Object { $_.method -eq "GET" -and $_.route -eq "/work" }
$work.throughput.before.value
$work.throughput.after.value
$work.throughput.unit
$work.latency.before.value
$work.latency.after.value
$work.latency.unit
$work.throughput.evidence | Select-Object side, pointer
```

```sh
python3 -c 'import json; v=json.load(open("tmp/demo-pair/comparison/validation.json"))["validation"]; print(v["outcome"]); print(v["comparability"]["comparable"]);
r=next(x for x in v["routes"] if x["method"]=="GET" and x["route"]=="/work");
t=r["throughput"]; L=r["latency"];
print(t["before"]["value"], t["after"]["value"], t["unit"]);
print(L["before"]["value"], L["after"]["value"], L["unit"]);
[print(e["side"], e["pointer"]) for e in t["evidence"]]'
```

### Check the analysis hashes

The stored hashes are lowercase hex. `Get-FileHash` prints uppercase. Compare the letters
without regard to case. The digest is of the file bytes, not of a reformatted JSON value.

Windows PowerShell:

```powershell
Get-FileHash -Algorithm SHA256 tmp\demo-pair\before\analysis.json
Get-FileHash -Algorithm SHA256 tmp\demo-pair\after\analysis.json
```

Linux:

```sh
sha256sum tmp/demo-pair/before/analysis.json tmp/demo-pair/after/analysis.json
```

macOS:

```sh
shasum -a 256 tmp/demo-pair/before/analysis.json tmp/demo-pair/after/analysis.json
```

## One recorded result

The numbers below are from one controlled regression-lab run already written up as
[Task 12A — Live Guided Before/After Validation](../README.md#task-12a--live-guided-beforeafter-validation).
They are not the output this guide will print, and they are not a guarantee for the next
run. A different machine, background load, or timing changes the counts. The run is not a
statistical significance test, not a production benchmark, and not proof that a software
fix worked. The change was the lab's CPU mode, applied by stopping and starting the lab
outside Ruptiq.

On that Windows run the eligible `GET /work` server throughput was about 141.16
requests/second in the heavy run and about 3979.69 requests/second in the light run. The
Task 12A table rounds those rates to 141.2 and 3979.7 requests/second. The median
per-window p95 estimate was 24.250 ms in the heavy run and 0.938 ms in the light run. The
comparison outcome was `improvement_observed`, and comparability was true.

Each run's own `investigation.outcome` was `no_material_change`. The heavy run stayed in
heavy CPU mode, and the light run stayed in light CPU mode, so the single-run investigator
had no change inside either file. The before/after comparison is what identified the
change between the two runs. k6 client latency is recorded in `experiment.json` and is not
the server-side conclusion.

## How the pieces fit

Ruptiq Core is Go. `ruptiq-pair` is a thin command over `internal/pairworkflow`. The
workflow calls the existing diagnosis and comparison code. It does not contain a second
copy of the judgment rules.

For `before` and `after`, the command starts k6 as a separate process and scrapes the
Prometheus URL on the interval you set. When the workload returns, analysis and the
single-run investigation run offline on the samples just saved. `compare` does not start a
process for the target. It checks the two diagnosis directories and calls the same
comparison used by `ruptiq-validate-runs`.

Each stage has a manifest. The pair manifest records the workflow state. An exclusive
reservation file stops two workflows from writing one pair directory at the same time.
A finished baseline, candidate, or comparison is not overwritten by a later command.
Configuration fingerprinting compares the after stage with the baseline before any
candidate workload starts, including the target URL, metrics URL, virtual users, duration,
and scrape interval. The original URL strings are not stored; the fingerprint stores a
redacted URL and a SHA-256 of the original string. Evidence pointers name objects inside
the analysis file, and the comparison manifest records the SHA-256 of each analysis file
so a later reader can see which bytes were judged.

GitHub Actions runs this workflow on Ubuntu with real k6 and the loopback lab. That job
checks that the stages complete and the evidence resolves. It does not require a
particular throughput or latency. A separate job cross-compiles the three CLIs. It
executes the Linux AMD64 binaries only for `-h` and checks the other archives by file
header. Those archives are unsigned development builds, not a release.

The design write-up is [docs/architecture.md](architecture.md).

## Limits of this repository

- There is no GUI and no hosted web dashboard.
- Ruptiq does not change application code and does not restart a service.
- An investigation or a comparison does not establish a proven root cause.
- There is no statistical significance test.
- There is no database of past runs. A pair directory is the record.
- k6 is an external program. This repository does not ship it.
- The CLI archives are unsigned development builds. CI does not execute the Windows or macOS binaries.
- Windows and macOS can build and, on a machine where the tools are installed, run the demo. The GitHub workflow that runs the demo path is Ubuntu.
- The repository has no license file. This document does not grant one.
