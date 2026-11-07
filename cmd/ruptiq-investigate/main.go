// Command ruptiq-investigate looks for performance changes in a saved
// ruptiq-analyze report and prints ranked, evidence-backed findings: server
// error increases, latency and throughput degradation, and the routes for
// which the evidence is insufficient. Findings are observations and
// hypotheses to test, never proven root causes. It reads only the analysis;
// it never runs a load test or contacts a target.
//
//	ruptiq-investigate -in analysis.json -out investigation.json
//
// Exit status: 0 the analysis was investigated (including when the evidence
// is insufficient), 2 invalid arguments, 3 the analysis could not be read, is
// invalid or unsupported, records a failed analysis, or the investigation
// could not be written.
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
	"github.com/RajK158/ruptiq/internal/investigation"
)

const (
	exitInvestigated = 0
	exitUsage        = 2
	exitBadInput     = 3
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

type config struct {
	in, out       string
	maxInputBytes int64
	investigation investigation.Config
}

func parseArgs(args []string, stderr io.Writer) (config, error) {
	fs := flag.NewFlagSet("ruptiq-investigate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: ruptiq-investigate -in FILE [-out FILE] [flags]")
		fmt.Fprintln(stderr, "Ranks evidence-backed performance findings in a saved ruptiq-analyze report (written with ruptiq-analyze -out).")
		fs.PrintDefaults()
	}
	var c config
	def := investigation.DefaultConfig()
	fs.StringVar(&c.in, "in", "", "saved ruptiq-analyze report `file` (required)")
	fs.StringVar(&c.out, "out", "", "also write the investigation as JSON to this `file` (optional)")
	fs.Int64Var(&c.maxInputBytes, "max-input-bytes", analysis.DefaultMaxInputBytes, "largest analysis accepted, in bytes")
	fs.IntVar(&c.investigation.MinWindows, "min-windows", def.MinWindows, "fewest usable windows in each of the baseline and the comparison")
	fs.Float64Var(&c.investigation.MinRequests, "min-requests", def.MinRequests, "fewest requests (or latency observations) per side")
	fs.Float64Var(&c.investigation.MinServerErrors, "min-server-errors", def.MinServerErrors, "fewest 5xx responses in the comparison for a server error increase")
	fs.Float64Var(&c.investigation.ErrorRatioIncrease, "error-ratio-increase", def.ErrorRatioIncrease, "smallest rise of the 5xx ratio, as a fraction (0.01 = 1 percentage point)")
	fs.Float64Var(&c.investigation.ErrorRatioFactor, "error-ratio-factor", def.ErrorRatioFactor, "how many times a non-zero baseline 5xx ratio the comparison's must be")
	fs.Float64Var(&c.investigation.LatencyIncrease, "latency-increase", def.LatencyIncrease, "smallest relative rise of the p95 estimate (0.5 = +50%)")
	fs.Float64Var(&c.investigation.ThroughputDecrease, "throughput-decrease", def.ThroughputDecrease, "smallest relative fall of the request rate (0.3 = -30%)")
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
		return config{}, errors.New("-out must not be the input analysis")
	}
	if c.maxInputBytes < analysis.MinInputBytes || c.maxInputBytes > analysis.MaxInputBytesLimit {
		return config{}, fmt.Errorf("-max-input-bytes must be between %d and %d, got %d",
			analysis.MinInputBytes, analysis.MaxInputBytesLimit, c.maxInputBytes)
	}
	if err := c.investigation.Validate(); err != nil {
		return config{}, err
	}
	return c, nil
}

// run is main without process-level side effects, for tests.
func run(args []string, stdout, stderr io.Writer) int {
	c, err := parseArgs(args, stderr)
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stderr, "ruptiq-investigate:", err)
		}
		return exitUsage
	}

	report := investigation.Report{SchemaVersion: investigation.SchemaVersion, Input: c.in}
	code := exitInvestigated
	inv, err := investigate(c)
	if err != nil {
		report.Error = err.Error()
		fmt.Fprintln(stderr, "ruptiq-investigate:", err)
		code = exitBadInput
	} else {
		report.Investigation = &inv
		if err := investigation.WriteSummary(stdout, inv); err != nil {
			fmt.Fprintln(stderr, "ruptiq-investigate:", err)
		}
	}

	if c.out != "" {
		if err := writeReport(c.out, report); err != nil {
			fmt.Fprintln(stderr, "ruptiq-investigate: writing investigation:", err)
			return exitBadInput
		}
		fmt.Fprintln(stdout, "\nInvestigation written to", c.out)
	}
	return code
}

func investigate(c config) (investigation.Investigation, error) {
	f, err := os.Open(c.in)
	if err != nil {
		return investigation.Investigation{}, fmt.Errorf("analysis file: %w", err)
	}
	defer f.Close()
	a, err := investigation.ReadAnalysis(f, c.maxInputBytes)
	if err != nil {
		return investigation.Investigation{}, fmt.Errorf("analysis file %s: %w", c.in, err)
	}
	return investigation.Investigate(a, c.investigation)
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
func writeReport(path string, report investigation.Report) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), ".ruptiq-investigate-*.json")
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
