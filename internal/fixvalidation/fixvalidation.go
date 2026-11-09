// Package fixvalidation compares two saved analyses (analysis.Report, written
// by ruptiq-analyze): one from a baseline experiment and one from a candidate
// experiment after a proposed fix. It reports per-route server-side
// differences with the measurements they come from.
//
// A result is an observation about two saved runs. It is not a statistical
// significance test, and it is not evidence that a code change caused the
// difference or that the difference would appear again. The package works
// offline: it never runs a workload, contacts a target, or changes code.
package fixvalidation

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/RajK158/ruptiq/internal/experiment"
)

// SchemaVersion is the current Report format.
const SchemaVersion = 1

// Defaults are rules of thumb, aligned with the investigator's conservative
// bars. They are written into every report so a result can be checked by hand.
const (
	DefaultMinWindows         = 2
	DefaultMinRequests        = 100
	DefaultMinServerErrors    = 5
	DefaultErrorRatioIncrease = 0.01
	DefaultErrorRatioFactor   = 2
	DefaultLatencyChange      = 0.5
	DefaultThroughputChange   = 0.3

	MaxMinWindows  = 1000
	MaxMinRequests = 1e12
	maxChange      = 100
)

// Sides of a comparison.
const (
	SideBefore = "before"
	SideAfter  = "after"
)

// Outcomes of a comparison. A route uses the same words, except
// not_comparable, which applies only to the pair of experiments.
const (
	OutcomeImprovement      = "improvement_observed"
	OutcomeRegression       = "regression_observed"
	OutcomeMixed            = "mixed"
	OutcomeNoMaterialChange = "no_material_change"
	OutcomeInsufficient     = "insufficient_evidence"
	OutcomeNotComparable    = "not_comparable"
)

// Signal names.
const (
	SignalThroughput   = "throughput"
	SignalServerErrors = "server_error_ratio"
	SignalLatency      = "p95_latency"
)

// Signal statuses.
const (
	StatusImprovement      = "improvement"
	StatusRegression       = "regression"
	StatusNoMaterialChange = "no_material_change"
	StatusInconclusive     = "inconclusive"
	StatusIncompatible     = "incompatible"
	StatusInsufficient     = "insufficient_evidence"
)

// Units.
const (
	UnitRequestsPerSecond = "requests/s"
	UnitRatio             = "ratio"
	UnitSeconds           = "seconds"
)

// Where a route was found.
const (
	PresentBoth   = "both"
	PresentBefore = "before"
	PresentAfter  = "after"
)

// Evidence roles.
const (
	RoleMeasurement = "measurement"
	RoleExcluded    = "excluded"
)

// Config holds the material-change thresholds. Every value is explicit.
type Config struct {
	// MinWindows is the fewest eligible windows required in each experiment
	// before a signal is compared.
	MinWindows int `json:"min_windows"`
	// MinRequests is the fewest completed requests the before side must
	// contain for throughput, and that each side must contain for a 5xx
	// ratio. Latency uses it as a minimum observation count on each side.
	MinRequests float64 `json:"min_requests"`
	// MinServerErrors is the fewest 5xx responses required on the side that
	// got worse before an error-ratio change is called material.
	MinServerErrors float64 `json:"min_server_errors"`
	// ErrorRatioIncrease is the smallest absolute change of the 5xx ratio,
	// as a fraction (0.01 is one percentage point).
	ErrorRatioIncrease float64 `json:"error_ratio_increase"`
	// ErrorRatioFactor is how many times the better side's 5xx ratio the
	// worse side's must be, when the better side's ratio is not zero.
	ErrorRatioFactor float64 `json:"error_ratio_factor"`
	// LatencyChange is the smallest relative change of the median per-window
	// p95 estimate (0.5 is 50%).
	LatencyChange float64 `json:"latency_change"`
	// ThroughputChange is the smallest relative change of server throughput
	// (0.3 is 30%).
	ThroughputChange float64 `json:"throughput_change"`
}

// DefaultConfig returns the conservative default thresholds.
func DefaultConfig() Config {
	return Config{
		MinWindows:         DefaultMinWindows,
		MinRequests:        DefaultMinRequests,
		MinServerErrors:    DefaultMinServerErrors,
		ErrorRatioIncrease: DefaultErrorRatioIncrease,
		ErrorRatioFactor:   DefaultErrorRatioFactor,
		LatencyChange:      DefaultLatencyChange,
		ThroughputChange:   DefaultThroughputChange,
	}
}

