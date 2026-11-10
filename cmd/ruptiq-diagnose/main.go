// Command ruptiq-diagnose runs one diagnosis: a k6 workload with Prometheus
// sampling, then the existing analysis and investigation of that run. It
// writes experiment.json, analysis.json, investigation.json, and manifest.json
// in one directory and prints a short summary.
//
//	ruptiq-diagnose -url http://127.0.0.1:8090/work -metrics-url http://127.0.0.1:8090/metrics \
//	    -vus 2 -duration 20s -interval 1s -k6 PATH_TO_K6 -out-dir tmp/diagnosis-run
//
// Exit status: 0 the stages that could run finished and their reports were
// saved (an abstaining investigation is still success, and a completed run is
// not itself a performance finding), 2 invalid arguments or an output
// directory that already contains a diagnosis, 3 a stage failed or was
// skipped because the workload did not complete.
package main

import (
	"context"
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

	"github.com/RajK158/ruptiq/internal/analysis"
	"github.com/RajK158/ruptiq/internal/diagnosis"
	"github.com/RajK158/ruptiq/internal/experiment"
	"github.com/RajK158/ruptiq/internal/investigation"
	"github.com/RajK158/ruptiq/internal/telemetry"
	"github.com/RajK158/ruptiq/internal/workload"
	"github.com/RajK158/ruptiq/internal/workload/k6"
)

const (
	exitCompleted = 0
	exitUsage     = 2
	exitFailed    = 3

	k6PathEnv            = "RUPTIQ_K6_PATH"
	defaultScrapeTimeout = 5 * time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, func(exe string) workload.Runner {
		return k6.Runner{Executable: exe}
	}, func(url string, timeout time.Duration) (telemetry.Reader, error) {
		return telemetry.NewPrometheusReader(telemetry.PrometheusConfig{URL: url, Timeout: timeout})
	})
	stop()
	os.Exit(code)
}

type config struct {
	diagnosis     diagnosis.Config
	scrapeTimeout time.Duration
	k6Path        string
}

func parseArgs(args []string, stderr io.Writer) (config, error) {
	fs := flag.NewFlagSet("ruptiq-diagnose", flag.ContinueOnError)
	// Discard the parser's own text. It quotes the raw flag value before the
	// caller can redact credentials or query strings.
	fs.SetOutput(io.Discard)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: ruptiq-diagnose -url URL -metrics-url URL -out-dir DIR [flags]")
		fmt.Fprintln(stderr, "Runs one experiment, analyses it, and investigates that analysis. Writes experiment.json, analysis.json, investigation.json, and manifest.json.")
		fs.SetOutput(stderr)
		fs.PrintDefaults()
		fs.SetOutput(io.Discard)
	}

	var c config
	e := &c.diagnosis.Experiment
	a := &c.diagnosis.Analysis
	inv := &c.diagnosis.Investigation
	aDef := analysis.DefaultConfig()
	iDef := investigation.DefaultConfig()
	*a = aDef
	*inv = iDef
	defaultK6 := os.Getenv(k6PathEnv)
	if defaultK6 == "" {
		defaultK6 = k6.DefaultExecutable
	}

	fs.StringVar(&e.Workload.TargetURL, "url", "", "target `URL` (http or https) to send GET requests to (required)")
	fs.StringVar(&e.MetricsURL, "metrics-url", "", "Prometheus metrics endpoint `URL` to sample (required)")
	fs.StringVar(&c.diagnosis.OutputDir, "out-dir", "", "directory for experiment.json, analysis.json, investigation.json, and manifest.json (required)")
	fs.IntVar(&e.Workload.VUs, "vus", workload.DefaultVUs, "number of concurrent virtual users")
	fs.DurationVar(&e.Workload.Duration, "duration", workload.DefaultDuration, "how long to apply load, in whole seconds (for example 20s, 1m)")
	fs.DurationVar(&e.Interval, "interval", experiment.DefaultInterval, "time between metrics scrapes while the load runs")
	fs.IntVar(&e.MaxSamples, "max-samples", experiment.DefaultMaxSamples, "most scrapes in one experiment, including the ones before and after the load")
	fs.Int64Var(&e.MaxTelemetryBytes, "max-telemetry-bytes", experiment.DefaultMaxTelemetryBytes, "retention budget: most sample data (JSON bytes) kept from one experiment")
	fs.DurationVar(&e.Timeout, "timeout", 0, "hard limit for the whole experiment (default: duration + 60s)")
	fs.DurationVar(&c.scrapeTimeout, "scrape-timeout", defaultScrapeTimeout, "maximum time for one metrics scrape")
	fs.StringVar(&c.k6Path, "k6", defaultK6, "k6 executable name or path (default from "+k6PathEnv+", else k6 on PATH)")
	fs.Float64Var(&e.MaxFailureRate, "max-failure-rate", 0, "highest failure rate (0 to 1) for the load test to pass")

	fs.StringVar(&a.RequestsCounter, "requests-metric", aDef.RequestsCounter, "counter `family` of completed HTTP requests")
	fs.StringVar(&a.DurationHistogram, "duration-metric", aDef.DurationHistogram, "histogram `family` of HTTP request durations, in seconds")
	fs.StringVar(&a.MethodLabel, "method-label", aDef.MethodLabel, "label holding the HTTP method (empty: do not split by method)")
	fs.StringVar(&a.RouteLabel, "route-label", aDef.RouteLabel, "label holding the route template (empty: do not split by route)")
	fs.StringVar(&a.StatusLabel, "status-label", aDef.StatusLabel, "label holding the HTTP status code (empty: 5xx rates unavailable)")
	fs.IntVar(&a.MinObservations, "min-observations", aDef.MinObservations, "fewest requests in a window for a p95 estimate")

	fs.IntVar(&inv.MinWindows, "min-windows", iDef.MinWindows, "fewest usable windows in each of the baseline and the comparison")
	fs.Float64Var(&inv.MinRequests, "min-requests", iDef.MinRequests, "fewest requests (or latency observations) per side")
	fs.Float64Var(&inv.MinServerErrors, "min-server-errors", iDef.MinServerErrors, "fewest 5xx responses in the comparison for a server error increase")
	fs.Float64Var(&inv.ErrorRatioIncrease, "error-ratio-increase", iDef.ErrorRatioIncrease, "smallest rise of the 5xx ratio, as a fraction (0.01 = 1 percentage point)")
	fs.Float64Var(&inv.ErrorRatioFactor, "error-ratio-factor", iDef.ErrorRatioFactor, "how many times a non-zero baseline 5xx ratio the comparison's must be")
	fs.Float64Var(&inv.LatencyIncrease, "latency-increase", iDef.LatencyIncrease, "smallest relative rise of the p95 estimate (0.5 = +50%)")
	fs.Float64Var(&inv.ThroughputDecrease, "throughput-decrease", iDef.ThroughputDecrease, "smallest relative fall of the request rate (0.3 = -30%)")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return config{}, err
		}
		return config{}, fmt.Errorf("%s", workload.RedactText(err.Error(), ""))
	}
	if fs.NArg() > 0 {
		shown := workload.RedactText(fmt.Sprintf("%q", fs.Args()), "")
		return config{}, fmt.Errorf("unexpected arguments: %s", shown)
	}
	c.diagnosis = c.diagnosis.WithDefaults()
	var problems []string
	if err := c.diagnosis.Validate(); err != nil {
		var verr *diagnosis.ValidationError
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
		for i, p := range problems {
			problems[i] = workload.RedactText(p, "")
		}
		return config{}, &diagnosis.ValidationError{Problems: problems}
	}
	return c, nil
}

