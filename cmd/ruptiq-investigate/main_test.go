package main

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/RajK158/ruptiq/internal/analysis"
	"github.com/RajK158/ruptiq/internal/investigation"
)

var (
	// experimentFixture is the synthetic Inventory Service experiment report
	// used by the analysis tests, and analysisFixture its saved analysis.
	experimentFixture = filepath.Join("..", "..", "internal", "analysis", "testdata", "inventory_experiment.json")
	analysisFixture   = filepath.Join("..", "..", "internal", "analysis", "testdata", "inventory_analysis.golden.json")
)

type outcome struct {
	code           int
	stdout, stderr string
}

func runCmd(args ...string) outcome {
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return outcome{code, stdout.String(), stderr.String()}
}

// readReport decodes an investigation file strictly: every field must be
// part of the schema.
func readReport(t *testing.T, path string) investigation.Report {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var r investigation.Report
	if err := dec.Decode(&r); err != nil {
		t.Fatalf("investigation JSON: %v", err)
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

func decodeJSON(t *testing.T, data []byte) any {
	t.Helper()
	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// resolve follows a JSON pointer made of keys and array indexes.
func resolve(t *testing.T, doc any, pointer string) map[string]any {
	t.Helper()
	cur := doc
	for _, tok := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		switch v := cur.(type) {
		case map[string]any:
			next, ok := v[tok]
			if !ok {
				t.Fatalf("pointer %q: no key %q", pointer, tok)
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(v) {
				t.Fatalf("pointer %q: bad index %q", pointer, tok)
			}
			cur = v[i]
		default:
			t.Fatalf("pointer %q: cannot descend into %T", pointer, cur)
		}
	}
	m, ok := cur.(map[string]any)
	if !ok {
		t.Fatalf("pointer %q is not an object: %v", pointer, cur)
	}
	return m
}

// analyseFixture runs the analysis step of ruptiq-analyze on the synthetic
// experiment and saves the result as ruptiq-analyze -out does.
func analyseFixture(t *testing.T) (string, []byte) {
	t.Helper()
	f, err := os.Open(experimentFixture)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	exp, err := analysis.ReadExperiment(f, analysis.DefaultMaxInputBytes)
	if err != nil {
		t.Fatal(err)
	}
	a, err := analysis.Analyze(exp, analysis.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(analysis.Report{SchemaVersion: analysis.SchemaVersion, Input: experimentFixture, Analysis: &a}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return writeFile(t, "analysis.json", append(data, '\n')), data
}

// Offline end to end: synthetic experiment, then analysis, then
// investigation. Only two windows are taken during the workload, so with the
// default thresholds every route abstains.
func TestEndToEndAbstains(t *testing.T) {
	in, data := analyseFixture(t)
	// The saved analysis is the one checked in as the analysis golden file.
	golden, err := os.ReadFile(analysisFixture)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := decodeJSON(t, data).(map[string]any)["analysis"], decodeJSON(t, golden).(map[string]any)["analysis"]; !jsonEqual(got, want) {
		t.Fatal("the analysis differs from the analysis golden file")
	}

	out := filepath.Join(t.TempDir(), "investigation.json")
	o := runCmd("-in", in, "-out", out)
	if o.code != exitInvestigated || o.stderr != "" {
		t.Fatalf("exit %d, stderr %q", o.code, o.stderr)
	}
	for _, want := range []string{
		"Source:       passed experiment, telemetry complete, 4 windows (2 comparable: 2-3)",
		"Outcome:      insufficient_evidence",
		"GET /inventory/{sku}: 3 of 3 signals could not be compared",
		"throughput: 2 usable window(s) of 2 comparable; at least 4 (2 per side) are needed",
		"window 1: spans the workload's start or end (samples #1 before_workload to #2 during_workload)",
		"window 4: spans the workload's start or end (samples #4 during_workload to #5 after_workload)",
		"Investigation written to " + out,
	} {
		if !strings.Contains(o.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, o.stdout)
		}
	}

	r := readReport(t, out)
	inv := r.Investigation
	if r.SchemaVersion != investigation.SchemaVersion || r.Input != in || r.Error != "" || inv == nil {
		t.Fatalf("report %+v", r)
	}
	if inv.Outcome != investigation.OutcomeInsufficientEvidence || len(inv.Findings) != 2 || len(inv.Assessments) != 6 {
		t.Fatalf("outcome %s, %d findings, %d assessments", inv.Outcome, len(inv.Findings), len(inv.Assessments))
	}
	// Each abstention points at its own route in the excluded windows.
	doc := decodeJSON(t, data)
	for _, f := range inv.Findings {
		if f.Type != investigation.FindingInsufficientEvidence || len(f.Evidence) != 2 {
			t.Fatalf("finding %+v", f)
		}
		for i, e := range f.Evidence {
			rs := resolve(t, doc, e.Pointer)
			if e.Role != "excluded" || e.Window != []int{1, 4}[i] || rs["method"] != f.Method || rs["route"] != f.Route {
				t.Errorf("%s: evidence %+v resolves to %v %v", f.ID, e, rs["method"], rs["route"])
			}
		}
	}
}

// With one window per side, the synthetic experiment's GET rate falls from
// 3000 requests in 1.008s (2976.2/s) in window 2 to 3020 in 1.490s
// (2026.8/s) in window 3: -31.9%, past the -30% threshold.
func TestEndToEndFinding(t *testing.T) {
	in, data := analyseFixture(t)
	out := filepath.Join(t.TempDir(), "investigation.json")
	o := runCmd("-in", in, "-out", out, "-min-windows", "1")
	if o.code != exitInvestigated {
		t.Fatalf("exit %d: %s", o.code, o.stderr)
	}
	if !strings.Contains(o.stdout, "1. throughput_degradation [moderate, observation]") ||
		!strings.Contains(o.stdout, "GET /inventory/{sku}: completed requests fell from 2976.2 requests/s to 2026.8 requests/s (-31.9%)") {
		t.Errorf("stdout:\n%s", o.stdout)
	}
	inv := readReport(t, out).Investigation
	if inv.Outcome != investigation.OutcomeChangesObserved || len(inv.Findings) != 2 {
		t.Fatalf("outcome %s, findings %+v", inv.Outcome, inv.Findings)
	}
	f := inv.Findings[0]
	c := f.Changes[0]
	if f.Type != investigation.FindingThroughputDegradation || f.Method != "GET" || f.Route != "/inventory/{sku}" || f.ProvenRootCause ||
		math.Abs(c.Baseline.Value-3000/1.008) > 1e-9 || math.Abs(c.Comparison.Value-3020/1.49) > 1e-9 {
		t.Fatalf("finding %+v", f)
	}
	doc := decodeJSON(t, data)
	wantRequests := []float64{3000, 3020}
	for i, e := range f.Evidence {
		if e.Pointer != []string{"/analysis/windows/1/routes/0/requests", "/analysis/windows/2/routes/0/requests"}[i] {
			t.Errorf("evidence %d = %s", i, e.Pointer)
		}
		req := resolve(t, doc, e.Pointer)
		if req["requests"].(float64) != wantRequests[i] || req["evidence"].(map[string]any)["metric"] != analysis.DefaultRequestsCounter {
			t.Errorf("%s = %v", e.Pointer, req)
		}
	}
	// POST has 40 requests in window 2 and 24 in window 3.
	if p := inv.Findings[1]; p.Type != investigation.FindingInsufficientEvidence || p.Method != "POST" ||
		!strings.Contains(p.Gaps[0].Reason, "the baseline has 40 requests, fewer than 100") {
		t.Errorf("POST %+v", p)
	}
	if !strings.Contains(strings.Join(inv.Limitations, " "), "single noisy window") {
		t.Errorf("limitations %q", inv.Limitations)
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func TestReportSchema(t *testing.T) {
	out := filepath.Join(t.TempDir(), "investigation.json")
	if o := runCmd("-in", analysisFixture, "-out", out, "-min-windows", "1"); o.code != exitInvestigated {
		t.Fatalf("exit %d: %s", o.code, o.stderr)
	}
	data, _ := os.ReadFile(out)
	doc := decodeJSON(t, data).(map[string]any)
	inv := doc["investigation"].(map[string]any)
	for _, key := range []string{"source", "config", "outcome", "summary", "windows", "findings", "assessments", "limitations"} {
		if _, ok := inv[key]; !ok {
			t.Errorf("investigation lacks %q", key)
		}
	}
	f := inv["findings"].([]any)[0].(map[string]any)
	for _, key := range []string{"rank", "id", "type", "designation", "proven_root_cause", "method", "route", "severity", "symptom", "changes",
		"baseline_windows", "comparison_windows", "evidence", "explanation", "ranking", "contradictions", "limitations",
		"possible_explanations", "next_experiment"} {
		if _, ok := f[key]; !ok {
			t.Errorf("finding lacks %q", key)
		}
	}
	m := f["changes"].([]any)[0].(map[string]any)["baseline"].(map[string]any)
	if m["unit"] != investigation.UnitRequestsPerSecond || doc["schema_version"].(float64) != investigation.SchemaVersion {
		t.Errorf("baseline %v", m)
	}
}

func TestBadInputs(t *testing.T) {
	valid, err := os.ReadFile(analysisFixture)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		data []byte
		args []string
		want string
	}{
		{"not JSON", []byte("not json"), nil, "invalid analysis file: not valid JSON"},
		{"unsupported", []byte(`{"schema_version": 7}`), nil, "unsupported analysis schema: schema_version 7 (supported: 1)"},
		{"failed analysis", []byte(`{"schema_version": 1, "error": "invalid experiment report: bad"}`), nil, "analysis cannot be investigated: the file records a failed analysis"},
		{"experiment report", mustRead(t, experimentFixture), nil, `unknown field`},
		{"too large", valid, []string{"-max-input-bytes", "1024"}, "larger than 1024 bytes"},
		{"inconsistent", bytes.Replace(valid, []byte(`"per_second": 2144`), []byte(`"per_second": 2145`), 1), nil, "per_second 2145 is not requests / elapsed_seconds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := writeFile(t, "analysis.json", tc.data)
			// A previous investigation is replaced by the failure, so it
			// cannot be mistaken for this one.
			out := writeFile(t, "investigation.json", []byte(`{"schema_version": 1, "investigation": {"outcome": "changes_observed"}}`))
			o := runCmd(append([]string{"-in", in, "-out", out}, tc.args...)...)
			if o.code != exitBadInput || !strings.Contains(o.stderr, tc.want) {
				t.Errorf("exit %d, stderr %q, want %q", o.code, o.stderr, tc.want)
			}
			if strings.Contains(o.stdout, "Findings") || strings.Contains(o.stdout, "Outcome") {
				t.Errorf("findings printed for a bad input:\n%s", o.stdout)
			}
			r := readReport(t, out)
			if r.Investigation != nil || !strings.Contains(r.Error, tc.want) || r.SchemaVersion != investigation.SchemaVersion {
				t.Errorf("written report = %+v", r)
			}
			if matches, _ := filepath.Glob(filepath.Join(filepath.Dir(out), ".ruptiq-investigate-*")); len(matches) > 0 {
				t.Errorf("temporary files left: %v", matches)
			}
		})
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestMissingInput(t *testing.T) {
	o := runCmd("-in", filepath.Join(t.TempDir(), "nope.json"))
	if o.code != exitBadInput || !strings.Contains(o.stderr, "analysis file:") {
		t.Errorf("exit %d, stderr %q", o.code, o.stderr)
	}
}

func TestUsage(t *testing.T) {
	in := writeFile(t, "analysis.json", []byte("{}"))
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no input", nil, "-in is required"},
		{"extra argument", []string{"-in", in, "more"}, "unexpected arguments"},
		{"out is in", []string{"-in", in, "-out", in}, "-out must not be the input analysis"},
		{"tiny limit", []string{"-in", in, "-max-input-bytes", "10"}, "-max-input-bytes must be between"},
		{"min windows", []string{"-in", in, "-min-windows", "0"}, "min windows per side must be between"},
		{"min requests", []string{"-in", in, "-min-requests", "0"}, "min requests per side must be between"},
		{"ratio", []string{"-in", in, "-error-ratio-increase", "2"}, "error ratio increase must be"},
		{"latency", []string{"-in", in, "-latency-increase", "NaN"}, "latency increase must be"},
		{"throughput", []string{"-in", in, "-throughput-decrease", "1"}, "throughput decrease must be"},
		{"unknown flag", []string{"-in", in, "-diagnose"}, "flag provided but not defined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := runCmd(tc.args...)
			if o.code != exitUsage || !strings.Contains(o.stderr, tc.want) {
				t.Errorf("exit %d, stderr %q, want %q", o.code, o.stderr, tc.want)
			}
		})
	}
	if o := runCmd("-h"); o.code != exitUsage || !strings.Contains(o.stderr, "Usage: ruptiq-investigate") {
		t.Errorf("-h: exit %d, stderr %q", o.code, o.stderr)
	}
}

func TestUnwritableOutput(t *testing.T) {
	out := filepath.Join(t.TempDir(), "missing-dir", "investigation.json")
	o := runCmd("-in", analysisFixture, "-out", out)
	if o.code != exitBadInput || !strings.Contains(o.stderr, "writing investigation") {
		t.Errorf("exit %d, stderr %q", o.code, o.stderr)
	}
}

// Credentials in the analysis never reach stdout, stderr, or the file.
func TestCredentialsAreNotShown(t *testing.T) {
	valid := mustRead(t, analysisFixture)
	leaky := bytes.ReplaceAll(valid, []byte("http://127.0.0.1:8082/metrics"), []byte("http://scraper:hunter2@127.0.0.1:8082/metrics?token=topsecret"))
	leaky = bytes.ReplaceAll(leaky, []byte("http://127.0.0.1:8082/inventory/LAPTOP-001"), []byte("http://loaduser:hunter2@127.0.0.1:8082/inventory/LAPTOP-001?api_key=topsecret"))
	leaky = bytes.ReplaceAll(leaky, []byte(`"/reservations"`), []byte(`"https://u:hunter2@h/reservations?k=topsecret"`))
	if bytes.Equal(leaky, valid) {
		t.Fatal("fixture values not found")
	}
	in := writeFile(t, "analysis.json", leaky)
	out := filepath.Join(t.TempDir(), "investigation.json")
	o := runCmd("-in", in, "-out", out)
	if o.code != exitInvestigated {
		t.Fatalf("exit %d: %s", o.code, o.stderr)
	}
	written := mustRead(t, out)
	for _, text := range []string{o.stdout, o.stderr, string(written)} {
		for _, s := range []string{"hunter2", "topsecret", "scraper", "loaduser"} {
			if strings.Contains(text, s) {
				t.Errorf("output contains %q", s)
			}
		}
	}
	r := readReport(t, out)
	if r.Investigation.Source.TargetURL != "http://xxxxx@127.0.0.1:8082/inventory/LAPTOP-001?xxxxx" {
		t.Errorf("target URL %q", r.Investigation.Source.TargetURL)
	}

	// The same holds for a failed analysis recorded with a credential.
	in = writeFile(t, "failed.json", []byte(`{"schema_version": 1, "error": "experiment report http://u:hunter2@h/e.json?k=topsecret: not found"}`))
	o = runCmd("-in", in, "-out", out)
	written = mustRead(t, out)
	for _, text := range []string{o.stdout, o.stderr, string(written)} {
		if strings.Contains(text, "hunter2") || strings.Contains(text, "topsecret") {
			t.Errorf("failure output contains a credential: %s", text)
		}
	}
}
