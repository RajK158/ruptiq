package pairworkflow

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/RajK158/ruptiq/internal/diagnosis"
	"github.com/RajK158/ruptiq/internal/runvalidation"
	"github.com/RajK158/ruptiq/internal/telemetry"
	"github.com/RajK158/ruptiq/internal/workload"
)

// Inputs are the workload runner and telemetry reader for a before or after
// stage. Compare does not use them. The pair workflow does not start k6 or
// dial a metrics endpoint itself.
type Inputs struct {
	Runner workload.Runner
	Reader telemetry.Reader

	now   func() time.Time
	write func(dir, name string, v any) error
}

// Run performs one stage and returns. It does not start another stage.
// not_comparable and insufficient_evidence from compare are successful
// results. A returned error means this invocation did not complete its stage.
func Run(ctx context.Context, cfg Config, in Inputs) (Result, error) {
	cfg = cfg.WithDefaults()
	if err := cfg.Validate(); err != nil {
		return Result{}, err
	}
	if cfg.Stage != StageCompare && (in.Runner == nil || in.Reader == nil) {
		return Result{}, &ValidationError{Problems: []string{"a workload runner and a telemetry reader are required"}}
	}
	root, err := preparePairDir(cfg.PairDir)
	if err != nil {
		return Result{}, err
	}
	release, err := reservePairDir(root)
	if err != nil {
		return Result{}, err
	}
	defer release()
	if err := checkLayout(root); err != nil {
		return Result{}, err
	}
	found, err := inspect(root, cfg.MaxInputBytes)
	if err != nil {
		return Result{}, err
	}
	s := &session{ctx: ctx, cfg: cfg, in: in, root: root, found: found}
	s.now = in.now
	if s.now == nil {
		s.now = time.Now
	}
	s.write = in.write
	if s.write == nil {
		s.write = writeJSONFile
	}
	switch cfg.Stage {
	case StageBefore:
		return s.runBefore()
	case StageAfter:
		return s.runAfter()
	default:
		return s.runCompare()
	}
}

type session struct {
	ctx   context.Context
	cfg   Config
	in    Inputs
	root  string
	found facts
	now   func() time.Time
	write func(dir, name string, v any) error
}

func (s *session) runBefore() (Result, error) {
	if err := s.ctx.Err(); err != nil {
		return Result{}, err
	}
	switch s.found.before.kind {
	case kindCompleted:
		return Result{}, &ValidationError{Problems: []string{
			"before already contains a completed diagnosis and was not overwritten. Apply your change outside Ruptiq, then run -stage after",
		}}
	case kindAbsent:
		return s.capture(DirBefore, StageBefore)
	default:
		return s.blocked(StageBefore, s.found.before.kind, s.found.before.detail)
	}
}

func (s *session) runAfter() (Result, error) {
	if err := s.ctx.Err(); err != nil {
		return Result{}, err
	}
	if s.found.before.kind != kindCompleted {
		return Result{}, &ValidationError{Problems: []string{
			"after stage requires a completed baseline in before/. Run -stage before first. Ruptiq did not start a workload",
		}}
	}
	switch s.found.after.kind {
	case kindCompleted:
		return Result{}, &ValidationError{Problems: []string{
			"after already contains a completed diagnosis and was not overwritten. Run -stage compare. Ruptiq does not apply a change",
		}}
	case kindAbsent:
		saved := s.baselineFingerprint()
		requested := fingerprintFromConfig(s.cfg.Diagnosis)
		if problems := mismatch(saved, requested); len(problems) > 0 {
			return Result{}, &ValidationError{Problems: []string{
				"after configuration does not match the baseline, so no workload was started: " + joined(problems),
			}}
		}
		return s.capture(DirAfter, StageAfter)
	default:
		return s.blocked(StageAfter, s.found.after.kind, s.found.after.detail)
	}
}

