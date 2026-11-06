// Command ruptiq-run runs one HTTP GET load test against a target URL with
// k6, prints a report, and can save the results as JSON.
//
//	ruptiq-run -url http://localhost:8082/inventory/LAPTOP-001 -vus 2 -duration 5s -out run.json
//
// Exit status: 0 the run passed, 1 the run completed but failed its pass
// criteria, 2 invalid arguments, 3 the run could not be completed (k6
// missing, k6 failed, timeout, unreadable results).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/RajK158/ruptiq/internal/workload"
	"github.com/RajK158/ruptiq/internal/workload/k6"
)

const (
	exitPassed       = 0
	exitFailed       = 1
	exitUsage        = 2
	exitRunError     = 3
	k6PathEnv        = "RUPTIQ_K6_PATH"
	timeoutAllowance = 60 * time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, func(exe string) workload.Runner {
		return k6.Runner{Executable: exe}
	})
	stop()
	os.Exit(code)
}

// config is the parsed command line.
type config struct {
	spec           workload.Spec
	timeout        time.Duration
	k6Path         string
	outPath        string
	maxFailureRate float64
}

func parseArgs(args []string, stderr io.Writer) (config, error) {
	fs := flag.NewFlagSet("ruptiq-run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: ruptiq-run -url URL [flags]")
		fmt.Fprintln(stderr, "Runs one HTTP GET load test with k6 and reports what it measured.")
		fs.PrintDefaults()
	}

	var c config
	defaultK6 := os.Getenv(k6PathEnv)
	if defaultK6 == "" {
		defaultK6 = k6.DefaultExecutable
	}
	fs.StringVar(&c.spec.TargetURL, "url", "", "target `URL` (http or https) to send GET requests to (required)")
	fs.IntVar(&c.spec.VUs, "vus", workload.DefaultVUs, "number of concurrent virtual users")
	fs.DurationVar(&c.spec.Duration, "duration", workload.DefaultDuration, "how long to apply load, in whole seconds (for example 5s, 1m)")
	fs.DurationVar(&c.timeout, "timeout", 0, "hard limit for the whole run, after which k6 is stopped (default: duration + 60s)")
	fs.StringVar(&c.k6Path, "k6", defaultK6, "k6 executable name or path (default from "+k6PathEnv+", else k6 on PATH)")
	fs.StringVar(&c.outPath, "out", "", "write the results as JSON to this `file` (optional)")
	fs.Float64Var(&c.maxFailureRate, "max-failure-rate", 0, "highest failure rate (0 to 1) for the run to pass")

	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if fs.NArg() > 0 {
		return config{}, fmt.Errorf("unexpected arguments: %q", fs.Args())
	}

	var problems []string
	if err := c.spec.Validate(); err != nil {
		var verr *workload.ValidationError
		if errors.As(err, &verr) {
			problems = append(problems, verr.Problems...)
		} else {
			problems = append(problems, err.Error())
		}
	}
	if err := workload.ValidateMaxFailureRate(c.maxFailureRate); err != nil {
		problems = append(problems, err.Error())
	}
	switch {
	case c.timeout == 0:
		c.timeout = c.spec.Duration + timeoutAllowance
	case c.timeout <= c.spec.Duration:
		problems = append(problems, fmt.Sprintf("timeout (%v) must be longer than duration (%v)", c.timeout, c.spec.Duration))
	}
	if c.k6Path == "" {
		problems = append(problems, "k6 path must not be empty")
	}
	if len(problems) > 0 {
		return config{}, &workload.ValidationError{Problems: problems}
	}
	return c, nil
}

// run is main without process-level side effects, for tests.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, newRunner func(k6Path string) workload.Runner) int {
	c, err := parseArgs(args, stderr)
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stderr, "ruptiq-run:", err)
		}
		return exitUsage
	}

	fmt.Fprintf(stdout, "Running %d virtual users for %v against %s (timeout %v)...\n",
		c.spec.VUs, c.spec.Duration, workload.RedactURL(c.spec.TargetURL), c.timeout)

	runCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	result, runErr := newRunner(c.k6Path).Run(runCtx, c.spec)

	report := workload.Report{SchemaVersion: workload.ReportSchemaVersion}
	code := exitPassed
	if runErr != nil {
		if errors.Is(runErr, context.DeadlineExceeded) {
			runErr = fmt.Errorf("run exceeded the %v timeout: %w", c.timeout, runErr)
		}
		report.Error = workload.RedactText(runErr.Error(), c.spec.TargetURL)
		report.Verdict = workload.Verdict{Passed: false, MaxFailureRate: c.maxFailureRate,
			Reasons: []string{"the run did not complete"}}
		fmt.Fprintln(stderr, "ruptiq-run: run failed:", report.Error)
		code = exitRunError
	} else {
		// Taken from the spec, so that the report never depends on a runner
		// having redacted its URL.
		result.TargetURL = workload.RedactURL(c.spec.TargetURL)
		report.Run = &result
		report.Verdict = workload.Evaluate(result.Metrics, c.maxFailureRate)
		printReport(stdout, result, report.Verdict)
		if !report.Verdict.Passed {
			code = exitFailed
		}
	}

	if c.outPath != "" {
		if err := writeReport(c.outPath, report); err != nil {
			fmt.Fprintln(stderr, "ruptiq-run: writing results:", err)
			return exitRunError
		}
		fmt.Fprintln(stdout, "Results written to", c.outPath)
	}
	return code
}

func printReport(w io.Writer, r workload.Result, v workload.Verdict) {
	m := r.Metrics
	runner := r.Runner
	if r.RunnerVersion != "" {
		runner += " v" + r.RunnerVersion
	}
	fmt.Fprintf(w, "\nRuptiq load test report (%s)\n", runner)
	fmt.Fprintf(w, "  Target:            %s\n", r.TargetURL)
	fmt.Fprintf(w, "  Load:              %d virtual users, %gs configured, %.2fs measured\n",
		r.VUs, r.ConfiguredDurationSeconds, m.DurationSeconds)
	fmt.Fprintf(w, "  HTTP requests:     %d (%.1f per second)\n", m.HTTPRequests, m.RequestsPerSecond)
	fmt.Fprintf(w, "  Failed requests:   %d (%.2f%%)\n", m.FailedRequests, m.FailureRate*100)
	fmt.Fprintf(w, "  Latency (ms):      avg %.3f   p50 %.3f   p95 %.3f\n", m.LatencyMs.Avg, m.LatencyMs.P50, m.LatencyMs.P95)
	fmt.Fprintf(w, "                     min %.3f   p90 %.3f   max %.3f\n", m.LatencyMs.Min, m.LatencyMs.P90, m.LatencyMs.Max)
	if v.Passed {
		fmt.Fprintf(w, "  Result:            PASSED (failure rate at most %.2f%%)\n", v.MaxFailureRate*100)
	} else {
		fmt.Fprintf(w, "  Result:            FAILED\n")
		for _, reason := range v.Reasons {
			fmt.Fprintf(w, "                     - %s\n", reason)
		}
	}
}

// writeReport writes report as indented JSON. It writes a temporary file in
// the same directory and renames it, so a reader never sees a partial file.
func writeReport(path string, report workload.Report) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), ".ruptiq-run-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
