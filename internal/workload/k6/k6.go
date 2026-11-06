// Package k6 runs workloads with the k6 load generator (https://k6.io) as an
// external process. It implements workload.Runner.
//
// k6 is started directly with os/exec, never through a shell. The workload
// script is a static file embedded in this package; the target URL reaches it
// through an environment variable, not the command line.
package k6

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/RajK158/ruptiq/internal/workload"
)

// DefaultExecutable is the k6 command looked up on PATH when no path is set.
const DefaultExecutable = "k6"

const (
	// targetEnv carries the target URL to the script.
	targetEnv = "RUPTIQ_TARGET_URL"
	// outputTailBytes is how much of k6's combined output is kept for error
	// messages. A failing run can log one line per request.
	outputTailBytes = 4 << 10
	// maxSummaryBytes bounds the summary file read back from k6.
	maxSummaryBytes = 8 << 20
	// waitDelay bounds how long Run waits for k6's output after k6 is killed.
	waitDelay = 5 * time.Second
)

//go:embed scripts/http_get.js
var httpGetScript []byte

// ErrNotInstalled means the k6 executable could not be found.
var ErrNotInstalled = errors.New("k6 executable not found")

// ExitError means k6 ran but exited with a nonzero status.
type ExitError struct {
	Code int
	// Output is the end of k6's combined stdout and stderr.
	Output string
}

func (e *ExitError) Error() string {
	msg := fmt.Sprintf("k6 exited with status %d", e.Code)
	if e.Output != "" {
		msg += ": " + e.Output
	}
	return msg
}

// Runner runs HTTP GET workloads with k6. The zero value uses "k6" from PATH.
// It is safe for concurrent use; every run gets its own temporary directory.
type Runner struct {
	// Executable is the k6 command name or path. Empty means DefaultExecutable.
	Executable string
}

var _ workload.Runner = Runner{}

// Run validates spec, runs k6 against spec.TargetURL, and returns k6's
// measurements. It stops k6 when ctx is done and returns ctx's error.
//
// A run whose requests failed still returns a Result (judge it with
// workload.Evaluate); errors mean the run did not complete or its summary
// could not be trusted: ErrNotInstalled, *ExitError, ErrSummary, ErrNoRequests,
// or a context error.
func (r Runner) Run(ctx context.Context, spec workload.Spec) (workload.Result, error) {
	if err := spec.Validate(); err != nil {
		return workload.Result{}, err
	}
	exe, err := r.lookPath()
	if err != nil {
		return workload.Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return workload.Result{}, err
	}

	dir, err := os.MkdirTemp("", "ruptiq-k6-")
	if err != nil {
		return workload.Result{}, fmt.Errorf("creating temporary directory: %w", err)
	}
	defer os.RemoveAll(dir)

	scriptPath := filepath.Join(dir, "http_get.js")
	if err := os.WriteFile(scriptPath, httpGetScript, 0o600); err != nil {
		return workload.Result{}, fmt.Errorf("writing k6 script: %w", err)
	}
	summaryPath := filepath.Join(dir, "summary.json")

	out := &tailBuffer{max: outputTailBytes}
	cmd := exec.CommandContext(ctx, exe, args(spec, scriptPath, summaryPath)...)
	cmd.Env = environ(os.Environ(), spec.TargetURL)
	cmd.Dir = dir
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.WaitDelay = waitDelay

	started := time.Now()
	runErr := cmd.Run()
	finished := time.Now()

	if ctxErr := ctx.Err(); ctxErr != nil {
		return workload.Result{}, fmt.Errorf("k6 run stopped: %w", ctxErr)
	}
	output := workload.RedactText(out.String(), spec.TargetURL)
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return workload.Result{}, &ExitError{Code: exitErr.ExitCode(), Output: output}
		}
		return workload.Result{}, fmt.Errorf("running k6: %w", runErr)
	}

	data, err := readSummary(summaryPath)
	if err != nil {
		if tail := output; tail != "" {
			err = fmt.Errorf("%w (k6 output: %s)", err, tail)
		}
		return workload.Result{}, err
	}
	parsed, err := parseSummary(data)
	if err != nil {
		return workload.Result{}, err
	}

	return workload.Result{
		Runner:                    "k6",
		RunnerVersion:             parsed.K6Version,
		TargetURL:                 workload.RedactURL(spec.TargetURL),
		VUs:                       spec.VUs,
		ConfiguredDurationSeconds: spec.Duration.Seconds(),
		StartedAt:                 started.UTC(),
		FinishedAt:                finished.UTC(),
		Metrics:                   parsed.Metrics,
	}, nil
}

