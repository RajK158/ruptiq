package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RajK158/ruptiq/internal/analysis"
	"github.com/RajK158/ruptiq/internal/experiment"
	"github.com/RajK158/ruptiq/internal/fixvalidation"
	"github.com/RajK158/ruptiq/internal/investigation"
	"github.com/RajK158/ruptiq/internal/telemetry"
	"github.com/RajK158/ruptiq/internal/workload"
)

func TestCLIUndefinedErrorRatio(t *testing.T) {
	t.Run("both sides zero", func(t *testing.T) {
		stdout, out := runRatioCLI(t, 0, 0)
		if !strings.Contains(string(mustRead(t, out)), `"value_unavailable": "5xx ratio is undefined because no requests were completed"`) {
			t.Fatal("JSON omitted the undefined-ratio explanation")
		}
		before, after, evidence := errorMeasures(t, out)
		assertRawUndefined(t, before)
		assertRawUndefined(t, after)
		if evidence != 4 {
			t.Fatalf("evidence entries %d", evidence)
		}
		if got := readReport(t, out).Validation.Routes[0].ServerErrors.Status; got != fixvalidation.StatusInsufficient {
			t.Fatalf("status %s", got)
		}
		line := cliErrorLine(t, stdout)
		if strings.Contains(line, "0.00%") || !strings.Contains(line, "undefined (0 requests, 0 5xx) → undefined (0 requests, 0 5xx)") {
			t.Fatalf("summary %s", line)
		}
	})

	t.Run("one side zero", func(t *testing.T) {
		stdout, out := runRatioCLI(t, 1000, 0)
		before, after, evidence := errorMeasures(t, out)
		assertRawZeroRatio(t, before, 2000)
		assertRawUndefined(t, after)
		if evidence != 4 {
			t.Fatalf("evidence entries %d", evidence)
		}
		line := cliErrorLine(t, stdout)
		if !strings.Contains(line, "0.00% → undefined (0 requests, 0 5xx)") {
			t.Fatalf("summary %s", line)
		}
	})

	t.Run("real zero percent", func(t *testing.T) {
		stdout, out := runRatioCLI(t, 1000, 1000)
		if strings.Contains(string(mustRead(t, out)), "value_unavailable") {
			t.Fatal("a completed 0% ratio was marked undefined")
		}
		before, after, _ := errorMeasures(t, out)
		assertRawZeroRatio(t, before, 2000)
		assertRawZeroRatio(t, after, 2000)
		line := cliErrorLine(t, stdout)
		if !strings.Contains(line, "0.00% → 0.00%") || strings.Contains(line, "undefined") {
			t.Fatalf("summary %s", line)
		}
	})
}

func runRatioCLI(t *testing.T, beforeOK, afterOK float64) (stdout, outPath string) {
	t.Helper()
	dir := t.TempDir()
	before := writeAnalysis(t, filepath.Join(dir, "before.json"), beforeOK)
	after := writeAnalysis(t, filepath.Join(dir, "after.json"), afterOK)
	outPath = filepath.Join(dir, "validation.json")
	o := runCmd("-before", before, "-after", after, "-out", outPath)
	if o.code != exitCompared || o.stderr != "" {
		t.Fatalf("exit %d stderr %q\n%s", o.code, o.stderr, o.stdout)
	}
	return o.stdout, outPath
}

func writeAnalysis(t *testing.T, path string, ok float64) string {
	t.Helper()
	a := fxBuild(fxPair(2, fxRT().fxRequests(ok, 0, 0))...)
	data, err := json.Marshal(analysis.Report{SchemaVersion: analysis.SchemaVersion, Analysis: &a})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := investigation.DecodeAnalysis(data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func errorMeasures(t *testing.T, path string) (before, after map[string]any, evidence int) {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(mustRead(t, path), &doc); err != nil {
		t.Fatal(err)
	}
	validation, _ := doc["validation"].(map[string]any)
	routes, _ := validation["routes"].([]any)
	for _, item := range routes {
		route, _ := item.(map[string]any)
		if route["route"] != "/work" {
			continue
		}
		signal, _ := route["server_errors"].(map[string]any)
		before, _ = signal["before"].(map[string]any)
		after, _ = signal["after"].(map[string]any)
		refs, _ := signal["evidence"].([]any)
		return before, after, len(refs)
	}
	t.Fatal("GET /work server_errors missing")
	return nil, nil, 0
}

func assertRawUndefined(t *testing.T, m map[string]any) {
	t.Helper()
	value, ok := m["value"]
	if !ok || value != nil {
		t.Fatalf("value present=%v json=%#v measure=%v", ok, value, m)
	}
	if m["value_unavailable"] != "5xx ratio is undefined because no requests were completed" {
		t.Fatalf("value_unavailable %v", m["value_unavailable"])
	}
	if m["requests"] != 0.0 || m["server_errors_5xx"] != 0.0 || m["windows"] != 2.0 || m["elapsed_seconds"] != 2.0 {
		t.Fatalf("counts %v", m)
	}
}

func assertRawZeroRatio(t *testing.T, m map[string]any, requests float64) {
	t.Helper()
	if m["value"] != 0.0 || m["value_unavailable"] != nil {
		t.Fatalf("expected a real 0 ratio, got %v", m)
	}
	if m["requests"] != requests || m["server_errors_5xx"] != 0.0 {
		t.Fatalf("counts %v", m)
	}
}

func cliErrorLine(t *testing.T, text string) string {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "server_error_ratio:") {
			return line
		}
	}
	t.Fatalf("no server_error_ratio line:\n%s", text)
	return ""
}

