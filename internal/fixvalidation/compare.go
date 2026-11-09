package fixvalidation

import (
	"cmp"
	"fmt"
	"math"
	"net/url"
	"slices"
	"strings"

	"github.com/RajK158/ruptiq/internal/analysis"
	"github.com/RajK158/ruptiq/internal/experiment"
	"github.com/RajK158/ruptiq/internal/workload"
)

const (
	clientNote = "k6 client-side measurement over the whole run. Not used in the server-side comparison."
	maxText    = 500
)

// Compare reports how after differs from before. before and after are decoded
// analyses, not experiment reports. cfg must already be valid; an invalid
// config is returned as an error and is not written into a result.
func Compare(before, after analysis.Analysis, cfg Config) (Validation, error) {
	if err := cfg.Validate(); err != nil {
		return Validation{}, err
	}
	b, a := prepare(before, SideBefore), prepare(after, SideAfter)
	v := Validation{
		Config:         cfg,
		Comparability:  Comparability{Reasons: []string{}, Warnings: []string{}},
		Before:         b.source,
		After:          a.source,
		Routes:         []RouteComparison{},
		Ranked:         []RankedChange{},
		Limitations:    limitations(),
		NextExperiment: nextExperiment(cfg),
	}
	reasons := mismatch(before, after)
	warns := softWarnings(before, after)
	v.Comparability.Warnings = warns
	if len(reasons) > 0 {
		v.Comparability.Comparable = false
		v.Comparability.Reasons = reasons
		v.Outcome = OutcomeNotComparable
		v.Summary = "The experiments are not comparable: " + strings.Join(reasons, "; ") + ". No route was treated as equivalent."
		return v, nil
	}
	v.Comparability.Comparable = true
	v.Comparability.Reasons = []string{}

	keys := routeKeys(before, after)
	for _, k := range keys {
		v.Routes = append(v.Routes, compareRoute(b, a, k, cfg))
	}
	v.Ranked = rank(v.Routes)
	v.Outcome, v.Summary = overall(v)
	return v, nil
}

func limitations() []string {
	return []string{
		"An observed difference is one pair of saved runs. It does not show that the difference is repeatable, and it does not show that a code change caused it.",
		"No statistical significance test is performed, and no confidence percentage is reported.",
		"Matching VUs, duration, sampling interval, and metric configuration does not mean the machines, background load, or caches matched.",
		"Saved analyses redact URL user information and query strings. Those parts are not compared, so a matching target URL does not prove the original targets or workload parameters were the same.",
		"Server throughput is the total of per-window completed requests divided by the total elapsed time of those windows. Cumulative counters are not averaged, and per-window rates are not averaged.",
		"The 5xx ratio pools server errors and completed requests. 4xx responses are client errors and are kept separate.",
		"Latency is the median of per-window estimated p95s, not an experiment-wide p95. Overlapping histogram buckets are not called an improvement or a regression.",
		"k6 client measurements are reported separately and are not mixed into the server-side figures.",
		"A route missing from one experiment is not zero traffic.",
	}
}

func nextExperiment(cfg Config) string {
	return fmt.Sprintf("Repeat the before and after experiments at least twice more with the same target, %d or more eligible windows, and at least %g requests on the before side. One observed difference is not a reproducible result and is not proof of a cause.",
		cfg.MinWindows, cfg.MinRequests)
}

type routeKey struct {
	method, route string
}

type prepared struct {
	a        analysis.Analysis
	source   Source
	eligible map[int]bool // array position
}

func prepare(a analysis.Analysis, label string) prepared {
	p := prepared{a: a, eligible: map[int]bool{}}
	src := Source{
		Label:                 label,
		Status:                a.Experiment.Status,
		TelemetryCompleteness: a.Experiment.Completeness,
		TargetURL:             workload.RedactURL(a.Experiment.TargetURL),
		MetricsURL:            workload.RedactURL(a.Experiment.MetricsURL),
		VUs:                   a.Experiment.VUs,
		DurationSeconds:       finiteOrZero(a.Experiment.DurationSeconds),
		SampleIntervalSeconds: finiteOrZero(a.Experiment.SampleIntervalSeconds),
		RequestsCounter:       clean(a.Config.RequestsCounter),
		DurationHistogram:     clean(a.Config.DurationHistogram),
		MethodLabel:           clean(a.Config.MethodLabel),
		RouteLabel:            clean(a.Config.RouteLabel),
		StatusLabel:           clean(a.Config.StatusLabel),
		MinObservations:       a.Config.MinObservations,
		Windows:               len(a.Windows),
		EligibleWindows:       []int{},
		ExcludedWindows:       []ExcludedWindow{},
		Client: ClientSnapshot{
			Source:            clean(a.Client.Source),
			HTTPRequests:      a.Client.Metrics.HTTPRequests,
			RequestsPerSecond: finiteOrZero(a.Client.Metrics.RequestsPerSecond),
			FailureRate:       finiteOrZero(a.Client.Metrics.FailureRate),
			P95Millis:         finiteOrZero(a.Client.Metrics.LatencyMs.P95),
			Note:              clientNote,
		},
	}
	for i, w := range a.Windows {
		reason := ""
		switch {
		case w.From.Phase != experiment.DuringWorkload || w.To.Phase != experiment.DuringWorkload:
			reason = fmt.Sprintf("spans the workload boundary (samples #%d %s to #%d %s)", w.From.Sequence, w.From.Phase, w.To.Sequence, w.To.Phase)
		case len(w.MissedScrapes) > 0:
			reason = fmt.Sprintf("spans %d missed scrape(s), so it is not one sampling interval", len(w.MissedScrapes))
		case !finite(w.ElapsedSeconds) || w.ElapsedSeconds <= 0:
			reason = "elapsed time is not a positive finite number"
		}
		if reason != "" {
			src.ExcludedWindows = append(src.ExcludedWindows, ExcludedWindow{Window: w.Index, Reason: clean(reason), Pointer: windowPointer(i)})
			continue
		}
		p.eligible[i] = true
		src.EligibleWindows = append(src.EligibleWindows, w.Index)
	}
	p.source = src
	return p
}

