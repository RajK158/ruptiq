package investigation

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/RajK158/ruptiq/internal/experiment"
)

func summary(t *testing.T, inv Investigation) string {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteSummary(&buf, inv); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestSummaryOfFindings(t *testing.T) {
	out := summary(t, investigate(t, allFindings()))
	for _, want := range []string{
		"Source:       passed experiment, telemetry complete, 4 windows (4 comparable: 1-4)",
		"Thresholds:   2+ windows and 100+ requests per side; 5xx ratio +1.00 points and 2x (5+ errors); p95 +50.0%; throughput -30.0%",
		"Outcome:      changes_observed",
		// Both are major; server errors rank above latency by type.
		"1. server_error_increase [major, observation]",
		"2. latency_degradation [major, observation] latency_degradation-a76631a81bc2",
		"combined_degradation [moderate, hypothesis]",
		"insufficient_evidence [abstention]",
		"Windows: baseline 1-2, comparison 3-4; 4 evidence references (first: /analysis/windows/0/routes/0/latency/p95)",
		"DELETE /items: 3 of 3 signals could not be compared",
		"Next: Repeat the experiment at least 3 times",
		"Not a diagnosis: no finding is a proven root cause",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}
}

func TestSummaryOfInsufficientEvidence(t *testing.T) {
	a := build(
		w(get()).phases(experiment.BeforeWorkload, experiment.DuringWorkload),
		w(get()),
		w(get()).phases(experiment.DuringWorkload, experiment.AfterWorkload),
	)
	out := summary(t, investigate(t, a))
	for _, want := range []string{
		"Outcome:      insufficient_evidence",
		"No signal of any route could be compared (1 of 3 windows are comparable); the investigation abstains.",
		"throughput: 1 usable window(s) of 1 comparable; at least 4 (2 per side) are needed",
		"Excluded windows:\n  window 1: spans the workload's start or end",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}
}

func TestSummaryShowsInconclusiveSignals(t *testing.T) {
	inv := investigate(t, build(w(get()), w(get().requests(400, 0, 0)), w(get().requests(500, 0, 0)), w(get().requests(300, 0, 0))))
	out := summary(t, inv)
	if !strings.Contains(out, "Inconclusive (threshold reached, but not robust; not reported as findings):\n  GET /inventory/{sku} throughput: the median rate fell by") {
		t.Errorf("summary:\n%s", out)
	}
}

func TestSummaryIsBounded(t *testing.T) {
	var base, comp []spec
	for i := range 30 {
		r := rt("GET", fmt.Sprintf("/route/%02d/%s", i, strings.Repeat("x", 500)))
		base = append(base, r)
		comp = append(comp, r.latency(0.002, 0.001, 0.0025))
	}
	ws := []wspec{w(base...), w(base...), w(comp...), w(comp...)}
	for range 15 {
		ws = append([]wspec{w(base...).phases(experiment.BeforeWorkload, experiment.BeforeWorkload)}, ws...)
	}
	inv := investigate(t, build(ws...))
	out := summary(t, inv)
	if len(inv.Findings) != 30 {
		t.Fatalf("%d findings", len(inv.Findings))
	}
	if !strings.Contains(out, "... and 20 more findings in the JSON report") || !strings.Contains(out, "... and 5 more in the JSON report") {
		t.Errorf("summary is not capped:\n%s", out)
	}
	if n := strings.Count(out, "\n"); n > 80 {
		t.Errorf("summary has %d lines", n)
	}
	for _, l := range strings.Split(out, "\n") {
		if len([]rune(l)) > maxSummaryLine+40 {
			t.Errorf("line of %d characters", len([]rune(l)))
		}
	}
}
