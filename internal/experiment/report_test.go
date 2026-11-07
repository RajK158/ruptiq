package experiment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/RajK158/ruptiq/internal/workload"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	want = bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n"))
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the golden file (run with -update after checking):\n%s", name, got)
	}
}

// intermittentReport is an experiment with one failed scrape during the
// workload: four samples and one error.
func intermittentReport(t *testing.T) Report {
	t.Helper()
	h := newHarness()
	h.reader.fail[3] = errRefused
	cfg := baseConfig()
	cfg.Workload.Duration = 3 * time.Second
	return h.run(t, context.Background(), cfg, func(context.Context, workload.Spec) (workload.Result, error) {
		h.awaitBaseline(t)
		for range 3 {
			h.tick()
		}
		h.clock.Advance(250 * time.Millisecond)
		r := healthyResult()
		r.ConfiguredDurationSeconds = 3
		return r, nil
	})
}

func TestReportJSON(t *testing.T) {
	report := intermittentReport(t)
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "intermittent_report.golden.json", append(data, '\n'))

	var back Report
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, report) {
		t.Errorf("the report does not survive a JSON round trip:\n got %+v\nwant %+v", back, report)
	}
}

func TestEmptyTelemetryJSON(t *testing.T) {
	h := newHarness()
	for n := 1; n <= 3; n++ {
		h.reader.fail[n] = errRefused
	}
	report := h.run(t, context.Background(), baseConfig(), func(context.Context, workload.Spec) (workload.Result, error) {
		return workload.Result{}, errors.New("k6 executable not found")
	})
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	// No invented values: the samples list is present and empty, and the
	// workload has an error but no result.
	for _, want := range []string{`"samples":[]`, `"completeness":"none"`, `"status":"error"`, `"error":"k6 executable not found"`} {
		if !bytes.Contains(data, []byte(want)) {
			t.Errorf("JSON lacks %s:\n%s", want, data)
		}
	}
	if bytes.Contains(data, []byte(`"result"`)) {
		t.Errorf("JSON has a workload result for a workload that did not run:\n%s", data)
	}
}

func TestRedaction(t *testing.T) {
	const (
		targetURL  = "http://loaduser:load-pw@127.0.0.1:8082/inventory/LAPTOP-001?api_key=target-key"
		metricsURL = "http://scraper:scrape-pw@127.0.0.1:8082/metrics?token=metrics-token"
	)
	secrets := []string{"load-pw", "target-key", "scrape-pw", "metrics-token", "loaduser:", "scraper:"}

	h := newHarness()
	// Errors that echo the raw URLs, as a careless runner or reader might.
	h.reader.fail[2] = errors.New("GET " + metricsURL + ": connection refused")
	cfg := baseConfig()
	cfg.Workload.TargetURL, cfg.MetricsURL = targetURL, metricsURL
	report := h.run(t, context.Background(), cfg, func(context.Context, workload.Spec) (workload.Result, error) {
		h.awaitBaseline(t)
		h.tick()
		return workload.Result{}, errors.New("k6 failed: GET " + targetURL + " refused")
	})

	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var summary bytes.Buffer
	if err := WriteSummary(&summary, report); err != nil {
		t.Fatal(err)
	}
	for _, s := range secrets {
		if bytes.Contains(data, []byte(s)) {
			t.Errorf("JSON contains %q:\n%s", s, data)
		}
		if strings.Contains(summary.String(), s) {
			t.Errorf("summary contains %q:\n%s", s, summary.String())
		}
	}
	if report.Config.TargetURL != "http://xxxxx@127.0.0.1:8082/inventory/LAPTOP-001?xxxxx" ||
		report.Config.MetricsURL != "http://xxxxx@127.0.0.1:8082/metrics?xxxxx" {
		t.Errorf("recorded URLs %q and %q", report.Config.TargetURL, report.Config.MetricsURL)
	}

	// A runner that returns its URL unredacted in a result.
	h = newHarness()
	cfg = baseConfig()
	cfg.Workload.TargetURL = targetURL
	report = h.run(t, context.Background(), cfg, func(context.Context, workload.Spec) (workload.Result, error) {
		r := healthyResult()
		r.TargetURL = targetURL
		return r, nil
	})
	if got := report.Workload.Result.TargetURL; got != report.Config.TargetURL {
		t.Errorf("workload result URL = %q, want the redacted %q", got, report.Config.TargetURL)
	}
}

func TestWriteSummary(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteSummary(&buf, intermittentReport(t)); err != nil {
		t.Fatal(err)
	}
	golden(t, "intermittent_summary.golden.txt", buf.Bytes())
}

func TestWriteSummaryLimitsErrors(t *testing.T) {
	h := newHarness()
	for n := 1; n <= 20; n++ {
		h.reader.fail[n] = errors.New("refused\n" + strings.Repeat("x", 1000))
	}
	report := h.run(t, context.Background(), baseConfig(), func(context.Context, workload.Spec) (workload.Result, error) {
		h.awaitBaseline(t)
		for range 5 {
			h.tick()
		}
		return workload.Result{}, errors.New("k6 exited with status 99:\n" + strings.Repeat("log line\n", 500))
	})
	var buf bytes.Buffer
	if err := WriteSummary(&buf, report); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if got := strings.Count(out, "Scrape error:"); got != MaxErrorsShown {
		t.Errorf("%d scrape errors shown, want %d", got, MaxErrorsShown)
	}
	if !strings.Contains(out, "... 2 more scrape errors in the report") {
		t.Errorf("summary does not say how many errors were left out:\n%s", out)
	}
	if !strings.Contains(out, "Status:          ERROR") || !strings.Contains(out, "Workload:        did not complete: k6 exited with status 99: log line") {
		t.Errorf("summary lacks the status or workload error:\n%s", out)
	}
	if len(out) > 4000 {
		t.Errorf("summary is %d bytes; long messages should be shortened", len(out))
	}
}
