package pairworkflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RajK158/ruptiq/internal/analysis"
	"github.com/RajK158/ruptiq/internal/diagnosis"
	"github.com/RajK158/ruptiq/internal/experiment"
	"github.com/RajK158/ruptiq/internal/fixvalidation"
	"github.com/RajK158/ruptiq/internal/investigation"
	"github.com/RajK158/ruptiq/internal/runvalidation"
	"github.com/RajK158/ruptiq/internal/telemetry"
	"github.com/RajK158/ruptiq/internal/workload"
)

func TestWorkflowBeforeAfterCompare(t *testing.T) {
	dir := t.TempDir()
	runner := &callRunner{fn: func(context.Context, workload.Spec) (workload.Result, error) {
		return workload.Result{}, nil
	}}
	cfg := baseConfig(dir, StageBefore)
	res, err := Run(context.Background(), cfg, Inputs{Runner: runner, Reader: &scriptReader{}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Manifest.State != StateBaselineCaptured {
		t.Fatalf("state %s", res.Manifest.State)
	}
	var buf bytes.Buffer
	if err := WriteSummary(&buf, res); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), BaselineCapturedMessage) {
		t.Fatalf("summary:\n%s", buf.String())
	}
	if _, err := os.Stat(filepath.Join(dir, DirAfter)); !os.IsNotExist(err) {
		t.Fatal("before started the after stage")
	}
	if runner.calls() != 1 {
		t.Fatalf("runner calls %d", runner.calls())
	}
	beforeSnap := snap(t, filepath.Join(dir, DirBefore))

	cfg.Stage = StageAfter
	res, err = Run(context.Background(), cfg, Inputs{Runner: runner, Reader: &scriptReader{}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Manifest.State != StateCandidateCaptured || runner.calls() != 2 {
		t.Fatalf("state %s calls %d", res.Manifest.State, runner.calls())
	}
	if !sameSnap(beforeSnap, snap(t, filepath.Join(dir, DirBefore))) {
		t.Fatal("after changed the baseline")
	}
	buf.Reset()
	if err := WriteSummary(&buf, res); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), CandidateCapturedMessage) {
		t.Fatalf("summary:\n%s", buf.String())
	}

	cfg.Stage = StageCompare
	res, err = Run(context.Background(), cfg, Inputs{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Manifest.State != StateComparisonCompleted || res.Comparison == nil {
		t.Fatalf("state %s", res.Manifest.State)
	}
	if !knownOutcome(res.Manifest.Comparison.Outcome) || res.Manifest.Comparison.Outcome != res.Comparison.Manifest.Outcome {
		t.Fatalf("outcome %s", res.Manifest.Comparison.Outcome)
	}
	if runner.calls() != 2 {
		t.Fatal("compare started a workload")
	}
	if _, err := os.Stat(filepath.Join(dir, reservationFile)); !os.IsNotExist(err) {
		t.Fatal("reservation left behind")
	}
	text := mustText(t, filepath.Join(dir, FileManifest))
	if strings.Contains(text, dir) {
		t.Fatal("pair manifest contains a filesystem path")
	}
}

func TestAfterRequiresBaseline(t *testing.T) {
	dir := t.TempDir()
	runner := rejectRunner(t)
	_, err := Run(context.Background(), baseConfig(dir, StageAfter), Inputs{Runner: runner, Reader: &scriptReader{}})
	expectValidation(t, err, "completed baseline")
	if runner.calls() != 0 {
		t.Fatal("workload started")
	}
}

func TestCompareRequiresCandidate(t *testing.T) {
	dir := t.TempDir()
	if _, err := Run(context.Background(), baseConfig(dir, StageBefore), Inputs{Runner: instantRunner(), Reader: &scriptReader{}}); err != nil {
		t.Fatal(err)
	}
	_, err := Run(context.Background(), baseConfig(dir, StageCompare), Inputs{})
	expectValidation(t, err, "completed candidate")
	if _, statErr := os.Stat(filepath.Join(dir, DirComparison)); !os.IsNotExist(statErr) {
		t.Fatal("comparison directory was created")
	}
}

func TestDuplicateDoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	if _, err := Run(context.Background(), baseConfig(dir, StageBefore), Inputs{Runner: instantRunner(), Reader: &scriptReader{}}); err != nil {
		t.Fatal(err)
	}
	beforeSnap := snap(t, filepath.Join(dir, DirBefore))
	manifestSnap := mustText(t, filepath.Join(dir, FileManifest))
	runner := rejectRunner(t)
	_, err := Run(context.Background(), baseConfig(dir, StageBefore), Inputs{Runner: runner, Reader: &scriptReader{}})
	expectValidation(t, err, "not overwritten")
	if runner.calls() != 0 || !sameSnap(beforeSnap, snap(t, filepath.Join(dir, DirBefore))) {
		t.Fatal("before was overwritten")
	}
	if mustText(t, filepath.Join(dir, FileManifest)) != manifestSnap {
		t.Fatal("pair manifest changed on a duplicate before")
	}

	if _, err := Run(context.Background(), baseConfig(dir, StageAfter), Inputs{Runner: instantRunner(), Reader: &scriptReader{}}); err != nil {
		t.Fatal(err)
	}
	afterSnap := snap(t, filepath.Join(dir, DirAfter))
	_, err = Run(context.Background(), baseConfig(dir, StageAfter), Inputs{Runner: runner, Reader: &scriptReader{}})
	expectValidation(t, err, "not overwritten")
	if !sameSnap(afterSnap, snap(t, filepath.Join(dir, DirAfter))) || !sameSnap(beforeSnap, snap(t, filepath.Join(dir, DirBefore))) {
		t.Fatal("a completed diagnosis changed")
	}

	res, err := Run(context.Background(), baseConfig(dir, StageCompare), Inputs{})
	if err != nil {
		t.Fatal(err)
	}
	compSnap := snap(t, filepath.Join(dir, DirComparison))
	_, err = Run(context.Background(), baseConfig(dir, StageCompare), Inputs{})
	expectValidation(t, err, res.Manifest.Comparison.Outcome)
	expectValidation(t, err, "not overwritten")
	if !sameSnap(compSnap, snap(t, filepath.Join(dir, DirComparison))) {
		t.Fatal("comparison was overwritten")
	}
}

func TestConfigMismatchBeforeWorkload(t *testing.T) {
	dir := t.TempDir()
	cfg := baseConfig(dir, StageBefore)
	cfg.Diagnosis.Experiment.Workload.TargetURL = "http://127.0.0.1:8090/work?api_key=s3cret-token"
	if _, err := Run(context.Background(), cfg, Inputs{Runner: instantRunner(), Reader: &scriptReader{}}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{name: "vus", mutate: func(c *Config) { c.Diagnosis.Experiment.Workload.VUs = 4 }, want: "virtual users"},
		{name: "query", mutate: func(c *Config) {
			c.Diagnosis.Experiment.Workload.TargetURL = "http://127.0.0.1:8090/work?api_key=other-token"
		}, want: "query differs"},
		{name: "metric", mutate: func(c *Config) { c.Diagnosis.Analysis.RequestsCounter = "other_requests_total" }, want: "metric families"},
		{name: "metrics url", mutate: func(c *Config) { c.Diagnosis.Experiment.MetricsURL = "http://127.0.0.1:8091/metrics" }, want: "metrics URL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next := cfg
			next.Stage = StageAfter
			tc.mutate(&next)
			runner := rejectRunner(t)
			_, err := Run(context.Background(), next, Inputs{Runner: runner, Reader: &scriptReader{}})
			expectValidation(t, err, tc.want)
			if strings.Contains(err.Error(), "s3cret-token") || strings.Contains(err.Error(), "other-token") {
				t.Fatalf("secret in error: %v", err)
			}
			if runner.calls() != 0 {
				t.Fatal("workload started")
			}
			if _, statErr := os.Stat(filepath.Join(dir, DirAfter)); !os.IsNotExist(statErr) {
				t.Fatal("after directory was created")
			}
		})
	}
}

