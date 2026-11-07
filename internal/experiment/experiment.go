// Package experiment coordinates a performance experiment: one workload run
// while the target's telemetry is sampled periodically, combined into one
// report.
//
// It drives a workload.Runner and a telemetry.Reader and knows nothing about
// any target application. Telemetry samples are timestamped snapshots taken
// alongside the workload; they are not correlated with individual requests.
package experiment

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/RajK158/ruptiq/internal/telemetry"
	"github.com/RajK158/ruptiq/internal/workload"
)

// Defaults and limits for a Config.
const (
	DefaultInterval   = 1 * time.Second
	MinInterval       = 100 * time.Millisecond
	MaxInterval       = 1 * time.Minute
	DefaultMaxSamples = 1000
	MaxSamplesLimit   = 10000
	// DefaultMaxTelemetryBytes is the default retention budget: the most
	// serialized sample data (JSON) kept from one experiment.
	DefaultMaxTelemetryBytes = 32 << 20
	MinTelemetryBytes        = 1 << 10
	MaxTelemetryBytesLimit   = 1 << 30
	// DefaultTimeoutAllowance is added to the workload duration when no
	// timeout is set. It covers k6 starting and stopping and the scrapes
	// before and after the workload.
	DefaultTimeoutAllowance = 60 * time.Second
)

// Config describes one experiment.
type Config struct {
	// Workload is the load to apply.
	Workload workload.Spec
	// MaxFailureRate is the workload's pass criterion (see workload.Evaluate).
	MaxFailureRate float64
	// MetricsURL is the Prometheus endpoint the telemetry reader uses. It is
	// only validated and recorded (redacted) here; the reader does the reading.
	MetricsURL string
	// Interval is the time between scrapes while the workload runs. Zero
	// means DefaultInterval.
	Interval time.Duration
	// MaxSamples bounds the number of scrape attempts, including the ones
	// before and after the workload. Zero means DefaultMaxSamples.
	MaxSamples int
	// MaxTelemetryBytes is the retention budget: the most sample data, measured
	// as each sample's JSON size, kept in memory and in the report. Zero means
	// DefaultMaxTelemetryBytes. It approximates, but does not cap, the memory
	// the samples use.
	MaxTelemetryBytes int64
	// Timeout bounds the whole experiment. Zero means the workload duration
	// plus DefaultTimeoutAllowance.
	Timeout time.Duration
}

// WithDefaults returns c with zero values replaced by their defaults.
func (c Config) WithDefaults() Config {
	if c.Interval == 0 {
		c.Interval = DefaultInterval
	}
	if c.MaxSamples == 0 {
		c.MaxSamples = DefaultMaxSamples
	}
	if c.MaxTelemetryBytes == 0 {
		c.MaxTelemetryBytes = DefaultMaxTelemetryBytes
	}
	if c.Timeout == 0 {
		c.Timeout = c.Workload.Duration + DefaultTimeoutAllowance
	}
	return c
}

// ValidationError lists every problem found in a Config.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	return "invalid experiment: " + strings.Join(e.Problems, "; ")
}

