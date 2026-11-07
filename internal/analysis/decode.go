package analysis

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

	"github.com/RajK158/ruptiq/internal/experiment"
	"github.com/RajK158/ruptiq/internal/telemetry"
	"github.com/RajK158/ruptiq/internal/workload"
)

// Input size bounds. An experiment report with the default retention budget
// (32 MiB of compact sample data) is about 80 MiB as written, indented.
const (
	DefaultMaxInputBytes = 128 << 20
	MinInputBytes        = 1 << 10
	MaxInputBytesLimit   = 1 << 30
)

var (
	// ErrInvalidReport means a report is malformed, incomplete, or
	// inconsistent, so it cannot be interpreted safely.
	ErrInvalidReport = errors.New("invalid experiment report")
	// ErrUnsupportedSchema means a report has a schema version this code does
	// not understand.
	ErrUnsupportedSchema = errors.New("unsupported experiment report schema")
	// ErrNotAnalyzable means a valid report records an experiment whose
	// workload did not complete (status error, cancelled, or timed_out).
	ErrNotAnalyzable = errors.New("experiment cannot be analysed")
)

// timeTolerance is how far a recorded offset may differ from the offset
// recalculated from the timestamps: enough for rounding, far too little to
// hide a clock change or an edited timestamp.
const timeTolerance = 5 * time.Millisecond

// maxListedProblems bounds the problems quoted in one error.
const maxListedProblems = 10

// ReadExperiment reads at most maxBytes from r and decodes them with
// DecodeExperiment.
func ReadExperiment(r io.Reader, maxBytes int64) (experiment.Report, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return experiment.Report{}, err
	}
	if int64(len(data)) > maxBytes {
		return experiment.Report{}, fmt.Errorf("%w: larger than %d bytes", ErrInvalidReport, maxBytes)
	}
	return DecodeExperiment(data)
}

// DecodeExperiment decodes a saved experiment report (schema version 1) and
// checks that it is complete, ordered, and internally consistent. Reports
// with status passed, failed, or incomplete are accepted; others wrap
// ErrNotAnalyzable.
//
// Errors wrap ErrUnsupportedSchema, ErrNotAnalyzable, or ErrInvalidReport.
func DecodeExperiment(data []byte) (experiment.Report, error) {
	var head struct {
		SchemaVersion *json.RawMessage `json:"schema_version"`
	}
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(&head); err != nil {
		return experiment.Report{}, fmt.Errorf("%w: not valid report JSON: %s", ErrInvalidReport, jsonProblem(err))
	}
	if head.SchemaVersion == nil || string(*head.SchemaVersion) == "null" {
		return experiment.Report{}, fmt.Errorf("%w: schema_version is missing", ErrInvalidReport)
	}
	var version int
	if err := json.Unmarshal(*head.SchemaVersion, &version); err != nil {
		return experiment.Report{}, fmt.Errorf("%w: schema_version is not an integer", ErrInvalidReport)
	}
	if version != experiment.ReportSchemaVersion {
		return experiment.Report{}, fmt.Errorf("%w: schema_version %d (supported: %d)", ErrUnsupportedSchema, version, experiment.ReportSchemaVersion)
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var r experiment.Report
	if err := dec.Decode(&r); err != nil {
		return experiment.Report{}, fmt.Errorf("%w: not valid report JSON: %s", ErrInvalidReport, jsonProblem(err))
	}
	if _, err := dec.Token(); err != io.EOF {
		return experiment.Report{}, fmt.Errorf("%w: unexpected data after the report", ErrInvalidReport)
	}

	v := &validator{}
	var p pReport
	if err := json.Unmarshal(data, &p); err != nil {
		return experiment.Report{}, fmt.Errorf("%w: not valid report JSON: %s", ErrInvalidReport, jsonProblem(err))
	}
	p.check(v)
	if err := v.err(); err != nil {
		return experiment.Report{}, err
	}

	switch r.Status {
	case experiment.StatusPassed, experiment.StatusFailed, experiment.StatusIncomplete:
	case experiment.StatusError, experiment.StatusCancelled, experiment.StatusTimedOut:
		return experiment.Report{}, fmt.Errorf("%w: its status is %q; only passed, failed, and incomplete experiments, whose workload completed, are analysed",
			ErrNotAnalyzable, r.Status)
	default:
		return experiment.Report{}, fmt.Errorf("%w: status %q is not a known experiment status", ErrInvalidReport, shorten(string(r.Status), 40))
	}

	v.report(r)
	if err := v.err(); err != nil {
		return experiment.Report{}, err
	}
	return r, nil
}

// validator collects every problem in a report rather than stopping at the
// first.
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
	if len(shown) > maxListedProblems {
		more = fmt.Sprintf("; and %d more problems", len(shown)-maxListedProblems)
		shown = shown[:maxListedProblems]
	}
	return fmt.Errorf("%w: %s%s", ErrInvalidReport, strings.Join(shown, "; "), more)
}

