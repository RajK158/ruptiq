package analysis

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/RajK158/ruptiq/internal/experiment"
	"github.com/RajK158/ruptiq/internal/telemetry"
)

// A long experiment's summary stays short: window lines and warnings are
// capped, and raw telemetry (bucket counts, other families) is never shown.
func TestSummaryIsBounded(t *testing.T) {
	var entries []entry
	for i := range 40 {
		v := float64(i * 100)
		if i >= 20 {
			v = float64((i - 20) * 100) // the target restarted
		}
		entries = append(entries, sample(float64(i), experiment.DuringWorkload,
			counter(reqs, cs(get200, v)),
			telemetry.Family{Name: "process_secret_gauge", Type: telemetry.Gauge, Series: []telemetry.Series{cs(nil, 42)}}))
	}
	a := analyze(t, report(entries...))
	var buf bytes.Buffer
	if err := WriteSummary(&buf, a); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, fmt.Sprintf("#%d->#%d", maxSummaryWindowLines+1, maxSummaryWindowLines+2)) ||
		!strings.Contains(out, fmt.Sprintf("#%d->#%d", maxSummaryWindowLines, maxSummaryWindowLines+1)) {
		t.Errorf("want exactly %d window lines:\n%s", maxSummaryWindowLines, out)
	}
	if !strings.Contains(out, fmt.Sprintf("... %d more lines in the JSON report", 39-maxSummaryWindowLines)) {
		t.Errorf("hidden window lines are not counted:\n%s", out)
	}
	for _, raw := range []string{"process_secret_gauge", "cumulative", "upper_bound", "le="} {
		if strings.Contains(out, raw) {
			t.Errorf("summary shows raw telemetry %q:\n%s", raw, out)
		}
	}
	if !strings.Contains(out, "counter_reset (window 20, "+reqs+`{method="GET",route="/inventory/{sku}",status="200"})`) {
		t.Errorf("reset warning is missing:\n%s", out)
	}
	if !strings.Contains(out, "Throughput: 3800 requests in 38.000s = 100.0 req/s (windows 1-19, 21-39)") {
		t.Errorf("totals should exclude the reset window:\n%s", out)
	}
}

func TestSummaryWarningsAreCapped(t *testing.T) {
	var entries []entry
	for i := range 30 {
		entries = append(entries, sample(float64(i), experiment.DuringWorkload,
			counter(reqs, cs(labels("GET", fmt.Sprintf("/r%d", i), "200"), 1))))
	}
	a := analyze(t, report(entries...))
	if len(a.Warnings) <= maxSummaryWarnings {
		t.Fatalf("want more than %d warnings, got %d", maxSummaryWarnings, len(a.Warnings))
	}
	var buf bytes.Buffer
	if err := WriteSummary(&buf, a); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	_, warnings, _ := strings.Cut(out, "\nWarnings (")
	warnings, _, _ = strings.Cut(warnings, "\nLimitations:")
	if strings.Count(warnings, "\n  - ") != maxSummaryWarnings || !strings.Contains(out, fmt.Sprintf("... %d more in the JSON report", len(a.Warnings)-maxSummaryWarnings)) {
		t.Errorf("warnings are not capped:\n%s", out)
	}
	if strings.Count(out, "GET /r") != maxSummaryRoutes+maxSummaryWindowLines || !strings.Contains(out, "... 10 more routes in the JSON report") {
		t.Errorf("routes are not capped:\n%s", out)
	}
}

func TestSummaryWithoutWindows(t *testing.T) {
	a := analyze(t, report(failed(0, experiment.BeforeWorkload, "refused")))
	var buf bytes.Buffer
	if err := WriteSummary(&buf, a); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Windows:      none (fewer than two samples)", "none: no series of the configured metrics were found", "Fewer than two samples"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("summary lacks %q:\n%s", want, buf.String())
		}
	}
}

func TestFormatWindows(t *testing.T) {
	for _, tc := range []struct {
		in   []int
		want string
	}{{nil, "none"}, {[]int{3}, "3"}, {[]int{1, 2, 3}, "1-3"}, {[]int{1, 2, 4, 6, 7}, "1-2, 4, 6-7"}} {
		if got := formatWindows(tc.in); got != tc.want {
			t.Errorf("formatWindows(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
