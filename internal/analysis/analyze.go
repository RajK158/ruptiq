package analysis

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/RajK158/ruptiq/internal/experiment"
	"github.com/RajK158/ruptiq/internal/telemetry"
	"github.com/RajK158/ruptiq/internal/workload"
)

// maxExactCounter is the largest value up to which every whole number is
// exactly representable in a float64 (2^53).
const maxExactCounter = 1 << 53

// Analyze extracts signals from a report returned by DecodeExperiment. It
// returns an error only if cfg is invalid.
func Analyze(r experiment.Report, cfg Config) (Analysis, error) {
	if err := cfg.Validate(); err != nil {
		return Analysis{}, err
	}
	an := &analyzer{cfg: cfg, totals: map[routeKey]*totalAcc{}}
	out := Analysis{
		Experiment: experimentInfo(r),
		Config:     cfg,
		Client:     clientSignals(r),
		Totals:     []RouteSignals{},
		Windows:    []Window{},
	}

	missed := map[int]MissedScrape{}
	for _, e := range r.Telemetry.Errors {
		missed[e.Sequence] = MissedScrape{Sequence: e.Sequence, Phase: e.Phase, Reason: "scrape_failed",
			Error: shorten(workload.RedactText(e.Error, ""), 300)}
	}
	for _, n := range r.Telemetry.NotRetained {
		missed[n.Sequence] = MissedScrape{Sequence: n.Sequence, Phase: n.Phase, Reason: "not_retained"}
	}

	snaps := make([]*snapshot, len(r.Telemetry.Samples))
	for i, s := range r.Telemetry.Samples {
		snaps[i] = an.prepare(s)
	}
	an.familyWarnings(snaps)
	for i := 1; i < len(snaps); i++ {
		out.Windows = append(out.Windows, an.window(i, snaps[i-1], snaps[i], missed))
	}
	out.Totals = an.finishTotals()
	out.Warnings = an.warnings
	if out.Warnings == nil {
		out.Warnings = []Warning{}
	}
	out.Limitations = limitations(r, out)
	return out, nil
}

func experimentInfo(r experiment.Report) ExperimentInfo {
	t := r.Telemetry
	return ExperimentInfo{
		SchemaVersion:         r.SchemaVersion,
		Status:                r.Status,
		Reasons:               redactAll(r.Reasons),
		StartedAt:             r.StartedAt,
		FinishedAt:            r.FinishedAt,
		TargetURL:             workload.RedactURL(r.Config.TargetURL),
		MetricsURL:            workload.RedactURL(r.Config.MetricsURL),
		VUs:                   r.Config.VUs,
		DurationSeconds:       r.Config.DurationSeconds,
		SampleIntervalSeconds: r.Config.SampleIntervalSeconds,
		Completeness:          t.Completeness,
		CompletenessReasons:   redactAll(t.CompletenessReasons),
		Attempts:              t.Attempts,
		Samples:               len(t.Samples),
		ScrapeErrors:          len(t.Errors),
		NotRetained:           len(t.NotRetained),
	}
}

func clientSignals(r experiment.Report) ClientSignals {
	res := r.Workload.Result
	return ClientSignals{
		Source:        res.Runner + " (client side)",
		RunnerVersion: res.RunnerVersion,
		Metrics:       res.Metrics,
		Passed:        r.Workload.Verdict.Passed,
	}
}

func redactAll(texts []string) []string {
	if texts == nil {
		return nil
	}
	out := make([]string, len(texts))
	for i, t := range texts {
		out[i] = workload.RedactText(t, "")
	}
	return out
}

type analyzer struct {
	cfg      Config
	warnings []Warning
	totals   map[routeKey]*totalAcc
}

func (an *analyzer) warn(w Warning) {
	an.warnings = append(an.warnings, w)
}