func mismatch(before, after analysis.Analysis) []string {
	var reasons []string
	add := func(format string, args ...any) {
		reasons = append(reasons, fmt.Sprintf(format, args...))
	}
	b, a := before.Experiment, after.Experiment
	if !finite(b.DurationSeconds) || !finite(a.DurationSeconds) || !finite(b.SampleIntervalSeconds) || !finite(a.SampleIntervalSeconds) {
		add("duration or sampling interval is not a finite number")
	} else {
		if b.VUs != a.VUs {
			add("VUs differ (%d before, %d after)", b.VUs, a.VUs)
		}
		if !sameFloat(b.DurationSeconds, a.DurationSeconds) {
			add("configured duration differs (%.6gs before, %.6gs after)", b.DurationSeconds, a.DurationSeconds)
		}
		if !sameFloat(b.SampleIntervalSeconds, a.SampleIntervalSeconds) {
			add("sampling interval differs (%.6gs before, %.6gs after)", b.SampleIntervalSeconds, a.SampleIntervalSeconds)
		}
	}
	bt, at := workload.RedactURL(b.TargetURL), workload.RedactURL(a.TargetURL)
	if bt != at {
		add("target URL differs (%s before, %s after)", bt, at)
	}
	bc, ac := before.Config, after.Config
	if clean(bc.RequestsCounter) != clean(ac.RequestsCounter) || clean(bc.DurationHistogram) != clean(ac.DurationHistogram) {
		add("metric families differ (requests %q / %q, histogram %q / %q)",
			clean(bc.RequestsCounter), clean(ac.RequestsCounter), clean(bc.DurationHistogram), clean(ac.DurationHistogram))
	}
	if clean(bc.MethodLabel) != clean(ac.MethodLabel) || clean(bc.RouteLabel) != clean(ac.RouteLabel) || clean(bc.StatusLabel) != clean(ac.StatusLabel) {
		add("label configuration differs (method %q/%q, route %q/%q, status %q/%q)",
			clean(bc.MethodLabel), clean(ac.MethodLabel), clean(bc.RouteLabel), clean(ac.RouteLabel), clean(bc.StatusLabel), clean(ac.StatusLabel))
	}
	if bc.MinObservations != ac.MinObservations {
		add("minimum histogram observations differ (%d before, %d after)", bc.MinObservations, ac.MinObservations)
	}
	return reasons
}

func softWarnings(before, after analysis.Analysis) []string {
	var out []string
	b, a := before.Experiment, after.Experiment
	if b.Status != a.Status {
		out = append(out, fmt.Sprintf("experiment status differs (%s before, %s after); the comparison still uses only eligible server windows", b.Status, a.Status))
	}
	if b.Completeness != a.Completeness {
		out = append(out, fmt.Sprintf("telemetry completeness differs (%s before, %s after)", b.Completeness, a.Completeness))
	}
	if workload.RedactURL(b.MetricsURL) != workload.RedactURL(a.MetricsURL) {
		out = append(out, "metrics URL differs; metric family names still match")
	}
	if urlHidesParts(b.TargetURL) || urlHidesParts(a.TargetURL) {
		out = append(out, "target URL user information or query string is redacted and was not compared, so a URL match does not prove the original targets were the same")
	}
	if before.Client.Metrics.HTTPRequests != after.Client.Metrics.HTTPRequests || before.Client.Passed != after.Client.Passed {
		out = append(out, "k6 client results differ; they are not part of the server-side conclusion")
	}
	if len(out) == 0 {
		return []string{}
	}
	return out
}

func routeKeys(before, after analysis.Analysis) []routeKey {
	seen := map[routeKey]bool{}
	add := func(a analysis.Analysis) {
		for _, w := range a.Windows {
			for _, rs := range w.Routes {
				seen[routeKey{rs.Method, rs.Route}] = true
			}
		}
	}
	add(before)
	add(after)
	keys := make([]routeKey, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(x, y routeKey) int {
		return cmp.Or(strings.Compare(x.method, y.method), strings.Compare(x.route, y.route))
	})
	return keys
}

