package runvalidation

import (
	"fmt"

	"github.com/RajK158/ruptiq/internal/fixvalidation"
)

// Inputs supplies the JSON writer. Tests replace write to simulate a failed
// write. The default writer renames a temporary file in the output directory.
type Inputs struct {
	write func(dir, name string, v any) error
}

// Run checks both diagnosis directories and writes validation.json and
// manifest.json. not_comparable and insufficient_evidence are successful
// results. A returned error means the inputs or the output could not be used.
// Neither input directory is modified.
func Run(cfg Config, in Inputs) (Result, error) {
	cfg = cfg.WithDefaults()
	if err := cfg.Validate(); err != nil {
		return Result{}, err
	}
	before, err := loadDiagnosis(cfg.BeforeDir, "before", cfg.MaxInputBytes)
	if err != nil {
		return Result{}, wrapInput("before", err)
	}
	after, err := loadDiagnosis(cfg.AfterDir, "after", cfg.MaxInputBytes)
	if err != nil {
		return Result{}, wrapInput("after", err)
	}
	compared, err := fixvalidation.Compare(before.analysis, after.analysis, cfg.Validation)
	if err != nil {
		return Result{}, err
	}
	cfg.OutputDir, err = canonical(cfg.OutputDir)
	if err != nil {
		return Result{}, err
	}

	dir, err := ensureOutputDir(cfg.OutputDir)
	if err != nil {
		return Result{}, err
	}
	release, err := reserveOutputDir(dir)
	if err != nil {
		return Result{}, err
	}
	defer release()
	if err := rejectExistingArtifacts(dir); err != nil {
		return Result{}, err
	}

	write := in.write
	if write == nil {
		write = writeJSONFile
	}
	report := fixvalidation.Report{
		SchemaVersion: fixvalidation.SchemaVersion,
		Before:        before.input.AnalysisFile,
		After:         after.input.AnalysisFile,
		Validation:    &compared,
	}
	warnings := append([]string{}, compared.Comparability.Warnings...)
	manifest := Manifest{
		SchemaVersion: SchemaVersion,
		Versions:      currentVersions(),
		Outcome:       compared.Outcome,
		Comparable:    compared.Comparability.Comparable,
		Before:        before.input,
		After:         after.input,
		Artifacts: []Artifact{
			{Name: FileValidation, Schema: "fixvalidation", SchemaVersion: fixvalidation.SchemaVersion},
			{Name: FileManifest, Schema: "runvalidation", SchemaVersion: SchemaVersion},
		},
		Warnings:    warnings,
		Limitations: limitations(before.input, after.input, warnings),
	}
	if sameAnalysisFile(before.input, after.input) {
		manifest.Limitations = append(manifest.Limitations, duplicateSourceLimitation)
	}
	if err := write(dir, FileValidation, report); err != nil {
		return Result{}, fmt.Errorf("writing %s: %w", FileValidation, err)
	}
	if err := write(dir, FileManifest, manifest); err != nil {
		return Result{}, fmt.Errorf("writing %s: %w", FileManifest, err)
	}
	return Result{Dir: dir, Manifest: manifest, Report: report}, nil
}

const duplicateSourceLimitation = "The before and after analysis files have the same SHA-256. They are copies of one saved analysis, not two independent experiments, and this comparison is not evidence of a change."

func sameAnalysisFile(before, after Input) bool {
	return before.AnalysisSHA256 != "" && before.AnalysisSHA256 == after.AnalysisSHA256
}

func limitations(before, after Input, warnings []string) []string {
	out := baseLimitations(warnings)
	for _, side := range []Input{before, after} {
		if side.ExperimentStatus != "" && side.ExperimentStatus != "passed" {
			out = append(out, fmt.Sprintf("The %s experiment status is %s. The comparison still uses the saved analysis.", side.Label, side.ExperimentStatus))
		}
		if side.TelemetryCompleteness != "" && side.TelemetryCompleteness != "complete" {
			out = append(out, fmt.Sprintf("The %s telemetry completeness is %s. The comparison still uses the eligible windows in the saved analysis.", side.Label, side.TelemetryCompleteness))
		}
		if side.DiagnosisOutcome == "failed" {
			out = append(out, fmt.Sprintf("The %s diagnosis outcome is failed. Its analysis stage succeeded, so that analysis was compared.", side.Label))
		}
	}
	return out
}