// snapshot is one sample's configured families, indexed by series labels. A
// nil map means the family cannot be used in this sample, for the reason in
// the matching problem.
type snapshot struct {
	ref         SampleRef
	counters    map[string]counterSeries
	counterProb *familyProblem
	hists       map[string]histSeries
	histProb    *familyProblem
}

// familyProblem says why a configured family cannot be used in a sample.
type familyProblem struct {
	kind string // WarnFamilyMissing or WarnFamilyType
	msg  string
}

// probText returns the first problem's message, or "".
func probText(ps ...*familyProblem) string {
	for _, p := range ps {
		if p != nil {
			return p.msg
		}
	}
	return ""
}

type counterSeries struct {
	labels map[string]string
	value  float64
}

type histSeries struct {
	labels map[string]string
	h      *telemetry.HistogramValue
}

func (an *analyzer) prepare(s experiment.Sample) *snapshot {
	snap := &snapshot{ref: SampleRef{Sequence: s.Sequence, Phase: s.Phase, ScrapedAt: s.ScrapedAt, OffsetSeconds: s.OffsetSeconds}}
	if f, prob := findFamily(s, an.cfg.RequestsCounter, telemetry.Counter); prob != nil {
		snap.counterProb = prob
	} else {
		snap.counters = make(map[string]counterSeries, len(f.Series))
		for _, sr := range f.Series {
			snap.counters[labelKey(sr.Labels)] = counterSeries{labels: sr.Labels, value: float64(*sr.Value)}
		}
	}
	if f, prob := findFamily(s, an.cfg.DurationHistogram, telemetry.Histogram); prob != nil {
		snap.histProb = prob
	} else {
		snap.hists = make(map[string]histSeries, len(f.Series))
		for _, sr := range f.Series {
			snap.hists[labelKey(sr.Labels)] = histSeries{labels: sr.Labels, h: sr.Histogram}
		}
	}
	return snap
}

// findFamily returns the named family, or why it cannot be used.
func findFamily(s experiment.Sample, name string, want telemetry.MetricType) (telemetry.Family, *familyProblem) {
	for _, f := range s.Families {
		if f.Name != name {
			continue
		}
		if f.Type != want {
			return telemetry.Family{}, &familyProblem{WarnFamilyType,
				fmt.Sprintf("%s is a %s in sample #%d, not a %s", name, f.Type, s.Sequence, want)}
		}
		return f, nil
	}
	return telemetry.Family{}, &familyProblem{WarnFamilyMissing, fmt.Sprintf("%s is not in sample #%d", name, s.Sequence)}
}

// familyWarnings reports configured families that are missing or of the
// wrong type: once if no sample has them, otherwise once per sample.
func (an *analyzer) familyWarnings(snaps []*snapshot) {
	for _, fam := range []struct {
		name string
		prob func(*snapshot) *familyProblem
	}{
		{an.cfg.RequestsCounter, func(s *snapshot) *familyProblem { return s.counterProb }},
		{an.cfg.DurationHistogram, func(s *snapshot) *familyProblem { return s.histProb }},
	} {
		var missing, bad []*snapshot
		for _, s := range snaps {
			if p := fam.prob(s); p != nil {
				bad = append(bad, s)
				if p.kind == WarnFamilyMissing {
					missing = append(missing, s)
				}
			}
		}
		if len(missing) == len(snaps) && len(snaps) > 1 {
			an.warn(Warning{Kind: WarnFamilyMissing, Metric: fam.name,
				Message: fmt.Sprintf("%s is in none of the %d samples; check the metric name", fam.name, len(snaps))})
			continue
		}
		for _, s := range bad {
			p := fam.prob(s)
			an.warn(Warning{Kind: p.kind, Sequence: s.ref.Sequence, Metric: fam.name, Message: p.msg})
		}
	}
}

type routeKey struct{ method, route string }

func (an *analyzer) routeOf(labels map[string]string) routeKey {
	var k routeKey
	if an.cfg.MethodLabel != "" {
		k.method = labels[an.cfg.MethodLabel]
	}
	if an.cfg.RouteLabel != "" {
		k.route = labels[an.cfg.RouteLabel]
	}
	return k
}

