// Package investigation looks for performance changes in a saved analysis
// (analysis.Report, written by ruptiq-analyze) and reports them as ranked,
// evidence-backed findings. It is deterministic and works offline on the
// analysis only: it never runs a workload, contacts a target, or uses a
// language model.
//
// A finding is an observation (a measured change between earlier and later
// windows of one experiment) or a hypothesis to test (two observations that
// may share a cause). It is never a proven root cause: the analysis holds
// request counts, status codes, and latency histograms only, so the package
// cannot measure CPU, memory, databases, or networks, and one run cannot show
// a regression. Changes are compared with fixed thresholds; no statistical
// significance test is made. When the evidence is inadequate, the package
// abstains and says why.
package investigation

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/RajK158/ruptiq/internal/experiment"
)

// SchemaVersion is the current Report format.
const SchemaVersion = 1

// Defaults for a Config. They are deliberately conservative: a change must be
// large, seen in several windows, and backed by enough requests.
const (
	DefaultMinWindows         = 2
	DefaultMinRequests        = 100
	DefaultMinServerErrors    = 5
	DefaultErrorRatioIncrease = 0.01
	DefaultErrorRatioFactor   = 2
	DefaultLatencyIncrease    = 0.5
	DefaultThroughputDecrease = 0.3

	MaxMinWindows  = 1000
	MaxMinRequests = 1e12

	// MajorMultiple is how many times its threshold a change must reach to
	// be ranked "major" rather than "moderate".
	MajorMultiple = 2
)

// Config holds the thresholds. Every one is explicit and written into the
// report, so a finding can be checked by hand.
type Config struct {
	// MinWindows is the fewest usable windows in each of the baseline and the
	// comparison: a trend needs at least twice as many usable windows.
	MinWindows int `json:"min_windows_per_side"`
	// MinRequests is the fewest requests (or, for latency, histogram
	// observations) a side must contain. Throughput needs it in the baseline
	// only, so a collapse to almost no traffic is still detected.
	MinRequests float64 `json:"min_requests_per_side"`
	// MinServerErrors is the fewest 5xx responses in the comparison for a
	// server error increase.
	MinServerErrors float64 `json:"min_server_errors"`
	// ErrorRatioIncrease is the smallest rise of the 5xx ratio, as a
	// fraction (0.01 is one percentage point).
	ErrorRatioIncrease float64 `json:"error_ratio_increase"`
	// ErrorRatioFactor is how many times the baseline's 5xx ratio the
	// comparison's must be, when the baseline's is not zero.
	ErrorRatioFactor float64 `json:"error_ratio_factor"`
	// LatencyIncrease is the smallest relative rise of the p95 estimate
	// (0.5 is +50%).
	LatencyIncrease float64 `json:"latency_increase"`
	// ThroughputDecrease is the smallest relative fall of the request rate
	// (0.3 is -30%).
	ThroughputDecrease float64 `json:"throughput_decrease"`
}