// Validate reports every problem with c, or nil.
func (c Config) Validate() error {
	var problems []string
	check := func(ok bool, format string, args ...any) {
		if !ok {
			problems = append(problems, fmt.Sprintf(format, args...))
		}
	}
	finite := func(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }
	check(c.MinWindows >= 1 && c.MinWindows <= MaxMinWindows, "min windows must be between 1 and %d, got %d", MaxMinWindows, c.MinWindows)
	check(finite(c.MinRequests) && c.MinRequests >= 1 && c.MinRequests <= MaxMinRequests, "min requests must be between 1 and %g, got %g", float64(MaxMinRequests), c.MinRequests)
	check(finite(c.MinServerErrors) && c.MinServerErrors >= 1 && c.MinServerErrors <= MaxMinRequests, "min server errors must be between 1 and %g, got %g", float64(MaxMinRequests), c.MinServerErrors)
	check(finite(c.ErrorRatioIncrease) && c.ErrorRatioIncrease > 0 && c.ErrorRatioIncrease <= 1, "error ratio increase must be above 0 and at most 1, got %g", c.ErrorRatioIncrease)
	check(finite(c.ErrorRatioFactor) && c.ErrorRatioFactor >= 1 && c.ErrorRatioFactor <= 1000, "error ratio factor must be between 1 and 1000, got %g", c.ErrorRatioFactor)
	check(finite(c.LatencyChange) && c.LatencyChange > 0 && c.LatencyChange <= maxChange, "latency change must be above 0 and at most %g, got %g", float64(maxChange), c.LatencyChange)
	check(finite(c.ThroughputChange) && c.ThroughputChange > 0 && c.ThroughputChange <= maxChange, "throughput change must be above 0 and at most %g, got %g", float64(maxChange), c.ThroughputChange)
	if len(problems) > 0 {
		return errors.New("invalid fix-validation config: " + strings.Join(problems, "; "))
	}
	return nil
}

// Report is the JSON file of one comparison. Exactly one of Validation and
// Error is set, so a failed read cannot be mistaken for a saved result.
type Report struct {
	SchemaVersion int `json:"schema_version"`
	// Before and After are the analysis paths as given.
	Before     string      `json:"before,omitempty"`
	After      string      `json:"after,omitempty"`
	Validation *Validation `json:"validation,omitempty"`
	Error      string      `json:"error,omitempty"`
}

// Validation is the comparison of one before analysis and one after analysis.
type Validation struct {
	Config         Config            `json:"config"`
	Outcome        string            `json:"outcome"`
	Summary        string            `json:"summary"`
	Comparability  Comparability     `json:"comparability"`
	Before         Source            `json:"before"`
	After          Source            `json:"after"`
	Routes         []RouteComparison `json:"routes"`
	Ranked         []RankedChange    `json:"ranked"`
	Limitations    []string          `json:"limitations"`
	NextExperiment string            `json:"next_experiment"`
}

// Comparability says whether the two experiments were run as the same test.
// Comparable does not mean the machines or background load matched.
type Comparability struct {
	Comparable bool     `json:"comparable"`
	Reasons    []string `json:"reasons"`
	Warnings   []string `json:"warnings"`
}

// Source describes one analysed experiment. URLs are redacted.
type Source struct {
	Label                 string                  `json:"label"`
	Status                experiment.Status       `json:"experiment_status"`
	TelemetryCompleteness experiment.Completeness `json:"telemetry_completeness"`
	TargetURL             string                  `json:"target_url"`
	MetricsURL            string                  `json:"metrics_url"`
	VUs                   int                     `json:"vus"`
	DurationSeconds       float64                 `json:"duration_seconds"`
	SampleIntervalSeconds float64                 `json:"sample_interval_seconds"`
	RequestsCounter       string                  `json:"requests_counter"`
	DurationHistogram     string                  `json:"duration_histogram"`
	MethodLabel           string                  `json:"method_label"`
	RouteLabel            string                  `json:"route_label"`
	StatusLabel           string                  `json:"status_label"`
	MinObservations       int                     `json:"min_observations"`
	Windows               int                     `json:"windows"`
	EligibleWindows       []int                   `json:"eligible_windows"`
	ExcludedWindows       []ExcludedWindow        `json:"excluded_windows"`
	Client                ClientSnapshot          `json:"client"`
}