// statusClass returns 4 or 5 for 4xx and 5xx statuses, 1 for other valid
// statuses (100-399), and 0 for a missing or non-numeric status.
func statusClass(status string) int {
	n, err := strconv.Atoi(status)
	switch {
	case err != nil || n < 100 || n > 599:
		return 0
	case n >= 500:
		return 5
	case n >= 400:
		return 4
	}
	return 1
}

// reqAcc accumulates one route's counter increases.
type reqAcc struct {
	total, server, client, unclassified float64
	byStatus                            map[string]float64
	series                              []map[string]string
	problems                            []string
}

func (a *reqAcc) add(status string, class int, delta float64, labels map[string]string) {
	if a.byStatus == nil {
		a.byStatus = map[string]float64{}
	}
	a.total += delta
	a.byStatus[status] += delta
	switch class {
	case 5:
		a.server += delta
	case 4:
		a.client += delta
	case 0:
		a.unclassified += delta
	}
	if labels != nil {
		a.series = append(a.series, labels)
	}
}

// histAcc accumulates one route's interval histogram.
type histAcc struct {
	bounds   []float64
	counts   []uint64
	obs      uint64
	sum      float64
	series   []map[string]string
	problems []string
}

// merge adds an interval histogram, or returns false if its bounds differ.
func (a *histAcc) merge(bounds []float64, counts []uint64, obs uint64, sum float64) bool {
	if a.bounds == nil {
		a.bounds = slices.Clone(bounds)
		a.counts = make([]uint64, len(counts))
	} else if !slices.Equal(a.bounds, bounds) {
		return false
	}
	for i, c := range counts {
		a.counts[i] += c
	}
	a.obs += obs
	a.sum += sum
	return true
}

// routeAcc is one route's data in one window. A nil accumulator means the
// route has no series in that family.
type routeAcc struct {
	req  *reqAcc
	hist *histAcc
}

func (an *analyzer) window(index int, a, b *snapshot, missed map[int]MissedScrape) Window {
	w := Window{
		Index:          index,
		From:           a.ref,
		To:             b.ref,
		ElapsedSeconds: b.ref.ScrapedAt.Sub(a.ref.ScrapedAt).Seconds(),
		MissedScrapes:  []MissedScrape{},
		Routes:         []RouteSignals{},
	}
	for seq := a.ref.Sequence + 1; seq < b.ref.Sequence; seq++ {
		if m, ok := missed[seq]; ok {
			w.MissedScrapes = append(w.MissedScrapes, m)
		}
	}
	if n := len(w.MissedScrapes); n > 0 {
		an.warn(Warning{Kind: WarnScrapeGap, Window: index, Message: fmt.Sprintf(
			"window %d (samples #%d to #%d) spans %d scrape(s) without values, so it covers %.3fs rather than one interval",
			index, a.ref.Sequence, b.ref.Sequence, n, w.ElapsedSeconds)})
	}

	routes := map[routeKey]*routeAcc{}
	get := func(k routeKey) *routeAcc {
		r := routes[k]
		if r == nil {
			r = &routeAcc{}
			routes[k] = r
		}
		return r
	}
	reqProb := an.counterWindow(index, a, b, get)
	histProb := an.histWindow(index, a, b, get)

	keys := make([]routeKey, 0, len(routes))
	for k := range routes {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, compareRoutes)
	for _, k := range keys {
		rs := an.finishRoute(index, k, routes[k], w.ElapsedSeconds, a, b, reqProb, histProb)
		w.Routes = append(w.Routes, rs)
		an.addTotal(index, k, routes[k], rs, w.ElapsedSeconds)
	}
	return w
}

func compareRoutes(x, y routeKey) int {
	if c := strings.Compare(x.method, y.method); c != 0 {
		return c
	}
	return strings.Compare(x.route, y.route)
}

