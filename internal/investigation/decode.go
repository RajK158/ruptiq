package investigation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/RajK158/ruptiq/internal/analysis"
	"github.com/RajK158/ruptiq/internal/experiment"
	"github.com/RajK158/ruptiq/internal/workload"
)

var (
	// ErrInvalidAnalysis: the input is not a well-formed, internally
	// consistent analysis file.
	ErrInvalidAnalysis = errors.New("invalid analysis file")
	// ErrUnsupportedSchema: the analysis file has another schema version.
	ErrUnsupportedSchema = errors.New("unsupported analysis schema")
	// ErrNotInvestigable: the analysis file records a failed analysis.
	ErrNotInvestigable = errors.New("analysis cannot be investigated")
)

// maxProblems is the most validation problems listed in an error.
const maxProblems = 10

// relTolerance is the relative difference allowed between a stored value and
// the value recalculated from its parts.
const relTolerance = 1e-9

// ReadAnalysis reads at most maxBytes from r and decodes them with
// DecodeAnalysis.
func ReadAnalysis(r io.Reader, maxBytes int64) (analysis.Analysis, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return analysis.Analysis{}, err
	}
	if int64(len(data)) > maxBytes {
		return analysis.Analysis{}, fmt.Errorf("%w: larger than %d bytes", ErrInvalidAnalysis, maxBytes)
	}
	return DecodeAnalysis(data)
}

// DecodeAnalysis decodes an analysis file written by ruptiq-analyze, rejecting
// unknown fields, other schema versions, recorded failures, and values that
// contradict each other. Errors wrap ErrInvalidAnalysis, ErrUnsupportedSchema,
// or ErrNotInvestigable.
func DecodeAnalysis(data []byte) (analysis.Analysis, error) {
	var head struct {
		SchemaVersion *json.RawMessage `json:"schema_version"`
	}
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(&head); err != nil {
		return analysis.Analysis{}, fmt.Errorf("%w: not valid JSON: %s", ErrInvalidAnalysis, jsonProblem(err))
	}
	if head.SchemaVersion == nil || string(*head.SchemaVersion) == "null" {
		return analysis.Analysis{}, fmt.Errorf("%w: schema_version is missing", ErrInvalidAnalysis)
	}
	var version int
	if err := json.Unmarshal(*head.SchemaVersion, &version); err != nil {
		return analysis.Analysis{}, fmt.Errorf("%w: schema_version is not an integer", ErrInvalidAnalysis)
	}
	if version != analysis.SchemaVersion {
		return analysis.Analysis{}, fmt.Errorf("%w: schema_version %d (supported: %d)", ErrUnsupportedSchema, version, analysis.SchemaVersion)
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var r analysis.Report
	if err := dec.Decode(&r); err != nil {
		return analysis.Analysis{}, fmt.Errorf("%w: not valid JSON: %s", ErrInvalidAnalysis, jsonProblem(err))
	}
	if _, err := dec.Token(); err != io.EOF {
		return analysis.Analysis{}, fmt.Errorf("%w: unexpected data after the analysis", ErrInvalidAnalysis)
	}
	switch {
	case r.Analysis == nil && r.Error != "":
		return analysis.Analysis{}, fmt.Errorf("%w: the file records a failed analysis: %s", ErrNotInvestigable, shorten(r.Error, 300))
	case r.Analysis == nil:
		return analysis.Analysis{}, fmt.Errorf("%w: analysis is missing", ErrInvalidAnalysis)
	case r.Error != "":
		return analysis.Analysis{}, fmt.Errorf("%w: both analysis and error are set", ErrInvalidAnalysis)
	}
	v := &validator{}
	v.analysis(*r.Analysis)
	if err := v.err(); err != nil {
		return analysis.Analysis{}, err
	}
	return *r.Analysis, nil
}

// jsonProblem describes a decoding error without quoting values: syntax,
// type, and unknown-field errors name only a character, a JSON kind, or a
// key, while others (such as time.Time's) quote the rejected value.
func jsonProblem(err error) string {
	var syntax *json.SyntaxError
	var typ *json.UnmarshalTypeError
	if errors.As(err, &syntax) || errors.As(err, &typ) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) ||
		strings.HasPrefix(err.Error(), "json: unknown field ") {
		return workload.RedactText(err.Error(), "")
	}
	return "a timestamp, number, or other value has an invalid format"
}

// shorten redacts URL credentials in s before truncating it, so a cut cannot
// separate user information from the "@" that identifies it.
func shorten(s string, n int) string {
	s = workload.RedactText(s, "")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

type validator struct {
	problems []string
}

func (v *validator) fail(format string, args ...any) {
	v.problems = append(v.problems, fmt.Sprintf(format, args...))
}

func (v *validator) err() error {
	if len(v.problems) == 0 {
		return nil
	}
	shown := v.problems
	more := ""
	if len(shown) > maxProblems {
		more = fmt.Sprintf("; and %d more problems", len(shown)-maxProblems)
		shown = shown[:maxProblems]
	}
	return fmt.Errorf("%w: %s%s", ErrInvalidAnalysis, strings.Join(shown, "; "), more)
}

func finite(xs ...float64) bool {
	for _, x := range xs {
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return false
		}
	}
	return true
}