func compareRoute(before, after prepared, k routeKey, cfg Config) RouteComparison {
	inB, inA := hasRoute(before.a, k), hasRoute(after.a, k)
	rc := RouteComparison{
		Method: clean(k.method), Route: clean(k.route),
		Evidence: []EvidenceRef{}, Limitations: []string{},
		NextExperiment: nextExperiment(cfg),
	}
	switch {
	case inB && inA:
		rc.PresentIn = PresentBoth
	case inB:
		rc.PresentIn = PresentBefore
	default:
		rc.PresentIn = PresentAfter
	}
	if !inB || !inA {
		rc.Outcome = OutcomeInsufficient
		which := SideBefore
		side := before
		if !inB {
			which = SideAfter
			side = after
		}
		rc.Summary = fmt.Sprintf("%s %s is present only in the %s experiment. A missing route is not zero traffic.", rc.Method, rc.Route, which)
		rc.Limitations = append(rc.Limitations, rc.Summary)
		rc.Evidence = routeEvidence(side, which, k)
		return rc
	}

	tp := compareThroughput(before, after, k, cfg)
	er := compareErrors(before, after, k, cfg)
	lat := compareLatency(before, after, k, cfg)
	rc.Throughput, rc.ServerErrors, rc.Latency = &tp, &er, &lat
	rc.Evidence = appendUnique(rc.Evidence, tp.Evidence)
	rc.Evidence = appendUnique(rc.Evidence, er.Evidence)
	rc.Evidence = appendUnique(rc.Evidence, lat.Evidence)
	rc.Outcome = rollup(tp.Status, er.Status, lat.Status)
	rc.Summary = routeSummary(rc)
	if tp.Status == StatusInsufficient || er.Status == StatusInsufficient || lat.Status == StatusInsufficient || lat.Status == StatusIncompatible {
		rc.Limitations = append(rc.Limitations, "At least one signal for this route could not be judged. The other signals are still compared.")
	}
	if lat.Status == StatusInconclusive {
		rc.Limitations = append(rc.Limitations, "The p95 bucket ranges overlap, so the latency direction is not claimed.")
	}
	return rc
}

func routeSummary(rc RouteComparison) string {
	return fmt.Sprintf("%s %s: %s.", rc.Method, rc.Route, strings.ReplaceAll(rc.Outcome, "_", " "))
}

type reqPoint struct {
	pos, route, window                int
	requests, errors, client, elapsed float64
}

type latPoint struct {
	pos, route, window int
	value, lo, hi, obs float64
	buckets            []analysis.Bucket
}

func compareThroughput(before, after prepared, k routeKey, cfg Config) SignalComparison {
	s := newSignal(SignalThroughput)
	bp, be := requestPoints(before, k, true)
	ap, ae := requestPoints(after, k, true)
	s.ExcludedBefore, s.ExcludedAfter = be, ae
	s.BeforeWindows, s.AfterWindows = windowsOfReq(bp), windowsOfReq(ap)
	s.Evidence = reqEvidence(SideBefore, before, bp, "/requests")
	s.Evidence = append(s.Evidence, reqEvidence(SideAfter, after, ap, "/requests")...)
	s.Statistic = "total completed requests divided by total elapsed time of eligible windows"
	if len(bp) < cfg.MinWindows || len(ap) < cfg.MinWindows {
		s.Status = StatusInsufficient
		s.Reason = fmt.Sprintf("%d eligible before window(s) and %d after; at least %d are required on each side", len(bp), len(ap), cfg.MinWindows)
		fillReqMeasures(&s, bp, ap, s.Statistic)
		return s
	}
	b, a := poolReq(bp), poolReq(ap)
	fillReqMeasures(&s, bp, ap, s.Statistic)
	if b.requests == 0 && a.requests == 0 {
		s.Status = StatusNoMaterialChange
		zero := 0.0
		s.Absolute = &zero
		s.Reason = "Both sides completed no requests in the eligible windows. The absolute change is 0 requests/s, and no relative change is defined. This is not treated as an improvement."
		return s
	}
	if b.requests < cfg.MinRequests {
		s.Status = StatusInsufficient
		s.Reason = fmt.Sprintf("the before side completed %g requests, fewer than %g, so a relative change is not judged", b.requests, cfg.MinRequests)
		return s
	}
	if b.rate <= 0 || !finite(b.rate, a.rate) {
		s.Status = StatusInsufficient
		s.Reason = "throughput is not a positive finite number, so no relative change is defined"
		return s
	}
	abs := a.rate - b.rate
	rel := abs / b.rate
	s.Absolute, s.Relative = &abs, &rel
	switch {
	case math.Abs(rel) < cfg.ThroughputChange:
		s.Status = StatusNoMaterialChange
		s.Reason = fmt.Sprintf("throughput changed by %s, inside the ±%s threshold", percent(rel), percent(cfg.ThroughputChange))
	case rel > 0:
		s.Status = StatusImprovement
		s.Reason = fmt.Sprintf("throughput rose from %s to %s (%s)", rate(b.rate), rate(a.rate), percent(rel))
	default:
		s.Status = StatusRegression
		s.Reason = fmt.Sprintf("throughput fell from %s to %s (%s)", rate(b.rate), rate(a.rate), percent(rel))
	}
	return s
}