// unionKeys returns the keys of both maps, sorted.
func unionKeys[V any](a, b map[string]V) []string {
	keys := make([]string, 0, len(a)+len(b))
	for k := range a {
		keys = append(keys, k)
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys
}

// counterWindow adds the counter increases of every route, or returns why the
// counter family cannot be compared in this window at all.
func (an *analyzer) counterWindow(index int, a, b *snapshot, get func(routeKey) *routeAcc) string {
	metric := an.cfg.RequestsCounter
	if a.counters == nil || b.counters == nil {
		for _, m := range []map[string]counterSeries{a.counters, b.counters} {
			for _, s := range m {
				get(an.routeOf(s.labels))
			}
		}
		return probText(a.counterProb, b.counterProb)
	}
	imprecise := false
	for _, key := range unionKeys(a.counters, b.counters) {
		sa, inA := a.counters[key]
		sb, inB := b.counters[key]
		labels := sb.labels
		if !inB {
			labels = sa.labels
		}
		r := get(an.routeOf(labels))
		if r.req == nil {
			r.req = &reqAcc{}
		}
		switch {
		case !inA:
			r.req.problems = append(r.req.problems, "a series appeared")
			an.warn(Warning{Kind: WarnSeriesAppeared, Window: index, Metric: metric, Labels: cleanLabels(labels), Message: fmt.Sprintf(
				"series is in sample #%d but not #%d; its earlier value is unknown, so it is not counted from zero", b.ref.Sequence, a.ref.Sequence)})
		case !inB:
			r.req.problems = append(r.req.problems, "a series disappeared")
			an.warn(Warning{Kind: WarnSeriesDisappeared, Window: index, Metric: metric, Labels: cleanLabels(labels), Message: fmt.Sprintf(
				"series is in sample #%d but not #%d", a.ref.Sequence, b.ref.Sequence)})
		case sb.value < sa.value:
			r.req.problems = append(r.req.problems, "a counter decreased (possible reset)")
			an.warn(Warning{Kind: WarnCounterReset, Window: index, Metric: metric, Labels: cleanLabels(labels), Message: fmt.Sprintf(
				"counter decreased from %v (sample #%d) to %v (sample #%d): the target may have restarted; no increase is calculated",
				sa.value, a.ref.Sequence, sb.value, b.ref.Sequence)})
		default:
			if sa.value > maxExactCounter || sb.value > maxExactCounter {
				imprecise = true
			}
			status := ""
			if an.cfg.StatusLabel != "" {
				status = labels[an.cfg.StatusLabel]
			}
			r.req.add(status, statusClass(status), sb.value-sa.value, labels)
		}
	}
	if imprecise {
		an.warn(Warning{Kind: WarnPrecision, Window: index, Metric: metric, Message: fmt.Sprintf(
			"counter values above 2^53 are not exact in floating point; increases in window %d may be off by a few requests", index)})
	}
	return ""
}

// histWindow adds the interval histograms of every route, or returns why the
// histogram family cannot be compared in this window at all.
func (an *analyzer) histWindow(index int, a, b *snapshot, get func(routeKey) *routeAcc) string {
	metric := an.cfg.DurationHistogram
	if a.hists == nil || b.hists == nil {
		for _, m := range []map[string]histSeries{a.hists, b.hists} {
			for _, s := range m {
				get(an.routeOf(s.labels))
			}
		}
		return probText(a.histProb, b.histProb)
	}
	for _, key := range unionKeys(a.hists, b.hists) {
		sa, inA := a.hists[key]
		sb, inB := b.hists[key]
		labels := sb.labels
		if !inB {
			labels = sa.labels
		}
		r := get(an.routeOf(labels))
		if r.hist == nil {
			r.hist = &histAcc{}
		}
		warn := func(kind, problem, msg string) {
			r.hist.problems = append(r.hist.problems, problem)
			an.warn(Warning{Kind: kind, Window: index, Metric: metric, Labels: cleanLabels(labels), Message: msg})
		}
		switch {
		case !inA:
			warn(WarnSeriesAppeared, "a series appeared", fmt.Sprintf(
				"series is in sample #%d but not #%d; its earlier buckets are unknown, so they are not counted from zero", b.ref.Sequence, a.ref.Sequence))
			continue
		case !inB:
			warn(WarnSeriesDisappeared, "a series disappeared", fmt.Sprintf("series is in sample #%d but not #%d", a.ref.Sequence, b.ref.Sequence))
			continue
		}
		bounds, counts, obs, sum, kind, prob := subtractHistograms(sa.h, sb.h)
		if prob != "" {
			warn(kind, prob, fmt.Sprintf("%s between samples #%d and #%d; no interval histogram is calculated", prob, a.ref.Sequence, b.ref.Sequence))
			continue
		}
		if !r.hist.merge(bounds, counts, obs, sum) {
			warn(WarnBucketsChanged, "the route's series have different bucket bounds",
				"this series' bucket bounds differ from another series of the same route; they cannot be combined")
			continue
		}
		r.hist.series = append(r.hist.series, labels)
	}
	return ""
}

// subtractHistograms returns the interval histogram b - a: the observations
// made between the two samples. It refuses histograms whose bucket bounds
// differ or whose counts decreased (a reset).
func subtractHistograms(a, b *telemetry.HistogramValue) (bounds []float64, counts []uint64, obs uint64, sum float64, kind, problem string) {
	if len(a.Buckets) != len(b.Buckets) {
		return nil, nil, 0, 0, WarnBucketsChanged, "the bucket bounds changed"
	}
	bounds = make([]float64, len(b.Buckets))
	counts = make([]uint64, len(b.Buckets))
	for i := range b.Buckets {
		if a.Buckets[i].UpperBound != b.Buckets[i].UpperBound {
			return nil, nil, 0, 0, WarnBucketsChanged, "the bucket bounds changed"
		}
		if b.Buckets[i].CumulativeCount < a.Buckets[i].CumulativeCount {
			return nil, nil, 0, 0, WarnHistogramReset, "a bucket count decreased (possible reset)"
		}
		bounds[i] = float64(b.Buckets[i].UpperBound)
		counts[i] = b.Buckets[i].CumulativeCount - a.Buckets[i].CumulativeCount
		if i > 0 && counts[i] < counts[i-1] {
			return nil, nil, 0, 0, WarnHistogramReset, "the bucket increases are not cumulative"
		}
	}
	if b.Count < a.Count {
		return nil, nil, 0, 0, WarnHistogramReset, "the histogram count decreased (possible reset)"
	}
	obs = b.Count - a.Count
	if n := len(counts); n > 0 && counts[n-1] > obs {
		return nil, nil, 0, 0, WarnHistogramReset, "the bucket increases exceed the count's increase"
	}
	return bounds, counts, obs, float64(b.Sum) - float64(a.Sum), "", ""
}

func (an *analyzer) finishRoute(index int, k routeKey, r *routeAcc, elapsed float64, a, b *snapshot, reqProb, histProb string) RouteSignals {
	rs := RouteSignals{Method: cleanText(k.method), Route: cleanText(k.route)}
	between := fmt.Sprintf("between samples #%d and #%d", a.ref.Sequence, b.ref.Sequence)

	switch {
	case reqProb != "":
		rs.RequestsUnavailable = reqProb
	case r.req == nil:
		rs.RequestsUnavailable = fmt.Sprintf("no series of %s for this route %s", an.cfg.RequestsCounter, between)
	case len(r.req.problems) > 0:
		rs.RequestsUnavailable = "not comparable " + between + ": " + joinProblems(r.req.problems)
	default:
		sig, prob := an.requestSignals(r.req, elapsed)
		if prob != "" {
			rs.RequestsUnavailable = prob
			an.warn(Warning{Kind: WarnOverflow, Window: index, Metric: an.cfg.RequestsCounter, Message: prob})
		} else {
			rs.Requests = sig
		}
	}

	switch {
	case histProb != "":
		rs.LatencyUnavailable = histProb
	case r.hist == nil:
		rs.LatencyUnavailable = fmt.Sprintf("no series of %s for this route %s", an.cfg.DurationHistogram, between)
	case len(r.hist.problems) > 0:
		rs.LatencyUnavailable = "not comparable " + between + ": " + joinProblems(r.hist.problems)
	default:
		rs.Latency = an.latencySignals(r.hist)
	}
	return rs
}

// joinProblems lists distinct problems with their counts.
func joinProblems(problems []string) string {
	counts := map[string]int{}
	var order []string
	for _, p := range problems {
		if counts[p] == 0 {
			order = append(order, p)
		}
		counts[p]++
	}
	parts := make([]string, len(order))
	for i, p := range order {
		parts[i] = p
		if counts[p] > 1 {
			parts[i] = fmt.Sprintf("%s (%d series)", p, counts[p])
		}
	}
	return strings.Join(parts, "; ")
}

func finite(xs ...float64) bool {
	for _, x := range xs {
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return false
		}
	}
	return true
}

