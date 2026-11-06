package comparison

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/RajK158/ruptiq/internal/workload"
)

// SchemaVersion is the current format of a comparison Report.
const SchemaVersion = 1

// IncomparableError means the two runs used different workload settings, so
// their measurements describe different experiments.
type IncomparableError struct {
	Mismatches []string
}

func (e *IncomparableError) Error() string {
	return "runs are not comparable: " + strings.Join(e.Mismatches, "; ")
}

// Preference says which direction of change is desirable for a metric.
type Preference string

const (
	LowerIsBetter           Preference = "lower_is_better"
	HigherIsGenerallyBetter Preference = "higher_is_generally_better"
	NotAssessed             Preference = "not_assessed"
)

// Direction is how a metric moved from baseline to candidate.
type Direction string

const (
	Increase  Direction = "increase"
	Decrease  Direction = "decrease"
	Unchanged Direction = "unchanged"
)

// Assessment says whether a change is desirable, given the metric's
// Preference. It describes the measurements only; it is not a verdict on the
// system.
type Assessment string

const (
	Better          Assessment = "better"
	Worse           Assessment = "worse"
	GenerallyBetter Assessment = "generally_better"
	GenerallyWorse  Assessment = "generally_worse"
	NoChange        Assessment = "unchanged"
	NoAssessment    Assessment = "not_assessed"
)

// Unit of a metric's values.
const (
	UnitMilliseconds      = "ms"
	UnitRequestsPerSecond = "requests/s"
	UnitRatio             = "ratio"
	UnitRequests          = "requests"
)

// MetricChange is one metric in both runs and how it changed.
type MetricChange struct {
	Key        string     `json:"key"`
	Label      string     `json:"label"`
	Unit       string     `json:"unit"`
	Preference Preference `json:"preference"`
	Baseline   float64    `json:"baseline"`
	Candidate  float64    `json:"candidate"`
	// Change is Candidate - Baseline, in Unit.
	Change float64 `json:"change"`
	// ChangePercent is Change relative to Baseline, in percent. It is null
	// when Baseline is 0 (no meaningful relative change exists) and for
	// rates, whose change is given in percentage points instead.
	ChangePercent     *float64 `json:"change_percent"`
	ChangePercentNote string   `json:"change_percent_note,omitempty"`
	// ChangePercentagePoints is set for rates: the difference of the two
	// rates expressed as percentages.
	ChangePercentagePoints *float64   `json:"change_percentage_points,omitempty"`
	Direction              Direction  `json:"direction"`
	Assessment             Assessment `json:"assessment"`
}

// RunInfo identifies one side of a comparison.
type RunInfo struct {
	// Source is where the report came from, such as its file name. Set by the
	// caller; Compare leaves it empty.
	Source                    string    `json:"source,omitempty"`
	Runner                    string    `json:"runner"`
	RunnerVersion             string    `json:"runner_version,omitempty"`
	TargetURL                 string    `json:"target_url"`
	VUs                       int       `json:"vus"`
	ConfiguredDurationSeconds float64   `json:"configured_duration_seconds"`
	MeasuredDurationSeconds   float64   `json:"measured_duration_seconds"`
	StartedAt                 time.Time `json:"started_at"`
	FinishedAt                time.Time `json:"finished_at"`
	Passed                    bool      `json:"passed"`
	MaxFailureRate            float64   `json:"max_failure_rate"`
	VerdictReasons            []string  `json:"verdict_reasons,omitempty"`
}

// Comparison is the result of comparing a candidate run with a baseline run.
type Comparison struct {
	Baseline  RunInfo        `json:"baseline"`
	Candidate RunInfo        `json:"candidate"`
	Metrics   []MetricChange `json:"metrics"`
	// Caveats are limits on what the comparison shows. There is always at
	// least one.
	Caveats []string `json:"caveats"`
}

