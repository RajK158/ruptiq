// Package comparison compares two saved ruptiq-run reports: a baseline run
// and a candidate run. It works offline on the saved measurements only; it
// never runs a workload or contacts a target.
//
// It describes differences. It does not decide whether a difference is
// statistically significant, and it does not diagnose causes.
package comparison

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"strings"
	"time"

	"github.com/RajK158/ruptiq/internal/workload"
)

// MaxReportBytes bounds the size of a report accepted by DecodeReport.
const MaxReportBytes = 1 << 20

var (
	// ErrInvalidReport means a report is malformed, incomplete, or
	// inconsistent.
	ErrInvalidReport = errors.New("invalid run report")
	// ErrUnsupportedSchema means a report has a schema version this code does
	// not understand.
	ErrUnsupportedSchema = errors.New("unsupported run report schema")
	// ErrIncompleteRun means a report records a run that did not complete, so
	// it has no measurements to compare.
	ErrIncompleteRun = errors.New("report records a run that did not complete")
)

// Run is a completed run read from a saved report.
type Run struct {
	Result  workload.Result
	Verdict workload.Verdict
}

// The raw types mirror workload.Report with pointers, so that a missing field
// or JSON null is an error instead of silently becoming zero.
type rawReport struct {
	SchemaVersion *int        `json:"schema_version"`
	Run           *rawResult  `json:"run"`
	Error         *string     `json:"error"`
	Verdict       *rawVerdict `json:"verdict"`
}

type rawResult struct {
	Runner                    *string     `json:"runner"`
	RunnerVersion             *string     `json:"runner_version"`
	TargetURL                 *string     `json:"target_url"`
	VUs                       *int        `json:"vus"`
	ConfiguredDurationSeconds *float64    `json:"configured_duration_seconds"`
	StartedAt                 *time.Time  `json:"started_at"`
	FinishedAt                *time.Time  `json:"finished_at"`
	Metrics                   *rawMetrics `json:"metrics"`
}

type rawMetrics struct {
	DurationSeconds   *float64    `json:"duration_seconds"`
	HTTPRequests      *int64      `json:"http_requests"`
	FailedRequests    *int64      `json:"failed_requests"`
	FailureRate       *float64    `json:"failure_rate"`
	RequestsPerSecond *float64    `json:"requests_per_second"`
	LatencyMs         *rawLatency `json:"latency_ms"`
}

type rawLatency struct {
	Avg *float64 `json:"avg"`
	Min *float64 `json:"min"`
	P50 *float64 `json:"p50"`
	P90 *float64 `json:"p90"`
	P95 *float64 `json:"p95"`
	Max *float64 `json:"max"`
}

type rawVerdict struct {
	Passed         *bool    `json:"passed"`
	MaxFailureRate *float64 `json:"max_failure_rate"`
	Reasons        []string `json:"reasons"`
}

// relTolerance is how far a stored derived value (failure rate, requests per
// second) may differ from the value recomputed from its inputs: enough for
// floating-point rounding, far too little to hide an edited number.
const relTolerance = 1e-9

