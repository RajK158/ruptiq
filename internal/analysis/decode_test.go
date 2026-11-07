package analysis

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/RajK158/ruptiq/internal/experiment"
)

// edit decodes r's JSON into generic values, applies fn, and re-encodes it, so
// tests can produce reports the experiment package would never write.
func edit(t *testing.T, r experiment.Report, fn func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(encode(t, r), &m); err != nil {
		t.Fatal(err)
	}
	fn(m)
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

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

func telem(m map[string]any) map[string]any { return obj(m, "telemetry") }

func sampleN(m map[string]any, i int) map[string]any { return obj(m, "telemetry", "samples", i) }

func TestDecodeAcceptsStatuses(t *testing.T) {
	passed := inventoryReport()
	incomplete := report(
		sample(0, experiment.BeforeWorkload, counter(reqs, cs(get200, 1))),
		failed(1, experiment.DuringWorkload, "refused"),
		sample(2, experiment.AfterWorkload, counter(reqs, cs(get200, 2))),
	)
	failedRun := inventoryReport()
	failedRun.Status = experiment.StatusFailed
	failedRun.Reasons = []string{"failure rate 5.00% exceeds the allowed 0.00%"}
	failedRun.Workload.Verdict.Passed = false
	failedRun.Workload.Result.Metrics.FailedRequests = 450
	failedRun.Workload.Result.Metrics.FailureRate = 0.05

	for _, r := range []experiment.Report{passed, incomplete, failedRun} {
		got, err := DecodeExperiment(encode(t, r))
		if err != nil {
			t.Errorf("status %s: %v", r.Status, err)
			continue
		}
		if got.Status != r.Status || len(got.Telemetry.Samples) != len(r.Telemetry.Samples) {
			t.Errorf("status %s decoded as %+v", r.Status, got.Status)
		}
		if _, err := Analyze(got, DefaultConfig()); err != nil {
			t.Errorf("status %s: Analyze: %v", r.Status, err)
		}
	}
}

func TestDecodeRejectsUnfinishedExperiments(t *testing.T) {
	for _, status := range []experiment.Status{experiment.StatusError, experiment.StatusCancelled, experiment.StatusTimedOut} {
		data := edit(t, inventoryReport(), func(m map[string]any) { m["status"] = string(status) })
		_, err := DecodeExperiment(data)
		if !errors.Is(err, ErrNotAnalyzable) || !strings.Contains(err.Error(), string(status)) {
			t.Errorf("status %s: err = %v, want ErrNotAnalyzable", status, err)
		}
	}
}

func TestDecodeRejectsUnsupportedSchemas(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
		want error
		text string
	}{
		{"version 2", `{"schema_version": 2, "something": "new"}`, ErrUnsupportedSchema, "schema_version 2 (supported: 1)"},
		{"version 0", `{"schema_version": 0}`, ErrUnsupportedSchema, "schema_version 0"},
		{"missing version", `{"status": "passed"}`, ErrInvalidReport, "schema_version is missing"},
		{"null version", `{"schema_version": null}`, ErrInvalidReport, "schema_version is missing"},
		{"string version", `{"schema_version": "1"}`, ErrInvalidReport, "not an integer"},
		{"fractional version", `{"schema_version": 1.5}`, ErrInvalidReport, "not an integer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeExperiment([]byte(tc.data))
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.text) {
				t.Errorf("err = %v, want %v containing %q", err, tc.want, tc.text)
			}
		})
	}
}

