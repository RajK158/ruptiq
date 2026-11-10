package pairworkflow

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/RajK158/ruptiq/internal/analysis"
	"github.com/RajK158/ruptiq/internal/diagnosis"
	"github.com/RajK158/ruptiq/internal/experiment"
	"github.com/RajK158/ruptiq/internal/fixvalidation"
	"github.com/RajK158/ruptiq/internal/runvalidation"
	"github.com/RajK158/ruptiq/internal/workload"
)

const (
	diagnoseReservation = ".ruptiq-diagnose-reservation"
	compareReservation  = ".ruptiq-validate-runs-reservation"

	kindAbsent       = "absent"
	kindReserved     = "reserved"
	kindPartial      = "partial"
	kindFailed       = "failed"
	kindCompleted    = "completed"
	kindInconsistent = "inconsistent"
)

type diagFact struct {
	kind        string
	stage       DiagnosisStage
	fingerprint Fingerprint
	detail      string
}

type compFact struct {
	kind   string
	stage  ComparisonStage
	detail string
}

type facts struct {
	before     diagFact
	after      diagFact
	comparison compFact
	manifest   *Manifest
}

func inspect(root string, maxBytes int64) (facts, error) {
	var f facts
	var err error
	f.before, err = classifyDiagnosis(filepath.Join(root, DirBefore), DirBefore, maxBytes)
	if err != nil {
		return facts{}, err
	}
	f.after, err = classifyDiagnosis(filepath.Join(root, DirAfter), DirAfter, maxBytes)
	if err != nil {
		return facts{}, err
	}
	f.comparison, err = classifyComparison(filepath.Join(root, DirComparison), maxBytes)
	if err != nil {
		return facts{}, err
	}
	m, err := readPairManifest(root, maxBytes)
	if err != nil {
		return facts{}, err
	}
	f.manifest = m
	if m != nil {
		if problems := manifestAgrees(*m, f); len(problems) > 0 {
			return facts{}, &InputError{Problems: problems}
		}
	}
	return f, nil
}

func classifyDiagnosis(dir, label string, maxBytes int64) (diagFact, error) {
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return diagFact{kind: kindAbsent}, nil
	}
	if err != nil {
		return diagFact{}, err
	}
	if !info.IsDir() {
		return diagFact{kind: kindInconsistent, detail: label + " is not a directory"}, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return diagFact{}, err
	}
	if len(entries) == 0 {
		return diagFact{kind: kindAbsent}, nil
	}
	if _, err := os.Lstat(filepath.Join(dir, diagnoseReservation)); err == nil {
		return diagFact{kind: kindReserved, detail: label + " has an active " + diagnoseReservation + "; it was not removed"}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return diagFact{}, err
	}
	manPath := filepath.Join(dir, diagnosis.FileManifest)
	if _, err := os.Lstat(manPath); errors.Is(err, os.ErrNotExist) {
		return diagFact{kind: kindPartial, detail: label + " has files but no " + diagnosis.FileManifest}, nil
	} else if err != nil {
		return diagFact{}, err
	}
	var manifest diagnosis.Manifest
	if err := decodeFile(manPath, maxBytes, &manifest); err != nil {
		return diagFact{kind: kindInconsistent, detail: label + " manifest.json: " + cleanErr(err)}, nil
	}
	if manifest.Outcome == diagnosis.OutcomeFailed {
		return diagFact{kind: kindFailed, detail: label + " diagnosis outcome is failed"}, nil
	}
	if manifest.Outcome != diagnosis.OutcomeCompleted || !stageSucceeded(manifest, diagnosis.StageExperiment, diagnosis.FileExperiment) ||
		!stageSucceeded(manifest, diagnosis.StageAnalysis, diagnosis.FileAnalysis) ||
		!stageSucceeded(manifest, diagnosis.StageInvestigation, diagnosis.FileInvestigation) {
		return diagFact{kind: kindInconsistent, detail: label + " manifest does not record a completed diagnosis"}, nil
	}
	for _, name := range []string{diagnosis.FileExperiment, diagnosis.FileAnalysis, diagnosis.FileInvestigation} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
			return diagFact{kind: kindInconsistent, detail: label + " is missing " + name}, nil
		}
	}
	var exp experiment.Report
	if err := decodeFile(filepath.Join(dir, diagnosis.FileExperiment), maxBytes, &exp); err != nil {
		return diagFact{kind: kindInconsistent, detail: label + " experiment.json: " + cleanErr(err)}, nil
	}
	var report analysis.Report
	if err := decodeFile(filepath.Join(dir, diagnosis.FileAnalysis), maxBytes, &report); err != nil || report.Analysis == nil {
		detail := label + " analysis.json could not be read"
		if err != nil {
			detail = label + " analysis.json: " + cleanErr(err)
		}
		return diagFact{kind: kindInconsistent, detail: detail}, nil
	}
	sum, err := hashFile(filepath.Join(dir, diagnosis.FileAnalysis), maxBytes)
	if err != nil {
		return diagFact{}, err
	}
	return diagFact{
		kind: kindCompleted,
		stage: DiagnosisStage{
			Directory:             label,
			DiagnosisOutcome:      manifest.Outcome,
			ExperimentStatus:      string(exp.Status),
			TelemetryCompleteness: string(exp.Telemetry.Completeness),
			InvestigationOutcome:  investigationOutcome(manifest),
			AnalysisSHA256:        sum,
		},
		fingerprint: fingerprintFromSaved(exp, report.Analysis.Config),
	}, nil
}

