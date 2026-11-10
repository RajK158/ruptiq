package diagnosis

import (
	"fmt"
	"io"

	"github.com/RajK158/ruptiq/internal/experiment"
	"github.com/RajK158/ruptiq/internal/investigation"
)

// WriteSummary prints a short description of a diagnosis: whether the stages
// finished, the investigation's own summary when there is one, and the
// manifest's limitations. A completed run is not reported as a performance
// finding. An abstention is named as an abstention.
func WriteSummary(w io.Writer, res Result) error {
	m := res.Manifest
	if _, err := fmt.Fprintf(w, "Ruptiq diagnosis\n  Outcome:         %s\n  Target:          %s\n  Metrics:         %s\n",
		m.Outcome, m.TargetURL, m.MetricsURL); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "  Saved:           %s\n", artifactList(m)); err != nil {
		return err
	}
	fmt.Fprintln(w, "  Execution and a performance finding are different: a completed outcome only means the reports were saved.")

	for _, s := range m.Stages {
		switch s.Status {
		case StatusSucceeded:
			extra := ""
			if s.ExperimentStatus != "" {
				extra = " (experiment " + s.ExperimentStatus + ")"
			}
			if s.InvestigationOutcome != "" {
				extra = " (outcome " + s.InvestigationOutcome + ")"
			}
			fmt.Fprintf(w, "  Stage %-13s %s%s\n", s.Name+":", s.Status, extra)
		case StatusSkipped:
			fmt.Fprintf(w, "  Stage %-13s skipped: %s\n", s.Name+":", s.Reason)
		case StatusFailed:
			fmt.Fprintf(w, "  Stage %-13s failed: %s\n", s.Name+":", s.Error)
		}
	}
	fmt.Fprintln(w)

	if res.Experiment != nil {
		if err := experiment.WriteSummary(w, *res.Experiment); err != nil {
			return err
		}
		fmt.Fprintln(w)
	}
	if res.Analysis != nil {
		fmt.Fprintf(w, "Analysis: %d window(s), %d warning(s). Signals are measurements, not a finding.\n\n",
			len(res.Analysis.Windows), len(res.Analysis.Warnings))
	}
	if res.Investigation != nil {
		if res.Investigation.Outcome == investigation.OutcomeInsufficientEvidence {
			fmt.Fprintln(w, "The investigation abstained. That is not a failure of this command and not a finding that performance is healthy.")
		}
		if err := investigation.WriteSummary(w, *res.Investigation); err != nil {
			return err
		}
		fmt.Fprintln(w, "Evidence references are JSON pointers into analysis.json. Window numbers in the text start at 1; pointer array indexes start at 0.")
	}
	if len(m.Limitations) > 0 {
		fmt.Fprintln(w, "\nLimitations:")
		for _, l := range m.Limitations {
			fmt.Fprintf(w, "  - %s\n", l)
		}
	}
	return nil
}

func artifactList(m Manifest) string {
	if len(m.Artifacts) == 0 {
		return "none"
	}
	out := m.Artifacts[0]
	for _, name := range m.Artifacts[1:] {
		out += ", " + name
	}
	return out
}
