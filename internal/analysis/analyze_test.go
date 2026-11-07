package analysis

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/RajK158/ruptiq/internal/experiment"
	"github.com/RajK158/ruptiq/internal/telemetry"
)

var get200 = labels("GET", "/inventory/{sku}", "200")

// Rates are increases divided by the time between the two scrapes, not
// cumulative values, and not divided by the configured interval.
func TestCounterDeltasAndUnequalIntervals(t *testing.T) {
	a := analyze(t, report(
		sample(0, experiment.BeforeWorkload, counter(reqs, cs(get200, 100))),
		sample(1, experiment.DuringWorkload, counter(reqs, cs(get200, 1100))),
		sample(2.5, experiment.DuringWorkload, counter(reqs, cs(get200, 4100))),
		sample(2.75, experiment.AfterWorkload, counter(reqs, cs(get200, 4100))),
	))
	if len(a.Windows) != 3 {
		t.Fatalf("windows = %d, want 3", len(a.Windows))
	}
	for i, want := range []struct{ elapsed, requests, perSecond float64 }{
		{1, 1000, 1000},
		{1.5, 3000, 2000}, // not 3000/s (the configured 1s interval), and not 4100 (cumulative)
		{0.25, 0, 0},
	} {
		w := a.Windows[i]
		wantNear(t, "elapsed", w.ElapsedSeconds, want.elapsed)
		r := route(t, w.Routes, "GET", "/inventory/{sku}").Requests
		if r == nil {
			t.Fatalf("window %d: requests unavailable", i+1)
		}
		wantNear(t, "requests", r.Requests, want.requests)
		wantNear(t, "per second", r.PerSecond, want.perSecond)
		wantNear(t, "elapsed (requests)", r.ElapsedSeconds, want.elapsed)
	}
	if w := a.Windows[1]; w.From.Sequence != 2 || w.To.Sequence != 3 || !w.From.ScrapedAt.Equal(at(1)) || !w.To.ScrapedAt.Equal(at(2.5)) {
		t.Errorf("window 2 evidence = %+v -> %+v", w.From, w.To)
	}
	tot := route(t, a.Totals, "GET", "/inventory/{sku}").Requests
	wantNear(t, "total requests", tot.Requests, 4000)
	wantNear(t, "total elapsed", tot.ElapsedSeconds, 2.75)
	wantNear(t, "total per second", tot.PerSecond, 4000/2.75)
	if !slices.Equal(tot.Evidence.Windows, []int{1, 2, 3}) || tot.Evidence.Metric != reqs {
		t.Errorf("total evidence = %+v", tot.Evidence)
	}
	if len(a.Warnings) != 1 || a.Warnings[0].Kind != WarnFamilyMissing || a.Warnings[0].Metric != durs {
		t.Errorf("warnings = %+v, want only the missing histogram", a.Warnings)
	}
}

func TestRoutesAndStatuses(t *testing.T) {
	getA := []telemetry.Series{
		cs(labels("GET", "/inventory/{sku}", "200"), 1000),
		cs(labels("GET", "/inventory/{sku}", "404"), 50),
		cs(labels("GET", "/inventory/{sku}", "500"), 5),
		cs(labels("POST", "/reservations", "201"), 100),
		cs(labels("POST", "/reservations", "409"), 10),
		cs(labels("POST", "/reservations", "503"), 1),
	}
	getB := []telemetry.Series{
		cs(labels("GET", "/inventory/{sku}", "200"), 1900),
		cs(labels("GET", "/inventory/{sku}", "404"), 100), // +50 client errors
		cs(labels("GET", "/inventory/{sku}", "500"), 55),  // +50 server errors
		cs(labels("POST", "/reservations", "201"), 130),
		cs(labels("POST", "/reservations", "409"), 30),
		cs(labels("POST", "/reservations", "503"), 1),
	}
	a := analyze(t, report(
		sample(0, experiment.DuringWorkload, counter(reqs, getA...)),
		sample(2, experiment.DuringWorkload, counter(reqs, getB...)),
	))
	w := a.Windows[0]
	if len(w.Routes) != 2 {
		t.Fatalf("routes = %+v", w.Routes)
	}
	get := route(t, w.Routes, "GET", "/inventory/{sku}").Requests
	wantNear(t, "GET requests", get.Requests, 1000)
	wantNear(t, "GET per second", get.PerSecond, 500)
	wantNear(t, "GET 5xx", get.ServerErrors, 50)
	wantNear(t, "GET 4xx", get.ClientErrors, 50)
	wantNear(t, "GET 5xx ratio", *get.ServerErrorRatio, 0.05) // 404s are not server errors
	wantStatuses := []StatusCount{{"200", 900}, {"404", 50}, {"500", 50}}
	if !slices.Equal(get.ByStatus, wantStatuses) {
		t.Errorf("GET by status = %v, want %v", get.ByStatus, wantStatuses)
	}
	if len(get.Evidence.Series) != 3 || get.Evidence.Series[0]["status"] != "200" {
		t.Errorf("GET evidence = %+v", get.Evidence)
	}

	post := route(t, w.Routes, "POST", "/reservations").Requests
	wantNear(t, "POST requests", post.Requests, 50)
	wantNear(t, "POST 5xx", post.ServerErrors, 0)
	wantNear(t, "POST 4xx", post.ClientErrors, 20)
	wantNear(t, "POST 5xx ratio", *post.ServerErrorRatio, 0)
}

