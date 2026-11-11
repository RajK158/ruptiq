// Package pairworkflow guides one before/after performance comparison as
// three separate command invocations: capture a baseline, capture a
// candidate, then compare the two saved diagnoses.
//
// The process exits at the end of each invocation. It does not apply a code
// or deployment change, restart a service, or start the next stage. The
// developer does that work outside Ruptiq and then starts the next stage
// explicitly.
//
// Compare calls runvalidation.Run, which calls fixvalidation.Compare. This
// package does not calculate throughput, error ratios, histogram quantiles,
// or rankings. A finished workflow, including not_comparable and
// insufficient_evidence, is not proof that a change caused the difference.
package pairworkflow

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/RajK158/ruptiq/internal/analysis"
	"github.com/RajK158/ruptiq/internal/diagnosis"
	"github.com/RajK158/ruptiq/internal/fixvalidation"
	"github.com/RajK158/ruptiq/internal/runvalidation"
)

// SchemaVersion is the current pair-manifest format.
const SchemaVersion = 1

// Stage names. Each one is a separate process that exits when it finishes.
const (
	StageBefore  = "before"
	StageAfter   = "after"
	StageCompare = "compare"
)

// Workflow states that the saved artifacts can support. A failed or partial
// stage is never recorded as a captured baseline, a captured candidate, or a
// completed comparison.
const (
	StateBaselineCaptured    = "baseline_captured"
	StateCandidateCaptured   = "candidate_captured"
	StateComparisonCompleted = "comparison_completed"
	StateFailed              = "failed"
	StateInterrupted         = "interrupted"
)

// Directory and file names inside the pair directory. They are relative
// names, not absolute paths.
const (
	DirBefore       = "before"
	DirAfter        = "after"
	DirComparison   = "comparison"
	FileManifest    = "pair-manifest.json"
	reservationFile = ".ruptiq-pair-reservation"
)

// BaselineCapturedMessage is printed after a baseline has been saved. The
// process exits after this. It does not wait for a change.
const BaselineCapturedMessage = "Baseline captured. Apply your change outside Ruptiq, then explicitly run the after stage. Ruptiq does not apply or verify a code change."

// CandidateCapturedMessage is printed after the candidate diagnosis has been
// saved. Compare is a separate invocation.
const CandidateCapturedMessage = "Candidate captured. Run the compare stage to judge the two saved diagnoses. Ruptiq does not apply or verify a code change."

// Config is one invocation of the workflow.
// A zero Diagnosis analysis or investigation config, and a zero Validation,
// are replaced by those packages' defaults. Set every field of either config
// when any of its fields is set.
type Config struct {
	// Stage is before, after, or compare.
	Stage string
	// PairDir is the workflow directory. It holds before/, after/,
	// comparison/, and pair-manifest.json.
	PairDir string
	// Diagnosis is the experiment to run for before and after. OutputDir is
	// ignored; the stage directory inside PairDir is used.
	Diagnosis diagnosis.Config
	// Validation is the fix-validation config used by compare. A zero config
	// uses fixvalidation.DefaultConfig.
	Validation fixvalidation.Config
	// MaxInputBytes bounds diagnosis artifacts read back by compare.
	MaxInputBytes int64
}

// WithDefaults fills zero diagnosis, comparison, and size settings.
func (c Config) WithDefaults() Config {
	c.Diagnosis = c.Diagnosis.WithDefaults()
	if c.MaxInputBytes == 0 {
		c.MaxInputBytes = analysis.DefaultMaxInputBytes
	}
	if c.Validation == (fixvalidation.Config{}) {
		c.Validation = fixvalidation.DefaultConfig()
	}
	return c
}

// ValidationError is a problem with the request, the pair directory, or a
// stage that was refused before it could overwrite saved reports.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	return "invalid pair workflow: " + strings.Join(e.Problems, "; ")
}

// InputError means a saved manifest or stage directory is corrupt or
// disagrees with the files next to it. Nothing was rewritten to repair it.
type InputError struct {
	Problems []string
}

func (e *InputError) Error() string {
	return "invalid pair workflow input: " + strings.Join(e.Problems, "; ")
}

