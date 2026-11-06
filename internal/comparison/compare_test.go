package comparison

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/RajK158/ruptiq/internal/workload"
)

func f(v float64) *float64 { return &v }

// expect is the full expected content of one MetricChange.
type expect struct {
	key                 string
	baseline, candidate float64
	change              float64
	percent             *float64
	pp                  *float64
	direction           Direction
	assessment          Assessment
}

func checkMetrics(t *testing.T, got []MetricChange, want []expect) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d metrics, want %d", len(got), len(want))
	}
	eqPtr := func(a, b *float64) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
	for i, w := range want {
		g := got[i]
		if g.Key != w.key || g.Baseline != w.baseline || g.Candidate != w.candidate || g.Change != w.change ||
			!eqPtr(g.ChangePercent, w.percent) || !eqPtr(g.ChangePercentagePoints, w.pp) ||
			g.Direction != w.direction || g.Assessment != w.assessment {
			t.Errorf("metric %d:\n got %s b=%v c=%v change=%v pct=%v pp=%v %s %s\nwant %s b=%v c=%v change=%v pct=%v pp=%v %s %s",
				i, g.Key, g.Baseline, g.Candidate, g.Change, deref(g.ChangePercent), deref(g.ChangePercentagePoints), g.Direction, g.Assessment,
				w.key, w.baseline, w.candidate, w.change, deref(w.percent), deref(w.pp), w.direction, w.assessment)
		}
	}
}

