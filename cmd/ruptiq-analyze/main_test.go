package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RajK158/ruptiq/internal/analysis"
)

// fixture is a deterministic Inventory Service experiment report, written by
// the analysis package's tests.
var fixture = filepath.Join("..", "..", "internal", "analysis", "testdata", "inventory_experiment.json")

type outcome struct {
	code           int
	stdout, stderr string
}

func runCmd(args ...string) outcome {
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return outcome{code, stdout.String(), stderr.String()}
}

func readAnalysis(t *testing.T, path string) analysis.Report {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var r analysis.Report
	if err := dec.Decode(&r); err != nil {
		t.Fatalf("analysis JSON: %v", err)
	}
	return r
}

func writeFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAnalyse(t *testing.T) {
	out := filepath.Join(t.TempDir(), "analysis.json")
	o := runCmd("-in", fixture, "-out", out)
	if o.code != exitAnalysed || o.stderr != "" {
		t.Fatalf("exit %d, stderr %q", o.code, o.stderr)
	}
	for _, want := range []string{
		"Experiment:   passed, telemetry complete (5 samples",
		"Windows:      4, covering 3.750s",
		"Client:       k6 (client side): 9000 requests",
		"Throughput: 8040 requests in 3.750s = 2144.0 req/s (windows 1-4)",
		"5xx:        6 of 106 (5.66%)",
		"p95 ~0.989 ms (estimate; within the 0.500 ms to 1.000 ms bucket)",
		"Limitations:",
		"These are signals, not a diagnosis",
		"Analysis written to " + out,
	} {
		if !strings.Contains(o.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, o.stdout)
		}
	}
	if strings.Contains(o.stdout, "cumulative") || strings.Contains(o.stdout, "inventory_http_requests_in_flight") {
		t.Errorf("stdout shows raw telemetry:\n%s", o.stdout)
	}

	r := readAnalysis(t, out)
	if r.SchemaVersion != analysis.SchemaVersion || r.Input != fixture || r.Error != "" || r.Analysis == nil {
		t.Fatalf("report = %+v", r)
	}
	if len(r.Analysis.Windows) != 4 || len(r.Analysis.Totals) != 2 || r.Analysis.Config != analysis.DefaultConfig() {
		t.Errorf("analysis = %d windows, %d totals, config %+v", len(r.Analysis.Windows), len(r.Analysis.Totals), r.Analysis.Config)
	}
}

func TestCustomMetricNames(t *testing.T) {
	o := runCmd("-in", fixture, "-requests-metric", "http_requests_total", "-duration-metric", "http_duration_seconds")
	if o.code != exitAnalysed {
		t.Fatalf("exit %d: %s", o.code, o.stderr)
	}
	if !strings.Contains(o.stdout, "http_requests_total is in none of the 5 samples; check the metric name") {
		t.Errorf("stdout:\n%s", o.stdout)
	}
	o = runCmd("-in", fixture, "-method-label", "", "-route-label", "", "-min-observations", "5000")
	// No window has 5000 requests, but the totals (8146) do.
	if o.code != exitAnalysed ||
		!strings.Contains(o.stdout, "(any method) (any route): 3015.9 req/s, 5xx 0.00%, p95 n/a") ||
		!strings.Contains(o.stdout, "p95 ~1.134 ms (estimate; within the 1.000 ms to 2.500 ms bucket), mean 0.460 ms, 8146 requests") {
		t.Errorf("exit %d, stdout:\n%s", o.code, o.stdout)
	}
}

func TestBadReports(t *testing.T) {
	valid, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	cancelled := bytes.Replace(valid, []byte(`"status": "passed"`), []byte(`"status": "cancelled"`), 1)
	for _, tc := range []struct {
		name string
		data []byte
		args []string
		want string
	}{
		{"not JSON", []byte("not json"), nil, "invalid experiment report: not valid report JSON"},
		{"unsupported", []byte(`{"schema_version": 7}`), nil, "unsupported experiment report schema: schema_version 7 (supported: 1)"},
		{"cancelled", cancelled, nil, `experiment cannot be analysed: its status is "cancelled"`},
		{"too large", valid, []string{"-max-input-bytes", "1024"}, "larger than 1024 bytes"},
		{"inconsistent", bytes.Replace(valid, []byte(`"attempts": 5`), []byte(`"attempts": 6`), 1), nil, "telemetry.attempts is 6"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := writeFile(t, "experiment.json", tc.data)
			out := filepath.Join(t.TempDir(), "analysis.json")
			o := runCmd(append([]string{"-in", in, "-out", out}, tc.args...)...)
			if o.code != exitBadReport || !strings.Contains(o.stderr, tc.want) {
				t.Errorf("exit %d, stderr %q, want %q", o.code, o.stderr, tc.want)
			}
			if strings.Contains(o.stdout, "Throughput") {
				t.Errorf("signals printed for a bad report:\n%s", o.stdout)
			}
			// The failure is recorded, so an older analysis is not mistaken
			// for this one.
			r := readAnalysis(t, out)
			if r.Analysis != nil || !strings.Contains(r.Error, tc.want) {
				t.Errorf("written report = %+v", r)
			}
		})
	}
}