// Validate reports every problem with c, or nil. Call it on c.WithDefaults().
// It does not create the pair directory or contact a target.
func (c Config) Validate() error {
	var problems []string
	switch c.Stage {
	case StageBefore, StageAfter, StageCompare:
	default:
		problems = append(problems, "stage must be before, after, or compare")
	}
	if strings.TrimSpace(c.PairDir) == "" {
		problems = append(problems, "pair directory is required")
	}
	if strings.ContainsRune(c.PairDir, 0) {
		problems = append(problems, "pair directory contains a NUL byte")
	}
	if c.MaxInputBytes < analysis.MinInputBytes || c.MaxInputBytes > analysis.MaxInputBytesLimit {
		problems = append(problems, fmt.Sprintf("max input bytes must be between %d and %d, got %d",
			analysis.MinInputBytes, analysis.MaxInputBytesLimit, c.MaxInputBytes))
	}
	if err := c.Validation.Validate(); err != nil {
		problems = append(problems, err.Error())
	}
	if c.Stage == StageBefore || c.Stage == StageAfter {
		d := c.Diagnosis
		d.OutputDir = c.PairDir
		if err := d.Validate(); err != nil {
			var verr *diagnosis.ValidationError
			if errors.As(err, &verr) {
				problems = append(problems, verr.Problems...)
			} else {
				problems = append(problems, err.Error())
			}
		}
	}
	if len(problems) > 0 {
		return &ValidationError{Problems: problems}
	}
	return nil
}

// Versions records the schemas this workflow reads and writes.
type Versions struct {
	PairWorkflow  int `json:"pair_workflow_schema"`
	Diagnosis     int `json:"diagnosis_schema"`
	RunValidation int `json:"run_validation_schema"`
	FixValidation int `json:"fix_validation_schema"`
}

// Manifest is the versioned workflow state. Paths inside it are the directory
// names before, after, and comparison. URLs are redacted, and URL identity
// beyond that redaction is a SHA-256 of the original string, not the string.
type Manifest struct {
	SchemaVersion int              `json:"schema_version"`
	Versions      Versions         `json:"versions"`
	State         string           `json:"state"`
	Before        *DiagnosisStage  `json:"before,omitempty"`
	After         *DiagnosisStage  `json:"after,omitempty"`
	Comparison    *ComparisonStage `json:"comparison,omitempty"`
	Fingerprint   *Fingerprint     `json:"fingerprint,omitempty"`
	Failure       *Failure         `json:"failure,omitempty"`
	Limitations   []string         `json:"limitations"`
	UpdatedAt     time.Time        `json:"updated_at"`
}

// DiagnosisStage is one saved diagnosis the workflow has verified.
type DiagnosisStage struct {
	Directory             string `json:"directory"`
	DiagnosisOutcome      string `json:"diagnosis_outcome"`
	ExperimentStatus      string `json:"experiment_status"`
	TelemetryCompleteness string `json:"telemetry_completeness"`
	InvestigationOutcome  string `json:"investigation_outcome,omitempty"`
	AnalysisSHA256        string `json:"analysis_sha256"`
}

// ComparisonStage is one saved run-validation result.
type ComparisonStage struct {
	Directory            string   `json:"directory"`
	Outcome              string   `json:"outcome"`
	Comparable           bool     `json:"comparable"`
	BeforeAnalysisSHA256 string   `json:"before_analysis_sha256"`
	AfterAnalysisSHA256  string   `json:"after_analysis_sha256"`
	Warnings             []string `json:"warnings,omitempty"`
}

// Failure explains a stage that did not complete. Recovery is a manual step.
// This workflow does not delete the stage directory.
type Failure struct {
	Stage    string `json:"stage"`
	Message  string `json:"message"`
	Recovery string `json:"recovery"`
}

// Result is the outcome of one invocation.
type Result struct {
	// Dir is the pair directory that was used, after aliases were resolved.
	Dir        string
	Manifest   Manifest
	Comparison *runvalidation.Result
}

func currentVersions() Versions {
	return Versions{
		PairWorkflow:  SchemaVersion,
		Diagnosis:     diagnosis.SchemaVersion,
		RunValidation: runvalidation.SchemaVersion,
		FixValidation: fixvalidation.SchemaVersion,
	}
}

func baseLimitations() []string {
	return []string{
		"Each stage is a separate invocation. The process exits when the stage finishes and does not start the next stage.",
		"Ruptiq does not edit application code, restart a service, run a deployment, or decide that a change was applied.",
		"The developer applies any change outside Ruptiq after the baseline and before the after stage.",
		"A completed workflow is not proof that a change caused the difference, and one pair is not a statistical significance test.",
		"not_comparable and insufficient_evidence are completed comparisons. They are not command failures.",
		"An investigation outcome of insufficient_evidence is an abstention, not a command failure and not evidence that performance is healthy.",
		"SHA-256 values in this manifest are consistency fingerprints of the original URL strings or analysis bytes. They are not a secure substitute for protecting a short or guessable secret: anyone who can guess the value can test it against the hash.",
		"The pair manifest stores redacted URLs. It does not store URL user information, query strings, or tokens.",
		"A change acknowledgment is not proof of causality.",
	}
}