func (v *validator) report(r experiment.Report) {
	if r.StartedAt.IsZero() || r.FinishedAt.IsZero() {
		v.fail("started_at and finished_at must be set")
		return
	}
	if r.FinishedAt.Before(r.StartedAt) {
		v.fail("finished_at is before started_at")
		return
	}

	c := r.Config
	v.httpURL("config.target_url", c.TargetURL)
	v.httpURL("config.metrics_url", c.MetricsURL)
	if c.VUs < 1 {
		v.fail("config.vus must be at least 1, got %d", c.VUs)
	}
	v.positive("config.duration_seconds", c.DurationSeconds)
	v.positive("config.sample_interval_seconds", c.SampleIntervalSeconds)

	v.workload(r)
	v.telemetry(r)
}

func (v *validator) httpURL(name, raw string) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		v.fail("%s is not an absolute http or https URL", name)
	}
}

func (v *validator) positive(name string, x float64) {
	if math.IsNaN(x) || math.IsInf(x, 0) || x <= 0 {
		v.fail("%s must be a positive number, got %v", name, x)
	}
}

func (v *validator) nonNegative(name string, x float64) {
	if math.IsNaN(x) || math.IsInf(x, 0) || x < 0 {
		v.fail("%s must be a finite, non-negative number, got %v", name, x)
	}
}

func (v *validator) workload(r experiment.Report) {
	w := r.Workload
	if w.Error != "" {
		v.fail("workload.error is set, but status %q means the workload completed", r.Status)
	}
	if w.Result == nil {
		v.fail("workload.result is missing, but status %q means the workload completed", r.Status)
		return
	}
	m := w.Result.Metrics
	v.nonNegative("workload.result.metrics.duration_seconds", m.DurationSeconds)
	v.nonNegative("workload.result.metrics.failure_rate", m.FailureRate)
	v.nonNegative("workload.result.metrics.requests_per_second", m.RequestsPerSecond)
	for _, l := range []struct {
		name string
		x    float64
	}{{"avg", m.LatencyMs.Avg}, {"min", m.LatencyMs.Min}, {"p50", m.LatencyMs.P50}, {"p90", m.LatencyMs.P90}, {"p95", m.LatencyMs.P95}, {"max", m.LatencyMs.Max}} {
		v.nonNegative("workload.result.metrics.latency_ms."+l.name, l.x)
	}
	if m.HTTPRequests < 0 || m.FailedRequests < 0 || m.FailedRequests > m.HTTPRequests {
		v.fail("workload.result.metrics: failed_requests (%d) must be between 0 and http_requests (%d)", m.FailedRequests, m.HTTPRequests)
	}
	if m.FailureRate > 1 {
		v.fail("workload.result.metrics.failure_rate must be at most 1, got %v", m.FailureRate)
	}

	passed, complete := w.Verdict.Passed, r.Telemetry.Completeness == experiment.Complete
	switch r.Status {
	case experiment.StatusPassed:
		if !passed || !complete {
			v.fail("status passed requires a passed workload verdict and complete telemetry")
		}
	case experiment.StatusFailed:
		if passed {
			v.fail("status failed contradicts the passed workload verdict")
		}
	case experiment.StatusIncomplete:
		if !passed || complete {
			v.fail("status incomplete requires a passed workload verdict and telemetry that is not complete")
		}
	}
}

// record is one scrape attempt, from any of the three telemetry lists.
type record struct {
	sequence int
	phase    experiment.Phase
	at       time.Time
	offset   float64
	list     string
}

