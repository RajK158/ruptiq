package k6

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/RajK158/ruptiq/internal/workload"
)

// ErrSummary means k6's summary was missing, malformed, or lacked a required
// measurement.
var ErrSummary = errors.New("invalid k6 summary")

// ErrNoRequests means k6 ran but sent no HTTP requests, so there is nothing to
// measure.
var ErrNoRequests = errors.New("k6 made no HTTP requests")

// summary mirrors the parts Ruptiq reads of k6's machine-readable summary
// (k6 run --new-machine-readable-summary --summary-export), format version 1:
//
//	{"version": "1.0.0",
//	 "config": {"duration": 5.0004, ...},             // measured run time, seconds
//	 "metadata": {"k6_version": "2.3.0", ...},
//	 "results": {"metrics": [
//	   {"name": "http_reqs", "type": "counter", "values": {"count": 1234}},
//	   {"name": "http_req_failed", "type": "rate", "values": {"matches": 0, "rate": 0, "total": 1234}},
//	   {"name": "http_req_duration", "type": "trend", "contains": "time",
//	    "values": {"avg": .., "min": .., "med": .., "p90": .., "p95": .., "max": ..}}, ...]}}
//
// Trend values of "time" metrics are milliseconds. Counters carry no rate in
// this format, so requests per second is computed from count and duration.
type summary struct {
	Version string `json:"version"`
	Config  struct {
		Duration *float64 `json:"duration"`
	} `json:"config"`
	Metadata struct {
		K6Version string `json:"k6_version"`
	} `json:"metadata"`
	Results struct {
		Metrics []summaryMetric `json:"metrics"`
	} `json:"results"`
}

type summaryMetric struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Contains string `json:"contains"`
	// Pointers distinguish a JSON null from a measured zero.
	Values map[string]*float64 `json:"values"`
}

// parsedSummary is what parseSummary extracts.
type parsedSummary struct {
	K6Version string
	Metrics   workload.Metrics
}

// parseSummary reads a k6 machine-readable summary. Any missing, malformed,
// or inconsistent measurement is an error wrapping ErrSummary; nothing is
// defaulted or estimated.
func parseSummary(data []byte) (parsedSummary, error) {
	var s summary
	if err := json.Unmarshal(data, &s); err != nil {
		return parsedSummary{}, fmt.Errorf("%w: not valid JSON: %v", ErrSummary, err)
	}
	if !strings.HasPrefix(s.Version, "1.") {
		return parsedSummary{}, fmt.Errorf("%w: unsupported summary format version %q (expected 1.x; run k6 with --new-machine-readable-summary)", ErrSummary, s.Version)
	}
	if s.Config.Duration == nil {
		return parsedSummary{}, fmt.Errorf("%w: config.duration is missing", ErrSummary)
	}
	duration := *s.Config.Duration
	if !finite(duration) || duration <= 0 {
		return parsedSummary{}, fmt.Errorf("%w: config.duration must be a positive number of seconds, got %v", ErrSummary, duration)
	}

	metrics := make(map[string]summaryMetric, len(s.Results.Metrics))
	for _, m := range s.Results.Metrics {
		if _, dup := metrics[m.Name]; dup {
			return parsedSummary{}, fmt.Errorf("%w: metric %q appears more than once", ErrSummary, m.Name)
		}
		metrics[m.Name] = m
	}

	reqs, ok := metrics["http_reqs"]
	if !ok {
		// k6 omits HTTP metrics entirely when no request was sent.
		return parsedSummary{}, ErrNoRequests
	}
	if err := checkType(reqs, "counter"); err != nil {
		return parsedSummary{}, err
	}
	count, err := countValue(reqs, "count")
	if err != nil {
		return parsedSummary{}, err
	}
	if count == 0 {
		return parsedSummary{}, ErrNoRequests
	}

	failed, ok := metrics["http_req_failed"]
	if !ok {
		return parsedSummary{}, fmt.Errorf("%w: metric http_req_failed is missing", ErrSummary)
	}
	if err := checkType(failed, "rate"); err != nil {
		return parsedSummary{}, err
	}
	failures, err := countValue(failed, "matches")
	if err != nil {
		return parsedSummary{}, err
	}
	total, err := countValue(failed, "total")
	if err != nil {
		return parsedSummary{}, err
	}
	rate, err := value(failed, "rate")
	if err != nil {
		return parsedSummary{}, err
	}
	if failures > total || rate < 0 || rate > 1 {
		return parsedSummary{}, fmt.Errorf("%w: http_req_failed is inconsistent (matches %d, total %d, rate %v)", ErrSummary, failures, total, rate)
	}

	dur, ok := metrics["http_req_duration"]
	if !ok {
		return parsedSummary{}, fmt.Errorf("%w: metric http_req_duration is missing", ErrSummary)
	}
	if err := checkType(dur, "trend"); err != nil {
		return parsedSummary{}, err
	}
	if dur.Contains != "time" {
		return parsedSummary{}, fmt.Errorf("%w: http_req_duration contains %q, expected time", ErrSummary, dur.Contains)
	}
	var lat workload.Latency
	for _, f := range []struct {
		key string
		dst *float64
	}{
		{"avg", &lat.Avg}, {"min", &lat.Min}, {"med", &lat.P50},
		{"p90", &lat.P90}, {"p95", &lat.P95}, {"max", &lat.Max},
	} {
		v, err := value(dur, f.key)
		if err != nil {
			return parsedSummary{}, err
		}
		if v < 0 {
			return parsedSummary{}, fmt.Errorf("%w: http_req_duration %s is negative", ErrSummary, f.key)
		}
		*f.dst = v
	}

	return parsedSummary{
		K6Version: s.Metadata.K6Version,
		Metrics: workload.Metrics{
			DurationSeconds:   duration,
			HTTPRequests:      count,
			FailedRequests:    failures,
			FailureRate:       rate,
			RequestsPerSecond: float64(count) / duration,
			LatencyMs:         lat,
		},
	}, nil
}

func checkType(m summaryMetric, want string) error {
	if m.Type != want {
		return fmt.Errorf("%w: metric %s has type %q, expected %q", ErrSummary, m.Name, m.Type, want)
	}
	return nil
}

func value(m summaryMetric, key string) (float64, error) {
	p := m.Values[key]
	if p == nil {
		return 0, fmt.Errorf("%w: metric %s has no %q value", ErrSummary, m.Name, key)
	}
	v := *p
	if !finite(v) {
		return 0, fmt.Errorf("%w: metric %s value %q is not a finite number", ErrSummary, m.Name, key)
	}
	return v, nil
}

func countValue(m summaryMetric, key string) (int64, error) {
	v, err := value(m, key)
	if err != nil {
		return 0, err
	}
	if v < 0 || v != math.Trunc(v) || v > math.MaxInt64/2 {
		return 0, fmt.Errorf("%w: metric %s value %q must be a non-negative whole number, got %v", ErrSummary, m.Name, key, v)
	}
	return int64(v), nil
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
