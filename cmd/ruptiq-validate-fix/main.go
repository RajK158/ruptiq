// Command ruptiq-validate-fix compares two saved ruptiq-analyze reports: a
// baseline experiment and a candidate experiment after a proposed fix. It
// prints route-level server-side differences and writes versioned JSON.
//
// An observed difference is not proof that a code change caused it, and it
// is not a statistical significance test.
//
//	ruptiq-validate-fix -before baseline-analysis.json -after candidate-analysis.json -out validation.json
//
// Exit status: 0 the reports were compared (including not comparable or
// insufficient evidence), 2 invalid arguments, 3 a report could not be read,
// is invalid or unsupported, or the result could not be written.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/RajK158/ruptiq/internal/analysis"
	"github.com/RajK158/ruptiq/internal/fixvalidation"
	"github.com/RajK158/ruptiq/internal/investigation"
	"github.com/RajK158/ruptiq/internal/workload"
)

const (
	exitCompared = 0
	exitUsage    = 2
	exitBadInput = 3
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

type config struct {
	before, after string
	out           string
	maxInputBytes int64
	validation    fixvalidation.Config
}

func parseArgs(args []string, stderr io.Writer) (config, error) {
	fs := flag.NewFlagSet("ruptiq-validate-fix", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: ruptiq-validate-fix -before FILE -after FILE [-out FILE] [flags]")
		fmt.Fprintln(stderr, "Compares two saved ruptiq-analyze reports. An observed difference is not proof of a cause.")
		fs.PrintDefaults()
	}
	var c config
	def := fixvalidation.DefaultConfig()
	fs.StringVar(&c.before, "before", "", "saved ruptiq-analyze report from the baseline experiment (required)")
	fs.StringVar(&c.after, "after", "", "saved ruptiq-analyze report from the candidate experiment (required)")
	fs.StringVar(&c.out, "out", "", "write the comparison as JSON to this file (optional)")
	fs.Int64Var(&c.maxInputBytes, "max-input-bytes", analysis.DefaultMaxInputBytes, "largest analysis accepted, in bytes")
	fs.IntVar(&c.validation.MinWindows, "min-windows", def.MinWindows, "fewest eligible windows required in each experiment")
	fs.Float64Var(&c.validation.MinRequests, "min-requests", def.MinRequests, "fewest completed requests required before a relative change is judged")
	fs.Float64Var(&c.validation.MinServerErrors, "min-server-errors", def.MinServerErrors, "fewest 5xx responses on the worse side")
	fs.Float64Var(&c.validation.ErrorRatioIncrease, "error-ratio-increase", def.ErrorRatioIncrease, "smallest 5xx-ratio change, as a fraction (0.01 = 1 percentage point)")
	fs.Float64Var(&c.validation.ErrorRatioFactor, "error-ratio-factor", def.ErrorRatioFactor, "how many times the better 5xx ratio the worse ratio must be")
	fs.Float64Var(&c.validation.LatencyChange, "latency-change", def.LatencyChange, "smallest relative change of the median per-window p95 (0.5 = 50%)")
	fs.Float64Var(&c.validation.ThroughputChange, "throughput-change", def.ThroughputChange, "smallest relative change of server throughput (0.3 = 30%)")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if fs.NArg() > 0 {
		return config{}, fmt.Errorf("unexpected arguments: %q", fs.Args())
	}
	if c.before == "" || c.after == "" {
		return config{}, errors.New("-before and -after are required")
	}
	if c.out != "" && (samePath(c.out, c.before) || samePath(c.out, c.after)) {
		return config{}, errors.New("-out must not replace either input analysis")
	}
	if c.maxInputBytes < analysis.MinInputBytes || c.maxInputBytes > analysis.MaxInputBytesLimit {
		return config{}, fmt.Errorf("-max-input-bytes must be between %d and %d, got %d",
			analysis.MinInputBytes, analysis.MaxInputBytesLimit, c.maxInputBytes)
	}
	if err := c.validation.Validate(); err != nil {
		return config{}, err
	}
	return c, nil
}

func run(args []string, stdout, stderr io.Writer) int {
	c, err := parseArgs(args, stderr)
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stderr, "ruptiq-validate-fix:", redact(err.Error()))
		}
		return exitUsage
	}

	report := fixvalidation.Report{SchemaVersion: fixvalidation.SchemaVersion, Before: c.before, After: c.after}
	code := exitCompared
	v, err := validate(c)
	if err != nil {
		report.Error = redact(err.Error())
		fmt.Fprintln(stderr, "ruptiq-validate-fix:", report.Error)
		code = exitBadInput
	} else {
		report.Validation = &v
		if err := fixvalidation.WriteSummary(stdout, v); err != nil {
			fmt.Fprintln(stderr, "ruptiq-validate-fix:", redact(err.Error()))
		}
	}
	if c.out != "" {
		if err := writeReport(c.out, report); err != nil {
			fmt.Fprintln(stderr, "ruptiq-validate-fix: writing validation:", redact(err.Error()))
			return exitBadInput
		}
		fmt.Fprintln(stdout, "\nValidation written to", c.out)
	}
	return code
}

func validate(c config) (fixvalidation.Validation, error) {
	before, err := readAnalysis(c.before, c.maxInputBytes)
	if err != nil {
		return fixvalidation.Validation{}, fmt.Errorf("before: %w", err)
	}
	after, err := readAnalysis(c.after, c.maxInputBytes)
	if err != nil {
		return fixvalidation.Validation{}, fmt.Errorf("after: %w", err)
	}
	return fixvalidation.Compare(before, after, c.validation)
}

func readAnalysis(path string, maxBytes int64) (analysis.Analysis, error) {
	f, err := os.Open(path)
	if err != nil {
		return analysis.Analysis{}, fmt.Errorf("analysis file: %w", err)
	}
	defer f.Close()
	a, err := investigation.ReadAnalysis(f, maxBytes)
	if err != nil {
		return analysis.Analysis{}, fmt.Errorf("analysis file %s: %w", filepath.Base(path), err)
	}
	return a, nil
}

func samePath(a, b string) bool {
	aa, errA := filepath.Abs(a)
	bb, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return false
	}
	aa, bb = filepath.Clean(aa), filepath.Clean(bb)
	if aa == bb || (runtime.GOOS == "windows" && strings.EqualFold(aa, bb)) {
		return true
	}
	fa, errFa := os.Stat(aa)
	fb, errFb := os.Stat(bb)
	return errFa == nil && errFb == nil && os.SameFile(fa, fb)
}

func writeReport(path string, report fixvalidation.Report) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if dir == "" {
		dir = "."
	}
	tmp, err := os.CreateTemp(dir, ".ruptiq-validate-fix-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
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

func redact(s string) string {
	return workload.RedactText(s, "")
}