// Report is the JSON file of a comparison. Exactly one of Comparison and
// Error is set, so a failed comparison never leaves an older result looking
// current.
type Report struct {
	SchemaVersion int         `json:"schema_version"`
	Comparison    *Comparison `json:"comparison,omitempty"`
	Error         string      `json:"error,omitempty"`
	// Mismatches lists the differing settings when the runs were not
	// comparable.
	Mismatches []string `json:"mismatches,omitempty"`
}

// Compare compares candidate with baseline. Both must come from DecodeReport.
// It returns *IncomparableError if the runs used a different target URL,
// number of virtual users, or configured duration.
func Compare(baseline, candidate Run) (Comparison, error) {
	b, c := baseline.Result, candidate.Result
	var mismatches []string
	if b.TargetURL != c.TargetURL {
		bu, cu := workload.RedactURL(b.TargetURL), workload.RedactURL(c.TargetURL)
		msg := fmt.Sprintf("target URL differs (baseline %s, candidate %s)", bu, cu)
		if bu == cu {
			msg = fmt.Sprintf("target URL differs in its user information or query string (both shown redacted as %s)", bu)
		}
		mismatches = append(mismatches, msg)
	}
	if b.VUs != c.VUs {
		mismatches = append(mismatches, fmt.Sprintf("virtual users differ (baseline %d, candidate %d)", b.VUs, c.VUs))
	}
	if b.ConfiguredDurationSeconds != c.ConfiguredDurationSeconds {
		mismatches = append(mismatches, fmt.Sprintf("configured duration differs (baseline %gs, candidate %gs)",
			b.ConfiguredDurationSeconds, c.ConfiguredDurationSeconds))
	}
	if len(mismatches) > 0 {
		return Comparison{}, &IncomparableError{Mismatches: mismatches}
	}

	bm, cm := b.Metrics, c.Metrics
	return Comparison{
		Baseline:  info(baseline),
		Candidate: info(candidate),
		Metrics: []MetricChange{
			change("latency_avg_ms", "Average latency", UnitMilliseconds, LowerIsBetter, bm.LatencyMs.Avg, cm.LatencyMs.Avg),
			change("latency_p50_ms", "p50 latency", UnitMilliseconds, LowerIsBetter, bm.LatencyMs.P50, cm.LatencyMs.P50),
			change("latency_p95_ms", "p95 latency", UnitMilliseconds, LowerIsBetter, bm.LatencyMs.P95, cm.LatencyMs.P95),
			change("requests_per_second", "Requests per second", UnitRequestsPerSecond, HigherIsGenerallyBetter, bm.RequestsPerSecond, cm.RequestsPerSecond),
			change("failure_rate", "HTTP failure rate", UnitRatio, LowerIsBetter, bm.FailureRate, cm.FailureRate),
			change("failed_requests", "Failed requests", UnitRequests, NotAssessed, float64(bm.FailedRequests), float64(cm.FailedRequests)),
			change("http_requests", "Total requests", UnitRequests, NotAssessed, float64(bm.HTTPRequests), float64(cm.HTTPRequests)),
		},
		Caveats: caveats(baseline, candidate),
	}, nil
}

func info(r Run) RunInfo {
	return RunInfo{
		Runner:                    r.Result.Runner,
		RunnerVersion:             r.Result.RunnerVersion,
		TargetURL:                 workload.RedactURL(r.Result.TargetURL),
		VUs:                       r.Result.VUs,
		ConfiguredDurationSeconds: r.Result.ConfiguredDurationSeconds,
		MeasuredDurationSeconds:   r.Result.Metrics.DurationSeconds,
		StartedAt:                 r.Result.StartedAt,
		FinishedAt:                r.Result.FinishedAt,
		Passed:                    r.Verdict.Passed,
		MaxFailureRate:            r.Verdict.MaxFailureRate,
		VerdictReasons:            r.Verdict.Reasons,
	}
}

