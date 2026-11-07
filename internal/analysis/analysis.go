// Package analysis extracts performance signals from a saved experiment report
// (experiment.Report): request throughput, server error rates, and latency
// estimates calculated from the differences between timestamped Prometheus
// snapshots. It works offline on the saved measurements only; it never runs a
// workload or contacts a target.
//
// Prometheus counters and histogram buckets are cumulative, so every signal
// is calculated per window: the difference between two consecutive retained
// samples, divided by the time that actually elapsed between them. A series
// that cannot be compared across a window (it appeared, disappeared, or
// decreased) is reported as a warning and its route's signal is unavailable
// for that window; it is never treated as zero.
//
// The signals describe what the measurements show. They are not a diagnosis:
// the package does not rank causes, test statistical significance, or explain
// why a signal changed.
package analysis

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/RajK158/ruptiq/internal/experiment"
	"github.com/RajK158/ruptiq/internal/telemetry"
	"github.com/RajK158/ruptiq/internal/workload"
)

// SchemaVersion is the current Report format.
const SchemaVersion = 1

// Defaults for a Config, matching the Inventory Service's HTTP metrics.
const (
	DefaultRequestsCounter   = "inventory_http_requests_total"
	DefaultDurationHistogram = "inventory_http_request_duration_seconds"
	DefaultMethodLabel       = "method"
	DefaultRouteLabel        = "route"
	DefaultStatusLabel       = "status"
	// DefaultMinObservations is the fewest observations in a window's
	// histogram for its p95 to be estimated. With fewer, the 95th percentile
	// is little more than the slowest one or two requests.
	DefaultMinObservations = 20
	MaxMinObservations     = 1_000_000
)

// Quantile is the latency quantile estimated from histograms.
const Quantile = 0.95

// Config names the metrics to analyse. Nothing in it is specific to one
// service: any Prometheus HTTP server with a request counter and a request
// duration histogram, labelled by method, route, and status, can be analysed.
type Config struct {
	// RequestsCounter is the counter family of completed HTTP requests.
	RequestsCounter string `json:"requests_counter"`
	// DurationHistogram is the histogram family of request durations, in
	// seconds.
	DurationHistogram string `json:"duration_histogram"`
	// MethodLabel and RouteLabel group series into routes. Either may be
	// empty, in which case series are not split by it. The route label should
	// hold a route template (such as /inventory/{sku}), not a raw path.
	MethodLabel string `json:"method_label"`
	RouteLabel  string `json:"route_label"`
	// StatusLabel holds the HTTP response status code. Empty means statuses
	// are unknown, so server error rates are unavailable.
	StatusLabel string `json:"status_label"`
	// MinObservations is the fewest observations needed to estimate p95.
	MinObservations int `json:"min_observations"`
}

// DefaultConfig returns the configuration for the Inventory Service.
func DefaultConfig() Config {
	return Config{
		RequestsCounter:   DefaultRequestsCounter,
		DurationHistogram: DefaultDurationHistogram,
		MethodLabel:       DefaultMethodLabel,
		RouteLabel:        DefaultRouteLabel,
		StatusLabel:       DefaultStatusLabel,
		MinObservations:   DefaultMinObservations,
	}
}

// Validate reports every problem with c, or nil.
func (c Config) Validate() error {
	var problems []string
	if strings.TrimSpace(c.RequestsCounter) == "" {
		problems = append(problems, "requests counter name is required")
	}
	if strings.TrimSpace(c.DurationHistogram) == "" {
		problems = append(problems, "duration histogram name is required")
	}
	if c.RequestsCounter != "" && c.RequestsCounter == c.DurationHistogram {
		problems = append(problems, "requests counter and duration histogram must be different families")
	}
	labels := map[string]string{}
	for _, l := range []struct{ role, name string }{{"method", c.MethodLabel}, {"route", c.RouteLabel}, {"status", c.StatusLabel}} {
		if l.name == "" {
			continue
		}
		if other, ok := labels[l.name]; ok {
			problems = append(problems, fmt.Sprintf("the %s and %s labels must be different, both are %q", other, l.role, l.name))
		}
		labels[l.name] = l.role
	}
	if c.MinObservations < 1 || c.MinObservations > MaxMinObservations {
		problems = append(problems, fmt.Sprintf("min observations must be between 1 and %d, got %d", MaxMinObservations, c.MinObservations))
	}
	if len(problems) > 0 {
		return errors.New("invalid analysis config: " + strings.Join(problems, "; "))
	}
	return nil
}

