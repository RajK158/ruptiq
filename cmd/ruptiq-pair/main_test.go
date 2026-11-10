package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RajK158/ruptiq/internal/telemetry"
	"github.com/RajK158/ruptiq/internal/workload"
)

func TestUsageAndRedaction(t *testing.T) {
	secretURL := "http://s3cret-user:s3cret-token@127.0.0.1:8090/work?api_key=s3cret-token"
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"-h"}, &stdout, &stderr, nil, nil)
	if code != exitUsage || !strings.Contains(stderr.String(), "Usage:") || !strings.Contains(stderr.String(), "-stage") {
		t.Fatalf("help code %d\n%s", code, stderr.String())
	}

	stderr.Reset()
	code = run(context.Background(), []string{"-stage", "before", "-pair-dir", "tmp/pair", secretURL}, &stdout, &stderr, nil, nil)
	if code != exitUsage {
		t.Fatalf("code %d", code)
	}
	if strings.Contains(stderr.String(), "s3cret-user") || strings.Contains(stderr.String(), "s3cret-token") {
		t.Fatalf("secret in error:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "unexpected") {
		t.Fatalf("stderr = %s", stderr.String())
	}

	stderr.Reset()
	code = run(context.Background(), []string{"-stage", "compare", "-pair-dir", "tmp/pair", "-compare-min-windows", "0", secretURL}, &stdout, &stderr, nil, nil)
	if code != exitUsage {
		t.Fatalf("compare code %d\n%s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "s3cret-user") || strings.Contains(stderr.String(), "s3cret-token") {
		t.Fatalf("secret in compare error:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "min windows") && !strings.Contains(stderr.String(), "unexpected") {
		t.Fatalf("stderr = %s", stderr.String())
	}
}

func TestCLIArgumentsAndSecrets(t *testing.T) {
	secret := "http://s3cret-user:s3cret-token@127.0.0.1:8090/work?api_key=s3cret-token"
	var stdout, stderr bytes.Buffer
	assertClean := func(code int, want string) {
		t.Helper()
		text := stdout.String() + stderr.String()
		if strings.Contains(text, "s3cret-user") || strings.Contains(text, "s3cret-token") || strings.Contains(text, "api_key") {
			t.Fatalf("secret in output:\n%s", text)
		}
		if code != exitUsage || !strings.Contains(stderr.String(), want) {
			t.Fatalf("code %d want %s\n%s", code, want, stderr.String())
		}
		stdout.Reset()
		stderr.Reset()
	}
	assertClean(run(context.Background(), []string{"-pair-dir", t.TempDir()}, &stdout, &stderr, nil, nil), "stage")
	assertClean(run(context.Background(), []string{"-stage", "sideways", "-pair-dir", t.TempDir()}, &stdout, &stderr, nil, nil), "stage")
	assertClean(run(context.Background(), []string{"-stage", "compare"}, &stdout, &stderr, nil, nil), "pair directory")
	assertClean(run(context.Background(), []string{"-stage", "before", "-pair-dir", t.TempDir(), "-duration", secret}, &stdout, &stderr, nil, nil), "duration")
	assertClean(run(context.Background(), []string{"-stage", "before", "-pair-dir", t.TempDir(), "-url", secret, "-vus", secret}, &stdout, &stderr, nil, nil), "invalid")
}

func TestCLIAfterMismatchAndIncompleteCompare(t *testing.T) {
	dir := t.TempDir()
	secretURL := "http://s3cret-user:s3cret-token@127.0.0.1:8090/work?mode=baseline"
	metrics := "http://s3cret-user:s3cret-token@127.0.0.1:8090/metrics?api_key=s3cret-token"
	args := []string{
		"-stage", "before", "-pair-dir", dir,
		"-url", secretURL, "-metrics-url", metrics,
		"-vus", "2", "-duration", "1s", "-interval", "100ms", "-k6", "k6",
	}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, &stdout, &stderr, func(string) workload.Runner {
		return instantCLIRunner{}
	}, func(string, time.Duration) (telemetry.Reader, error) {
		return &cliReader{}, nil
	})
	if code != exitCompleted {
		t.Fatalf("before code %d\n%s\n%s", code, stdout.String(), stderr.String())
	}
	if secretLeak(stdout.String()+stderr.String()) || secretLeak(dirText(t, dir)) {
		t.Fatal("baseline output or metadata contains a secret")
	}

	stdout.Reset()
	stderr.Reset()
	var afterCalls int
	args[1] = "after"
	for i, a := range args {
		if a == "-url" {
			args[i+1] = "http://s3cret-user:s3cret-token@127.0.0.1:8090/work?mode=optimized"
		}
	}
	code = run(context.Background(), args, &stdout, &stderr, func(string) workload.Runner {
		return countingCLIRunner{n: &afterCalls}
	}, func(string, time.Duration) (telemetry.Reader, error) {
		return &cliReader{n: &afterCalls}, nil
	})
	if code != exitUsage || afterCalls != 0 {
		t.Fatalf("after code %d calls %d\n%s", code, afterCalls, stderr.String())
	}
	text := stdout.String() + stderr.String()
	if secretLeak(text) || secretLeak(dirText(t, dir)) || !strings.Contains(stderr.String(), "query") {
		t.Fatalf("after output:\n%s\n%s", stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	args[1] = "compare"
	code = run(context.Background(), []string{"-stage", "compare", "-pair-dir", dir}, &stdout, &stderr, func(string) workload.Runner {
		t.Error("compare started a runner")
		return instantCLIRunner{}
	}, func(string, time.Duration) (telemetry.Reader, error) {
		t.Error("compare started a reader")
		return &cliReader{}, nil
	})
	if code != exitUsage || !strings.Contains(stderr.String(), "candidate") {
		t.Fatalf("compare code %d\n%s", code, stderr.String())
	}
	if secretLeak(stdout.String() + stderr.String()) {
		t.Fatal("compare output contains a secret")
	}
}

func secretLeak(text string) bool {
	return strings.Contains(text, "s3cret-user") || strings.Contains(text, "s3cret-token") ||
		strings.Contains(text, "mode=baseline") || strings.Contains(text, "mode=optimized") ||
		strings.Contains(text, "api_key")
}

func dirText(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		b.Write(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

type countingCLIRunner struct{ n *int }

func (c countingCLIRunner) Run(ctx context.Context, spec workload.Spec) (workload.Result, error) {
	if c.n != nil {
		*c.n++
	}
	return instantCLIRunner{}.Run(ctx, spec)
}

type instantCLIRunner struct{}

func (instantCLIRunner) Run(context.Context, workload.Spec) (workload.Result, error) {
	return workload.Result{
		Runner: "fake", RunnerVersion: "0", VUs: 2, ConfiguredDurationSeconds: 1,
		StartedAt: time.Now().Add(-time.Second).UTC(), FinishedAt: time.Now().UTC(),
		Metrics: workload.Metrics{
			DurationSeconds: 1, HTTPRequests: 10, RequestsPerSecond: 10,
			LatencyMs: workload.Latency{Avg: 1, Min: 0.1, P50: 1, P90: 2, P95: 3, Max: 4},
		},
	}, nil
}

type cliReader struct {
	last time.Time
	n    *int
}

func (r *cliReader) Read(context.Context) (telemetry.Snapshot, error) {
	for {
		at := time.Now().UTC()
		if r.last.IsZero() || at.After(r.last) {
			r.last = at
			if r.n != nil {
				*r.n++
			}
			value := telemetry.Float(1)
			return telemetry.Snapshot{
				ScrapedAt: at, ScrapeDurationSeconds: 0.001,
				Families: []telemetry.Family{{
					Name: "inventory_http_requests_total", Type: telemetry.Counter,
					Series: []telemetry.Series{{Labels: map[string]string{"method": "GET", "route": "/work", "status": "200"}, Value: &value}},
				}},
			}, nil
		}
		time.Sleep(time.Millisecond)
	}
}

func TestCompareDoesNotStartWorkload(t *testing.T) {
	var stdout, stderr bytes.Buffer
	started := false
	code := run(context.Background(), []string{"-stage", "compare", "-pair-dir", t.TempDir()}, &stdout, &stderr, func(string) workload.Runner {
		started = true
		return nil
	}, func(string, time.Duration) (telemetry.Reader, error) {
		started = true
		return nil, nil
	})
	if started {
		t.Fatal("compare started a runner or reader")
	}
	if code != exitUsage {
		t.Fatalf("code %d\n%s\n%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "baseline") {
		t.Fatalf("stderr = %s", stderr.String())
	}
}