func compareErrors(before, after prepared, k routeKey, cfg Config) SignalComparison {
	s := newSignal(SignalServerErrors)
	bp, be := requestPoints(before, k, false)
	ap, ae := requestPoints(after, k, false)
	s.ExcludedBefore, s.ExcludedAfter = be, ae
	s.BeforeWindows, s.AfterWindows = windowsOfReq(bp), windowsOfReq(ap)
	s.Evidence = reqEvidence(SideBefore, before, bp, "/requests")
	s.Evidence = append(s.Evidence, reqEvidence(SideAfter, after, ap, "/requests")...)
	s.Statistic = "pooled 5xx responses divided by pooled completed requests; 4xx responses are not included"
	if len(bp) < cfg.MinWindows || len(ap) < cfg.MinWindows {
		s.Status = StatusInsufficient
		s.Reason = fmt.Sprintf("%d eligible before window(s) and %d after; at least %d are required on each side", len(bp), len(ap), cfg.MinWindows)
		fillErrMeasures(&s, bp, ap, s.Statistic)
		return s
	}
	b, a := poolReq(bp), poolReq(ap)
	fillErrMeasures(&s, bp, ap, s.Statistic)
	if b.requests == 0 || a.requests == 0 {
		s.Status = StatusInsufficient
		s.Reason = fmt.Sprintf("the 5xx ratio is undefined because a side completed no requests (before %g, after %g). 4xx responses are not used in its place", b.requests, a.requests)
		return s
	}
	if b.requests < cfg.MinRequests || a.requests < cfg.MinRequests {
		s.Status = StatusInsufficient
		s.Reason = fmt.Sprintf("completed requests are %g before and %g after; each side needs at least %g before the 5xx ratio is judged", b.requests, a.requests, cfg.MinRequests)
		return s
	}
	pp := (a.ratio - b.ratio) * 100
	s.PercentagePoints = &pp
	abs := a.ratio - b.ratio
	s.Absolute = &abs
	if math.Abs(abs) < cfg.ErrorRatioIncrease {
		s.Status = StatusNoMaterialChange
		s.Reason = fmt.Sprintf("the 5xx ratio changed by %s percentage points, inside the ±%s point threshold. 4xx responses are not counted as server errors", signedPoints(abs), points(cfg.ErrorRatioIncrease))
		return s
	}
	if a.ratio > b.ratio {
		if a.errors < cfg.MinServerErrors {
			s.Status = StatusInsufficient
			s.Reason = fmt.Sprintf("the after side has %g 5xx responses, fewer than %g", a.errors, cfg.MinServerErrors)
			return s
		}
		if b.ratio > 0 && a.ratio < b.ratio*cfg.ErrorRatioFactor {
			s.Status = StatusNoMaterialChange
			s.Reason = fmt.Sprintf("the 5xx ratio rose by %s points but is less than %g times the before ratio", signedPoints(abs), cfg.ErrorRatioFactor)
			return s
		}
		s.Status = StatusRegression
		s.Reason = fmt.Sprintf("the 5xx ratio rose from %s to %s (%s points)", ratioText(b.ratio), ratioText(a.ratio), signedPoints(abs))
		return s
	}
	if b.errors < cfg.MinServerErrors {
		s.Status = StatusInsufficient
		s.Reason = fmt.Sprintf("the before side has %g 5xx responses, fewer than %g, so a fall in the ratio is not judged", b.errors, cfg.MinServerErrors)
		return s
	}
	if a.ratio > 0 && b.ratio < a.ratio*cfg.ErrorRatioFactor {
		s.Status = StatusNoMaterialChange
		s.Reason = fmt.Sprintf("the 5xx ratio fell by %s points but the before ratio is less than %g times the after ratio", signedPoints(-abs), cfg.ErrorRatioFactor)
		return s
	}
	s.Status = StatusImprovement
	s.Reason = fmt.Sprintf("the 5xx ratio fell from %s to %s (%s points)", ratioText(b.ratio), ratioText(a.ratio), signedPoints(abs))
	return s
}