func TestCancellationPreservesBaseline(t *testing.T) {
	dir := t.TempDir()
	if _, err := Run(context.Background(), baseConfig(dir, StageBefore), Inputs{Runner: instantRunner(), Reader: &scriptReader{}}); err != nil {
		t.Fatal(err)
	}
	beforeSnap := snap(t, filepath.Join(dir, DirBefore))
	started := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := baseConfig(dir, StageAfter)
	errCh := make(chan error, 1)
	go func() {
		_, err := Run(ctx, cfg, Inputs{
			Runner: &callRunner{fn: func(ctx context.Context, spec workload.Spec) (workload.Result, error) {
				close(started)
				<-ctx.Done()
				return workload.Result{}, ctx.Err()
			}},
			Reader: &scriptReader{},
		})
		errCh <- err
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("after stage did not start")
	}
	cancel()
	if err := <-errCh; err == nil {
		t.Fatal("cancellation returned success")
	}
	m := readManifest(t, dir)
	if m.State != StateInterrupted || m.Failure == nil || m.Failure.Stage != StageAfter {
		t.Fatalf("manifest %+v", m)
	}
	if m.Before == nil || m.State == StateCandidateCaptured || m.State == StateComparisonCompleted {
		t.Fatalf("baseline was not kept: %+v", m)
	}
	if !sameSnap(beforeSnap, snap(t, filepath.Join(dir, DirBefore))) {
		t.Fatal("baseline files changed")
	}
	if _, err := os.Stat(filepath.Join(dir, reservationFile)); !os.IsNotExist(err) {
		t.Fatal("reservation left behind")
	}
}

func TestWorkloadFailureIsNotSuccess(t *testing.T) {
	dir := t.TempDir()
	_, err := Run(context.Background(), baseConfig(dir, StageBefore), Inputs{
		Runner: &callRunner{fn: func(context.Context, workload.Spec) (workload.Result, error) {
			return workload.Result{}, errors.New("k6 failed")
		}},
		Reader: &scriptReader{},
	})
	if err == nil {
		t.Fatal("expected failure")
	}
	m := readManifest(t, dir)
	if m.State != StateFailed || m.Before != nil || m.State == StateBaselineCaptured {
		t.Fatalf("state %s before %#v", m.State, m.Before)
	}
	if _, statErr := os.Stat(filepath.Join(dir, reservationFile)); !os.IsNotExist(statErr) {
		t.Fatal("reservation left behind")
	}
}