// DefaultConfig returns the conservative default thresholds.
func DefaultConfig() Config {
	return Config{
		MinWindows:         DefaultMinWindows,
		MinRequests:        DefaultMinRequests,
		MinServerErrors:    DefaultMinServerErrors,
		ErrorRatioIncrease: DefaultErrorRatioIncrease,
		ErrorRatioFactor:   DefaultErrorRatioFactor,
		LatencyIncrease:    DefaultLatencyIncrease,
		ThroughputDecrease: DefaultThroughputDecrease,
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
	check(c.MinWindows >= 1 && c.MinWindows <= MaxMinWindows, "min windows per side must be between 1 and %d, got %d", MaxMinWindows, c.MinWindows)
	check(finite(c.MinRequests) && c.MinRequests >= 1 && c.MinRequests <= MaxMinRequests, "min requests per side must be between 1 and %g, got %g", float64(MaxMinRequests), c.MinRequests)
	check(finite(c.MinServerErrors) && c.MinServerErrors >= 1 && c.MinServerErrors <= MaxMinRequests, "min server errors must be between 1 and %g, got %g", float64(MaxMinRequests), c.MinServerErrors)
	check(finite(c.ErrorRatioIncrease) && c.ErrorRatioIncrease > 0 && c.ErrorRatioIncrease <= 1, "error ratio increase must be above 0 and at most 1, got %g", c.ErrorRatioIncrease)
	check(finite(c.ErrorRatioFactor) && c.ErrorRatioFactor >= 1 && c.ErrorRatioFactor <= 1000, "error ratio factor must be between 1 and 1000, got %g", c.ErrorRatioFactor)
	check(finite(c.LatencyIncrease) && c.LatencyIncrease > 0 && c.LatencyIncrease <= 100, "latency increase must be above 0 and at most 100, got %g", c.LatencyIncrease)
	check(finite(c.ThroughputDecrease) && c.ThroughputDecrease > 0 && c.ThroughputDecrease < 1, "throughput decrease must be above 0 and below 1, got %g", c.ThroughputDecrease)
	if len(problems) > 0 {
		return errors.New("invalid investigation config: " + strings.Join(problems, "; "))
	}
	return nil
}

// Report is the JSON file of one investigation. Exactly one of Investigation
// and Error is set: a failure is recorded, so an older investigation cannot be
// mistaken for the latest one.
type Report struct {
	SchemaVersion int `json:"schema_version"`
	// Input is the analysis file's path as given.
	Input         string         `json:"input,omitempty"`
	Investigation *Investigation `json:"investigation,omitempty"`
	Error         string         `json:"error,omitempty"`
}

// Outcomes of an investigation.
const (
	// At least one finding is an observed change.
	OutcomeChangesObserved = "changes_observed"
	// Some signals could be compared and none changed materially.
	OutcomeNoMaterialChange = "no_material_change"
	// No signal of any route could be compared: the investigation abstains.
	OutcomeInsufficientEvidence = "insufficient_evidence"
)

// Investigation is the result of investigating one analysis.
type Investigation struct {
	Source  Source `json:"source"`
	Config  Config `json:"config"`
	Outcome string `json:"outcome"`
	// Summary is one sentence describing the outcome.
	Summary string          `json:"summary"`
	Windows WindowSelection `json:"windows"`
	// Findings are ranked: Findings[0] has rank 1.
	Findings []Finding `json:"findings"`
	// Assessments are every route and signal compared, including those with
	// no material change, so a missing finding can be told from a missing
	// measurement.
	Assessments []Assessment `json:"assessments"`
	Limitations []string     `json:"limitations"`
}

// Source describes the analysed experiment. URLs are redacted.
type Source struct {
	AnalysisSchemaVersion int                     `json:"analysis_schema_version"`
	ExperimentStatus      experiment.Status       `json:"experiment_status"`
	TelemetryCompleteness experiment.Completeness `json:"telemetry_completeness"`
	StartedAt             time.Time               `json:"started_at"`
	TargetURL             string                  `json:"target_url"`
	MetricsURL            string                  `json:"metrics_url"`
	VUs                   int                     `json:"vus"`
	DurationSeconds       float64                 `json:"duration_seconds"`
	SampleIntervalSeconds float64                 `json:"sample_interval_seconds"`
	RequestsCounter       string                  `json:"requests_counter"`
	DurationHistogram     string                  `json:"duration_histogram"`
	// Windows is the number of windows in the analysis.
	Windows int `json:"windows"`
}

// WindowSelection says which analysis windows can be compared at all.
type WindowSelection struct {
	// Comparable windows were taken during the workload with no missed
	// scrapes. Each route and signal uses the subset in which it is
	// available.
	Comparable []int            `json:"comparable"`
	Excluded   []ExcludedWindow `json:"excluded"`
}

// ExcludedWindow is a window that is not compared, and why.
type ExcludedWindow struct {
	Window int    `json:"window"`
	Reason string `json:"reason"`
	// Pointer is the window's JSON pointer in the analysis file.
	Pointer string `json:"pointer"`
}

// Signals compared for each route.
const (
	SignalThroughput       = "throughput"
	SignalServerErrorRatio = "server_error_ratio"
	SignalP95Latency       = "p95_latency"
)

// Assessment statuses.
const (
	// The change exceeds its thresholds and passes the robustness checks.
	StatusMaterialChange = "material_change"
	// The signal was compared and did not change materially.
	StatusNoMaterialChange = "no_material_change"
	// The change reaches its threshold but is not robust: for example the
	// p95 estimates share histogram buckets, or the windows overlap.
	StatusInconclusive = "inconclusive"
	// The signal could not be compared.
	StatusInsufficientEvidence = "insufficient_evidence"
)

// Assessment is one signal of one route compared between its baseline (the
// earlier half of its usable windows) and its comparison (the later half).
type Assessment struct {
	Method            string   `json:"method"`
	Route             string   `json:"route"`
	Signal            string   `json:"signal"`
	Status            string   `json:"status"`
	BaselineWindows   []int    `json:"baseline_windows"`
	ComparisonWindows []int    `json:"comparison_windows"`
	Baseline          *Measure `json:"baseline,omitempty"`
	Comparison        *Measure `json:"comparison,omitempty"`
	Change            *Change  `json:"change,omitempty"`
	// Reason explains the status in a sentence.
	Reason string `json:"reason"`
	// Gaps are the windows in which the signal was unavailable for the route.
	Gaps []Gap `json:"gaps"`
}

// Units of a Measure.
const (
	UnitRequestsPerSecond = "requests/s"
	UnitRatio             = "ratio"
	UnitSeconds           = "seconds"
)

// Measure is one side of a comparison.
type Measure struct {
	Value float64 `json:"value"`
	Unit  string  `json:"unit"`
	// Statistic says how Value combines the side's windows.
	Statistic string `json:"statistic"`
	// Estimate is true for histogram quantiles.
	Estimate bool `json:"estimate,omitempty"`
	// For p95: the lowest bucket lower bound and the highest bucket upper
	// bound of the side's windows. The true p95s lie within them.
	BucketLowerSeconds *float64 `json:"bucket_lower_seconds,omitempty"`
	BucketUpperSeconds *float64 `json:"bucket_upper_seconds,omitempty"`
	// Requests is the side's requests (or histogram observations), and
	// ServerErrors its 5xx responses for the server error ratio.
	Requests     float64  `json:"requests"`
	ServerErrors *float64 `json:"server_errors_5xx,omitempty"`
	// ElapsedSeconds is the side's total window time.
	ElapsedSeconds float64 `json:"elapsed_seconds"`
}

// Change compares the comparison with the baseline.
type Change struct {
	// Absolute is comparison - baseline, in the measures' unit.
	Absolute float64 `json:"absolute"`
	Unit     string  `json:"unit"`
	// Relative is Absolute / baseline. It is unset for a zero baseline.
	Relative            *float64 `json:"relative,omitempty"`
	RelativeUnavailable string   `json:"relative_unavailable,omitempty"`
	// ThresholdMultiple is the change divided by its threshold, in the
	// direction of degradation: 1 is exactly at the threshold, and below 0
	// means the signal improved.
	ThresholdMultiple float64 `json:"threshold_multiple"`
}

// Gap is a window in which a route's signal is unavailable.
type Gap struct {
	Window int    `json:"window"`
	Reason string `json:"reason"`
}

// Finding types.
const (
	FindingServerErrorIncrease   = "server_error_increase"
	FindingLatencyDegradation    = "latency_degradation"
	FindingThroughputDegradation = "throughput_degradation"
	FindingCombinedDegradation   = "combined_degradation"
	FindingInsufficientEvidence  = "insufficient_evidence"
)

// Designations of a finding. None is a root cause.
const (
	// A measured change.
	DesignationObservation = "observation"
	// A possible link between observations, to be tested.
	DesignationHypothesis = "hypothesis"
	// The engine declined to conclude anything.
	DesignationAbstention = "abstention"
)

// Severities. Insufficient evidence has none.
const (
	SeverityMajor    = "major"
	SeverityModerate = "moderate"
)

// Finding is one ranked result.
type Finding struct {
	Rank int `json:"rank"`
	// ID is stable: the same type, method, and route always give the same ID.
	ID          string `json:"id"`
	Type        string `json:"type"`
	Designation string `json:"designation"`
	// ProvenRootCause is always false: no finding establishes a cause.
	ProvenRootCause bool   `json:"proven_root_cause"`
	Method          string `json:"method"`
	Route           string `json:"route"`
	Severity        string `json:"severity,omitempty"`
	Symptom         string `json:"symptom"`
	// Changes are the compared signals, with values and units.
	Changes           []SignalChange `json:"changes"`
	BaselineWindows   []int          `json:"baseline_windows"`
	ComparisonWindows []int          `json:"comparison_windows"`
	Evidence          []EvidenceRef  `json:"evidence"`
	Explanation       string         `json:"explanation"`
	Ranking           Ranking        `json:"ranking"`
	// Contradictions are measurements that weaken the finding.
	Contradictions []string `json:"contradictions"`
	Limitations    []string `json:"limitations"`
	// PossibleExplanations are hypotheses to test, none established.
	PossibleExplanations []string `json:"possible_explanations"`
	NextExperiment       string   `json:"next_experiment"`
	// Gaps, for insufficient evidence: what could not be compared.
	Gaps []SignalGap `json:"gaps,omitempty"`
	// Related are the IDs of the findings a combined finding joins.
	Related []string `json:"related,omitempty"`
}

// SignalChange is one signal's comparison within a finding.
type SignalChange struct {
	Signal     string  `json:"signal"`
	Baseline   Measure `json:"baseline"`
	Comparison Measure `json:"comparison"`
	Change     Change  `json:"change"`
}

// SignalGap is why one signal of a route could not be compared.
type SignalGap struct {
	Signal string `json:"signal"`
	Reason string `json:"reason"`
	Gaps   []Gap  `json:"gaps"`
}

// EvidenceRef points at one measurement in the analysis file.
type EvidenceRef struct {
	// Pointer is an RFC 6901 JSON pointer into the analysis file, such as
	// /analysis/windows/1/routes/0/latency/p95 (array positions count from
	// 0; window indexes from 1).
	Pointer string `json:"pointer"`
	Window  int    `json:"window,omitempty"`
	// Role is "baseline", "comparison", "warning" (an analysis warning about
	// the route), or "excluded" (the route in a window that is not compared).
	Role   string              `json:"role"`
	Metric string              `json:"metric,omitempty"`
	Series []map[string]string `json:"series,omitempty"`
}

// Ranking holds the values findings are ordered by.
type Ranking struct {
	Severity          string  `json:"severity,omitempty"`
	TypePriority      int     `json:"type_priority"`
	ThresholdMultiple float64 `json:"threshold_multiple"`
	// Windows is the number of windows compared (both sides).
	Windows int `json:"windows"`
	// Requests is the number of requests or observations compared.
	Requests float64 `json:"requests"`
	Rule     string  `json:"rule"`
}

// RankingRule describes the ordering, written into every finding.
const RankingRule = "observations and hypotheses before abstentions; then major before moderate severity; " +
	"then type (combined, server errors, latency, throughput); then the larger threshold multiple; " +
	"then more windows and requests compared; then method, route, and ID"
