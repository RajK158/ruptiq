package analysis

import (
	"bytes"
	"encoding/json"
	"flag"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/RajK158/ruptiq/internal/experiment"
	"github.com/RajK158/ruptiq/internal/telemetry"
	"github.com/RajK158/ruptiq/internal/workload"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	want = bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n"))
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the golden file (run with -update after checking):\n%s", name, got)
	}
}

var t0 = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

const (
	reqs = DefaultRequestsCounter
	durs = DefaultDurationHistogram
)

// inventoryBounds are the Inventory Service's bucket bounds.
var inventoryBounds = []float64{0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// entry is one scrape attempt of a fixture: a sample (families set), a failed
// scrape (err set), or a sample not retained (notRetained set).
type entry struct {
	at          float64 // seconds after t0
	phase       experiment.Phase
	families    []telemetry.Family
	err         string
	notRetained bool
}

func sample(at float64, phase experiment.Phase, families ...telemetry.Family) entry {
	if families == nil {
		families = []telemetry.Family{}
	}
	return entry{at: at, phase: phase, families: families}
}

func failed(at float64, phase experiment.Phase, msg string) entry {
	return entry{at: at, phase: phase, err: msg}
}

func dropped(at float64, phase experiment.Phase) entry {
	return entry{at: at, phase: phase, notRetained: true}
}

func at(s float64) time.Time {
	return t0.Add(time.Duration(math.Round(s * float64(time.Second))))
}

func labels(method, route, status string) map[string]string {
	return map[string]string{"method": method, "route": route, "status": status}
}

func counter(name string, series ...telemetry.Series) telemetry.Family {
	if series == nil {
		series = []telemetry.Series{}
	}
	return telemetry.Family{Name: name, Type: telemetry.Counter, Series: series}
}

func cs(l map[string]string, v float64) telemetry.Series {
	f := telemetry.Float(v)
	return telemetry.Series{Labels: l, Value: &f}
}

func histogram(name string, series ...telemetry.Series) telemetry.Family {
	if series == nil {
		series = []telemetry.Series{}
	}
	return telemetry.Family{Name: name, Type: telemetry.Histogram, Series: series}
}

// hs is a histogram series with the given finite bounds, their cumulative
// counts, and a +Inf bucket holding count.
func hs(l map[string]string, bounds []float64, cumulative []uint64, count uint64, sum float64) telemetry.Series {
	h := &telemetry.HistogramValue{Count: count, Sum: telemetry.Float(sum)}
	for i, b := range bounds {
		h.Buckets = append(h.Buckets, telemetry.Bucket{UpperBound: telemetry.Float(b), CumulativeCount: cumulative[i]})
	}
	h.Buckets = append(h.Buckets, telemetry.Bucket{UpperBound: telemetry.Float(math.Inf(1)), CumulativeCount: count})
	return telemetry.Series{Labels: l, Histogram: h}
}

// report builds a consistent experiment report from its scrape attempts, in
// order. The workload passed; the status follows from the telemetry.
func report(entries ...entry) experiment.Report {
	r := experiment.Report{
		SchemaVersion: experiment.ReportSchemaVersion,
		StartedAt:     t0,
		Config: experiment.ReportConfig{
			TargetURL: "http://127.0.0.1:8082/inventory/LAPTOP-001", VUs: 2, DurationSeconds: 3,
			MetricsURL: "http://127.0.0.1:8082/metrics", SampleIntervalSeconds: 1, MaxSamples: 1000,
			MaxTelemetryBytes: experiment.DefaultMaxTelemetryBytes, TimeoutSeconds: 63,
		},
		Workload: experiment.WorkloadOutcome{
			Result: &workload.Result{
				Runner: "k6", RunnerVersion: "2.3.0", TargetURL: "http://127.0.0.1:8082/inventory/LAPTOP-001",
				VUs: 2, ConfiguredDurationSeconds: 3, StartedAt: at(0.01), FinishedAt: at(3.1),
				Metrics: workload.Metrics{DurationSeconds: 3.0004, HTTPRequests: 9000, FailureRate: 0,
					RequestsPerSecond: 9000 / 3.0004,
					LatencyMs:         workload.Latency{Avg: 0.42, Min: 0.1, P50: 0.4, P90: 0.6, P95: 0.68, Max: 8.57}},
			},
			Verdict: workload.Verdict{Passed: true},
		},
		Telemetry: experiment.TelemetryOutcome{
			Samples: []experiment.Sample{}, Errors: []experiment.ScrapeError{}, NotRetained: []experiment.NotRetained{},
		},
	}
	t := &r.Telemetry
	end := 0.0
	has := map[experiment.Phase]bool{}
	for i, e := range entries {
		seq := i + 1
		switch {
		case e.err != "":
			t.Errors = append(t.Errors, experiment.ScrapeError{Sequence: seq, Phase: e.phase, AttemptedAt: at(e.at), OffsetSeconds: e.at, Error: e.err})
		case e.notRetained:
			t.NotRetained = append(t.NotRetained, experiment.NotRetained{Sequence: seq, Phase: e.phase, ScrapedAt: at(e.at), OffsetSeconds: e.at, SizeBytes: 4096})
		default:
			t.Samples = append(t.Samples, experiment.Sample{Sequence: seq, Phase: e.phase, ScrapedAt: at(e.at), OffsetSeconds: e.at,
				ScrapeDurationSeconds: 0.002, Families: e.families})
			has[e.phase] = true
			size, _ := json.Marshal(t.Samples[len(t.Samples)-1])
			t.RetainedBytes += int64(len(size))
		}
		end = e.at
	}
	r.FinishedAt = at(end + 0.25)
	t.Attempts = len(entries)
	t.Failed = len(t.Errors)
	t.Succeeded = len(t.Samples) + len(t.NotRetained)
	t.RetentionLimitReached = len(t.NotRetained) > 0
	switch {
	case len(t.Samples) == 0:
		t.Completeness = experiment.None
	case t.Failed == 0 && len(t.NotRetained) == 0 && has[experiment.BeforeWorkload] && has[experiment.DuringWorkload] && has[experiment.AfterWorkload]:
		t.Completeness = experiment.Complete
	default:
		t.Completeness = experiment.Partial
	}
	if t.Completeness == experiment.Complete {
		r.Status = experiment.StatusPassed
	} else {
		r.Status = experiment.StatusIncomplete
		r.Reasons = []string{"telemetry is " + string(t.Completeness)}
		t.CompletenessReasons = []string{"see the samples"}
	}
	return r
}

// encode returns r as the JSON an experiment writes.
func encode(t *testing.T, r experiment.Report) []byte {
	t.Helper()
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// roundTrip encodes r and decodes it as the analyzer does, so every test also
// exercises the validation.
func roundTrip(t *testing.T, r experiment.Report) experiment.Report {
	t.Helper()
	got, err := DecodeExperiment(encode(t, r))
	if err != nil {
		t.Fatalf("DecodeExperiment: %v", err)
	}
	return got
}

// analyze round-trips r and analyses it with the default config.
func analyze(t *testing.T, r experiment.Report) Analysis {
	t.Helper()
	return analyzeWith(t, r, DefaultConfig())
}

func analyzeWith(t *testing.T, r experiment.Report, cfg Config) Analysis {
	t.Helper()
	a, err := Analyze(roundTrip(t, r), cfg)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if _, err := json.Marshal(a); err != nil {
		t.Fatalf("analysis cannot be written as JSON: %v", err)
	}
	return a
}

// route returns the signals of method and route in rs.
func route(t *testing.T, rs []RouteSignals, method, path string) RouteSignals {
	t.Helper()
	for _, r := range rs {
		if r.Method == method && r.Route == path {
			return r
		}
	}
	t.Fatalf("no signals for %s %s in %+v", method, path, rs)
	return RouteSignals{}
}

func warningsOf(a Analysis, kind string) []Warning {
	var out []Warning
	for _, w := range a.Warnings {
		if w.Kind == kind {
			out = append(out, w)
		}
	}
	return out
}

// near compares floats to within rounding.
func near(got, want float64) bool {
	return math.Abs(got-want) <= 1e-9*math.Max(1, math.Abs(want))
}

func wantNear(t *testing.T, name string, got, want float64) {
	t.Helper()
	if !near(got, want) {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

// inventoryReport is a realistic Inventory Service experiment: a baseline
// with earlier traffic, three samples during the workload at uneven
// intervals, and one after. GET /inventory/{sku} returns 200s and some 404s;
// POST /reservations has a few 503s.
func inventoryReport() experiment.Report {
	get200 := labels("GET", "/inventory/{sku}", "200")
	get404 := labels("GET", "/inventory/{sku}", "404")
	post201 := labels("POST", "/reservations", "201")
	post503 := labels("POST", "/reservations", "503")
	b := inventoryBounds
	snap := func(g200, g404, p201, p503 float64, hg200, hg404, hp201, hp503 telemetry.Series) []telemetry.Family {
		return []telemetry.Family{
			histogram(durs, hg200, hg404, hp201, hp503),
			counter(reqs, cs(get200, g200), cs(get404, g404), cs(post201, p201), cs(post503, p503)),
			{Name: "inventory_http_requests_in_flight", Type: telemetry.Gauge, Series: []telemetry.Series{cs(nil, 1)}},
		}
	}
	// Cumulative bucket counts for each sample: 16 finite bounds.
	g200 := func(n uint64, slow uint64) telemetry.Series {
		c := []uint64{0, n / 4, n / 2, n - slow, n - slow/2, n, n, n, n, n, n, n, n, n, n, n}
		return hs(get200, b, c, n, float64(n)*0.0004)
	}
	g404 := func(n uint64) telemetry.Series {
		c := []uint64{0, n / 2, n, n, n, n, n, n, n, n, n, n, n, n, n, n}
		return hs(get404, b, c, n, float64(n)*0.0002)
	}
	p201 := func(n uint64) telemetry.Series {
		c := []uint64{0, 0, 0, 0, n / 2, n, n, n, n, n, n, n, n, n, n, n}
		return hs(post201, b, c, n, float64(n)*0.003)
	}
	p503 := func(n uint64) telemetry.Series {
		c := []uint64{0, 0, 0, 0, 0, 0, 0, 0, n, n, n, n, n, n, n, n}
		return hs(post503, b, c, n, float64(n)*0.04)
	}
	return report(
		sample(0.000, experiment.BeforeWorkload, snap(1000, 10, 100, 2, g200(1000, 40), g404(10), p201(100), p503(2))...),
		sample(1.002, experiment.DuringWorkload, snap(3000, 30, 140, 4, g200(3000, 120), g404(30), p201(140), p503(4))...),
		sample(2.010, experiment.DuringWorkload, snap(6000, 30, 180, 4, g200(6000, 240), g404(30), p201(180), p503(4))...),
		sample(3.500, experiment.DuringWorkload, snap(9000, 50, 200, 8, g200(9000, 360), g404(50), p201(200), p503(8))...),
		sample(3.750, experiment.AfterWorkload, snap(9000, 50, 200, 8, g200(9000, 360), g404(50), p201(200), p503(8))...),
	)
}
