// Command ruptiq-experiment runs one experiment: an HTTP GET load test with
// k6 while a Prometheus metrics endpoint is sampled periodically. It prints a
// summary and can save the workload results and every telemetry sample as one
// JSON report.
//
//	ruptiq-experiment -url http://localhost:8082/inventory/LAPTOP-001 \
//	    -metrics-url http://localhost:8082/metrics -vus 2 -duration 5s -interval 1s -out experiment.json
//
// Exit status: 0 passed (workload passed, telemetry complete), 1 the workload
// completed but failed its pass criteria, 2 invalid arguments, 3 the workload
// could not be completed or the report could not be written, 4 the workload
// passed but the telemetry is incomplete, 5 cancelled or timed out.
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
	"strings"
	"syscall"
	"time"

	"github.com/RajK158/ruptiq/internal/experiment"
	"github.com/RajK158/ruptiq/internal/telemetry"
	"github.com/RajK158/ruptiq/internal/workload"
	"github.com/RajK158/ruptiq/internal/workload/k6"
)

const (
	exitPassed     = 0
	exitFailed     = 1
	exitUsage      = 2
	exitError      = 3
	exitIncomplete = 4
	exitStopped    = 5

	k6PathEnv            = "RUPTIQ_K6_PATH"
	defaultScrapeTimeout = 5 * time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, func(exe string) workload.Runner {
		return k6.Runner{Executable: exe}
	})
	stop()
	os.Exit(code)
}

type config struct {
	experiment    experiment.Config
	scrapeTimeout time.Duration
	k6Path        string
	outPath       string
}

func parseArgs(args []string, stderr io.Writer) (config, error) {
	fs := flag.NewFlagSet("ruptiq-experiment", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: ruptiq-experiment -url URL -metrics-url URL [flags]")
		fmt.Fprintln(stderr, "Runs one HTTP GET load test with k6 while sampling Prometheus metrics, and reports both.")
		fs.PrintDefaults()
	}

	var c config
	e := &c.experiment
	defaultK6 := os.Getenv(k6PathEnv)
	if defaultK6 == "" {
		defaultK6 = k6.DefaultExecutable
	}
	fs.StringVar(&e.Workload.TargetURL, "url", "", "target `URL` (http or https) to send GET requests to (required)")
	fs.StringVar(&e.MetricsURL, "metrics-url", "", "Prometheus metrics endpoint `URL` to sample (required)")
	fs.IntVar(&e.Workload.VUs, "vus", workload.DefaultVUs, "number of concurrent virtual users")
	fs.DurationVar(&e.Workload.Duration, "duration", workload.DefaultDuration, "how long to apply load, in whole seconds (for example 5s, 1m)")
	fs.DurationVar(&e.Interval, "interval", experiment.DefaultInterval, "time between metrics scrapes while the load runs")
	fs.IntVar(&e.MaxSamples, "max-samples", experiment.DefaultMaxSamples, "most scrapes in one experiment, including the ones before and after the load")
	fs.Int64Var(&e.MaxTelemetryBytes, "max-telemetry-bytes", experiment.DefaultMaxTelemetryBytes, "retention budget: most sample data (JSON bytes) kept from one experiment")
	fs.DurationVar(&e.Timeout, "timeout", 0, "hard limit for the whole experiment (default: duration + 60s)")
	fs.DurationVar(&c.scrapeTimeout, "scrape-timeout", defaultScrapeTimeout, "maximum time for one metrics scrape")
	fs.StringVar(&c.k6Path, "k6", defaultK6, "k6 executable name or path (default from "+k6PathEnv+", else k6 on PATH)")
	fs.Float64Var(&e.MaxFailureRate, "max-failure-rate", 0, "highest failure rate (0 to 1) for the load test to pass")
	fs.StringVar(&c.outPath, "out", "", "write the experiment report as JSON to this `file` (optional)")

	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if fs.NArg() > 0 {
		return config{}, fmt.Errorf("unexpected arguments: %q", fs.Args())
	}

	c.experiment = c.experiment.WithDefaults()
	var problems []string
	if err := c.experiment.Validate(); err != nil {
		var verr *experiment.ValidationError
		if errors.As(err, &verr) {
			problems = append(problems, verr.Problems...)
		} else {
			problems = append(problems, err.Error())
		}
	}
	if c.scrapeTimeout <= 0 {
		problems = append(problems, fmt.Sprintf("scrape timeout must be positive, got %v", c.scrapeTimeout))
	}
	if c.k6Path == "" {
		problems = append(problems, "k6 path must not be empty")
	}
	if len(problems) > 0 {
		return config{}, &experiment.ValidationError{Problems: problems}
	}
	return c, nil
}

// run is main without process-level side effects, for tests.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, newRunner func(k6Path string) workload.Runner) int {
	c, err := parseArgs(args, stderr)
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stderr, "ruptiq-experiment:", err)
		}
		return exitUsage
	}
	reader, err := telemetry.NewPrometheusReader(telemetry.PrometheusConfig{URL: c.experiment.MetricsURL, Timeout: c.scrapeTimeout})
	if err != nil {
		fmt.Fprintln(stderr, "ruptiq-experiment:", err)
		return exitUsage
	}

	e := c.experiment
	fmt.Fprintf(stdout, "Running %d virtual users for %v against %s,\nsampling %s every %v (timeout %v)...\n\n",
		e.Workload.VUs, e.Workload.Duration, workload.RedactURL(e.Workload.TargetURL),
		reader.Endpoint(), e.Interval, e.Timeout)

	coordinator := &experiment.Coordinator{Runner: newRunner(c.k6Path), Reader: reader}
	report, err := coordinator.Run(ctx, e)
	if err != nil {
		fmt.Fprintln(stderr, "ruptiq-experiment:", err)
		return exitUsage
	}
	if err := experiment.WriteSummary(stdout, report); err != nil {
		fmt.Fprintln(stderr, "ruptiq-experiment:", err)
	}

	code := exitCode(report.Status)
	if code != exitPassed {
		fmt.Fprintf(stderr, "ruptiq-experiment: experiment %s\n", strings.ReplaceAll(string(report.Status), "_", " "))
	}
	if c.outPath != "" {
		if err := writeReport(c.outPath, report); err != nil {
			fmt.Fprintln(stderr, "ruptiq-experiment: writing report:", err)
			return exitError
		}
		fmt.Fprintln(stdout, "\nReport written to", c.outPath)
	}
	return code
}

func exitCode(s experiment.Status) int {
	switch s {
	case experiment.StatusPassed:
		return exitPassed
	case experiment.StatusFailed:
		return exitFailed
	case experiment.StatusIncomplete:
		return exitIncomplete
	case experiment.StatusCancelled, experiment.StatusTimedOut:
		return exitStopped
	}
	return exitError
}

// writeReport writes report as indented JSON through a temporary file in the
// same directory, so a reader never sees a partial file.
func writeReport(path string, report experiment.Report) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), ".ruptiq-experiment-*.json")
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
