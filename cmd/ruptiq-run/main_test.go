package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RajK158/ruptiq/internal/workload"
	"github.com/RajK158/ruptiq/internal/workload/k6"
)

// fakeRunner records the run it was asked for and returns canned output.
type fakeRunner struct {
	result   workload.Result
	err      error
	called   bool
	spec     workload.Spec
	deadline time.Duration
}

func (f *fakeRunner) Run(ctx context.Context, spec workload.Spec) (workload.Result, error) {
	f.called = true
	f.spec = spec
	if d, ok := ctx.Deadline(); ok {
		f.deadline = time.Until(d)
	}
	return f.result, f.err
}

func healthyResult() workload.Result {
	return workload.Result{
		Runner: "k6", RunnerVersion: "2.3.0",
		TargetURL: "http://127.0.0.1:8082/inventory/LAPTOP-001",
		VUs:       2, ConfiguredDurationSeconds: 5,
		StartedAt:  time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC),
		FinishedAt: time.Date(2026, 11, 6, 10, 0, 5, 0, time.UTC),
		Metrics: workload.Metrics{
			DurationSeconds: 5.0004, HTTPRequests: 20000, RequestsPerSecond: 20000 / 5.0004,
			LatencyMs: workload.Latency{Avg: 0.42, Min: 0, P50: 0.53, P90: 0.62, P95: 0.68, Max: 8.57},
		},
	}
}

type outcome struct {
	code           int
	stdout, stderr string
	runner         *fakeRunner
	k6Path         string
}

func runCLI(t *testing.T, f *fakeRunner, args ...string) outcome {
	t.Helper()
	var stdout, stderr bytes.Buffer
	o := outcome{runner: f}
	o.code = run(context.Background(), args, &stdout, &stderr, func(path string) workload.Runner {
		o.k6Path = path
		return f
	})
	o.stdout, o.stderr = stdout.String(), stderr.String()
	return o
}

func readReport(t *testing.T, path string) workload.Report {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var r workload.Report
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatalf("results file is not valid JSON: %v\n%s", err, data)
	}
	return r
}

func TestUsageErrorsDoNotRun(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no URL", nil, "target URL is required"},
		{"bad scheme", []string{"-url", "ftp://example.com"}, "http:// or https://"},
		{"zero VUs", []string{"-url", "http://localhost/", "-vus", "0"}, "vus must be between"},
		{"fractional duration", []string{"-url", "http://localhost/", "-duration", "1500ms"}, "whole number of seconds"},
		{"timeout not above duration", []string{"-url", "http://localhost/", "-duration", "10s", "-timeout", "10s"}, "must be longer than duration"},
		{"failure rate out of range", []string{"-url", "http://localhost/", "-max-failure-rate", "2"}, "between 0 and 1"},
		{"empty k6 path", []string{"-url", "http://localhost/", "-k6", ""}, "k6 path must not be empty"},
		{"unknown flag", []string{"-url", "http://localhost/", "-rps", "5"}, "flag provided but not defined"},
		{"stray argument", []string{"-url", "http://localhost/", "extra"}, "unexpected arguments"},
		{"several problems", []string{"-vus", "0", "-duration", "0s"}, "target URL is required; vus must be between"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeRunner{}
			o := runCLI(t, f, tc.args...)
			if o.code != exitUsage || f.called {
				t.Fatalf("code = %d, runner called = %v; want %d and not called", o.code, f.called, exitUsage)
			}
			if !strings.Contains(o.stderr, tc.want) {
				t.Fatalf("stderr = %q, want it to contain %q", o.stderr, tc.want)
			}
		})
	}
}

func TestHelp(t *testing.T) {
	o := runCLI(t, &fakeRunner{}, "-h")
	if o.code != exitUsage || !strings.Contains(o.stderr, "Usage: ruptiq-run -url URL") {
		t.Fatalf("code = %d stderr = %q", o.code, o.stderr)
	}
}

func TestDefaults(t *testing.T) {
	t.Setenv(k6PathEnv, "")
	f := &fakeRunner{result: healthyResult()}
	o := runCLI(t, f, "-url", "http://127.0.0.1:8082/inventory/LAPTOP-001")
	if o.code != exitPassed {
		t.Fatalf("code = %d, stderr = %s", o.code, o.stderr)
	}
	want := workload.Spec{TargetURL: "http://127.0.0.1:8082/inventory/LAPTOP-001", VUs: 2, Duration: 5 * time.Second}
	if f.spec != want {
		t.Errorf("spec = %+v, want %+v", f.spec, want)
	}
	if o.k6Path != k6.DefaultExecutable {
		t.Errorf("k6 path = %q, want %q", o.k6Path, k6.DefaultExecutable)
	}
	if f.deadline <= 60*time.Second || f.deadline > 65*time.Second {
		t.Errorf("deadline in %v, want duration + 60s", f.deadline)
	}
}