func (an *analyzer) requestSignals(acc *reqAcc, elapsed float64) (*RequestSignals, string) {
	per := acc.total / elapsed
	if !finite(acc.total, acc.server, acc.client, acc.unclassified, per) {
		return nil, "the request increase or rate is too large to represent as a finite number"
	}
	sig := &RequestSignals{
		Requests:       acc.total,
		ElapsedSeconds: elapsed,
		PerSecond:      per,
		ByStatus:       []StatusCount{},
		ServerErrors:   acc.server,
		ClientErrors:   acc.client,
		Unclassified:   acc.unclassified,
		Evidence:       an.evidence(an.cfg.RequestsCounter, acc.series),
	}
	statuses := make([]string, 0, len(acc.byStatus))
	for s := range acc.byStatus {
		statuses = append(statuses, s)
	}
	slices.Sort(statuses)
	for _, s := range statuses {
		sig.ByStatus = append(sig.ByStatus, StatusCount{Status: cleanText(s), Requests: acc.byStatus[s]})
	}
	switch {
	case an.cfg.StatusLabel == "":
		sig.ServerErrorRatioUnavailable = "no status label is configured"
	case acc.total == 0:
		sig.ServerErrorRatioUnavailable = "no requests were completed"
	case acc.unclassified > 0:
		sig.ServerErrorRatioUnavailable = fmt.Sprintf("%v requests have no numeric %q label, so they may or may not be server errors",
			acc.unclassified, an.cfg.StatusLabel)
	default:
		ratio := acc.server / acc.total
		sig.ServerErrorRatio = &ratio
	}
	return sig, ""
}