func (r Runner) lookPath() (string, error) {
	name := r.Executable
	if name == "" {
		name = DefaultExecutable
	}
	path, err := exec.LookPath(name)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("%w: '%s' (install k6 from https://grafana.com/docs/k6/latest/set-up/install-k6/ or set its path)", ErrNotInstalled, name)
		}
		return "", fmt.Errorf("finding k6 executable '%s': %w", name, err)
	}
	return path, nil
}

// args builds k6's command line. Every value is a separate argument; nothing
// is interpreted by a shell. The target URL is deliberately absent (see
// environ).
func args(spec workload.Spec, scriptPath, summaryPath string) []string {
	return []string{
		"run",
		"--quiet",
		"--no-color",
		"--no-usage-report",
		// The end-of-test summary must stay enabled: k6 writes no
		// --summary-export file when the summary mode is "disabled".
		"--summary-mode", "compact",
		"--new-machine-readable-summary",
		"--summary-export", summaryPath,
		"--include-system-env-vars=true",
		"--vus", strconv.Itoa(spec.VUs),
		"--duration", strconv.FormatInt(int64(spec.Duration/time.Second), 10) + "s",
		scriptPath,
	}
}

// environ returns the environment for k6: the parent's, without any K6_*
// variables (they would silently change k6 options such as the summary mode),
// plus the target URL for the script. Passing the URL here instead of with
// "-e" keeps it, and any credentials in it, out of the process list.
func environ(parent []string, targetURL string) []string {
	env := make([]string, 0, len(parent)+1)
	for _, kv := range parent {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(strings.ToUpper(name), "K6_") || strings.EqualFold(name, targetEnv) {
			continue
		}
		env = append(env, kv)
	}
	return append(env, targetEnv+"="+targetURL)
}

func readSummary(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: k6 did not write a summary file", ErrSummary)
		}
		return nil, fmt.Errorf("%w: %v", ErrSummary, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxSummaryBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: reading summary: %v", ErrSummary, err)
	}
	if len(data) > maxSummaryBytes {
		return nil, fmt.Errorf("%w: summary file is larger than %d bytes", ErrSummary, maxSummaryBytes)
	}
	return data, nil
}

// tailBuffer keeps only the last max bytes written to it. exec.Cmd writes to
// it from at most one goroutine when it is both Stdout and Stderr.
type tailBuffer struct {
	max       int
	buf       []byte
	truncated bool
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) >= t.max {
		t.truncated = t.truncated || len(t.buf) > 0 || len(p) > t.max
		t.buf = append(t.buf[:0], p[len(p)-t.max:]...)
		return n, nil
	}
	if over := len(t.buf) + len(p) - t.max; over > 0 {
		t.truncated = true
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	t.buf = append(t.buf, p...)
	return n, nil
}

// String returns the kept output, trimmed, marked when earlier output was
// dropped. After a drop, the first kept line is cut somewhere in the middle,
// possibly inside a URL where redaction could no longer recognize a secret, so
// it is dropped too.
func (t *tailBuffer) String() string {
	s := string(t.buf)
	if t.truncated {
		_, s, _ = strings.Cut(s, "\n")
	}
	s = strings.TrimSpace(s)
	if t.truncated {
		s = strings.TrimSpace("[earlier output omitted]\n" + s)
	}
	return s
}