// Validate reports every problem with c, or nil. Call it on
// c.WithDefaults(). It never contacts the target or the metrics endpoint.
func (c Config) Validate() error {
	var problems []string
	if err := c.Workload.Validate(); err != nil {
		var verr *workload.ValidationError
		if errors.As(err, &verr) {
			problems = append(problems, verr.Problems...)
		} else {
			problems = append(problems, err.Error())
		}
	}
	if err := workload.ValidateMaxFailureRate(c.MaxFailureRate); err != nil {
		problems = append(problems, err.Error())
	}
	if _, err := telemetry.NewPrometheusReader(telemetry.PrometheusConfig{URL: c.MetricsURL}); err != nil {
		problems = append(problems, "metrics URL: "+err.Error())
	}

	intervalOK := c.Interval >= MinInterval && c.Interval <= MaxInterval
	if !intervalOK {
		problems = append(problems, fmt.Sprintf("interval must be between %v and %v, got %v", MinInterval, MaxInterval, c.Interval))
	}
	samplesOK := c.MaxSamples >= 3 && c.MaxSamples <= MaxSamplesLimit
	if !samplesOK {
		problems = append(problems, fmt.Sprintf("max samples must be between 3 and %d, got %d", MaxSamplesLimit, c.MaxSamples))
	}
	if d := c.Workload.Duration; d > 0 && intervalOK {
		if c.Interval > d {
			problems = append(problems, fmt.Sprintf("interval (%v) must not be longer than the workload duration (%v)", c.Interval, d))
		} else if need := ExpectedSamples(d, c.Interval); samplesOK && need > c.MaxSamples {
			problems = append(problems, fmt.Sprintf("sampling every %v for %v needs about %d samples, more than max samples (%d); use a longer interval or more samples",
				c.Interval, d, need, c.MaxSamples))
		}
	}
	if c.MaxTelemetryBytes < MinTelemetryBytes || c.MaxTelemetryBytes > MaxTelemetryBytesLimit {
		problems = append(problems, fmt.Sprintf("max telemetry bytes must be between %d and %d, got %d",
			MinTelemetryBytes, MaxTelemetryBytesLimit, c.MaxTelemetryBytes))
	}
	if c.Timeout <= c.Workload.Duration {
		problems = append(problems, fmt.Sprintf("timeout (%v) must be longer than the workload duration (%v)", c.Timeout, c.Workload.Duration))
	}
	if len(problems) > 0 {
		return &ValidationError{Problems: problems}
	}
	return nil
}

// ExpectedSamples is the number of scrapes an experiment needs if the
// workload runs for exactly duration: one before, one per interval during,
// and one after. Real runs take longer (k6 starts and stops), so MaxSamples
// should leave room.
func ExpectedSamples(duration, interval time.Duration) int {
	return int(duration/interval) + 2
}

// ReportSchemaVersion is the current Report format.
const ReportSchemaVersion = 1

// Status is the overall outcome of an experiment. Only StatusPassed means the
// workload completed, met its pass criteria, and its telemetry is complete.
type Status string

const (
	// StatusPassed: the workload passed and the telemetry is complete.
	StatusPassed Status = "passed"
	// StatusFailed: the workload completed but failed its pass criteria, for
	// example because too many requests failed.
	StatusFailed Status = "failed"
	// StatusIncomplete: the workload passed, but the telemetry is partial or
	// missing.
	StatusIncomplete Status = "incomplete"
	// StatusError: the workload could not be run or its results could not be
	// read.
	StatusError Status = "error"
	// StatusCancelled: the experiment was stopped before it finished.
	StatusCancelled Status = "cancelled"
	// StatusTimedOut: the experiment did not finish within its timeout.
	StatusTimedOut Status = "timed_out"
)

// Report is the JSON file of one experiment. It is written for every
// experiment that started, including failed and cancelled ones, so an old
// successful report cannot be mistaken for the latest experiment.
type Report struct {
	SchemaVersion int    `json:"schema_version"`
	Status        Status `json:"status"`
	// Reasons explain any status other than StatusPassed.
	Reasons    []string         `json:"reasons,omitempty"`
	StartedAt  time.Time        `json:"started_at"`
	FinishedAt time.Time        `json:"finished_at"`
	Config     ReportConfig     `json:"config"`
	Workload   WorkloadOutcome  `json:"workload"`
	Telemetry  TelemetryOutcome `json:"telemetry"`
}

// ReportConfig is the experiment's configuration as recorded. URLs are
// redacted by workload.RedactURL (no user information or query string).
type ReportConfig struct {
	TargetURL             string  `json:"target_url"`
	VUs                   int     `json:"vus"`
	DurationSeconds       float64 `json:"duration_seconds"`
	MaxFailureRate        float64 `json:"max_failure_rate"`
	MetricsURL            string  `json:"metrics_url"`
	SampleIntervalSeconds float64 `json:"sample_interval_seconds"`
	MaxSamples            int     `json:"max_samples"`
	MaxTelemetryBytes     int64   `json:"max_telemetry_bytes"`
	TimeoutSeconds        float64 `json:"timeout_seconds"`
}

