package pairworkflow

import (
	"fmt"
	"io"
	"strings"

	"github.com/RajK158/ruptiq/internal/runvalidation"
)

// WriteSummary prints the workflow state, the manual-change boundary, and,
// when this invocation compared two diagnoses, the fix-validation summary.
func WriteSummary(w io.Writer, res Result) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Pair workflow state: %s\n", res.Manifest.State)
	if res.Manifest.Before != nil {
		writeStage(&b, "Baseline", res.Manifest.Before)
	}
	if res.Manifest.After != nil {
		writeStage(&b, "Candidate", res.Manifest.After)
	}
	if fp := res.Manifest.Fingerprint; fp != nil {
		fmt.Fprintf(&b, "Baseline configuration: %s, metrics %s, %d VUs, %.4gs, interval %.4gs, requests %s, histogram %s\n",
			fp.TargetURL, fp.MetricsURL, fp.VUs, fp.DurationSeconds, fp.SampleIntervalSeconds,
			fp.RequestsCounter, fp.DurationHistogram)
	}
	switch res.Manifest.State {
	case StateBaselineCaptured:
		b.WriteString(BaselineCapturedMessage + "\n")
	case StateCandidateCaptured:
		b.WriteString(CandidateCapturedMessage + "\n")
	case StateComparisonCompleted:
		fmt.Fprintf(&b, "Comparison outcome: %s\n", outcomeOf(res))
		b.WriteString("The pair workflow finished. The outcome is the fix-validation result for this one pair. A completed workflow is not proof that a change caused the difference, and it is not a statistical significance test.\n")
	}
	for _, line := range res.Manifest.Limitations {
		fmt.Fprintf(&b, "Limitation: %s\n", line)
	}
	if res.Manifest.Failure != nil {
		fmt.Fprintf(&b, "Failure (%s): %s\nRecovery: %s\n", res.Manifest.Failure.Stage, res.Manifest.Failure.Message, res.Manifest.Failure.Recovery)
	}
	if _, err := io.WriteString(w, b.String()); err != nil {
		return err
	}
	if res.Comparison != nil && res.Comparison.Report.Validation != nil {
		return runvalidation.WriteSummary(w, *res.Comparison)
	}
	return nil
}

func writeStage(b *strings.Builder, label string, stage *DiagnosisStage) {
	fmt.Fprintf(b, "%s: %s diagnosis %s, experiment %s, telemetry %s, investigation %s, sha256 %s\n",
		label, stage.Directory, stage.DiagnosisOutcome, stage.ExperimentStatus, stage.TelemetryCompleteness,
		stage.InvestigationOutcome, stage.AnalysisSHA256)
}

func outcomeOf(res Result) string {
	if res.Manifest.Comparison != nil && res.Manifest.Comparison.Outcome != "" {
		return res.Manifest.Comparison.Outcome
	}
	if res.Comparison != nil {
		return res.Comparison.Manifest.Outcome
	}
	return ""
}