func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func compareFixtures(t *testing.T, baseline, candidate string) Comparison {
	t.Helper()
	c, err := Compare(mustDecode(t, baseline), mustDecode(t, candidate))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCompareRegression(t *testing.T) {
	c := compareFixtures(t, "baseline.json", "candidate_regression.json")
	checkMetrics(t, c.Metrics, []expect{
		{"latency_avg_ms", 2, 2.5, 0.5, f(25), nil, Increase, Worse},
		{"latency_p50_ms", 1.5, 1.5, 0, f(0), nil, Unchanged, NoChange},
		{"latency_p95_ms", 4, 5, 1, f(25), nil, Increase, Worse},
		{"requests_per_second", 2000, 1600, -400, f(-20), nil, Decrease, GenerallyWorse},
		{"failure_rate", 0, 0.01, 0.01, nil, f(1), Increase, Worse},
		{"failed_requests", 0, 80, 80, nil, nil, Increase, NoAssessment},
		{"http_requests", 10000, 8000, -2000, f(-20), nil, Decrease, NoAssessment},
	})
	if c.Metrics[5].ChangePercentNote != "baseline is 0" || c.Metrics[4].ChangePercentNote == "" {
		t.Errorf("percent notes = %q, %q", c.Metrics[5].ChangePercentNote, c.Metrics[4].ChangePercentNote)
	}
	if c.Baseline.Passed != true || c.Candidate.Passed != false || len(c.Candidate.VerdictReasons) != 1 {
		t.Errorf("verdicts = %+v / %+v", c.Baseline, c.Candidate)
	}
}

func TestCompareImprovement(t *testing.T) {
	c := compareFixtures(t, "baseline.json", "candidate_improvement.json")
	checkMetrics(t, c.Metrics, []expect{
		{"latency_avg_ms", 2, 1.5, -0.5, f(-25), nil, Decrease, Better},
		{"latency_p50_ms", 1.5, 1.125, -0.375, f(-25), nil, Decrease, Better},
		{"latency_p95_ms", 4, 3, -1, f(-25), nil, Decrease, Better},
		{"requests_per_second", 2000, 2500, 500, f(25), nil, Increase, GenerallyBetter},
		{"failure_rate", 0, 0, 0, nil, f(0), Unchanged, NoChange},
		{"failed_requests", 0, 0, 0, nil, nil, Unchanged, NoAssessment},
		{"http_requests", 10000, 12500, 2500, f(25), nil, Increase, NoAssessment},
	})
	if len(c.Caveats) != 2 {
		t.Errorf("caveats = %q, want only the two that always apply", c.Caveats)
	}
}

func TestCompareSameRunIsUnchanged(t *testing.T) {
	c := compareFixtures(t, "baseline.json", "baseline.json")
	for _, m := range c.Metrics {
		if m.Change != 0 || m.Direction != Unchanged || (m.Assessment != NoChange && m.Assessment != NoAssessment) {
			t.Errorf("%s: %+v", m.Key, m)
		}
	}
}

func TestCompareFailureRateRegressionFromNonzeroBaseline(t *testing.T) {
	// Swapping the fixtures: the "baseline" has failures, the "candidate" none.
	c := compareFixtures(t, "candidate_regression.json", "baseline.json")
	checkMetrics(t, c.Metrics[4:6], []expect{
		{"failure_rate", 0.01, 0, -0.01, nil, f(-1), Decrease, Better},
		{"failed_requests", 80, 0, -80, f(-100), nil, Decrease, NoAssessment},
	})
	// Throughput rose (1600 to 2000) while failures fell: no fast-failure caveat.
	for _, cv := range c.Caveats {
		if strings.Contains(cv, "fast failures") {
			t.Errorf("unexpected caveat %q", cv)
		}
	}
}

func TestCompareZeroBaselines(t *testing.T) {
	b, c := mustDecode(t, "baseline.json"), mustDecode(t, "candidate_improvement.json")
	b.Result.Metrics.LatencyMs = workload.Latency{}
	cmp, err := Compare(b, c)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range cmp.Metrics[:3] {
		if m.Baseline != 0 || m.ChangePercent != nil || m.ChangePercentNote != "baseline is 0" || m.Direction != Increase || m.Assessment != Worse {
			t.Errorf("%s: %+v, want no percentage from a zero baseline", m.Key, m)
		}
	}

	c.Result.Metrics.LatencyMs = workload.Latency{}
	cmp, err = Compare(b, c)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range cmp.Metrics[:3] {
		if m.Change != 0 || m.ChangePercent != nil || m.Direction != Unchanged || m.Assessment != NoChange {
			t.Errorf("%s (both 0): %+v", m.Key, m)
		}
	}
}

func TestCompareRejectsDifferentSettings(t *testing.T) {
	b := mustDecode(t, "baseline.json")
	for _, tc := range []struct {
		name string
		edit func(*Run)
		want []string
	}{
		{"target URL", func(r *Run) { r.Result.TargetURL = "http://127.0.0.1:8082/inventory/PHONE-7" },
			[]string{"target URL differs (baseline http://127.0.0.1:8082/inventory/LAPTOP-001, candidate http://127.0.0.1:8082/inventory/PHONE-7)"}},
		{"target URL secret part", func(r *Run) { r.Result.TargetURL = "http://127.0.0.1:8082/inventory/LAPTOP-001?token=SECRET" },
			[]string{"target URL differs (baseline http://127.0.0.1:8082/inventory/LAPTOP-001, candidate http://127.0.0.1:8082/inventory/LAPTOP-001?xxxxx)"}},
		{"virtual users", func(r *Run) { r.Result.VUs = 4 },
			[]string{"virtual users differ (baseline 2, candidate 4)"}},
		{"configured duration", func(r *Run) { r.Result.ConfiguredDurationSeconds = 10 },
			[]string{"configured duration differs (baseline 5s, candidate 10s)"}},
		{"everything", func(r *Run) {
			r.Result.TargetURL = "https://example.com/"
			r.Result.VUs = 1
			r.Result.ConfiguredDurationSeconds = 60
		}, []string{
			"target URL differs (baseline http://127.0.0.1:8082/inventory/LAPTOP-001, candidate https://example.com/)",
			"virtual users differ (baseline 2, candidate 1)",
			"configured duration differs (baseline 5s, candidate 60s)",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := b
			tc.edit(&c)
			cmp, err := Compare(b, c)
			var inc *IncomparableError
			if !errors.As(err, &inc) {
				t.Fatalf("err = %v, want *IncomparableError", err)
			}
			if !slices.Equal(inc.Mismatches, tc.want) {
				t.Fatalf("mismatches =\n%q\nwant\n%q", inc.Mismatches, tc.want)
			}
			if cmp.Metrics != nil || strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("comparison = %+v, err = %v", cmp, err)
			}
		})
	}
}

func TestCompareMismatchHiddenByRedaction(t *testing.T) {
	b, c := mustDecode(t, "baseline.json"), mustDecode(t, "baseline.json")
	b.Result.TargetURL = "http://127.0.0.1:8082/inventory/LAPTOP-001?token=A"
	c.Result.TargetURL = "http://127.0.0.1:8082/inventory/LAPTOP-001?token=B"
	_, err := Compare(b, c)
	var inc *IncomparableError
	if !errors.As(err, &inc) || !strings.Contains(err.Error(), "differs in its user information or query string") ||
		strings.Contains(err.Error(), "token=") {
		t.Fatalf("err = %v", err)
	}
}

