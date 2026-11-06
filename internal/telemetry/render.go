package telemetry

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Limits on what WriteSummary prints, so a large or hostile endpoint cannot
// flood the console. The Snapshot itself is not truncated.
const (
	MaxFamiliesShown = 30
	MaxSeriesShown   = 8
	maxNameLen       = 100
	maxLabelValueLen = 60
)

// WriteSummary prints a short, readable overview of s: where and when it was
// read, how many families and series it holds, and the first few series of
// the first families with their values. Text from the target (names and label
// values) is quoted where needed and shortened.
func WriteSummary(w io.Writer, s Snapshot) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Endpoint:    %s\n", s.Endpoint)
	fmt.Fprintf(&b, "Scraped at:  %s (took %.1f ms)\n", s.ScrapedAt.Format("2006-01-02T15:04:05.000Z07:00"), s.ScrapeDurationSeconds*1000)
	fmt.Fprintf(&b, "Contents:    %d families, %d series (%s)\n", len(s.Families), s.SeriesCount(), typeCounts(s))

	for i, f := range s.Families {
		if i == MaxFamiliesShown {
			fmt.Fprintf(&b, "\n... %d more families not shown\n", len(s.Families)-i)
			break
		}
		fmt.Fprintf(&b, "\n%s  %s, %d series\n", displayName(f.Name), f.Type, len(f.Series))
		for j, series := range f.Series {
			if j == MaxSeriesShown {
				fmt.Fprintf(&b, "  ... %d more series not shown\n", len(f.Series)-j)
				break
			}
			labels := displayLabels(series.Labels)
			if labels == "" {
				labels = "(no labels)"
			}
			fmt.Fprintf(&b, "  %s  %s\n", labels, displayValue(series))
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func typeCounts(s Snapshot) string {
	counts := make(map[MetricType]int)
	for _, f := range s.Families {
		counts[f.Type]++
	}
	var parts []string
	for _, t := range []MetricType{Counter, Gauge, Histogram, Summary, Untyped} {
		if counts[t] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[t], t))
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

func displayValue(s Series) string {
	switch {
	case s.Value != nil:
		return formatFloat(*s.Value)
	case s.Histogram != nil:
		return fmt.Sprintf("count %d, sum %s, %d buckets", s.Histogram.Count, formatFloat(s.Histogram.Sum), len(s.Histogram.Buckets))
	case s.Summary != nil:
		return fmt.Sprintf("count %d, sum %s, %d quantiles", s.Summary.Count, formatFloat(s.Summary.Sum), len(s.Summary.Quantiles))
	}
	return "(no value)"
}

func formatFloat(f Float) string {
	return strconv.FormatFloat(float64(f), 'g', -1, 64)
}

// displayName returns name as is if it is plain, or quoted (escaping control
// characters) if not, shortened to maxNameLen characters.
func displayName(name string) string {
	name = shorten(name, maxNameLen)
	if strings.IndexFunc(name, func(c rune) bool {
		return !(c == '_' || c == ':' || c == '.' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9')
	}) >= 0 {
		return strconv.Quote(name)
	}
	return name
}

func displayLabels(l map[string]string) string {
	if len(l) == 0 {
		return ""
	}
	short := make(map[string]string, len(l))
	for n, v := range l {
		short[shorten(n, maxNameLen)] = shorten(v, maxLabelValueLen)
	}
	return labelString(short)
}

// shorten cuts s to at most n runes, marking the cut with "...".
func shorten(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return string(runes[:n-3]) + "..."
}