func compareLatency(before, after prepared, k routeKey, cfg Config) SignalComparison {
	s := newSignal(SignalLatency)
	bp, be := latencyPoints(before, k)
	ap, ae := latencyPoints(after, k)
	s.ExcludedBefore, s.ExcludedAfter = be, ae
	s.BeforeWindows, s.AfterWindows = windowsOfLat(bp), windowsOfLat(ap)
	s.Evidence = latEvidence(SideBefore, bp)
	s.Evidence = append(s.Evidence, latEvidence(SideAfter, ap)...)
	s.Statistic = "median of per-window estimated p95s, not an experiment-wide p95"
	if len(bp) < cfg.MinWindows || len(ap) < cfg.MinWindows {
		s.Status = StatusInsufficient
		s.Reason = fmt.Sprintf("%d eligible before window(s) and %d after; at least %d are required on each side", len(bp), len(ap), cfg.MinWindows)
		fillLatMeasures(&s, bp, ap)
		return s
	}
	if !sameHistogram(bp, ap) {
		s.Status = StatusIncompatible
		s.Reason = "histogram bucket boundaries differ between the eligible windows, so the p95 estimates are not on the same scale"
		fillLatMeasures(&s, bp, ap)
		return s
	}
	b, a := poolLat(bp), poolLat(ap)
	fillLatMeasures(&s, bp, ap)
	if b.obs < cfg.MinRequests || a.obs < cfg.MinRequests {
		s.Status = StatusInsufficient
		s.Reason = fmt.Sprintf("p95 observations are %g before and %g after; each side needs at least %g", b.obs, a.obs, cfg.MinRequests)
		return s
	}
	if b.value <= 0 || !finite(b.value, a.value) {
		s.Status = StatusInsufficient
		s.Reason = "a median p95 estimate is not a positive finite number, so no relative change is defined"
		return s
	}
	abs := a.value - b.value
	rel := abs / b.value
	s.Absolute, s.Relative = &abs, &rel
	if math.Abs(rel) < cfg.LatencyChange {
		s.Status = StatusNoMaterialChange
		s.Reason = fmt.Sprintf("the median per-window p95 changed by %s, inside the ±%s threshold. This is not an experiment-wide p95", percent(rel), percent(cfg.LatencyChange))
		return s
	}
	if overlaps(b.lo, b.hi, a.lo, a.hi) {
		s.Status = StatusInconclusive
		s.Reason = fmt.Sprintf("the median per-window p95 changed by %s, but the bucket ranges [%s, %s] and [%s, %s] overlap, so the direction is not claimed",
			percent(rel), seconds(b.lo), seconds(b.hi), seconds(a.lo), seconds(a.hi))
		return s
	}
	if rel < 0 {
		s.Status = StatusImprovement
		s.Reason = fmt.Sprintf("the median per-window p95 fell from %s to %s (%s), and the bucket ranges do not overlap", seconds(b.value), seconds(a.value), percent(rel))
		return s
	}
	s.Status = StatusRegression
	s.Reason = fmt.Sprintf("the median per-window p95 rose from %s to %s (%s), and the bucket ranges do not overlap", seconds(b.value), seconds(a.value), percent(rel))
	return s
}

func newSignal(name string) SignalComparison {
	return SignalComparison{
		Signal: name, BeforeWindows: []int{}, AfterWindows: []int{},
		ExcludedBefore: []ExcludedWindow{}, ExcludedAfter: []ExcludedWindow{}, Evidence: []EvidenceRef{},
	}
}

func requestPoints(p prepared, k routeKey, throughput bool) ([]reqPoint, []ExcludedWindow) {
	excluded := append([]ExcludedWindow{}, p.source.ExcludedWindows...)
	var points []reqPoint
	for i, w := range p.a.Windows {
		if !p.eligible[i] {
			continue
		}
		ri, ok := findRoute(w, k)
		if !ok {
			excluded = append(excluded, ExcludedWindow{Window: w.Index, Reason: "the route has no series in this window", Pointer: windowPointer(i)})
			continue
		}
		rs := w.Routes[ri]
		if rs.Requests == nil {
			reason := rs.RequestsUnavailable
			if reason == "" {
				reason = "request measurements are unavailable"
			}
			excluded = append(excluded, ExcludedWindow{Window: w.Index, Reason: clean(reason), Pointer: routePointer(i, ri)})
			continue
		}
		kinds := []string{analysis.WarnCounterReset, analysis.WarnSeriesAppeared, analysis.WarnSeriesDisappeared, analysis.WarnFamilyMissing}
		if msg := warningBlocks(p.a, w.Index, p.a.Config.RequestsCounter, k, kinds); msg != "" {
			excluded = append(excluded, ExcludedWindow{Window: w.Index, Reason: "incompatible counter measurement: " + clean(msg), Pointer: routePointer(i, ri)})
			continue
		}
		r := rs.Requests
		if !finite(r.Requests, r.ElapsedSeconds, r.ServerErrors, r.ClientErrors, r.PerSecond) || r.ElapsedSeconds <= 0 || r.Requests < 0 || r.ServerErrors < 0 || r.ClientErrors < 0 {
			excluded = append(excluded, ExcludedWindow{Window: w.Index, Reason: "request measurement is not finite and non-negative", Pointer: routePointer(i, ri)})
			continue
		}
		if !throughput && r.Requests > 0 && r.ServerErrorRatio == nil {
			excluded = append(excluded, ExcludedWindow{Window: w.Index, Reason: clean(orText(r.ServerErrorRatioUnavailable, "5xx ratio is unavailable")), Pointer: routePointer(i, ri)})
			continue
		}
		points = append(points, reqPoint{pos: i, route: ri, window: w.Index, requests: r.Requests, errors: r.ServerErrors, client: r.ClientErrors, elapsed: r.ElapsedSeconds})
	}
	return points, excluded
}

