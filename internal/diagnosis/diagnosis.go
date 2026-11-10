// Package diagnosis runs one performance diagnosis: an experiment, then
// analysis, then investigation. It orchestrates those packages. It does not
// calculate signals, rank findings, or compare two runs.
//
// A completed run means the stages that could run finished and their reports
// were saved. It is not itself a performance finding. An investigation that
// abstains is a completed investigation, not a failure and not evidence that
// the service is healthy.
package diagnosis

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/RajK158/ruptiq/internal/analysis"
	"github.com/RajK158/ruptiq/internal/experiment"
	"github.com/RajK158/ruptiq/internal/investigation"
	"github.com/RajK158/ruptiq/internal/workload"
)

// SchemaVersion is the current Manifest format.
const SchemaVersion = 1

// Artifact file names written inside the output directory. They are relative
// to that directory and are the only names the package writes.
const (
	FileExperiment    = "experiment.json"
	FileAnalysis      = "analysis.json"
	FileInvestigation = "investigation.json"
	FileManifest      = "manifest.json"
)

// Outcomes of a diagnosis. OutcomeCompleted means every stage that the
// experiment's data allowed did finish and its artifact was saved. An
// abstaining investigation is still OutcomeCompleted.
const (
	OutcomeCompleted = "completed"
	OutcomeFailed    = "failed"
)

// Stage names, in pipeline order.
const (
	StageExperiment    = "experiment"
	StageAnalysis      = "analysis"
	StageInvestigation = "investigation"
)

// Stage statuses.
const (
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusSkipped   = "skipped"
)

// Config is one diagnosis. Zero analysis and investigation configs are
// replaced by those packages' defaults; set every field of either config
// when any of its fields is set.
type Config struct {
	// OutputDir is the directory that receives the four artifacts. It is
	// created if it does not exist. An existing artifact in it is an error.
	OutputDir string
	// Experiment is the workload and telemetry sampling configuration.
	Experiment experiment.Config
	// Analysis names the metric families and labels. A zero config uses
	// analysis.DefaultConfig.
	Analysis analysis.Config
	// Investigation holds the investigator's thresholds. A zero config uses
	// investigation.DefaultConfig.
	Investigation investigation.Config
}

// WithDefaults returns c with zero experiment, analysis, and investigation
// settings replaced by their packages' defaults.
func (c Config) WithDefaults() Config {
	c.Experiment = c.Experiment.WithDefaults()
	if c.Analysis.RequestsCounter == "" && c.Analysis.DurationHistogram == "" && c.Analysis.MinObservations == 0 {
		c.Analysis = analysis.DefaultConfig()
	}
	if c.Investigation.MinWindows == 0 && c.Investigation.MinRequests == 0 {
		c.Investigation = investigation.DefaultConfig()
	}
	return c
}

// ValidationError lists every problem found in a Config.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	return "invalid diagnosis: " + strings.Join(e.Problems, "; ")
}

// Validate reports every problem with c, or nil. Call it on c.WithDefaults().
// It does not create the output directory or contact the target.
func (c Config) Validate() error {
	var problems []string
	if strings.TrimSpace(c.OutputDir) == "" {
		problems = append(problems, "output directory is required")
	}
	if strings.ContainsRune(c.OutputDir, 0) {
		problems = append(problems, "output directory contains a NUL byte")
	}
	if err := c.Experiment.Validate(); err != nil {
		var verr *experiment.ValidationError
		if errors.As(err, &verr) {
			problems = append(problems, verr.Problems...)
		} else {
			problems = append(problems, err.Error())
		}
	}
	if err := c.Analysis.Validate(); err != nil {
		problems = append(problems, err.Error())
	}
	if err := c.Investigation.Validate(); err != nil {
		problems = append(problems, err.Error())
	}
	if len(problems) > 0 {
		return &ValidationError{Problems: problems}
	}
	return nil
}

