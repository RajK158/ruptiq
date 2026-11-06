package comparison

import (
	"strings"
	"testing"
)

func TestWriteText(t *testing.T) {
	c := compareFixtures(t, "baseline.json", "candidate_regression.json")
	c.Baseline.Source, c.Candidate.Source = "baseline.json", "candidate_regression.json"
	var b strings.Builder
	if err := WriteText(&b, c); err != nil {
		t.Fatal(err)
	}
	want := `Ruptiq run comparison (candidate compared with baseline)
  Target:    http://127.0.0.1:8082/inventory/LAPTOP-001
  Load:      2 virtual users, 5s configured
  Baseline:  baseline.json (k6 v2.3.0, started 2026-11-06T10:00:00Z, measured 5.00s, PASSED)
  Candidate: candidate_regression.json (k6 v2.3.0, started 2026-11-06T10:10:00Z, measured 5.00s, FAILED)

  Metric                 Baseline   Candidate   Change     Change %           Assessment
  Average latency (ms)   2.000      2.500       +0.500     +25.0%             worse (lower is better)
  p50 latency (ms)       1.500      1.500       0.000      0.0%               unchanged
  p95 latency (ms)       4.000      5.000       +1.000     +25.0%             worse (lower is better)
  Requests per second    2000.0     1600.0      -400.0     -20.0%             lower (generally worse)
  HTTP failure rate      0.00%      1.00%       +1.00 pp   -                  worse (lower is better)
  Failed requests        0          80          +80        n/a (baseline 0)   not assessed
  Total requests         10000      8000        -2000      -20.0%             not assessed

Caveats:
  - Each side is a single run. Ruptiq does not measure run-to-run variation or statistical significance, so small differences may be noise; repeat both runs before drawing conclusions.
  - Only the target URL, virtual users, and configured duration were checked. Ruptiq cannot tell whether the target's code, data, configuration, or the machines and network changed between runs.
  - At least one run had failed requests. Latency includes failed requests, which k6 may time as 0 ms (for example refused connections), so latency changes are hard to interpret.
  - The candidate run completed but FAILED its pass criteria (failure rate 1.00% exceeds the allowed 0.00%).
`
	if got := b.String(); got != want {
		t.Fatalf("WriteText =\n%s\nwant\n%s", got, want)
	}
}

func TestWriteTextImprovementLabels(t *testing.T) {
	c := compareFixtures(t, "baseline.json", "candidate_improvement.json")
	var b strings.Builder
	if err := WriteText(&b, c); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"  Baseline:  (unnamed) (k6 v2.3.0,",
		"  Average latency (ms)   2.000      1.500       -0.500    -25.0%             better (lower is better)\n",
		"  Requests per second    2000.0     2500.0      +500.0    +25.0%             higher (generally better)\n",
		"  HTTP failure rate      0.00%      0.00%       0.00 pp   -                  unchanged\n",
		"  Failed requests        0          0           0         n/a (baseline 0)   not assessed\n",
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("output missing %q:\n%s", want, b.String())
		}
	}
}