func TestTelemetryFailureDoesNotInventImprovement(t *testing.T) {
	dir := t.TempDir()
	secret := "s3cret-token"
	cfg := baseConfig(dir, StageBefore)
	cfg.Diagnosis.Experiment.MetricsURL = "http://s3cret-user:" + secret + "@127.0.0.1:8090/metrics?" + secret
	res, err := Run(context.Background(), cfg, Inputs{
		Runner: instantRunner(),
		Reader: &scriptReader{err: errors.New("read " + cfg.Diagnosis.Experiment.MetricsURL + " failed")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Manifest.State != StateBaselineCaptured || res.Manifest.Comparison != nil {
		t.Fatalf("state %s", res.Manifest.State)
	}
	if res.Manifest.Before.InvestigationOutcome != investigation.OutcomeInsufficientEvidence {
		t.Fatalf("investigation %s", res.Manifest.Before.InvestigationOutcome)
	}
	if res.Manifest.Before.TelemetryCompleteness == "complete" {
		t.Fatal("telemetry was reported complete")
	}
	if !hasLimitation(res.Manifest, "telemetry") {
		t.Fatalf("limitations %v", res.Manifest.Limitations)
	}
	var buf bytes.Buffer
	if err := WriteSummary(&buf, res); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), fixvalidation.OutcomeImprovement) || strings.Contains(allFiles(t, dir), secret) || strings.Contains(allFiles(t, dir), "s3cret-user") {
		t.Fatalf("fabricated success or leaked secret\n%s", buf.String())
	}
}

func TestAbstentionIsPreserved(t *testing.T) {
	dir := t.TempDir()
	res, err := Run(context.Background(), baseConfig(dir, StageBefore), Inputs{Runner: instantRunner(), Reader: &scriptReader{}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Manifest.Before.InvestigationOutcome != investigation.OutcomeInsufficientEvidence {
		t.Fatalf("outcome %s", res.Manifest.Before.InvestigationOutcome)
	}
	if res.Manifest.State != StateBaselineCaptured {
		t.Fatalf("state %s", res.Manifest.State)
	}
	if !hasLimitation(res.Manifest, "insufficient_evidence") {
		t.Fatalf("limitations %v", res.Manifest.Limitations)
	}
}

func TestSixOutcomesPassThrough(t *testing.T) {
	slow := []float64{0, 0, 1000, 2000, 2000}
	fast := []float64{0, 0, 2000, 4000, 4000}
	cases := []struct {
		name   string
		before diagOpt
		after  diagOpt
		want   string
	}{
		{name: "improvement", before: diagOpt{counts: slow}, after: diagOpt{counts: fast, fast: true}, want: fixvalidation.OutcomeImprovement},
		{name: "regression", before: diagOpt{counts: fast, fast: true}, after: diagOpt{counts: slow}, want: fixvalidation.OutcomeRegression},
		{name: "mixed", before: diagOpt{counts: slow, fast: true}, after: diagOpt{counts: fast}, want: fixvalidation.OutcomeMixed},
		{name: "no change", before: diagOpt{counts: slow, fast: true}, after: diagOpt{counts: slow, fast: true}, want: fixvalidation.OutcomeNoMaterialChange},
		{name: "insufficient", before: diagOpt{counts: []float64{0, 0, 50, 50}}, after: diagOpt{counts: []float64{0, 10, 40, 40}}, want: fixvalidation.OutcomeInsufficient},
		{name: "not comparable", before: diagOpt{counts: slow, fast: true}, after: diagOpt{counts: fast, fast: true, vus: 8}, want: fixvalidation.OutcomeNotComparable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.before.invOutcome = investigation.OutcomeInsufficientEvidence
			tc.after.invOutcome = investigation.OutcomeInsufficientEvidence
			writeDiagnosis(t, filepath.Join(dir, DirBefore), tc.before)
			writeDiagnosis(t, filepath.Join(dir, DirAfter), tc.after)
			beforeSnap := snap(t, filepath.Join(dir, DirBefore))
			afterSnap := snap(t, filepath.Join(dir, DirAfter))
			res, err := Run(context.Background(), Config{Stage: StageCompare, PairDir: dir}, Inputs{})
			if err != nil {
				t.Fatal(err)
			}
			if res.Manifest.State != StateComparisonCompleted || res.Manifest.Comparison.Outcome != tc.want {
				t.Fatalf("state %s outcome %s", res.Manifest.State, res.Manifest.Comparison.Outcome)
			}
			if res.Comparison.Report.Validation.Outcome != tc.want {
				t.Fatalf("report outcome %s", res.Comparison.Report.Validation.Outcome)
			}
			if res.Manifest.Comparison.BeforeAnalysisSHA256 == res.Manifest.Comparison.AfterAnalysisSHA256 && !hasLimitation(res.Manifest, "same SHA-256") {
				t.Fatal("duplicate analysis was not disclosed")
			}
			if !sameSnap(beforeSnap, snap(t, filepath.Join(dir, DirBefore))) || !sameSnap(afterSnap, snap(t, filepath.Join(dir, DirAfter))) {
				t.Fatal("compare changed a diagnosis")
			}
		})
	}
}

func TestConcurrentPairStageIsRejected(t *testing.T) {
	dir := t.TempDir()
	started := make(chan struct{})
	hold := make(chan struct{})
	var once sync.Once
	runner := &callRunner{fn: func(context.Context, workload.Spec) (workload.Result, error) {
		once.Do(func() { close(started) })
		<-hold
		return healthyResult(), nil
	}}
	cfg := baseConfig(dir, StageBefore)
	errCh := make(chan error, 1)
	go func() {
		_, err := Run(context.Background(), cfg, Inputs{Runner: runner, Reader: &scriptReader{}})
		errCh <- err
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("first stage did not start")
	}
	second := rejectRunner(t)
	_, err := Run(context.Background(), cfg, Inputs{Runner: second, Reader: &scriptReader{}})
	expectValidation(t, err, reservationFile)
	if second.calls() != 0 {
		t.Fatal("second stage started a workload")
	}
	close(hold)
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, reservationFile)); !os.IsNotExist(statErr) {
		t.Fatal("reservation left behind")
	}
}