func TestDecodeRejectsMalformedJSON(t *testing.T) {
	valid := encode(t, inventoryReport())
	for _, tc := range []struct {
		name string
		data []byte
		text string
	}{
		{"empty", nil, "not valid report JSON"},
		{"not JSON", []byte("experiment"), "not valid report JSON"},
		{"array", []byte(`[1]`), "not valid report JSON"},
		{"truncated", valid[:len(valid)/2], "not valid report JSON"},
		{"trailing data", append(bytes.Clone(valid), []byte(` {}`)...), "unexpected data after the report"},
		{"unknown field", edit(t, inventoryReport(), func(m map[string]any) { m["extra"] = 1 }), `unknown field "extra"`},
		{"unknown nested field", edit(t, inventoryReport(), func(m map[string]any) { sampleN(m, 0)["values"] = 1 }), `unknown field "values"`},
		{"wrong type", edit(t, inventoryReport(), func(m map[string]any) { telem(m)["attempts"] = "five" }), "not valid report JSON"},
		{"bad float", edit(t, inventoryReport(), func(m map[string]any) {
			obj(sampleN(m, 0), "families", 1, "series", 0)["value"] = "lots"
		}), "not valid report JSON"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeExperiment(tc.data)
			if !errors.Is(err, ErrInvalidReport) || !strings.Contains(err.Error(), tc.text) {
				t.Errorf("err = %v, want ErrInvalidReport containing %q", err, tc.text)
			}
		})
	}
}

func TestDecodeRejectsMissingFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(m map[string]any)
		text string
	}{
		{"status", func(m map[string]any) { delete(m, "status") }, "status is missing"},
		{"started_at", func(m map[string]any) { delete(m, "started_at") }, "started_at is missing"},
		{"config", func(m map[string]any) { delete(m, "config") }, "config is missing"},
		{"interval", func(m map[string]any) { delete(obj(m, "config"), "sample_interval_seconds") }, "config.sample_interval_seconds is missing"},
		{"verdict", func(m map[string]any) { delete(obj(m, "workload"), "verdict") }, "workload.verdict is missing"},
		{"k6 requests", func(m map[string]any) { delete(obj(m, "workload", "result", "metrics"), "http_requests") }, "workload.result.metrics.http_requests is missing"},
		{"k6 p95", func(m map[string]any) { delete(obj(m, "workload", "result", "metrics", "latency_ms"), "p95") }, "latency_ms.p95 is missing"},
		{"telemetry", func(m map[string]any) { delete(m, "telemetry") }, "telemetry is missing"},
		{"attempts", func(m map[string]any) { delete(telem(m), "attempts") }, "telemetry.attempts is missing"},
		{"samples null", func(m map[string]any) { telem(m)["samples"] = nil }, "telemetry.samples is missing"},
		{"not_retained", func(m map[string]any) { delete(telem(m), "not_retained") }, "telemetry.not_retained is missing"},
		{"scraped_at", func(m map[string]any) { delete(sampleN(m, 2), "scraped_at") }, "telemetry.samples[2].scraped_at is missing"},
		{"offset", func(m map[string]any) { delete(sampleN(m, 2), "offset_seconds") }, "telemetry.samples[2].offset_seconds is missing"},
		{"families", func(m map[string]any) { delete(sampleN(m, 1), "families") }, "telemetry.samples[1].families is missing"},
		{"family type", func(m map[string]any) { delete(obj(sampleN(m, 1), "families", 1), "type") }, "families[1].type is missing"},
		{"histogram sum", func(m map[string]any) {
			delete(obj(sampleN(m, 1), "families", 0, "series", 0, "histogram"), "sum")
		}, "telemetry.samples[1].families[0].series[0].histogram.sum is missing"},
		{"bucket count", func(m map[string]any) {
			delete(obj(sampleN(m, 1), "families", 0, "series", 0, "histogram", "buckets", 3), "cumulative_count")
		}, "buckets[3].cumulative_count is missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeExperiment(edit(t, inventoryReport(), tc.edit))
			if !errors.Is(err, ErrInvalidReport) || !strings.Contains(err.Error(), tc.text) {
				t.Errorf("err = %v, want ErrInvalidReport containing %q", err, tc.text)
			}
		})
	}
}