func TestZeroTraffic(t *testing.T) {
	h := hs(get200, []float64{0.001, 0.01}, []uint64{5, 8}, 10, 0.05)
	a := analyze(t, report(
		sample(0, experiment.DuringWorkload, histogram(durs, h), counter(reqs, cs(get200, 10))),
		sample(1, experiment.DuringWorkload, histogram(durs, h), counter(reqs, cs(get200, 10))),
	))
	r := route(t, a.Windows[0].Routes, "GET", "/inventory/{sku}")
	if r.Requests == nil || r.Requests.Requests != 0 || r.Requests.PerSecond != 0 {
		t.Fatalf("requests = %+v, want 0 at 0/s", r.Requests)
	}
	if r.Requests.ServerErrorRatio != nil || r.Requests.ServerErrorRatioUnavailable == "" {
		t.Errorf("5xx ratio with no requests = %v, want unavailable", r.Requests.ServerErrorRatio)
	}
	l := r.Latency
	if l == nil || l.Observations != 0 || l.MeanSeconds != nil || l.P95.Seconds != nil || l.P95.Unavailable != "no requests were completed" {
		t.Errorf("latency = %+v, want no observations and no estimates", l)
	}
}

// A series that appears was not observed before, so its earlier value is
// unknown: the route's window is unavailable instead of counting from zero.
func TestSeriesAppearingAndDisappearing(t *testing.T) {
	get500 := labels("GET", "/inventory/{sku}", "500")
	a := analyze(t, report(
		sample(0, experiment.DuringWorkload, counter(reqs, cs(get200, 100))),
		sample(1, experiment.DuringWorkload, counter(reqs, cs(get200, 200), cs(get500, 7))),
		sample(2, experiment.DuringWorkload, counter(reqs, cs(get200, 300), cs(get500, 9))),
		sample(3, experiment.DuringWorkload, counter(reqs, cs(get200, 400))),
	))
	r1 := route(t, a.Windows[0].Routes, "GET", "/inventory/{sku}")
	if r1.Requests != nil || !strings.Contains(r1.RequestsUnavailable, "a series appeared") {
		t.Errorf("window 1 = %+v, want unavailable because a series appeared", r1)
	}
	r2 := route(t, a.Windows[1].Routes, "GET", "/inventory/{sku}").Requests
	wantNear(t, "window 2 requests", r2.Requests, 102)
	wantNear(t, "window 2 5xx ratio", *r2.ServerErrorRatio, 2.0/102)
	r3 := route(t, a.Windows[2].Routes, "GET", "/inventory/{sku}")
	if r3.Requests != nil || !strings.Contains(r3.RequestsUnavailable, "a series disappeared") {
		t.Errorf("window 3 = %+v, want unavailable because a series disappeared", r3)
	}

	appeared, gone := warningsOf(a, WarnSeriesAppeared), warningsOf(a, WarnSeriesDisappeared)
	if len(appeared) != 1 || appeared[0].Window != 1 || appeared[0].Labels["status"] != "500" || appeared[0].Metric != reqs {
		t.Errorf("appeared warnings = %+v", appeared)
	}
	if len(gone) != 1 || gone[0].Window != 3 || gone[0].Labels["status"] != "500" {
		t.Errorf("disappeared warnings = %+v", gone)
	}
	tot := route(t, a.Totals, "GET", "/inventory/{sku}").Requests
	wantNear(t, "total requests", tot.Requests, 102) // only window 2: the 7 and 100 are not counted from zero
	if !slices.Equal(tot.Evidence.Windows, []int{2}) || !slices.Equal(tot.Evidence.ExcludedWindows, []int{1, 3}) {
		t.Errorf("total evidence = %+v", tot.Evidence)
	}
}