func (v *validator) telemetry(r experiment.Report) {
	t := r.Telemetry
	switch t.Completeness {
	case experiment.Complete, experiment.Partial, experiment.None:
	default:
		v.fail("telemetry.completeness %q is not a known value", shorten(string(t.Completeness), 40))
	}
	if t.Failed != len(t.Errors) {
		v.fail("telemetry.failed is %d, but there are %d errors", t.Failed, len(t.Errors))
	}
	if t.Succeeded != len(t.Samples)+len(t.NotRetained) {
		v.fail("telemetry.succeeded is %d, but there are %d samples and %d not retained", t.Succeeded, len(t.Samples), len(t.NotRetained))
	}
	if t.Attempts != t.Succeeded+t.Failed {
		v.fail("telemetry.attempts is %d, but succeeded + failed is %d", t.Attempts, t.Succeeded+t.Failed)
	}
	if t.RetentionLimitReached != (len(t.NotRetained) > 0) {
		v.fail("telemetry.retention_limit_reached is %v, but %d samples were not retained", t.RetentionLimitReached, len(t.NotRetained))
	}
	if t.RetainedBytes < 0 {
		v.fail("telemetry.retained_bytes is negative")
	}

	var records []record
	add := func(list string, i, seq int, phase experiment.Phase, at time.Time, offset float64) {
		name := fmt.Sprintf("telemetry.%s[%d]", list, i)
		if i > 0 && seq <= records[len(records)-1].sequence {
			v.fail("%s: sequence %d is not after the previous entry's", name, seq)
		}
		switch phase {
		case experiment.BeforeWorkload, experiment.DuringWorkload, experiment.AfterWorkload:
		default:
			v.fail("%s: phase %q is not a known phase", name, shorten(string(phase), 40))
		}
		if at.IsZero() {
			v.fail("%s: timestamp is missing", name)
		} else if want := at.Sub(r.StartedAt).Seconds(); math.IsNaN(offset) || math.Abs(offset-want) > timeTolerance.Seconds() {
			v.fail("%s: offset_seconds %v does not match its timestamp (%v after started_at)", name, offset, want)
		} else if at.Before(r.StartedAt.Add(-timeTolerance)) || at.After(r.FinishedAt.Add(timeTolerance)) {
			v.fail("%s: timestamp is outside the experiment (started_at to finished_at)", name)
		}
		records = append(records, record{sequence: seq, phase: phase, at: at, offset: offset, list: list})
	}
	for i, s := range t.Samples {
		add("samples", i, s.Sequence, s.Phase, s.ScrapedAt, s.OffsetSeconds)
		v.sample(fmt.Sprintf("telemetry.samples[%d]", i), s)
	}
	samples := records
	records = nil
	for i, e := range t.Errors {
		add("errors", i, e.Sequence, e.Phase, e.AttemptedAt, e.OffsetSeconds)
	}
	errs := records
	records = nil
	for i, n := range t.NotRetained {
		add("not_retained", i, n.Sequence, n.Phase, n.ScrapedAt, n.OffsetSeconds)
		if n.SizeBytes < 0 {
			v.fail("telemetry.not_retained[%d]: size_bytes is negative", i)
		}
	}
	all := append(append(samples, errs...), records...)
	if len(v.problems) > 0 {
		return
	}

	// Every attempt has one sequence number, 1 to attempts, in order of time,
	// with the scrape before the workload first and the one after it last.
	bySeq := make([]*record, t.Attempts+1)
	for i := range all {
		rec := &all[i]
		if rec.sequence < 1 || rec.sequence > t.Attempts {
			v.fail("telemetry.%s: sequence %d is outside 1..%d", rec.list, rec.sequence, t.Attempts)
			continue
		}
		if bySeq[rec.sequence] != nil {
			v.fail("telemetry: sequence %d appears more than once", rec.sequence)
			continue
		}
		bySeq[rec.sequence] = rec
	}
	if len(v.problems) > 0 {
		return
	}
	var prev *record
	for seq := 1; seq <= t.Attempts; seq++ {
		rec := bySeq[seq]
		if rec == nil {
			v.fail("telemetry: sequence %d is missing", seq)
			continue
		}
		if rec.phase == experiment.BeforeWorkload && seq != 1 {
			v.fail("telemetry: sequence %d is before the workload but is not the first scrape", seq)
		}
		if rec.phase == experiment.AfterWorkload && seq != t.Attempts {
			v.fail("telemetry: sequence %d is after the workload but is not the last scrape", seq)
		}
		if prev != nil && rec.at.Before(prev.at) {
			v.fail("telemetry: sequence %d is earlier than sequence %d", seq, prev.sequence)
		}
		prev = rec
	}
	for i := 1; i < len(samples); i++ {
		if !samples[i].at.After(samples[i-1].at) {
			v.fail("telemetry: samples %d and %d have the same or decreasing timestamps", samples[i-1].sequence, samples[i].sequence)
		}
	}

	has := map[experiment.Phase]bool{}
	for _, s := range samples {
		has[s.phase] = true
	}
	want := experiment.Partial
	switch {
	case len(samples) == 0:
		want = experiment.None
	case t.Failed == 0 && len(t.NotRetained) == 0 && !t.SampleLimitReached &&
		has[experiment.BeforeWorkload] && has[experiment.DuringWorkload] && has[experiment.AfterWorkload]:
		want = experiment.Complete
	}
	if t.Completeness != want {
		v.fail("telemetry.completeness is %q, but the samples and errors make it %q", t.Completeness, want)
	}
}

