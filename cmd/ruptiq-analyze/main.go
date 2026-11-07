// Command ruptiq-analyze extracts performance signals from a saved
// ruptiq-experiment report: server request throughput, HTTP 5xx rates, and
// estimated latency percentiles, per window between telemetry samples. It
// reads only the report; it never runs a load test or contacts a target.
//
//	ruptiq-analyze -in experiment.json -out analysis.json
//
// Exit status: 0 the report was analysed, 2 invalid arguments, 3 the report
// could not be read, is invalid or unsupported, records an experiment whose
// workload did not complete, or the analysis could not be written.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/RajK158/ruptiq/internal/analysis"
)

const (
	exitAnalysed  = 0
	exitUsage     = 2
	exitBadReport = 3
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

type config struct {
	in, out       string
	maxInputBytes int64
	analysis      analysis.Config
}

func parseArgs(args []string, stderr io.Writer) (config, error) {
	fs := flag.NewFlagSet("ruptiq-analyze", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: ruptiq-analyze -in FILE [-out FILE] [flags]")
		fmt.Fprintln(stderr, "Extracts performance signals from a saved ruptiq-experiment report (written with ruptiq-experiment -out).")
		fs.PrintDefaults()
	}
	var c config
	def := analysis.DefaultConfig()
	fs.StringVar(&c.in, "in", "", "saved ruptiq-experiment report `file` (required)")
	fs.StringVar(&c.out, "out", "", "also write the analysis as JSON to this `file` (optional)")
	fs.Int64Var(&c.maxInputBytes, "max-input-bytes", analysis.DefaultMaxInputBytes, "largest report accepted, in bytes")
	fs.StringVar(&c.analysis.RequestsCounter, "requests-metric", def.RequestsCounter, "counter `family` of completed HTTP requests")
	fs.StringVar(&c.analysis.DurationHistogram, "duration-metric", def.DurationHistogram, "histogram `family` of HTTP request durations, in seconds")
	fs.StringVar(&c.analysis.MethodLabel, "method-label", def.MethodLabel, "label holding the HTTP method (empty: do not split by method)")
	fs.StringVar(&c.analysis.RouteLabel, "route-label", def.RouteLabel, "label holding the route template (empty: do not split by route)")
	fs.StringVar(&c.analysis.StatusLabel, "status-label", def.StatusLabel, "label holding the HTTP status code (empty: 5xx rates unavailable)")
	fs.IntVar(&c.analysis.MinObservations, "min-observations", def.MinObservations, "fewest requests in a window for a p95 estimate")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if fs.NArg() > 0 {
		return config{}, fmt.Errorf("unexpected arguments: %q", fs.Args())
	}
	if c.in == "" {
		return config{}, errors.New("-in is required")
	}
	if c.out != "" && samePath(c.out, c.in) {
		return config{}, errors.New("-out must not be the input report")
	}
	if c.maxInputBytes < analysis.MinInputBytes || c.maxInputBytes > analysis.MaxInputBytesLimit {
		return config{}, fmt.Errorf("-max-input-bytes must be between %d and %d, got %d",
			analysis.MinInputBytes, analysis.MaxInputBytesLimit, c.maxInputBytes)
	}
	if err := c.analysis.Validate(); err != nil {
		return config{}, err
	}
	return c, nil
}

// run is main without process-level side effects, for tests.
func run(args []string, stdout, stderr io.Writer) int {
	c, err := parseArgs(args, stderr)
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stderr, "ruptiq-analyze:", err)
		}
		return exitUsage
	}

	report := analysis.Report{SchemaVersion: analysis.SchemaVersion, Input: c.in}
	code := exitAnalysed
	a, err := analyze(c)
	if err != nil {
		report.Error = err.Error()
		fmt.Fprintln(stderr, "ruptiq-analyze:", err)
		code = exitBadReport
	} else {
		report.Analysis = &a
		if err := analysis.WriteSummary(stdout, a); err != nil {
			fmt.Fprintln(stderr, "ruptiq-analyze:", err)
		}
	}

	if c.out != "" {
		if err := writeReport(c.out, report); err != nil {
			fmt.Fprintln(stderr, "ruptiq-analyze: writing analysis:", err)
			return exitBadReport
		}
		fmt.Fprintln(stdout, "\nAnalysis written to", c.out)
	}
	return code
}

func analyze(c config) (analysis.Analysis, error) {
	f, err := os.Open(c.in)
	if err != nil {
		return analysis.Analysis{}, fmt.Errorf("experiment report: %w", err)
	}
	defer f.Close()
	exp, err := analysis.ReadExperiment(f, c.maxInputBytes)
	if err != nil {
		return analysis.Analysis{}, fmt.Errorf("experiment report %s: %w", c.in, err)
	}
	return analysis.Analyze(exp, c.analysis)
}

func samePath(a, b string) bool {
	if fa, err := os.Stat(a); err == nil {
		if fb, err := os.Stat(b); err == nil {
			return os.SameFile(fa, fb)
		}
	}
	aa, errA := filepath.Abs(a)
	bb, errB := filepath.Abs(b)
	return errA == nil && errB == nil && filepath.Clean(aa) == filepath.Clean(bb)
}

// writeReport writes report as indented JSON through a temporary file in the
// same directory, so a reader never sees a partial file.
func writeReport(path string, report analysis.Report) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), ".ruptiq-analyze-*.json")
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