// WorkloadOutcome is the workload's result or error. Exactly one of Result
// and Error is set.
type WorkloadOutcome struct {
	Result  *workload.Result `json:"result,omitempty"`
	Error   string           `json:"error,omitempty"`
	Verdict workload.Verdict `json:"verdict"`
}

// Completeness says how much of the planned telemetry was collected.
type Completeness string

const (
	// Complete: every scrape succeeded and was retained, there is at least one
	// sample before, during, and after the workload, and neither the sample
	// limit nor the retention budget was reached.
	Complete Completeness = "complete"
	// Partial: some samples were collected, but not all of the above holds.
	Partial Completeness = "partial"
	// None: no sample was collected.
	None Completeness = "none"
)

// Phase says when a scrape was made relative to the workload run.
type Phase string

const (
	// BeforeWorkload: the baseline scrape, before the workload was started.
	BeforeWorkload Phase = "before_workload"
	// DuringWorkload: while the workload runner was running. This includes
	// the load generator starting up and shutting down, so the first and last
	// of these samples may see little or no load.
	DuringWorkload Phase = "during_workload"
	// AfterWorkload: the final scrape, after the workload runner returned.
	AfterWorkload Phase = "after_workload"
)

// TelemetryOutcome is the telemetry collected during an experiment.
//
// Every scrape attempt gets the next Sequence number and ends up in exactly
// one of Samples, Errors, and NotRetained, so
// Attempts = Succeeded + Failed, Succeeded = len(Samples) + len(NotRetained),
// and gaps in Samples' sequence numbers are explained by the other two lists.
// A scrape interrupted by the experiment being cancelled or timing out is not
// counted.
type TelemetryOutcome struct {
	Completeness Completeness `json:"completeness"`
	// CompletenessReasons explain any completeness other than Complete.
	CompletenessReasons []string `json:"completeness_reasons,omitempty"`
	Attempts            int      `json:"attempts"`
	Succeeded           int      `json:"succeeded"`
	Failed              int      `json:"failed"`
	// SampleLimitReached means sampling stopped at MaxSamples while the
	// workload was still running.
	SampleLimitReached bool `json:"sample_limit_reached"`
	// RetainedBytes is the JSON size of the retained samples, counted
	// against MaxTelemetryBytes.
	RetainedBytes int64 `json:"retained_bytes"`
	// RetentionLimitReached means a successful scrape did not fit in the
	// retention budget: it was not kept, and sampling stopped.
	RetentionLimitReached bool          `json:"retention_limit_reached"`
	Samples               []Sample      `json:"samples"`
	Errors                []ScrapeError `json:"errors"`
	NotRetained           []NotRetained `json:"not_retained"`
}

// Sample is one successful scrape. Families are the telemetry.Snapshot's
// families, as the target exposed them; the endpoint is recorded once, in
// ReportConfig.MetricsURL.
type Sample struct {
	Sequence int   `json:"sequence"`
	Phase    Phase `json:"phase"`
	// ScrapedAt is when the request was sent, in UTC.
	ScrapedAt time.Time `json:"scraped_at"`
	// OffsetSeconds is ScrapedAt relative to Report.StartedAt.
	OffsetSeconds         float64            `json:"offset_seconds"`
	ScrapeDurationSeconds float64            `json:"scrape_duration_seconds"`
	Families              []telemetry.Family `json:"families"`
}

// ScrapeError is one failed scrape. No metric values are recorded for it.
type ScrapeError struct {
	Sequence    int       `json:"sequence"`
	Phase       Phase     `json:"phase"`
	AttemptedAt time.Time `json:"attempted_at"`
	// OffsetSeconds is AttemptedAt relative to Report.StartedAt.
	OffsetSeconds float64 `json:"offset_seconds"`
	Error         string  `json:"error"`
}

// NotRetained is a successful scrape whose sample was not kept because it
// did not fit in the retention budget. Only its size is recorded, not its
// values.
type NotRetained struct {
	Sequence      int       `json:"sequence"`
	Phase         Phase     `json:"phase"`
	ScrapedAt     time.Time `json:"scraped_at"`
	OffsetSeconds float64   `json:"offset_seconds"`
	// SizeBytes is the JSON size the sample would have had.
	SizeBytes int64 `json:"size_bytes"`
}