func TestFlagsAndEnvironment(t *testing.T) {
	t.Setenv(k6PathEnv, `C:\tools\k6.exe`)
	f := &fakeRunner{result: healthyResult()}
	o := runCLI(t, f, "-url", "https://example.com/", "-vus", "10", "-duration", "1m", "-timeout", "90s")
	if o.code != exitPassed {
		t.Fatalf("code = %d, stderr = %s", o.code, o.stderr)
	}
	if f.spec.VUs != 10 || f.spec.Duration != time.Minute {
		t.Errorf("spec = %+v", f.spec)
	}
	if o.k6Path != `C:\tools\k6.exe` {
		t.Errorf("k6 path = %q, want it from %s", o.k6Path, k6PathEnv)
	}
	if f.deadline <= 85*time.Second || f.deadline > 90*time.Second {
		t.Errorf("deadline in %v, want 90s", f.deadline)
	}

	o = runCLI(t, &fakeRunner{result: healthyResult()}, "-url", "https://example.com/", "-k6", "/opt/k6")
	if o.k6Path != "/opt/k6" {
		t.Errorf("-k6 flag ignored: %q", o.k6Path)
	}
}

func TestPassedRunReportAndFile(t *testing.T) {
	out := filepath.Join(t.TempDir(), "run.json")
	o := runCLI(t, &fakeRunner{result: healthyResult()},
		"-url", "http://user:s3cret@127.0.0.1:8082/inventory/LAPTOP-001", "-out", out)
	if o.code != exitPassed {
		t.Fatalf("code = %d, stderr = %s", o.code, o.stderr)
	}
	for _, want := range []string{
		"Ruptiq load test report (k6 v2.3.0)",
		"HTTP requests:     20000 (3999.7 per second)",
		"Failed requests:   0 (0.00%)",
		"avg 0.420   p50 0.530   p95 0.680",
		"5.00s measured",
		"PASSED",
		"Results written to " + out,
	} {
		if !strings.Contains(o.stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, o.stdout)
		}
	}
	if strings.Contains(o.stdout+o.stderr, "s3cret") {
		t.Errorf("output leaks the password:\n%s%s", o.stdout, o.stderr)
	}

	r := readReport(t, out)
	if r.SchemaVersion != workload.ReportSchemaVersion || r.Error != "" || r.Run == nil || !r.Verdict.Passed {
		t.Fatalf("report = %+v", r)
	}
	if r.Run.Metrics != healthyResult().Metrics {
		t.Errorf("report metrics = %+v", r.Run.Metrics)
	}
	entries, _ := os.ReadDir(filepath.Dir(out))
	if len(entries) != 1 {
		t.Errorf("results directory has %d entries, want only the results file", len(entries))
	}
}

func TestSecretsStayOutOfOutputAndResults(t *testing.T) {
	for _, tc := range []struct {
		name, url, shown string
		secrets          []string
	}{
		{"user and password", "http://alice:pw-SECRET1@127.0.0.1:8082/inventory/LAPTOP-001",
			"http://xxxxx@127.0.0.1:8082/inventory/LAPTOP-001", []string{"alice", "pw-SECRET1"}},
		{"api_key query", "http://127.0.0.1:8082/inventory/LAPTOP-001?api_key=KEY-SECRET2",
			"http://127.0.0.1:8082/inventory/LAPTOP-001?xxxxx", []string{"KEY-SECRET2"}},
		{"token query", "http://127.0.0.1:8082/healthz?token=TOK-SECRET3",
			"http://127.0.0.1:8082/healthz?xxxxx", []string{"TOK-SECRET3"}},
		{"several query parameters", "https://example.com/p?q=1&api_key=KEY-SECRET2&token=TOK-SECRET3",
			"https://example.com/p?xxxxx", []string{"KEY-SECRET2", "TOK-SECRET3"}},
		{"ordinary URL", "http://127.0.0.1:8082/inventory/LAPTOP-001",
			"http://127.0.0.1:8082/inventory/LAPTOP-001", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The fake runner is careless on purpose: it returns the
			// original URL, and an error that quotes it.
			completed := healthyResult()
			completed.TargetURL = tc.url
			failed := fmt.Errorf("k6 exited with status 1: Get %q: connection refused", tc.url)

			for _, f := range []*fakeRunner{{result: completed}, {err: failed}} {
				out := filepath.Join(t.TempDir(), "run.json")
				o := runCLI(t, f, "-url", tc.url, "-out", out)
				if f.spec.TargetURL != tc.url {
					t.Errorf("runner got %q, want the original URL", f.spec.TargetURL)
				}
				data, err := os.ReadFile(out)
				if err != nil {
					t.Fatal(err)
				}
				for _, s := range tc.secrets {
					for where, text := range map[string]string{"stdout": o.stdout, "stderr": o.stderr, "results file": string(data)} {
						if strings.Contains(text, s) {
							t.Errorf("%s leaks %q:\n%s", where, s, text)
						}
					}
				}
				if !strings.Contains(o.stdout, "against "+tc.shown) {
					t.Errorf("stdout does not show the redacted URL %q:\n%s", tc.shown, o.stdout)
				}
				r := readReport(t, out)
				if f.err == nil && (r.Run == nil || r.Run.TargetURL != tc.shown || !strings.Contains(o.stdout, "Target:            "+tc.shown)) {
					t.Errorf("report target = %+v, want %q", r.Run, tc.shown)
				}
				if f.err != nil && !strings.Contains(r.Error, "connection refused") {
					t.Errorf("report error lost the cause: %q", r.Error)
				}
			}
		})
	}
}

