// Package telemetry is Ruptiq Core's TelemetryReader boundary: reading a
// target's own measurements, such as request counts or queue depths, as
// structured data.
//
// It knows nothing about any target application. Metric names, labels, and
// values are kept exactly as the target exposed them; nothing is renamed,
// derived, or assumed to exist.
package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"
)

// Reader reads one snapshot of a target's telemetry.
//
// Read returns an error, never a partial Snapshot, if the telemetry could not
// be fetched or is not valid.
type Reader interface {
	Read(ctx context.Context) (Snapshot, error)
}

// ReportSchemaVersion is the current Report format.
const ReportSchemaVersion = 1

// Report is the JSON file for one attempt to read a snapshot. Exactly one of
// Snapshot and Error is set: a failed read is recorded, never omitted, so an
// old snapshot cannot be mistaken for the latest one.
type Report struct {
	SchemaVersion int `json:"schema_version"`
	// Endpoint is redacted like Snapshot.Endpoint.
	Endpoint string    `json:"endpoint"`
	Snapshot *Snapshot `json:"snapshot,omitempty"`
	Error    string    `json:"error,omitempty"`
}

// Snapshot is a target's telemetry at one moment.
type Snapshot struct {
	// Endpoint is where the telemetry was read, redacted by
	// workload.RedactURL (no user information or query string).
	Endpoint string `json:"endpoint"`
	// ScrapedAt is when the request was sent, in UTC.
	ScrapedAt time.Time `json:"scraped_at"`
	// ScrapeDurationSeconds is how long the request and reading the response
	// took.
	ScrapeDurationSeconds float64 `json:"scrape_duration_seconds"`
	// Families are sorted by name.
	Families []Family `json:"families"`
}

// Family returns the metric family with the given name.
func (s Snapshot) Family(name string) (Family, bool) {
	for _, f := range s.Families {
		if f.Name == name {
			return f, true
		}
	}
	return Family{}, false
}

// SeriesCount returns the number of series in all families.
func (s Snapshot) SeriesCount() int {
	n := 0
	for _, f := range s.Families {
		n += len(f.Series)
	}
	return n
}

// MetricType is a metric family's type as the target declared it.
type MetricType string

const (
	Counter   MetricType = "counter"
	Gauge     MetricType = "gauge"
	Histogram MetricType = "histogram"
	Summary   MetricType = "summary"
	// Untyped is a family exposed without a TYPE line.
	Untyped MetricType = "untyped"
)

// Family is all series of one metric.
type Family struct {
	Name string     `json:"name"`
	Help string     `json:"help,omitempty"`
	Type MetricType `json:"type"`
	// Series are sorted by their labels.
	Series []Series `json:"series"`
}

// Series is one labeled sample of a family. Exactly one of Value, Histogram,
// and Summary is set, according to the family's type: Value for counters,
// gauges, and untyped metrics.
type Series struct {
	Labels    map[string]string `json:"labels,omitempty"`
	Value     *Float            `json:"value,omitempty"`
	Histogram *HistogramValue   `json:"histogram,omitempty"`
	Summary   *SummaryValue     `json:"summary,omitempty"`
	// Timestamp is set only if the target attached one to the sample.
	Timestamp *time.Time `json:"timestamp,omitempty"`
}

// HistogramValue is a histogram as exposed: cumulative bucket counts, the sum
// of all observations, and their count.
type HistogramValue struct {
	Count uint64 `json:"count"`
	Sum   Float  `json:"sum"`
	// Buckets are sorted by upper bound and include the +Inf bucket when the
	// target exposed it (Prometheus targets always do).
	Buckets []Bucket `json:"buckets"`
}

// Bucket counts the observations less than or equal to UpperBound.
type Bucket struct {
	UpperBound      Float  `json:"upper_bound"`
	CumulativeCount uint64 `json:"cumulative_count"`
}

// SummaryValue is a summary as exposed: precomputed quantiles, the sum of all
// observations, and their count.
type SummaryValue struct {
	Count     uint64     `json:"count"`
	Sum       Float      `json:"sum"`
	Quantiles []Quantile `json:"quantiles,omitempty"`
}

// Quantile is one precomputed quantile of a summary.
type Quantile struct {
	Quantile float64 `json:"quantile"`
	Value    Float   `json:"value"`
}

// Float is a float64 that also survives JSON when it is not finite, which
// Prometheus allows: NaN, +Inf, and -Inf are written as the strings "NaN",
// "+Inf", and "-Inf". Finite values are plain JSON numbers.
type Float float64

func (f Float) MarshalJSON() ([]byte, error) {
	v := float64(f)
	switch {
	case math.IsNaN(v):
		return []byte(`"NaN"`), nil
	case math.IsInf(v, 1):
		return []byte(`"+Inf"`), nil
	case math.IsInf(v, -1):
		return []byte(`"-Inf"`), nil
	}
	return strconv.AppendFloat(nil, v, 'g', -1, 64), nil
}

func (f *Float) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		switch s {
		case "NaN":
			*f = Float(math.NaN())
		case "+Inf":
			*f = Float(math.Inf(1))
		case "-Inf":
			*f = Float(math.Inf(-1))
		default:
			return fmt.Errorf("invalid float %q: want a number, \"NaN\", \"+Inf\", or \"-Inf\"", s)
		}
		return nil
	}
	var v float64
	if err := json.Unmarshal(data, &v); err != nil {
		return fmt.Errorf("invalid float %s: %w", data, err)
	}
	*f = Float(v)
	return nil
}
