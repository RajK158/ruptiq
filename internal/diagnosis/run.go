package diagnosis

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/RajK158/ruptiq/internal/analysis"
	"github.com/RajK158/ruptiq/internal/experiment"
	"github.com/RajK158/ruptiq/internal/investigation"
	"github.com/RajK158/ruptiq/internal/telemetry"
	"github.com/RajK158/ruptiq/internal/workload"
)

// Inputs are the workload runner and telemetry reader for one diagnosis.
// The diagnosis package does not start k6 or dial a metrics endpoint itself.
type Inputs struct {
	Runner workload.Runner
	Reader telemetry.Reader

	now   func() time.Time
	write func(dir, name string, v any) error
}

// Run validates cfg, runs the experiment, and continues through analysis and
// investigation when the saved experiment is one the existing analyzer
// accepts. A later failure leaves earlier artifacts in the output directory
// and records the failure in the manifest. It does not overwrite an existing
// artifact.
func Run(ctx context.Context, cfg Config, in Inputs) (Result, error) {
	cfg = cfg.WithDefaults()
	if err := cfg.Validate(); err != nil {
		return Result{}, err
	}
	if in.Runner == nil || in.Reader == nil {
		return Result{}, &ValidationError{Problems: []string{"a workload runner and a telemetry reader are required"}}
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

	now := in.now
	if now == nil {
		now = time.Now
	}
	write := in.write
	if write == nil {
		write = writeJSONFile
	}
	target := cfg.Experiment.Workload.TargetURL
	metrics := cfg.Experiment.MetricsURL

	res := Result{Dir: dir}
	res.Manifest = Manifest{
		SchemaVersion: SchemaVersion,
		Versions:      currentVersions(),
		StartedAt:     now().UTC(),
		TargetURL:     workload.RedactURL(target),
		MetricsURL:    workload.RedactURL(metrics),
		Stages: []Stage{
			{Name: StageExperiment},
			{Name: StageAnalysis},
			{Name: StageInvestigation},
		},
	}

	report, runErr := (&experiment.Coordinator{Runner: in.Runner, Reader: in.Reader}).Run(ctx, cfg.Experiment)
	if runErr != nil {
		msg := clean(runErr.Error(), target, metrics)
		res.Manifest.Stages[0] = Stage{Name: StageExperiment, Status: StatusFailed, Error: msg}
		skip(&res, 1, "the experiment did not produce a report")
		skip(&res, 2, "the experiment did not produce a report")
		return finish(&res, dir, write, now, target, metrics, stageErr(StageExperiment, msg))
	}

	res.Experiment = &report
	if err := write(dir, FileExperiment, report); err != nil {
		msg := clean(err.Error(), target, metrics)
		res.Manifest.Stages[0] = Stage{Name: StageExperiment, Status: StatusFailed, ExperimentStatus: string(report.Status), Error: msg}
		skip(&res, 1, "the experiment report could not be saved")
		skip(&res, 2, "the experiment report could not be saved")
		return finish(&res, dir, write, now, target, metrics, stageErr(StageExperiment, msg))
	}
	res.Manifest.Stages[0] = Stage{
		Name:             StageExperiment,
		Status:           StatusSucceeded,
		Artifact:         FileExperiment,
		ExperimentStatus: string(report.Status),
	}
	res.Manifest.Artifacts = append(res.Manifest.Artifacts, FileExperiment)

	if !analyzable(report.Status) {
		reason := notAnalyzableReason(report.Status)
		skip(&res, 1, reason)
		skip(&res, 2, "analysis was not run")
		return finish(&res, dir, write, now, target, metrics, stageErr(StageAnalysis, reason))
	}
	if err := ctx.Err(); err != nil {
		reason := "cancelled before analysis; the experiment report was kept"
		skip(&res, 1, reason)
		skip(&res, 2, "analysis was not run")
		return finish(&res, dir, write, now, target, metrics, stageErr(StageAnalysis, reason))
	}

	expData, err := readLimited(dir, FileExperiment)
	if err != nil {
		return failStage(&res, 1, dir, write, now, target, metrics, clean(err.Error(), target, metrics))
	}
	decoded, err := analysis.DecodeExperiment(expData)
	if err != nil {
		if errors.Is(err, analysis.ErrNotAnalyzable) {
			reason := clean(err.Error(), target, metrics)
			skip(&res, 1, reason)
			skip(&res, 2, "analysis was not run")
			return finish(&res, dir, write, now, target, metrics, stageErr(StageAnalysis, reason))
		}
		return failStage(&res, 1, dir, write, now, target, metrics, clean(err.Error(), target, metrics))
	}

	a, err := analysis.Analyze(decoded, cfg.Analysis)
	if err != nil {
		return failStage(&res, 1, dir, write, now, target, metrics, clean(err.Error(), target, metrics))
	}
	aReport := analysis.Report{SchemaVersion: analysis.SchemaVersion, Input: FileExperiment, Analysis: &a}
	if err := write(dir, FileAnalysis, aReport); err != nil {
		return failStage(&res, 1, dir, write, now, target, metrics, clean(err.Error(), target, metrics))
	}
	res.Analysis = &a
	res.Manifest.Stages[1] = Stage{Name: StageAnalysis, Status: StatusSucceeded, Artifact: FileAnalysis}
	res.Manifest.Artifacts = append(res.Manifest.Artifacts, FileAnalysis)

	if err := ctx.Err(); err != nil {
		reason := "cancelled before investigation; the experiment and analysis reports were kept"
		skip(&res, 2, reason)
		return finish(&res, dir, write, now, target, metrics, stageErr(StageInvestigation, reason))
	}

	anData, err := readLimited(dir, FileAnalysis)
	if err != nil {
		return failStage(&res, 2, dir, write, now, target, metrics, clean(err.Error(), target, metrics))
	}
	saved, err := investigation.DecodeAnalysis(anData)
	if err != nil {
		return failStage(&res, 2, dir, write, now, target, metrics, clean(err.Error(), target, metrics))
	}
	inv, err := investigation.Investigate(saved, cfg.Investigation)
	if err != nil {
		return failStage(&res, 2, dir, write, now, target, metrics, clean(err.Error(), target, metrics))
	}
	iReport := investigation.Report{SchemaVersion: investigation.SchemaVersion, Input: FileAnalysis, Investigation: &inv}
	if err := write(dir, FileInvestigation, iReport); err != nil {
		return failStage(&res, 2, dir, write, now, target, metrics, clean(err.Error(), target, metrics))
	}
	res.Investigation = &inv
	res.Manifest.Stages[2] = Stage{
		Name:                 StageInvestigation,
		Status:               StatusSucceeded,
		Artifact:             FileInvestigation,
		InvestigationOutcome: inv.Outcome,
	}
	res.Manifest.Artifacts = append(res.Manifest.Artifacts, FileInvestigation)
	return finish(&res, dir, write, now, target, metrics, nil)
}

func skip(res *Result, index int, reason string) {
	name := res.Manifest.Stages[index].Name
	res.Manifest.Stages[index] = Stage{Name: name, Status: StatusSkipped, Reason: reason}
}

func failStage(res *Result, index int, dir string, write func(string, string, any) error, now func() time.Time, target, metrics, message string) (Result, error) {
	name := res.Manifest.Stages[index].Name
	res.Manifest.Stages[index] = Stage{Name: name, Status: StatusFailed, Error: message}
	for i := index + 1; i < len(res.Manifest.Stages); i++ {
		if res.Manifest.Stages[i].Status == "" {
			skip(res, i, name+" failed")
		}
	}
	return finish(res, dir, write, now, target, metrics, stageErr(name, message))
}

func finish(res *Result, dir string, write func(string, string, any) error, now func() time.Time, target, metrics string, stage error) (Result, error) {
	res.Manifest.FinishedAt = now().UTC()
	res.Manifest.Limitations = baseLimitations()
	if res.Experiment != nil && res.Experiment.Status != experiment.StatusPassed {
		res.Manifest.Limitations = append(res.Manifest.Limitations,
			clean(fmt.Sprintf("The experiment status is %s.", res.Experiment.Status), target, metrics))
	}
	if stage == nil {
		res.Manifest.Outcome = OutcomeCompleted
	} else {
		res.Manifest.Outcome = OutcomeFailed
	}
	res.Manifest.Artifacts = append(res.Manifest.Artifacts, FileManifest)
	if err := write(dir, FileManifest, res.Manifest); err != nil {
		res.Manifest.Artifacts = res.Manifest.Artifacts[:len(res.Manifest.Artifacts)-1]
		res.Manifest.Outcome = OutcomeFailed
		msg := clean(err.Error(), target, metrics)
		if stage != nil {
			return *res, stageErr(stageName(stage), stageMessage(stage)+"; manifest could not be saved: "+msg)
		}
		return *res, stageErr("manifest", msg)
	}
	return *res, stage
}

func stageName(err error) string {
	var se *StageError
	if errors.As(err, &se) {
		return se.Stage
	}
	return "diagnosis"
}

func stageMessage(err error) string {
	var se *StageError
	if errors.As(err, &se) {
		return se.Message
	}
	return err.Error()
}

func readLimited(dir, name string) ([]byte, error) {
	path, err := artifactPath(dir, name)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, analysis.MaxInputBytesLimit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > analysis.MaxInputBytesLimit {
		return nil, fmt.Errorf("%s is larger than %d bytes", name, analysis.MaxInputBytesLimit)
	}
	return data, nil
}
