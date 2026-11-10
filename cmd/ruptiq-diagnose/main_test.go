package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RajK158/ruptiq/internal/diagnosis"
	"github.com/RajK158/ruptiq/internal/telemetry"
	"github.com/RajK158/ruptiq/internal/workload"
)

func TestUsageAndInvalidConfig(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"-h"}, &stdout, &stderr, nil, nil); code != exitUsage {
		t.Fatalf("help exit = %d", code)
	}
	stderr.Reset()
	if code := run(context.Background(), nil, &stdout, &stderr, nil, nil); code != exitUsage {
		t.Fatalf("no args exit = %d, stderr %s", code, stderr.String())
	}
	stderr.Reset()
	args := []string{"-url", "http://127.0.0.1/work", "-metrics-url", "http://127.0.0.1/metrics", "-out-dir", t.TempDir(), "extra"}
	if code := run(context.Background(), args, &stdout, &stderr, nil, nil); code != exitUsage {
		t.Fatalf("extra arg exit = %d", code)
	}
	if !strings.Contains(stderr.String(), "unexpected") {
		t.Fatalf("stderr = %s", stderr.String())
	}
}

func TestCLICollisionDoesNotStart(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, diagnosis.FileExperiment), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	ran := false
	code := run(context.Background(), baseArgs(dir), &stdout, &stderr, func(string) workload.Runner {
		return runnerFunc(func(context.Context, workload.Spec) (workload.Result, error) {
			ran = true
			return workload.Result{}, errors.New("should not run")
		})
	}, func(string, time.Duration) (telemetry.Reader, error) {
		return emptyReader{}, nil
	})
	if code != exitUsage {
		t.Fatalf("exit = %d, stderr %s", code, stderr.String())
	}
	if ran {
		t.Fatal("collision still started the workload")
	}
	data, err := os.ReadFile(filepath.Join(dir, diagnosis.FileExperiment))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "{}\n" {
		t.Fatalf("existing report changed: %s", data)
	}
}

func TestCLIKeepsEarlierArtifactOnWorkloadFailure(t *testing.T) {
	dir := t.TempDir()
	const secret = "s3cret-token"
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"-url", "http://user:" + secret + "@127.0.0.1:8090/work?token=" + secret,
		"-metrics-url", "http://user:" + secret + "@127.0.0.1:8090/metrics?token=" + secret,
		"-out-dir", dir,
		"-k6", "k6",
	}, &stdout, &stderr,
		func(string) workload.Runner {
			return runnerFunc(func(context.Context, workload.Spec) (workload.Result, error) {
				return workload.Result{}, errors.New("k6 failed for http://user:" + secret + "@127.0.0.1:8090/work?token=" + secret)
			})
		},
		func(string, time.Duration) (telemetry.Reader, error) {
			return emptyReader{}, nil
		},
	)
	if code != exitFailed {
		t.Fatalf("exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, diagnosis.FileExperiment)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, diagnosis.FileAnalysis)); !os.IsNotExist(err) {
		t.Fatal("analysis was fabricated")
	}
	blob := stdout.String() + stderr.String()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		blob += string(data)
	}
	if strings.Contains(blob, secret) {
		t.Fatal("secret leaked through the CLI")
	}
	if !strings.Contains(stdout.String(), "Outcome:") || !strings.Contains(stdout.String(), diagnosis.OutcomeFailed) {
		t.Fatalf("stdout =\n%s", stdout.String())
	}
}

func TestCLICancellation(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	code := run(ctx, baseArgs(dir), &stdout, &stderr,
		func(string) workload.Runner {
			return runnerFunc(func(ctx context.Context, _ workload.Spec) (workload.Result, error) {
				<-ctx.Done()
				return workload.Result{}, ctx.Err()
			})
		},
		func(url string, timeout time.Duration) (telemetry.Reader, error) {
			return &emptyReader{}, nil
		},
	)
	if code != exitFailed {
		t.Fatalf("exit = %d\n%s\n%s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, diagnosis.FileExperiment)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, diagnosis.FileAnalysis)); !os.IsNotExist(err) {
		t.Fatal("analysis was written after cancellation")
	}
}

func TestRelativeK6PathIsResolved(t *testing.T) {
	dir := t.TempDir()
	rel := filepath.Join("tmp", "k6", "k6.exe")
	var got string
	code := run(context.Background(), []string{
		"-url", "http://127.0.0.1:8090/work",
		"-metrics-url", "http://127.0.0.1:8090/metrics",
		"-out-dir", dir,
		"-k6", rel,
	}, &bytes.Buffer{}, &bytes.Buffer{},
		func(path string) workload.Runner {
			got = path
			return runnerFunc(func(context.Context, workload.Spec) (workload.Result, error) {
				return workload.Result{}, errors.New("stopped before k6")
			})
		},
		func(string, time.Duration) (telemetry.Reader, error) {
			return emptyReader{}, nil
		},
	)
	if code != exitFailed {
		t.Fatalf("exit = %d", code)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("k6 path %q is relative; k6 would look for it in its temporary directory", got)
	}
	if !strings.HasSuffix(filepath.Clean(got), filepath.Clean(rel)) {
		t.Fatalf("resolved k6 path %q does not end with %s", got, rel)
	}
}

func baseArgs(dir string) []string {
	return []string{
		"-url", "http://127.0.0.1:8090/work",
		"-metrics-url", "http://127.0.0.1:8090/metrics",
		"-out-dir", dir,
		"-k6", "k6",
	}
}

type runnerFunc func(context.Context, workload.Spec) (workload.Result, error)

func (f runnerFunc) Run(ctx context.Context, spec workload.Spec) (workload.Result, error) {
	return f(ctx, spec)
}

type emptyReader struct{}

func (emptyReader) Read(context.Context) (telemetry.Snapshot, error) {
	return telemetry.Snapshot{}, errors.New("metrics unavailable")
}