func TestCounterReset(t *testing.T) {
	a := analyze(t, report(
		sample(0, experiment.DuringWorkload, counter(reqs, cs(get200, 1000))),
		sample(1, experiment.DuringWorkload, counter(reqs, cs(get200, 50))),
		sample(2, experiment.DuringWorkload, counter(reqs, cs(get200, 150))),
	))
	r1 := route(t, a.Windows[0].Routes, "GET", "/inventory/{sku}")
	if r1.Requests != nil || !strings.Contains(r1.RequestsUnavailable, "possible reset") {
		t.Errorf("window 1 = %+v, want unavailable because of a reset", r1)
	}
	resets := warningsOf(a, WarnCounterReset)
	if len(resets) != 1 || resets[0].Window != 1 || !strings.Contains(resets[0].Message, "from 1000 (sample #1) to 50 (sample #2)") {
		t.Errorf("reset warnings = %+v", resets)
	}
	wantNear(t, "window 2", route(t, a.Windows[1].Routes, "GET", "/inventory/{sku}").Requests.Requests, 100)
	data, _ := json.Marshal(a)
	if bytes.Contains(data, []byte(`"requests":-`)) || bytes.Contains(data, []byte(`"per_second":-`)) {
		t.Errorf("analysis has a negative count or rate: %s", data)
	}
}

// A window spanning a failed or unretained scrape is one longer window: its
// rate is over the whole span, and it is flagged.
func TestScrapeGaps(t *testing.T) {
	a := analyze(t, report(
		sample(0, experiment.BeforeWorkload, counter(reqs, cs(get200, 0))),
		sample(1, experiment.DuringWorkload, counter(reqs, cs(get200, 1000))),
		failed(2, experiment.DuringWorkload, "metrics request failed: GET http://scraper:pw@127.0.0.1:8082/metrics?token=abc: connection refused"),
		sample(3, experiment.DuringWorkload, counter(reqs, cs(get200, 5000))),
		sample(3.5, experiment.AfterWorkload, counter(reqs, cs(get200, 5000))),
	))
	if len(a.Windows) != 3 {
		t.Fatalf("windows = %d, want 3", len(a.Windows))
	}
	w := a.Windows[1]
	if w.From.Sequence != 2 || w.To.Sequence != 4 || len(w.MissedScrapes) != 1 || w.MissedScrapes[0].Sequence != 3 || w.MissedScrapes[0].Reason != "scrape_failed" {
		t.Fatalf("window 2 = %+v", w)
	}
	if strings.Contains(w.MissedScrapes[0].Error, "pw") || strings.Contains(w.MissedScrapes[0].Error, "abc") {
		t.Errorf("missed scrape error is not redacted: %q", w.MissedScrapes[0].Error)
	}
	r := route(t, w.Routes, "GET", "/inventory/{sku}").Requests
	wantNear(t, "gap elapsed", r.ElapsedSeconds, 2)
	wantNear(t, "gap per second", r.PerSecond, 2000)
	if g := warningsOf(a, WarnScrapeGap); len(g) != 1 || g[0].Window != 2 {
		t.Errorf("gap warnings = %+v", g)
	}
	if !hasLimitation(a, "span scrapes without values") || !hasLimitation(a, "Telemetry is partial") {
		t.Errorf("limitations = %q", a.Limitations)
	}
}

func TestNotRetainedGap(t *testing.T) {
	a := analyze(t, report(
		sample(0, experiment.BeforeWorkload, counter(reqs, cs(get200, 0))),
		sample(1, experiment.DuringWorkload, counter(reqs, cs(get200, 600))),
		dropped(2, experiment.DuringWorkload),
	))
	if a.Experiment.Status != experiment.StatusIncomplete || a.Experiment.NotRetained != 1 || len(a.Windows) != 1 {
		t.Fatalf("analysis = %+v", a.Experiment)
	}
	if len(a.Windows[0].MissedScrapes) != 0 {
		t.Errorf("a scrape after the last sample is not inside a window: %+v", a.Windows[0].MissedScrapes)
	}
}

func hasLimitation(a Analysis, text string) bool {
	return slices.ContainsFunc(a.Limitations, func(l string) bool { return strings.Contains(l, text) })
}

