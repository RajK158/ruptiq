package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RajK158/ruptiq/internal/experiment"
	"github.com/RajK158/ruptiq/internal/workload"
	"github.com/RajK158/ruptiq/internal/workload/k6"
)

// metricsServer serves a counter that goes up with every scrape, or fails
// with status code when it is set.
type metricsServer struct {
	*httptest.Server
	scrapes atomic.Int32
	status  atomic.Int32
	user    atomic.Value
}

func newMetricsServer(t *testing.T) *metricsServer {
	t.Helper()
	m := &metricsServer{}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := m.scrapes.Add(1)
		if u, _, ok := r.BasicAuth(); ok {
			m.user.Store(u)
		}
		if code := m.status.Load(); code != 0 {
			http.Error(w, "unavailable", int(code))
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		fmt.Fprintf(w, "# HELP app_requests_total Requests.\n# TYPE app_requests_total counter\napp_requests_total{code=\"200\"} %d\n", n*10)
	}))
	t.Cleanup(m.Close)
	return m
}

// fakeRunner returns its result once the metrics server has been scraped
// at least scrapes times, so samples are taken while it "runs".
type fakeRunner struct {
	metrics *metricsServer
	scrapes int32
	result  workload.Result
	err     error
	block   bool
	// untilIdle makes Run return only once scraping has stopped for a while
	// (several intervals), as it does when the retention budget is reached.
	untilIdle bool
	called    atomic.Bool
}

