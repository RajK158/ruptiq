#!/usr/bin/env bash
# Guided before/after smoke test for CI and a Linux machine.
#
# It builds ruptiq-pair and the regression lab, runs real k6 against a
# loopback lab, and checks the saved JSON. It does not assert exact request
# rates or latency. A different but valid comparison outcome is still a pass.
#
#   bash scripts/guided-workflow-smoke-test.sh self-check
#   bash scripts/guided-workflow-smoke-test.sh
#
# Requires bash, curl, python3, Go, and k6 (v2.3.0 in CI). K6_BIN overrides
# the k6 executable. The script never uses Docker and only deletes the
# temporary directory it creates.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
STAGE="init"
LAB_PID=""
WORKDIR=""
PAIR_LOG=""
PORT=""

log() { printf '%s\n' "$*"; }

redact() {
  python3 -c 'import re,sys
text = sys.stdin.read()
text = re.sub(r"(https?://)[^/\s:@]+:[^@\s]*@", r"\1", text)
text = re.sub(r"(https?://[^\s?]+)\?[^\s]*", r"\1", text)
sys.stdout.write(text)'
}

stop_owned() {
  local pid="${1:-}"
  local i
  if [[ -z "$pid" ]]; then
    return 0
  fi
  if ! kill -0 "$pid" 2>/dev/null; then
    wait "$pid" 2>/dev/null || true
    return 0
  fi
  kill "$pid" 2>/dev/null || true
  for i in $(seq 1 50); do
    if ! kill -0 "$pid" 2>/dev/null; then
      wait "$pid" 2>/dev/null || true
      return 0
    fi
    sleep 0.1
  done
  kill -9 "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
}

port_is_free() {
  python3 - "$PORT" <<'PY'
import socket, sys
port = int(sys.argv[1])
sock = socket.socket()
sock.settimeout(0.2)
try:
    code = sock.connect_ex(("127.0.0.1", port))
finally:
    sock.close()
sys.exit(0 if code != 0 else 1)
PY
}

wait_port_free() {
  local i
  for i in $(seq 1 50); do
    if port_is_free; then
      return 0
    fi
    sleep 0.1
  done
  log "port ${PORT} stayed busy after the owned lab was stopped" >&2
  return 1
}

wait_ready() {
  local i
  for i in $(seq 1 50); do
    if curl -fsS --max-time 1 "http://127.0.0.1:${PORT}/healthz" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.2
  done
  log "regression lab did not answer /healthz on 127.0.0.1:${PORT}" >&2
  return 1
}

# remove_workdir deletes only a directory this process created with mktemp.
# KEEP_WORKDIR=1, an empty flag, or any other path leaves the directory in place.
remove_workdir() {
  if [[ "${KEEP_WORKDIR:-}" == "1" || "${CREATED_WORKDIR:-}" != "1" ]]; then
    return 0
  fi
  if [[ -z "$WORKDIR" || ! -d "$WORKDIR" ]]; then
    return 0
  fi
  case "$WORKDIR" in
    */ruptiq-guided-smoke.*) rm -rf "$WORKDIR" ;;
    *) log "refusing to delete unexpected directory" >&2 ;;
  esac
}

cleanup() {
  local status=$?
  if [[ -n "$LAB_PID" ]]; then
    stop_owned "$LAB_PID" || true
    LAB_PID=""
  fi
  remove_workdir || true
  if [[ "$status" -ne 0 ]]; then
    log "guided-workflow-smoke failed during: ${STAGE}" >&2
  fi
  exit "$status"
}

trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

