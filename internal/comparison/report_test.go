package comparison

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RajK158/ruptiq/internal/workload"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func mustDecode(t *testing.T, name string) Run {
	t.Helper()
	run, err := DecodeReport(fixture(t, name))
	if err != nil {
		t.Fatalf("DecodeReport(%s) = %v", name, err)
	}
	return run
}

func TestDecodeReport(t *testing.T) {
	run := mustDecode(t, "baseline.json")
	want := Run{
		Result: workload.Result{
			Runner: "k6", RunnerVersion: "2.3.0",
			TargetURL: "http://127.0.0.1:8082/inventory/LAPTOP-001",
			VUs:       2, ConfiguredDurationSeconds: 5,
			StartedAt:  time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC),
			FinishedAt: time.Date(2026, 11, 6, 10, 0, 5, 250_000_000, time.UTC),
			Metrics: workload.Metrics{
				DurationSeconds: 5, HTTPRequests: 10000, RequestsPerSecond: 2000,
				LatencyMs: workload.Latency{Avg: 2, Min: 0.5, P50: 1.5, P90: 3, P95: 4, Max: 20},
			},
		},
		Verdict: workload.Verdict{Passed: true},
	}
	if !run.Result.StartedAt.Equal(want.Result.StartedAt) || !run.Result.FinishedAt.Equal(want.Result.FinishedAt) {
		t.Errorf("times = %v, %v", run.Result.StartedAt, run.Result.FinishedAt)
	}
	run.Result.StartedAt, run.Result.FinishedAt = want.Result.StartedAt, want.Result.FinishedAt
	if run.Result != want.Result || run.Verdict.Passed != want.Verdict.Passed || run.Verdict.MaxFailureRate != 0 {
		t.Errorf("DecodeReport =\n%+v\nwant\n%+v", run, want)
	}
}

func TestDecodeReportAcceptsCompletedFailedRun(t *testing.T) {
	run := mustDecode(t, "candidate_regression.json")
	if run.Verdict.Passed || len(run.Verdict.Reasons) != 1 || run.Result.Metrics.FailedRequests != 80 {
		t.Fatalf("run = %+v", run)
	}
}

func TestDecodeReportAcceptsRealRuptiqRunOutput(t *testing.T) {
	// Written by ruptiq-run with k6 v2.3.0 against the Inventory Service.
	run := mustDecode(t, "real_inventory_k6_2.3.0.json")
	if run.Result.Runner != "k6" || run.Result.Metrics.HTTPRequests == 0 || !run.Verdict.Passed {
		t.Fatalf("run = %+v", run)
	}
}

func TestDecodeReportRejectsIncompleteRun(t *testing.T) {
	_, err := DecodeReport(fixture(t, "run_error.json"))
	if !errors.Is(err, ErrIncompleteRun) || !strings.Contains(err.Error(), "k6 executable not found") {
		t.Fatalf("err = %v, want ErrIncompleteRun with the recorded error", err)
	}
}

func TestDecodeReportSchemaVersion(t *testing.T) {
	for _, tc := range []struct {
		version any
		want    error
	}{
		{2, ErrUnsupportedSchema},
		{0, ErrUnsupportedSchema},
		{-1, ErrUnsupportedSchema},
		{nil, ErrInvalidReport},
		{"1", ErrInvalidReport},
		{1.5, ErrInvalidReport},
	} {
		data := mutate(t, "baseline.json", func(d map[string]any) { d["schema_version"] = tc.version })
		if _, err := DecodeReport(data); !errors.Is(err, tc.want) {
			t.Errorf("schema_version %v: err = %v, want %v", tc.version, err, tc.want)
		}
	}
	data := mutate(t, "baseline.json", func(d map[string]any) { delete(d, "schema_version") })
	if _, err := DecodeReport(data); !errors.Is(err, ErrInvalidReport) || !strings.Contains(err.Error(), "schema_version is missing") {
		t.Errorf("missing schema_version: err = %v", err)
	}
}

// mutate decodes a fixture, applies edit, and re-encodes it.
func mutate(t *testing.T, name string, edit func(doc map[string]any)) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(fixture(t, name), &doc); err != nil {
		t.Fatal(err)
	}
	edit(doc)
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func obj(d map[string]any, path ...string) map[string]any {
	for _, p := range path {
		d = d[p].(map[string]any)
	}
	return d
}