func (an *analyzer) latencySignals(acc *histAcc) *LatencySignals {
	sig := &LatencySignals{
		Observations: acc.obs,
		P95:          estimateQuantile(Quantile, acc.bounds, acc.counts, acc.obs, an.cfg.MinObservations),
		Buckets:      make([]Bucket, len(acc.bounds)),
		Evidence:     an.evidence(an.cfg.DurationHistogram, acc.series),
	}
	for i := range acc.bounds {
		sig.Buckets[i] = Bucket{UpperBound: telemetry.Float(acc.bounds[i]), CumulativeCount: acc.counts[i]}
	}
	switch {
	case acc.obs == 0:
		sig.MeanSecondsUnavailable = "no requests were completed"
	case !finite(acc.sum) || acc.sum < 0:
		sig.MeanSecondsUnavailable = "the histogram's sum did not increase by a finite, non-negative amount"
	default:
		mean := acc.sum / float64(acc.obs)
		sig.MeanSeconds = &mean
	}
	return sig
}

// estimateQuantile estimates quantile q from cumulative bucket counts by
// linear interpolation within the bucket that contains rank q*total, as
// PromQL's histogram_quantile does. The lowest bucket is taken to start at 0.
func estimateQuantile(q float64, bounds []float64, counts []uint64, total uint64, minObs int) QuantileEstimate {
	e := QuantileEstimate{Quantile: q, Estimate: true}
	switch {
	case len(bounds) == 0:
		e.Unavailable = "the histogram has no buckets"
		return e
	case total == 0:
		e.Unavailable = "no requests were completed"
		return e
	case total < uint64(minObs):
		e.Unavailable = fmt.Sprintf("only %d requests were completed; at least %d are needed for an estimate", total, minObs)
		return e
	}
	rank := q * float64(total)
	i := slices.IndexFunc(counts, func(c uint64) bool { return float64(c) >= rank })
	if i < 0 {
		e.Unavailable = fmt.Sprintf("the p%g rank lies above the histogram's largest bucket bound (%gs)", q*100, bounds[len(bounds)-1])
		return e
	}
	upper := bounds[i]
	if math.IsInf(upper, 1) {
		if i == 0 {
			e.Unavailable = "the histogram has only a +Inf bucket"
		} else {
			e.Unavailable = fmt.Sprintf("the p%g rank lies in the +Inf bucket, above the largest finite bound (%gs)", q*100, bounds[i-1])
		}
		return e
	}
	lower, below := math.Min(0, upper), uint64(0)
	if i > 0 {
		lower, below = bounds[i-1], counts[i-1]
	}
	if math.IsInf(lower, -1) {
		e.Unavailable = "the p95 rank lies in a bucket with no lower bound"
		return e
	}
	est := lower + (upper-lower)*(rank-float64(below))/float64(counts[i]-below)
	e.Seconds, e.BucketLowerSeconds, e.BucketUpperSeconds = &est, &lower, &upper
	return e
}

