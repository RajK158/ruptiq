package fixvalidation

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RajK158/ruptiq/internal/analysis"
	"github.com/RajK158/ruptiq/internal/experiment"
	"github.com/RajK158/ruptiq/internal/investigation"
	"github.com/RajK158/ruptiq/internal/telemetry"
	"github.com/RajK158/ruptiq/internal/workload"
)

var t0 = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

type spec struct {
	method, route    string
	ok, e5, e4       float64
	noRequests       string
	p95, lo, hi      float64
	obs              uint64
	mean             float64
	noLatency, noP95 string
	bounds           []float64
}

func rt(method, route string) spec {
	return spec{method: method, route: route, ok: 1000, obs: 1000, p95: 0.008, lo: 0.005, hi: 0.01, mean: 0.004}
}

func (s spec) requests(ok, e5, e4 float64) spec {
	s.ok, s.e5, s.e4 = ok, e5, e4
	return s
}

func (s spec) latency(p95, lo, hi float64) spec {
	s.p95, s.lo, s.hi = p95, lo, hi
	return s
}

func (s spec) boundsOf(b ...float64) spec {
	s.bounds = b
	return s
}

func (s spec) withoutRequests(reason string) spec {
	s.noRequests = reason
	return s
}

func (s spec) withoutLatency(reason string) spec {
	s.noLatency = reason
	return s
}

func (s spec) withoutP95(reason string) spec {
	s.noP95 = reason
	return s
}

type wspec struct {
	elapsed  float64
	from, to experiment.Phase
	missed   int
	routes   []spec
}

func w(routes ...spec) wspec {
	return wspec{elapsed: 1, from: experiment.DuringWorkload, to: experiment.DuringWorkload, routes: routes}
}

func (x wspec) secs(e float64) wspec {
	x.elapsed = e
	return x
}

func (x wspec) phases(from, to experiment.Phase) wspec {
	x.from, x.to = from, to
	return x
}

func (x wspec) gaps(n int) wspec {
	x.missed = n
	return x
}

func pair(n int, routes ...spec) []wspec {
	out := make([]wspec, n)
	for i := range out {
		out[i] = w(routes...)
	}
	return out
}

func (s spec) signals(elapsed float64, cfg analysis.Config) analysis.RouteSignals {
	rs := analysis.RouteSignals{Method: s.method, Route: s.route}
	if s.noRequests != "" {
		rs.RequestsUnavailable = s.noRequests
	} else {
		total := s.ok + s.e5 + s.e4
		r := &analysis.RequestSignals{
			Requests: total, ElapsedSeconds: elapsed, PerSecond: 0,
			ByStatus: []analysis.StatusCount{
				{Status: "200", Requests: s.ok},
				{Status: "404", Requests: s.e4},
				{Status: "500", Requests: s.e5},
			},
			ServerErrors: s.e5, ClientErrors: s.e4,
			Evidence: analysis.Evidence{Metric: cfg.RequestsCounter, Series: []map[string]string{
				{"method": s.method, "route": s.route, "status": "200"},
			}},
		}
		if elapsed > 0 {
			r.PerSecond = total / elapsed
		}
		if total > 0 {
			r.ServerErrorRatio = ptr(s.e5 / total)
		} else {
			r.ServerErrorRatioUnavailable = "no requests in this window"
		}
		rs.Requests = r
	}
	if s.noLatency != "" {
		rs.LatencyUnavailable = s.noLatency
		return rs
	}
	bounds := s.bounds
	if len(bounds) == 0 {
		bounds = []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1}
	}
	buckets := make([]analysis.Bucket, len(bounds))
	for i, b := range bounds {
		c := uint64(0)
		if i == len(bounds)-1 {
			c = s.obs
		}
		buckets[i] = analysis.Bucket{UpperBound: telemetry.Float(b), CumulativeCount: c}
	}
	l := &analysis.LatencySignals{
		Observations: s.obs,
		P95:          analysis.QuantileEstimate{Quantile: analysis.Quantile, Estimate: true},
		Buckets:      buckets,
		Evidence:     analysis.Evidence{Metric: cfg.DurationHistogram, Series: []map[string]string{{"method": s.method, "route": s.route, "status": "200"}}},
	}
	if s.obs > 0 {
		l.MeanSeconds = ptr(s.mean)
	} else {
		l.MeanSecondsUnavailable = "no observations"
	}
	if s.noP95 != "" {
		l.P95.Unavailable = s.noP95
	} else {
		l.P95.Seconds, l.P95.BucketLowerSeconds, l.P95.BucketUpperSeconds = ptr(s.p95), ptr(s.lo), ptr(s.hi)
	}
	rs.Latency = l
	return rs
}

