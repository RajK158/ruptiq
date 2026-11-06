package k6

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RajK158/ruptiq/internal/workload"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// The fixtures are unedited summaries written by k6 v2.3.0.

func TestParseSummaryHealthyRun(t *testing.T) {
	got, err := parseSummary(readFixture(t, "summary_ok.json"))
	if err != nil {
		t.Fatal(err)
	}
	m := got.Metrics
	if got.K6Version != "2.3.0" {
		t.Errorf("K6Version = %q", got.K6Version)
	}
	if m.HTTPRequests != 12355 || m.FailedRequests != 0 || m.FailureRate != 0 {
		t.Errorf("requests = %d, failed = %d, rate = %v; want 12355, 0, 0", m.HTTPRequests, m.FailedRequests, m.FailureRate)
	}
	if !near(m.DurationSeconds, 3.0006048) || !near(m.RequestsPerSecond, 12355/3.0006048) {
		t.Errorf("duration = %v, rps = %v", m.DurationSeconds, m.RequestsPerSecond)
	}
	want := workload.Latency{Avg: 0.4210809955483617, Min: 0, P50: 0.5296, P90: 0.618, P95: 0.6818, Max: 8.5694}
	if m.LatencyMs != want {
		t.Errorf("latency = %+v, want %+v", m.LatencyMs, want)
	}
}

func TestParseSummaryEveryRequestFailed(t *testing.T) {
	// k6 exits 0 here (target refused every connection); the failure is only
	// visible in the metrics.
	got, err := parseSummary(readFixture(t, "summary_all_failed.json"))
	if err != nil {
		t.Fatal(err)
	}
	m := got.Metrics
	if m.HTTPRequests != 4078 || m.FailedRequests != 4078 || m.FailureRate != 1 {
		t.Fatalf("metrics = %+v, want 4078 requests all failed", m)
	}
	if v := workload.Evaluate(m, 0); v.Passed {
		t.Fatalf("Evaluate = %+v, want failed", v)
	}
}

func TestParseSummaryNoRequests(t *testing.T) {
	_, err := parseSummary(readFixture(t, "summary_no_requests.json"))
	if !errors.Is(err, ErrNoRequests) {
		t.Fatalf("err = %v, want ErrNoRequests", err)
	}
}

// mutate decodes the healthy fixture, applies edit, and re-encodes it.
func mutate(t *testing.T, edit func(doc map[string]any)) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(readFixture(t, "summary_ok.json"), &doc); err != nil {
		t.Fatal(err)
	}
	edit(doc)
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func metricList(doc map[string]any) []any {
	return doc["results"].(map[string]any)["metrics"].([]any)
}

func setMetrics(doc map[string]any, metrics []any) {
	doc["results"].(map[string]any)["metrics"] = metrics
}

func metric(doc map[string]any, name string) map[string]any {
	for _, m := range metricList(doc) {
		if mm := m.(map[string]any); mm["name"] == name {
			return mm
		}
	}
	panic("fixture has no metric " + name)
}

func values(doc map[string]any, name string) map[string]any {
	return metric(doc, name)["values"].(map[string]any)
}

func removeMetric(doc map[string]any, name string) {
	var kept []any
	for _, m := range metricList(doc) {
		if m.(map[string]any)["name"] != name {
			kept = append(kept, m)
		}
	}
	setMetrics(doc, kept)
}

