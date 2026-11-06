// Command ruptiq-compare compares two saved ruptiq-run reports (a baseline and
// a candidate) and describes how the candidate's measurements differ. It
// reads only the two files; it never runs a load test or contacts a target.
//
//	ruptiq-compare -baseline baseline.json -candidate candidate.json -out comparison.json
//
// Exit status: 0 the runs were compared, 1 the runs are not comparable
// (different target URL, virtual users, or configured duration), 2 invalid
// arguments, 3 a report could not be read or is invalid, unsupported, or
// records a run that did not complete.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/RajK158/ruptiq/internal/comparison"
)

const (
	exitCompared     = 0
	exitIncomparable = 1
	exitUsage        = 2
	exitBadReport    = 3
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

type config struct {
	baseline, candidate, out string
}

func parseArgs(args []string, stderr io.Writer) (config, error) {
	fs := flag.NewFlagSet("ruptiq-compare", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: ruptiq-compare -baseline FILE -candidate FILE [-out FILE]")
		fmt.Fprintln(stderr, "Compares two saved ruptiq-run reports (written with ruptiq-run -out).")
		fs.PrintDefaults()
	}
	var c config
	fs.StringVar(&c.baseline, "baseline", "", "saved ruptiq-run report of the baseline run (required)")
	fs.StringVar(&c.candidate, "candidate", "", "saved ruptiq-run report of the candidate run (required)")
	fs.StringVar(&c.out, "out", "", "also write the comparison as JSON to this `file` (optional)")
	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	if fs.NArg() > 0 {
		return config{}, fmt.Errorf("unexpected arguments: %q", fs.Args())
	}
	switch {
	case c.baseline == "" && c.candidate == "":
		return config{}, errors.New("-baseline and -candidate are required")
	case c.baseline == "":
		return config{}, errors.New("-baseline is required")
	case c.candidate == "":
		return config{}, errors.New("-candidate is required")
	}
	if c.out != "" && (samePath(c.out, c.baseline) || samePath(c.out, c.candidate)) {
		return config{}, errors.New("-out must not be one of the input reports")
	}
	return c, nil
}

// run is main without process-level side effects, for tests.
func run(args []string, stdout, stderr io.Writer) int {
	c, err := parseArgs(args, stderr)
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stderr, "ruptiq-compare:", err)
		}
		return exitUsage
	}

	report := comparison.Report{SchemaVersion: comparison.SchemaVersion}
	code := compare(c, &report, stdout, stderr)

	if c.out != "" {
		if err := writeReport(c.out, report); err != nil {
			fmt.Fprintln(stderr, "ruptiq-compare: writing comparison:", err)
			return exitBadReport
		}
		fmt.Fprintln(stdout, "Comparison written to", c.out)
	}
	return code
}

// compare fills report and prints the result. It returns the exit status.
func compare(c config, report *comparison.Report, stdout, stderr io.Writer) int {
	badReport := func(err error) int {
		report.Error = err.Error()
		fmt.Fprintln(stderr, "ruptiq-compare:", err)
		return exitBadReport
	}
	baseline, err := load("baseline", c.baseline)
	if err != nil {
		return badReport(err)
	}
	candidate, err := load("candidate", c.candidate)
	if err != nil {
		return badReport(err)
	}

	cmp, err := comparison.Compare(baseline, candidate)
	var inc *comparison.IncomparableError
	if errors.As(err, &inc) {
		report.Error, report.Mismatches = err.Error(), inc.Mismatches
		fmt.Fprintln(stderr, "ruptiq-compare: the runs are not comparable; no comparison was made:")
		for _, m := range inc.Mismatches {
			fmt.Fprintln(stderr, "  -", m)
		}
		return exitIncomparable
	}
	if err != nil {
		return badReport(err)
	}

	cmp.Baseline.Source, cmp.Candidate.Source = c.baseline, c.candidate
	report.Comparison = &cmp
	if err := comparison.WriteText(stdout, cmp); err != nil {
		fmt.Fprintln(stderr, "ruptiq-compare:", err)
		return exitBadReport
	}
	return exitCompared
}

func load(role, path string) (comparison.Run, error) {
	f, err := os.Open(path)
	if err != nil {
		return comparison.Run{}, fmt.Errorf("%s report: %w", role, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, comparison.MaxReportBytes+1))
	if err != nil {
		return comparison.Run{}, fmt.Errorf("%s report %s: %w", role, path, err)
	}
	run, err := comparison.DecodeReport(data)
	if err != nil {
		return comparison.Run{}, fmt.Errorf("%s report %s: %w", role, path, err)
	}
	return run, nil
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
func writeReport(path string, report comparison.Report) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), ".ruptiq-compare-*.json")
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