func ptr(x float64) *float64 { return &x }

func at(s float64) time.Time {
	return t0.Add(time.Duration(math.Round(s * float64(time.Second))))
}

func build(ws ...wspec) analysis.Analysis {
	cfg := analysis.DefaultConfig()
	a := analysis.Analysis{
		Experiment: analysis.ExperimentInfo{
			SchemaVersion: experiment.ReportSchemaVersion, Status: experiment.StatusPassed,
			StartedAt: t0, FinishedAt: t0.Add(time.Minute),
			TargetURL: "http://127.0.0.1:8090/work", MetricsURL: "http://127.0.0.1:8090/metrics",
			VUs: 2, DurationSeconds: 10, SampleIntervalSeconds: 1, Completeness: experiment.Complete,
		},
		Config: cfg,
		Client: analysis.ClientSignals{Source: "k6 (client side)", Metrics: workload.Metrics{HTTPRequests: 100}, Passed: true},
		Totals: []analysis.RouteSignals{}, Windows: []analysis.Window{},
		Warnings: []analysis.Warning{}, Limitations: []string{"synthetic fixture"},
	}
	seq, offset := 1, 0.0
	for i, x := range ws {
		from := analysis.SampleRef{Sequence: seq, Phase: x.from, ScrapedAt: at(offset), OffsetSeconds: offset}
		win := analysis.Window{Index: i + 1, From: from, MissedScrapes: []analysis.MissedScrape{}, Routes: []analysis.RouteSignals{}}
		for range x.missed {
			seq++
			win.MissedScrapes = append(win.MissedScrapes, analysis.MissedScrape{Sequence: seq, Phase: experiment.DuringWorkload, Reason: "scrape_failed", Error: "refused"})
		}
		seq++
		offset += x.elapsed
		win.To = analysis.SampleRef{Sequence: seq, Phase: x.to, ScrapedAt: at(offset), OffsetSeconds: offset}
		win.ElapsedSeconds = win.To.ScrapedAt.Sub(win.From.ScrapedAt).Seconds()
		for _, s := range x.routes {
			win.Routes = append(win.Routes, s.signals(win.ElapsedSeconds, cfg))
		}
		a.Windows = append(a.Windows, win)
	}
	return a
}

func ready(t *testing.T, a analysis.Analysis) (analysis.Analysis, []byte) {
	t.Helper()
	data, err := json.Marshal(analysis.Report{SchemaVersion: analysis.SchemaVersion, Analysis: &a})
	if err != nil {
		t.Fatal(err)
	}
	got, err := investigation.DecodeAnalysis(data)
	if err != nil {
		t.Fatal(err)
	}
	return got, data
}