func (an *analyzer) evidence(metric string, series []map[string]string) Evidence {
	ev := Evidence{Metric: metric, Series: make([]map[string]string, 0, len(series))}
	seen := map[string]bool{}
	for _, l := range series {
		k := labelKey(l)
		if seen[k] {
			continue
		}
		seen[k] = true
		ev.Series = append(ev.Series, cleanLabels(l))
	}
	slices.SortFunc(ev.Series, func(x, y map[string]string) int { return strings.Compare(labelKey(x), labelKey(y)) })
	return ev
}

// totalAcc combines one route's windows.
type totalAcc struct {
	req      reqAcc
	reqTime  float64
	reqWins  []int
	reqSkip  []int
	hist     histAcc
	histWins []int
	histSkip []int
	histBad  bool
}

func (an *analyzer) addTotal(index int, k routeKey, r *routeAcc, rs RouteSignals, elapsed float64) {
	t := an.totals[k]
	if t == nil {
		t = &totalAcc{}
		an.totals[k] = t
	}
	if rs.Requests != nil {
		for s, n := range r.req.byStatus {
			t.req.add(s, statusClass(s), n, nil)
		}
		t.req.series = append(t.req.series, r.req.series...)
		t.reqTime += elapsed
		t.reqWins = append(t.reqWins, index)
	} else {
		t.reqSkip = append(t.reqSkip, index)
	}
	if rs.Latency != nil && !t.histBad {
		if t.hist.merge(r.hist.bounds, r.hist.counts, r.hist.obs, r.hist.sum) {
			t.hist.series = append(t.hist.series, r.hist.series...)
			t.histWins = append(t.histWins, index)
		} else {
			t.histBad = true
			an.warn(Warning{Kind: WarnBucketsChanged, Window: index, Metric: an.cfg.DurationHistogram, Message: fmt.Sprintf(
				"%s %s has different bucket bounds in window %d than in earlier windows, so no total latency is calculated",
				cleanText(k.method), cleanText(k.route), index)})
		}
	} else if rs.Latency == nil {
		t.histSkip = append(t.histSkip, index)
	}
}

