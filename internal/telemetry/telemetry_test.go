package telemetry

import (
	"bytes"
	"encoding/json"
	"flag"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

// fixtureSnapshot parses testdata/valid.prom into a Snapshot with a fixed
// endpoint and time, without any network.
func fixtureSnapshot(t *testing.T) Snapshot {
	t.Helper()
	families, err := parse(fixture(t, "valid.prom"))
	if err != nil {
		t.Fatal(err)
	}
	return Snapshot{
		Endpoint:              "http://127.0.0.1:8082/metrics",
		ScrapedAt:             time.Date(2026, 11, 6, 12, 0, 0, 125_000_000, time.UTC),
		ScrapeDurationSeconds: 0.0021,
		Families:              families,
	}
}

// golden compares got with testdata/name, or rewrites the file with -update.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want := fixture(t, name)
	if !bytes.Equal(got, want) {
		t.Errorf("output differs from %s (run go test -update to rewrite it after checking):\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

func TestSnapshotJSON(t *testing.T) {
	report := Report{SchemaVersion: ReportSchemaVersion, Endpoint: "http://127.0.0.1:8082/metrics"}
	s := fixtureSnapshot(t)
	report.Snapshot = &s
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "valid_report.golden.json", append(data, '\n'))

	// It reads back to the same snapshot.
	var back Report
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	again, err := json.MarshalIndent(back, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, data) {
		t.Errorf("JSON does not round-trip:\n%s", again)
	}
}

func TestFailedReportJSON(t *testing.T) {
	data, err := json.Marshal(Report{SchemaVersion: 1, Endpoint: "http://h/metrics?xxxxx", Error: "metrics request failed: boom"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":1,"endpoint":"http://h/metrics?xxxxx","error":"metrics request failed: boom"}`
	if string(data) != want {
		t.Errorf("JSON = %s, want %s", data, want)
	}
}

func TestFloatJSON(t *testing.T) {
	tests := []struct {
		value float64
		json  string
	}{
		{0, `0`},
		{-3.25, `-3.25`},
		{1027, `1027`},
		{0.0001, `0.0001`},
		{1e21, `1e+21`},
		{math.NaN(), `"NaN"`},
		{math.Inf(1), `"+Inf"`},
		{math.Inf(-1), `"-Inf"`},
	}
	for _, tt := range tests {
		data, err := json.Marshal(Float(tt.value))
		if err != nil || string(data) != tt.json {
			t.Errorf("Marshal(%v) = %s, %v; want %s", tt.value, data, err, tt.json)
		}
		var back Float
		if err := json.Unmarshal([]byte(tt.json), &back); err != nil {
			t.Errorf("Unmarshal(%s): %v", tt.json, err)
			continue
		}
		if b := float64(back); b != tt.value && !(math.IsNaN(b) && math.IsNaN(tt.value)) {
			t.Errorf("Unmarshal(%s) = %v, want %v", tt.json, b, tt.value)
		}
	}

	for _, bad := range []string{`"inf"`, `"1.5"`, `true`, `{}`, `null2`} {
		var f Float
		if err := json.Unmarshal([]byte(bad), &f); err == nil {
			t.Errorf("Unmarshal(%s) succeeded, want an error", bad)
		}
	}
}

func TestSnapshotHelpers(t *testing.T) {
	s := fixtureSnapshot(t)
	if f, ok := s.Family("app_queue_depth"); !ok || f.Type != Gauge {
		t.Errorf("Family(app_queue_depth) = %+v, %v", f, ok)
	}
	if _, ok := s.Family("missing"); ok {
		t.Error("Family(missing) found something")
	}
	if n := s.SeriesCount(); n != 10 {
		t.Errorf("SeriesCount = %d, want 10", n)
	}
	if strings.Contains(labelString(map[string]string{"a": "x\ny"}), "\n") {
		t.Error("labelString did not escape a newline")
	}
}