func TestParseSummaryRejectsBadInput(t *testing.T) {
	for _, tc := range []struct {
		name string
		data func(t *testing.T) []byte
		want string
	}{
		{"empty file", func(*testing.T) []byte { return nil }, "not valid JSON"},
		{"truncated", func(t *testing.T) []byte { d := readFixture(t, "summary_ok.json"); return d[:len(d)/2] }, "not valid JSON"},
		{"not an object", func(*testing.T) []byte { return []byte(`[1,2]`) }, "not valid JSON"},
		{"legacy format", func(*testing.T) []byte {
			return []byte(`{"metrics":{"http_reqs":{"count":10,"rate":5}}}`)
		}, "unsupported summary format version"},
		{"future version", func(t *testing.T) []byte {
			return mutate(t, func(d map[string]any) { d["version"] = "2.0.0" })
		}, "unsupported summary format version"},
		{"missing duration", func(t *testing.T) []byte {
			return mutate(t, func(d map[string]any) { delete(d["config"].(map[string]any), "duration") })
		}, "config.duration is missing"},
		{"null duration", func(t *testing.T) []byte {
			return mutate(t, func(d map[string]any) { d["config"].(map[string]any)["duration"] = nil })
		}, "config.duration is missing"},
		{"zero duration", func(t *testing.T) []byte {
			return mutate(t, func(d map[string]any) { d["config"].(map[string]any)["duration"] = 0 })
		}, "positive number"},
		{"string duration", func(t *testing.T) []byte {
			return mutate(t, func(d map[string]any) { d["config"].(map[string]any)["duration"] = "5s" })
		}, "not valid JSON"},
		{"no metrics", func(t *testing.T) []byte {
			return mutate(t, func(d map[string]any) { setMetrics(d, nil) })
		}, ErrNoRequests.Error()},
		{"missing http_req_failed", func(t *testing.T) []byte {
			return mutate(t, func(d map[string]any) { removeMetric(d, "http_req_failed") })
		}, "http_req_failed is missing"},
		{"missing http_req_duration", func(t *testing.T) []byte {
			return mutate(t, func(d map[string]any) { removeMetric(d, "http_req_duration") })
		}, "http_req_duration is missing"},
		{"missing p95", func(t *testing.T) []byte {
			return mutate(t, func(d map[string]any) { delete(values(d, "http_req_duration"), "p95") })
		}, `no "p95" value`},
		{"null p95", func(t *testing.T) []byte {
			return mutate(t, func(d map[string]any) { values(d, "http_req_duration")["p95"] = nil })
		}, `no "p95" value`},
		{"negative latency", func(t *testing.T) []byte {
			return mutate(t, func(d map[string]any) { values(d, "http_req_duration")["avg"] = -1 })
		}, "avg is negative"},
		{"latency not time", func(t *testing.T) []byte {
			return mutate(t, func(d map[string]any) { metric(d, "http_req_duration")["contains"] = "default" })
		}, "expected time"},
		{"missing request count", func(t *testing.T) []byte {
			return mutate(t, func(d map[string]any) { delete(values(d, "http_reqs"), "count") })
		}, `no "count" value`},
		{"fractional request count", func(t *testing.T) []byte {
			return mutate(t, func(d map[string]any) { values(d, "http_reqs")["count"] = 10.5 })
		}, "whole number"},
		{"negative request count", func(t *testing.T) []byte {
			return mutate(t, func(d map[string]any) { values(d, "http_reqs")["count"] = -1 })
		}, "whole number"},
		{"wrong metric type", func(t *testing.T) []byte {
			return mutate(t, func(d map[string]any) { metric(d, "http_reqs")["type"] = "rate" })
		}, `expected "counter"`},
		{"failures exceed total", func(t *testing.T) []byte {
			return mutate(t, func(d map[string]any) { values(d, "http_req_failed")["matches"] = 99999 })
		}, "inconsistent"},
		{"failure rate above 1", func(t *testing.T) []byte {
			return mutate(t, func(d map[string]any) { values(d, "http_req_failed")["rate"] = 1.5 })
		}, "inconsistent"},
		{"duplicate metric", func(t *testing.T) []byte {
			return mutate(t, func(d map[string]any) {
				setMetrics(d, append(metricList(d), metric(d, "http_reqs")))
			})
		}, "appears more than once"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseSummary(tc.data(t))
			if err == nil {
				t.Fatal("parseSummary succeeded, want error")
			}
			if !errors.Is(err, ErrSummary) && !errors.Is(err, ErrNoRequests) {
				t.Errorf("err = %v, want ErrSummary or ErrNoRequests", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}
