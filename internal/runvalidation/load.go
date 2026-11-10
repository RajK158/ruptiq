package runvalidation

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/RajK158/ruptiq/internal/analysis"
	"github.com/RajK158/ruptiq/internal/diagnosis"
	"github.com/RajK158/ruptiq/internal/experiment"
	"github.com/RajK158/ruptiq/internal/investigation"
	"github.com/RajK158/ruptiq/internal/workload"
)

// diagnoseReservation is the file ruptiq-diagnose creates while it owns a
// directory. This package never deletes it.
const diagnoseReservation = ".ruptiq-diagnose-reservation"

// loaded is one diagnosis directory that passed the authenticity checks.
type loaded struct {
	input    Input
	analysis analysis.Analysis
}

func loadDiagnosis(dir, label string, maxBytes int64) (loaded, error) {
	abs, err := absClean(dir)
	if err != nil {
		return loaded{}, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return loaded{}, &InputError{Problems: []string{cleanErr(err)}}
	}
	if !info.IsDir() {
		return loaded{}, &InputError{Problems: []string{"path is not a directory"}}
	}
	if _, err := os.Lstat(filepath.Join(abs, diagnoseReservation)); err == nil {
		return loaded{}, &ValidationError{Problems: []string{
			"directory has an active " + diagnoseReservation + "; it was not removed",
		}}
	} else if !os.IsNotExist(err) {
		return loaded{}, err
	}

	manifest, err := readManifest(abs, maxBytes)
	if err != nil {
		return loaded{}, err
	}
	if err := checkManifest(manifest); err != nil {
		return loaded{}, err
	}
	expBytes, err := readFile(abs, diagnosis.FileExperiment, maxBytes)
	if err != nil {
		return loaded{}, err
	}
	exp, err := analysis.DecodeExperiment(expBytes)
	if err != nil {
		return loaded{}, &InputError{Problems: []string{"experiment.json: " + cleanErr(err)}}
	}
	expStage, _ := stageByName(manifest, diagnosis.StageExperiment)
	if expStage.ExperimentStatus != string(exp.Status) {
		return loaded{}, &InputError{Problems: []string{
			"manifest experiment status " + expStage.ExperimentStatus + " does not match experiment.json",
		}}
	}

	anBytes, err := readFile(abs, diagnosis.FileAnalysis, maxBytes)
	if err != nil {
		return loaded{}, err
	}
	report, err := decodeAnalysisReport(anBytes)
	if err != nil {
		return loaded{}, err
	}
	if report.Input != diagnosis.FileExperiment {
		return loaded{}, &InputError{Problems: []string{
			"analysis.json input is " + report.Input + ", want " + diagnosis.FileExperiment,
		}}
	}
	decoded, err := investigation.DecodeAnalysis(anBytes)
	if err != nil {
		return loaded{}, &InputError{Problems: []string{"analysis.json: " + cleanErr(err)}}
	}
	fresh, err := analysis.Analyze(exp, decoded.Config)
	if err != nil {
		return loaded{}, &InputError{Problems: []string{"analysis.json: " + cleanErr(err)}}
	}
	if err := sameAnalysis(decoded, fresh); err != nil {
		return loaded{}, err
	}

	invOutcome, err := loadInvestigation(abs, manifest, maxBytes)
	if err != nil {
		return loaded{}, err
	}
	sum := sha256.Sum256(anBytes)
	return loaded{
		input: Input{
			Label:                 label,
			DiagnosisOutcome:      manifest.Outcome,
			ExperimentStatus:      string(exp.Status),
			TelemetryCompleteness: string(exp.Telemetry.Completeness),
			AnalysisFile:          label + "/" + diagnosis.FileAnalysis,
			AnalysisSHA256:        hex.EncodeToString(sum[:]),
			InvestigationOutcome:  invOutcome,
		},
		analysis: decoded,
	}, nil
}