func TestCaveats(t *testing.T) {
	always := []string{"Each side is a single run", "Only the target URL, virtual users, and configured duration were checked"}
	for _, tc := range []struct {
		name string
		edit func(b, c *Run)
		want []string
	}{
		{"none extra", func(b, c *Run) {}, nil},
		{"redacted URL", func(b, c *Run) {
			b.Result.TargetURL = "http://xxxxx@127.0.0.1:8082/inventory/LAPTOP-001?xxxxx"
			c.Result.TargetURL = b.Result.TargetURL
		}, []string{"stored redacted"}},
		{"different k6 version", func(b, c *Run) { c.Result.RunnerVersion = "2.4.0" },
			[]string{"different load generators (baseline k6 2.3.0, candidate k6 2.4.0)"}},
		{"candidate first", func(b, c *Run) { c.Result.StartedAt = b.Result.StartedAt.Add(-time.Hour) },
			[]string{"candidate run started before the baseline"}},
		{"different pass criteria", func(b, c *Run) { c.Verdict.MaxFailureRate = 0.05 },
			[]string{"different maximum failure rates (baseline 0.00%, candidate 5.00%)"}},
		{"failures with higher throughput", func(b, c *Run) {
			c.Result.Metrics.FailureRate, c.Result.Metrics.FailedRequests = 0.5, 6250
			c.Verdict.Passed, c.Verdict.Reasons = false, []string{"failure rate 50.00% exceeds the allowed 0.00%"}
		}, []string{"Latency includes failed requests", "fast failures can raise throughput",
			"candidate run completed but FAILED its pass criteria (failure rate 50.00% exceeds the allowed 0.00%)"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, c := mustDecode(t, "baseline.json"), mustDecode(t, "candidate_improvement.json")
			tc.edit(&b, &c)
			cmp, err := Compare(b, c)
			if err != nil {
				t.Fatal(err)
			}
			want := append(append([]string{}, always...), tc.want...)
			if len(cmp.Caveats) != len(want) {
				t.Fatalf("caveats =\n%s\nwant %d", strings.Join(cmp.Caveats, "\n"), len(want))
			}
			for i, w := range want {
				if !strings.Contains(cmp.Caveats[i], w) {
					t.Errorf("caveat %d = %q, want it to contain %q", i, cmp.Caveats[i], w)
				}
			}
		})
	}
}

func TestComparisonJSON(t *testing.T) {
	c := compareFixtures(t, "baseline.json", "candidate_regression.json")
	c.Baseline.Source, c.Candidate.Source = "baseline.json", "candidate.json"
	data, err := json.Marshal(Report{SchemaVersion: SchemaVersion, Comparison: &c})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		SchemaVersion int `json:"schema_version"`
		Comparison    struct {
			Baseline  map[string]any   `json:"baseline"`
			Candidate map[string]any   `json:"candidate"`
			Metrics   []map[string]any `json:"metrics"`
			Caveats   []string         `json:"caveats"`
		} `json:"comparison"`
		Error *string `json:"error"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.SchemaVersion != 1 || doc.Error != nil || len(doc.Comparison.Metrics) != 7 || len(doc.Comparison.Caveats) != 4 {
		t.Fatalf("report = %s", data)
	}
	if doc.Comparison.Baseline["source"] != "baseline.json" || doc.Comparison.Candidate["passed"] != false ||
		doc.Comparison.Baseline["target_url"] != "http://127.0.0.1:8082/inventory/LAPTOP-001" {
		t.Errorf("runs = %v / %v", doc.Comparison.Baseline, doc.Comparison.Candidate)
	}

	want := map[string]map[string]any{
		"latency_avg_ms": {"label": "Average latency", "unit": "ms", "preference": "lower_is_better",
			"baseline": 2.0, "candidate": 2.5, "change": 0.5, "change_percent": 25.0, "direction": "increase", "assessment": "worse"},
		"requests_per_second": {"unit": "requests/s", "preference": "higher_is_generally_better",
			"change": -400.0, "change_percent": -20.0, "direction": "decrease", "assessment": "generally_worse"},
		"failure_rate": {"unit": "ratio", "baseline": 0.0, "candidate": 0.01, "change_percent": nil,
			"change_percentage_points": 1.0, "assessment": "worse"},
		"failed_requests": {"unit": "requests", "preference": "not_assessed", "change": 80.0,
			"change_percent": nil, "change_percent_note": "baseline is 0", "assessment": "not_assessed"},
	}
	for _, m := range doc.Comparison.Metrics {
		for field, v := range want[m["key"].(string)] {
			got, ok := m[field]
			if !ok || got != v {
				t.Errorf("%s.%s = %v (present %v), want %v", m["key"], field, got, ok, v)
			}
		}
	}
}
