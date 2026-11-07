package experiment

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// MaxErrorsShown bounds the scrape errors WriteSummary prints; the report
// keeps all of them.
const MaxErrorsShown = 5

// WriteSummary prints a short, human-readable summary of r. It never prints
// metric values: the samples are only counted.
func WriteSummary(w io.Writer, r Report) error {
	bw := bufio.NewWriter(w)
	p := func(format string, args ...any) { fmt.Fprintf(bw, format, args...) }

	p("Ruptiq experiment report\n")
	p("  Status:          %s\n", statusLabel(r.Status))
	for _, reason := range r.Reasons {
		p("                   - %s\n", oneLine(reason))
	}
	p("  Target:          %s\n", r.Config.TargetURL)
	p("  Metrics:         %s (every %gs)\n", r.Config.MetricsURL, r.Config.SampleIntervalSeconds)
	p("  Started:         %s (took %.1fs)\n", r.StartedAt.Format("2006-01-02T15:04:05.000Z07:00"), r.FinishedAt.Sub(r.StartedAt).Seconds())

	if res := r.Workload.Result; res != nil {
		m := res.Metrics
		runner := res.Runner
		if res.RunnerVersion != "" {
			runner += " v" + res.RunnerVersion
		}
		p("  Workload:        %s, %d virtual users, %gs configured, %.2fs measured\n",
			runner, res.VUs, res.ConfiguredDurationSeconds, m.DurationSeconds)
		p("  HTTP requests:   %d (%.1f per second), %d failed (%.2f%%)\n",
			m.HTTPRequests, m.RequestsPerSecond, m.FailedRequests, m.FailureRate*100)
		p("  Latency (ms):    avg %.3f   p50 %.3f   p95 %.3f   max %.3f\n",
			m.LatencyMs.Avg, m.LatencyMs.P50, m.LatencyMs.P95, m.LatencyMs.Max)
		if r.Workload.Verdict.Passed {
			p("  Workload result: passed (failure rate at most %.2f%%)\n", r.Workload.Verdict.MaxFailureRate*100)
		} else {
			p("  Workload result: failed\n")
			for _, reason := range r.Workload.Verdict.Reasons {
				p("                   - %s\n", reason)
			}
		}
	} else {
		p("  Workload:        did not complete: %s\n", oneLine(r.Workload.Error))
	}

	t := r.Telemetry
	p("  Telemetry:       %s: %d of %d scrapes succeeded; %d samples kept (%d before, %d during, %d after the workload)\n",
		t.Completeness, t.Succeeded, t.Attempts, len(t.Samples),
		countPhase(t.Samples, BeforeWorkload), countPhase(t.Samples, DuringWorkload), countPhase(t.Samples, AfterWorkload))
	for _, reason := range t.CompletenessReasons {
		p("                   - %s\n", reason)
	}
	p("  Retained:        %d of %d bytes of sample data\n", t.RetainedBytes, r.Config.MaxTelemetryBytes)
	if n := len(t.Samples); n > 0 {
		first, last := t.Samples[0], t.Samples[n-1]
		p("  Samples:         #%d at +%.1fs to #%d at +%.1fs (metric families in the last: %d)\n",
			first.Sequence, first.OffsetSeconds, last.Sequence, last.OffsetSeconds, len(last.Families))
	}
	for i, e := range t.Errors {
		if i == MaxErrorsShown {
			p("                   ... %d more scrape errors in the report\n", len(t.Errors)-MaxErrorsShown)
			break
		}
		p("  Scrape error:    #%d at +%.1fs (%s): %s\n", e.Sequence, e.OffsetSeconds, phaseText(e.Phase), oneLine(e.Error))
	}
	return bw.Flush()
}

func statusLabel(s Status) string {
	return strings.ToUpper(strings.ReplaceAll(string(s), "_", " "))
}

func countPhase(samples []Sample, p Phase) int {
	n := 0
	for _, s := range samples {
		if s.Phase == p {
			n++
		}
	}
	return n
}

// oneLine collapses whitespace and shortens long messages, such as k6 output
// kept in an error, for the console. The report keeps the full text.
func oneLine(s string) string {
	const limit = 300
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > limit {
		s = string(r[:limit]) + " ..."
	}
	return s
}
