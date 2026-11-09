package fixvalidation

import (
	"strings"
	"testing"
)

func TestSummaryNamesTheConclusion(t *testing.T) {
	before := build(pair(2, rt("GET", "/work").latency(0.020, 0.010, 0.025))...)
	after := build(pair(2, rt("GET", "/work").requests(1400, 0, 0).latency(0.004, 0.002, 0.005))...)
	v := mustCompare(t, before, after)
	var b strings.Builder
	if err := WriteSummary(&b, v); err != nil {
		t.Fatal(err)
	}
	text := b.String()
	for _, want := range []string{
		"Outcome: improvement_observed",
		"GET /work  improvement_observed",
		"1000.0 requests/s → 1400.0 requests/s, +40.0%",
		"median of window estimates",
		"not a statistical significance test",
		"Client-side k6",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("summary lacks %q:\n%s", want, text)
		}
	}
}