func latencyPoints(p prepared, k routeKey) ([]latPoint, []ExcludedWindow) {
	excluded := append([]ExcludedWindow{}, p.source.ExcludedWindows...)
	var points []latPoint
	for i, w := range p.a.Windows {
		if !p.eligible[i] {
			continue
		}
		ri, ok := findRoute(w, k)
		if !ok {
			excluded = append(excluded, ExcludedWindow{Window: w.Index, Reason: "the route has no series in this window", Pointer: windowPointer(i)})
			continue
		}
		rs := w.Routes[ri]
		if rs.Latency == nil {
			excluded = append(excluded, ExcludedWindow{Window: w.Index, Reason: clean(orText(rs.LatencyUnavailable, "latency measurements are unavailable")), Pointer: routePointer(i, ri)})
			continue
		}
		kinds := []string{analysis.WarnHistogramReset, analysis.WarnBucketsChanged, analysis.WarnFamilyMissing}
		if msg := warningBlocks(p.a, w.Index, p.a.Config.DurationHistogram, k, kinds); msg != "" {
			excluded = append(excluded, ExcludedWindow{Window: w.Index, Reason: "incompatible histogram: " + clean(msg), Pointer: routePointer(i, ri)})
			continue
		}
		l := rs.Latency
		if l.P95.Seconds == nil || l.P95.BucketLowerSeconds == nil || l.P95.BucketUpperSeconds == nil {
			excluded = append(excluded, ExcludedWindow{Window: w.Index, Reason: clean(orText(l.P95.Unavailable, "p95 estimate is unavailable")), Pointer: routePointer(i, ri) + "/latency"})
			continue
		}
		est, lo, hi := *l.P95.Seconds, *l.P95.BucketLowerSeconds, *l.P95.BucketUpperSeconds
		if math.IsNaN(est) || math.IsNaN(lo) || math.IsNaN(hi) || math.IsInf(est, 0) || math.IsInf(lo, 0) || math.IsInf(hi, -1) || est < lo || est > hi || est < 0 {
			excluded = append(excluded, ExcludedWindow{Window: w.Index, Reason: "p95 estimate is not a finite number inside its bucket", Pointer: routePointer(i, ri) + "/latency/p95"})
			continue
		}
		if len(l.Buckets) == 0 {
			excluded = append(excluded, ExcludedWindow{Window: w.Index, Reason: "histogram buckets are missing", Pointer: routePointer(i, ri) + "/latency"})
			continue
		}
		points = append(points, latPoint{pos: i, route: ri, window: w.Index, value: est, lo: lo, hi: hi, obs: float64(l.Observations), buckets: l.Buckets})
	}
	return points, excluded
}

func warningBlocks(a analysis.Analysis, window int, metric string, k routeKey, kinds []string) string {
	for _, w := range a.Warnings {
		if w.Window != window || w.Metric != metric || !slices.Contains(kinds, w.Kind) {
			continue
		}
		if m := a.Config.MethodLabel; m != "" && w.Labels[m] != "" && w.Labels[m] != k.method {
			continue
		}
		if r := a.Config.RouteLabel; r != "" && w.Labels[r] != "" && w.Labels[r] != k.route {
			continue
		}
		if w.Message != "" {
			return w.Message
		}
		return w.Kind
	}
	return ""
}

type reqPool struct {
	requests, errors, client, elapsed, rate, ratio float64
}

func poolReq(ps []reqPoint) reqPool {
	var p reqPool
	for _, x := range ps {
		p.requests += x.requests
		p.errors += x.errors
		p.client += x.client
		p.elapsed += x.elapsed
	}
	if p.elapsed > 0 && finite(p.elapsed, p.requests) {
		p.rate = p.requests / p.elapsed
	}
	if p.requests > 0 && finite(p.requests, p.errors) {
		p.ratio = p.errors / p.requests
	}
	return p
}

type latPool struct {
	value, lo, hi, obs float64
	windows            int
}

func poolLat(ps []latPoint) latPool {
	if len(ps) == 0 {
		return latPool{}
	}
	vals := make([]float64, len(ps))
	lo, hi := ps[0].lo, ps[0].hi
	var obs float64
	for i, p := range ps {
		vals[i] = p.value
		obs += p.obs
		if p.lo < lo {
			lo = p.lo
		}
		if p.hi > hi {
			hi = p.hi
		}
	}
	return latPool{value: median(vals), lo: lo, hi: hi, obs: obs, windows: len(ps)}
}

func median(xs []float64) float64 {
	s := append([]float64(nil), xs...)
	slices.Sort(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

func sameHistogram(b, a []latPoint) bool {
	all := append(append([]latPoint{}, b...), a...)
	if len(all) == 0 {
		return true
	}
	for _, p := range all[1:] {
		if !sameBuckets(all[0].buckets, p.buckets) {
			return false
		}
	}
	return true
}

func sameBuckets(a, b []analysis.Bucket) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !sameFloat(float64(a[i].UpperBound), float64(b[i].UpperBound)) {
			return false
		}
	}
	return true
}

func overlaps(aLo, aHi, bLo, bHi float64) bool {
	return aLo <= bHi && bLo <= aHi
}

func fillReqMeasures(s *SignalComparison, b, a []reqPoint, stat string) {
	if len(b) == 0 && len(a) == 0 {
		return
	}
	bp, ap := poolReq(b), poolReq(a)
	if len(b) > 0 {
		s.Before = rateMeasure(bp, len(b), stat)
	}
	if len(a) > 0 {
		s.After = rateMeasure(ap, len(a), stat)
	}
}

func fillErrMeasures(s *SignalComparison, b, a []reqPoint, stat string) {
	if len(b) == 0 && len(a) == 0 {
		return
	}
	bp, ap := poolReq(b), poolReq(a)
	if len(b) > 0 {
		s.Before = ratioMeasure(bp, len(b), stat)
	}
	if len(a) > 0 {
		s.After = ratioMeasure(ap, len(a), stat)
	}
}

