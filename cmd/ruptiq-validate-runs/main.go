// Command ruptiq-validate-runs compares two saved ruptiq-diagnose directories.
// It does not run a workload, contact a service, or change a deployment.
//
//	ruptiq-validate-runs -before-dir tmp/diagnosis-before -after-dir tmp/diagnosis-after -out-dir tmp/validation-pair
//
// Exit status: 0 the analyses were compared (including not comparable or
// insufficient evidence), 2 invalid arguments or an output collision, 3 a
// diagnosis could not be read or the result could not be written.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/RajK158/ruptiq/internal/analysis"
	"github.com/RajK158/ruptiq/internal/fixvalidation"
	"github.com/RajK158/ruptiq/internal/runvalidation"
	"github.com/RajK158/ruptiq/internal/workload"
)

const (
	exitCompared = 0
	exitUsage    = 2
	exitFailed   = 3
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	cfg, err := parseArgs(args, stderr)
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stderr, "ruptiq-validate-runs:", workload.RedactText(err.Error(), ""))
		}
		return exitUsage
	}
	res, err := runvalidation.Run(cfg, runvalidation.Inputs{})
	if err != nil {
		fmt.Fprintln(stderr, "ruptiq-validate-runs:", workload.RedactText(err.Error(), ""))
		var verr *runvalidation.ValidationError
		if errors.As(err, &verr) {
			return exitUsage
		}
		return exitFailed
	}
	if err := runvalidation.WriteSummary(stdout, res); err != nil {
		fmt.Fprintln(stderr, "ruptiq-validate-runs:", workload.RedactText(err.Error(), ""))
		return exitFailed
	}
	fmt.Fprintf(stdout, "\nValidation written to %s\n", cfg.OutputDir)
	return exitCompared
}

func parseArgs(args []string, stderr io.Writer) (runvalidation.Config, error) {
	fs := flag.NewFlagSet("ruptiq-validate-runs", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: ruptiq-validate-runs -before-dir DIR -after-dir DIR -out-dir DIR [flags]")
		fmt.Fprintln(stderr, "Compares two saved ruptiq-diagnose directories. An observed difference is not proof of a cause, and this command does not apply a change.")
		fs.SetOutput(stderr)
		fs.PrintDefaults()
		fs.SetOutput(io.Discard)
	}
	var c runvalidation.Config
	def := fixvalidation.DefaultConfig()
	fs.StringVar(&c.BeforeDir, "before-dir", "", "saved ruptiq-diagnose directory for the earlier run (required)")
	fs.StringVar(&c.AfterDir, "after-dir", "", "saved ruptiq-diagnose directory for the later run (required)")
	fs.StringVar(&c.OutputDir, "out-dir", "", "directory for validation.json and manifest.json (required)")
	fs.Int64Var(&c.MaxInputBytes, "max-input-bytes", analysis.DefaultMaxInputBytes, "largest diagnosis artifact accepted, in bytes")
	fs.IntVar(&c.Validation.MinWindows, "min-windows", def.MinWindows, "fewest eligible windows required in each experiment")
	fs.Float64Var(&c.Validation.MinRequests, "min-requests", def.MinRequests, "fewest completed requests required before a relative change is judged")
	fs.Float64Var(&c.Validation.MinServerErrors, "min-server-errors", def.MinServerErrors, "fewest 5xx responses on the worse side")
	fs.Float64Var(&c.Validation.ErrorRatioIncrease, "error-ratio-increase", def.ErrorRatioIncrease, "smallest 5xx-ratio change, as a fraction (0.01 = 1 percentage point)")
	fs.Float64Var(&c.Validation.ErrorRatioFactor, "error-ratio-factor", def.ErrorRatioFactor, "how many times the better 5xx ratio the worse ratio must be")
	fs.Float64Var(&c.Validation.LatencyChange, "latency-change", def.LatencyChange, "smallest relative change of the median per-window p95 (0.5 = 50%)")
	fs.Float64Var(&c.Validation.ThroughputChange, "throughput-change", def.ThroughputChange, "smallest relative change of server throughput (0.3 = 30%)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return runvalidation.Config{}, err
		}
		return runvalidation.Config{}, fmt.Errorf("%s", workload.RedactText(err.Error(), ""))
	}
	if fs.NArg() > 0 {
		shown := workload.RedactText(fmt.Sprintf("%q", fs.Args()), "")
		return runvalidation.Config{}, fmt.Errorf("unexpected arguments: %s", shown)
	}
	c = c.WithDefaults()
	if err := c.Validate(); err != nil {
		return runvalidation.Config{}, fmt.Errorf("%s", workload.RedactText(err.Error(), ""))
	}
	return c, nil
}