func (an *analyzer) finishTotals() []RouteSignals {
	keys := make([]routeKey, 0, len(an.totals))
	for k := range an.totals {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, compareRoutes)
	out := make([]RouteSignals, 0, len(keys))
	for _, k := range keys {
		t := an.totals[k]
		rs := RouteSignals{Method: cleanText(k.method), Route: cleanText(k.route)}
		if len(t.reqWins) == 0 {
			rs.RequestsUnavailable = "unavailable in every window"
		} else if sig, prob := an.requestSignals(&t.req, t.reqTime); prob != "" {
			rs.RequestsUnavailable = prob
		} else {
			sig.Evidence.Windows = t.reqWins
			sig.Evidence.ExcludedWindows = t.reqSkip
			rs.Requests = sig
		}
		switch {
		case t.histBad:
			rs.LatencyUnavailable = "the route's bucket bounds differ between windows, so its windows cannot be combined"
		case len(t.histWins) == 0:
			rs.LatencyUnavailable = "unavailable in every window"
		default:
			sig := an.latencySignals(&t.hist)
			sig.Evidence.Windows = t.histWins
			sig.Evidence.ExcludedWindows = t.histSkip
			rs.Latency = sig
		}
		out = append(out, rs)
	}
	return out
}

func limitations(r experiment.Report, a Analysis) []string {
	out := []string{
		"Rates are averages over each window between two scrapes (for totals, over the windows used); anything shorter than a window is not visible.",
		"p95 latencies are estimated from histogram buckets: the true value lies within the reported bucket bounds, and the estimate assumes requests are spread evenly within that bucket.",
		"Latency signals combine every response status of a route, including errors.",
		"Server-side signals count every request the service handled, from any client, not only this experiment's workload.",
		"k6's figures are measured on the client, include network time, and cover a different time span; they are reported separately and not reconciled with the server's.",
		"These are signals, not a diagnosis: no cause is inferred, and with a few windows no statistical significance is claimed.",
	}
	if len(a.Windows) == 0 {
		out = append(out, fmt.Sprintf("Fewer than two samples were retained (%d), so no window could be analysed.", len(r.Telemetry.Samples)))
	}
	if r.Telemetry.Completeness != experiment.Complete {
		out = append(out, fmt.Sprintf("Telemetry is %s: %s.", r.Telemetry.Completeness, strings.Join(redactAll(r.Telemetry.CompletenessReasons), "; ")))
	}
	gaps := 0
	for _, w := range a.Windows {
		if len(w.MissedScrapes) > 0 {
			gaps++
		}
	}
	if gaps > 0 {
		out = append(out, fmt.Sprintf("%d window(s) span scrapes without values, so they are longer than the sampling interval and hide what happened within them.", gaps))
	}
	if n := len(a.Windows); n > 0 && (a.Windows[0].From.Phase == experiment.BeforeWorkload || a.Windows[n-1].To.Phase == experiment.AfterWorkload) {
		out = append(out, "Windows that start before the workload or end after it include time without load, and the first and last windows during it include k6 starting and stopping.")
	}
	if r.Status == experiment.StatusFailed {
		out = append(out, "The workload failed its pass criteria; the signals describe the run as it happened.")
	}
	return out
}

// labelKey is a canonical form of a label set: sorted, quoted pairs.
func labelKey(l map[string]string) string {
	names := make([]string, 0, len(l))
	for n := range l {
		names = append(names, n)
	}
	slices.Sort(names)
	var b strings.Builder
	for _, n := range names {
		b.WriteString(strconv.Quote(n))
		b.WriteByte('=')
		b.WriteString(strconv.Quote(l[n]))
		b.WriteByte(',')
	}
	return b.String()
}

// cleanText redacts any URL credentials or query string in a label value.
func cleanText(s string) string {
	return workload.RedactText(s, "")
}

func cleanLabels(l map[string]string) map[string]string {
	if l == nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(l))
	for k, v := range l {
		out[k] = cleanText(v)
	}
	return out
}
