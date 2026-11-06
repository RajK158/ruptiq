package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RajK158/ruptiq/internal/comparison"
)

func fixturePath(name string) string {
	return filepath.Join("..", "..", "internal", "comparison", "testdata", name)
}

type outcome struct {
	code           int
	stdout, stderr string
}

func runCLI(args ...string) outcome {
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return outcome{code, stdout.String(), stderr.String()}
}

func readComparison(t *testing.T, path string) comparison.Report {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var r comparison.Report
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatalf("comparison file is not valid JSON: %v\n%s", err, data)
	}
	return r
}

// writeFile writes a report into a temporary directory and returns its path.
func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCompareAndWriteJSON(t *testing.T) {
	out := filepath.Join(t.TempDir(), "comparison.json")
	o := runCLI("-baseline", fixturePath("baseline.json"), "-candidate", fixturePath("candidate_regression.json"), "-out", out)
	if o.code != exitCompared || o.stderr != "" {
		t.Fatalf("code = %d, stderr = %s", o.code, o.stderr)
	}
	for _, want := range []string{
		"Ruptiq run comparison",
		"  Average latency (ms)   2.000      2.500       +0.500     +25.0%",
		"FAILED its pass criteria",
		"Comparison written to " + out,
	} {
		if !strings.Contains(o.stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, o.stdout)
		}
	}

	r := readComparison(t, out)
	if r.SchemaVersion != comparison.SchemaVersion || r.Error != "" || r.Comparison == nil {
		t.Fatalf("report = %+v", r)
	}
	c := r.Comparison
	if c.Baseline.Source != fixturePath("baseline.json") || c.Candidate.Source != fixturePath("candidate_regression.json") {
		t.Errorf("sources = %q, %q", c.Baseline.Source, c.Candidate.Source)
	}
	if len(c.Metrics) != 7 || c.Metrics[0].Change != 0.5 || *c.Metrics[0].ChangePercent != 25 || c.Metrics[5].ChangePercent != nil {
		t.Errorf("metrics = %+v", c.Metrics)
	}
	entries, _ := os.ReadDir(filepath.Dir(out))
	if len(entries) != 1 {
		t.Errorf("output directory has %d entries, want only the comparison file", len(entries))
	}
}

func TestIncomparableRuns(t *testing.T) {
	data, err := os.ReadFile(fixturePath("baseline.json"))
	if err != nil {
		t.Fatal(err)
	}
	other := strings.Replace(string(data), `"vus": 2`, `"vus": 4`, 1)
	candidate := writeFile(t, "candidate.json", other)
	out := filepath.Join(t.TempDir(), "comparison.json")

	o := runCLI("-baseline", fixturePath("baseline.json"), "-candidate", candidate, "-out", out)
	if o.code != exitIncomparable {
		t.Fatalf("code = %d, want %d; stderr = %s", o.code, exitIncomparable, o.stderr)
	}
	if !strings.Contains(o.stderr, "not comparable") || !strings.Contains(o.stderr, "virtual users differ (baseline 2, candidate 4)") ||
		strings.Contains(o.stdout, "Average latency") {
		t.Errorf("stdout = %q\nstderr = %q", o.stdout, o.stderr)
	}
	r := readComparison(t, out)
	if r.Comparison != nil || len(r.Mismatches) != 1 || !strings.Contains(r.Error, "not comparable") {
		t.Errorf("report = %+v", r)
	}
}

func TestBadReports(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.json")
	for _, tc := range []struct {
		name, baseline, candidate, want string
	}{
		{"incomplete candidate", fixturePath("baseline.json"), fixturePath("run_error.json"),
			"candidate report " + fixturePath("run_error.json") + ": report records a run that did not complete: k6 executable not found"},
		{"unsupported baseline", writeFile(t, "v2.json", `{"schema_version":2}`), fixturePath("baseline.json"),
			"unsupported run report schema: schema_version 2 (supported: 1)"},
		{"malformed", writeFile(t, "bad.json", `{"schema_version":1,"run":`), fixturePath("baseline.json"),
			"baseline report"},
		{"missing file", missing, fixturePath("baseline.json"), "baseline report: open"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "comparison.json")
			o := runCLI("-baseline", tc.baseline, "-candidate", tc.candidate, "-out", out)
			if o.code != exitBadReport || !strings.Contains(o.stderr, tc.want) {
				t.Fatalf("code = %d stderr = %q, want %d and %q", o.code, o.stderr, exitBadReport, tc.want)
			}
			if r := readComparison(t, out); r.Comparison != nil || r.Error == "" {
				t.Errorf("report = %+v, want an error report", r)
			}
		})
	}
}

func TestFailedComparisonReplacesOldResult(t *testing.T) {
	out := filepath.Join(t.TempDir(), "comparison.json")
	if o := runCLI("-baseline", fixturePath("baseline.json"), "-candidate", fixturePath("candidate_improvement.json"), "-out", out); o.code != exitCompared {
		t.Fatalf("first comparison code = %d", o.code)
	}
	runCLI("-baseline", fixturePath("baseline.json"), "-candidate", fixturePath("run_error.json"), "-out", out)
	if r := readComparison(t, out); r.Comparison != nil || r.Error == "" {
		t.Fatalf("report = %+v, want the old comparison replaced by the error", r)
	}
}

func TestUsageErrors(t *testing.T) {
	base := fixturePath("baseline.json")
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no flags", nil, "-baseline and -candidate are required"},
		{"no candidate", []string{"-baseline", base}, "-candidate is required"},
		{"no baseline", []string{"-candidate", base}, "-baseline is required"},
		{"out overwrites baseline", []string{"-baseline", base, "-candidate", fixturePath("candidate_improvement.json"), "-out", base},
			"-out must not be one of the input reports"},
		{"stray argument", []string{"-baseline", base, "-candidate", base, "extra"}, "unexpected arguments"},
		{"unknown flag", []string{"-baseline", base, "-candidate", base, "-rerun"}, "flag provided but not defined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := runCLI(tc.args...)
			if o.code != exitUsage || !strings.Contains(o.stderr, tc.want) {
				t.Fatalf("code = %d stderr = %q, want %d and %q", o.code, o.stderr, exitUsage, tc.want)
			}
		})
	}
	if o := runCLI("-h"); o.code != exitUsage || !strings.Contains(o.stderr, "Usage: ruptiq-compare") {
		t.Errorf("-h: code = %d stderr = %q", o.code, o.stderr)
	}
}

func TestInputsAreNotModified(t *testing.T) {
	before, _ := os.ReadFile(fixturePath("baseline.json"))
	runCLI("-baseline", fixturePath("baseline.json"), "-candidate", fixturePath("candidate_regression.json"))
	after, _ := os.ReadFile(fixturePath("baseline.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("baseline report was modified")
	}
}