func classifyComparison(dir string, maxBytes int64) (compFact, error) {
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return compFact{kind: kindAbsent}, nil
	}
	if err != nil {
		return compFact{}, err
	}
	if !info.IsDir() {
		return compFact{kind: kindInconsistent, detail: DirComparison + " is not a directory"}, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return compFact{}, err
	}
	if len(entries) == 0 {
		return compFact{kind: kindAbsent}, nil
	}
	if _, err := os.Lstat(filepath.Join(dir, compareReservation)); err == nil {
		return compFact{kind: kindReserved, detail: DirComparison + " has an active " + compareReservation + "; it was not removed"}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return compFact{}, err
	}
	manPath := filepath.Join(dir, runvalidation.FileManifest)
	valPath := filepath.Join(dir, runvalidation.FileValidation)
	_, manErr := os.Lstat(manPath)
	_, valErr := os.Lstat(valPath)
	if errors.Is(manErr, os.ErrNotExist) || errors.Is(valErr, os.ErrNotExist) {
		return compFact{kind: kindPartial, detail: DirComparison + " is missing validation.json or manifest.json"}, nil
	}
	if manErr != nil {
		return compFact{}, manErr
	}
	if valErr != nil {
		return compFact{}, valErr
	}
	var manifest runvalidation.Manifest
	if err := decodeFile(manPath, maxBytes, &manifest); err != nil {
		return compFact{kind: kindInconsistent, detail: "comparison manifest.json: " + cleanErr(err)}, nil
	}
	if manifest.SchemaVersion != runvalidation.SchemaVersion || !knownOutcome(manifest.Outcome) {
		return compFact{kind: kindInconsistent, detail: "comparison manifest outcome or schema is not a run-validation result"}, nil
	}
	return compFact{
		kind: kindCompleted,
		stage: ComparisonStage{
			Directory:            DirComparison,
			Outcome:              manifest.Outcome,
			Comparable:           manifest.Comparable,
			BeforeAnalysisSHA256: manifest.Before.AnalysisSHA256,
			AfterAnalysisSHA256:  manifest.After.AnalysisSHA256,
			Warnings:             append([]string{}, manifest.Warnings...),
		},
	}, nil
}

func readPairManifest(root string, maxBytes int64) (*Manifest, error) {
	path := filepath.Join(root, FileManifest)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var m Manifest
	if err := decodeFile(path, maxBytes, &m); err != nil {
		return nil, manifestReadError("pair-manifest.json: " + cleanErr(err))
	}
	if m.SchemaVersion != SchemaVersion {
		return nil, manifestReadError(fmt.Sprintf("pair-manifest.json schema_version %d is not supported (%d)", m.SchemaVersion, SchemaVersion))
	}
	switch m.State {
	case StateBaselineCaptured, StateCandidateCaptured, StateComparisonCompleted, StateFailed, StateInterrupted:
	default:
		return nil, manifestReadError("pair-manifest.json state " + m.State + " is not a workflow state")
	}
	return &m, nil
}

func manifestReadError(detail string) error {
	return &InputError{Problems: []string{detail + ". pair-manifest.json was not modified and the stage directories were not modified. Inspect that file, then move it aside and run -stage compare if before/ and after/ are already complete. A new after workload needs the URL fingerprints saved by -stage before."}}
}