// Report is the JSON file of one analysis. Exactly one of Analysis and Error
// is set: a report that could not be analysed is recorded, never omitted, so
// an old analysis cannot be mistaken for the latest one.
type Report struct {
	SchemaVersion int `json:"schema_version"`
	// Input is the experiment report's path as given.
	Input    string    `json:"input,omitempty"`
	Analysis *Analysis `json:"analysis,omitempty"`
	Error    string    `json:"error,omitempty"`
}

// Analysis is every signal extracted from one experiment report.
type Analysis struct {
	Experiment ExperimentInfo `json:"experiment"`
	Config     Config         `json:"config"`
	// Client is what the load generator measured, kept separate from the
	// server-side signals: the two are measured at different places over
	// different time spans.
	Client ClientSignals `json:"client"`
	// Totals combine, per route, every window in which the route's signal was
	// available.
	Totals      []RouteSignals `json:"totals"`
	Windows     []Window       `json:"windows"`
	Warnings    []Warning      `json:"warnings"`
	Limitations []string       `json:"limitations"`
}

// ExperimentInfo identifies the analysed experiment. URLs are redacted (no
// user information or query string).
type ExperimentInfo struct {
	SchemaVersion         int                     `json:"schema_version"`
	Status                experiment.Status       `json:"status"`
	Reasons               []string                `json:"reasons,omitempty"`
	StartedAt             time.Time               `json:"started_at"`
	FinishedAt            time.Time               `json:"finished_at"`
	TargetURL             string                  `json:"target_url"`
	MetricsURL            string                  `json:"metrics_url"`
	VUs                   int                     `json:"vus"`
	DurationSeconds       float64                 `json:"duration_seconds"`
	SampleIntervalSeconds float64                 `json:"sample_interval_seconds"`
	Completeness          experiment.Completeness `json:"telemetry_completeness"`
	CompletenessReasons   []string                `json:"telemetry_completeness_reasons,omitempty"`
	Attempts              int                     `json:"scrape_attempts"`
	Samples               int                     `json:"samples"`
	ScrapeErrors          int                     `json:"scrape_errors"`
	NotRetained           int                     `json:"not_retained"`
}

// ClientSignals are the load generator's own measurements, copied unchanged.
type ClientSignals struct {
	// Source says where the figures come from, for example "k6 (client side)".
	Source        string           `json:"source"`
	RunnerVersion string           `json:"runner_version,omitempty"`
	Metrics       workload.Metrics `json:"metrics"`
	Passed        bool             `json:"passed"`
}

// SampleRef identifies one retained sample of the experiment report.
type SampleRef struct {
	Sequence      int              `json:"sequence"`
	Phase         experiment.Phase `json:"phase"`
	ScrapedAt     time.Time        `json:"scraped_at"`
	OffsetSeconds float64          `json:"offset_seconds"`
}

// MissedScrape is a scrape attempt between a window's two samples that has
// no values: it failed, or its sample was not retained.
type MissedScrape struct {
	Sequence int              `json:"sequence"`
	Phase    experiment.Phase `json:"phase"`
	// Reason is "scrape_failed" or "not_retained".
	Reason string `json:"reason"`
	// Error is the scrape's (redacted) error, for a failed scrape.
	Error string `json:"error,omitempty"`
}

// Window is the interval between two consecutive retained samples.
type Window struct {
	// Index counts windows from 1.
	Index int       `json:"index"`
	From  SampleRef `json:"from"`
	To    SampleRef `json:"to"`
	// ElapsedSeconds is the time between the two scrapes, from their
	// timestamps; every rate in the window is divided by it.
	ElapsedSeconds float64 `json:"elapsed_seconds"`
	// MissedScrapes is non-empty when the window spans scrapes without
	// values, which makes it longer than one sampling interval.
	MissedScrapes []MissedScrape `json:"missed_scrapes"`
	Routes        []RouteSignals `json:"routes"`
}

// RouteSignals are the signals of one method and route. For each of requests
// and latency, exactly one of the signal and its "unavailable" reason is set.
type RouteSignals struct {
	Method string `json:"method"`
	Route  string `json:"route"`

	Requests            *RequestSignals `json:"requests,omitempty"`
	RequestsUnavailable string          `json:"requests_unavailable,omitempty"`
	Latency             *LatencySignals `json:"latency,omitempty"`
	LatencyUnavailable  string          `json:"latency_unavailable,omitempty"`
}