func TestDecodeRejectsInconsistentReports(t *testing.T) {
	gapped := func() experiment.Report {
		return report(
			sample(0, experiment.BeforeWorkload, counter(reqs, cs(get200, 1))),
			failed(1, experiment.DuringWorkload, "refused"),
			sample(2, experiment.DuringWorkload, counter(reqs, cs(get200, 2))),
			sample(3, experiment.AfterWorkload, counter(reqs, cs(get200, 3))),
		)
	}
	for _, tc := range []struct {
		name string
		r    experiment.Report
		edit func(m map[string]any)
		text string
	}{
		{"unknown status", inventoryReport(), func(m map[string]any) { m["status"] = "great" }, `status "great" is not a known`},
		{"finished before started", inventoryReport(), func(m map[string]any) { m["finished_at"] = "2026-11-06T11:00:00Z" }, "finished_at is before started_at"},
		{"bad metrics URL", inventoryReport(), func(m map[string]any) { obj(m, "config")["metrics_url"] = "ftp://host/metrics" }, "config.metrics_url is not an absolute http"},
		{"zero interval", inventoryReport(), func(m map[string]any) { obj(m, "config")["sample_interval_seconds"] = 0 }, "sample_interval_seconds must be a positive number"},
		{"passed with partial telemetry", gapped(), func(m map[string]any) { m["status"] = "passed" }, "status passed requires"},
		{"failed with passed verdict", inventoryReport(), func(m map[string]any) { m["status"] = "failed" }, "status failed contradicts"},
		{"no workload result", inventoryReport(), func(m map[string]any) { delete(obj(m, "workload"), "result") }, "workload.result is missing"},
		{"workload error", inventoryReport(), func(m map[string]any) { obj(m, "workload")["error"] = "k6 crashed" }, "workload.error is set"},
		{"failed above total", inventoryReport(), func(m map[string]any) {
			obj(m, "workload", "result", "metrics")["failed_requests"] = 9001
		}, "failed_requests (9001) must be between 0 and http_requests (9000)"},
		{"attempts", inventoryReport(), func(m map[string]any) { telem(m)["attempts"] = 6 }, "telemetry.attempts is 6, but succeeded + failed is 5"},
		{"succeeded", gapped(), func(m map[string]any) { telem(m)["succeeded"] = 4 }, "telemetry.succeeded is 4"},
		{"failed count", gapped(), func(m map[string]any) { telem(m)["failed"] = 0 }, "telemetry.failed is 0, but there are 1 errors"},
		{"retention flag", inventoryReport(), func(m map[string]any) { telem(m)["retention_limit_reached"] = true }, "retention_limit_reached is true"},
		{"completeness", gapped(), func(m map[string]any) { telem(m)["completeness"] = "complete"; m["status"] = "passed" }, `completeness is "complete", but the samples and errors make it "partial"`},
		{"unknown completeness", inventoryReport(), func(m map[string]any) { telem(m)["completeness"] = "mostly" }, `completeness "mostly" is not a known value`},
		{"duplicate sequence", inventoryReport(), func(m map[string]any) { sampleN(m, 2)["sequence"] = 2 }, "sequence 2 is not after the previous entry's"},
		{"sequence out of range", gapped(), func(m map[string]any) { obj(telem(m), "errors", 0)["sequence"] = 9 }, "sequence 9 is outside 1..4"},
		{"sequence used twice across lists", gapped(), func(m map[string]any) { obj(telem(m), "errors", 0)["sequence"] = 1 }, "sequence 1 appears more than once"},
		{"samples out of order", inventoryReport(), func(m map[string]any) {
			s := telem(m)["samples"].([]any)
			s[1], s[2] = s[2], s[1]
		}, "sequence 2 is not after the previous entry's"},
		{"timestamps decrease", inventoryReport(), func(m map[string]any) {
			sampleN(m, 2)["scraped_at"] = "2026-11-06T12:00:00.5Z"
			sampleN(m, 2)["offset_seconds"] = 0.5
		}, "sequence 3 is earlier than sequence 2"},
		{"equal sample timestamps", inventoryReport(), func(m map[string]any) {
			sampleN(m, 2)["scraped_at"] = "2026-11-06T12:00:01.002Z"
			sampleN(m, 2)["offset_seconds"] = 1.002
		}, "samples 2 and 3 have the same or decreasing timestamps"},
		{"offset disagrees", inventoryReport(), func(m map[string]any) { sampleN(m, 1)["offset_seconds"] = 1.5 }, "offset_seconds 1.5 does not match its timestamp"},
		{"timestamp outside", inventoryReport(), func(m map[string]any) {
			sampleN(m, 4)["scraped_at"] = "2026-11-06T12:01:00Z"
			sampleN(m, 4)["offset_seconds"] = 60
		}, "timestamp is outside the experiment"},
		{"unknown phase", inventoryReport(), func(m map[string]any) { sampleN(m, 1)["phase"] = "warmup" }, `phase "warmup" is not a known phase`},
		{"baseline not first", inventoryReport(), func(m map[string]any) { sampleN(m, 1)["phase"] = "before_workload" }, "sequence 2 is before the workload but is not the first scrape"},
		{"final not last", inventoryReport(), func(m map[string]any) { sampleN(m, 3)["phase"] = "after_workload" }, "sequence 4 is after the workload but is not the last scrape"},
		{"negative counter", inventoryReport(), func(m map[string]any) {
			obj(sampleN(m, 0), "families", 1, "series", 0)["value"] = -1
		}, "counter value must be finite and non-negative, got -1"},
		{"NaN counter", inventoryReport(), func(m map[string]any) {
			obj(sampleN(m, 0), "families", 1, "series", 0)["value"] = "NaN"
		}, "counter value must be finite and non-negative, got NaN"},
		{"infinite counter", inventoryReport(), func(m map[string]any) {
			obj(sampleN(m, 0), "families", 1, "series", 0)["value"] = "+Inf"
		}, "counter value must be finite and non-negative, got +Inf"},
		{"counter without value", inventoryReport(), func(m map[string]any) {
			delete(obj(sampleN(m, 0), "families", 1, "series", 0), "value")
		}, "a counter series must have exactly one value"},
		{"histogram with a value", inventoryReport(), func(m map[string]any) {
			obj(sampleN(m, 0), "families", 0, "series", 0)["value"] = 3
		}, "a histogram series must have exactly one histogram"},
		{"unknown type", inventoryReport(), func(m map[string]any) { obj(sampleN(m, 0), "families", 2)["type"] = "meter" }, `type "meter" is not a known metric type`},
		{"duplicate labels", inventoryReport(), func(m map[string]any) {
			series := obj(sampleN(m, 0), "families", 1)["series"].([]any)
			obj(series[1])["labels"] = obj(series[0])["labels"]
		}, "the same labels appear more than once"},
		{"count below a bucket", inventoryReport(), func(m map[string]any) {
			obj(sampleN(m, 0), "families", 0, "series", 0, "histogram")["count"] = 999
		}, "histogram bucket le=0.005 has 1000 observations, more than the count 999"},
		{"+Inf bucket disagrees", inventoryReport(), func(m map[string]any) {
			obj(sampleN(m, 0), "families", 0, "series", 0, "histogram")["count"] = 1001
		}, "histogram +Inf bucket has 1000 observations but count is 1001"},
		{"bucket counts decrease", inventoryReport(), func(m map[string]any) {
			obj(sampleN(m, 0), "families", 0, "series", 0, "histogram", "buckets", 2)["cumulative_count"] = 1
		}, "histogram bucket counts decrease"},
		{"bucket bounds unordered", inventoryReport(), func(m map[string]any) {
			obj(sampleN(m, 0), "families", 0, "series", 0, "histogram", "buckets", 2)["upper_bound"] = 0.00001
		}, "upper bounds are not strictly increasing"},
		{"NaN bucket bound", inventoryReport(), func(m map[string]any) {
			obj(sampleN(m, 0), "families", 0, "series", 0, "histogram", "buckets", 2)["upper_bound"] = "NaN"
		}, "upper bound is NaN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeExperiment(edit(t, tc.r, tc.edit))
			if !errors.Is(err, ErrInvalidReport) || !strings.Contains(err.Error(), tc.text) {
				t.Errorf("err = %v, want ErrInvalidReport containing %q", err, tc.text)
			}
		})
	}
}