func approx(a, b float64) bool {
	return math.Abs(a-b) <= relTolerance*math.Max(1, math.Max(math.Abs(a), math.Abs(b)))
}

func (v *validator) analysis(a analysis.Analysis) {
	switch a.Experiment.Status {
	case experiment.StatusPassed, experiment.StatusFailed, experiment.StatusIncomplete:
	default:
		v.fail("experiment.status %q is not an analysed status", shorten(string(a.Experiment.Status), 40))
	}
	if a.Experiment.SchemaVersion != experiment.ReportSchemaVersion {
		v.fail("experiment.schema_version is %d, want %d", a.Experiment.SchemaVersion, experiment.ReportSchemaVersion)
	}
	if err := a.Config.Validate(); err != nil {
		v.fail("config: %s", shorten(err.Error(), 300))
	}
	if a.Windows == nil {
		v.fail("windows is missing")
	}
	if a.Totals == nil {
		v.fail("totals is missing")
	}
	if a.Warnings == nil {
		v.fail("warnings is missing")
	}
	if a.Limitations == nil {
		v.fail("limitations is missing")
	}
	for i, w := range a.Windows {
		v.window(a.Config, i, w, a.Windows)
	}
	for i, rs := range a.Totals {
		v.route(a.Config, fmt.Sprintf("totals[%d]", i), rs, 0)
	}
	v.uniqueRoutes("totals", a.Totals)
	for i, w := range a.Warnings {
		if w.Kind == "" || w.Message == "" {
			v.fail("warnings[%d]: kind and message are required", i)
		}
		if w.Window < 0 || w.Window > len(a.Windows) {
			v.fail("warnings[%d]: window %d does not exist", i, w.Window)
		}
	}
}

func knownPhase(p experiment.Phase) bool {
	switch p {
	case experiment.BeforeWorkload, experiment.DuringWorkload, experiment.AfterWorkload:
		return true
	}
	return false
}

func (v *validator) window(cfg analysis.Config, i int, w analysis.Window, all []analysis.Window) {
	name := fmt.Sprintf("windows[%d]", i)
	if w.Index != i+1 {
		v.fail("%s: index is %d, want %d", name, w.Index, i+1)
	}
	if !knownPhase(w.From.Phase) || !knownPhase(w.To.Phase) {
		v.fail("%s: unknown sample phase", name)
	}
	if w.From.Sequence < 1 || w.To.Sequence <= w.From.Sequence {
		v.fail("%s: sample sequences %d to %d are not increasing", name, w.From.Sequence, w.To.Sequence)
	}
	if i > 0 && all[i-1].To.Sequence != w.From.Sequence {
		v.fail("%s: does not start at the sample the previous window ends at", name)
	}
	if !finite(w.ElapsedSeconds) || w.ElapsedSeconds <= 0 {
		v.fail("%s: elapsed_seconds must be positive", name)
	} else if !approx(w.ElapsedSeconds, w.To.ScrapedAt.Sub(w.From.ScrapedAt).Seconds()) {
		v.fail("%s: elapsed_seconds %v does not match its samples' timestamps", name, w.ElapsedSeconds)
	}
	if w.MissedScrapes == nil || w.Routes == nil {
		v.fail("%s: missed_scrapes and routes are required", name)
	}
	for j, m := range w.MissedScrapes {
		if m.Sequence <= w.From.Sequence || m.Sequence >= w.To.Sequence {
			v.fail("%s.missed_scrapes[%d]: sequence %d is outside the window", name, j, m.Sequence)
		}
	}
	for j, rs := range w.Routes {
		v.route(cfg, fmt.Sprintf("%s.routes[%d]", name, j), rs, w.ElapsedSeconds)
	}
	v.uniqueRoutes(name, w.Routes)
}

func (v *validator) uniqueRoutes(name string, routes []analysis.RouteSignals) {
	seen := map[[2]string]bool{}
	for _, rs := range routes {
		k := [2]string{rs.Method, rs.Route}
		if seen[k] {
			v.fail("%s: route %s %s appears more than once", name, shorten(rs.Method, 20), shorten(rs.Route, 80))
		}
		seen[k] = true
	}
}

// route checks one route's signals. elapsed is the window's length, or 0 for
// totals.
func (v *validator) route(cfg analysis.Config, name string, rs analysis.RouteSignals, elapsed float64) {
	if (rs.Requests == nil) == (rs.RequestsUnavailable == "") {
		v.fail("%s: exactly one of requests and requests_unavailable must be set", name)
	}
	if (rs.Latency == nil) == (rs.LatencyUnavailable == "") {
		v.fail("%s: exactly one of latency and latency_unavailable must be set", name)
	}
	if r := rs.Requests; r != nil {
		v.requests(cfg, name+".requests", r, elapsed)
	}
	if l := rs.Latency; l != nil {
		v.latency(cfg, name+".latency", l)
	}
}