func (f *fakeRunner) Run(ctx context.Context, spec workload.Spec) (workload.Result, error) {
	f.called.Store(true)
	if f.block {
		<-ctx.Done()
		return workload.Result{}, fmt.Errorf("k6 run stopped: %w", ctx.Err())
	}
	for last, idle := int32(-1), 0; f.untilIdle && idle < 5; {
		select {
		case <-ctx.Done():
			return workload.Result{}, fmt.Errorf("k6 run stopped: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
		if n := f.metrics.scrapes.Load(); n == last {
			idle++
		} else {
			last, idle = n, 0
		}
	}
	for f.metrics != nil && f.metrics.scrapes.Load() < f.scrapes {
		select {
		case <-ctx.Done():
			return workload.Result{}, fmt.Errorf("k6 run stopped: %w", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if f.err != nil {
		return workload.Result{}, f.err
	}
	r := f.result
	r.TargetURL = spec.TargetURL
	return r, nil
}

func healthyResult() workload.Result {
	return workload.Result{
		Runner: "k6", RunnerVersion: "2.3.0", VUs: 2, ConfiguredDurationSeconds: 1,
		StartedAt:  time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC),
		FinishedAt: time.Date(2026, 11, 6, 10, 0, 1, 0, time.UTC),
		Metrics: workload.Metrics{
			DurationSeconds: 1.0004, HTTPRequests: 4000, RequestsPerSecond: 4000 / 1.0004,
			LatencyMs: workload.Latency{Avg: 0.42, Min: 0.1, P50: 0.53, P90: 0.62, P95: 0.68, Max: 8.57},
		},
	}
}

type outcome struct {
	code           int
	stdout, stderr string
	k6Path         string
}

func runCLI(t *testing.T, ctx context.Context, f *fakeRunner, args ...string) outcome {
	t.Helper()
	var stdout, stderr bytes.Buffer
	var o outcome
	o.code = run(ctx, args, &stdout, &stderr, func(path string) workload.Runner {
		o.k6Path = path
		return f
	})
	o.stdout, o.stderr = stdout.String(), stderr.String()
	return o
}

func args(target, metrics, out string, extra ...string) []string {
	a := []string{"-url", target, "-metrics-url", metrics, "-duration", "1s", "-interval", "100ms"}
	if out != "" {
		a = append(a, "-out", out)
	}
	return append(a, extra...)
}

func readReport(t *testing.T, path string) experiment.Report {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var r experiment.Report
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatalf("report is not valid JSON: %v\n%s", err, data)
	}
	return r
}

const target = "http://127.0.0.1:8082/inventory/LAPTOP-001"

func TestPassedExperiment(t *testing.T) {
	m := newMetricsServer(t)
	out := filepath.Join(t.TempDir(), "experiment.json")
	f := &fakeRunner{metrics: m, scrapes: 4, result: healthyResult()}
	o := runCLI(t, context.Background(), f, args(target, m.URL+"/metrics", out)...)

	if o.code != exitPassed {
		t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", o.code, exitPassed, o.stdout, o.stderr)
	}
	for _, want := range []string{"Running 2 virtual users for 1s against " + target, "every 100ms",
		"Status:          PASSED", "Telemetry:       complete:", "Report written to " + out} {
		if !strings.Contains(o.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, o.stdout)
		}
	}
	if o.stderr != "" {
		t.Errorf("stderr = %q, want nothing", o.stderr)
	}
	r := readReport(t, out)
	tel := r.Telemetry
	if r.Status != experiment.StatusPassed || tel.Completeness != experiment.Complete || tel.Succeeded < 4 {
		t.Fatalf("report status %s, telemetry %s with %d samples", r.Status, tel.Completeness, tel.Succeeded)
	}
	// Real scrapes through the Prometheus reader: the counter rises.
	var last float64
	for i, s := range tel.Samples {
		v := float64(*s.Families[0].Series[0].Value)
		if i > 0 && v <= last {
			t.Errorf("sample #%d counter %v not above %v", s.Sequence, v, last)
		}
		last = v
	}
	if o.k6Path != k6.DefaultExecutable && o.k6Path != os.Getenv(k6PathEnv) {
		t.Errorf("k6 path %q", o.k6Path)
	}
	if r.Config.MaxTelemetryBytes != experiment.DefaultMaxTelemetryBytes || tel.RetainedBytes <= 0 {
		t.Errorf("budget %d, %d bytes kept", r.Config.MaxTelemetryBytes, tel.RetainedBytes)
	}
}

func TestRetentionBudgetReached(t *testing.T) {
	m := newMetricsServer(t)
	out := filepath.Join(t.TempDir(), "experiment.json")
	// Each sample of this server is a few hundred bytes, so 1 KiB holds only
	// the first few. Sampling at 100ms then stops; the runner returns once it
	// has seen no scrape for 500ms.
	f := &fakeRunner{metrics: m, untilIdle: true, result: healthyResult()}
	o := runCLI(t, context.Background(), f, args(target, m.URL+"/metrics", out, "-max-telemetry-bytes", "1024")...)
	if o.code != exitIncomplete {
		t.Fatalf("exit %d, want %d\n%s%s", o.code, exitIncomplete, o.stdout, o.stderr)
	}
	if !strings.Contains(o.stdout, "telemetry retention budget of 1024 bytes reached") || strings.Contains(o.stdout, "PASSED") {
		t.Errorf("stdout does not explain the budget, or claims a pass:\n%s", o.stdout)
	}
	r := readReport(t, out)
	tel := r.Telemetry
	if !tel.RetentionLimitReached || len(tel.NotRetained) != 1 || len(tel.Samples) == 0 || tel.RetainedBytes > 1024 {
		t.Errorf("telemetry: limit %v, %d not retained, %d samples, %d bytes", tel.RetentionLimitReached, len(tel.NotRetained), len(tel.Samples), tel.RetainedBytes)
	}
	if r.Workload.Result == nil || !r.Workload.Verdict.Passed {
		t.Errorf("workload = %+v, want a passed result", r.Workload)
	}
	if got := int(m.scrapes.Load()); got != tel.Attempts {
		t.Errorf("server saw %d scrapes, report has %d attempts: no scrapes after the budget was reached", got, tel.Attempts)
	}
}

func TestExitCodes(t *testing.T) {
	failing := healthyResult()
	failing.Metrics.FailedRequests, failing.Metrics.FailureRate = 1000, 0.25
	tests := []struct {
		name       string
		runner     *fakeRunner
		status     int // metrics server error status, 0 for healthy
		code       int
		wantStatus experiment.Status
		wantStderr string
	}{
		{"HTTP failures", &fakeRunner{scrapes: 3, result: failing}, 0, exitFailed, experiment.StatusFailed, "experiment failed"},
		{"k6 missing", &fakeRunner{err: fmt.Errorf("%w: 'k6'", k6.ErrNotInstalled)}, 0, exitError, experiment.StatusError, "experiment error"},
		{"k6 exit", &fakeRunner{scrapes: 3, err: &k6.ExitError{Code: 107, Output: "script error"}}, 0, exitError, experiment.StatusError, "experiment error"},
		{"telemetry unavailable", &fakeRunner{scrapes: 3, result: healthyResult()}, http.StatusServiceUnavailable, exitIncomplete, experiment.StatusIncomplete, "experiment incomplete"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newMetricsServer(t)
			m.status.Store(int32(tt.status))
			tt.runner.metrics = m
			out := filepath.Join(t.TempDir(), "experiment.json")
			o := runCLI(t, context.Background(), tt.runner, args(target, m.URL+"/metrics", out)...)
			if o.code != tt.code {
				t.Fatalf("exit %d, want %d\nstdout:\n%s\nstderr:\n%s", o.code, tt.code, o.stdout, o.stderr)
			}
			if !strings.Contains(o.stderr, tt.wantStderr) {
				t.Errorf("stderr = %q, want %q", o.stderr, tt.wantStderr)
			}
			if strings.Contains(o.stdout, "PASSED") {
				t.Errorf("stdout claims PASSED:\n%s", o.stdout)
			}
			if r := readReport(t, out); r.Status != tt.wantStatus {
				t.Errorf("report status %s, want %s", r.Status, tt.wantStatus)
			}
		})
	}
}

func TestTelemetryUnavailableRecordsErrors(t *testing.T) {
	m := newMetricsServer(t)
	m.Close() // nothing listens any more
	out := filepath.Join(t.TempDir(), "experiment.json")
	o := runCLI(t, context.Background(), &fakeRunner{result: healthyResult()}, args(target, m.URL+"/metrics", out)...)
	if o.code != exitIncomplete {
		t.Fatalf("exit %d, want %d\n%s%s", o.code, exitIncomplete, o.stdout, o.stderr)
	}
	r := readReport(t, out)
	if r.Telemetry.Completeness != experiment.None || len(r.Telemetry.Samples) != 0 || len(r.Telemetry.Errors) == 0 {
		t.Errorf("telemetry = %+v, want none with errors", r.Telemetry)
	}
	if !strings.Contains(r.Telemetry.Errors[0].Error, "metrics request failed") {
		t.Errorf("error = %q", r.Telemetry.Errors[0].Error)
	}
	if !strings.Contains(o.stdout, "Scrape error:") {
		t.Errorf("stdout does not show scrape errors:\n%s", o.stdout)
	}
}

func TestCancelledAndTimedOut(t *testing.T) {
	m := newMetricsServer(t)
	dir := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for m.scrapes.Load() < 2 {
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()
	out := filepath.Join(dir, "cancelled.json")
	o := runCLI(t, ctx, &fakeRunner{block: true}, args(target, m.URL+"/metrics", out)...)
	if o.code != exitStopped || !strings.Contains(o.stdout, "Status:          CANCELLED") {
		t.Errorf("cancelled: exit %d, want %d\n%s", o.code, exitStopped, o.stdout)
	}
	if r := readReport(t, out); r.Status != experiment.StatusCancelled {
		t.Errorf("cancelled: report status %s", r.Status)
	}

	out = filepath.Join(dir, "timeout.json")
	o = runCLI(t, context.Background(), &fakeRunner{block: true}, args(target, m.URL+"/metrics", out, "-timeout", "1200ms")...)
	if o.code != exitStopped || !strings.Contains(o.stdout, "Status:          TIMED OUT") || !strings.Contains(o.stderr, "experiment timed out") {
		t.Errorf("timeout: exit %d, want %d\n%s", o.code, exitStopped, o.stdout)
	}
	if r := readReport(t, out); r.Status != experiment.StatusTimedOut || r.Config.TimeoutSeconds != 1.2 {
		t.Errorf("timeout: report status %s, timeout %v", r.Status, r.Config.TimeoutSeconds)
	}
}

func TestFailureReplacesOldReport(t *testing.T) {
	m := newMetricsServer(t)
	out := filepath.Join(t.TempDir(), "experiment.json")
	if o := runCLI(t, context.Background(), &fakeRunner{metrics: m, scrapes: 3, result: healthyResult()}, args(target, m.URL+"/metrics", out)...); o.code != exitPassed {
		t.Fatalf("first run: exit %d\n%s%s", o.code, o.stdout, o.stderr)
	}
	o := runCLI(t, context.Background(), &fakeRunner{err: errors.New("k6 exited with status 1")}, args(target, m.URL+"/metrics", out)...)
	if o.code != exitError {
		t.Fatalf("second run: exit %d", o.code)
	}
	r := readReport(t, out)
	if r.Status != experiment.StatusError || r.Workload.Result != nil || r.Workload.Error != "k6 exited with status 1" {
		t.Errorf("report after a failed run: %+v", r)
	}
	entries, err := os.ReadDir(filepath.Dir(out))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("directory has %d entries, want only the report (temporary files left behind?)", len(entries))
	}
}

func TestReportWriteFailure(t *testing.T) {
	m := newMetricsServer(t)
	out := filepath.Join(t.TempDir(), "missing-dir", "experiment.json")
	o := runCLI(t, context.Background(), &fakeRunner{metrics: m, scrapes: 3, result: healthyResult()}, args(target, m.URL+"/metrics", out)...)
	if o.code != exitError || !strings.Contains(o.stderr, "writing report") {
		t.Errorf("exit %d, stderr %q; want %d and a write error", o.code, o.stderr, exitError)
	}
}

func TestOutputRedactsCredentials(t *testing.T) {
	m := newMetricsServer(t)
	metricsURL := strings.Replace(m.URL, "http://", "http://scraper:scrape-pw@", 1) + "/metrics?token=metrics-token"
	targetURL := "http://loaduser:load-pw@127.0.0.1:8082/inventory/LAPTOP-001?api_key=target-key"
	secrets := []string{"scrape-pw", "metrics-token", "load-pw", "target-key", "scraper:", "loaduser:"}

	for _, tt := range []struct {
		name   string
		runner *fakeRunner
		status int
	}{
		{"success", &fakeRunner{scrapes: 3, result: healthyResult()}, 0},
		{"failures echo URLs", &fakeRunner{err: errors.New("k6 failed: GET " + targetURL)}, http.StatusForbidden},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.runner.metrics = m
			m.status.Store(int32(tt.status))
			out := filepath.Join(t.TempDir(), "experiment.json")
			o := runCLI(t, context.Background(), tt.runner, args(targetURL, metricsURL, out)...)
			data, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			for where, text := range map[string]string{"stdout": o.stdout, "stderr": o.stderr, "report": string(data)} {
				for _, s := range secrets {
					if strings.Contains(text, s) {
						t.Errorf("%s contains %q:\n%s", where, s, text)
					}
				}
			}
			if !strings.Contains(o.stdout, "/metrics?xxxxx") {
				t.Errorf("stdout does not show the redacted metrics URL:\n%s", o.stdout)
			}
		})
	}
	if u, _ := m.user.Load().(string); u != "scraper" {
		t.Errorf("metrics server saw user %q, want the configured credentials to be sent", u)
	}
}

