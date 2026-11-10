// Command ruptiq-pair captures a baseline diagnosis, captures a candidate
// diagnosis, and compares them. Each stage is a separate invocation. The
// process exits after the stage finishes. It does not apply a code or
// deployment change.
//
//	ruptiq-pair -stage before  -pair-dir tmp/my-validation -url URL -metrics-url URL -k6 PATH_TO_K6
//	ruptiq-pair -stage after   -pair-dir tmp/my-validation -url URL -metrics-url URL -k6 PATH_TO_K6
//	ruptiq-pair -stage compare -pair-dir tmp/my-validation
//
// Exit status: 0 the stage finished (a comparison includes not_comparable and
// insufficient_evidence), 2 invalid arguments, a configuration mismatch, a
// reservation, or a refusal to overwrite saved reports, 3 a stage failed or a
// saved manifest could not be read or written.
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
	"github.com/RajK158/ruptiq/internal/experiment"
	"github.com/RajK158/ruptiq/internal/fixvalidation"
	"github.com/RajK158/ruptiq/internal/investigation"
	"github.com/RajK158/ruptiq/internal/pairworkflow"
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

type parsed struct {
	cfg           pairworkflow.Config
	k6Path        string
	scrapeTimeout time.Duration
}

func parseArgs(args []string, stderr io.Writer) (parsed, error) {
	fs := flag.NewFlagSet("ruptiq-pair", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: ruptiq-pair -stage before|after|compare -pair-dir DIR [flags]")
		fmt.Fprintln(stderr, "before saves a baseline and exits. Change the service outside Ruptiq, then run after. compare reads the two saved diagnoses and does not start a workload.")
		fs.SetOutput(stderr)
		fs.PrintDefaults()
		fs.SetOutput(io.Discard)
	}

	var p parsed
	e := &p.cfg.Diagnosis.Experiment
	a := &p.cfg.Diagnosis.Analysis
	inv := &p.cfg.Diagnosis.Investigation
	aDef := analysis.DefaultConfig()
	iDef := investigation.DefaultConfig()
	vDef := fixvalidation.DefaultConfig()
	*a = aDef
	*inv = iDef
	defaultK6 := os.Getenv(k6PathEnv)
	if defaultK6 == "" {
		defaultK6 = k6.DefaultExecutable
	}

	fs.StringVar(&p.cfg.Stage, "stage", "", "workflow stage: before, after, or compare (required)")
	fs.StringVar(&p.cfg.PairDir, "pair-dir", "", "directory for before/, after/, comparison/, and pair-manifest.json (required)")
	fs.StringVar(&e.Workload.TargetURL, "url", "", "target URL for before and after (required for those stages)")
	fs.StringVar(&e.MetricsURL, "metrics-url", "", "Prometheus metrics URL for before and after (required for those stages)")
	fs.IntVar(&e.Workload.VUs, "vus", workload.DefaultVUs, "number of concurrent virtual users")
	fs.DurationVar(&e.Workload.Duration, "duration", workload.DefaultDuration, "how long to apply load, in whole seconds")
	fs.DurationVar(&e.Interval, "interval", experiment.DefaultInterval, "time between metrics scrapes while the load runs")
	fs.IntVar(&e.MaxSamples, "max-samples", experiment.DefaultMaxSamples, "most scrapes in one experiment, including before and after the load")
	fs.Int64Var(&e.MaxTelemetryBytes, "max-telemetry-bytes", experiment.DefaultMaxTelemetryBytes, "most sample data (JSON bytes) kept from one experiment")
	fs.DurationVar(&e.Timeout, "timeout", 0, "hard limit for one experiment (default: duration + 60s)")
	fs.DurationVar(&p.scrapeTimeout, "scrape-timeout", defaultScrapeTimeout, "maximum time for one metrics scrape")
	fs.StringVar(&p.k6Path, "k6", defaultK6, "k6 executable name or path (default from "+k6PathEnv+", else k6 on PATH)")
	fs.Float64Var(&e.MaxFailureRate, "max-failure-rate", 0, "highest failure rate (0 to 1) for the load test to pass")
	fs.Int64Var(&p.cfg.MaxInputBytes, "max-input-bytes", analysis.DefaultMaxInputBytes, "largest diagnosis artifact compare will read, in bytes")

	fs.StringVar(&a.RequestsCounter, "requests-metric", aDef.RequestsCounter, "counter family of completed HTTP requests")
	fs.StringVar(&a.DurationHistogram, "duration-metric", aDef.DurationHistogram, "histogram family of HTTP request durations, in seconds")
	fs.StringVar(&a.MethodLabel, "method-label", aDef.MethodLabel, "label holding the HTTP method")
	fs.StringVar(&a.RouteLabel, "route-label", aDef.RouteLabel, "label holding the route template")
	fs.StringVar(&a.StatusLabel, "status-label", aDef.StatusLabel, "label holding the HTTP status code")
	fs.IntVar(&a.MinObservations, "min-observations", aDef.MinObservations, "fewest requests in a window for a p95 estimate")

	fs.IntVar(&inv.MinWindows, "min-windows", iDef.MinWindows, "fewest usable windows in each half of one investigation")
	fs.Float64Var(&inv.MinRequests, "min-requests", iDef.MinRequests, "fewest requests (or latency observations) per investigation side")
	fs.Float64Var(&inv.MinServerErrors, "min-server-errors", iDef.MinServerErrors, "fewest 5xx responses for an investigation error increase")
	fs.Float64Var(&inv.ErrorRatioIncrease, "error-ratio-increase", iDef.ErrorRatioIncrease, "smallest 5xx-ratio rise for an investigation, as a fraction")
	fs.Float64Var(&inv.ErrorRatioFactor, "error-ratio-factor", iDef.ErrorRatioFactor, "how many times a non-zero baseline 5xx ratio the comparison's must be")
	fs.Float64Var(&inv.LatencyIncrease, "latency-increase", iDef.LatencyIncrease, "smallest relative p95 rise for an investigation")
	fs.Float64Var(&inv.ThroughputDecrease, "throughput-decrease", iDef.ThroughputDecrease, "smallest relative throughput fall for an investigation")

	fs.IntVar(&p.cfg.Validation.MinWindows, "compare-min-windows", vDef.MinWindows, "fewest eligible windows required in each experiment")
	fs.Float64Var(&p.cfg.Validation.MinRequests, "compare-min-requests", vDef.MinRequests, "fewest completed requests required before a relative change is judged")
	fs.Float64Var(&p.cfg.Validation.MinServerErrors, "compare-min-server-errors", vDef.MinServerErrors, "fewest 5xx responses on the worse side")
	fs.Float64Var(&p.cfg.Validation.ErrorRatioIncrease, "compare-error-ratio-increase", vDef.ErrorRatioIncrease, "smallest 5xx-ratio change, as a fraction")
	fs.Float64Var(&p.cfg.Validation.ErrorRatioFactor, "compare-error-ratio-factor", vDef.ErrorRatioFactor, "how many times the better 5xx ratio the worse ratio must be")
	fs.Float64Var(&p.cfg.Validation.LatencyChange, "compare-latency-change", vDef.LatencyChange, "smallest relative change of the median per-window p95")
	fs.Float64Var(&p.cfg.Validation.ThroughputChange, "compare-throughput-change", vDef.ThroughputChange, "smallest relative change of server throughput")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return parsed{}, err
		}
		return parsed{}, fmt.Errorf("%s", workload.RedactText(err.Error(), ""))
	}
	if fs.NArg() > 0 {
		shown := workload.RedactText(fmt.Sprintf("%q", fs.Args()), "")
		return parsed{}, fmt.Errorf("unexpected arguments: %s", shown)
	}
	p.cfg = p.cfg.WithDefaults()
	var problems []string
	if err := p.cfg.Validate(); err != nil {
		var verr *pairworkflow.ValidationError
		if errors.As(err, &verr) {
			problems = append(problems, verr.Problems...)
		} else {
			problems = append(problems, err.Error())
		}
	}
	if p.cfg.Stage != pairworkflow.StageCompare {
		if p.scrapeTimeout <= 0 {
			problems = append(problems, fmt.Sprintf("scrape timeout must be positive, got %v", p.scrapeTimeout))
		}
		if p.k6Path == "" {
			problems = append(problems, "k6 path must not be empty")
		}
	}
	if len(problems) > 0 {
		for i, problem := range problems {
			problems[i] = workload.RedactText(problem, "")
		}
		return parsed{}, &pairworkflow.ValidationError{Problems: problems}
	}
	return p, nil
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, newRunner func(k6Path string) workload.Runner, newReader func(metricsURL string, timeout time.Duration) (telemetry.Reader, error)) int {
	p, err := parseArgs(args, stderr)
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stderr, "ruptiq-pair:", workload.RedactText(err.Error(), ""))
		}
		return exitUsage
	}
	var in pairworkflow.Inputs
	if p.cfg.Stage != pairworkflow.StageCompare {
		reader, err := newReader(p.cfg.Diagnosis.Experiment.MetricsURL, p.scrapeTimeout)
		if err != nil {
			fmt.Fprintln(stderr, "ruptiq-pair:", workload.RedactText(err.Error(), p.cfg.Diagnosis.Experiment.MetricsURL))
			return exitUsage
		}
		in.Runner = newRunner(k6Executable(p.k6Path))
		in.Reader = reader
		e := p.cfg.Diagnosis.Experiment
		fmt.Fprintf(stdout, "Pair stage %s for %d virtual users for %v against %s,\nsampling %s every %v.\n",
			p.cfg.Stage, e.Workload.VUs, e.Workload.Duration, workload.RedactURL(e.Workload.TargetURL),
			workload.RedactURL(e.MetricsURL), e.Interval)
	} else {
		fmt.Fprintf(stdout, "Pair stage compare. No workload is started.\n")
	}
	fmt.Fprintf(stdout, "Pair directory: %s\n\n", p.cfg.PairDir)

	res, err := pairworkflow.Run(ctx, p.cfg, in)
	if err != nil {
		fmt.Fprintln(stderr, "ruptiq-pair:", workload.RedactText(err.Error(), ""))
		var verr *pairworkflow.ValidationError
		if errors.As(err, &verr) {
			return exitUsage
		}
		return exitFailed
	}
	if werr := pairworkflow.WriteSummary(stdout, res); werr != nil {
		fmt.Fprintln(stderr, "ruptiq-pair:", workload.RedactText(werr.Error(), ""))
		return exitFailed
	}
	return exitCompleted
}

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
