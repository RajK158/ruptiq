package comparison

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
)

// WriteText writes c as a readable console report.
func WriteText(w io.Writer, c Comparison) error {
	var b strings.Builder
	b.WriteString("Ruptiq run comparison (candidate compared with baseline)\n")
	fmt.Fprintf(&b, "  %-11s%s\n", "Target:", c.Baseline.TargetURL)
	fmt.Fprintf(&b, "  %-11s%d virtual users, %gs configured\n", "Load:", c.Baseline.VUs, c.Baseline.ConfiguredDurationSeconds)
	writeRun(&b, "Baseline:", c.Baseline)
	writeRun(&b, "Candidate:", c.Candidate)
	b.WriteString("\n")

	tw := tabwriter.NewWriter(&b, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "  Metric\tBaseline\tCandidate\tChange\tChange %\tAssessment")
	for _, m := range c.Metrics {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\n",
			label(m), value(m, m.Baseline), value(m, m.Candidate), changeText(m), percentText(m), assessmentText(m))
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	b.WriteString("\nCaveats:\n")
	for _, cv := range c.Caveats {
		fmt.Fprintf(&b, "  - %s\n", cv)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func writeRun(b *strings.Builder, name string, r RunInfo) {
	source := r.Source
	if source == "" {
		source = "(unnamed)"
	}
	verdict := "PASSED"
	if !r.Passed {
		verdict = "FAILED"
	}
	runner := r.Runner
	if r.RunnerVersion != "" {
		runner += " v" + r.RunnerVersion
	}
	fmt.Fprintf(b, "  %-11s%s (%s, started %s, measured %.2fs, %s)\n",
		name, source, runner, r.StartedAt.UTC().Format(time.RFC3339), r.MeasuredDurationSeconds, verdict)
}

func label(m MetricChange) string {
	switch m.Unit {
	case UnitMilliseconds:
		return m.Label + " (ms)"
	default:
		return m.Label
	}
}

func value(m MetricChange, v float64) string {
	switch m.Unit {
	case UnitMilliseconds:
		return fmt.Sprintf("%.3f", v)
	case UnitRequestsPerSecond:
		return fmt.Sprintf("%.1f", v)
	case UnitRatio:
		return fmt.Sprintf("%.2f%%", v*100)
	default:
		return fmt.Sprintf("%.0f", v)
	}
}

func changeText(m MetricChange) string {
	switch m.Unit {
	case UnitMilliseconds:
		return signed("%.3f", m.Change)
	case UnitRequestsPerSecond:
		return signed("%.1f", m.Change)
	case UnitRatio:
		return signed("%.2f", *m.ChangePercentagePoints) + " pp"
	default:
		return signed("%.0f", m.Change)
	}
}

func percentText(m MetricChange) string {
	switch {
	case m.ChangePercent != nil:
		return signed("%.1f", *m.ChangePercent) + "%"
	case m.Unit == UnitRatio:
		return "-"
	default:
		return "n/a (baseline 0)"
	}
}

// signed formats v with an explicit sign, except for exactly zero.
func signed(format string, v float64) string {
	if v == 0 {
		return fmt.Sprintf(format, 0.0)
	}
	return fmt.Sprintf("%+"+format[1:], v)
}

func assessmentText(m MetricChange) string {
	switch m.Assessment {
	case Better:
		return "better (lower is better)"
	case Worse:
		return "worse (lower is better)"
	case GenerallyBetter:
		return "higher (generally better)"
	case GenerallyWorse:
		return "lower (generally worse)"
	case NoChange:
		return "unchanged"
	default:
		return "not assessed"
	}
}