// ClientSnapshot is what k6 measured. It is not a server-side signal and is
// never an input to the comparison.
type ClientSnapshot struct {
	Source            string  `json:"source"`
	HTTPRequests      int64   `json:"http_requests"`
	RequestsPerSecond float64 `json:"requests_per_second"`
	FailureRate       float64 `json:"failure_rate"`
	P95Millis         float64 `json:"p95_ms"`
	Note              string  `json:"note"`
}

// ExcludedWindow is a window left out of a calculation, and why.
type ExcludedWindow struct {
	Window  int    `json:"window"`
	Reason  string `json:"reason"`
	Pointer string `json:"pointer"`
}

// RouteComparison is one method and route across the two experiments.
type RouteComparison struct {
	Method         string            `json:"method"`
	Route          string            `json:"route"`
	Outcome        string            `json:"outcome"`
	PresentIn      string            `json:"present_in"`
	Summary        string            `json:"summary"`
	Throughput     *SignalComparison `json:"throughput,omitempty"`
	ServerErrors   *SignalComparison `json:"server_errors,omitempty"`
	Latency        *SignalComparison `json:"latency,omitempty"`
	Evidence       []EvidenceRef     `json:"evidence"`
	Limitations    []string          `json:"limitations"`
	NextExperiment string            `json:"next_experiment"`
}

// SignalComparison is one server-side signal of one route.
type SignalComparison struct {
	Signal           string           `json:"signal"`
	Status           string           `json:"status"`
	Statistic        string           `json:"statistic,omitempty"`
	Before           *Measure         `json:"before,omitempty"`
	After            *Measure         `json:"after,omitempty"`
	Absolute         *float64         `json:"absolute,omitempty"`
	Relative         *float64         `json:"relative,omitempty"`
	PercentagePoints *float64         `json:"percentage_points,omitempty"`
	BeforeWindows    []int            `json:"before_windows"`
	AfterWindows     []int            `json:"after_windows"`
	ExcludedBefore   []ExcludedWindow `json:"excluded_before"`
	ExcludedAfter    []ExcludedWindow `json:"excluded_after"`
	Evidence         []EvidenceRef    `json:"evidence"`
	Reason           string           `json:"reason"`
}

// Measure is one side of a signal. For latency, Value is the median of
// per-window p95 estimates, not an experiment-wide p95.
type Measure struct {
	Value              float64  `json:"value"`
	Unit               string   `json:"unit"`
	Statistic          string   `json:"statistic"`
	Requests           float64  `json:"requests"`
	ServerErrors       *float64 `json:"server_errors_5xx,omitempty"`
	ClientErrors       *float64 `json:"client_errors_4xx,omitempty"`
	ElapsedSeconds     float64  `json:"elapsed_seconds"`
	Observations       *float64 `json:"observations,omitempty"`
	Windows            int      `json:"windows"`
	Estimate           bool     `json:"estimate,omitempty"`
	BucketLowerSeconds *float64 `json:"bucket_lower_seconds,omitempty"`
	BucketUpperSeconds *float64 `json:"bucket_upper_seconds,omitempty"`
}

// EvidenceRef points at one object in a source analysis file.
type EvidenceRef struct {
	Side    string `json:"side"`
	Window  int    `json:"window"`
	Pointer string `json:"pointer"`
	Role    string `json:"role"`
}

// RankedChange is one material improvement or regression, most serious first.
type RankedChange struct {
	Rank      int      `json:"rank"`
	Method    string   `json:"method"`
	Route     string   `json:"route"`
	Signal    string   `json:"signal"`
	Direction string   `json:"direction"`
	Absolute  *float64 `json:"absolute,omitempty"`
	Relative  *float64 `json:"relative,omitempty"`
	// PercentagePoints is set for the 5xx ratio.
	PercentagePoints *float64 `json:"percentage_points,omitempty"`
	// Magnitude is the absolute relative change, or the absolute ratio change
	// for 5xx, used only to order rows.
	Magnitude float64 `json:"magnitude"`
}