// The interval histogram is the later sample's cumulative buckets minus the
// earlier one's, combined over a route's statuses.
func TestHistogramP95(t *testing.T) {
	get404 := labels("GET", "/inventory/{sku}", "404")
	bounds := []float64{0.001, 0.0025, 0.005}
	a := analyze(t, report(
		sample(0, experiment.DuringWorkload, histogram(durs,
			hs(get200, bounds, []uint64{10, 20, 30}, 30, 0.05),
			hs(get404, bounds, []uint64{0, 0, 0}, 0, 0)),
			counter(reqs, cs(get200, 30), cs(get404, 0))),
		// Interval: 200s add [40, 80, 90, 90], 404s add [20, 30, 40, 40]:
		// together [60, 110, 130, 130]; sums add 0.21 and 0.05.
		sample(1, experiment.DuringWorkload, histogram(durs,
			hs(get200, bounds, []uint64{50, 100, 120}, 120, 0.26),
			hs(get404, bounds, []uint64{20, 30, 40}, 40, 0.05)),
			counter(reqs, cs(get200, 120), cs(get404, 40))),
	))
	l := route(t, a.Windows[0].Routes, "GET", "/inventory/{sku}").Latency
	if l == nil {
		t.Fatal("latency unavailable")
	}
	if l.Observations != 130 {
		t.Errorf("observations = %d, want 130", l.Observations)
	}
	wantNear(t, "mean", *l.MeanSeconds, 0.26/130)
	// Rank 0.95 * 130 = 123.5 lies in (0.0025, 0.005], which holds
	// observations 111..130: 0.0025 + 0.0025 * (123.5 - 110) / 20.
	p := l.P95
	if !p.Estimate || p.Quantile != 0.95 || p.Seconds == nil {
		t.Fatalf("p95 = %+v", p)
	}
	wantNear(t, "p95", *p.Seconds, 0.0025+0.0025*13.5/20)
	wantNear(t, "p95 lower", *p.BucketLowerSeconds, 0.0025)
	wantNear(t, "p95 upper", *p.BucketUpperSeconds, 0.005)
	wantBuckets := []Bucket{{0.001, 60}, {0.0025, 110}, {0.005, 130}, {telemetry.Float(math.Inf(1)), 130}}
	if !slices.Equal(l.Buckets, wantBuckets) {
		t.Errorf("buckets = %v, want %v", l.Buckets, wantBuckets)
	}
	if len(l.Evidence.Series) != 2 || l.Evidence.Metric != durs {
		t.Errorf("evidence = %+v", l.Evidence)
	}
}

func TestEstimateQuantile(t *testing.T) {
	inf := math.Inf(1)
	for _, tc := range []struct {
		name        string
		bounds      []float64
		counts      []uint64
		total       uint64
		min         int
		want        float64
		lower, uppr float64
		unavailable string
	}{
		{"first bucket starts at zero", []float64{0.01, inf}, []uint64{100, 100}, 100, 20, 0.0095, 0, 0.01, ""},
		{"rank exactly on a bound", []float64{1, 2, inf}, []uint64{95, 100, 100}, 100, 20, 1, 0, 1, ""},
		{"just above a bound", []float64{1, 2, inf}, []uint64{94, 100, 100}, 100, 20, 1 + 1.0/6, 1, 2, ""},
		{"in the +Inf bucket", []float64{1, inf}, []uint64{90, 100}, 100, 20, 0, 0, 0, "the p95 rank lies in the +Inf bucket, above the largest finite bound (1s)"},
		{"only +Inf", []float64{inf}, []uint64{100}, 100, 20, 0, 0, 0, "the histogram has only a +Inf bucket"},
		{"too few", []float64{1, inf}, []uint64{19, 19}, 19, 20, 0, 0, 0, "only 19 requests were completed; at least 20 are needed for an estimate"},
		{"none", []float64{1, inf}, []uint64{0, 0}, 0, 20, 0, 0, 0, "no requests were completed"},
		{"no buckets", nil, nil, 50, 20, 0, 0, 0, "the histogram has no buckets"},
		{"no +Inf bucket, rank above", []float64{1, 2}, []uint64{50, 90}, 100, 20, 0, 0, 0, "the p95 rank lies above the histogram's largest bucket bound (2s)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := estimateQuantile(0.95, tc.bounds, tc.counts, tc.total, tc.min)
			if !e.Estimate || e.Quantile != 0.95 {
				t.Errorf("estimate = %+v", e)
			}
			if tc.unavailable != "" {
				if e.Seconds != nil || e.Unavailable != tc.unavailable {
					t.Errorf("got %+v, want unavailable %q", e, tc.unavailable)
				}
				return
			}
			if e.Seconds == nil {
				t.Fatalf("unavailable: %s", e.Unavailable)
			}
			wantNear(t, "estimate", *e.Seconds, tc.want)
			wantNear(t, "lower", *e.BucketLowerSeconds, tc.lower)
			wantNear(t, "upper", *e.BucketUpperSeconds, tc.uppr)
		})
	}
}