func TestDecodeRejectsDuplicateFamilies(t *testing.T) {
	data := edit(t, inventoryReport(), func(m map[string]any) {
		s := sampleN(m, 0)
		fams := s["families"].([]any)
		s["families"] = append(fams, fams[1])
	})
	_, err := DecodeExperiment(data)
	if !errors.Is(err, ErrInvalidReport) || !strings.Contains(err.Error(), `family "inventory_http_requests_total" appears more than once`) {
		t.Errorf("err = %v", err)
	}
}

func TestDecodeListsProblemsBriefly(t *testing.T) {
	data := edit(t, inventoryReport(), func(m map[string]any) {
		for i := range 5 {
			for j := range 4 {
				obj(sampleN(m, i), "families", 1, "series", j)["value"] = -1
			}
		}
	})
	_, err := DecodeExperiment(data)
	if err == nil || !strings.Contains(err.Error(), "and 10 more problems") || strings.Count(err.Error(), "counter value") != maxListedProblems {
		t.Errorf("err = %v", err)
	}
}

func TestReadExperimentLimit(t *testing.T) {
	data := encode(t, inventoryReport())
	if _, err := ReadExperiment(bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("at the limit: %v", err)
	}
	_, err := ReadExperiment(bytes.NewReader(data), int64(len(data))-1)
	if !errors.Is(err, ErrInvalidReport) || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("over the limit: err = %v", err)
	}
}