func (v *validator) sample(name string, s experiment.Sample) {
	seen := make(map[string]bool, len(s.Families))
	for i, f := range s.Families {
		fname := fmt.Sprintf("%s.families[%d]", name, i)
		if f.Name == "" {
			v.fail("%s: name is empty", fname)
		} else if seen[f.Name] {
			v.fail("%s: family %q appears more than once", fname, shorten(f.Name, 80))
		}
		seen[f.Name] = true
		v.family(fname, f)
	}
}

func (v *validator) family(name string, f telemetry.Family) {
	switch f.Type {
	case telemetry.Counter, telemetry.Gauge, telemetry.Histogram, telemetry.Summary, telemetry.Untyped:
	default:
		v.fail("%s: type %q is not a known metric type", name, shorten(string(f.Type), 40))
		return
	}
	seen := make(map[string]bool, len(f.Series))
	for i, s := range f.Series {
		sname := fmt.Sprintf("%s.series[%d]", name, i)
		key := labelKey(s.Labels)
		if seen[key] {
			v.fail("%s: the same labels appear more than once", sname)
		}
		seen[key] = true

		wantValue, wantHist, wantSummary := false, false, false
		switch f.Type {
		case telemetry.Histogram:
			wantHist = true
		case telemetry.Summary:
			wantSummary = true
		default:
			wantValue = true
		}
		if (s.Value != nil) != wantValue || (s.Histogram != nil) != wantHist || (s.Summary != nil) != wantSummary {
			v.fail("%s: a %s series must have exactly one %s", sname, f.Type, valueField(f.Type))
			continue
		}
		switch {
		case f.Type == telemetry.Counter:
			if x := float64(*s.Value); math.IsNaN(x) || math.IsInf(x, 0) || x < 0 {
				v.fail("%s: counter value must be finite and non-negative, got %v", sname, x)
			}
		case wantHist:
			if err := checkHistogram(s.Histogram); err != nil {
				v.fail("%s: %v", sname, err)
			}
		}
	}
}

func valueField(t telemetry.MetricType) string {
	switch t {
	case telemetry.Histogram:
		return "histogram"
	case telemetry.Summary:
		return "summary"
	}
	return "value"
}

// checkHistogram applies the invariants of a cumulative histogram: bounds
// strictly increasing and not NaN, counts never decreasing and never above
// the total, and a +Inf bucket equal to the total.
func checkHistogram(h *telemetry.HistogramValue) error {
	for i, b := range h.Buckets {
		ub := float64(b.UpperBound)
		if math.IsNaN(ub) {
			return errors.New("histogram bucket upper bound is NaN")
		}
		if i > 0 {
			prev := h.Buckets[i-1]
			if !(ub > float64(prev.UpperBound)) {
				return errors.New("histogram bucket upper bounds are not strictly increasing")
			}
			if b.CumulativeCount < prev.CumulativeCount {
				return errors.New("histogram bucket counts decrease")
			}
		}
		if b.CumulativeCount > h.Count {
			return fmt.Errorf("histogram bucket le=%v has %d observations, more than the count %d", ub, b.CumulativeCount, h.Count)
		}
		if math.IsInf(ub, 1) && b.CumulativeCount != h.Count {
			return fmt.Errorf("histogram +Inf bucket has %d observations but count is %d", b.CumulativeCount, h.Count)
		}
	}
	return nil
}

// jsonProblem describes a decoding error without quoting report values.
// Syntax, type, and unknown-field errors name only a character, a JSON kind,
// or a key; other errors, such as those of time.Time and telemetry.Float,
// quote the rejected value, which may hold a credential.
func jsonProblem(err error) string {
	var syntax *json.SyntaxError
	var typ *json.UnmarshalTypeError
	if errors.As(err, &syntax) || errors.As(err, &typ) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) ||
		strings.HasPrefix(err.Error(), "json: unknown field ") {
		return workload.RedactText(err.Error(), "")
	}
	return "a timestamp, number, or other value has an invalid format"
}