func change(key, label, unit string, pref Preference, baseline, candidate float64) MetricChange {
	m := MetricChange{
		Key: key, Label: label, Unit: unit, Preference: pref,
		Baseline: baseline, Candidate: candidate,
		Change: candidate - baseline,
	}
	switch {
	case unit == UnitRatio:
		pp := candidate*100 - baseline*100
		m.ChangePercentagePoints = &pp
		m.ChangePercentNote = "rate: see change_percentage_points"
	case baseline == 0:
		m.ChangePercentNote = "baseline is 0"
	default:
		// Multiplying first keeps results exact for exactly representable
		// inputs (for example -400 of 2000 is -20, not -20.000000000000004).
		pct := (candidate - baseline) * 100 / baseline
		m.ChangePercent = &pct
	}

	switch {
	case candidate > baseline:
		m.Direction = Increase
	case candidate < baseline:
		m.Direction = Decrease
	default:
		m.Direction = Unchanged
	}

	switch {
	case pref == NotAssessed:
		m.Assessment = NoAssessment
	case m.Direction == Unchanged:
		m.Assessment = NoChange
	case pref == LowerIsBetter && m.Direction == Decrease:
		m.Assessment = Better
	case pref == LowerIsBetter:
		m.Assessment = Worse
	case m.Direction == Increase:
		m.Assessment = GenerallyBetter
	default:
		m.Assessment = GenerallyWorse
	}
	return m
}

func caveats(baseline, candidate Run) []string {
	b, c := baseline.Result, candidate.Result
	out := []string{
		"Each side is a single run. Ruptiq does not measure run-to-run variation or statistical " +
			"significance, so small differences may be noise; repeat both runs before drawing conclusions.",
		"Only the target URL, virtual users, and configured duration were checked. Ruptiq cannot tell " +
			"whether the target's code, data, configuration, or the machines and network changed between runs.",
	}
	if redacted(b.TargetURL) || redacted(c.TargetURL) {
		out = append(out, "The target URL is stored redacted (user information and query string replaced by "+
			"xxxxx), so the runs may have used different credentials or query strings even though the "+
			"stored URLs match.")
	}
	if b.Runner != c.Runner || b.RunnerVersion != c.RunnerVersion {
		out = append(out, fmt.Sprintf("The runs used different load generators (baseline %s, candidate %s).",
			runnerName(b), runnerName(c)))
	}
	if c.StartedAt.Before(b.StartedAt) {
		out = append(out, "The candidate run started before the baseline run.")
	}
	if b.Metrics.FailureRate > 0 || c.Metrics.FailureRate > 0 {
		out = append(out, "At least one run had failed requests. Latency includes failed requests, which k6 "+
			"may time as 0 ms (for example refused connections), so latency changes are hard to interpret.")
	}
	if c.Metrics.RequestsPerSecond > b.Metrics.RequestsPerSecond && c.Metrics.FailureRate > b.Metrics.FailureRate {
		out = append(out, "Requests per second rose together with the failure rate; fast failures can raise "+
			"throughput without the target doing more useful work.")
	}
	if baseline.Verdict.MaxFailureRate != candidate.Verdict.MaxFailureRate {
		out = append(out, fmt.Sprintf("The verdicts used different maximum failure rates (baseline %.2f%%, candidate %.2f%%).",
			baseline.Verdict.MaxFailureRate*100, candidate.Verdict.MaxFailureRate*100))
	}
	for _, side := range []struct {
		name string
		run  Run
	}{{"baseline", baseline}, {"candidate", candidate}} {
		if !side.run.Verdict.Passed {
			reason := strings.Join(side.run.Verdict.Reasons, "; ")
			if reason == "" {
				reason = "no reason recorded"
			}
			out = append(out, fmt.Sprintf("The %s run completed but FAILED its pass criteria (%s).", side.name, reason))
		}
	}
	return out
}

func redacted(rawURL string) bool {
	u, err := url.Parse(rawURL)
	return err == nil && (u.User != nil || u.RawQuery != "")
}

func runnerName(r workload.Result) string {
	if r.RunnerVersion == "" {
		return r.Runner
	}
	return r.Runner + " " + r.RunnerVersion
}