func TestDecodeReportRejectsInvalidReports(t *testing.T) {
	type edit = func(map[string]any)
	metrics := func(d map[string]any) map[string]any { return obj(d, "run", "metrics") }
	latency := func(d map[string]any) map[string]any { return obj(d, "run", "metrics", "latency_ms") }
	for _, tc := range []struct {
		name string
		edit edit
		want string
	}{
		{"run missing", func(d map[string]any) { delete(d, "run") }, "run is missing"},
		{"verdict missing", func(d map[string]any) { delete(d, "verdict") }, "verdict is missing"},
		{"run and error", func(d map[string]any) { d["error"] = "boom" }, "both a run and an error"},
		{"unknown field", func(d map[string]any) { d["comment"] = "x" }, "unknown field"},
		{"metrics missing", func(d map[string]any) { delete(obj(d, "run"), "metrics") }, "run.metrics is missing"},
		{"latency missing", func(d map[string]any) { delete(metrics(d), "latency_ms") }, "metrics.latency_ms is missing"},
		{"p95 missing", func(d map[string]any) { delete(latency(d), "p95") }, "metrics.latency_ms.p95 is missing"},
		{"p95 null", func(d map[string]any) { latency(d)["p95"] = nil }, "metrics.latency_ms.p95 is missing"},
		{"p95 string", func(d map[string]any) { latency(d)["p95"] = "4" }, "not valid report JSON"},
		{"negative latency", func(d map[string]any) { latency(d)["min"] = -0.5 }, "metrics.latency_ms.min is negative"},
		{"latency out of order", func(d map[string]any) { latency(d)["p95"] = 2.5 }, "out of order"},
		{"average above max", func(d map[string]any) { latency(d)["avg"] = 25 }, "avg is outside min..max"},
		{"requests missing", func(d map[string]any) { delete(metrics(d), "http_requests") }, "metrics.http_requests is missing"},
		{"zero requests", func(d map[string]any) { metrics(d)["http_requests"] = 0 }, "must be at least 1"},
		{"fractional requests", func(d map[string]any) { metrics(d)["http_requests"] = 10000.5 }, "not valid report JSON"},
		{"failed missing", func(d map[string]any) { delete(metrics(d), "failed_requests") }, "metrics.failed_requests is missing"},
		{"failed exceeds total", func(d map[string]any) {
			metrics(d)["failed_requests"] = 10001
			metrics(d)["failure_rate"] = 1
			obj(d, "verdict")["passed"] = false
		}, "exceeds metrics.http_requests"},
		{"failure rate inconsistent", func(d map[string]any) { metrics(d)["failure_rate"] = 0.5 }, "does not match failed_requests / http_requests"},
		{"failure rate above 1", func(d map[string]any) { metrics(d)["failure_rate"] = 1.5 }, "at most 1"},
		{"requests per second inconsistent", func(d map[string]any) { metrics(d)["requests_per_second"] = 2100 }, "does not match http_requests / duration_seconds"},
		{"measured duration zero", func(d map[string]any) { metrics(d)["duration_seconds"] = 0 }, "must be greater than 0"},
		{"runner missing", func(d map[string]any) { delete(obj(d, "run"), "runner") }, "run.runner is missing"},
		{"target missing", func(d map[string]any) { delete(obj(d, "run"), "target_url") }, "run.target_url is missing"},
		{"target not http", func(d map[string]any) { obj(d, "run")["target_url"] = "ftp://example.com/" }, "not an absolute http or https URL"},
		{"target relative", func(d map[string]any) { obj(d, "run")["target_url"] = "/inventory" }, "not an absolute http or https URL"},
		{"vus missing", func(d map[string]any) { delete(obj(d, "run"), "vus") }, "run.vus is missing"},
		{"vus zero", func(d map[string]any) { obj(d, "run")["vus"] = 0 }, "run.vus must be between"},
		{"configured duration missing", func(d map[string]any) { delete(obj(d, "run"), "configured_duration_seconds") }, "configured_duration_seconds is missing"},
		{"configured duration fractional", func(d map[string]any) { obj(d, "run")["configured_duration_seconds"] = 2.5 }, "must be a whole number"},
		{"started missing", func(d map[string]any) { delete(obj(d, "run"), "started_at") }, "run.started_at is missing"},
		{"finished before started", func(d map[string]any) { obj(d, "run")["finished_at"] = "2026-11-06T09:59:00Z" }, "before run.started_at"},
		{"verdict passed missing", func(d map[string]any) { delete(obj(d, "verdict"), "passed") }, "verdict.passed is missing"},
		{"verdict max rate missing", func(d map[string]any) { delete(obj(d, "verdict"), "max_failure_rate") }, "verdict.max_failure_rate is missing"},
		{"verdict max rate invalid", func(d map[string]any) { obj(d, "verdict")["max_failure_rate"] = 2 }, "between 0 and 1"},
		{"verdict contradicts metrics", func(d map[string]any) { obj(d, "verdict")["passed"] = false }, "verdict.passed is false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeReport(mutate(t, "baseline.json", tc.edit))
			if !errors.Is(err, ErrInvalidReport) {
				t.Fatalf("err = %v, want ErrInvalidReport", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestDecodeReportRejectsMalformedJSON(t *testing.T) {
	good := fixture(t, "baseline.json")
	for name, data := range map[string][]byte{
		"empty":         nil,
		"not JSON":      []byte("schema_version: 1"),
		"truncated":     good[:len(good)/2],
		"array":         []byte(`[{"schema_version":1}]`),
		"trailing data": append(append([]byte{}, good...), []byte(`{"x":1}`)...),
		"too large":     append([]byte(`{"schema_version":1,"error":"`), []byte(strings.Repeat("a", MaxReportBytes))...),
	} {
		if _, err := DecodeReport(data); !errors.Is(err, ErrInvalidReport) {
			t.Errorf("%s: err = %v, want ErrInvalidReport", name, err)
		}
	}
}

func TestDecodeReportListsEveryProblem(t *testing.T) {
	data := mutate(t, "baseline.json", func(d map[string]any) {
		delete(obj(d, "run"), "runner")
		obj(d, "run")["vus"] = 0
		delete(obj(d, "run", "metrics", "latency_ms"), "avg")
	})
	_, err := DecodeReport(data)
	for _, want := range []string{"run.runner is missing", "run.vus must be between", "metrics.latency_ms.avg is missing"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to contain %q", err, want)
		}
	}
}