func readManifest(dir string, maxBytes int64) (diagnosis.Manifest, error) {
	data, err := readFile(dir, diagnosis.FileManifest, maxBytes)
	if err != nil {
		return diagnosis.Manifest{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m diagnosis.Manifest
	if err := dec.Decode(&m); err != nil {
		return diagnosis.Manifest{}, &InputError{Problems: []string{"manifest.json: " + cleanErr(err)}}
	}
	if _, err := dec.Token(); err != io.EOF {
		return diagnosis.Manifest{}, &InputError{Problems: []string{"manifest.json: unexpected data after the manifest"}}
	}
	return m, nil
}

func checkManifest(m diagnosis.Manifest) error {
	var problems []string
	fail := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	if m.SchemaVersion != diagnosis.SchemaVersion {
		fail("manifest schema_version %d is not supported (%d)", m.SchemaVersion, diagnosis.SchemaVersion)
	}
	if m.Versions.Diagnosis != diagnosis.SchemaVersion || m.Versions.Experiment != experiment.ReportSchemaVersion ||
		m.Versions.Analysis != analysis.SchemaVersion || m.Versions.Investigation != investigation.SchemaVersion {
		fail("manifest schema versions do not match the supported diagnosis, experiment, analysis, and investigation schemas")
	}
	if m.Outcome != diagnosis.OutcomeCompleted && m.Outcome != diagnosis.OutcomeFailed {
		fail("manifest outcome %q is not a diagnosis outcome", m.Outcome)
	}
	if !listedOnce(m, diagnosis.FileManifest) {
		fail("manifest artifacts must list %s once", diagnosis.FileManifest)
	}
	expStage, err := stageByName(m, diagnosis.StageExperiment)
	if err != nil {
		fail("%s", err.Error())
	} else if expStage.Status != diagnosis.StatusSucceeded || expStage.Artifact != diagnosis.FileExperiment || !listedOnce(m, diagnosis.FileExperiment) {
		fail("experiment stage must have succeeded and reference %s", diagnosis.FileExperiment)
	}
	anStage, err := stageByName(m, diagnosis.StageAnalysis)
	if err != nil {
		fail("%s", err.Error())
	} else if anStage.Status != diagnosis.StatusSucceeded || anStage.Artifact != diagnosis.FileAnalysis || !listedOnce(m, diagnosis.FileAnalysis) {
		fail("analysis stage must have succeeded and reference %s", diagnosis.FileAnalysis)
	}
	invStage, err := stageByName(m, diagnosis.StageInvestigation)
	if err != nil {
		fail("%s", err.Error())
	} else if invStage.Status == diagnosis.StatusSucceeded {
		if invStage.Artifact != diagnosis.FileInvestigation || !listedOnce(m, diagnosis.FileInvestigation) {
			fail("investigation stage references %s, want %s", invStage.Artifact, diagnosis.FileInvestigation)
		}
		if m.Outcome != diagnosis.OutcomeCompleted {
			fail("manifest outcome is %s but every stage succeeded", m.Outcome)
		}
	} else if invStage.Status != diagnosis.StatusFailed && invStage.Status != diagnosis.StatusSkipped {
		fail("investigation stage status %q is not a known status", invStage.Status)
	} else if m.Outcome != diagnosis.OutcomeFailed {
		fail("manifest outcome is %s but investigation is %s", m.Outcome, invStage.Status)
	}
	for _, name := range m.Artifacts {
		if name == "" || name != filepath.Base(name) || strings.Contains(name, "..") {
			fail("artifact %q is not a file name in the diagnosis directory", name)
		}
	}
	if len(problems) > 0 {
		return &InputError{Problems: problems}
	}
	return nil
}

func loadInvestigation(dir string, m diagnosis.Manifest, maxBytes int64) (string, error) {
	stage, err := stageByName(m, diagnosis.StageInvestigation)
	if err != nil {
		return "", &InputError{Problems: []string{err.Error()}}
	}
	if stage.Status != diagnosis.StatusSucceeded {
		return "", nil
	}
	data, err := readFile(dir, diagnosis.FileInvestigation, maxBytes)
	if err != nil {
		return "", err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var rep investigation.Report
	if err := dec.Decode(&rep); err != nil {
		return "", &InputError{Problems: []string{"investigation.json: " + cleanErr(err)}}
	}
	if _, err := dec.Token(); err != io.EOF {
		return "", &InputError{Problems: []string{"investigation.json: unexpected data after the report"}}
	}
	switch {
	case rep.SchemaVersion != investigation.SchemaVersion:
		return "", &InputError{Problems: []string{fmt.Sprintf("investigation.json schema_version %d is not supported", rep.SchemaVersion)}}
	case rep.Input != diagnosis.FileAnalysis:
		return "", &InputError{Problems: []string{"investigation.json input does not reference " + diagnosis.FileAnalysis}}
	case rep.Investigation == nil:
		return "", &InputError{Problems: []string{"investigation.json has no investigation"}}
	case rep.Investigation.Outcome != investigation.OutcomeChangesObserved &&
		rep.Investigation.Outcome != investigation.OutcomeNoMaterialChange &&
		rep.Investigation.Outcome != investigation.OutcomeInsufficientEvidence:
		return "", &InputError{Problems: []string{"investigation.json outcome " + rep.Investigation.Outcome + " is not a known outcome"}}
	case stage.InvestigationOutcome != "" && stage.InvestigationOutcome != rep.Investigation.Outcome:
		return "", &InputError{Problems: []string{"manifest investigation outcome does not match investigation.json"}}
	}
	return rep.Investigation.Outcome, nil
}

func sameAnalysis(saved, fresh analysis.Analysis) error {
	got, err := json.Marshal(saved)
	if err != nil {
		return err
	}
	want, err := json.Marshal(fresh)
	if err != nil {
		return err
	}
	if bytes.Equal(got, want) {
		return nil
	}
	return &InputError{Problems: []string{
		"analysis.json does not match the saved experiment; it was not repaired",
	}}
}

func decodeAnalysisReport(data []byte) (analysis.Report, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var rep analysis.Report
	if err := dec.Decode(&rep); err != nil {
		return analysis.Report{}, &InputError{Problems: []string{"analysis.json: " + cleanErr(err)}}
	}
	if _, err := dec.Token(); err != io.EOF {
		return analysis.Report{}, &InputError{Problems: []string{"analysis.json: unexpected data after the report"}}
	}
	if rep.SchemaVersion != analysis.SchemaVersion {
		return analysis.Report{}, &InputError{Problems: []string{fmt.Sprintf(
			"analysis.json schema_version %d is not supported", rep.SchemaVersion)}}
	}
	return rep, nil
}

func stageByName(m diagnosis.Manifest, name string) (diagnosis.Stage, error) {
	var found *diagnosis.Stage
	for i := range m.Stages {
		if m.Stages[i].Name != name {
			continue
		}
		if found != nil {
			return diagnosis.Stage{}, fmt.Errorf("stage %s is repeated", name)
		}
		s := m.Stages[i]
		found = &s
	}
	if found == nil {
		return diagnosis.Stage{}, fmt.Errorf("stage %s is missing", name)
	}
	return *found, nil
}

func listedOnce(m diagnosis.Manifest, name string) bool {
	n := 0
	for _, a := range m.Artifacts {
		if a == name {
			n++
		}
	}
	return n == 1
}

func readFile(dir, name string, maxBytes int64) ([]byte, error) {
	path, err := artifactPath(dir, name)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, &InputError{Problems: []string{name + " is missing"}}
		}
		return nil, &InputError{Problems: []string{name + ": " + cleanErr(err)}}
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, &InputError{Problems: []string{name + ": " + cleanErr(err)}}
	}
	if int64(len(data)) > maxBytes {
		return nil, &InputError{Problems: []string{fmt.Sprintf("%s is larger than %d bytes", name, maxBytes)}}
	}
	return data, nil
}

func cleanErr(err error) string {
	if err == nil {
		return ""
	}
	text := workload.RedactText(err.Error(), "")
	text = strings.Join(strings.Fields(text), " ")
	const max = 300
	if len(text) > max {
		text = text[:max] + "..."
	}
	return text
}