func TestIncompatibleHistograms(t *testing.T) {
	b1 := []float64{0.001, 0.01}
	b2 := []float64{0.001, 0.05}
	tests := []struct {
		name     string
		from, to telemetry.Series
		kind     string
		reason   string
	}{
		{"bounds changed", hs(get200, b1, []uint64{10, 20}, 30, 1), hs(get200, b2, []uint64{20, 40}, 60, 2),
			WarnBucketsChanged, "the bucket bounds changed"},
		{"bucket count changed", hs(get200, b1, []uint64{10, 20}, 30, 1), hs(get200, []float64{0.001}, []uint64{20}, 60, 2),
			WarnBucketsChanged, "the bucket bounds changed"},
		{"count reset", hs(get200, b1, []uint64{10, 20}, 30, 1), hs(get200, b1, []uint64{10, 20}, 20, 0.5),
			WarnHistogramReset, "possible reset"},
		{"bucket reset", hs(get200, b1, []uint64{10, 20}, 30, 1), hs(get200, b1, []uint64{5, 25}, 35, 1.5),
			WarnHistogramReset, "a bucket count decreased"},
		{"increases not cumulative", hs(get200, b1, []uint64{10, 20}, 30, 1), hs(get200, b1, []uint64{25, 30}, 40, 1.5),
			WarnHistogramReset, "the bucket increases are not cumulative"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := analyze(t, report(
				sample(0, experiment.DuringWorkload, histogram(durs, tc.from), counter(reqs, cs(get200, 30))),
				sample(1, experiment.DuringWorkload, histogram(durs, tc.to), counter(reqs, cs(get200, 60))),
			))
			r := route(t, a.Windows[0].Routes, "GET", "/inventory/{sku}")
			if r.Latency != nil || !strings.Contains(r.LatencyUnavailable, tc.reason) {
				t.Errorf("latency = %+v / %q, want unavailable: %q", r.Latency, r.LatencyUnavailable, tc.reason)
			}
			if r.Requests == nil {
				t.Errorf("requests should still be available: %s", r.RequestsUnavailable)
			}
			if w := warningsOf(a, tc.kind); len(w) != 1 || w[0].Metric != durs {
				t.Errorf("%s warnings = %+v", tc.kind, w)
			}
		})
	}
}

func TestRouteSeriesWithDifferentBounds(t *testing.T) {
	get404 := labels("GET", "/inventory/{sku}", "404")
	a := analyze(t, report(
		sample(0, experiment.DuringWorkload, histogram(durs,
			hs(get200, []float64{0.001}, []uint64{0}, 0, 0), hs(get404, []float64{0.002}, []uint64{0}, 0, 0))),
		sample(1, experiment.DuringWorkload, histogram(durs,
			hs(get200, []float64{0.001}, []uint64{10}, 30, 1), hs(get404, []float64{0.002}, []uint64{5}, 5, 0.01))),
	))
	r := route(t, a.Windows[0].Routes, "GET", "/inventory/{sku}")
	if r.Latency != nil || !strings.Contains(r.LatencyUnavailable, "different bucket bounds") {
		t.Errorf("latency = %+v / %q", r.Latency, r.LatencyUnavailable)
	}
}

func TestFamilyOfWrongType(t *testing.T) {
	gauge := telemetry.Family{Name: reqs, Type: telemetry.Gauge, Series: []telemetry.Series{cs(get200, 5)}}
	a := analyze(t, report(
		sample(0, experiment.DuringWorkload, gauge),
		sample(1, experiment.DuringWorkload, gauge),
	))
	if len(a.Windows[0].Routes) != 0 || len(a.Totals) != 0 {
		t.Errorf("a gauge must not be read as a counter: routes = %+v", a.Windows[0].Routes)
	}
	w := warningsOf(a, WarnFamilyType)
	if len(w) != 2 || w[0].Sequence != 1 || w[1].Sequence != 2 || !strings.Contains(w[0].Message, "is a gauge in sample #1, not a counter") {
		t.Errorf("type warnings = %+v", w)
	}
}

func TestFamilyMissingFromOneSample(t *testing.T) {
	a := analyze(t, report(
		sample(0, experiment.DuringWorkload, counter(reqs, cs(get200, 5))),
		sample(1, experiment.DuringWorkload),
		sample(2, experiment.DuringWorkload, counter(reqs, cs(get200, 25))),
	))
	for i := range 2 {
		r := route(t, a.Windows[i].Routes, "GET", "/inventory/{sku}")
		if r.Requests != nil || !strings.Contains(r.RequestsUnavailable, "is not in sample #2") {
			t.Errorf("window %d requests = %+v / %q", i+1, r.Requests, r.RequestsUnavailable)
		}
	}
	if w := warningsOf(a, WarnFamilyMissing); len(w) != 2 || w[0].Metric != reqs || w[0].Sequence != 2 {
		t.Errorf("missing warnings = %+v", w) // the counter in sample 2, and the histogram everywhere
	}
}

func TestEmptyAndSingleSampleTelemetry(t *testing.T) {
	for _, tc := range []struct {
		name    string
		r       experiment.Report
		samples int
	}{
		{"no samples", report(failed(0, experiment.BeforeWorkload, "refused"), failed(1, experiment.DuringWorkload, "refused")), 0},
		{"one sample", report(sample(0, experiment.BeforeWorkload, counter(reqs, cs(get200, 1))), failed(1, experiment.DuringWorkload, "refused")), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := analyze(t, tc.r)
			if len(a.Windows) != 0 || len(a.Totals) != 0 || a.Experiment.Samples != tc.samples {
				t.Errorf("windows = %d, totals = %d, samples = %d", len(a.Windows), len(a.Totals), a.Experiment.Samples)
			}
			if !hasLimitation(a, "Fewer than two samples") {
				t.Errorf("limitations = %q", a.Limitations)
			}
			data, _ := json.Marshal(a)
			if !bytes.Contains(data, []byte(`"windows":[]`)) || !bytes.Contains(data, []byte(`"totals":[]`)) {
				t.Errorf("empty lists should be [] not null: %s", data)
			}
		})
	}
}