func (s *session) runCompare() (Result, error) {
	if err := s.ctx.Err(); err != nil {
		return Result{}, err
	}
	if s.found.before.kind != kindCompleted {
		return Result{}, &ValidationError{Problems: []string{
			"compare stage requires a completed baseline in before/. No comparison was written",
		}}
	}
	if s.found.after.kind != kindCompleted {
		return Result{}, &ValidationError{Problems: []string{
			"compare stage requires a completed candidate in after/. Run -stage after after you change the service outside Ruptiq. No comparison was written",
		}}
	}
	switch s.found.comparison.kind {
	case kindCompleted:
		if s.found.manifest != nil && s.found.manifest.State == StateComparisonCompleted && s.found.manifest.Comparison != nil {
			return Result{}, &ValidationError{Problems: []string{
				"comparison already contains a completed comparison (" + s.found.comparison.stage.Outcome + ") and was not overwritten",
			}}
		}
		return s.adoptComparison()
	case kindAbsent:
		return s.compare()
	default:
		return s.blocked(StageCompare, s.found.comparison.kind, s.found.comparison.detail)
	}
}

func (s *session) capture(dirName, stage string) (Result, error) {
	cfg := s.cfg.Diagnosis
	cfg.OutputDir = filepath.Join(s.root, dirName)
	res, err := diagnosis.Run(s.ctx, cfg, diagnosis.Inputs{Runner: s.in.Runner, Reader: s.in.Reader})
	if err != nil || res.Manifest.Outcome != diagnosis.OutcomeCompleted {
		var verr *diagnosis.ValidationError
		if errors.As(err, &verr) {
			return Result{}, &ValidationError{Problems: verr.Problems}
		}
		msg := dirName + " did not complete"
		if err != nil {
			msg = err.Error()
		}
		return s.fail(stage, s.cancelled(err), msg)
	}
	sum, hashErr := hashFile(filepath.Join(s.root, dirName, diagnosis.FileAnalysis), s.cfg.MaxInputBytes)
	if hashErr != nil {
		return s.fail(stage, false, hashErr.Error())
	}
	record := DiagnosisStage{
		Directory:             dirName,
		DiagnosisOutcome:      res.Manifest.Outcome,
		ExperimentStatus:      string(res.Experiment.Status),
		TelemetryCompleteness: string(res.Experiment.Telemetry.Completeness),
		AnalysisSHA256:        sum,
	}
	if res.Investigation != nil {
		record.InvestigationOutcome = res.Investigation.Outcome
	}
	m := s.kept()
	m.Failure = nil
	if stage == StageBefore {
		m.State = StateBaselineCaptured
		m.Before = &record
		m.After = nil
		m.Comparison = nil
		fp := fingerprintFromConfig(s.cfg.Diagnosis)
		m.Fingerprint = &fp
	} else {
		m.State = StateCandidateCaptured
		m.After = &record
		m.Comparison = nil
		if m.Fingerprint == nil {
			fp := s.found.before.fingerprint
			m.Fingerprint = &fp
		}
	}
	if err := s.save(&m, stageNotes(record)...); err != nil {
		return Result{}, err
	}
	return Result{Dir: s.root, Manifest: m}, nil
}

// adoptComparison records a comparison that is already on disk. It does not
// call runvalidation again and does not replace validation.json.
func (s *session) adoptComparison() (Result, error) {
	m := s.kept()
	m.State = StateComparisonCompleted
	m.Failure = nil
	m.Before = &s.found.before.stage
	m.After = &s.found.after.stage
	stage := s.found.comparison.stage
	m.Comparison = &stage
	if m.Fingerprint == nil {
		fp := s.found.before.fingerprint
		m.Fingerprint = &fp
	}
	extra := []string{
		"The comparison files were already complete. They were kept, and the comparison was not run again.",
		"The outcome is the fix-validation result already stored for these two saved runs. It is not a claim that a fix worked.",
	}
	if stage.BeforeAnalysisSHA256 != "" && stage.BeforeAnalysisSHA256 == stage.AfterAnalysisSHA256 {
		extra = append(extra, "The before and after analysis files have the same SHA-256. They are copies of one saved analysis, not two independent experiments, and this comparison is not evidence of a change.")
	}
	if err := s.save(&m, extra...); err != nil {
		return Result{}, err
	}
	return Result{Dir: s.root, Manifest: m}, nil
}