// RequestSignals are calculated from the requests counter.
type RequestSignals struct {
	// Requests is the counter's increase, summed over the route's series.
	Requests float64 `json:"requests"`
	// ElapsedSeconds is the time the increase was observed over: the window's
	// length, or for totals the sum of the windows used.
	ElapsedSeconds float64 `json:"elapsed_seconds"`
	// PerSecond is Requests / ElapsedSeconds.
	PerSecond float64       `json:"per_second"`
	ByStatus  []StatusCount `json:"by_status"`
	// ServerErrors counts responses with a 5xx status. 4xx responses are
	// client errors and are counted separately; they are not server errors.
	ServerErrors float64 `json:"server_errors_5xx"`
	ClientErrors float64 `json:"client_errors_4xx"`
	// Unclassified counts requests without a numeric 1xx-5xx status label.
	Unclassified float64 `json:"unclassified_status"`
	// ServerErrorRatio is ServerErrors / Requests, from 0 to 1.
	ServerErrorRatio            *float64 `json:"server_error_ratio,omitempty"`
	ServerErrorRatioUnavailable string   `json:"server_error_ratio_unavailable,omitempty"`
	Evidence                    Evidence `json:"evidence"`
}

// StatusCount is the requests counted for one status label value.
type StatusCount struct {
	Status   string  `json:"status"`
	Requests float64 `json:"requests"`
}

// LatencySignals are calculated from the duration histogram: the histogram
// of only the requests completed in the window, obtained by subtracting the
// earlier sample's cumulative buckets from the later one's.
type LatencySignals struct {
	Observations uint64 `json:"observations"`
	// MeanSeconds is the increase in the histogram's sum divided by
	// Observations: an exact mean, up to floating-point rounding.
	MeanSeconds            *float64 `json:"mean_seconds,omitempty"`
	MeanSecondsUnavailable string   `json:"mean_seconds_unavailable,omitempty"`
	// P95 is an estimate, never an exact measurement.
	P95 QuantileEstimate `json:"p95"`
	// Buckets is the window's histogram: cumulative counts of the requests
	// completed in the window.
	Buckets  []Bucket `json:"buckets"`
	Evidence Evidence `json:"evidence"`
}

// Bucket is one cumulative histogram bucket. UpperBound is +Inf for the last
// bucket, written as the string "+Inf".
type Bucket struct {
	UpperBound      telemetry.Float `json:"upper_bound"`
	CumulativeCount uint64          `json:"cumulative_count"`
}

// QuantileEstimate is a quantile estimated from histogram buckets by linear
// interpolation within the bucket that contains it, as PromQL's
// histogram_quantile does. The true value lies somewhere in
// [BucketLowerSeconds, BucketUpperSeconds]; the interpolation assumes
// observations are spread evenly within that bucket.
type QuantileEstimate struct {
	Quantile float64 `json:"quantile"`
	// Estimate is always true: histogram quantiles are estimates.
	Estimate           bool     `json:"estimate"`
	Seconds            *float64 `json:"seconds,omitempty"`
	BucketLowerSeconds *float64 `json:"bucket_lower_seconds,omitempty"`
	BucketUpperSeconds *float64 `json:"bucket_upper_seconds,omitempty"`
	Unavailable        string   `json:"unavailable,omitempty"`
}

// Evidence points at the measurements a signal was calculated from.
type Evidence struct {
	Metric string `json:"metric"`
	// Windows lists the windows used, and ExcludedWindows those in which the
	// signal was unavailable; both are for totals only.
	Windows         []int `json:"windows,omitempty"`
	ExcludedWindows []int `json:"excluded_windows,omitempty"`
	// Series are the labels of the series used.
	Series []map[string]string `json:"series"`
}

// Warning kinds.
const (
	// A series is in the later sample of a window but not the earlier one.
	WarnSeriesAppeared = "series_appeared"
	// A series is in the earlier sample of a window but not the later one.
	WarnSeriesDisappeared = "series_disappeared"
	// A counter decreased: the target probably restarted.
	WarnCounterReset = "counter_reset"
	// A histogram's count or a bucket decreased.
	WarnHistogramReset = "histogram_reset"
	// A histogram's bucket bounds differ between samples or between series
	// of one route.
	WarnBucketsChanged = "histogram_buckets_changed"
	// A configured family is absent from a sample.
	WarnFamilyMissing = "family_missing"
	// A configured family has an unexpected type.
	WarnFamilyType = "family_type"
	// A window spans scrapes without values.
	WarnScrapeGap = "scrape_gap"
	// Counter values above 2^53 lose integer precision.
	WarnPrecision = "precision"
	// A calculated value is not a finite number.
	WarnOverflow = "overflow"
)

// Warning is missing or unusable evidence. Window is 0 for a warning about a
// single sample.
type Warning struct {
	Kind     string            `json:"kind"`
	Window   int               `json:"window,omitempty"`
	Sequence int               `json:"sequence,omitempty"`
	Metric   string            `json:"metric,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`
	Message  string            `json:"message"`
}
