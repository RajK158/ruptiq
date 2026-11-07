package investigation

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/RajK158/ruptiq/internal/analysis"
	"github.com/RajK158/ruptiq/internal/experiment"
	"github.com/RajK158/ruptiq/internal/telemetry"
	"github.com/RajK158/ruptiq/internal/workload"
)

var t0 = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

// spec is one route's signals in one window of a fake analysis. Request
// counts are per window; the window's length turns them into rates.
type spec struct {
	method, route    string
	ok, e5, e4       float64
	noRequests       string
	p95, lo, hi      float64
	obs              uint64
	mean             float64
	noLatency, noP95 string
}

func rt(method, route string) spec {
	return spec{method: method, route: route, ok: 1000, obs: 1000, p95: 0.0008, lo: 0.0005, hi: 0.001, mean: 0.0004}
}

func get() spec  { return rt("GET", "/inventory/{sku}") }
func post() spec { return rt("POST", "/reservations") }

func (s spec) requests(ok, e5, e4 float64) spec {
	s.ok, s.e5, s.e4 = ok, e5, e4
	return s
}

func (s spec) latency(p95, lo, hi float64) spec {
	s.p95, s.lo, s.hi = p95, lo, hi
	return s
}

func (s spec) observations(n uint64) spec {
	s.obs = n
	return s
}

func (s spec) meanSeconds(m float64) spec {
	s.mean = m
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

// wspec is one window of a fake analysis.
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

func (x wspec) missing(n int) wspec {
	x.missed = n
	return x
}

func labels(method, route, status string) map[string]string {
	return map[string]string{"method": method, "route": route, "status": status}
}

func ptr(x float64) *float64 { return &x }

func (s spec) signals(elapsed float64, cfg analysis.Config) analysis.RouteSignals {
	rs := analysis.RouteSignals{Method: s.method, Route: s.route}
	if s.noRequests != "" {
		rs.RequestsUnavailable = s.noRequests
	} else {
		total := s.ok + s.e5 + s.e4
		r := &analysis.RequestSignals{
			Requests: total, ElapsedSeconds: elapsed, PerSecond: total / elapsed,
			ByStatus:     []analysis.StatusCount{{Status: "200", Requests: s.ok}, {Status: "404", Requests: s.e4}, {Status: "500", Requests: s.e5}},
			ServerErrors: s.e5, ClientErrors: s.e4,
			Evidence: analysis.Evidence{Metric: cfg.RequestsCounter, Series: []map[string]string{
				labels(s.method, s.route, "200"), labels(s.method, s.route, "404"), labels(s.method, s.route, "500")}},
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
	l := &analysis.LatencySignals{
		Observations: s.obs,
		P95:          analysis.QuantileEstimate{Quantile: analysis.Quantile, Estimate: true},
		Buckets:      []analysis.Bucket{{UpperBound: telemetry.Float(math.Inf(1)), CumulativeCount: s.obs}},
		Evidence:     analysis.Evidence{Metric: cfg.DurationHistogram, Series: []map[string]string{labels(s.method, s.route, "200")}},
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

func at(s float64) time.Time {
	return t0.Add(time.Duration(math.Round(s * float64(time.Second))))
}

// build returns a consistent analysis of the windows, in order.
func build(ws ...wspec) analysis.Analysis {
	cfg := analysis.DefaultConfig()
	a := analysis.Analysis{
		Experiment: analysis.ExperimentInfo{
			SchemaVersion: experiment.ReportSchemaVersion, Status: experiment.StatusPassed, StartedAt: t0, FinishedAt: t0.Add(time.Minute),
			TargetURL: "http://127.0.0.1:8082/inventory/LAPTOP-001", MetricsURL: "http://127.0.0.1:8082/metrics",
			VUs: 2, DurationSeconds: 10, SampleIntervalSeconds: 1, Completeness: experiment.Complete,
		},
		Config:      cfg,
		Client:      analysis.ClientSignals{Source: "k6 (client side)", Metrics: workload.Metrics{HTTPRequests: 1}, Passed: true},
		Totals:      []analysis.RouteSignals{},
		Windows:     []analysis.Window{},
		Warnings:    []analysis.Warning{},
		Limitations: []string{"fake"},
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

// steady returns n windows of the same routes.
func steady(n int, routes ...spec) []wspec {
	var out []wspec
	for range n {
		out = append(out, w(routes...))
	}
	return out
}

func file(t *testing.T, a analysis.Analysis) []byte {
	t.Helper()
	data, err := json.MarshalIndent(analysis.Report{SchemaVersion: analysis.SchemaVersion, Input: "analysis.json", Analysis: &a}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// investigate round-trips a through the analysis file format, so every test
// also exercises the validation, and investigates it.
func investigate(t *testing.T, a analysis.Analysis) Investigation {
	t.Helper()
	return investigateWith(t, a, DefaultConfig())
}

func investigateWith(t *testing.T, a analysis.Analysis, cfg Config) Investigation {
	t.Helper()
	decoded, err := DecodeAnalysis(file(t, a))
	if err != nil {
		t.Fatalf("DecodeAnalysis: %v", err)
	}
	inv, err := Investigate(decoded, cfg)
	if err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if _, err := json.Marshal(inv); err != nil {
		t.Fatalf("investigation cannot be written as JSON: %v", err)
	}
	return inv
}

func findingsOf(inv Investigation, typ string) []Finding {
	var out []Finding
	for _, f := range inv.Findings {
		if f.Type == typ {
			out = append(out, f)
		}
	}
	return out
}

func onlyFinding(t *testing.T, inv Investigation, typ string) Finding {
	t.Helper()
	fs := findingsOf(inv, typ)
	if len(fs) != 1 {
		t.Fatalf("%d %s findings, want 1: %+v", len(fs), typ, inv.Findings)
	}
	return fs[0]
}

func assessment(t *testing.T, inv Investigation, method, signal string) Assessment {
	t.Helper()
	for _, a := range inv.Assessments {
		if a.Method == method && a.Signal == signal {
			return a
		}
	}
	t.Fatalf("no %s assessment for %s", signal, method)
	return Assessment{}
}

func near(got, want float64) bool {
	return math.Abs(got-want) <= 1e-9*math.Max(1, math.Abs(want))
}

func wantNear(t *testing.T, name string, got, want float64) {
	t.Helper()
	if !near(got, want) {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

// resolve follows an RFC 6901 JSON pointer in a decoded JSON document.
func resolve(t *testing.T, doc any, pointer string) any {
	t.Helper()
	if !strings.HasPrefix(pointer, "/") {
		t.Fatalf("pointer %q does not start with /", pointer)
	}
	cur := doc
	for _, tok := range strings.Split(pointer[1:], "/") {
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
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
			t.Fatalf("pointer %q: cannot descend into %T", pointer, cur)
		}
	}
	return cur
}

func decodeJSON(t *testing.T, data []byte) any {
	t.Helper()
	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}
