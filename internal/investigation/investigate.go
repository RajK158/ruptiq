package investigation

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"slices"
	"strings"
	"unicode"

	"github.com/RajK158/ruptiq/internal/analysis"
	"github.com/RajK158/ruptiq/internal/experiment"
	"github.com/RajK158/ruptiq/internal/workload"
)

// Caps that keep a report readable when an analysis has many windows.
const (
	maxGapsPerSignal    = 20
	maxEvidencePerGroup = 40
	maxTextLen          = 300
)

// Investigate compares, for every route, each signal's earlier and later
// usable windows and returns the ranked findings. It reads only a; the same
// analysis and config always give the same investigation.
func Investigate(a analysis.Analysis, cfg Config) (Investigation, error) {
	if err := cfg.Validate(); err != nil {
		return Investigation{}, err
	}
	in := &investigator{a: a, cfg: cfg}
	inv := Investigation{
		Source:      source(a),
		Config:      cfg,
		Windows:     in.selectWindows(),
		Findings:    []Finding{},
		Assessments: []Assessment{},
	}
	for _, k := range in.routes() {
		results := map[string]result{}
		for _, signal := range []string{SignalThroughput, SignalServerErrorRatio, SignalP95Latency} {
			r := in.assess(k, signal)
			results[signal] = r
			inv.Assessments = append(inv.Assessments, r.a)
		}
		inv.Findings = append(inv.Findings, in.findings(k, results)...)
	}
	rank(inv.Findings)
	inv.Outcome, inv.Summary = outcome(inv)
	inv.Limitations = in.limitations()
	return inv, nil
}

type investigator struct {
	a          analysis.Analysis
	cfg        Config
	comparable map[int]bool // by window array position
	positions  map[routeKey][]routeAt
}

type routeKey struct{ method, route string }

// routeAt locates a route's signals in the analysis.
type routeAt struct {
	window int // array position in Windows
	route  int // array position in the window's Routes
}

func source(a analysis.Analysis) Source {
	e := a.Experiment
	return Source{
		AnalysisSchemaVersion: analysis.SchemaVersion,
		ExperimentStatus:      e.Status,
		TelemetryCompleteness: e.Completeness,
		StartedAt:             e.StartedAt,
		TargetURL:             workload.RedactURL(e.TargetURL),
		MetricsURL:            workload.RedactURL(e.MetricsURL),
		VUs:                   e.VUs,
		DurationSeconds:       e.DurationSeconds,
		SampleIntervalSeconds: e.SampleIntervalSeconds,
		RequestsCounter:       clean(a.Config.RequestsCounter),
		DurationHistogram:     clean(a.Config.DurationHistogram),
		Windows:               len(a.Windows),
	}
}

func windowPointer(pos int) string { return fmt.Sprintf("/analysis/windows/%d", pos) }

// selectWindows keeps the windows taken entirely during the workload with no
// missed scrapes. Windows touching the before or after sample include the
// load generator starting or stopping, and a window spanning missed scrapes
// averages over a longer, different span.
func (in *investigator) selectWindows() WindowSelection {
	in.comparable = map[int]bool{}
	sel := WindowSelection{Comparable: []int{}, Excluded: []ExcludedWindow{}}
	for i, w := range in.a.Windows {
		var reason string
		switch {
		case w.From.Phase != experiment.DuringWorkload || w.To.Phase != experiment.DuringWorkload:
			reason = fmt.Sprintf("spans the workload's start or end (samples #%d %s to #%d %s), so its load is not steady",
				w.From.Sequence, w.From.Phase, w.To.Sequence, w.To.Phase)
		case len(w.MissedScrapes) > 0:
			reason = fmt.Sprintf("spans %d missed scrape(s), so it covers %.3fs instead of one sampling interval",
				len(w.MissedScrapes), w.ElapsedSeconds)
		}
		if reason != "" {
			sel.Excluded = append(sel.Excluded, ExcludedWindow{Window: w.Index, Reason: reason, Pointer: windowPointer(i)})
			continue
		}
		in.comparable[i] = true
		sel.Comparable = append(sel.Comparable, w.Index)
	}
	return sel
}

// routes returns every route of every window, sorted.
func (in *investigator) routes() []routeKey {
	in.positions = map[routeKey][]routeAt{}
	for i, w := range in.a.Windows {
		for j, rs := range w.Routes {
			k := routeKey{rs.Method, rs.Route}
			in.positions[k] = append(in.positions[k], routeAt{i, j})
		}
	}
	keys := make([]routeKey, 0, len(in.positions))
	for k := range in.positions {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(x, y routeKey) int {
		return cmp.Or(strings.Compare(x.method, y.method), strings.Compare(x.route, y.route))
	})
	return keys
}