func fillLatMeasures(s *SignalComparison, b, a []latPoint) {
	if len(b) > 0 {
		s.Before = latMeasure(poolLat(b), latencyStatistic(len(b)))
	}
	if len(a) > 0 {
		s.After = latMeasure(poolLat(a), latencyStatistic(len(a)))
	}
}

func latencyStatistic(n int) string {
	text := fmt.Sprintf("median of %d per-window estimated p95s; not an experiment-wide p95", n)
	if n > 0 && n%2 == 0 {
		text += "; an even count uses the mean of the two central estimates"
	}
	return text
}

func rateMeasure(p reqPool, n int, stat string) *Measure {
	return &Measure{
		Value: p.rate, Unit: UnitRequestsPerSecond, Statistic: stat,
		Requests: p.requests, ElapsedSeconds: p.elapsed, Windows: n,
		ClientErrors: fptr(p.client),
	}
}

func ratioMeasure(p reqPool, n int, stat string) *Measure {
	value := 0.0
	if p.requests > 0 {
		value = p.ratio
	}
	return &Measure{
		Value: value, Unit: UnitRatio, Statistic: stat,
		Requests: p.requests, ServerErrors: fptr(p.errors), ClientErrors: fptr(p.client),
		ElapsedSeconds: p.elapsed, Windows: n,
	}
}

func latMeasure(p latPool, stat string) *Measure {
	obs := p.obs
	lo, hi := p.lo, p.hi
	return &Measure{
		Value: p.value, Unit: UnitSeconds, Statistic: stat,
		Requests: p.obs, ElapsedSeconds: 0, Observations: &obs, Windows: p.windows,
		Estimate: true, BucketLowerSeconds: &lo, BucketUpperSeconds: &hi,
	}
}

func windowsOfReq(ps []reqPoint) []int {
	out := make([]int, len(ps))
	for i, p := range ps {
		out[i] = p.window
	}
	return out
}

func windowsOfLat(ps []latPoint) []int {
	out := make([]int, len(ps))
	for i, p := range ps {
		out[i] = p.window
	}
	return out
}

func reqEvidence(side string, p prepared, ps []reqPoint, suffix string) []EvidenceRef {
	out := make([]EvidenceRef, len(ps))
	for i, pt := range ps {
		out[i] = EvidenceRef{Side: side, Window: pt.window, Pointer: routePointer(pt.pos, pt.route) + suffix, Role: RoleMeasurement}
	}
	return out
}

func latEvidence(side string, ps []latPoint) []EvidenceRef {
	out := make([]EvidenceRef, len(ps))
	for i, pt := range ps {
		out[i] = EvidenceRef{Side: side, Window: pt.window, Pointer: routePointer(pt.pos, pt.route) + "/latency/p95", Role: RoleMeasurement}
	}
	return out
}

func routeEvidence(p prepared, side string, k routeKey) []EvidenceRef {
	var out []EvidenceRef
	for i, w := range p.a.Windows {
		ri, ok := findRoute(w, k)
		if !ok {
			continue
		}
		out = append(out, EvidenceRef{Side: side, Window: w.Index, Pointer: routePointer(i, ri), Role: RoleMeasurement})
	}
	if out == nil {
		return []EvidenceRef{}
	}
	return out
}

func hasRoute(a analysis.Analysis, k routeKey) bool {
	for _, w := range a.Windows {
		if _, ok := findRoute(w, k); ok {
			return true
		}
	}
	return false
}

func findRoute(w analysis.Window, k routeKey) (int, bool) {
	for i, rs := range w.Routes {
		if rs.Method == k.method && rs.Route == k.route {
			return i, true
		}
	}
	return 0, false
}

func rollup(statuses ...string) string {
	imp, reg, judged := 0, 0, 0
	for _, s := range statuses {
		switch s {
		case StatusImprovement:
			imp++
			judged++
		case StatusRegression:
			reg++
			judged++
		case StatusNoMaterialChange, StatusInconclusive:
			judged++
		}
	}
	switch {
	case imp > 0 && reg > 0:
		return OutcomeMixed
	case imp > 0:
		return OutcomeImprovement
	case reg > 0:
		return OutcomeRegression
	case judged > 0:
		return OutcomeNoMaterialChange
	default:
		return OutcomeInsufficient
	}
}

func overall(v Validation) (string, string) {
	var statuses []string
	missing := 0
	inconclusive := 0
	for _, r := range v.Routes {
		if r.PresentIn != PresentBoth {
			missing++
			statuses = append(statuses, StatusInsufficient)
			continue
		}
		for _, s := range []*SignalComparison{r.Throughput, r.ServerErrors, r.Latency} {
			if s == nil {
				continue
			}
			statuses = append(statuses, s.Status)
			if s.Status == StatusInconclusive {
				inconclusive++
			}
		}
	}
	outcome := rollup(statuses...)
	if len(v.Routes) == 0 {
		outcome = OutcomeInsufficient
	}
	return outcome, summaryText(outcome, v, missing, inconclusive)
}