func mustCompare(t *testing.T, before, after analysis.Analysis) Validation {
	t.Helper()
	b, _ := ready(t, before)
	a, _ := ready(t, after)
	v, err := Compare(b, a, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func route(v Validation, method, path string) RouteComparison {
	for _, r := range v.Routes {
		if r.Method == method && r.Route == path {
			return r
		}
	}
	return RouteComparison{}
}

func near(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9*math.Max(1, math.Abs(want)) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestImprovement(t *testing.T) {
	// Two 1s windows of 1000 requests at p95 20ms, then two of 1400 at p95 4ms.
	// Throughput 1000 → 1400 requests/s (+40%). Latency buckets do not overlap.
	before := build(pair(2, rt("GET", "/work").latency(0.020, 0.010, 0.025))...)
	after := build(pair(2, rt("GET", "/work").requests(1400, 0, 0).latency(0.004, 0.002, 0.005))...)
	v := mustCompare(t, before, after)
	if v.Outcome != OutcomeImprovement {
		t.Fatalf("outcome %s: %s", v.Outcome, v.Summary)
	}
	r := route(v, "GET", "/work")
	if r.Throughput.Status != StatusImprovement || r.Latency.Status != StatusImprovement || r.ServerErrors.Status != StatusNoMaterialChange {
		t.Fatalf("signals %s %s %s", r.Throughput.Status, r.ServerErrors.Status, r.Latency.Status)
	}
	near(t, r.Throughput.Before.Value, 1000)
	near(t, r.Throughput.After.Value, 1400)
	near(t, *r.Throughput.Absolute, 400)
	near(t, *r.Throughput.Relative, 0.4)
	near(t, r.Latency.Before.Value, 0.020)
	near(t, r.Latency.After.Value, 0.004)
	near(t, *r.Latency.Relative, -0.8)
	if !r.Latency.Before.Estimate || !strings.Contains(r.Latency.Statistic, "not an experiment-wide p95") {
		t.Fatalf("latency statistic %q estimate %v", r.Latency.Statistic, r.Latency.Before.Estimate)
	}
	if r.Latency.Before.BucketLowerSeconds == nil || *r.Latency.Before.BucketLowerSeconds != 0.010 || *r.Latency.After.BucketUpperSeconds != 0.005 {
		t.Fatalf("buckets %+v %+v", r.Latency.Before, r.Latency.After)
	}
	if len(v.Ranked) != 2 || v.Ranked[0].Signal != SignalLatency || v.Ranked[1].Signal != SignalThroughput {
		t.Fatalf("ranked %+v", v.Ranked)
	}
	for _, bad := range []string{"statistically significant", "proves that", "caused the"} {
		if strings.Contains(strings.ToLower(v.Summary), bad) {
			t.Errorf("summary claims %q: %s", bad, v.Summary)
		}
	}
}

func TestRegressionAndMixed(t *testing.T) {
	before := build(pair(2, rt("GET", "/work").latency(0.004, 0.002, 0.005))...)
	after := build(pair(2, rt("GET", "/work").requests(600, 50, 0).latency(0.020, 0.010, 0.025))...)
	v := mustCompare(t, before, after)
	if v.Outcome != OutcomeRegression {
		t.Fatalf("outcome %s: %s", v.Outcome, v.Summary)
	}
	r := route(v, "GET", "/work")
	if r.Throughput.Status != StatusRegression || r.ServerErrors.Status != StatusRegression || r.Latency.Status != StatusRegression {
		t.Fatalf("signals %s %s %s (%s)", r.Throughput.Status, r.ServerErrors.Status, r.Latency.Status, r.ServerErrors.Reason)
	}
	near(t, r.Throughput.Before.Value, 1000)
	near(t, r.Throughput.After.Value, 650)
	near(t, *r.ServerErrors.PercentagePoints, 50.0/650.0*100-0)

	// Throughput improves and the 5xx ratio gets worse: mixed, regressions ranked first.
	betterRate := build(pair(2, rt("GET", "/work").requests(1500, 80, 0))...)
	mixed := mustCompare(t, before, betterRate)
	if mixed.Outcome != OutcomeMixed {
		t.Fatalf("mixed outcome %s: %s", mixed.Outcome, mixed.Summary)
	}
	if mixed.Ranked[0].Direction != StatusRegression || mixed.Ranked[0].Signal != SignalServerErrors {
		t.Fatalf("rank %+v", mixed.Ranked)
	}
	if mixed.Ranked[1].Direction != StatusImprovement || mixed.Ranked[1].Signal != SignalThroughput {
		t.Fatalf("rank %+v", mixed.Ranked)
	}
}

func TestIdenticalMeasurements(t *testing.T) {
	a := build(pair(2, rt("GET", "/work"))...)
	v := mustCompare(t, a, a)
	if v.Outcome != OutcomeNoMaterialChange {
		t.Fatalf("outcome %s: %s", v.Outcome, v.Summary)
	}
	r := route(v, "GET", "/work")
	near(t, *r.Throughput.Relative, 0)
	near(t, *r.Throughput.Absolute, 0)
	if r.ServerErrors.Status != StatusNoMaterialChange || r.Latency.Status != StatusNoMaterialChange {
		t.Fatalf("errors %s latency %s", r.ServerErrors.Status, r.Latency.Status)
	}
	again := mustCompare(t, a, a)
	jb, _ := json.Marshal(v)
	ja, _ := json.Marshal(again)
	if string(jb) != string(ja) {
		t.Fatal("comparison is not deterministic")
	}
}

func TestThroughputUsesTotalTime(t *testing.T) {
	// 1s × 100 requests and 3s × 0 requests. The mean of the rates is 50/s.
	// Total requests over total time is 25/s.
	idle := rt("GET", "/work").requests(0, 0, 0).withoutLatency("no observations")
	before := build(
		w(rt("GET", "/work").requests(100, 0, 0)).secs(1),
		w(idle).secs(3),
	)
	after := build(
		w(rt("GET", "/work").requests(100, 0, 0)).secs(1),
		w(rt("GET", "/work").requests(300, 0, 0)).secs(3),
	)
	// Lower the request floor so the small counts are judged.
	cfg := DefaultConfig()
	cfg.MinRequests = 50
	b, _ := ready(t, before)
	a, _ := ready(t, after)
	v, err := Compare(b, a, cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := route(v, "GET", "/work")
	near(t, r.Throughput.Before.Value, 25)
	near(t, r.Throughput.After.Value, 100)
	if r.Throughput.Before.Value == 50 {
		t.Fatal("throughput averaged per-window rates")
	}
	near(t, *r.Throughput.Relative, 3)
}

func TestZeroRequestsAndZeroErrorBaseline(t *testing.T) {
	zero := rt("GET", "/work").requests(0, 0, 0).withoutLatency("no observations")
	v := mustCompare(t, build(pair(2, zero)...), build(pair(2, zero)...))
	r := route(v, "GET", "/work")
	if r.Throughput.Status != StatusNoMaterialChange || r.Throughput.Relative != nil {
		t.Fatalf("zero throughput %+v", r.Throughput)
	}
	if r.ServerErrors.Status != StatusInsufficient || r.ServerErrors.Relative != nil {
		t.Fatalf("zero errors %+v", r.ServerErrors)
	}
	if strings.Contains(r.Throughput.Reason, "improvement") && strings.Contains(r.Throughput.Reason, "material improvement") == false {
		// reason must say this is not an improvement
	}
	if !strings.Contains(r.Throughput.Reason, "no relative change") {
		t.Fatalf("reason %q", r.Throughput.Reason)
	}

	before := build(pair(2, rt("GET", "/work"))...)
	after := build(pair(2, rt("GET", "/work").requests(980, 20, 0))...)
	got := mustCompare(t, before, after)
	er := route(got, "GET", "/work").ServerErrors
	if er.Status != StatusRegression {
		t.Fatalf("zero-baseline errors: %s (%s)", er.Status, er.Reason)
	}
	near(t, er.Before.Value, 0)
	near(t, er.After.Value, 0.02)
	near(t, *er.PercentagePoints, 2)
}

func TestClientErrorsAreNotServerErrors(t *testing.T) {
	before := build(pair(2, rt("GET", "/work"))...)
	after := build(pair(2, rt("GET", "/work").requests(900, 0, 100))...)
	r := route(mustCompare(t, before, after), "GET", "/work")
	if r.ServerErrors.Status != StatusNoMaterialChange {
		t.Fatalf("status %s (%s)", r.ServerErrors.Status, r.ServerErrors.Reason)
	}
	if r.ServerErrors.After.ClientErrors == nil || *r.ServerErrors.After.ClientErrors != 200 {
		t.Fatalf("4xx %+v", r.ServerErrors.After)
	}
	if r.ServerErrors.After.Value != 0 {
		t.Fatalf("5xx ratio %v", r.ServerErrors.After.Value)
	}
}

func TestMissingRouteIsNotZero(t *testing.T) {
	before := build(pair(2, rt("GET", "/keep"), rt("GET", "/gone"))...)
	after := build(pair(2, rt("GET", "/keep"))...)
	v := mustCompare(t, before, after)
	gone := route(v, "GET", "/gone")
	if gone.Outcome != OutcomeInsufficient || gone.PresentIn != PresentBefore {
		t.Fatalf("gone %+v", gone)
	}
	if gone.Throughput != nil || strings.Contains(gone.Summary, "0") && strings.Contains(gone.Summary, "requests/s") {
		t.Fatalf("missing route was given a rate: %+v", gone)
	}
	if !strings.Contains(gone.Summary, "not zero traffic") {
		t.Fatalf("summary %q", gone.Summary)
	}
	keep := route(v, "GET", "/keep")
	if keep.Outcome != OutcomeNoMaterialChange || keep.PresentIn != PresentBoth {
		t.Fatalf("keep %+v", keep)
	}
	if v.Outcome != OutcomeNoMaterialChange || !strings.Contains(v.Summary, "missing") {
		t.Fatalf("overall %s %s", v.Outcome, v.Summary)
	}
	// Route order is method, then path, regardless of input order.
	rev := mustCompare(t, build(pair(2, rt("GET", "/gone"), rt("GET", "/keep"))...), after)
	if rev.Routes[0].Route != "/gone" || rev.Routes[1].Route != "/keep" {
		t.Fatalf("order %+v %+v", rev.Routes[0].Route, rev.Routes[1].Route)
	}
}

func TestScrapeGapResetAndBoundary(t *testing.T) {
	base := rt("GET", "/work")
	gap := build(
		w(base),
		w(base.requests(5000, 0, 0)).gaps(1),
		w(base),
	)
	v := mustCompare(t, gap, build(pair(2, base)...))
	r := route(v, "GET", "/work")
	if r.Throughput.Before.Windows != 2 || r.Throughput.Before.Value != 1000 {
		t.Fatalf("gap leaked into the rate: %+v excluded %+v", r.Throughput.Before, r.Throughput.ExcludedBefore)
	}
	foundGap := false
	for _, e := range r.Throughput.ExcludedBefore {
		if strings.Contains(e.Reason, "missed scrape") {
			foundGap = true
		}
	}
	if !foundGap {
		t.Fatalf("gap not excluded: %+v", r.Throughput.ExcludedBefore)
	}

	reset := build(pair(3, base)...)
	reset.Windows[1].Routes[0] = base.requests(9000, 0, 0).signals(reset.Windows[1].ElapsedSeconds, reset.Config)
	reset.Warnings = append(reset.Warnings, analysis.Warning{
		Kind: analysis.WarnCounterReset, Window: 2, Metric: reset.Config.RequestsCounter,
		Labels: map[string]string{"method": "GET", "route": "/work"}, Message: "counter decreased",
	})
	got := mustCompare(t, reset, build(pair(2, base)...))
	tr := route(got, "GET", "/work").Throughput
	if tr.Before.Windows != 2 || tr.Before.Value != 1000 {
		t.Fatalf("reset window included: %+v excluded %+v", tr.Before, tr.ExcludedBefore)
	}
	foundReset := false
	for _, e := range tr.ExcludedBefore {
		if strings.Contains(e.Reason, "counter decreased") {
			foundReset = true
		}
	}
	if !foundReset {
		t.Fatalf("reset not named: %+v", tr.ExcludedBefore)
	}

	edge := build(
		w(base.requests(100000, 0, 0)).phases(experiment.BeforeWorkload, experiment.DuringWorkload),
		w(base),
		w(base),
		w(base.requests(0, 0, 0)).phases(experiment.DuringWorkload, experiment.AfterWorkload),
	)
	edgeV := mustCompare(t, edge, build(pair(2, base)...))
	if route(edgeV, "GET", "/work").Throughput.Before.Value != 1000 {
		t.Fatalf("boundary window changed the rate: %+v", route(edgeV, "GET", "/work").Throughput.Before)
	}
	if len(edgeV.Before.EligibleWindows) != 2 || edgeV.Before.EligibleWindows[0] != 2 {
		t.Fatalf("eligible %+v excluded %+v", edgeV.Before.EligibleWindows, edgeV.Before.ExcludedWindows)
	}
}

func TestHistogramOverlapAndIncompatibleBuckets(t *testing.T) {
	before := build(pair(2, rt("GET", "/work").latency(0.020, 0.010, 0.050))...)
	after := build(pair(2, rt("GET", "/work").latency(0.008, 0.005, 0.020))...)
	r := route(mustCompare(t, before, after), "GET", "/work")
	if r.Latency.Status != StatusInconclusive {
		t.Fatalf("status %s (%s)", r.Latency.Status, r.Latency.Reason)
	}
	if r.Outcome == OutcomeImprovement || r.Latency.Status == StatusImprovement {
		t.Fatal("overlapping buckets were called an improvement")
	}
	if r.Outcome != OutcomeNoMaterialChange {
		t.Fatalf("route outcome %s", r.Outcome)
	}

	other := rt("GET", "/work").latency(0.004, 0.002, 0.005).boundsOf(0.002, 0.02, 0.2)
	bad := route(mustCompare(t, before, build(pair(2, other)...)), "GET", "/work")
	if bad.Latency.Status != StatusIncompatible {
		t.Fatalf("status %s (%s)", bad.Latency.Status, bad.Latency.Reason)
	}
	if bad.Outcome == OutcomeImprovement {
		t.Fatal("incompatible histograms were judged as an improvement")
	}
}

func TestNotComparableConfigurations(t *testing.T) {
	base := build(pair(2, rt("GET", "/work"))...)
	other := base
	other.Experiment.VUs = 8
	v := mustCompare(t, base, other)
	if v.Outcome != OutcomeNotComparable || v.Comparability.Comparable || len(v.Routes) != 0 {
		t.Fatalf("outcome %s routes %d reasons %v", v.Outcome, len(v.Routes), v.Comparability.Reasons)
	}
	if !strings.Contains(strings.Join(v.Comparability.Reasons, ";"), "VUs") {
		t.Fatalf("reasons %v", v.Comparability.Reasons)
	}

	dur := base
	dur.Experiment.DurationSeconds = 30
	if mustCompare(t, base, dur).Outcome != OutcomeNotComparable {
		t.Fatal("duration mismatch compared")
	}
	interval := base
	interval.Experiment.SampleIntervalSeconds = 5
	if mustCompare(t, base, interval).Outcome != OutcomeNotComparable {
		t.Fatal("interval mismatch compared")
	}
	url := base
	url.Experiment.TargetURL = "http://127.0.0.1:8090/other"
	if mustCompare(t, base, url).Outcome != OutcomeNotComparable {
		t.Fatal("URL mismatch compared")
	}
	labels := base
	labels.Config.MethodLabel = "verb"
	if mustCompare(t, base, labels).Outcome != OutcomeNotComparable {
		t.Fatal("label mismatch compared")
	}
	obs := base
	obs.Config.MinObservations = 10
	if mustCompare(t, base, obs).Outcome != OutcomeNotComparable {
		t.Fatal("min observation mismatch compared")
	}
}

func TestInsufficientWindows(t *testing.T) {
	one := build(w(rt("GET", "/work")))
	v := mustCompare(t, one, one)
	if v.Outcome != OutcomeInsufficient {
		t.Fatalf("outcome %s", v.Outcome)
	}
	r := route(v, "GET", "/work")
	if r.Throughput.Status != StatusInsufficient || !strings.Contains(r.Throughput.Reason, "at least 2") {
		t.Fatalf("%+v", r.Throughput)
	}
}

func TestNonFiniteMeasurements(t *testing.T) {
	a, _ := ready(t, build(pair(2, rt("GET", "/work"))...))
	a.Windows[0].Routes[0].Latency.P95.Seconds = ptr(math.NaN())
	b, _ := ready(t, build(pair(2, rt("GET", "/work"))...))
	v, err := Compare(a, b, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	lat := route(v, "GET", "/work").Latency
	if lat.BeforeWindows[0] == 1 {
		t.Fatal("NaN p95 was used")
	}
	found := false
	for _, e := range lat.ExcludedBefore {
		if strings.Contains(e.Reason, "not a finite") {
			found = true
		}
	}
	if !found {
		t.Fatalf("excluded %+v windows %v", lat.ExcludedBefore, lat.BeforeWindows)
	}
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "NaN") || strings.Contains(string(data), "Inf") {
		t.Fatalf("non-finite JSON: %s", data)
	}

	nanDur, _ := ready(t, build(pair(2, rt("GET", "/work"))...))
	nanDur.Experiment.DurationSeconds = math.NaN()
	got, err := Compare(nanDur, b, DefaultConfig())
	if err != nil || got.Outcome != OutcomeNotComparable {
		t.Fatalf("err %v outcome %s", err, got.Outcome)
	}
	if _, err := json.Marshal(got); err != nil {
		t.Fatal(err)
	}
}

func TestEvidencePointersResolve(t *testing.T) {
	rawBefore := build(pair(2, rt("GET", "/work").latency(0.020, 0.010, 0.025))...)
	rawAfter := build(pair(2, rt("GET", "/work").requests(1400, 0, 0).latency(0.004, 0.002, 0.005))...)
	before, beforeJSON := ready(t, rawBefore)
	after, afterJSON := ready(t, rawAfter)
	v, err := Compare(before, after, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	var bdoc, adoc any
	if err := json.Unmarshal(beforeJSON, &bdoc); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(afterJSON, &adoc); err != nil {
		t.Fatal(err)
	}
	r := route(v, "GET", "/work")
	if len(r.Evidence) == 0 {
		t.Fatal("no evidence")
	}
	for _, e := range r.Evidence {
		doc := bdoc
		if e.Side == SideAfter {
			doc = adoc
		}
		obj := resolve(t, doc, e.Pointer)
		switch {
		case strings.HasSuffix(e.Pointer, "/requests"):
			req, ok := obj["requests"].(float64)
			if !ok {
				t.Fatalf("%s did not resolve to requests: %v", e.Pointer, obj)
			}
			want := 1000.0
			if e.Side == SideAfter {
				want = 1400
			}
			if req != want {
				t.Fatalf("%s %s requests = %v, want %v", e.Side, e.Pointer, req, want)
			}
		case strings.HasSuffix(e.Pointer, "/p95"):
			sec, ok := obj["seconds"].(float64)
			if !ok || obj["bucket_lower_seconds"] == nil {
				t.Fatalf("%s did not resolve to a p95: %v", e.Pointer, obj)
			}
			want := 0.020
			if e.Side == SideAfter {
				want = 0.004
			}
			if math.Abs(sec-want) > 1e-12 {
				t.Fatalf("%s %s p95 = %v, want %v", e.Side, e.Pointer, sec, want)
			}
		default:
			t.Fatalf("unexpected pointer %s", e.Pointer)
		}
	}
	if r.Throughput.Evidence[0].Pointer != "/analysis/windows/0/routes/0/requests" {
		t.Fatalf("pointer %s", r.Throughput.Evidence[0].Pointer)
	}
}

func TestDifferentQueriesAreNotTreatedAsTheSameTarget(t *testing.T) {
	before := build(pair(2, rt("GET", "/work"))...)
	after := build(pair(2, rt("GET", "/work"))...)
	before.Experiment.TargetURL = "http://127.0.0.1:8090/work?sku=alpha-secret"
	after.Experiment.TargetURL = "http://127.0.0.1:8090/work?sku=beta-secret"
	v := mustCompare(t, before, after)
	if v.Outcome == OutcomeNotComparable {
		t.Fatalf("redacted URLs differ only in the query, outcome %s", v.Outcome)
	}
	if v.Outcome != OutcomeNoMaterialChange {
		t.Fatalf("outcome %s", v.Outcome)
	}
	warn := strings.Join(v.Comparability.Warnings, " ")
	if !strings.Contains(warn, "query string is redacted") || !strings.Contains(warn, "does not prove") {
		t.Fatalf("warnings %v", v.Comparability.Warnings)
	}
	blob, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"alpha-secret", "beta-secret"} {
		if strings.Contains(string(blob), secret) || strings.Contains(v.Summary, secret) {
			t.Errorf("output contains %q", secret)
		}
	}
}

func TestErrorRatioUsesOnlyEligibleWindows(t *testing.T) {
	base := rt("GET", "/work")
	before := build(
		w(base),
		w(base.requests(100, 500, 0)).gaps(1),
		w(base),
	)
	r := route(mustCompare(t, before, build(pair(2, base)...)), "GET", "/work")
	if r.ServerErrors.Status != StatusNoMaterialChange || r.ServerErrors.Before.Value != 0 {
		t.Fatalf("gap 5xx affected the ratio: %+v", r.ServerErrors)
	}
	if r.ServerErrors.Before.ServerErrors == nil || *r.ServerErrors.Before.ServerErrors != 0 {
		t.Fatalf("pooled 5xx %+v", r.ServerErrors.Before)
	}
}

func TestRedactionAndInvalidConfig(t *testing.T) {
	a := build(pair(2, rt("GET", "http://user:s3cret@example.test/work?token=abcdef"))...)
	a.Experiment.TargetURL = "http://loaduser:s3cret@127.0.0.1:8090/work?api_key=abcdef"
	v := mustCompare(t, a, a)
	blob, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	text := string(blob) + v.Summary
	for _, secret := range []string{"s3cret", "abcdef", "loaduser"} {
		if strings.Contains(text, secret) {
			t.Errorf("output contains %q", secret)
		}
	}
	if v.Before.TargetURL != "http://xxxxx@127.0.0.1:8090/work?xxxxx" {
		t.Errorf("target %q", v.Before.TargetURL)
	}
	if !strings.Contains(strings.Join(v.Comparability.Warnings, " "), "query string is redacted") {
		t.Fatalf("warnings %v", v.Comparability.Warnings)
	}
	if v.Outcome == OutcomeNotComparable {
		t.Fatal("redacted query rejected a same-path comparison")
	}
	if _, err := Compare(a, a, Config{}); err == nil {
		t.Fatal("invalid config accepted")
	}
}

func resolve(t *testing.T, doc any, pointer string) map[string]any {
	t.Helper()
	cur := doc
	for _, tok := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		switch v := cur.(type) {
		case map[string]any:
			next, ok := v[tok]
			if !ok {
				t.Fatalf("pointer %q: no key %q", pointer, tok)
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(v) {
				t.Fatalf("pointer %q: bad index %q", pointer, tok)
			}
			cur = v[i]
		default:
			t.Fatalf("pointer %q: %T", pointer, cur)
		}
	}
	m, ok := cur.(map[string]any)
	if !ok {
		t.Fatalf("pointer %q is %T", pointer, cur)
	}
	return m
}