func TestStaleReservationIsNotDeleted(t *testing.T) {
	dir := t.TempDir()
	body := []byte("pid 9\n")
	if err := os.WriteFile(filepath.Join(dir, reservationFile), body, 0o600); err != nil {
		t.Fatal(err)
	}
	runner := rejectRunner(t)
	_, err := Run(context.Background(), baseConfig(dir, StageBefore), Inputs{Runner: runner, Reader: &scriptReader{}})
	expectValidation(t, err, reservationFile)
	if runner.calls() != 0 {
		t.Fatal("workload started")
	}
	got, readErr := os.ReadFile(filepath.Join(dir, reservationFile))
	if readErr != nil || string(got) != string(body) {
		t.Fatalf("reservation = %q err %v", got, readErr)
	}
}

func TestCorruptManifestIsRejected(t *testing.T) {
	t.Run("truncated", func(t *testing.T) {
		dir := t.TempDir()
		writeBytes(t, filepath.Join(dir, FileManifest), []byte("{"))
		runner := rejectRunner(t)
		_, err := Run(context.Background(), baseConfig(dir, StageBefore), Inputs{Runner: runner, Reader: &scriptReader{}})
		var ierr *InputError
		if !errors.As(err, &ierr) || runner.calls() != 0 {
			t.Fatalf("err %v calls %d", err, runner.calls())
		}
		if mustText(t, filepath.Join(dir, FileManifest)) != "{" {
			t.Fatal("corrupt manifest was replaced")
		}
	})
	t.Run("wrong schema", func(t *testing.T) {
		dir := t.TempDir()
		writeBytes(t, filepath.Join(dir, FileManifest), []byte("{\"schema_version\":9,\"state\":\"baseline_captured\"}\n"))
		_, err := Run(context.Background(), baseConfig(dir, StageBefore), Inputs{Runner: instantRunner(), Reader: &scriptReader{}})
		var ierr *InputError
		if !errors.As(err, &ierr) || !strings.Contains(err.Error(), "schema_version") {
			t.Fatalf("err %v", err)
		}
		if _, statErr := os.Stat(filepath.Join(dir, DirBefore)); !os.IsNotExist(statErr) {
			t.Fatal("before was created")
		}
	})
	t.Run("claims a missing baseline", func(t *testing.T) {
		dir := t.TempDir()
		writeBytes(t, filepath.Join(dir, FileManifest), []byte("{\"schema_version\":1,\"state\":\"baseline_captured\",\"limitations\":[],\"updated_at\":\"2026-11-10T12:00:00Z\"}\n"))
		_, err := Run(context.Background(), baseConfig(dir, StageAfter), Inputs{Runner: rejectRunner(t), Reader: &scriptReader{}})
		var ierr *InputError
		if !errors.As(err, &ierr) {
			t.Fatalf("err %v", err)
		}
	})
}

func TestInterruptedPartialIsKept(t *testing.T) {
	dir := t.TempDir()
	writeDiagnosis(t, filepath.Join(dir, DirBefore), diagOpt{counts: []float64{0, 0, 1000, 2000, 2000}})
	if err := os.MkdirAll(filepath.Join(dir, DirAfter), 0o755); err != nil {
		t.Fatal(err)
	}
	kept := []byte("keep\n")
	writeBytes(t, filepath.Join(dir, DirAfter, diagnosis.FileExperiment), kept)
	runner := rejectRunner(t)
	_, err := Run(context.Background(), baseConfig(dir, StageAfter), Inputs{Runner: runner, Reader: &scriptReader{}})
	expectValidation(t, err, "Remove the after directory")
	if runner.calls() != 0 {
		t.Fatal("workload started")
	}
	if mustText(t, filepath.Join(dir, DirAfter, diagnosis.FileExperiment)) != string(kept) {
		t.Fatal("partial after file changed")
	}
	m := readManifest(t, dir)
	if m.State != StateInterrupted || m.Failure == nil || m.Failure.Stage != StageAfter || m.Before == nil {
		t.Fatalf("manifest %+v", m)
	}
}

func TestSecretsAreNotStored(t *testing.T) {
	dir := t.TempDir()
	cfg := baseConfig(dir, StageBefore)
	cfg.Diagnosis.Experiment.Workload.TargetURL = "http://s3cret-user:s3cret-token@127.0.0.1:8090/work?api_key=s3cret-token"
	cfg.Diagnosis.Experiment.MetricsURL = "http://s3cret-user:s3cret-token@127.0.0.1:8090/metrics?api_key=s3cret-token"
	res, err := Run(context.Background(), cfg, Inputs{Runner: instantRunner(), Reader: &scriptReader{}})
	if err != nil {
		t.Fatal(err)
	}
	blob := allFiles(t, dir)
	if strings.Contains(blob, "s3cret-user") || strings.Contains(blob, "s3cret-token") {
		t.Fatal("saved metadata contains a credential or query secret")
	}
	if res.Manifest.Fingerprint == nil || res.Manifest.Fingerprint.TargetSHA256 == "" || !strings.Contains(res.Manifest.Fingerprint.TargetURL, "xxxxx") {
		t.Fatalf("fingerprint %+v", res.Manifest.Fingerprint)
	}
}