// Versions records the schema of each artifact this diagnosis writes.
type Versions struct {
	Diagnosis     int `json:"diagnosis_schema"`
	Experiment    int `json:"experiment_schema"`
	Analysis      int `json:"analysis_schema"`
	Investigation int `json:"investigation_schema"`
}

// Stage is one pipeline stage as it actually finished.
type Stage struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	// Artifact is the file name relative to the output directory, set only
	// when this stage wrote one.
	Artifact string `json:"artifact,omitempty"`
	// ExperimentStatus is the experiment report's status, when this stage is
	// the experiment and a report was saved.
	ExperimentStatus string `json:"experiment_status,omitempty"`
	// InvestigationOutcome is the investigator's outcome, when this stage
	// produced an investigation. insufficient_evidence means it abstained.
	InvestigationOutcome string `json:"investigation_outcome,omitempty"`
	// Reason explains a skipped stage.
	Reason string `json:"reason,omitempty"`
	// Error explains a failed stage. It is redacted.
	Error string `json:"error,omitempty"`
}

// Manifest is the versioned record of one diagnosis. Paths are file names
// relative to the output directory. URLs are redacted.
type Manifest struct {
	SchemaVersion int       `json:"schema_version"`
	Versions      Versions  `json:"versions"`
	Outcome       string    `json:"outcome"`
	StartedAt     time.Time `json:"started_at"`
	FinishedAt    time.Time `json:"finished_at"`
	TargetURL     string    `json:"target_url,omitempty"`
	MetricsURL    string    `json:"metrics_url,omitempty"`
	Stages        []Stage   `json:"stages"`
	// Artifacts lists the file names written, including this manifest.
	Artifacts   []string `json:"artifacts"`
	Limitations []string `json:"limitations"`
}

// Result is a finished diagnosis, including one that did not complete.
// Earlier artifacts are kept on disk when a later stage fails.
type Result struct {
	Dir           string
	Manifest      Manifest
	Experiment    *experiment.Report
	Analysis      *analysis.Analysis
	Investigation *investigation.Investigation
}

// StageError means the diagnosis did not complete. Valid artifacts written
// before the failure are left in place. Message is already redacted.
type StageError struct {
	Stage   string
	Message string
}

func (e *StageError) Error() string {
	if e.Message == "" {
		return e.Stage + " failed"
	}
	return e.Stage + ": " + e.Message
}

func currentVersions() Versions {
	return Versions{
		Diagnosis:     SchemaVersion,
		Experiment:    experiment.ReportSchemaVersion,
		Analysis:      analysis.SchemaVersion,
		Investigation: investigation.SchemaVersion,
	}
}

func baseLimitations() []string {
	return []string{
		"Completing this command means the stages that could run finished and their reports were saved. It is not itself a performance finding.",
		"Findings are observations or hypotheses from one run. They are not root causes, and no statistical significance is claimed.",
		"An investigation outcome of insufficient_evidence is an abstention: not a command failure, and not evidence that performance is healthy.",
		"This command diagnoses one run. Comparing a before run with an after run is ruptiq-validate-fix, a separate command.",
	}
}

func clean(text, target, metrics string) string {
	text = workload.RedactText(text, target)
	text = workload.RedactText(text, metrics)
	text = strings.Join(strings.Fields(text), " ")
	const max = 300
	if len(text) > max {
		text = text[:max] + "..."
	}
	return text
}

func stageErr(stage, message string) error {
	return &StageError{Stage: stage, Message: message}
}

// analyzable is the experiment statuses whose workload finished, which is
// the set ruptiq-analyze accepts. Cancelled, timed out, and errored
// experiments are saved and not analysed.
func analyzable(s experiment.Status) bool {
	switch s {
	case experiment.StatusPassed, experiment.StatusFailed, experiment.StatusIncomplete:
		return true
	default:
		return false
	}
}

func notAnalyzableReason(s experiment.Status) string {
	return fmt.Sprintf("the experiment status is %s; analysis runs only when the workload completed (passed, failed its pass criteria, or incomplete telemetry)", s)
}