// manifestAgrees rejects a manifest that claims a stage the files do not
// support, or that records a different analysis than the one on disk.
// A manifest that is merely behind the files is not an error; the caller
// follows the files.
func manifestAgrees(m Manifest, f facts) []string {
	var problems []string
	if m.Before != nil {
		problems = append(problems, stageClaim("before", m.Before, f.before)...)
	}
	if m.After != nil {
		problems = append(problems, stageClaim("after", m.After, f.after)...)
	}
	if m.Comparison != nil {
		if f.comparison.kind != kindCompleted || m.Comparison.Outcome != f.comparison.stage.Outcome ||
			m.Comparison.BeforeAnalysisSHA256 != f.comparison.stage.BeforeAnalysisSHA256 ||
			m.Comparison.AfterAnalysisSHA256 != f.comparison.stage.AfterAnalysisSHA256 {
			problems = append(problems, "pair-manifest.json comparison does not match comparison/")
		}
	}
	switch m.State {
	case StateBaselineCaptured:
		// A later stage that is already complete means the manifest is behind
		// the files. That is recovered from the files. It is not a false claim.
		if f.before.kind != kindCompleted || m.Before == nil {
			problems = append(problems, "pair-manifest.json says baseline_captured but before/ is not a completed diagnosis")
		}
	case StateCandidateCaptured:
		if f.before.kind != kindCompleted || f.after.kind != kindCompleted || m.Before == nil || m.After == nil {
			problems = append(problems, "pair-manifest.json says candidate_captured but before/ and after/ are not both completed diagnoses")
		}
	case StateComparisonCompleted:
		if f.before.kind != kindCompleted || f.after.kind != kindCompleted || f.comparison.kind != kindCompleted || m.Comparison == nil {
			problems = append(problems, "pair-manifest.json says comparison_completed but the stage directories do not")
		}
	case StateFailed, StateInterrupted:
		if m.Failure == nil || (m.Failure.Stage != StageBefore && m.Failure.Stage != StageAfter && m.Failure.Stage != StageCompare) {
			problems = append(problems, "pair-manifest.json failure does not name before, after, or compare")
		}
	}
	if m.Fingerprint != nil && f.before.kind == kindCompleted {
		if diff := sameRedactedConfig(*m.Fingerprint, f.before.fingerprint); len(diff) > 0 {
			problems = append(problems, diff...)
		}
	}
	return problems
}

func stageClaim(label string, claimed *DiagnosisStage, disk diagFact) []string {
	if disk.kind != kindCompleted || claimed.AnalysisSHA256 != disk.stage.AnalysisSHA256 || claimed.DiagnosisOutcome != diagnosis.OutcomeCompleted {
		return []string{"pair-manifest.json " + label + " record does not match the saved diagnosis"}
	}
	return nil
}

func stageSucceeded(m diagnosis.Manifest, name, artifact string) bool {
	for _, s := range m.Stages {
		if s.Name == name {
			return s.Status == diagnosis.StatusSucceeded && s.Artifact == artifact
		}
	}
	return false
}

func investigationOutcome(m diagnosis.Manifest) string {
	for _, s := range m.Stages {
		if s.Name == diagnosis.StageInvestigation {
			return s.InvestigationOutcome
		}
	}
	return ""
}

func knownOutcome(outcome string) bool {
	switch outcome {
	case fixvalidation.OutcomeImprovement, fixvalidation.OutcomeRegression, fixvalidation.OutcomeMixed,
		fixvalidation.OutcomeNoMaterialChange, fixvalidation.OutcomeInsufficient, fixvalidation.OutcomeNotComparable:
		return true
	default:
		return false
	}
}

func decodeFile(path string, maxBytes int64, dest any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > maxBytes {
		return fmt.Errorf("%s is larger than %d bytes", filepath.Base(path), maxBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dest); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("%s: unexpected data after the JSON value", filepath.Base(path))
		}
		return err
	}
	return nil
}

func hashFile(path string, maxBytes int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, maxBytes+1))
	if err != nil {
		return "", err
	}
	if n > maxBytes {
		return "", fmt.Errorf("%s is larger than %d bytes", filepath.Base(path), maxBytes)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func cleanErr(err error) string {
	text := workload.RedactText(err.Error(), "")
	text = strings.Join(strings.Fields(text), " ")
	const max = 300
	if len(text) > max {
		return text[:max] + "..."
	}
	return text
}