func (s *session) compare() (Result, error) {
	compared, err := runvalidation.Run(runvalidation.Config{
		BeforeDir:     filepath.Join(s.root, DirBefore),
		AfterDir:      filepath.Join(s.root, DirAfter),
		OutputDir:     filepath.Join(s.root, DirComparison),
		MaxInputBytes: s.cfg.MaxInputBytes,
		Validation:    s.cfg.Validation,
	}, runvalidation.Inputs{})
	if err != nil {
		var verr *runvalidation.ValidationError
		if errors.As(err, &verr) {
			return Result{}, &ValidationError{Problems: verr.Problems}
		}
		return s.fail(StageCompare, s.cancelled(err), err.Error())
	}
	m := s.kept()
	m.State = StateComparisonCompleted
	m.Failure = nil
	m.Before = &s.found.before.stage
	m.After = &s.found.after.stage
	if m.Fingerprint == nil {
		fp := s.found.before.fingerprint
		m.Fingerprint = &fp
	}
	warnings := append([]string{}, compared.Manifest.Warnings...)
	m.Comparison = &ComparisonStage{
		Directory:            DirComparison,
		Outcome:              compared.Manifest.Outcome,
		Comparable:           compared.Manifest.Comparable,
		BeforeAnalysisSHA256: compared.Manifest.Before.AnalysisSHA256,
		AfterAnalysisSHA256:  compared.Manifest.After.AnalysisSHA256,
		Warnings:             warnings,
	}
	extra := []string{
		"The outcome is the fix-validation result for these two saved runs. It is not a claim that a fix worked.",
	}
	if compared.Manifest.Before.AnalysisSHA256 != "" && compared.Manifest.Before.AnalysisSHA256 == compared.Manifest.After.AnalysisSHA256 {
		extra = append(extra, "The before and after analysis files have the same SHA-256. They are copies of one saved analysis, not two independent experiments, and this comparison is not evidence of a change.")
	}
	if err := s.save(&m, extra...); err != nil {
		return Result{Dir: s.root, Manifest: m, Comparison: &compared}, err
	}
	return Result{Dir: s.root, Manifest: m, Comparison: &compared}, nil
}

func (s *session) blocked(stage, kind, detail string) (Result, error) {
	recovery := recoveryFor(stage)
	if kind == kindReserved && stage != StageCompare {
		recovery = "If no diagnosis is running, remove " + stage + "/" + diagnoseReservation + " manually. It was not deleted. Then run -stage " + stage + " again."
	}
	if kind == kindReserved && stage == StageCompare {
		recovery = "If no comparison is running, remove comparison/" + compareReservation + " manually. It was not deleted. Then run -stage compare again."
	}
	msg := detail
	if msg == "" {
		msg = stage + " is " + kind + " and was not overwritten"
	}
	msg = msg + ". " + recovery
	m := s.kept()
	m.State = StateInterrupted
	if kind == kindFailed {
		m.State = StateFailed
	}
	m.Failure = &Failure{Stage: stage, Message: s.clean(msg), Recovery: recovery}
	if err := s.save(&m); err != nil {
		return Result{}, err
	}
	return Result{}, &ValidationError{Problems: []string{s.clean(msg)}}
}