// DecodeReport reads one saved ruptiq-run report (workload.Report, schema
// version 1) and checks that it describes a completed run with complete,
// finite, and internally consistent measurements. A completed run whose
// verdict is failed is accepted.
//
// Errors wrap ErrUnsupportedSchema, ErrIncompleteRun, or ErrInvalidReport.
func DecodeReport(data []byte) (Run, error) {
	if len(data) > MaxReportBytes {
		return Run{}, fmt.Errorf("%w: larger than %d bytes", ErrInvalidReport, MaxReportBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var raw rawReport
	if err := dec.Decode(&raw); err != nil {
		return Run{}, fmt.Errorf("%w: not valid report JSON: %v", ErrInvalidReport, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Run{}, fmt.Errorf("%w: unexpected data after the report", ErrInvalidReport)
	}

	if raw.SchemaVersion == nil {
		return Run{}, fmt.Errorf("%w: schema_version is missing", ErrInvalidReport)
	}
	if *raw.SchemaVersion != workload.ReportSchemaVersion {
		return Run{}, fmt.Errorf("%w: schema_version %d (supported: %d)", ErrUnsupportedSchema, *raw.SchemaVersion, workload.ReportSchemaVersion)
	}
	if raw.Error != nil && *raw.Error != "" {
		if raw.Run != nil {
			return Run{}, fmt.Errorf("%w: report has both a run and an error", ErrInvalidReport)
		}
		return Run{}, fmt.Errorf("%w: %s", ErrIncompleteRun, shorten(*raw.Error, 300))
	}
	if raw.Run == nil {
		return Run{}, fmt.Errorf("%w: run is missing", ErrInvalidReport)
	}
	if raw.Verdict == nil {
		return Run{}, fmt.Errorf("%w: verdict is missing", ErrInvalidReport)
	}

	v := validator{}
	run := Run{Result: v.result(raw.Run), Verdict: v.verdict(raw.Verdict)}
	if len(v.problems) == 0 {
		v.consistency(run)
	}
	if len(v.problems) > 0 {
		return Run{}, fmt.Errorf("%w: %s", ErrInvalidReport, strings.Join(v.problems, "; "))
	}
	return run, nil
}

// validator collects every problem in a report rather than stopping at the
// first.
type validator struct {
	problems []string
}

func (v *validator) fail(format string, args ...any) {
	v.problems = append(v.problems, fmt.Sprintf(format, args...))
}

func (v *validator) number(name string, p *float64) float64 {
	switch {
	case p == nil:
		v.fail("%s is missing", name)
	case math.IsNaN(*p) || math.IsInf(*p, 0):
		v.fail("%s is not a finite number", name)
	case *p < 0:
		v.fail("%s is negative", name)
	default:
		return *p
	}
	return 0
}

func (v *validator) result(r *rawResult) workload.Result {
	var res workload.Result
	if r.Runner == nil || strings.TrimSpace(*r.Runner) == "" {
		v.fail("run.runner is missing")
	} else {
		res.Runner = *r.Runner
	}
	if r.RunnerVersion != nil {
		res.RunnerVersion = *r.RunnerVersion
	}

	switch {
	case r.TargetURL == nil || *r.TargetURL == "":
		v.fail("run.target_url is missing")
	default:
		u, err := url.Parse(*r.TargetURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			v.fail("run.target_url is not an absolute http or https URL")
		} else {
			res.TargetURL = *r.TargetURL
		}
	}

	switch {
	case r.VUs == nil:
		v.fail("run.vus is missing")
	case *r.VUs < 1 || *r.VUs > workload.MaxVUs:
		v.fail("run.vus must be between 1 and %d, got %d", workload.MaxVUs, *r.VUs)
	default:
		res.VUs = *r.VUs
	}

	before := len(v.problems)
	d := v.number("run.configured_duration_seconds", r.ConfiguredDurationSeconds)
	if len(v.problems) == before &&
		(d < workload.MinDuration.Seconds() || d > workload.MaxDuration.Seconds() || d != math.Trunc(d)) {
		v.fail("run.configured_duration_seconds must be a whole number from %v to %v, got %v",
			workload.MinDuration.Seconds(), workload.MaxDuration.Seconds(), d)
	}
	res.ConfiguredDurationSeconds = d

	switch {
	case r.StartedAt == nil || r.StartedAt.IsZero():
		v.fail("run.started_at is missing")
	case r.FinishedAt == nil || r.FinishedAt.IsZero():
		v.fail("run.finished_at is missing")
	case r.FinishedAt.Before(*r.StartedAt):
		v.fail("run.finished_at is before run.started_at")
	default:
		res.StartedAt, res.FinishedAt = *r.StartedAt, *r.FinishedAt
	}

	if r.Metrics == nil {
		v.fail("run.metrics is missing")
		return res
	}
	res.Metrics = v.metrics(r.Metrics)
	return res
}

func (v *validator) metrics(m *rawMetrics) workload.Metrics {
	var out workload.Metrics
	before := len(v.problems)
	out.DurationSeconds = v.number("metrics.duration_seconds", m.DurationSeconds)
	if len(v.problems) == before && out.DurationSeconds == 0 {
		v.fail("metrics.duration_seconds must be greater than 0")
	}
	switch {
	case m.HTTPRequests == nil:
		v.fail("metrics.http_requests is missing")
	case *m.HTTPRequests < 1:
		v.fail("metrics.http_requests must be at least 1, got %d", *m.HTTPRequests)
	default:
		out.HTTPRequests = *m.HTTPRequests
	}
	switch {
	case m.FailedRequests == nil:
		v.fail("metrics.failed_requests is missing")
	case *m.FailedRequests < 0:
		v.fail("metrics.failed_requests is negative")
	default:
		out.FailedRequests = *m.FailedRequests
	}
	out.FailureRate = v.number("metrics.failure_rate", m.FailureRate)
	if out.FailureRate > 1 {
		v.fail("metrics.failure_rate must be at most 1, got %v", out.FailureRate)
	}
	out.RequestsPerSecond = v.number("metrics.requests_per_second", m.RequestsPerSecond)

	if m.LatencyMs == nil {
		v.fail("metrics.latency_ms is missing")
		return out
	}
	l := m.LatencyMs
	out.LatencyMs = workload.Latency{
		Avg: v.number("metrics.latency_ms.avg", l.Avg),
		Min: v.number("metrics.latency_ms.min", l.Min),
		P50: v.number("metrics.latency_ms.p50", l.P50),
		P90: v.number("metrics.latency_ms.p90", l.P90),
		P95: v.number("metrics.latency_ms.p95", l.P95),
		Max: v.number("metrics.latency_ms.max", l.Max),
	}
	return out
}

func (v *validator) verdict(r *rawVerdict) workload.Verdict {
	var out workload.Verdict
	if r.Passed == nil {
		v.fail("verdict.passed is missing")
	} else {
		out.Passed = *r.Passed
	}
	if r.MaxFailureRate == nil {
		v.fail("verdict.max_failure_rate is missing")
	} else if err := workload.ValidateMaxFailureRate(*r.MaxFailureRate); err != nil {
		v.fail("verdict.max_failure_rate must be between 0 and 1")
	} else {
		out.MaxFailureRate = *r.MaxFailureRate
	}
	out.Reasons = r.Reasons
	return out
}

// consistency checks relationships between values that are each valid alone.
func (v *validator) consistency(run Run) {
	m := run.Result.Metrics
	if m.FailedRequests > m.HTTPRequests {
		v.fail("metrics.failed_requests (%d) exceeds metrics.http_requests (%d)", m.FailedRequests, m.HTTPRequests)
	} else if want := float64(m.FailedRequests) / float64(m.HTTPRequests); !near(m.FailureRate, want) {
		v.fail("metrics.failure_rate %v does not match failed_requests / http_requests (%v)", m.FailureRate, want)
	}
	if want := float64(m.HTTPRequests) / m.DurationSeconds; !near(m.RequestsPerSecond, want) {
		v.fail("metrics.requests_per_second %v does not match http_requests / duration_seconds (%v)", m.RequestsPerSecond, want)
	}

	l := m.LatencyMs
	if !(l.Min <= l.P50 && l.P50 <= l.P90 && l.P90 <= l.P95 && l.P95 <= l.Max) {
		v.fail("metrics.latency_ms values are out of order (expected min <= p50 <= p90 <= p95 <= max)")
	}
	if l.Avg < l.Min || l.Avg > l.Max {
		v.fail("metrics.latency_ms.avg is outside min..max")
	}

	if want := workload.Evaluate(m, run.Verdict.MaxFailureRate); want.Passed != run.Verdict.Passed {
		v.fail("verdict.passed is %v, but the metrics and max_failure_rate give %v", run.Verdict.Passed, want.Passed)
	}
}

func near(got, want float64) bool {
	return math.Abs(got-want) <= relTolerance*math.Max(1, math.Abs(want))
}

func shorten(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