// The fixture below is only large enough for the zero-request CLI cases.

var fxT0 = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

type fxSpec struct {
	ok, e5, e4 float64
}

func fxRT() fxSpec { return fxSpec{ok: 1000} }

func (s fxSpec) fxRequests(ok, e5, e4 float64) fxSpec {
	s.ok, s.e5, s.e4 = ok, e5, e4
	return s
}

type fxWindow struct {
	routes []fxSpec
}

func fxPair(n int, routes ...fxSpec) []fxWindow {
	out := make([]fxWindow, n)
	for i := range out {
		out[i] = fxWindow{routes: routes}
	}
	return out
}

func (s fxSpec) signals(elapsed float64, cfg analysis.Config) analysis.RouteSignals {
	total := s.ok + s.e5 + s.e4
	r := &analysis.RequestSignals{
		Requests: total, ElapsedSeconds: elapsed,
		ByStatus: []analysis.StatusCount{
			{Status: "200", Requests: s.ok},
			{Status: "404", Requests: s.e4},
			{Status: "500", Requests: s.e5},
		},
		ServerErrors: s.e5, ClientErrors: s.e4,
		Evidence: analysis.Evidence{Metric: cfg.RequestsCounter, Series: []map[string]string{
			{"method": "GET", "route": "/work", "status": "200"},
		}},
	}
	if elapsed > 0 {
		r.PerSecond = total / elapsed
	}
	if total > 0 {
		r.ServerErrorRatio = fxPtr(s.e5 / total)
	} else {
		r.ServerErrorRatioUnavailable = "no requests in this window"
	}
	bounds := []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1}
	buckets := make([]analysis.Bucket, len(bounds))
	for i, b := range bounds {
		c := uint64(0)
		if i == len(bounds)-1 {
			c = 1000
		}
		buckets[i] = analysis.Bucket{UpperBound: telemetry.Float(b), CumulativeCount: c}
	}
	p95, lo, hi, mean := 0.008, 0.005, 0.01, 0.004
	return analysis.RouteSignals{
		Method: "GET", Route: "/work", Requests: r,
		Latency: &analysis.LatencySignals{
			Observations: 1000, MeanSeconds: &mean,
			P95: analysis.QuantileEstimate{
				Quantile: analysis.Quantile, Estimate: true,
				Seconds: &p95, BucketLowerSeconds: &lo, BucketUpperSeconds: &hi,
			},
			Buckets:  buckets,
			Evidence: analysis.Evidence{Metric: cfg.DurationHistogram, Series: []map[string]string{{"method": "GET", "route": "/work", "status": "200"}}},
		},
	}
}

func fxBuild(ws ...fxWindow) analysis.Analysis {
	cfg := analysis.DefaultConfig()
	a := analysis.Analysis{
		Experiment: analysis.ExperimentInfo{
			SchemaVersion: experiment.ReportSchemaVersion, Status: experiment.StatusPassed,
			StartedAt: fxT0, FinishedAt: fxT0.Add(time.Minute),
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
		from := analysis.SampleRef{Sequence: seq, Phase: experiment.DuringWorkload, ScrapedAt: fxAt(offset), OffsetSeconds: offset}
		seq++
		offset++
		win := analysis.Window{
			Index: i + 1, From: from, ElapsedSeconds: 1, MissedScrapes: []analysis.MissedScrape{},
			To: analysis.SampleRef{Sequence: seq, Phase: experiment.DuringWorkload, ScrapedAt: fxAt(offset), OffsetSeconds: offset},
		}
		win.ElapsedSeconds = win.To.ScrapedAt.Sub(win.From.ScrapedAt).Seconds()
		for _, s := range x.routes {
			win.Routes = append(win.Routes, s.signals(win.ElapsedSeconds, cfg))
		}
		a.Windows = append(a.Windows, win)
	}
	return a
}

func fxPtr(x float64) *float64 { return &x }

func fxAt(s float64) time.Time {
	return fxT0.Add(time.Duration(math.Round(s * float64(time.Second))))
}