func TestJunctionsCannotBypassIsolation(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	writeBytes(t, filepath.Join(outside, "keep.txt"), []byte("keep\n"))
	linkDir(t, outside, filepath.Join(dir, DirBefore))
	runner := rejectRunner(t)
	_, err := Run(context.Background(), baseConfig(dir, StageBefore), Inputs{Runner: runner, Reader: &scriptReader{}})
	expectValidation(t, err, "link or alias")
	if runner.calls() != 0 {
		t.Fatal("workload started through an alias")
	}
	if mustText(t, filepath.Join(outside, "keep.txt")) != "keep\n" {
		t.Fatal("alias target changed")
	}
	if _, statErr := os.Stat(filepath.Join(outside, diagnosis.FileExperiment)); !os.IsNotExist(statErr) {
		t.Fatal("diagnosis was written through the junction")
	}

	real := t.TempDir()
	writeDiagnosis(t, filepath.Join(real, DirBefore), diagOpt{})
	linkDir(t, filepath.Join(real, DirBefore), filepath.Join(real, DirAfter))
	beforeSnap := snap(t, filepath.Join(real, DirBefore))
	_, err = Run(context.Background(), baseConfig(real, StageAfter), Inputs{Runner: runner, Reader: &scriptReader{}})
	expectValidation(t, err, "link or alias")
	if !sameSnap(beforeSnap, snap(t, filepath.Join(real, DirBefore))) {
		t.Fatal("after alias changed the baseline")
	}
}

func TestEvidencePointersResolve(t *testing.T) {
	dir := t.TempDir()
	writeDiagnosis(t, filepath.Join(dir, DirBefore), diagOpt{counts: []float64{0, 0, 1000, 2000, 2000}})
	writeDiagnosis(t, filepath.Join(dir, DirAfter), diagOpt{counts: []float64{0, 0, 2000, 4000, 4000}, fast: true})
	res, err := Run(context.Background(), Config{Stage: StageCompare, PairDir: dir}, Inputs{})
	if err != nil {
		t.Fatal(err)
	}
	docs := map[string]any{
		"before": decodeJSON(t, mustRead(t, filepath.Join(dir, DirBefore, diagnosis.FileAnalysis))),
		"after":  decodeJSON(t, mustRead(t, filepath.Join(dir, DirAfter, diagnosis.FileAnalysis))),
	}
	n := 0
	for _, route := range res.Comparison.Report.Validation.Routes {
		for _, ref := range route.Evidence {
			if ref.Role != fixvalidation.RoleMeasurement {
				continue
			}
			n++
			got := resolve(t, docs[ref.Side], ref.Pointer)
			switch {
			case strings.HasSuffix(ref.Pointer, "/requests"):
				obj, ok := got.(map[string]any)
				if !ok || obj["requests"] == nil {
					t.Fatalf("%s %s = %#v", ref.Side, ref.Pointer, got)
				}
			case strings.HasSuffix(ref.Pointer, "/p95"):
				obj, ok := got.(map[string]any)
				if !ok || obj["seconds"] == nil {
					t.Fatalf("%s %s = %#v", ref.Side, ref.Pointer, got)
				}
			default:
				t.Fatalf("unexpected pointer %s %s", ref.Side, ref.Pointer)
			}
		}
	}
	if n == 0 {
		t.Fatal("no evidence pointers")
	}
}