// shorten redacts URL credentials in a report value before truncating it, so
// a cut cannot separate user information from the "@" that identifies it.
func shorten(s string, n int) string {
	s = workload.RedactText(s, "")
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// present records whether a JSON field was given and not null. The strict
// decode into experiment.Report cannot tell a missing field from a zero one.
type present bool

func (p *present) UnmarshalJSON(b []byte) error {
	*p = present(string(b) != "null")
	return nil
}

type pReport struct {
	SchemaVersion present     `json:"schema_version"`
	Status        present     `json:"status"`
	StartedAt     present     `json:"started_at"`
	FinishedAt    present     `json:"finished_at"`
	Config        *pConfig    `json:"config"`
	Workload      *pWorkload  `json:"workload"`
	Telemetry     *pTelemetry `json:"telemetry"`
}

type pConfig struct {
	TargetURL             present `json:"target_url"`
	VUs                   present `json:"vus"`
	DurationSeconds       present `json:"duration_seconds"`
	MetricsURL            present `json:"metrics_url"`
	SampleIntervalSeconds present `json:"sample_interval_seconds"`
}

type pWorkload struct {
	Result  *pResult `json:"result"`
	Verdict *struct {
		Passed present `json:"passed"`
	} `json:"verdict"`
}

type pResult struct {
	Runner  present `json:"runner"`
	Metrics *struct {
		DurationSeconds   present `json:"duration_seconds"`
		HTTPRequests      present `json:"http_requests"`
		FailedRequests    present `json:"failed_requests"`
		FailureRate       present `json:"failure_rate"`
		RequestsPerSecond present `json:"requests_per_second"`
		LatencyMs         *struct {
			Avg present `json:"avg"`
			Min present `json:"min"`
			P50 present `json:"p50"`
			P90 present `json:"p90"`
			P95 present `json:"p95"`
			Max present `json:"max"`
		} `json:"latency_ms"`
	} `json:"metrics"`
}

type pTelemetry struct {
	Completeness          present        `json:"completeness"`
	Attempts              present        `json:"attempts"`
	Succeeded             present        `json:"succeeded"`
	Failed                present        `json:"failed"`
	SampleLimitReached    present        `json:"sample_limit_reached"`
	RetainedBytes         present        `json:"retained_bytes"`
	RetentionLimitReached present        `json:"retention_limit_reached"`
	Samples               []pSample      `json:"samples"`
	Errors                []pError       `json:"errors"`
	NotRetained           []pNotRetained `json:"not_retained"`
}

type pSample struct {
	Sequence      present   `json:"sequence"`
	Phase         present   `json:"phase"`
	ScrapedAt     present   `json:"scraped_at"`
	OffsetSeconds present   `json:"offset_seconds"`
	Families      []pFamily `json:"families"`
}

type pFamily struct {
	Name   present   `json:"name"`
	Type   present   `json:"type"`
	Series []pSeries `json:"series"`
}

type pSeries struct {
	Histogram *struct {
		Count   present `json:"count"`
		Sum     present `json:"sum"`
		Buckets []struct {
			UpperBound      present `json:"upper_bound"`
			CumulativeCount present `json:"cumulative_count"`
		} `json:"buckets"`
	} `json:"histogram"`
}

type pError struct {
	Sequence      present `json:"sequence"`
	Phase         present `json:"phase"`
	AttemptedAt   present `json:"attempted_at"`
	OffsetSeconds present `json:"offset_seconds"`
	Error         present `json:"error"`
}

type pNotRetained struct {
	Sequence      present `json:"sequence"`
	Phase         present `json:"phase"`
	ScrapedAt     present `json:"scraped_at"`
	OffsetSeconds present `json:"offset_seconds"`
	SizeBytes     present `json:"size_bytes"`
}

// check reports every required field that is missing or null.
func (p *pReport) check(v *validator) {
	need := func(ok present, name string) {
		if !ok {
			v.fail("%s is missing", name)
		}
	}
	need(p.Status, "status")
	need(p.StartedAt, "started_at")
	need(p.FinishedAt, "finished_at")

	if c := p.Config; c == nil {
		v.fail("config is missing")
	} else {
		need(c.TargetURL, "config.target_url")
		need(c.VUs, "config.vus")
		need(c.DurationSeconds, "config.duration_seconds")
		need(c.MetricsURL, "config.metrics_url")
		need(c.SampleIntervalSeconds, "config.sample_interval_seconds")
	}

	if w := p.Workload; w == nil {
		v.fail("workload is missing")
	} else {
		if w.Verdict == nil {
			v.fail("workload.verdict is missing")
		} else {
			need(w.Verdict.Passed, "workload.verdict.passed")
		}
		if r := w.Result; r != nil {
			need(r.Runner, "workload.result.runner")
			if m := r.Metrics; m == nil {
				v.fail("workload.result.metrics is missing")
			} else {
				need(m.DurationSeconds, "workload.result.metrics.duration_seconds")
				need(m.HTTPRequests, "workload.result.metrics.http_requests")
				need(m.FailedRequests, "workload.result.metrics.failed_requests")
				need(m.FailureRate, "workload.result.metrics.failure_rate")
				need(m.RequestsPerSecond, "workload.result.metrics.requests_per_second")
				if l := m.LatencyMs; l == nil {
					v.fail("workload.result.metrics.latency_ms is missing")
				} else {
					for _, f := range []struct {
						ok   present
						name string
					}{{l.Avg, "avg"}, {l.Min, "min"}, {l.P50, "p50"}, {l.P90, "p90"}, {l.P95, "p95"}, {l.Max, "max"}} {
						need(f.ok, "workload.result.metrics.latency_ms."+f.name)
					}
				}
			}
		}
	}

	t := p.Telemetry
	if t == nil {
		v.fail("telemetry is missing")
		return
	}
	need(t.Completeness, "telemetry.completeness")
	need(t.Attempts, "telemetry.attempts")
	need(t.Succeeded, "telemetry.succeeded")
	need(t.Failed, "telemetry.failed")
	need(t.SampleLimitReached, "telemetry.sample_limit_reached")
	need(t.RetainedBytes, "telemetry.retained_bytes")
	need(t.RetentionLimitReached, "telemetry.retention_limit_reached")
	for _, l := range []struct {
		missing bool
		name    string
	}{{t.Samples == nil, "samples"}, {t.Errors == nil, "errors"}, {t.NotRetained == nil, "not_retained"}} {
		if l.missing {
			v.fail("telemetry.%s is missing", l.name)
		}
	}
	for i, s := range t.Samples {
		name := fmt.Sprintf("telemetry.samples[%d]", i)
		need(s.Sequence, name+".sequence")
		need(s.Phase, name+".phase")
		need(s.ScrapedAt, name+".scraped_at")
		need(s.OffsetSeconds, name+".offset_seconds")
		if s.Families == nil {
			v.fail("%s.families is missing", name)
		}
		for j, f := range s.Families {
			fname := fmt.Sprintf("%s.families[%d]", name, j)
			need(f.Name, fname+".name")
			need(f.Type, fname+".type")
			if f.Series == nil {
				v.fail("%s.series is missing", fname)
			}
			for k, sr := range f.Series {
				h := sr.Histogram
				if h == nil {
					continue
				}
				hname := fmt.Sprintf("%s.series[%d].histogram", fname, k)
				need(h.Count, hname+".count")
				need(h.Sum, hname+".sum")
				if h.Buckets == nil {
					v.fail("%s.buckets is missing", hname)
				}
				for b, bk := range h.Buckets {
					need(bk.UpperBound, fmt.Sprintf("%s.buckets[%d].upper_bound", hname, b))
					need(bk.CumulativeCount, fmt.Sprintf("%s.buckets[%d].cumulative_count", hname, b))
				}
			}
		}
	}
	for i, e := range t.Errors {
		name := fmt.Sprintf("telemetry.errors[%d]", i)
		need(e.Sequence, name+".sequence")
		need(e.Phase, name+".phase")
		need(e.AttemptedAt, name+".attempted_at")
		need(e.OffsetSeconds, name+".offset_seconds")
		need(e.Error, name+".error")
	}
	for i, n := range t.NotRetained {
		name := fmt.Sprintf("telemetry.not_retained[%d]", i)
		need(n.Sequence, name+".sequence")
		need(n.Phase, name+".phase")
		need(n.ScrapedAt, name+".scraped_at")
		need(n.OffsetSeconds, name+".offset_seconds")
		need(n.SizeBytes, name+".size_bytes")
	}
}