// run is main without process-level side effects, for tests.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, newRunner func(k6Path string) workload.Runner, newReader func(metricsURL string, timeout time.Duration) (telemetry.Reader, error)) int {
	c, err := parseArgs(args, stderr)
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stderr, "ruptiq-diagnose:", workload.RedactText(err.Error(), ""))
		}
		return exitUsage
	}
	reader, err := newReader(c.diagnosis.Experiment.MetricsURL, c.scrapeTimeout)
	if err != nil {
		fmt.Fprintln(stderr, "ruptiq-diagnose:", workload.RedactText(err.Error(), c.diagnosis.Experiment.MetricsURL))
		return exitUsage
	}

	e := c.diagnosis.Experiment
	fmt.Fprintf(stdout, "Diagnosing %d virtual users for %v against %s,\nsampling %s every %v (timeout %v).\nOutput directory: %s\n\n",
		e.Workload.VUs, e.Workload.Duration, workload.RedactURL(e.Workload.TargetURL),
		workload.RedactURL(e.MetricsURL), e.Interval, e.Timeout, c.diagnosis.OutputDir)

	res, err := diagnosis.Run(ctx, c.diagnosis, diagnosis.Inputs{Runner: newRunner(k6Executable(c.k6Path)), Reader: reader})
	if res.Manifest.SchemaVersion != 0 {
		if werr := diagnosis.WriteSummary(stdout, res); werr != nil {
			fmt.Fprintln(stderr, "ruptiq-diagnose:", workload.RedactText(werr.Error(), e.Workload.TargetURL))
		}
	}
	if err != nil {
		fmt.Fprintln(stderr, "ruptiq-diagnose:", workload.RedactText(err.Error(), e.Workload.TargetURL))
		var verr *diagnosis.ValidationError
		if errors.As(err, &verr) {
			return exitUsage
		}
		return exitFailed
	}
	return exitCompleted
}

// k6Executable resolves a path-like -k6 value against the current directory.
// k6 is started with a temporary working directory, so a relative path would
// otherwise be looked up there. A bare name such as "k6" is left for PATH.
func k6Executable(path string) string {
	if path == "" || !strings.ContainsAny(path, `/\`) {
		return path
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return abs
}