// point is one usable window of a route's signal.
type point struct {
	at       routeAt
	window   int // window index
	value    float64
	requests float64 // requests, or histogram observations
	errors   float64 // 5xx responses, for the server error ratio
	elapsed  float64
	lower    float64 // p95 bucket bounds
	upper    float64
	mean     *float64
	pointer  string
	metric   string
	series   []map[string]string
}

// result is one assessment with what the findings need.
type result struct {
	a          Assessment
	base, comp []point
	change     Change
}

func (in *investigator) signalAt(at routeAt, signal string) (point, string) {
	w := in.a.Windows[at.window]
	rs := w.Routes[at.route]
	p := point{at: at, window: w.Index}
	base := fmt.Sprintf("%s/routes/%d", windowPointer(at.window), at.route)
	switch signal {
	case SignalThroughput, SignalServerErrorRatio:
		r := rs.Requests
		if r == nil {
			return p, rs.RequestsUnavailable
		}
		p.requests, p.errors, p.elapsed, p.value = r.Requests, r.ServerErrors, r.ElapsedSeconds, r.PerSecond
		p.metric, p.series, p.pointer = r.Evidence.Metric, r.Evidence.Series, base+"/requests"
		if signal == SignalServerErrorRatio {
			if r.ServerErrorRatio == nil {
				return p, r.ServerErrorRatioUnavailable
			}
			p.value, p.pointer = *r.ServerErrorRatio, base+"/requests/server_error_ratio"
		}
	case SignalP95Latency:
		l := rs.Latency
		if l == nil {
			return p, rs.LatencyUnavailable
		}
		if l.P95.Seconds == nil {
			return p, "p95 unavailable: " + l.P95.Unavailable
		}
		p.value, p.lower, p.upper = *l.P95.Seconds, *l.P95.BucketLowerSeconds, *l.P95.BucketUpperSeconds
		p.requests, p.elapsed, p.mean = float64(l.Observations), w.ElapsedSeconds, l.MeanSeconds
		p.metric, p.series, p.pointer = l.Evidence.Metric, l.Evidence.Series, base+"/latency/p95"
	}
	return p, ""
}

func (in *investigator) assess(k routeKey, signal string) result {
	res := result{a: Assessment{
		Method: clean(k.method), Route: clean(k.route), Signal: signal,
		BaselineWindows: []int{}, ComparisonWindows: []int{}, Gaps: []Gap{},
	}}
	present := map[int]routeAt{}
	for _, at := range in.positions[k] {
		present[at.window] = at
	}
	var usable []point
	for i, w := range in.a.Windows {
		if !in.comparable[i] {
			continue
		}
		at, ok := present[i]
		if !ok {
			res.a.Gaps = append(res.a.Gaps, Gap{w.Index, "the route has no series in this window"})
			continue
		}
		p, missing := in.signalAt(at, signal)
		if missing != "" {
			res.a.Gaps = append(res.a.Gaps, Gap{w.Index, clean(missing)})
			continue
		}
		usable = append(usable, p)
	}
	if len(res.a.Gaps) > maxGapsPerSignal {
		res.a.Gaps = res.a.Gaps[:maxGapsPerSignal]
	}

	insufficient := func(format string, args ...any) result {
		res.a.Status = StatusInsufficientEvidence
		res.a.Reason = fmt.Sprintf(format, args...)
		return res
	}
	n, need := len(usable), 2*in.cfg.MinWindows
	if n < need {
		return insufficient("%d usable window(s) of %d comparable; at least %d (%d per side) are needed",
			n, len(in.comparable), need, in.cfg.MinWindows)
	}
	half := n / 2
	res.base, res.comp = usable[:half], usable[n-half:]
	for _, p := range res.base {
		res.a.BaselineWindows = append(res.a.BaselineWindows, p.window)
	}
	for _, p := range res.comp {
		res.a.ComparisonWindows = append(res.a.ComparisonWindows, p.window)
	}
	b, c := in.measure(signal, res.base), in.measure(signal, res.comp)

	switch signal {
	case SignalThroughput:
		if b.Requests < in.cfg.MinRequests {
			return insufficient("the baseline has %g requests, fewer than %g", b.Requests, in.cfg.MinRequests)
		}
		if b.Value <= 0 {
			return insufficient("the baseline's median request rate is zero, so no relative change can be calculated")
		}
	case SignalServerErrorRatio:
		if b.Requests < in.cfg.MinRequests || c.Requests < in.cfg.MinRequests {
			return insufficient("the baseline has %g and the comparison %g requests; each needs at least %g",
				b.Requests, c.Requests, in.cfg.MinRequests)
		}
	case SignalP95Latency:
		if b.Requests < in.cfg.MinRequests || c.Requests < in.cfg.MinRequests {
			return insufficient("the baseline has %g and the comparison %g observations; each needs at least %g",
				b.Requests, c.Requests, in.cfg.MinRequests)
		}
		if b.Value <= 0 {
			return insufficient("the baseline's median p95 estimate is zero, so no relative change can be calculated")
		}
	}
	res.a.Baseline, res.a.Comparison = &b, &c
	res.change = in.change(signal, b, c)
	res.a.Change = &res.change
	res.a.Status, res.a.Reason = in.judge(signal, res, b, c)
	if n%2 == 1 {
		res.a.Reason += fmt.Sprintf(" (window %d, in the middle of %d usable windows, is in neither side)", usable[half].window, n)
	}
	return res
}

