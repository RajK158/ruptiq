package investigation

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goldenAnalysis is a saved ruptiq-analyze report of the synthetic Inventory
// Service experiment.
var goldenAnalysis = filepath.Join("..", "analysis", "testdata", "inventory_analysis.golden.json")

func readGolden(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(goldenAnalysis)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// edit decodes data, applies fn, and encodes it again.
func edit(t *testing.T, data []byte, fn func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	fn(m)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// obj walks maps and slices by key or index.
func obj(v any, keys ...any) map[string]any {
	for _, k := range keys {
		switch k := k.(type) {
		case string:
			v = v.(map[string]any)[k]
		case int:
			v = v.([]any)[k]
		}
	}
	return v.(map[string]any)
}

func TestDecodeGoldenAnalysis(t *testing.T) {
	a, err := DecodeAnalysis(readGolden(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Windows) != 4 || len(a.Totals) != 2 || a.Experiment.Status != "passed" {
		t.Errorf("analysis: %d windows, %d totals, status %s", len(a.Windows), len(a.Totals), a.Experiment.Status)
	}
	if _, err := DecodeAnalysis(file(t, allFindings())); err != nil {
		t.Errorf("fixture: %v", err)
	}
}

func TestDecodeRejectsBadFiles(t *testing.T) {
	valid := readGolden(t)
	for _, tc := range []struct {
		name string
		data []byte
		is   error
		want string
	}{
		{"empty", nil, ErrInvalidAnalysis, "not valid JSON"},
		{"not JSON", []byte("analysis"), ErrInvalidAnalysis, "not valid JSON"},
		{"truncated", valid[:len(valid)/2], ErrInvalidAnalysis, "not valid JSON"},
		{"trailing data", append(bytes.Clone(valid), []byte(" {}")...), ErrInvalidAnalysis, "unexpected data after the analysis"},
		{"no schema", []byte(`{"analysis": {}}`), ErrInvalidAnalysis, "schema_version is missing"},
		{"null schema", []byte(`{"schema_version": null}`), ErrInvalidAnalysis, "schema_version is missing"},
		{"text schema", []byte(`{"schema_version": "1"}`), ErrInvalidAnalysis, "schema_version is not an integer"},
		{"future schema", []byte(`{"schema_version": 2}`), ErrUnsupportedSchema, "schema_version 2 (supported: 1)"},
		{"neither", []byte(`{"schema_version": 1}`), ErrInvalidAnalysis, "analysis is missing"},
		{"failed analysis", []byte(`{"schema_version": 1, "input": "e.json", "error": "invalid experiment report: telemetry.attempts is 6"}`),
			ErrNotInvestigable, "records a failed analysis: invalid experiment report: telemetry.attempts is 6"},
		{"both", edit(t, valid, func(m map[string]any) { m["error"] = "boom" }), ErrInvalidAnalysis, "both analysis and error are set"},
		{"unknown field", edit(t, valid, func(m map[string]any) { m["diagnosis"] = "cpu" }), ErrInvalidAnalysis, `unknown field "diagnosis"`},
		{"unknown nested field", edit(t, valid, func(m map[string]any) { obj(m, "analysis", "windows", 0)["p99"] = 1 }), ErrInvalidAnalysis, `unknown field "p99"`},
		{"wrong type", edit(t, valid, func(m map[string]any) { obj(m, "analysis", "windows", 0)["index"] = "one" }), ErrInvalidAnalysis, "not valid JSON"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeAnalysis(tc.data)
			if !errors.Is(err, tc.is) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %v containing %q", err, tc.is, tc.want)
			}
		})
	}
}

func TestDecodeRejectsInconsistentAnalyses(t *testing.T) {
	valid := readGolden(t)
	route := func(m map[string]any, w, r int) map[string]any { return obj(m, "analysis", "windows", w, "routes", r) }
	for _, tc := range []struct {
		name string
		fn   func(m map[string]any)
		want string
	}{
		{"status", func(m map[string]any) { obj(m, "analysis", "experiment")["status"] = "cancelled" }, `experiment.status "cancelled" is not an analysed status`},
		{"experiment schema", func(m map[string]any) { obj(m, "analysis", "experiment")["schema_version"] = 9 }, "experiment.schema_version is 9"},
		{"config", func(m map[string]any) { obj(m, "analysis", "config")["min_observations"] = 0 }, "config: invalid analysis config"},
		{"windows missing", func(m map[string]any) { delete(obj(m, "analysis"), "windows") }, "windows is missing"},
		{"warnings missing", func(m map[string]any) { delete(obj(m, "analysis"), "warnings") }, "warnings is missing"},
		{"index", func(m map[string]any) { obj(m, "analysis", "windows", 1)["index"] = 7 }, "windows[1]: index is 7, want 2"},
		{"phase", func(m map[string]any) { obj(m, "analysis", "windows", 1, "from")["phase"] = "warmup" }, "windows[1]: unknown sample phase"},
		{"chain", func(m map[string]any) {
			obj(m, "analysis", "windows", 2, "from")["sequence"] = 1
		}, "windows[2]: does not start at the sample the previous window ends at"},
		{"elapsed", func(m map[string]any) { obj(m, "analysis", "windows", 1)["elapsed_seconds"] = 5 }, "windows[1]: elapsed_seconds 5 does not match"},
		{"zero elapsed", func(m map[string]any) { obj(m, "analysis", "windows", 1)["elapsed_seconds"] = 0 }, "elapsed_seconds must be positive"},
		{"missed scrape", func(m map[string]any) {
			obj(m, "analysis", "windows", 1)["missed_scrapes"] = []any{map[string]any{"sequence": 9, "phase": "during_workload", "reason": "scrape_failed"}}
		}, "windows[1].missed_scrapes[0]: sequence 9 is outside the window"},
		{"rate", func(m map[string]any) { obj(route(m, 1, 0), "requests")["per_second"] = 9999 }, "per_second 9999 is not requests / elapsed_seconds"},
		{"route elapsed", func(m map[string]any) { obj(route(m, 1, 0), "requests")["elapsed_seconds"] = 2 }, "differs from its window's"},
		{"negative", func(m map[string]any) { obj(route(m, 1, 0), "requests")["requests"] = -1 }, "counts must be finite and non-negative"},
		{"by status", func(m map[string]any) { obj(route(m, 1, 0), "requests", "by_status", 0)["requests"] = 1 }, "by_status adds up to"},
		{"ratio", func(m map[string]any) { obj(route(m, 1, 1), "requests")["server_error_ratio"] = 0.5 }, "server_error_ratio 0.5 is not server_errors_5xx / requests"},
		{"errors exceed", func(m map[string]any) { obj(route(m, 1, 0), "requests")["client_errors_4xx"] = 1e9 }, "exceed the requests"},
		{"both requests", func(m map[string]any) { route(m, 1, 0)["requests_unavailable"] = "gone" }, "exactly one of requests and requests_unavailable"},
		{"neither latency", func(m map[string]any) { delete(route(m, 1, 0), "latency") }, "exactly one of latency and latency_unavailable"},
		{"p95 outside bucket", func(m map[string]any) { obj(route(m, 1, 0), "latency", "p95")["seconds"] = 0.5 }, "is not finite, non-negative, and within its bucket"},
		{"p95 not estimate", func(m map[string]any) { obj(route(m, 1, 0), "latency", "p95")["estimate"] = false }, "marked as an estimate"},
		{"p95 few observations", func(m map[string]any) {
			l := obj(route(m, 1, 0), "latency")
			l["observations"] = 5
			l["buckets"] = []any{map[string]any{"upper_bound": "+Inf", "cumulative_count": 5}}
		}, "fewer than min_observations 20"},
		{"buckets", func(m map[string]any) { obj(route(m, 1, 0), "latency", "buckets", 3)["cumulative_count"] = 0 }, "buckets are not cumulative"},
		{"metric", func(m map[string]any) { obj(route(m, 1, 0), "latency", "evidence")["metric"] = "other" }, "evidence metric is not the configured duration histogram"},
		{"duplicate route", func(m map[string]any) {
			w := obj(m, "analysis", "windows", 1)
			w["routes"] = append(w["routes"].([]any), w["routes"].([]any)[0])
		}, "windows[1]: route GET /inventory/{sku} appears more than once"},
		{"warning window", func(m map[string]any) {
			obj(m, "analysis")["warnings"] = []any{map[string]any{"kind": "scrape_gap", "window": 9, "message": "x"}}
		}, "warnings[0]: window 9 does not exist"},
		{"totals", func(m map[string]any) { obj(m, "analysis", "totals", 0, "requests")["per_second"] = 1 }, "totals[0].requests: per_second 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeAnalysis(edit(t, valid, tc.fn))
			if !errors.Is(err, ErrInvalidAnalysis) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestDecodeListsProblemsBriefly(t *testing.T) {
	data := edit(t, readGolden(t), func(m map[string]any) {
		for w := range 4 {
			for r := range 2 {
				obj(m, "analysis", "windows", w, "routes", r, "requests")["per_second"] = 1
				obj(m, "analysis", "windows", w, "routes", r, "latency", "p95")["estimate"] = false
			}
		}
	})
	_, err := DecodeAnalysis(data)
	if err == nil || !strings.Contains(err.Error(), "; and ") || strings.Count(err.Error(), "windows[") > maxProblems {
		t.Errorf("err = %v", err)
	}
}

func TestReadAnalysisLimit(t *testing.T) {
	data := readGolden(t)
	if _, err := ReadAnalysis(bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("at the limit: %v", err)
	}
	if _, err := ReadAnalysis(bytes.NewReader(data), int64(len(data)-1)); !errors.Is(err, ErrInvalidAnalysis) || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("over the limit: %v", err)
	}
}

func TestDecodeErrorsDoNotQuoteSecrets(t *testing.T) {
	valid := readGolden(t)
	const secret = "ftp://scraper:hunter2@127.0.0.1/metrics?token=topsecret"
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"timestamp", edit(t, valid, func(m map[string]any) { obj(m, "analysis", "windows", 0, "from")["scraped_at"] = secret })},
		{"float", edit(t, valid, func(m map[string]any) {
			obj(m, "analysis", "windows", 0, "routes", 0, "latency", "buckets", 0)["upper_bound"] = secret
		})},
		{"status", edit(t, valid, func(m map[string]any) {
			obj(m, "analysis", "experiment")["status"] = "https://u:hunter2@h/?k=topsecret"
		})},
		{"failed analysis", []byte(`{"schema_version": 1, "error": "could not read https://u:hunter2@h/x?k=topsecret"}`)},
		{"route", edit(t, valid, func(m map[string]any) {
			w := obj(m, "analysis", "windows", 1)
			r := obj(w, "routes", 0)
			r["route"] = "https://u:hunter2@h/?k=topsecret"
			w["routes"] = append(w["routes"].([]any), r)
		})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeAnalysis(tc.data)
			if err == nil {
				t.Fatal("want an error")
			}
			for _, s := range []string{"hunter2", "topsecret"} {
				if strings.Contains(err.Error(), s) {
					t.Errorf("error contains %q: %v", s, err)
				}
			}
		})
	}
}