self_check() {
  local owned other stubborn listener portfile fixture_root preexisting unrelated owned_dir i freed=0
  command -v python3 >/dev/null 2>&1 || { log "self-check: python3 is required" >&2; exit 1; }

  sleep 30 &
  owned=$!
  stop_owned "$owned"
  if kill -0 "$owned" 2>/dev/null; then
    log "self-check: owned process was still running" >&2
    exit 1
  fi

  sleep 30 &
  other=$!
  stop_owned ""
  if ! kill -0 "$other" 2>/dev/null; then
    log "self-check: an unowned process was stopped" >&2
    exit 1
  fi
  stop_owned "$other"
  if kill -0 "$other" 2>/dev/null; then
    log "self-check: owned sleep was not reaped" >&2
    exit 1
  fi

  # Ignore SIGTERM so stop_owned must escalate to SIGKILL within its poll bound.
  bash -c 'trap "" TERM; while true; do :; done' &
  stubborn=$!
  stop_owned "$stubborn"
  if kill -0 "$stubborn" 2>/dev/null; then
    log "self-check: SIGTERM-resistant process was not reaped" >&2
    exit 1
  fi

  fixture_root="$(mktemp -d "${TMPDIR:-/tmp}/ruptiq-self-check.XXXXXX")"
  portfile="$fixture_root/port"
  python3 -c 'import socket, sys, time
s = socket.socket()
s.bind(("127.0.0.1", 0))
s.listen(1)
sys.stdout.write("%s\n" % s.getsockname()[1])
sys.stdout.flush()
time.sleep(30)
' >"$portfile" &
  listener=$!
  for i in $(seq 1 50); do
    if [[ -s "$portfile" ]]; then
      break
    fi
    sleep 0.1
  done
  PORT="$(tr -d '[:space:]' <"$portfile")"
  if [[ -z "$PORT" ]] || port_is_free; then
    stop_owned "$listener" || true
    log "self-check: occupied port was not detected" >&2
    exit 1
  fi
  stop_owned "$listener"
  freed=0
  for i in $(seq 1 50); do
    if port_is_free; then
      freed=1
      break
    fi
    sleep 0.1
  done
  if [[ "$freed" -ne 1 ]]; then
    log "self-check: port stayed busy after the owned listener was stopped" >&2
    exit 1
  fi

  preexisting="$(mktemp -d "${TMPDIR:-/tmp}/ruptiq-guided-smoke.XXXXXX")"
  unrelated="$(mktemp -d "${fixture_root}/unrelated.XXXXXX")"
  owned_dir="$(mktemp -d "${TMPDIR:-/tmp}/ruptiq-guided-smoke.XXXXXX")"
  echo keep >"$preexisting/marker"
  echo keep >"$unrelated/marker"
  echo gone >"$owned_dir/marker"
  WORKDIR="$preexisting"
  CREATED_WORKDIR=
  remove_workdir
  WORKDIR="$unrelated"
  CREATED_WORKDIR=1
  remove_workdir
  if [[ ! -f "$preexisting/marker" || ! -f "$unrelated/marker" ]]; then
    log "self-check: cleanup deleted a directory it does not own" >&2
    exit 1
  fi
  WORKDIR="$owned_dir"
  CREATED_WORKDIR=1
  remove_workdir
  if [[ -d "$owned_dir" ]]; then
    log "self-check: cleanup left an owned temporary directory" >&2
    exit 1
  fi
  WORKDIR=""
  CREATED_WORKDIR=
  rm -rf "$fixture_root" "$preexisting"

  verify cpu-self-test "$PWD"
  log "self-check: owned processes were stopped and reaped, and cpu-mode checks passed"
}