func TestNumericalEdgeCases(t *testing.T) {
	t.Run("counters above 2^53", func(t *testing.T) {
		big := float64(1 << 54)
		a := analyze(t, report(
			sample(0, experiment.DuringWorkload, counter(reqs, cs(get200, big))),
			sample(1, experiment.DuringWorkload, counter(reqs, cs(get200, big+1024))),
		))
		wantNear(t, "requests", route(t, a.Windows[0].Routes, "GET", "/inventory/{sku}").Requests.Requests, 1024)
		if w := warningsOf(a, WarnPrecision); len(w) != 1 {
			t.Errorf("precision warnings = %+v", w)
		}
	})
	t.Run("rate overflows", func(t *testing.T) {
		a := analyze(t, report(
			sample(0, experiment.DuringWorkload, counter(reqs, cs(get200, 0))),
			sample(0.001, experiment.DuringWorkload, counter(reqs, cs(get200, math.MaxFloat64))),
		))
		r := route(t, a.Windows[0].Routes, "GET", "/inventory/{sku}")
		if r.Requests != nil || !strings.Contains(r.RequestsUnavailable, "finite") {
			t.Errorf("requests = %+v / %q", r.Requests, r.RequestsUnavailable)
		}
		if w := warningsOf(a, WarnOverflow); len(w) != 1 {
			t.Errorf("overflow warnings = %+v", w)
		}
	})
	t.Run("fractional counters", func(t *testing.T) {
		a := analyze(t, report(
			sample(0, experiment.DuringWorkload, counter(reqs, cs(get200, 0.5))),
			sample(0.25, experiment.DuringWorkload, counter(reqs, cs(get200, 1.75))),
		))
		wantNear(t, "per second", route(t, a.Windows[0].Routes, "GET", "/inventory/{sku}").Requests.PerSecond, 5)
	})
	t.Run("negative histogram sum increase", func(t *testing.T) {
		a := analyze(t, report(
			sample(0, experiment.DuringWorkload, histogram(durs, hs(get200, []float64{1}, []uint64{0}, 0, 5))),
			sample(1, experiment.DuringWorkload, histogram(durs, hs(get200, []float64{1}, []uint64{30}, 30, 4))),
		))
		l := route(t, a.Windows[0].Routes, "GET", "/inventory/{sku}").Latency
		if l.MeanSeconds != nil || l.MeanSecondsUnavailable == "" || l.P95.Seconds == nil {
			t.Errorf("latency = %+v: want no mean but a p95", l)
		}
	})
	t.Run("NaN histogram sum", func(t *testing.T) {
		a := analyze(t, report(
			sample(0, experiment.DuringWorkload, histogram(durs, hs(get200, []float64{1}, []uint64{0}, 0, math.NaN()))),
			sample(1, experiment.DuringWorkload, histogram(durs, hs(get200, []float64{1}, []uint64{30}, 30, math.NaN()))),
		))
		if l := route(t, a.Windows[0].Routes, "GET", "/inventory/{sku}").Latency; l.MeanSeconds != nil {
			t.Errorf("mean = %v, want unavailable", *l.MeanSeconds)
		}
	})
}

func TestStatusClassification(t *testing.T) {
	for _, tc := range []struct {
		status string
		class  int
	}{{"200", 1}, {"302", 1}, {"399", 1}, {"400", 4}, {"404", 4}, {"499", 4}, {"500", 5}, {"503", 5}, {"599", 5},
		{"", 0}, {"abc", 0}, {"600", 0}, {"99", 0}, {"5xx", 0}, {"-500", 0}} {
		if got := statusClass(tc.status); got != tc.class {
			t.Errorf("statusClass(%q) = %d, want %d", tc.status, got, tc.class)
		}
	}
}

func TestUnclassifiedStatusesMakeRatioUnavailable(t *testing.T) {
	a := analyze(t, report(
		sample(0, experiment.DuringWorkload, counter(reqs, cs(labels("GET", "/x", "unknown"), 0), cs(labels("GET", "/x", "500"), 0))),
		sample(1, experiment.DuringWorkload, counter(reqs, cs(labels("GET", "/x", "unknown"), 10), cs(labels("GET", "/x", "500"), 1))),
	))
	r := route(t, a.Windows[0].Routes, "GET", "/x").Requests
	if r.ServerErrorRatio != nil || !strings.Contains(r.ServerErrorRatioUnavailable, "10 requests have no numeric") || r.Unclassified != 10 {
		t.Errorf("requests = %+v", r)
	}
}