func (v *validator) requests(cfg analysis.Config, name string, r *analysis.RequestSignals, elapsed float64) {
	if !finite(r.Requests, r.ElapsedSeconds, r.PerSecond, r.ServerErrors, r.ClientErrors, r.Unclassified) ||
		r.Requests < 0 || r.ServerErrors < 0 || r.ClientErrors < 0 || r.Unclassified < 0 || r.ElapsedSeconds <= 0 {
		v.fail("%s: counts must be finite and non-negative, and elapsed_seconds positive", name)
		return
	}
	if elapsed > 0 && !approx(r.ElapsedSeconds, elapsed) {
		v.fail("%s: elapsed_seconds %v differs from its window's %v", name, r.ElapsedSeconds, elapsed)
	}
	if !approx(r.PerSecond, r.Requests/r.ElapsedSeconds) {
		v.fail("%s: per_second %v is not requests / elapsed_seconds", name, r.PerSecond)
	}
	if r.ByStatus == nil {
		v.fail("%s: by_status is missing", name)
	}
	sum := 0.0
	for _, s := range r.ByStatus {
		if !finite(s.Requests) || s.Requests < 0 {
			v.fail("%s: by_status counts must be finite and non-negative", name)
			return
		}
		sum += s.Requests
	}
	if !approx(sum, r.Requests) {
		v.fail("%s: by_status adds up to %v, not %v requests", name, sum, r.Requests)
	}
	if r.ServerErrors+r.ClientErrors+r.Unclassified > r.Requests*(1+relTolerance)+relTolerance {
		v.fail("%s: 5xx, 4xx, and unclassified requests exceed the requests", name)
	}
	if (r.ServerErrorRatio == nil) == (r.ServerErrorRatioUnavailable == "") {
		v.fail("%s: exactly one of server_error_ratio and server_error_ratio_unavailable must be set", name)
	}
	if p := r.ServerErrorRatio; p != nil {
		if r.Requests == 0 || !finite(*p) || *p < 0 || *p > 1 || !approx(*p, r.ServerErrors/r.Requests) {
			v.fail("%s: server_error_ratio %v is not server_errors_5xx / requests", name, *p)
		}
	}
	if r.Evidence.Metric != cfg.RequestsCounter {
		v.fail("%s: evidence metric is not the configured requests counter", name)
	}
}

func (v *validator) latency(cfg analysis.Config, name string, l *analysis.LatencySignals) {
	if (l.MeanSeconds == nil) == (l.MeanSecondsUnavailable == "") {
		v.fail("%s: exactly one of mean_seconds and mean_seconds_unavailable must be set", name)
	}
	if m := l.MeanSeconds; m != nil && (!finite(*m) || *m < 0) {
		v.fail("%s: mean_seconds must be finite and non-negative", name)
	}
	prev := uint64(0)
	for i, b := range l.Buckets {
		x := float64(b.UpperBound)
		if math.IsNaN(x) || (math.IsInf(x, 1) && i != len(l.Buckets)-1) || b.CumulativeCount < prev || b.CumulativeCount > l.Observations {
			v.fail("%s: buckets are not cumulative, ordered, and within the observations", name)
			break
		}
		if i > 0 && x <= float64(l.Buckets[i-1].UpperBound) {
			v.fail("%s: bucket bounds are not increasing", name)
			break
		}
		prev = b.CumulativeCount
	}
	p := l.P95
	if p.Quantile != analysis.Quantile || !p.Estimate {
		v.fail("%s: p95 must be the %g quantile, marked as an estimate", name, analysis.Quantile)
	}
	if (p.Seconds == nil) == (p.Unavailable == "") {
		v.fail("%s: exactly one of p95 seconds and unavailable must be set", name)
	}
	if p.Seconds != nil {
		if p.BucketLowerSeconds == nil || p.BucketUpperSeconds == nil {
			v.fail("%s: a p95 estimate needs its bucket bounds", name)
		} else if s, lo, hi := *p.Seconds, *p.BucketLowerSeconds, *p.BucketUpperSeconds; !finite(s, lo, hi) || lo > s || s > hi || s < 0 {
			v.fail("%s: p95 %v is not finite, non-negative, and within its bucket [%v, %v]", name, s, lo, hi)
		}
		if l.Observations < uint64(cfg.MinObservations) {
			v.fail("%s: p95 is estimated from %d observations, fewer than min_observations %d", name, l.Observations, cfg.MinObservations)
		}
	}
	if l.Evidence.Metric != cfg.DurationHistogram {
		v.fail("%s: evidence metric is not the configured duration histogram", name)
	}
}