func TestMissingManifestDoesNotInventURLIdentity(t *testing.T) {
	dir := t.TempDir()
	cfg := baseConfig(dir, StageBefore)
	cfg.Diagnosis.Experiment.Workload.TargetURL = "http://127.0.0.1:8090/work?mode=baseline"
	if _, err := Run(context.Background(), cfg, Inputs{Runner: instantRunner(), Reader: &scriptReader{}}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, FileManifest)); err != nil {
		t.Fatal(err)
	}
	beforeSnap := snap(t, filepath.Join(dir, DirBefore))
	redacted := "http://127.0.0.1:8090/work?xxxxx"
	invented := hashText(redacted)

	same := cfg
	same.Stage = StageAfter
	runner := rejectRunner(t)
	_, err := Run(context.Background(), same, Inputs{Runner: runner, Reader: &scriptReader{}})
	expectValidation(t, err, "not invented")
	expectValidation(t, err, "before/ directory was kept")
	if strings.Contains(err.Error(), "mode=baseline") || strings.Contains(err.Error(), invented) {
		t.Fatalf("error exposes the query or an invented hash: %v", err)
	}

	other := same
	other.Diagnosis.Experiment.Workload.TargetURL = "http://127.0.0.1:8090/work?mode=optimized"
	_, err = Run(context.Background(), other, Inputs{Runner: runner, Reader: &scriptReader{}})
	expectValidation(t, err, "no after workload was started")
	if strings.Contains(err.Error(), "mode=optimized") || strings.Contains(err.Error(), "mode=baseline") {
		t.Fatalf("error exposes a query: %v", err)
	}
	if runner.calls() != 0 || !sameSnap(beforeSnap, snap(t, filepath.Join(dir, DirBefore))) {
		t.Fatal("baseline changed or a workload started")
	}
	if _, statErr := os.Stat(filepath.Join(dir, FileManifest)); !os.IsNotExist(statErr) {
		t.Fatal("a pair manifest was written without the original URL fingerprints")
	}
	if _, statErr := os.Stat(filepath.Join(dir, DirAfter)); !os.IsNotExist(statErr) {
		t.Fatal("after directory was created")
	}

	writeDiagnosis(t, filepath.Join(dir, DirAfter), diagOpt{counts: []float64{0, 0, 1000, 2000, 2000}})
	afterSnap := snap(t, filepath.Join(dir, DirAfter))
	res, err := Run(context.Background(), Config{Stage: StageCompare, PairDir: dir}, Inputs{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Manifest.State != StateComparisonCompleted || res.Manifest.Fingerprint == nil {
		t.Fatalf("state %s", res.Manifest.State)
	}
	fp := res.Manifest.Fingerprint
	if fp.TargetSHA256 != "" || fp.MetricsSHA256 != "" || fp.TargetSHA256 == invented || fp.TargetSHA256 == hashText(fp.TargetURL) {
		t.Fatalf("fingerprint invented a hash: %+v", fp)
	}
	if !sameSnap(beforeSnap, snap(t, filepath.Join(dir, DirBefore))) || !sameSnap(afterSnap, snap(t, filepath.Join(dir, DirAfter))) {
		t.Fatal("compare changed a diagnosis")
	}
	if !hasLimitation(res.Manifest, "not complete configuration verification") {
		t.Fatalf("limitations %v", res.Manifest.Limitations)
	}
}

func TestCompletedComparisonBehindManifestIsAdopted(t *testing.T) {
	dir := t.TempDir()
	writeDiagnosis(t, filepath.Join(dir, DirBefore), diagOpt{counts: []float64{0, 0, 1000, 2000, 2000}})
	writeDiagnosis(t, filepath.Join(dir, DirAfter), diagOpt{counts: []float64{0, 0, 2000, 4000, 4000}, fast: true})
	res, err := Run(context.Background(), Config{Stage: StageCompare, PairDir: dir}, Inputs{})
	if err != nil {
		t.Fatal(err)
	}
	outcome := res.Manifest.Comparison.Outcome
	if outcome != fixvalidation.OutcomeImprovement {
		t.Fatalf("outcome %s", outcome)
	}
	beforeSnap := snap(t, filepath.Join(dir, DirBefore))
	afterSnap := snap(t, filepath.Join(dir, DirAfter))
	compSnap := snap(t, filepath.Join(dir, DirComparison))
	m := readManifest(t, dir)
	m.State = StateCandidateCaptured
	m.Comparison = nil
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeBytes(t, filepath.Join(dir, FileManifest), append(data, '\n'))

	runner := rejectRunner(t)
	adopted, err := Run(context.Background(), Config{Stage: StageCompare, PairDir: dir}, Inputs{Runner: runner, Reader: &scriptReader{}})
	if err != nil {
		t.Fatal(err)
	}
	if adopted.Comparison != nil || adopted.Manifest.State != StateComparisonCompleted || adopted.Manifest.Comparison.Outcome != outcome {
		t.Fatalf("adopted %#v", adopted.Manifest.Comparison)
	}
	if runner.calls() != 0 {
		t.Fatal("a workload ran during recovery")
	}
	if !sameSnap(beforeSnap, snap(t, filepath.Join(dir, DirBefore))) || !sameSnap(afterSnap, snap(t, filepath.Join(dir, DirAfter))) || !sameSnap(compSnap, snap(t, filepath.Join(dir, DirComparison))) {
		t.Fatal("recovery rewrote a saved stage")
	}
	if _, err := Run(context.Background(), Config{Stage: StageCompare, PairDir: dir}, Inputs{}); err == nil || !strings.Contains(err.Error(), "not overwritten") {
		t.Fatalf("second compare err %v", err)
	}
	if !sameSnap(compSnap, snap(t, filepath.Join(dir, DirComparison))) {
		t.Fatal("second compare rewrote the result")
	}
}

func TestCorruptValidationIsNotAdopted(t *testing.T) {
	dir := pairReadyToAdopt(t)
	beforeSnap, afterSnap := diagnosisSnaps(t, dir)
	writeBytes(t, filepath.Join(dir, DirComparison, runvalidation.FileValidation), []byte("{"))
	compSnap := snap(t, filepath.Join(dir, DirComparison))

	_, err := Run(context.Background(), Config{Stage: StageCompare, PairDir: dir}, Inputs{})
	expectRejectedComparison(t, dir, beforeSnap, afterSnap, compSnap, err, "validation.json")
	if mustText(t, filepath.Join(dir, DirComparison, runvalidation.FileValidation)) != "{" {
		t.Fatal("corrupt validation.json was repaired")
	}
}

func TestValidationOutcomeMismatchIsNotAdopted(t *testing.T) {
	dir := pairReadyToAdopt(t)
	beforeSnap, afterSnap := diagnosisSnaps(t, dir)
	valPath := filepath.Join(dir, DirComparison, runvalidation.FileValidation)
	rewriteJSON(t, valPath, func(doc map[string]any) {
		validation, ok := doc["validation"].(map[string]any)
		if !ok {
			t.Fatal("validation.json has no validation object")
		}
		if validation["outcome"] == fixvalidation.OutcomeRegression {
			t.Fatal("fixture outcome is already the disagreement value")
		}
		validation["outcome"] = fixvalidation.OutcomeRegression
	})
	compSnap := snap(t, filepath.Join(dir, DirComparison))

	_, err := Run(context.Background(), Config{Stage: StageCompare, PairDir: dir}, Inputs{})
	expectRejectedComparison(t, dir, beforeSnap, afterSnap, compSnap, err, "does not match")
	text := mustText(t, valPath)
	if !strings.Contains(text, fixvalidation.OutcomeRegression) {
		t.Fatal("disagreed validation.json was repaired")
	}
	manifest := mustText(t, filepath.Join(dir, DirComparison, runvalidation.FileManifest))
	if strings.Contains(manifest, fixvalidation.OutcomeRegression) || !strings.Contains(manifest, fixvalidation.OutcomeImprovement) {
		t.Fatal("comparison manifest outcome changed")
	}
}

func TestForeignComparisonHashesAreNotAdopted(t *testing.T) {
	dir := pairReadyToAdopt(t)
	beforeSnap, afterSnap := diagnosisSnaps(t, dir)
	foreignBefore := strings.Repeat("ab", 32)
	foreignAfter := strings.Repeat("cd", 32)
	manPath := filepath.Join(dir, DirComparison, runvalidation.FileManifest)
	rewriteJSON(t, manPath, func(doc map[string]any) {
		before, ok := doc["before"].(map[string]any)
		after, okAfter := doc["after"].(map[string]any)
		if !ok || !okAfter {
			t.Fatal("comparison manifest has no before and after records")
		}
		before["analysis_sha256"] = foreignBefore
		after["analysis_sha256"] = foreignAfter
	})
	compSnap := snap(t, filepath.Join(dir, DirComparison))

	_, err := Run(context.Background(), Config{Stage: StageCompare, PairDir: dir}, Inputs{})
	expectRejectedComparison(t, dir, beforeSnap, afterSnap, compSnap, err, "SHA-256")
	text := mustText(t, manPath)
	if !strings.Contains(text, foreignBefore) || !strings.Contains(text, foreignAfter) {
		t.Fatal("foreign comparison manifest was repaired")
	}
}

func TestPairManifestReplaceKeepsPreviousFile(t *testing.T) {
	dir := t.TempDir()
	reservation := filepath.Join(dir, reservationFile)
	if err := os.WriteFile(reservation, []byte("pid 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	first := Manifest{SchemaVersion: SchemaVersion, State: StateBaselineCaptured, Limitations: []string{"one"}}
	if err := writeJSONFile(dir, FileManifest, first); err != nil {
		t.Fatal(err)
	}
	if readManifest(t, dir).State != StateBaselineCaptured {
		t.Fatal(readManifest(t, dir).State)
	}
	if temps := pairTempFiles(t, dir); len(temps) != 0 {
		t.Fatalf("temporary files after create: %v", temps)
	}

	second := first
	second.State = StateCandidateCaptured
	second.Limitations = []string{"two"}
	if err := writeJSONFile(dir, FileManifest, second); err != nil {
		t.Fatal(err)
	}
	if readManifest(t, dir).State != StateCandidateCaptured {
		t.Fatal("existing manifest was not replaced")
	}
	if temps := pairTempFiles(t, dir); len(temps) != 0 {
		t.Fatalf("temporary files after replace: %v", temps)
	}
	if mustText(t, reservation) != "pid 1\n" {
		t.Fatal("replacement changed the pair reservation")
	}
	previous := mustText(t, filepath.Join(dir, FileManifest))

	original := replaceFile
	t.Cleanup(func() { replaceFile = original })
	replaceFile = func(string, string) error { return errors.New("rename failed") }
	third := second
	third.State = StateComparisonCompleted
	err := writeJSONFile(dir, FileManifest, third)
	if err == nil || !strings.Contains(err.Error(), "rename failed") {
		t.Fatalf("err %v", err)
	}
	if mustText(t, filepath.Join(dir, FileManifest)) != previous {
		t.Fatal("failed replacement destroyed the previous manifest")
	}
	if temps := pairTempFiles(t, dir); len(temps) != 0 {
		t.Fatalf("temporary files after failed replacement: %v", temps)
	}
	if mustText(t, reservation) != "pid 1\n" {
		t.Fatal("failed replacement released the pair reservation")
	}
}

func TestWriteFailurePreservesDiagnosis(t *testing.T) {
	dir := t.TempDir()
	_, err := Run(context.Background(), baseConfig(dir, StageBefore), Inputs{
		Runner: instantRunner(),
		Reader: &scriptReader{},
		write: func(string, string, any) error {
			return errors.New("disk full")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, DirBefore, diagnosis.FileManifest)); statErr != nil {
		t.Fatal(statErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, FileManifest)); !os.IsNotExist(statErr) {
		t.Fatal("pair manifest was written")
	}
	if _, statErr := os.Stat(filepath.Join(dir, reservationFile)); !os.IsNotExist(statErr) {
		t.Fatal("reservation left behind")
	}
}

func pairReadyToAdopt(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeDiagnosis(t, filepath.Join(dir, DirBefore), diagOpt{counts: []float64{0, 0, 1000, 2000, 2000}})
	writeDiagnosis(t, filepath.Join(dir, DirAfter), diagOpt{counts: []float64{0, 0, 2000, 4000, 4000}, fast: true})
	res, err := Run(context.Background(), Config{Stage: StageCompare, PairDir: dir}, Inputs{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Manifest.Comparison == nil || res.Manifest.Comparison.Outcome != fixvalidation.OutcomeImprovement {
		t.Fatalf("outcome %#v", res.Manifest.Comparison)
	}
	m := res.Manifest
	m.State = StateCandidateCaptured
	m.Comparison = nil
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeBytes(t, filepath.Join(dir, FileManifest), append(data, '\n'))
	return dir
}

func diagnosisSnaps(t *testing.T, dir string) (before, after map[string]string) {
	t.Helper()
	return snap(t, filepath.Join(dir, DirBefore)), snap(t, filepath.Join(dir, DirAfter))
}

func expectRejectedComparison(t *testing.T, dir string, beforeSnap, afterSnap, compSnap map[string]string, err error, text string) {
	t.Helper()
	expectValidation(t, err, text)
	expectValidation(t, err, "were not repaired")
	if !sameSnap(beforeSnap, snap(t, filepath.Join(dir, DirBefore))) ||
		!sameSnap(afterSnap, snap(t, filepath.Join(dir, DirAfter))) ||
		!sameSnap(compSnap, snap(t, filepath.Join(dir, DirComparison))) {
		t.Fatal("rejected adoption changed a saved stage")
	}
	if readManifest(t, dir).State == StateComparisonCompleted {
		t.Fatal("rejected comparison was adopted")
	}
	if temps := pairTempFiles(t, dir); len(temps) != 0 {
		t.Fatalf("temporary files left behind: %v", temps)
	}
}

func rewriteJSON(t *testing.T, path string, edit func(map[string]any)) {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(mustRead(t, path), &doc); err != nil {
		t.Fatal(err)
	}
	edit(doc)
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeBytes(t, path, append(data, '\n'))
}

func pairTempFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".ruptiq-pair-") && strings.HasSuffix(name, ".json") {
			names = append(names, name)
		}
	}
	return names
}

func baseConfig(dir, stage string) Config {
	return Config{
		Stage:   stage,
		PairDir: dir,
		Diagnosis: diagnosis.Config{
			Experiment: experiment.Config{
				Workload:          workload.Spec{TargetURL: "http://127.0.0.1:8090/work", VUs: 2, Duration: time.Second},
				MetricsURL:        "http://127.0.0.1:8090/metrics",
				Interval:          100 * time.Millisecond,
				Timeout:           30 * time.Second,
				MaxSamples:        100,
				MaxTelemetryBytes: experiment.DefaultMaxTelemetryBytes,
			},
			Analysis:      analysis.DefaultConfig(),
			Investigation: investigation.DefaultConfig(),
		},
	}
}

func instantRunner() *callRunner {
	return &callRunner{fn: func(context.Context, workload.Spec) (workload.Result, error) {
		return healthyResult(), nil
	}}
}

func rejectRunner(t *testing.T) *callRunner {
	t.Helper()
	return &callRunner{fn: func(context.Context, workload.Spec) (workload.Result, error) {
		t.Error("workload started")
		return workload.Result{}, errors.New("workload started")
	}}
}

func healthyResult() workload.Result {
	return workload.Result{
		Runner: "fake", RunnerVersion: "0", VUs: 2, ConfiguredDurationSeconds: 1,
		StartedAt: time.Now().Add(-time.Second).UTC(), FinishedAt: time.Now().UTC(),
		Metrics: workload.Metrics{
			DurationSeconds: 1, HTTPRequests: 10, RequestsPerSecond: 10,
			LatencyMs: workload.Latency{Avg: 1, Min: 0.1, P50: 1, P90: 2, P95: 3, Max: 4},
		},
	}
}

type callRunner struct {
	fn func(context.Context, workload.Spec) (workload.Result, error)
	mu sync.Mutex
	n  int
}

func (r *callRunner) Run(ctx context.Context, spec workload.Spec) (workload.Result, error) {
	r.mu.Lock()
	r.n++
	fn := r.fn
	r.mu.Unlock()
	return fn(ctx, spec)
}

func (r *callRunner) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n
}

type scriptReader struct {
	mu   sync.Mutex
	last time.Time
	err  error
}

func (r *scriptReader) Read(context.Context) (telemetry.Snapshot, error) {
	if r.err != nil {
		return telemetry.Snapshot{}, r.err
	}
	r.mu.Lock()
	for {
		at := time.Now().UTC()
		if r.last.IsZero() || at.After(r.last) {
			r.last = at
			r.mu.Unlock()
			value := telemetry.Float(1)
			return telemetry.Snapshot{
				ScrapedAt:             at,
				ScrapeDurationSeconds: 0.001,
				Families: []telemetry.Family{{
					Name: analysis.DefaultRequestsCounter, Type: telemetry.Counter,
					Series: []telemetry.Series{{Labels: map[string]string{"method": "GET", "route": "/work", "status": "200"}, Value: &value}},
				}},
			}, nil
		}
		r.mu.Unlock()
		time.Sleep(time.Millisecond)
		r.mu.Lock()
	}
}

func expectValidation(t *testing.T, err error, text string) {
	t.Helper()
	var verr *ValidationError
	if !errors.As(err, &verr) || !strings.Contains(err.Error(), text) {
		t.Fatalf("err = %v", err)
	}
}

func hasLimitation(m Manifest, text string) bool {
	for _, line := range m.Limitations {
		if strings.Contains(line, text) {
			return true
		}
	}
	return false
}

func readManifest(t *testing.T, dir string) Manifest {
	t.Helper()
	var m Manifest
	if err := json.Unmarshal(mustRead(t, filepath.Join(dir, FileManifest)), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func snap(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		out[e.Name()] = string(mustRead(t, filepath.Join(dir, e.Name())))
	}
	return out
}

func sameSnap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func mustText(t *testing.T, path string) string {
	t.Helper()
	return string(mustRead(t, path))
}

func allFiles(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		b.Write(data)
		b.WriteByte('\n')
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func decodeJSON(t *testing.T, data []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func resolve(t *testing.T, doc any, pointer string) any {
	t.Helper()
	if !strings.HasPrefix(pointer, "/") {
		t.Fatalf("pointer %s", pointer)
	}
	cur := doc
	for _, part := range strings.Split(pointer, "/")[1:] {
		switch node := cur.(type) {
		case map[string]any:
			next, ok := node[part]
			if !ok {
				t.Fatalf("pointer %s missing %s", pointer, part)
			}
			cur = next
		case []any:
			i := 0
			for _, r := range part {
				if r < '0' || r > '9' {
					t.Fatalf("pointer %s index %s", pointer, part)
				}
				i = i*10 + int(r-'0')
			}
			if i < 0 || i >= len(node) {
				t.Fatalf("pointer %s index %d", pointer, i)
			}
			cur = node[i]
		default:
			t.Fatalf("pointer %s at %#v", pointer, cur)
		}
	}
	return cur
}

func linkDir(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err == nil {
		return
	}
	out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		t.Fatalf("cannot alias %s as %s: %v\n%s", target, link, err, out)
	}
}
