package runvalidation

import (
	"fmt"
	"io"
	"strings"

	"github.com/RajK158/ruptiq/internal/fixvalidation"
)

// WriteSummary prints the comparison and where its evidence pointers point.
// The numbers and the outcome come from fixvalidation.WriteSummary.
func WriteSummary(w io.Writer, res Result) error {
	if res.Report.Validation == nil {
		return fmt.Errorf("validation is missing")
	}
	var b strings.Builder
	b.WriteString("Saved diagnosis comparison. No workload was run, and neither input directory was changed.\n")
	fmt.Fprintf(&b, "Before: %s sha256 %s (experiment %s, telemetry %s)\n",
		res.Manifest.Before.AnalysisFile, res.Manifest.Before.AnalysisSHA256,
		res.Manifest.Before.ExperimentStatus, res.Manifest.Before.TelemetryCompleteness)
	fmt.Fprintf(&b, "After:  %s sha256 %s (experiment %s, telemetry %s)\n",
		res.Manifest.After.AnalysisFile, res.Manifest.After.AnalysisSHA256,
		res.Manifest.After.ExperimentStatus, res.Manifest.After.TelemetryCompleteness)
	if sameAnalysisFile(res.Manifest.Before, res.Manifest.After) {
		b.WriteString("Duplicate source: " + duplicateSourceLimitation + "\n")
	}
	b.WriteString("Evidence pointers in validation.json are JSON pointers into that side's analysis.json. The side is before or after. They are not pointers into validation.json.\n")
	b.WriteString("A completed comparison is not proof that a code or deployment change caused the difference. Ruptiq does not apply that change.\n\n")
	if _, err := io.WriteString(w, b.String()); err != nil {
		return err
	}
	return fixvalidation.WriteSummary(w, *res.Report.Validation)
}