func summaryText(outcome string, v Validation, missing, inconclusive int) string {
	base := map[string]string{
		OutcomeImprovement:      "Every material server-side change is an improvement. This is one observed difference, not proof that a code change caused it and not evidence that it would recur.",
		OutcomeRegression:       "A material server-side regression was observed. This does not prove a code change caused it.",
		OutcomeMixed:            "Some server-side signals improved and some regressed. Neither direction is a proven cause.",
		OutcomeNoMaterialChange: "The compared server-side signals did not cross a material-change threshold.",
		OutcomeInsufficient:     "No server-side signal could be judged.",
	}[outcome]
	extra := fmt.Sprintf(" %d route(s) compared, %d ranked material change(s).", len(v.Routes), len(v.Ranked))
	if missing > 0 {
		extra += fmt.Sprintf(" %d route(s) are missing from one experiment and were not treated as zero traffic.", missing)
	}
	if inconclusive > 0 {
		extra += fmt.Sprintf(" %d latency comparison(s) reached the threshold but the bucket ranges overlap, so their direction is not claimed.", inconclusive)
	}
	if len(v.Comparability.Warnings) > 0 {
		extra += " Comparability warnings: " + strings.Join(v.Comparability.Warnings, "; ") + "."
	}
	return base + extra
}

func rank(routes []RouteComparison) []RankedChange {
	var out []RankedChange
	for _, r := range routes {
		for _, s := range []*SignalComparison{r.Throughput, r.ServerErrors, r.Latency} {
			if s == nil || (s.Status != StatusImprovement && s.Status != StatusRegression) {
				continue
			}
			item := RankedChange{
				Method: r.Method, Route: r.Route, Signal: s.Signal, Direction: s.Status,
				Absolute: s.Absolute, Relative: s.Relative, PercentagePoints: s.PercentagePoints,
			}
			switch {
			case s.Relative != nil:
				item.Magnitude = math.Abs(*s.Relative)
			case s.PercentagePoints != nil:
				item.Magnitude = math.Abs(*s.PercentagePoints) / 100
			}
			out = append(out, item)
		}
	}
	slices.SortFunc(out, func(x, y RankedChange) int {
		xr, yr := 1, 1
		if x.Direction == StatusRegression {
			xr = 0
		}
		if y.Direction == StatusRegression {
			yr = 0
		}
		if xr != yr {
			return xr - yr
		}
		if x.Magnitude != y.Magnitude {
			if x.Magnitude > y.Magnitude {
				return -1
			}
			return 1
		}
		return cmp.Or(strings.Compare(x.Method, y.Method), strings.Compare(x.Route, y.Route), strings.Compare(x.Signal, y.Signal))
	})
	if out == nil {
		return []RankedChange{}
	}
	for i := range out {
		out[i].Rank = i + 1
	}
	return out
}

func appendUnique(dst, src []EvidenceRef) []EvidenceRef {
	seen := map[string]bool{}
	for _, e := range dst {
		seen[e.Side+"\x00"+e.Pointer+"\x00"+e.Role] = true
	}
	for _, e := range src {
		k := e.Side + "\x00" + e.Pointer + "\x00" + e.Role
		if seen[k] {
			continue
		}
		seen[k] = true
		dst = append(dst, e)
	}
	return dst
}

func windowPointer(i int) string { return fmt.Sprintf("/analysis/windows/%d", i) }

func routePointer(window, route int) string {
	return fmt.Sprintf("/analysis/windows/%d/routes/%d", window, route)
}

// urlHidesParts reports whether a stored target URL still carries user
// information or a query string. Analysis reports replace both with a
// placeholder, so the original values are not available to compare.
func urlHidesParts(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return true
	}
	return u.User != nil || u.RawQuery != ""
}

func finiteOrZero(x float64) float64 {
	if !finite(x) {
		return 0
	}
	return x
}

func finite(xs ...float64) bool {
	for _, x := range xs {
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return false
		}
	}
	return true
}

func sameFloat(a, b float64) bool {
	if math.IsNaN(a) || math.IsNaN(b) {
		return false
	}
	if math.IsInf(a, 0) || math.IsInf(b, 0) {
		return a == b
	}
	diff := math.Abs(a - b)
	scale := math.Max(1, math.Max(math.Abs(a), math.Abs(b)))
	return diff <= 1e-9*scale
}

func fptr(x float64) *float64 { return &x }

func clean(s string) string {
	s = workload.RedactText(s, "")
	if r := []rune(s); len(r) > maxText {
		return string(r[:maxText]) + "..."
	}
	return s
}

func orText(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

func percent(x float64) string      { return fmt.Sprintf("%+.1f%%", x*100) }
func points(x float64) string       { return fmt.Sprintf("%.2f", x*100) }
func signedPoints(x float64) string { return fmt.Sprintf("%+.2f", x*100) }
func ratioText(x float64) string    { return fmt.Sprintf("%.2f%%", x*100) }
func rate(x float64) string         { return fmt.Sprintf("%.1f requests/s", x) }

func seconds(s float64) string {
	if math.IsInf(s, 1) {
		return "+Inf"
	}
	if s < 1 {
		return fmt.Sprintf("%.3f ms", s*1000)
	}
	return fmt.Sprintf("%.3f s", s)
}