if [[ $# -gt 1 || ( $# -eq 1 && "${1:-}" != "self-check" ) ]]; then
  log "usage: $0 [self-check]" >&2
  exit 2
fi

if [[ "${1:-}" != "self-check" ]]; then
for tool in curl python3 go; do
  command -v "$tool" >/dev/null 2>&1 || { log "missing required tool: $tool" >&2; exit 1; }
done
K6_BIN="${K6_BIN:-$(command -v k6 || true)}"
if [[ -z "$K6_BIN" || ! -x "$K6_BIN" ]]; then
  log "k6 is required (set K6_BIN to the executable)" >&2
  exit 1
fi

STAGE="choose-port"
for PORT in $(seq 18090 18099); do
  if port_is_free; then
    break
  fi
  PORT=""
done
if [[ -z "$PORT" ]]; then
  log "no free loopback port in 18090-18099" >&2
  exit 1
fi

STAGE="build"
WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/ruptiq-guided-smoke.XXXXXX")"
CREATED_WORKDIR=1
PAIR_DIR="$WORKDIR/pair"
mkdir -p "$WORKDIR/bin"
(
  cd "$ROOT"
  go build -o "$WORKDIR/bin/regression-lab" ./demo/regression-lab
  go build -o "$WORKDIR/bin/ruptiq-pair" ./cmd/ruptiq-pair
)
LAB_BIN="$WORKDIR/bin/regression-lab"
PAIR_BIN="$WORKDIR/bin/ruptiq-pair"
fi

start_lab() {
  local degrade_after="$1"
  if [[ -n "$LAB_PID" ]]; then
    stop_owned "$LAB_PID"
    LAB_PID=""
  fi
  if ! port_is_free; then
    log "port ${PORT} is in use before starting the lab" >&2
    exit 1
  fi
  "$LAB_BIN" -addr "127.0.0.1:${PORT}" -degrade-after "$degrade_after" \
    -baseline-iters 157135 -degraded-iters 11233031 &
  LAB_PID=$!
  if ! wait_ready; then
    stop_owned "$LAB_PID" || true
    LAB_PID=""
    exit 1
  fi
}

run_pair() {
  PAIR_LOG="$WORKDIR/pair-last.log"
  set +e
  "$PAIR_BIN" "$@" >"$PAIR_LOG" 2>&1
  PAIR_STATUS=$?
  set -e
}

require_status() {
  local want="$1"
  if [[ "$PAIR_STATUS" -ne "$want" ]]; then
    log "ruptiq-pair exited ${PAIR_STATUS}, want ${want}" >&2
    if [[ -f "$PAIR_LOG" ]]; then
      redact <"$PAIR_LOG" | tail -n 40 >&2 || true
    fi
    exit 1
  fi
}

snapshot_before() {
  (
    cd "$PAIR_DIR/before"
    sha256sum experiment.json analysis.json investigation.json manifest.json
  ) | sort
}

assert_no_reservations() {
  local found
  found="$(find "$PAIR_DIR" -name '.ruptiq-*' -print)"
  if [[ -n "$found" ]]; then
    log "a pair, diagnosis, or comparison reservation is still present" >&2
    exit 1
  fi
}

run_verify() {
  if ! verify "$@"; then
    if [[ -n "$PAIR_LOG" && -f "$PAIR_LOG" ]]; then
      redact <"$PAIR_LOG" | tail -n 40 >&2 || true
    fi
    exit 1
  fi
}

verify() {
  python3 - "$1" "${2:-$PAIR_DIR}" <<'PY'
import hashlib, json, os, sys

mode, root = sys.argv[1], sys.argv[2]
known = {
    "improvement_observed",
    "regression_observed",
    "mixed",
    "no_material_change",
    "insufficient_evidence",
    "not_comparable",
}

def load(path):
    with open(path, encoding="utf-8") as handle:
        return json.load(handle)

def sha(path):
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        digest.update(handle.read())
    return digest.hexdigest()

def fail(message):
    raise SystemExit(message)

def gauge(sample, name):
    for family in sample.get("families", []):
        if family.get("name") == name:
            series = family.get("series") or []
            if not series or "value" not in series[0]:
                fail(f"sample {sample.get('sequence')} has no {name} value")
            value = series[0]["value"]
            if isinstance(value, bool) or float(value) != int(value):
                fail(f"{name} value {value!r} is not an integer")
            return int(value)
    fail(f"sample {sample.get('sequence')} is missing {name}")

def check_diagnosis(side, mode_name):
    directory = os.path.join(root, side)
    for name in ("experiment.json", "analysis.json", "investigation.json", "manifest.json"):
        if not os.path.isfile(os.path.join(directory, name)):
            fail(f"{side} is missing {name}")
    manifest = load(os.path.join(directory, "manifest.json"))
    if manifest.get("outcome") != "completed":
        fail(f"{side} diagnosis outcome is {manifest.get('outcome')}")
    experiment = load(os.path.join(directory, "experiment.json"))
    if experiment.get("status") != "passed":
        fail(f"{side} experiment status is {experiment.get('status')}")
    telemetry = experiment.get("telemetry") or {}
    if telemetry.get("completeness") != "complete":
        fail(f"{side} telemetry is {telemetry.get('completeness')}")
    if telemetry.get("failed") != 0 or telemetry.get("succeeded") != telemetry.get("attempts"):
        fail(f"{side} scrapes succeeded={telemetry.get('succeeded')} attempts={telemetry.get('attempts')} failed={telemetry.get('failed')}")
    metrics = ((experiment.get("workload") or {}).get("result") or {}).get("metrics") or {}
    if metrics.get("failed_requests") != 0 or not metrics.get("http_requests"):
        fail(f"{side} k6 requests={metrics.get('http_requests')} failed={metrics.get('failed_requests')}")
    during = [sample for sample in telemetry.get("samples") or [] if sample.get("phase") == "during_workload"]
    check_cpu_mode(during, mode_name)
    return experiment

BASELINE_ITERS = 157135
DEGRADED_ITERS = 11233031
MIN_DURING_SAMPLES = 3

def sample_mode(sample):
    degraded = gauge(sample, "regression_lab_degraded")
    iterations = gauge(sample, "regression_lab_work_iterations")
    sequence = sample.get("sequence")
    if degraded == 0:
        if iterations != BASELINE_ITERS:
            fail(f"sample {sequence} is baseline mode with iterations {iterations}")
        return "baseline"
    if degraded == 1:
        if iterations != DEGRADED_ITERS:
            fail(f"sample {sequence} is degraded mode with iterations {iterations}")
        return "degraded"
    fail(f"sample {sequence} has unexpected mode {degraded}")

def check_cpu_mode(during, mode_name):
    if len(during) < MIN_DURING_SAMPLES:
        fail(f"{mode_name} has {len(during)} during_workload samples, want at least {MIN_DURING_SAMPLES}")
    modes = [sample_mode(sample) for sample in during]
    if mode_name == "light":
        if any(mode != "baseline" for mode in modes):
            fail("light run left baseline mode")
        return
    if mode_name != "heavy":
        fail(f"unknown cpu mode {mode_name}")
    if "degraded" not in modes:
        fail("heavy run never transitioned to degraded mode")
    first = modes.index("degraded")
    if any(mode != "degraded" for mode in modes[first:]):
        fail("heavy run left degraded mode after the transition")

def cpu_self_test():
    def one(seq, degraded, iterations, omit=None):
        families = []
        if omit != "degraded":
            families.append({"name": "regression_lab_degraded", "series": [{"value": degraded}]})
        if omit != "iterations":
            families.append({"name": "regression_lab_work_iterations", "series": [{"value": iterations}]})
        return {"sequence": seq, "phase": "during_workload", "families": families}

    def expect_ok(samples, mode_name):
        check_cpu_mode(samples, mode_name)

    def expect_fail(samples, mode_name):
        try:
            check_cpu_mode(samples, mode_name)
        except SystemExit:
            return
        raise SystemExit("cpu check accepted a sample sequence it must reject")

    heavy = [one(i, 1, DEGRADED_ITERS) for i in range(1, 4)]
    expect_ok(heavy, "heavy")
    mixed = [one(1, 0, BASELINE_ITERS), one(2, 0, BASELINE_ITERS), one(3, 1, DEGRADED_ITERS), one(4, 1, DEGRADED_ITERS)]
    expect_ok(mixed, "heavy")
    expect_ok([one(i, 0, BASELINE_ITERS) for i in range(1, 4)], "light")
    expect_fail([one(i, 0, BASELINE_ITERS) for i in range(1, 4)], "heavy")
    expect_fail([one(1, 1, DEGRADED_ITERS), one(2, 0, BASELINE_ITERS), one(3, 1, DEGRADED_ITERS)], "heavy")
    expect_fail([one(1, 0, BASELINE_ITERS), one(2, 1, BASELINE_ITERS), one(3, 1, DEGRADED_ITERS)], "heavy")
    expect_fail([one(1, 1, DEGRADED_ITERS), one(2, 1, DEGRADED_ITERS)], "heavy")
    expect_fail([one(1, 2, DEGRADED_ITERS), one(2, 1, DEGRADED_ITERS), one(3, 1, DEGRADED_ITERS)], "heavy")
    expect_fail([one(1, 0, DEGRADED_ITERS), one(2, 0, BASELINE_ITERS), one(3, 0, BASELINE_ITERS)], "light")
    expect_fail([one(1, 1, DEGRADED_ITERS), one(2, 0, BASELINE_ITERS), one(3, 0, BASELINE_ITERS)], "light")
    expect_fail([one(1, True, BASELINE_ITERS), one(2, 0, BASELINE_ITERS), one(3, 0, BASELINE_ITERS)], "light")
    expect_fail([one(1, 0, BASELINE_ITERS, omit="degraded"), one(2, 0, BASELINE_ITERS), one(3, 0, BASELINE_ITERS)], "light")
    expect_fail([one(1, 0, BASELINE_ITERS, omit="iterations"), one(2, 0, BASELINE_ITERS), one(3, 0, BASELINE_ITERS)], "light")
    expect_fail([one(1, 1.5, DEGRADED_ITERS), one(2, 1, DEGRADED_ITERS), one(3, 1, DEGRADED_ITERS)], "heavy")
    print("cpu-self-test passed")

def resolve(document, pointer):
    if not isinstance(pointer, str) or not pointer.startswith("/"):
        fail(f"pointer {pointer!r} is not a JSON pointer")
    current = document
    for part in pointer.split("/")[1:]:
        if isinstance(current, list):
            if not part.isdigit() or int(part) >= len(current):
                fail(f"pointer {pointer} index {part} is missing")
            current = current[int(part)]
        elif isinstance(current, dict):
            if part not in current:
                fail(f"pointer {pointer} field {part} is missing")
            current = current[part]
        else:
            fail(f"pointer {pointer} cannot continue")
        if current is None:
            fail(f"pointer {pointer} resolved to null")
    return current

def pointers(node, found):
    if isinstance(node, dict):
        if "side" in node and "pointer" in node:
            found.append(node)
        for value in node.values():
            pointers(value, found)
    elif isinstance(node, list):
        for value in node:
            pointers(value, found)

if mode == "before":
    check_diagnosis("before", "heavy")
    pair = load(os.path.join(root, "pair-manifest.json"))
    if pair.get("state") != "baseline_captured":
        fail(f"pair state is {pair.get('state')}")
    if os.path.exists(os.path.join(root, "after")):
        fail("after was created during the before stage")
elif mode == "after":
    check_diagnosis("before", "heavy")
    check_diagnosis("after", "light")
    pair = load(os.path.join(root, "pair-manifest.json"))
    if pair.get("state") != "candidate_captured":
        fail(f"pair state is {pair.get('state')}")
elif mode == "compare":
    before = check_diagnosis("before", "heavy")
    after = check_diagnosis("after", "light")
    pair = load(os.path.join(root, "pair-manifest.json"))
    compared = load(os.path.join(root, "comparison", "manifest.json"))
    report = load(os.path.join(root, "comparison", "validation.json"))
    if pair.get("state") != "comparison_completed":
        fail(f"pair state is {pair.get('state')}")
    if compared.get("schema_version") != 1 or compared.get("versions", {}).get("run_validation_schema") != 1:
        fail("comparison manifest schema is not the supported run-validation schema")
    outcome = (report.get("validation") or {}).get("outcome")
    comparable = ((report.get("validation") or {}).get("comparability") or {}).get("comparable")
    if outcome not in known or outcome == "not_comparable" or comparable is not True:
        fail(f"outcome {outcome} comparable {comparable}")
    if compared.get("outcome") != outcome or compared.get("comparable") is not True:
        fail("comparison manifest does not match validation.json")
    if pair.get("comparison", {}).get("outcome") != outcome or pair.get("comparison", {}).get("comparable") is not True:
        fail("pair manifest comparison does not match validation.json")
    before_sha = sha(os.path.join(root, "before", "analysis.json"))
    after_sha = sha(os.path.join(root, "after", "analysis.json"))
    for label, digest in (("before", before_sha), ("after", after_sha)):
        if pair.get(label, {}).get("analysis_sha256") != digest:
            fail(f"pair manifest {label} SHA-256 does not match analysis.json")
        if compared.get(label, {}).get("analysis_sha256") != digest:
            fail(f"comparison manifest {label} SHA-256 does not match analysis.json")
        if pair.get("comparison", {}).get(f"{label}_analysis_sha256") != digest:
            fail(f"pair comparison {label} SHA-256 does not match analysis.json")
    docs = {
        "before": load(os.path.join(root, "before", "analysis.json")),
        "after": load(os.path.join(root, "after", "analysis.json")),
    }
    found = []
    pointers(report, found)
    if not found:
        fail("validation.json has no evidence pointers")
    for ref in found:
        side = ref.get("side")
        if side not in docs:
            fail(f"pointer side {side} is not before or after")
        resolved = resolve(docs[side], ref.get("pointer"))
        pointer = ref.get("pointer")
        if pointer.endswith("/requests") and (not isinstance(resolved, dict) or "requests" not in resolved):
            fail(f"{side} {pointer} did not resolve to a requests object")
        if pointer.endswith("/p95") and (not isinstance(resolved, dict) or "seconds" not in resolved):
            fail(f"{side} {pointer} did not resolve to a p95 object")
    reservations = []
    for dirpath, _, names in os.walk(root):
        reservations.extend(name for name in names if name.startswith(".ruptiq-"))
    if reservations:
        fail("reservations still present: " + ", ".join(reservations))
    before_metrics = before["workload"]["result"]["metrics"]
    after_metrics = after["workload"]["result"]["metrics"]
    print("before_status", before["status"])
    print("after_status", after["status"])
    print("before_requests", before_metrics["http_requests"])
    print("after_requests", after_metrics["http_requests"])
    print("before_telemetry", before["telemetry"]["completeness"])
    print("after_telemetry", after["telemetry"]["completeness"])
    print("outcome", outcome)
    print("comparable", "true")
    print("evidence_pointers", len(found))
elif mode == "cpu-self-test":
    cpu_self_test()
else:
    fail(f"unknown verify mode {mode}")
PY
}

if [[ "${1:-}" == "self-check" ]]; then
  self_check
  exit 0
fi

pair_args=(
  -pair-dir "$PAIR_DIR"
  -url "http://127.0.0.1:${PORT}/work"
  -metrics-url "http://127.0.0.1:${PORT}/metrics"
  -vus 2
  -interval 1s
  -k6 "$K6_BIN"
  -requests-metric regression_lab_http_requests_total
  -duration-metric regression_lab_http_request_duration_seconds
)

STAGE="baseline-lab"
start_lab 0s
STAGE="before"
run_pair -stage before "${pair_args[@]}" -duration 12s
require_status 0
run_verify before
assert_no_reservations
BEFORE_SNAP="$(snapshot_before)"
stop_owned "$LAB_PID"
LAB_PID=""
wait_port_free

STAGE="mismatch"
run_pair -stage after "${pair_args[@]}" -duration 5s
require_status 2
if [[ -e "$PAIR_DIR/after" ]]; then
  log "mismatched after created a candidate directory" >&2
  exit 1
fi
if [[ "$(snapshot_before)" != "$BEFORE_SNAP" ]]; then
  log "mismatched after changed the baseline" >&2
  exit 1
fi

STAGE="candidate-lab"
start_lab 1h
STAGE="after"
run_pair -stage after "${pair_args[@]}" -duration 12s
require_status 0
run_verify after
assert_no_reservations
if [[ "$(snapshot_before)" != "$BEFORE_SNAP" ]]; then
  log "valid after changed the baseline" >&2
  exit 1
fi
stop_owned "$LAB_PID"
LAB_PID=""
wait_port_free
if port_is_free; then
  :
else
  log "lab still owns the port before compare" >&2
  exit 1
fi

STAGE="compare"
run_pair -stage compare -pair-dir "$PAIR_DIR"
require_status 0
run_verify compare >/dev/null
assert_no_reservations

STAGE="overwrite"
cmp_before="$(sha256sum "$PAIR_DIR/comparison/validation.json" "$PAIR_DIR/comparison/manifest.json")"
run_pair -stage compare -pair-dir "$PAIR_DIR"
require_status 2
cmp_after="$(sha256sum "$PAIR_DIR/comparison/validation.json" "$PAIR_DIR/comparison/manifest.json")"
if [[ "$cmp_before" != "$cmp_after" ]]; then
  log "second compare changed the saved comparison" >&2
  exit 1
fi

STAGE="summary"
log "guided-workflow-smoke passed"
run_verify compare
assert_no_reservations
log "cleanup will remove the temporary pair directory and no lab process remains"