func median(xs []float64) float64 {
	s := slices.Clone(xs)
	slices.Sort(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

func (in *investigator) measure(signal string, pts []point) Measure {
	var values []float64
	m := Measure{}
	errs := 0.0
	for _, p := range pts {
		values = append(values, p.value)
		m.Requests += p.requests
		m.ElapsedSeconds += p.elapsed
		errs += p.errors
	}
	switch signal {
	case SignalThroughput:
		m.Value, m.Unit, m.Statistic = median(values), UnitRequestsPerSecond, "median of the windows' request rates"
	case SignalServerErrorRatio:
		m.Unit, m.Statistic = UnitRatio, "5xx responses / requests, pooled over the windows"
		m.ServerErrors = &errs
		if m.Requests > 0 {
			m.Value = errs / m.Requests
		}
	case SignalP95Latency:
		m.Value, m.Unit, m.Statistic, m.Estimate = median(values), UnitSeconds, "median of the windows' p95 estimates", true
		lo, hi := math.Inf(1), math.Inf(-1)
		for _, p := range pts {
			lo, hi = math.Min(lo, p.lower), math.Max(hi, p.upper)
		}
		m.BucketLowerSeconds, m.BucketUpperSeconds = &lo, &hi
	}
	return m
}

// change returns the comparison's change from the baseline. Its threshold
// multiple counts in the direction of degradation.
func (in *investigator) change(signal string, b, c Measure) Change {
	ch := Change{Absolute: c.Value - b.Value, Unit: b.Unit}
	if b.Value != 0 {
		rel := ch.Absolute / b.Value
		ch.Relative = &rel
	} else {
		ch.RelativeUnavailable = "the baseline is zero"
	}
	switch signal {
	case SignalThroughput:
		ch.ThresholdMultiple = -*ch.Relative / in.cfg.ThroughputDecrease
	case SignalServerErrorRatio:
		ch.ThresholdMultiple = ch.Absolute / in.cfg.ErrorRatioIncrease
	case SignalP95Latency:
		ch.ThresholdMultiple = *ch.Relative / in.cfg.LatencyIncrease
	}
	return ch
}

// judge decides whether a compared signal changed materially.
func (in *investigator) judge(signal string, r result, b, c Measure) (string, string) {
	ch := r.change
	switch signal {
	case SignalThroughput:
		drop := -*ch.Relative
		if drop < in.cfg.ThroughputDecrease {
			return StatusNoMaterialChange, fmt.Sprintf("the request rate changed by %s, short of the %s threshold",
				percent(*ch.Relative), percent(-in.cfg.ThroughputDecrease))
		}
		if maxValue(r.comp) >= minValue(r.base) {
			return StatusInconclusive, fmt.Sprintf("the median rate fell by %s, but not every comparison window is below every baseline window, so noise may explain it",
				percent(drop))
		}
		return StatusMaterialChange, fmt.Sprintf("the median rate fell by %s (threshold %s) and every comparison window is below every baseline window",
			percent(drop), percent(in.cfg.ThroughputDecrease))
	case SignalServerErrorRatio:
		if ch.Absolute < in.cfg.ErrorRatioIncrease {
			return StatusNoMaterialChange, fmt.Sprintf("the 5xx ratio changed by %s points, short of the +%s points threshold",
				signedPoints(ch.Absolute), points(in.cfg.ErrorRatioIncrease))
		}
		if *c.ServerErrors < in.cfg.MinServerErrors {
			return StatusInconclusive, fmt.Sprintf("the 5xx ratio rose by %s points, but from only %g 5xx responses (at least %g needed)",
				points(ch.Absolute), *c.ServerErrors, in.cfg.MinServerErrors)
		}
		if b.Value > 0 && c.Value < b.Value*in.cfg.ErrorRatioFactor {
			return StatusInconclusive, fmt.Sprintf("the 5xx ratio rose by %s points, but to only %.2f times the baseline's (at least %g times needed)",
				points(ch.Absolute), c.Value/b.Value, in.cfg.ErrorRatioFactor)
		}
		return StatusMaterialChange, fmt.Sprintf("the 5xx ratio rose by %s points (threshold +%s) from %g 5xx responses",
			points(ch.Absolute), points(in.cfg.ErrorRatioIncrease), *c.ServerErrors)
	default:
		rise := *ch.Relative
		if rise < in.cfg.LatencyIncrease {
			return StatusNoMaterialChange, fmt.Sprintf("the p95 estimate changed by %s, short of the %s threshold",
				percent(rise), percent(in.cfg.LatencyIncrease))
		}
		if *c.BucketLowerSeconds < *b.BucketUpperSeconds {
			return StatusInconclusive, fmt.Sprintf("the p95 estimate rose by %s, but the comparison's buckets (from %s) overlap the baseline's (up to %s): within a bucket the estimate is an interpolation, so the rise may not be real",
				percent(rise), seconds(*c.BucketLowerSeconds), seconds(*b.BucketUpperSeconds))
		}
		return StatusMaterialChange, fmt.Sprintf("the p95 estimate rose by %s (threshold %s) and every comparison window's p95 bucket (from %s) lies above every baseline window's (up to %s)",
			percent(rise), percent(in.cfg.LatencyIncrease), seconds(*c.BucketLowerSeconds), seconds(*b.BucketUpperSeconds))
	}
}

func minValue(pts []point) float64 {
	m := math.Inf(1)
	for _, p := range pts {
		m = math.Min(m, p.value)
	}
	return m
}

func maxValue(pts []point) float64 {
	m := math.Inf(-1)
	for _, p := range pts {
		m = math.Max(m, p.value)
	}
	return m
}

// findings turns one route's assessments into findings.
func (in *investigator) findings(k routeKey, rs map[string]result) []Finding {
	var out []Finding
	thr, errs, lat := rs[SignalThroughput], rs[SignalServerErrorRatio], rs[SignalP95Latency]
	material := func(r result) bool { return r.a.Status == StatusMaterialChange }

	if material(errs) {
		out = append(out, in.errorFinding(k, errs, lat))
	}
	var latF, thrF *Finding
	if material(lat) {
		f := in.latencyFinding(k, lat, thr)
		latF = &f
	}
	if material(thr) {
		f := in.throughputFinding(k, thr, lat)
		thrF = &f
	}
	if latF != nil && thrF != nil {
		if slices.Equal(lat.a.BaselineWindows, thr.a.BaselineWindows) && slices.Equal(lat.a.ComparisonWindows, thr.a.ComparisonWindows) {
			out = append(out, in.combinedFinding(k, lat, thr, latF.ID, thrF.ID))
		} else {
			note := "latency and throughput both changed, but over different windows, so they are not combined into one finding"
			latF.Limitations = append(latF.Limitations, note)
			thrF.Limitations = append(thrF.Limitations, note)
		}
	}
	for _, f := range []*Finding{latF, thrF} {
		if f != nil {
			out = append(out, *f)
		}
	}
	if f, ok := in.insufficientFinding(k, rs); ok {
		out = append(out, f)
	}
	return out
}

func findingID(typ string, k routeKey) string {
	sum := sha256.Sum256([]byte(k.method + "\x00" + k.route))
	return typ + "-" + hex.EncodeToString(sum[:6])
}

func severity(multiple float64) string {
	if multiple >= MajorMultiple {
		return SeverityMajor
	}
	return SeverityModerate
}

var typePriority = map[string]int{
	FindingCombinedDegradation:   1,
	FindingServerErrorIncrease:   2,
	FindingLatencyDegradation:    3,
	FindingThroughputDegradation: 4,
	FindingInsufficientEvidence:  5,
}

// newFinding fills what every finding of a compared signal shares.
func (in *investigator) newFinding(typ, designation string, k routeKey, multiple float64, rs ...result) Finding {
	f := Finding{
		ID: findingID(typ, k), Type: typ, Designation: designation,
		Method: clean(k.method), Route: clean(k.route), Severity: severity(multiple),
		Changes: []SignalChange{}, Evidence: []EvidenceRef{},
		BaselineWindows: rs[0].a.BaselineWindows, ComparisonWindows: rs[0].a.ComparisonWindows,
		Contradictions: []string{},
		Limitations: []string{
			"Observed within one experiment run: it is a change over time in this run, not a regression between versions or runs.",
			"No statistical significance test is made: the change exceeded fixed thresholds (see config), nothing more.",
		},
		Ranking: Ranking{TypePriority: typePriority[typ], ThresholdMultiple: multiple, Rule: RankingRule},
	}
	f.Ranking.Severity = f.Severity
	seen := map[string]bool{}
	for _, r := range rs {
		f.Changes = append(f.Changes, SignalChange{Signal: r.a.Signal, Baseline: *r.a.Baseline, Comparison: *r.a.Comparison, Change: r.change})
		f.Ranking.Windows = max(f.Ranking.Windows, len(r.base)+len(r.comp))
		f.Ranking.Requests = math.Max(f.Ranking.Requests, r.a.Baseline.Requests+r.a.Comparison.Requests)
		for _, side := range []struct {
			role string
			pts  []point
		}{{"baseline", r.base}, {"comparison", r.comp}} {
			for _, p := range side.pts {
				if seen[p.pointer] || len(f.Evidence) >= maxEvidencePerGroup {
					continue
				}
				seen[p.pointer] = true
				f.Evidence = append(f.Evidence, EvidenceRef{Pointer: p.pointer, Window: p.window, Role: side.role,
					Metric: clean(p.metric), Series: cleanSeries(p.series)})
			}
		}
	}
	return f
}

func (in *investigator) routeName(k routeKey) string {
	return strings.TrimSpace(orAny(clean(k.method), "(any method)") + " " + orAny(clean(k.route), "(any route)"))
}

func orAny(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}

// splitText describes a result's windows.
func splitText(r result) string {
	return fmt.Sprintf("baseline windows %s vs comparison windows %s", windowList(r.a.BaselineWindows), windowList(r.a.ComparisonWindows))
}

// repeatAdvice is the part of every next experiment that repeats the run.
func (in *investigator) repeatAdvice() string {
	s := in.a.Experiment
	need := 2*in.cfg.MinWindows*2 + 2
	dur := math.Max(s.DurationSeconds, float64(need)*s.SampleIntervalSeconds)
	return fmt.Sprintf("Repeat the experiment at least 3 times with the same target and %d VUs for at least %.0fs (about %d sampling intervals of %gs: twice the %d steady windows a comparison needs, plus the start and end), then run ruptiq-analyze and ruptiq-investigate on each run",
		s.VUs, dur, need, s.SampleIntervalSeconds, 2*in.cfg.MinWindows)
}

func (in *investigator) errorFinding(k routeKey, r, lat result) Finding {
	f := in.newFinding(FindingServerErrorIncrease, DesignationObservation, k, r.change.ThresholdMultiple, r)
	b, c := r.a.Baseline, r.a.Comparison
	f.Symptom = fmt.Sprintf("%s: the HTTP 5xx ratio rose from %s (%g of %g requests) to %s (%g of %g requests)",
		in.routeName(k), ratio(b.Value), *b.ServerErrors, b.Requests, ratio(c.Value), *c.ServerErrors, c.Requests)
	f.Explanation = fmt.Sprintf("Reported because %s, comparing %s. 4xx responses are client errors and are not counted.", r.a.Reason, splitText(r))
	for _, p := range r.comp {
		if p.value <= b.Value {
			f.Contradictions = append(f.Contradictions, fmt.Sprintf("in comparison window %d the 5xx ratio (%s) was no higher than the baseline's, so the increase is not uniform", p.window, ratio(p.value)))
		}
	}
	if lat.a.Status == StatusNoMaterialChange {
		f.Contradictions = append(f.Contradictions, "p95 latency did not change materially ("+lat.a.Reason+"), so the errors are not accompanied by a slowdown")
	}
	f.Limitations = append(f.Limitations,
		"A higher 5xx ratio does not by itself show lower service capacity: errors can come from invalid requests or data, a failing dependency, or deliberate load shedding.",
		"k6's failure rate covers the whole run on the client side and is not compared with these per-window server ratios.")
	f.PossibleExplanations = []string{
		"Hypothesis (untested): a dependency the route calls began failing; no dependency measurements were collected.",
		"Hypothesis (untested): the test data changed during the run (for example stock running out); the analysis has no application data.",
		"Hypothesis (untested): the service rejects requests under sustained load; no resource measurements were collected.",
	}
	f.NextExperiment = fmt.Sprintf("%s. Check that %s's 5xx ratio rises in the later windows of most runs, and collect the service's logs for windows %s to see which errors were returned.",
		in.repeatAdvice(), in.routeName(k), windowList(r.a.ComparisonWindows))
	return f
}

func (in *investigator) latencyFinding(k routeKey, r, thr result) Finding {
	f := in.newFinding(FindingLatencyDegradation, DesignationObservation, k, r.change.ThresholdMultiple, r)
	b, c := r.a.Baseline, r.a.Comparison
	f.Symptom = fmt.Sprintf("%s: the estimated server-side p95 rose from ~%s to ~%s (%s)",
		in.routeName(k), seconds(b.Value), seconds(c.Value), percent(*r.change.Relative))
	f.Explanation = fmt.Sprintf("Reported because %s, comparing %s.", r.a.Reason, splitText(r))
	if thr.a.Change != nil && thr.a.Change.Relative != nil && *thr.a.Change.Relative >= in.cfg.ThroughputDecrease {
		f.Contradictions = append(f.Contradictions, fmt.Sprintf("the request rate rose by %s over %s: more load can raise latency without the service degrading",
			percent(*thr.a.Change.Relative), splitText(thr)))
	}
	if bm, cm, ok := means(r); ok && cm < bm {
		f.Contradictions = append(f.Contradictions, fmt.Sprintf("the mean latency fell from %s to %s while the p95 estimate rose", seconds(bm), seconds(cm)))
	}
	f.Limitations = append(f.Limitations,
		fmt.Sprintf("p95 values are estimates interpolated within histogram buckets: the true p95s lie between %s and %s in the baseline and between %s and %s in the comparison.",
			seconds(*b.BucketLowerSeconds), seconds(*b.BucketUpperSeconds), seconds(*c.BucketLowerSeconds), seconds(*c.BucketUpperSeconds)),
		"Server-side latency excludes network and client time; k6's client-side latency covers the whole run and is not compared.")
	f.PossibleExplanations = []string{
		"Hypothesis (untested): requests wait for a contended resource (such as CPU, a lock, a connection pool, or a downstream call); none of these was measured.",
		"Hypothesis (untested): work per request grew during the run (for example as stored data grew); the analysis has no application data.",
	}
	f.NextExperiment = fmt.Sprintf("%s. Check that %s's p95 rises in the later windows of most runs; then run with half and with twice the VUs to see whether p95 follows the load, and add histogram buckets around %s to narrow the estimate.",
		in.repeatAdvice(), in.routeName(k), seconds(c.Value))
	return f
}

func means(r result) (float64, float64, bool) {
	var b, c []float64
	for _, p := range r.base {
		if p.mean != nil {
			b = append(b, *p.mean)
		}
	}
	for _, p := range r.comp {
		if p.mean != nil {
			c = append(c, *p.mean)
		}
	}
	if len(b) == 0 || len(c) == 0 {
		return 0, 0, false
	}
	return median(b), median(c), true
}

func (in *investigator) throughputFinding(k routeKey, r, lat result) Finding {
	f := in.newFinding(FindingThroughputDegradation, DesignationObservation, k, r.change.ThresholdMultiple, r)
	b, c := r.a.Baseline, r.a.Comparison
	f.Symptom = fmt.Sprintf("%s: completed requests fell from %s to %s (%s)",
		in.routeName(k), rate(b.Value), rate(c.Value), percent(*r.change.Relative))
	f.Explanation = fmt.Sprintf("Reported because %s, comparing %s. Rates are counter increases divided by each window's measured length.", r.a.Reason, splitText(r))
	if lat.a.Status == StatusNoMaterialChange {
		f.Contradictions = append(f.Contradictions, "p95 latency did not rise materially ("+lat.a.Reason+"): a slower service would usually raise latency, so the load generator may simply have sent fewer requests")
	}
	f.Limitations = append(f.Limitations,
		"The server's request rate depends on the load generator: with a fixed number of VUs it falls when responses slow down, and also when the client itself slows down. The analysis cannot tell these apart.")
	f.PossibleExplanations = []string{
		"Hypothesis (untested): the load generator sent fewer requests (k6 reports only whole-run totals, so its per-window rate is unknown).",
		"Hypothesis (untested): the service's capacity fell during the run; no resource measurements were collected.",
	}
	f.NextExperiment = fmt.Sprintf("%s. Check that %s's request rate falls in the later windows of most runs; then repeat with half the VUs: if the decline disappears at lower load, a capacity limit becomes a hypothesis worth testing with resource measurements, and if it persists, examine the load generator.",
		in.repeatAdvice(), in.routeName(k))
	return f
}

func (in *investigator) combinedFinding(k routeKey, lat, thr result, latID, thrID string) Finding {
	multiple := math.Min(lat.change.ThresholdMultiple, thr.change.ThresholdMultiple)
	f := in.newFinding(FindingCombinedDegradation, DesignationHypothesis, k, multiple, lat, thr)
	f.Related = []string{latID, thrID}
	f.Symptom = fmt.Sprintf("%s: over the same windows the estimated p95 rose by %s while completed requests fell by %s",
		in.routeName(k), percent(*lat.change.Relative), percent(-*thr.change.Relative))
	f.Explanation = fmt.Sprintf("Reported because both the latency and the throughput findings hold over the same windows (%s). Ranked by the smaller of the two threshold multiples.", splitText(lat))
	f.Limitations = append(f.Limitations,
		"The two changes are correlated in time; that does not show that one causes the other, or that they share a cause.",
		"With a fixed number of VUs, higher latency lowers the request rate by itself, so the two changes may be one effect seen twice.")
	f.PossibleExplanations = []string{
		"Hypothesis (untested): a resource the route needs is saturated; resource measurements (such as CPU or connection pools) would be needed to test it, and none was collected.",
	}
	f.NextExperiment = fmt.Sprintf("%s. Check that both changes recur together; then run at several VU counts (for example half, the same, and twice) and see whether the request rate stops rising while p95 keeps rising, which would make a saturation hypothesis worth testing with resource measurements.",
		in.repeatAdvice())
	return f
}

// insufficientFinding abstains for the route's signals that could not be
// compared.
func (in *investigator) insufficientFinding(k routeKey, rs map[string]result) (Finding, bool) {
	f := Finding{
		ID: findingID(FindingInsufficientEvidence, k), Type: FindingInsufficientEvidence, Designation: DesignationAbstention,
		Method: clean(k.method), Route: clean(k.route), Changes: []SignalChange{},
		BaselineWindows: []int{}, ComparisonWindows: []int{}, Evidence: []EvidenceRef{},
		Contradictions: []string{}, Limitations: []string{"No conclusion is drawn for these signals: a missing finding here is not evidence of stable performance."},
		PossibleExplanations: []string{}, Gaps: []SignalGap{},
		Ranking: Ranking{TypePriority: typePriority[FindingInsufficientEvidence], Rule: RankingRule},
	}
	var reasons []string
	fewWindows, lowTraffic, unavailable := false, false, false
	for _, signal := range []string{SignalThroughput, SignalServerErrorRatio, SignalP95Latency} {
		r := rs[signal]
		if r.a.Status != StatusInsufficientEvidence {
			continue
		}
		f.Gaps = append(f.Gaps, SignalGap{Signal: signal, Reason: r.a.Reason, Gaps: r.a.Gaps})
		reasons = append(reasons, signal+": "+r.a.Reason)
		switch {
		case strings.Contains(r.a.Reason, "usable window"):
			fewWindows = true
		default:
			lowTraffic = true
		}
		if len(r.a.Gaps) > 0 {
			unavailable = true
		}
	}
	if len(f.Gaps) == 0 {
		return Finding{}, false
	}
	f.Symptom = fmt.Sprintf("%s: %d of 3 signals could not be compared", in.routeName(k), len(f.Gaps))
	f.Explanation = "Abstained because " + strings.Join(reasons, "; ") + "."

	// Point at the analysis warnings about this route, and at the excluded
	// windows in which it was measured.
	cfg := in.a.Config
	for i, w := range in.a.Warnings {
		if len(f.Evidence) >= maxEvidencePerGroup {
			break
		}
		if w.Labels != nil && (cfg.MethodLabel == "" || w.Labels[cfg.MethodLabel] == k.method) && (cfg.RouteLabel == "" || w.Labels[cfg.RouteLabel] == k.route) {
			f.Evidence = append(f.Evidence, EvidenceRef{Pointer: fmt.Sprintf("/analysis/warnings/%d", i), Window: w.Window, Role: "warning", Metric: clean(w.Metric)})
		}
	}
	for _, at := range in.positions[k] {
		if len(f.Evidence) >= maxEvidencePerGroup {
			break
		}
		if !in.comparable[at.window] {
			f.Evidence = append(f.Evidence, EvidenceRef{Pointer: fmt.Sprintf("%s/routes/%d", windowPointer(at.window), at.route),
				Window: in.a.Windows[at.window].Index, Role: "excluded"})
		}
	}

	var steps []string
	if fewWindows {
		steps = append(steps, in.repeatAdvice()+" (fewer windows than needed were usable)")
	}
	if lowTraffic {
		steps = append(steps, fmt.Sprintf("raise the VUs or the duration so each side has at least %g requests", in.cfg.MinRequests))
	}
	if unavailable {
		steps = append(steps, "fix the causes listed in the gaps (for example keep the target running so counters do not reset, keep the metric names and histogram buckets fixed, and make sure every scrape succeeds) before re-running")
	}
	f.NextExperiment = capitalize(strings.Join(steps, "; ")) + "."
	return f, true
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// rank orders findings by RankingRule and numbers them from 1.
func rank(fs []Finding) {
	sevOrder := map[string]int{SeverityMajor: 0, SeverityModerate: 1, "": 2}
	slices.SortStableFunc(fs, func(x, y Finding) int {
		return cmp.Or(
			cmp.Compare(boolInt(x.Type == FindingInsufficientEvidence), boolInt(y.Type == FindingInsufficientEvidence)),
			cmp.Compare(sevOrder[x.Severity], sevOrder[y.Severity]),
			cmp.Compare(x.Ranking.TypePriority, y.Ranking.TypePriority),
			cmp.Compare(y.Ranking.ThresholdMultiple, x.Ranking.ThresholdMultiple),
			cmp.Compare(y.Ranking.Windows, x.Ranking.Windows),
			cmp.Compare(y.Ranking.Requests, x.Ranking.Requests),
			strings.Compare(x.Method, y.Method),
			strings.Compare(x.Route, y.Route),
			strings.Compare(x.ID, y.ID),
		)
	})
	for i := range fs {
		fs[i].Rank = i + 1
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func outcome(inv Investigation) (string, string) {
	changes, abstentions := 0, 0
	for _, f := range inv.Findings {
		if f.Type == FindingInsufficientEvidence {
			abstentions++
		} else {
			changes++
		}
	}
	compared, inconclusive := 0, 0
	for _, a := range inv.Assessments {
		if a.Status != StatusInsufficientEvidence {
			compared++
		}
		if a.Status == StatusInconclusive {
			inconclusive++
		}
	}
	switch {
	case changes > 0:
		return OutcomeChangesObserved, fmt.Sprintf("%d finding(s) of observed change and %d route(s) with insufficient evidence; these are observations and hypotheses to test, not proven root causes.",
			changes, abstentions)
	case compared > 0:
		return OutcomeNoMaterialChange, fmt.Sprintf("%d signal(s) compared without a material change (%d inconclusive) and %d route(s) with insufficient evidence; this run shows no material change, which does not prove there is none.",
			compared, inconclusive, abstentions)
	case len(inv.Assessments) == 0:
		return OutcomeInsufficientEvidence, "The analysis has no route signals, so nothing can be compared; the investigation abstains."
	default:
		return OutcomeInsufficientEvidence, fmt.Sprintf("No signal of any route could be compared (%d of %d windows are comparable); the investigation abstains.",
			len(inv.Windows.Comparable), inv.Source.Windows)
	}
}

func (in *investigator) limitations() []string {
	l := []string{
		"Findings are observations or hypotheses from one experiment run, never proven root causes; correlation between signals does not show causation.",
		"Only request counts, status codes, and latency histograms are analysed: there are no CPU, memory, database, or network measurements, so none of these can be identified as a cause.",
		"Changes are judged against the fixed thresholds in config; no statistical significance test is made.",
		"The baseline is the earlier half of each signal's usable windows and the comparison the later half; windows that span the workload's start or end, or missed scrapes, are not compared.",
		"p95 values are histogram estimates; a latency change is reported only when the later windows' buckets lie entirely above the earlier windows'.",
		"Client-side (k6) measurements cover the whole run and include network time, so they are not compared with the server's per-window signals.",
	}
	e := in.a.Experiment
	if e.Status != experiment.StatusPassed {
		l = append(l, fmt.Sprintf("The experiment's status is %s, so its workload or telemetry did not fully meet its criteria.", e.Status))
	}
	if e.Completeness != experiment.Complete {
		l = append(l, fmt.Sprintf("Telemetry is %s: some windows may be missing or longer than one interval.", e.Completeness))
	}
	if in.cfg.MinWindows == 1 {
		l = append(l, "With one window per side, a single noisy window can produce a finding.")
	}
	return l
}

// clean redacts URL credentials and removes control characters from text
// copied from the analysis, and bounds its length.
func clean(s string) string {
	s = workload.RedactText(s, "")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	if r := []rune(s); len(r) > maxTextLen {
		s = string(r[:maxTextLen]) + "..."
	}
	return s
}

func cleanSeries(series []map[string]string) []map[string]string {
	out := make([]map[string]string, 0, len(series))
	for _, l := range series {
		c := make(map[string]string, len(l))
		for k, v := range l {
			c[clean(k)] = clean(v)
		}
		out = append(out, c)
	}
	return out
}

func percent(x float64) string { return fmt.Sprintf("%+.1f%%", x*100) }

func points(x float64) string { return fmt.Sprintf("%.2f", x*100) }

func signedPoints(x float64) string { return fmt.Sprintf("%+.2f", x*100) }

func ratio(x float64) string { return fmt.Sprintf("%.2f%%", x*100) }

func rate(x float64) string { return fmt.Sprintf("%.1f requests/s", x) }

func seconds(s float64) string {
	if s < 1 {
		return fmt.Sprintf("%.3f ms", s*1000)
	}
	return fmt.Sprintf("%.3f s", s)
}

// windowList writes window indexes, joining runs: "2-4, 7".
func windowList(ws []int) string {
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
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}