func TestUsageErrors(t *testing.T) {
	m := "http://127.0.0.1:8082/metrics"
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no URL", []string{"-metrics-url", m}, "target URL is required"},
		{"no metrics URL", []string{"-url", target}, "metrics URL: invalid metrics endpoint: URL is required"},
		{"bad interval", args(target, m, "", "-interval", "10ms"), "interval must be between"},
		{"interval longer than duration", args(target, m, "", "-interval", "2s"), "must not be longer than the workload"},
		{"too many samples", args(target, m, "", "-max-samples", "5"), "more than max samples (5)"},
		{"timeout", args(target, m, "", "-timeout", "1s"), "timeout (1s) must be longer"},
		{"scrape timeout", args(target, m, "", "-scrape-timeout", "0s"), "scrape timeout must be positive"},
		{"telemetry budget", args(target, m, "", "-max-telemetry-bytes", "10"), "max telemetry bytes must be between"},
		{"empty k6", args(target, m, "", "-k6", ""), "k6 path must not be empty"},
		{"bad vus", args(target, m, "", "-vus", "0"), "vus must be between"},
		{"extra argument", append(args(target, m, ""), "extra"), "unexpected arguments"},
		{"unknown flag", []string{"-nope"}, "flag provided but not defined"},
		{"help", []string{"-h"}, "Usage: ruptiq-experiment"},
		{"secret in bad metrics URL", []string{"-url", target, "-metrics-url", "ftp://u:hunter2@h/metrics"}, "http:// or https://"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "experiment.json")
			f := &fakeRunner{}
			o := runCLI(t, context.Background(), f, append(tt.args, "-out", out)...)
			if o.code != exitUsage {
				t.Errorf("exit %d, want %d", o.code, exitUsage)
			}
			if !strings.Contains(o.stderr, tt.want) {
				t.Errorf("stderr = %q, want %q", o.stderr, tt.want)
			}
			if strings.Contains(o.stderr, "hunter2") {
				t.Errorf("stderr shows a password: %q", o.stderr)
			}
			if f.called.Load() {
				t.Error("the workload ran despite invalid arguments")
			}
			if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a report was written for invalid arguments (%v)", err)
			}
		})
	}
}

func TestK6PathFlag(t *testing.T) {
	m := newMetricsServer(t)
	t.Setenv(k6PathEnv, `C:\tools\k6-from-env.exe`)
	o := runCLI(t, context.Background(), &fakeRunner{metrics: m, scrapes: 3, result: healthyResult()}, args(target, m.URL+"/metrics", "")...)
	if o.k6Path != `C:\tools\k6-from-env.exe` {
		t.Errorf("k6 path from environment = %q", o.k6Path)
	}
	o = runCLI(t, context.Background(), &fakeRunner{metrics: m, scrapes: 3, result: healthyResult()}, args(target, m.URL+"/metrics", "", "-k6", "/opt/k6")...)
	if o.k6Path != "/opt/k6" {
		t.Errorf("k6 path from flag = %q", o.k6Path)
	}
}
