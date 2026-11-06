package telemetry

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestWriteSummary(t *testing.T) {
	var b strings.Builder
	if err := WriteSummary(&b, fixtureSnapshot(t)); err != nil {
		t.Fatal(err)
	}
	golden(t, "valid_summary.golden.txt", []byte(b.String()))
}

func TestWriteSummaryLimitsOutput(t *testing.T) {
	// 40 families of 20 series each, with long and hostile names and values.
	s := Snapshot{Endpoint: "http://h/metrics", ScrapedAt: time.Unix(0, 0).UTC()}
	long := strings.Repeat("v", 500)
	for i := range 40 {
		f := Family{Name: fmt.Sprintf("family_%02d", i), Type: Gauge}
		if i == 0 {
			f.Name = "evil\x1b[31m\nname" + strings.Repeat("n", 300)
		}
		for j := range 20 {
			v := Float(j)
			f.Series = append(f.Series, Series{Labels: map[string]string{"id": fmt.Sprintf("%d-%s\r\n", j, long)}, Value: &v})
		}
		s.Families = append(s.Families, f)
	}

	var b strings.Builder
	if err := WriteSummary(&b, s); err != nil {
		t.Fatal(err)
	}
	out := b.String()

	if !strings.Contains(out, "40 families, 800 series (40 gauge)") {
		t.Errorf("totals missing:\n%s", out)
	}
	if !strings.Contains(out, "... 10 more families not shown") {
		t.Error("missing family cut marker")
	}
	if n := strings.Count(out, "... 12 more series not shown"); n != MaxFamiliesShown {
		t.Errorf("series cut markers = %d, want %d", n, MaxFamiliesShown)
	}
	if strings.ContainsAny(out, "\x1b\r") {
		t.Error("control characters reached the output")
	}
	for _, line := range strings.Split(out, "\n") {
		if len(line) > 200 {
			t.Errorf("line of %d bytes: %.80s...", len(line), line)
		}
	}
	if lines := strings.Count(out, "\n"); lines > 400 {
		t.Errorf("summary has %d lines", lines)
	}
	if !strings.Contains(out, `"evil\x1b[31m\nname`) {
		t.Errorf("hostile family name not quoted:\n%.300s", out)
	}
}