func TestMissingInput(t *testing.T) {
	o := runCmd("-in", filepath.Join(t.TempDir(), "nope.json"))
	if o.code != exitBadReport || !strings.Contains(o.stderr, "experiment report:") {
		t.Errorf("exit %d, stderr %q", o.code, o.stderr)
	}
}

func TestUsage(t *testing.T) {
	in := writeFile(t, "experiment.json", []byte("{}"))
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no input", nil, "-in is required"},
		{"extra argument", []string{"-in", in, "more"}, "unexpected arguments"},
		{"out is in", []string{"-in", in, "-out", in}, "-out must not be the input report"},
		{"tiny limit", []string{"-in", in, "-max-input-bytes", "10"}, "-max-input-bytes must be between"},
		{"huge limit", []string{"-in", in, "-max-input-bytes", "2147483648"}, "-max-input-bytes must be between"},
		{"no counter", []string{"-in", in, "-requests-metric", ""}, "requests counter name is required"},
		{"same labels", []string{"-in", in, "-status-label", "method"}, "must be different"},
		{"min observations", []string{"-in", in, "-min-observations", "0"}, "min observations must be between"},
		{"unknown flag", []string{"-in", in, "-diagnose"}, "flag provided but not defined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := runCmd(tc.args...)
			if o.code != exitUsage || !strings.Contains(o.stderr, tc.want) {
				t.Errorf("exit %d, stderr %q, want %q", o.code, o.stderr, tc.want)
			}
		})
	}
	if o := runCmd("-h"); o.code != exitUsage || !strings.Contains(o.stderr, "Usage: ruptiq-analyze") {
		t.Errorf("-h: exit %d, stderr %q", o.code, o.stderr)
	}
}

func TestUnwritableOutput(t *testing.T) {
	out := filepath.Join(t.TempDir(), "missing-dir", "analysis.json")
	o := runCmd("-in", fixture, "-out", out)
	if o.code != exitBadReport || !strings.Contains(o.stderr, "writing analysis") {
		t.Errorf("exit %d, stderr %q", o.code, o.stderr)
	}
}

// Credentials in the input's URLs never reach stdout, stderr, or the
// analysis file.
func TestCredentialsAreNotShown(t *testing.T) {
	valid, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	leaky := bytes.ReplaceAll(valid, []byte("http://127.0.0.1:8082/metrics"), []byte("http://scraper:hunter2@127.0.0.1:8082/metrics?token=topsecret"))
	leaky = bytes.ReplaceAll(leaky, []byte("http://127.0.0.1:8082/inventory/LAPTOP-001"), []byte("http://loaduser:hunter2@127.0.0.1:8082/inventory/LAPTOP-001?api_key=topsecret"))
	if bytes.Equal(leaky, valid) {
		t.Fatal("fixture URLs not found")
	}
	in := writeFile(t, "experiment.json", leaky)
	out := filepath.Join(t.TempDir(), "analysis.json")
	o := runCmd("-in", in, "-out", out)
	if o.code != exitAnalysed {
		t.Fatalf("exit %d: %s", o.code, o.stderr)
	}
	written, _ := os.ReadFile(out)
	for _, text := range []string{o.stdout, o.stderr, string(written)} {
		for _, s := range []string{"hunter2", "topsecret", "scraper", "loaduser"} {
			if strings.Contains(text, s) {
				t.Errorf("output contains %q", s)
			}
		}
	}
	if !strings.Contains(o.stdout, "http://xxxxx@127.0.0.1:8082/metrics?xxxxx") {
		t.Errorf("stdout:\n%s", o.stdout)
	}
}