func TestFailedVerdict(t *testing.T) {
	res := healthyResult()
	res.Metrics.FailedRequests, res.Metrics.FailureRate = 20000, 1
	out := filepath.Join(t.TempDir(), "run.json")
	o := runCLI(t, &fakeRunner{result: res}, "-url", "http://127.0.0.1:1/", "-out", out)
	if o.code != exitFailed {
		t.Fatalf("code = %d, want %d", o.code, exitFailed)
	}
	if !strings.Contains(o.stdout, "FAILED") || !strings.Contains(o.stdout, "failure rate 100.00% exceeds the allowed 0.00%") {
		t.Errorf("stdout:\n%s", o.stdout)
	}
	if r := readReport(t, out); r.Verdict.Passed || r.Run == nil {
		t.Errorf("report = %+v, want a failed verdict with the run", r)
	}

	o = runCLI(t, &fakeRunner{result: res}, "-url", "http://127.0.0.1:1/", "-max-failure-rate", "1")
	if o.code != exitPassed {
		t.Errorf("with -max-failure-rate 1: code = %d, want %d", o.code, exitPassed)
	}
}

func TestRunErrorReplacesOldResults(t *testing.T) {
	out := filepath.Join(t.TempDir(), "run.json")
	if code := runCLI(t, &fakeRunner{result: healthyResult()}, "-url", "http://localhost/", "-out", out).code; code != exitPassed {
		t.Fatalf("first run code = %d", code)
	}

	runErr := fmt.Errorf("wrapped: %w", k6.ErrNotInstalled)
	o := runCLI(t, &fakeRunner{err: runErr}, "-url", "http://localhost/", "-out", out)
	if o.code != exitRunError {
		t.Fatalf("code = %d, want %d", o.code, exitRunError)
	}
	if !strings.Contains(o.stderr, "k6 executable not found") || strings.Contains(o.stdout, "PASSED") {
		t.Errorf("stdout = %q stderr = %q", o.stdout, o.stderr)
	}
	r := readReport(t, out)
	if r.Run != nil || r.Verdict.Passed || !strings.Contains(r.Error, "k6 executable not found") {
		t.Fatalf("report = %+v, want the error and a failed verdict, not the old run", r)
	}
}

func TestTimeoutIsExplained(t *testing.T) {
	o := runCLI(t, &fakeRunner{err: fmt.Errorf("k6 run stopped: %w", context.DeadlineExceeded)},
		"-url", "http://localhost/", "-duration", "2s", "-timeout", "3s")
	if o.code != exitRunError || !strings.Contains(o.stderr, "run exceeded the 3s timeout") {
		t.Fatalf("code = %d stderr = %q", o.code, o.stderr)
	}
}

func TestUnwritableResultsFile(t *testing.T) {
	out := filepath.Join(t.TempDir(), "missing-dir", "run.json")
	o := runCLI(t, &fakeRunner{result: healthyResult()}, "-url", "http://localhost/", "-out", out)
	if o.code != exitRunError || !strings.Contains(o.stderr, "writing results") {
		t.Fatalf("code = %d stderr = %q", o.code, o.stderr)
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("results file unexpectedly exists")
	}
}