func (s *session) fail(stage string, cancel bool, msg string) (Result, error) {
	m := s.kept()
	m.State = StateFailed
	if cancel {
		m.State = StateInterrupted
	}
	m.Comparison = nil
	recovery := recoveryFor(stage)
	m.Failure = &Failure{Stage: stage, Message: s.clean(msg), Recovery: recovery}
	if err := s.save(&m); err != nil {
		return Result{}, err
	}
	return Result{Dir: s.root, Manifest: m}, fmt.Errorf("%s", m.Failure.Message)
}

// kept returns a manifest that still records stages already completed on
// disk. It drops records the files do not support.
func (s *session) kept() Manifest {
	var m Manifest
	if s.found.manifest != nil {
		m = *s.found.manifest
	}
	m.Failure = nil
	if s.found.before.kind == kindCompleted {
		stage := s.found.before.stage
		m.Before = &stage
		if m.Fingerprint == nil {
			fp := s.found.before.fingerprint
			m.Fingerprint = &fp
		}
	} else {
		m.Before = nil
	}
	if s.found.after.kind == kindCompleted {
		stage := s.found.after.stage
		m.After = &stage
	} else {
		m.After = nil
	}
	if s.found.comparison.kind == kindCompleted {
		stage := s.found.comparison.stage
		m.Comparison = &stage
	} else {
		m.Comparison = nil
	}
	return m
}

func (s *session) baselineFingerprint() Fingerprint {
	if s.found.manifest != nil && s.found.manifest.Fingerprint != nil {
		return *s.found.manifest.Fingerprint
	}
	return s.found.before.fingerprint
}

func (s *session) save(m *Manifest, extra ...string) error {
	m.SchemaVersion = SchemaVersion
	m.Versions = currentVersions()
	m.UpdatedAt = s.now().UTC()
	m.Limitations = baseLimitations()
	if m.Fingerprint != nil && (m.Fingerprint.TargetSHA256 == "" || m.Fingerprint.MetricsSHA256 == "") {
		extra = append(extra, "The baseline URL fingerprint has no raw-URL hash. This is not complete configuration verification, and a SHA-256 was not computed from the redacted URL.")
	}
	m.Limitations = append(m.Limitations, extra...)
	return s.write(s.root, FileManifest, *m)
}

func (s *session) clean(text string) string {
	target := s.cfg.Diagnosis.Experiment.Workload.TargetURL
	metrics := s.cfg.Diagnosis.Experiment.MetricsURL
	text = workload.RedactText(text, target)
	text = workload.RedactText(text, metrics)
	text = strings.Join(strings.Fields(text), " ")
	const max = 300
	if len(text) > max {
		text = text[:max] + "..."
	}
	return text
}

func (s *session) cancelled(err error) bool {
	return errors.Is(s.ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled)
}

func recoveryFor(stage string) string {
	switch stage {
	case StageBefore:
		return "Remove the before directory inside the pair directory and run -stage before again. Ruptiq did not delete that directory."
	case StageAfter:
		return "Remove the after directory inside the pair directory and run -stage after again. The before directory was kept. Ruptiq did not delete the after directory."
	default:
		return "Remove the comparison directory inside the pair directory and run -stage compare again. The before and after diagnoses were kept. Ruptiq did not delete the comparison directory."
	}
}

func stageNotes(stage DiagnosisStage) []string {
	var notes []string
	if stage.ExperimentStatus != "" && stage.ExperimentStatus != "passed" {
		notes = append(notes, "The "+stage.Directory+" experiment status is "+stage.ExperimentStatus+". Saving the diagnosis does not mean the workload passed.")
	}
	if stage.TelemetryCompleteness != "" && stage.TelemetryCompleteness != "complete" {
		notes = append(notes, "The "+stage.Directory+" telemetry completeness is "+stage.TelemetryCompleteness+". Saving the diagnosis does not fill in missing measurements.")
	}
	if stage.InvestigationOutcome == "insufficient_evidence" {
		notes = append(notes, "The "+stage.Directory+" investigation abstained (insufficient_evidence). That is not a performance finding.")
	}
	return notes
}
