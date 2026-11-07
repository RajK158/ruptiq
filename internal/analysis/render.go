package analysis

import (
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
)

// Bounds on the summary; the JSON report has everything.
const (
	maxSummaryRoutes      = 20
	maxSummaryWindowLines = 20
	maxSummaryWarnings    = 10
)

// WriteSummary writes a concise, human-readable summary of a. It shows
// derived signals only, never raw telemetry.
func WriteSummary(w io.Writer, a Analysis) error {
	var b strings.Builder
	e := a.Experiment
	fmt.Fprintf(&b, "Experiment:   %s, telemetry %s (%d samples, %d scrape errors, %d not retained)\n",
		e.Status, e.Completeness, e.Samples, e.ScrapeErrors, e.NotRetained)
	fmt.Fprintf(&b, "  Target:     %s\n", e.TargetURL)
	fmt.Fprintf(&b, "  Metrics:    %s\n", e.MetricsURL)

	gaps := 0
	total := 0.0
	for _, win := range a.Windows {
		total += win.ElapsedSeconds
		if len(win.MissedScrapes) > 0 {
			gaps++
		}
	}
	if n := len(a.Windows); n == 0 {
		fmt.Fprintf(&b, "Windows:      none (fewer than two samples)\n")
	} else {
		fmt.Fprintf(&b, "Windows:      %d, covering %.3fs (samples #%d to #%d); %d span missed scrapes\n",
			n, total, a.Windows[0].From.Sequence, a.Windows[n-1].To.Sequence, gaps)
	}
	c := a.Client.Metrics
	fmt.Fprintf(&b, "Client:       %s: %d requests, %.1f req/s, %.2f%% failed, p95 %s (measured by the load generator)\n",
		a.Client.Source, c.HTTPRequests, c.RequestsPerSecond, c.FailureRate*100, formatSeconds(c.LatencyMs.P95/1000))

	fmt.Fprintf(&b, "\nServer signals over all usable windows (%s, %s):\n", a.Config.RequestsCounter, a.Config.DurationHistogram)
	if len(a.Totals) == 0 {
		fmt.Fprintf(&b, "  none: no series of the configured metrics were found\n")
	}
	for i, t := range a.Totals {
		if i == maxSummaryRoutes {
			fmt.Fprintf(&b, "  ... %d more routes in the JSON report\n", len(a.Totals)-i)
			break
		}
		fmt.Fprintf(&b, "  %s\n", routeName(t))
		if r := t.Requests; r != nil {
			fmt.Fprintf(&b, "    Throughput: %s requests in %.3fs = %.1f req/s (windows %s)\n",
				formatCount(r.Requests), r.ElapsedSeconds, r.PerSecond, formatWindows(r.Evidence.Windows))
			fmt.Fprintf(&b, "    5xx:        %s\n", serverErrors(r))
		} else {
			fmt.Fprintf(&b, "    Throughput: unavailable: %s\n", t.RequestsUnavailable)
		}
		if l := t.Latency; l != nil {
			fmt.Fprintf(&b, "    Latency:    %s\n", latency(l))
		} else {
			fmt.Fprintf(&b, "    Latency:    unavailable: %s\n", t.LatencyUnavailable)
		}
	}

	if len(a.Windows) > 0 {
		fmt.Fprintf(&b, "\nWindows:\n")
	}
	lines := 0
	hidden := 0
	for _, win := range a.Windows {
		for _, r := range win.Routes {
			if lines == maxSummaryWindowLines {
				hidden++
				continue
			}
			lines++
			fmt.Fprintf(&b, "  %3d  #%d->#%d  %7.3fs  %s: %s\n", win.Index, win.From.Sequence, win.To.Sequence,
				win.ElapsedSeconds, routeName(r), windowLine(r))
		}
		if len(win.Routes) == 0 {
			if lines == maxSummaryWindowLines {
				hidden++
				continue
			}
			lines++
			fmt.Fprintf(&b, "  %3d  #%d->#%d  %7.3fs  no series of the configured metrics\n",
				win.Index, win.From.Sequence, win.To.Sequence, win.ElapsedSeconds)
		}
	}
	if hidden > 0 {
		fmt.Fprintf(&b, "  ... %d more lines in the JSON report\n", hidden)
	}

	if n := len(a.Warnings); n > 0 {
		fmt.Fprintf(&b, "\nWarnings (%d):\n", n)
		for i, wn := range a.Warnings {
			if i == maxSummaryWarnings {
				fmt.Fprintf(&b, "  ... %d more in the JSON report\n", n-i)
				break
			}
			fmt.Fprintf(&b, "  - %s\n", warningLine(wn))
		}
	}

	fmt.Fprintf(&b, "\nLimitations:\n")
	for _, l := range a.Limitations {
		fmt.Fprintf(&b, "  - %s\n", l)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func routeName(r RouteSignals) string {
	m, p := r.Method, r.Route
	if m == "" {
		m = "(any method)"
	}
	if p == "" {
		p = "(any route)"
	}
	return m + " " + p
}

func windowLine(r RouteSignals) string {
	var parts []string
	if q := r.Requests; q != nil {
		parts = append(parts, fmt.Sprintf("%.1f req/s", q.PerSecond))
		if q.ServerErrorRatio != nil {
			parts = append(parts, fmt.Sprintf("5xx %.2f%%", *q.ServerErrorRatio*100))
		} else {
			parts = append(parts, "5xx n/a")
		}
	} else {
		parts = append(parts, "requests unavailable")
	}
	if l := r.Latency; l != nil && l.P95.Seconds != nil {
		parts = append(parts, "p95 ~"+formatSeconds(*l.P95.Seconds))
	} else {
		parts = append(parts, "p95 n/a")
	}
	return strings.Join(parts, ", ")
}

func serverErrors(r *RequestSignals) string {
	s := fmt.Sprintf("%s of %s", formatCount(r.ServerErrors), formatCount(r.Requests))
	if r.ServerErrorRatio != nil {
		s += fmt.Sprintf(" (%.2f%%)", *r.ServerErrorRatio*100)
	} else {
		s += " (ratio unavailable: " + r.ServerErrorRatioUnavailable + ")"
	}
	return s + fmt.Sprintf("; 4xx client errors, not counted as server errors: %s", formatCount(r.ClientErrors))
}

func latency(l *LatencySignals) string {
	var s string
	if p := l.P95; p.Seconds != nil {
		s = fmt.Sprintf("p95 ~%s (estimate; within the %s to %s bucket)",
			formatSeconds(*p.Seconds), formatSeconds(*p.BucketLowerSeconds), formatSeconds(*p.BucketUpperSeconds))
	} else {
		s = "p95 unavailable: " + p.Unavailable
	}
	if l.MeanSeconds != nil {
		s += ", mean " + formatSeconds(*l.MeanSeconds)
	}
	return s + fmt.Sprintf(", %d requests", l.Observations)
}

func warningLine(w Warning) string {
	var where []string
	if w.Window > 0 {
		where = append(where, fmt.Sprintf("window %d", w.Window))
	}
	if w.Sequence > 0 {
		where = append(where, fmt.Sprintf("sample #%d", w.Sequence))
	}
	if w.Metric != "" {
		where = append(where, w.Metric+labelText(w.Labels))
	}
	prefix := w.Kind
	if len(where) > 0 {
		prefix += " (" + strings.Join(where, ", ") + ")"
	}
	return prefix + ": " + w.Message
}

// labelText formats labels as {a="1",b="2"}, sorted by name.
func labelText(l map[string]string) string {
	if len(l) == 0 {
		return ""
	}
	names := make([]string, 0, len(l))
	for n := range l {
		names = append(names, n)
	}
	slices.Sort(names)
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = n + "=" + strconv.Quote(l[n])
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// formatSeconds shows a duration given in seconds, in milliseconds below one
// second.
func formatSeconds(s float64) string {
	if s < 1 {
		return fmt.Sprintf("%.3f ms", s*1000)
	}
	return fmt.Sprintf("%.3f s", s)
}

func formatCount(x float64) string {
	return strconv.FormatFloat(x, 'f', -1, 64)
}

func formatWindows(ws []int) string {
	if len(ws) == 0 {
		return "none"
	}
	var parts []string
	for i := 0; i < len(ws); {
		j := i
		for j+1 < len(ws) && ws[j+1] == ws[j]+1 {
			j++
		}
		if i == j {
			parts = append(parts, strconv.Itoa(ws[i]))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", ws[i], ws[j]))
		}
		i = j + 1
	}
	return strings.Join(parts, ", ")
}