// Other services use other names; nothing about the Inventory Service is
// assumed.
func TestConfigurableNames(t *testing.T) {
	l := func(code string) map[string]string {
		return map[string]string{"handler": "/api", "verb": "PUT", "code": code}
	}
	cfg := Config{RequestsCounter: "http_server_requests_total", DurationHistogram: "http_server_duration_seconds",
		MethodLabel: "verb", RouteLabel: "handler", StatusLabel: "code", MinObservations: 1}
	a := analyzeWith(t, report(
		sample(0, experiment.DuringWorkload, counter("http_server_requests_total", cs(l("200"), 1), cs(l("502"), 1)),
			histogram("http_server_duration_seconds", hs(l("200"), []float64{0.1}, []uint64{1}, 1, 0.01), hs(l("502"), []float64{0.1}, []uint64{0}, 1, 0.5))),
		sample(1, experiment.DuringWorkload, counter("http_server_requests_total", cs(l("200"), 4), cs(l("502"), 2)),
			histogram("http_server_duration_seconds", hs(l("200"), []float64{0.1}, []uint64{4}, 4, 0.04), hs(l("502"), []float64{0.1}, []uint64{0}, 2, 1))),
	), cfg)
	r := route(t, a.Windows[0].Routes, "PUT", "/api")
	wantNear(t, "5xx ratio", *r.Requests.ServerErrorRatio, 0.25)
	if r.Latency == nil || r.Latency.Observations != 4 {
		t.Errorf("latency = %+v / %q", r.Latency, r.LatencyUnavailable)
	}

	cfg = Config{RequestsCounter: reqs, DurationHistogram: durs, MinObservations: 1}
	a = analyzeWith(t, inventoryReport(), cfg)
	if len(a.Totals) != 1 || a.Totals[0].Method != "" || a.Totals[0].Route != "" {
		t.Fatalf("totals without route labels = %+v", a.Totals)
	}
	if r := a.Totals[0].Requests; r.ServerErrorRatio != nil || r.ServerErrorRatioUnavailable != "no status label is configured" {
		t.Errorf("5xx ratio without a status label = %+v", r)
	}
}

func TestInvalidConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Config)
		want string
	}{
		{"no counter", func(c *Config) { c.RequestsCounter = " " }, "requests counter name is required"},
		{"no histogram", func(c *Config) { c.DurationHistogram = "" }, "duration histogram name is required"},
		{"same family", func(c *Config) { c.DurationHistogram = c.RequestsCounter }, "must be different families"},
		{"same label", func(c *Config) { c.StatusLabel = "route" }, `the route and status labels must be different, both are "route"`},
		{"min observations", func(c *Config) { c.MinObservations = 0 }, "min observations must be between 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			tc.edit(&cfg)
			_, err := Analyze(inventoryReport(), cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// URLs are redacted again, and label values that hold URLs are redacted too,
// even though a report written by ruptiq-experiment is already redacted.
func TestCredentialRedaction(t *testing.T) {
	leaky := labels("GET", "http://user:hunter2@host/p?api_key=topsecret", "200")
	r := report(
		sample(0, experiment.DuringWorkload, counter(reqs, cs(leaky, 1))),
		sample(1, experiment.DuringWorkload, counter(reqs, cs(leaky, 2))),
		failed(2, experiment.DuringWorkload, "GET https://scraper:hunter2@m/metrics?token=topsecret failed"),
	)
	r.Config.TargetURL = "https://loaduser:hunter2@127.0.0.1/inventory?api_key=topsecret"
	r.Config.MetricsURL = "https://scraper:hunter2@127.0.0.1/metrics?token=topsecret"
	r.Reasons = []string{"see https://u:hunter2@x/?k=topsecret"}
	a := analyze(t, r)
	data, _ := json.Marshal(a)
	var summary bytes.Buffer
	if err := WriteSummary(&summary, a); err != nil {
		t.Fatal(err)
	}
	for _, out := range [][]byte{data, summary.Bytes()} {
		for _, secret := range []string{"hunter2", "topsecret", "loaduser", "scraper:", "user:"} {
			if bytes.Contains(out, []byte(secret)) {
				t.Errorf("output contains %q:\n%s", secret, out)
			}
		}
	}
	if a.Experiment.TargetURL != "https://xxxxx@127.0.0.1/inventory?xxxxx" {
		t.Errorf("target URL = %q", a.Experiment.TargetURL)
	}
}

func TestDeterministic(t *testing.T) {
	first, _ := json.Marshal(analyze(t, inventoryReport()))
	for range 20 {
		again, _ := json.Marshal(analyze(t, inventoryReport()))
		if !bytes.Equal(first, again) {
			t.Fatal("analysis output differs between runs")
		}
	}
}

// The Inventory fixture's numbers, worked out by hand; the golden files
// record the full output.
func TestInventoryExperiment(t *testing.T) {
	a := analyze(t, inventoryReport())
	if len(a.Windows) != 4 || len(a.Warnings) != 0 {
		t.Fatalf("windows = %d, warnings = %+v", len(a.Windows), a.Warnings)
	}
	get := route(t, a.Windows[0].Routes, "GET", "/inventory/{sku}")
	wantNear(t, "window 1 GET requests", get.Requests.Requests, 2020)
	wantNear(t, "window 1 GET per second", get.Requests.PerSecond, 2020/1.002)
	wantNear(t, "window 1 GET 5xx ratio", *get.Requests.ServerErrorRatio, 0)
	wantNear(t, "window 1 GET 4xx", get.Requests.ClientErrors, 20)
	// Interval buckets: 200s [0,500,1000,1920,1960,2000,...], 404s
	// [0,10,20,20,...]: total 2020, rank 1919 lies in (0.0005, 0.001], which
	// holds observations 1021..1940.
	wantNear(t, "window 1 GET p95", *get.Latency.P95.Seconds, 0.0005+0.0005*(1919-1020)/920)

	post := route(t, a.Windows[2].Routes, "POST", "/reservations")
	wantNear(t, "window 3 POST requests", post.Requests.Requests, 24)
	wantNear(t, "window 3 POST 5xx ratio", *post.Requests.ServerErrorRatio, 4.0/24)
	// Interval buckets: 201s add 10 at 0.0025 and 20 at 0.005, 503s add 4 at
	// 0.05: cumulative [..., 10, 20, 20, 20, 24]. Rank 22.8 lies in
	// (0.025, 0.05], which holds observations 21..24.
	if p := post.Latency.P95; p.Seconds == nil {
		t.Errorf("window 3 POST p95 = %+v", p)
	} else {
		wantNear(t, "window 3 POST p95", *p.Seconds, 0.025+0.025*2.8/4)
	}
	idle := route(t, a.Windows[3].Routes, "GET", "/inventory/{sku}")
	if idle.Requests.Requests != 0 || idle.Latency.P95.Seconds != nil {
		t.Errorf("after-workload window = %+v", idle)
	}

	tot := route(t, a.Totals, "GET", "/inventory/{sku}").Requests
	wantNear(t, "total GET requests", tot.Requests, 8040)
	wantNear(t, "total GET per second", tot.PerSecond, 8040/3.75)
	if !hasLimitation(a, "include time without load") {
		t.Errorf("limitations = %q", a.Limitations)
	}

	data, err := json.MarshalIndent(Report{SchemaVersion: SchemaVersion, Input: "inventory_experiment.json", Analysis: &a}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "inventory_analysis.golden.json", append(data, '\n'))
	var summary bytes.Buffer
	if err := WriteSummary(&summary, a); err != nil {
		t.Fatal(err)
	}
	golden(t, "inventory_summary.golden.txt", summary.Bytes())
	golden(t, "inventory_experiment.json", append(encode(t, inventoryReport()), '\n'))
}

// A report written by the real experiment coordinator (its golden file):
// one failed scrape, a generic counter named app_requests_total with a "code"
// label, and no histogram.
func TestCoordinatorReport(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "experiment", "testdata", "intermittent_report.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := DecodeExperiment(data)
	if err != nil {
		t.Fatalf("DecodeExperiment: %v", err)
	}
	cfg := Config{RequestsCounter: "app_requests_total", DurationHistogram: "app_request_duration_seconds",
		StatusLabel: "code", MinObservations: DefaultMinObservations}
	a, err := Analyze(r, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Windows) != 3 {
		t.Fatalf("windows = %d, want 3", len(a.Windows))
	}
	for i, want := range []struct{ elapsed, perSecond float64 }{{1, 1}, {2, 1}, {0.25, 4}} {
		r := a.Windows[i].Routes[0].Requests
		wantNear(t, "elapsed", r.ElapsedSeconds, want.elapsed)
		wantNear(t, "per second", r.PerSecond, want.perSecond)
	}
	if m := a.Windows[1].MissedScrapes; len(m) != 1 || m[0].Sequence != 3 || m[0].Reason != "scrape_failed" {
		t.Errorf("window 2 missed scrapes = %+v", m)
	}
	if a.Totals[0].LatencyUnavailable == "" || len(warningsOf(a, WarnFamilyMissing)) != 1 {
		t.Errorf("histogram missing everywhere: totals = %+v, warnings = %+v", a.Totals, a.Warnings)
	}
}