// Errors never quote the URLs of the report they reject.
func TestDecodeErrorsDoNotLeakURLs(t *testing.T) {
	r := inventoryReport()
	r.Config.TargetURL = "https://loaduser:hunter2@127.0.0.1/inventory?api_key=topsecret"
	data := edit(t, r, func(m map[string]any) {
		obj(m, "config")["metrics_url"] = "ftp://scraper:hunter2@127.0.0.1/metrics?token=topsecret"
		telem(m)["attempts"] = 99
	})
	_, err := DecodeExperiment(data)
	if err == nil {
		t.Fatal("want an error")
	}
	for _, s := range []string{"hunter2", "topsecret"} {
		if strings.Contains(err.Error(), s) {
			t.Errorf("error contains %q: %v", s, err)
		}
	}
}

func TestDecodeErrorsDoNotQuoteBadValues(t *testing.T) {
	const secret = "ftp://scraper:hunter2@127.0.0.1/metrics?token=topsecret"
	for _, tc := range []struct {
		name string
		fn   func(m map[string]any)
	}{
		{"timestamp", func(m map[string]any) { sampleN(m, 1)["scraped_at"] = secret }},
		{"float", func(m map[string]any) { obj(sampleN(m, 0), "families", 1, "series", 0)["value"] = secret }},
		{"status", func(m map[string]any) { m["status"] = "https://u:hunter2@h/?k=topsecret" }},
		{"phase", func(m map[string]any) { sampleN(m, 1)["phase"] = "https://hunter2@h" }},
		{"family type", func(m map[string]any) { obj(sampleN(m, 0), "families", 1)["type"] = "https://u:hunter2@h" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeExperiment(edit(t, inventoryReport(), tc.fn))
			if !errors.Is(err, ErrInvalidReport) {
				t.Fatalf("err = %v, want ErrInvalidReport", err)
			}
			for _, s := range []string{"hunter2", "topsecret"} {
				if strings.Contains(err.Error(), s) {
					t.Errorf("error contains %q: %v", s, err)
				}
			}
		})
	}
}
