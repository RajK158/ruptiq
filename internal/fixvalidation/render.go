package fixvalidation

import (
	"fmt"
	"io"
	"strings"
)

const (
	maxSummaryRoutes = 20
	maxSummaryLine   = 500
)

// WriteSummary writes a short table of route-level differences and the
// overall conclusion. The JSON report holds every pointer and limitation.
func WriteSummary(w io.Writer, v Validation) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Before: %s, %d VUs, %.4gs, interval %.4gs, %d windows (%d eligible: %s)\n",
		v.Before.TargetURL, v.Before.VUs, v.Before.DurationSeconds, v.Before.SampleIntervalSeconds,
		v.Before.Windows, len(v.Before.EligibleWindows), windowList(v.Before.EligibleWindows))
	fmt.Fprintf(&b, "After:  %s, %d VUs, %.4gs, interval %.4gs, %d windows (%d eligible: %s)\n",
		v.After.TargetURL, v.After.VUs, v.After.DurationSeconds, v.After.SampleIntervalSeconds,
		v.After.Windows, len(v.After.EligibleWindows), windowList(v.After.EligibleWindows))
	fmt.Fprintf(&b, "Thresholds: %d+ eligible windows; throughput ±%s; 5xx ratio ±%s points and %gx (%g+ errors); median window p95 ±%s\n",
		v.Config.MinWindows, percent(v.Config.ThroughputChange), points(v.Config.ErrorRatioIncrease),
		v.Config.ErrorRatioFactor, v.Config.MinServerErrors, percent(v.Config.LatencyChange))
	if v.Comparability.Comparable {
		b.WriteString("Comparable: yes. Matching configuration does not mean the environment matched.\n")
	} else {
		b.WriteString("Comparable: no\n")
		for _, r := range v.Comparability.Reasons {
			fmt.Fprintf(&b, "  %s\n", line(r))
		}
	}
	fmt.Fprintf(&b, "Outcome: %s\n          %s\n", v.Outcome, line(v.Summary))

	if len(v.Routes) > 0 {
		b.WriteString("\nRoutes (server-side; an observed change is not a proven cause):\n")
	}
	for i, r := range v.Routes {
		if i == maxSummaryRoutes {
			fmt.Fprintf(&b, "  ... and %d more routes in the JSON report\n", len(v.Routes)-i)
			break
		}
		fmt.Fprintf(&b, "  %s %s  %s  (present in %s)\n", orDash(r.Method), orDash(r.Route), r.Outcome, r.PresentIn)
		if r.Throughput != nil {
			fmt.Fprintf(&b, "    throughput: %s\n", line(signalLine(r.Throughput)))
		}
		if r.ServerErrors != nil {
			fmt.Fprintf(&b, "    server_error_ratio: %s\n", line(signalLine(r.ServerErrors)))
		}
		if r.Latency != nil {
			fmt.Fprintf(&b, "    p95_latency (median of window estimates): %s\n", line(signalLine(r.Latency)))
		}
		if r.PresentIn != PresentBoth {
			fmt.Fprintf(&b, "    %s\n", line(r.Summary))
		}
	}
	if len(v.Ranked) > 0 {
		b.WriteString("\nRanked material changes (regressions first):\n")
		for _, c := range v.Ranked {
			fmt.Fprintf(&b, "  %d. %s %s %s [%s] %s\n", c.Rank, orDash(c.Method), orDash(c.Route), c.Signal, c.Direction, line(changeText(c)))
		}
	}
	b.WriteString("\nClient-side k6 measurements are in the JSON report and are not part of this conclusion.\n")
	b.WriteString("Not a causal result, not a reproducibility result, and not a statistical significance test.\n")
	_, err := io.WriteString(w, b.String())
	return err
}

func signalLine(s *SignalComparison) string {
	if s.Before == nil || s.After == nil {
		return s.Status + ": " + s.Reason
	}
	switch s.Signal {
	case SignalServerErrors:
		pp := ""
		if s.PercentagePoints != nil {
			pp = fmt.Sprintf(", %s points", signedPoints(*s.PercentagePoints/100))
		}
		return fmt.Sprintf("%s → %s%s [%s]; %s", ratioText(s.Before.Value), ratioText(s.After.Value), pp, s.Status, s.Reason)
	case SignalLatency:
		bounds := ""
		if s.Before.BucketLowerSeconds != nil && s.After.BucketLowerSeconds != nil {
			bounds = fmt.Sprintf("; buckets [%s, %s] → [%s, %s]",
				seconds(*s.Before.BucketLowerSeconds), seconds(*s.Before.BucketUpperSeconds),
				seconds(*s.After.BucketLowerSeconds), seconds(*s.After.BucketUpperSeconds))
		}
		return fmt.Sprintf("%s → %s [%s]%s", seconds(s.Before.Value), seconds(s.After.Value), s.Status, bounds)
	default:
		rel := ""
		if s.Relative != nil {
			rel = ", " + percent(*s.Relative)
		}
		return fmt.Sprintf("%s → %s%s [%s]", rate(s.Before.Value), rate(s.After.Value), rel, s.Status)
	}
}

func changeText(c RankedChange) string {
	if c.Signal == SignalServerErrors && c.PercentagePoints != nil {
		return signedPoints(*c.PercentagePoints/100) + " points"
	}
	if c.Relative != nil {
		return percent(*c.Relative)
	}
	return ""
}

func windowList(ws []int) string {
	if len(ws) == 0 {
		return "none"
	}
	var parts []string
	for i := 0; i < len(ws); {
		j := i
		for j+1 < len(ws) && ws[j+1] == ws[j]+1 {
			j++
		}
		if j == i {
			parts = append(parts, fmt.Sprint(ws[i]))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", ws[i], ws[j]))
		}
		i = j + 1
	}
	return strings.Join(parts, ", ")
}

func line(s string) string {
	if r := []rune(s); len(r) > maxSummaryLine {
		return string(r[:maxSummaryLine]) + "..."
	}
	return s
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
