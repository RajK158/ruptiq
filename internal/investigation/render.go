package investigation

import (
	"fmt"
	"io"
	"strings"
)

// Caps on the console summary; the JSON report holds everything.
const (
	maxSummaryFindings     = 10
	maxSummaryInconclusive = 10
	maxSummaryExcluded     = 10
	maxSummaryLine         = 600
)

// WriteSummary writes a short, bounded description of inv: the outcome, the
// ranked findings, and what could not be compared.
func WriteSummary(w io.Writer, inv Investigation) error {
	var b strings.Builder
	s, c := inv.Source, inv.Config
	fmt.Fprintf(&b, "Source:       %s experiment, telemetry %s, %d windows (%d comparable: %s)\n",
		s.ExperimentStatus, s.TelemetryCompleteness, s.Windows, len(inv.Windows.Comparable), windowList(inv.Windows.Comparable))
	fmt.Fprintf(&b, "Thresholds:   %d+ windows and %g+ requests per side; 5xx ratio +%s points and %gx (%g+ errors); p95 %s; throughput %s\n",
		c.MinWindows, c.MinRequests, points(c.ErrorRatioIncrease), c.ErrorRatioFactor, c.MinServerErrors,
		percent(c.LatencyIncrease), percent(-c.ThroughputDecrease))
	fmt.Fprintf(&b, "Outcome:      %s\n              %s\n", inv.Outcome, inv.Summary)

	if len(inv.Findings) > 0 {
		b.WriteString("\nFindings (ranked; observations and hypotheses to test, not proven root causes):\n")
	}
	for i, f := range inv.Findings {
		if i == maxSummaryFindings {
			fmt.Fprintf(&b, "  ... and %d more findings in the JSON report\n", len(inv.Findings)-i)
			break
		}
		label := f.Designation
		if f.Severity != "" {
			label = f.Severity + ", " + label
		}
		fmt.Fprintf(&b, "%3d. %s [%s] %s\n", f.Rank, f.Type, label, f.ID)
		fmt.Fprintf(&b, "     %s\n", line(f.Symptom))
		if f.Type == FindingInsufficientEvidence {
			for _, g := range f.Gaps {
				fmt.Fprintf(&b, "     %s: %s\n", g.Signal, line(g.Reason))
			}
		} else {
			fmt.Fprintf(&b, "     Windows: baseline %s, comparison %s; %d evidence references (first: %s)\n",
				windowList(f.BaselineWindows), windowList(f.ComparisonWindows), len(f.Evidence), firstPointer(f.Evidence))
		}
		if n := len(f.Contradictions); n > 0 {
			more := ""
			if n > 1 {
				more = fmt.Sprintf(" (and %d more)", n-1)
			}
			fmt.Fprintf(&b, "     Contradicting: %s%s\n", line(f.Contradictions[0]), more)
		}
		fmt.Fprintf(&b, "     Next: %s\n", line(f.NextExperiment))
	}

	var inconclusive []Assessment
	for _, a := range inv.Assessments {
		if a.Status == StatusInconclusive {
			inconclusive = append(inconclusive, a)
		}
	}
	if len(inconclusive) > 0 {
		b.WriteString("\nInconclusive (threshold reached, but not robust; not reported as findings):\n")
	}
	for i, a := range inconclusive {
		if i == maxSummaryInconclusive {
			fmt.Fprintf(&b, "  ... and %d more in the JSON report\n", len(inconclusive)-i)
			break
		}
		fmt.Fprintf(&b, "  %s %s %s: %s\n", orAny(a.Method, "(any method)"), orAny(a.Route, "(any route)"), a.Signal, line(a.Reason))
	}

	if len(inv.Windows.Excluded) > 0 {
		b.WriteString("\nExcluded windows:\n")
	}
	for i, e := range inv.Windows.Excluded {
		if i == maxSummaryExcluded {
			fmt.Fprintf(&b, "  ... and %d more in the JSON report\n", len(inv.Windows.Excluded)-i)
			break
		}
		fmt.Fprintf(&b, "  window %d: %s\n", e.Window, line(e.Reason))
	}
	b.WriteString("\nNot a diagnosis: no finding is a proven root cause, and no statistical significance test is made. See the limitations in the JSON report.\n")
	_, err := io.WriteString(w, b.String())
	return err
}

func firstPointer(refs []EvidenceRef) string {
	if len(refs) == 0 {
		return "none"
	}
	return refs[0].Pointer
}

func line(s string) string {
	if r := []rune(s); len(r) > maxSummaryLine {
		return string(r[:maxSummaryLine]) + "..."
	}
	return s
}
