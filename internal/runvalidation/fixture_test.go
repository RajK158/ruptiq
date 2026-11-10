package runvalidation

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/RajK158/ruptiq/internal/analysis"
	"github.com/RajK158/ruptiq/internal/diagnosis"
	"github.com/RajK158/ruptiq/internal/experiment"
	"github.com/RajK158/ruptiq/internal/investigation"
	"github.com/RajK158/ruptiq/internal/telemetry"
	"github.com/RajK158/ruptiq/internal/workload"
)

var t0 = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

type diagOpt struct {
	vus        int
	status     experiment.Status
	counts     []float64
	fast       bool
	invOutcome string
	skipInv    bool
}

func writeDiagnosis(t *testing.T, dir string, opt diagOpt) {
	t.Helper()
	if opt.vus == 0 {
		opt.vus = 2
	}
	if opt.status == "" {
		opt.status = experiment.StatusPassed
	}
	if opt.counts == nil {
		opt.counts = []float64{0, 0, 1000, 2000, 2000}
	}
	if opt.invOutcome == "" {
		opt.invOutcome = investigation.OutcomeInsufficientEvidence
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	exp := buildExperiment(opt)
	expData, err := json.MarshalIndent(exp, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	expData = append(expData, '\n')
	decoded, err := analysis.DecodeExperiment(expData)
	if err != nil {
		t.Fatalf("fixture experiment: %v", err)
	}
	an, err := analysis.Analyze(decoded, analysis.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	rep := analysis.Report{SchemaVersion: analysis.SchemaVersion, Input: diagnosis.FileExperiment, Analysis: &an}
	anData, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	anData = append(anData, '\n')
	if _, err := investigation.DecodeAnalysis(anData); err != nil {
		t.Fatalf("fixture analysis: %v", err)
	}
	writeBytes(t, filepath.Join(dir, diagnosis.FileExperiment), expData)
	writeBytes(t, filepath.Join(dir, diagnosis.FileAnalysis), anData)

	invStatus := diagnosis.StatusSucceeded
	var invData []byte
	if opt.skipInv {
		invStatus = diagnosis.StatusSkipped
	} else {
		inv := investigation.Report{
			SchemaVersion: investigation.SchemaVersion,
			Input:         diagnosis.FileAnalysis,
			Investigation: &investigation.Investigation{
				Outcome:     opt.invOutcome,
				Summary:     "fixture investigation",
				Findings:    []investigation.Finding{},
				Assessments: []investigation.Assessment{},
				Limitations: []string{"fixture"},
			},
		}
		invData, err = json.MarshalIndent(inv, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		invData = append(invData, '\n')
		writeBytes(t, filepath.Join(dir, diagnosis.FileInvestigation), invData)
	}
	man := diagnosisManifest(exp, invStatus, opt.invOutcome)
	manData, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeBytes(t, filepath.Join(dir, diagnosis.FileManifest), append(manData, '\n'))
}

func diagnosisManifest(exp experiment.Report, invStatus, invOutcome string) diagnosis.Manifest {
	inv := diagnosis.Stage{Name: diagnosis.StageInvestigation, Status: invStatus}
	arts := []string{diagnosis.FileExperiment, diagnosis.FileAnalysis}
	outcome := diagnosis.OutcomeCompleted
	if invStatus == diagnosis.StatusSucceeded {
		inv.Artifact = diagnosis.FileInvestigation
		inv.InvestigationOutcome = invOutcome
		arts = append(arts, diagnosis.FileInvestigation)
	} else {
		inv.Reason = "investigation was not run"
		outcome = diagnosis.OutcomeFailed
	}
	arts = append(arts, diagnosis.FileManifest)
	return diagnosis.Manifest{
		SchemaVersion: diagnosis.SchemaVersion,
		Versions: diagnosis.Versions{
			Diagnosis:     diagnosis.SchemaVersion,
			Experiment:    experiment.ReportSchemaVersion,
			Analysis:      analysis.SchemaVersion,
			Investigation: investigation.SchemaVersion,
		},
		Outcome:    outcome,
		StartedAt:  exp.StartedAt,
		FinishedAt: exp.FinishedAt,
		TargetURL:  exp.Config.TargetURL,
		MetricsURL: exp.Config.MetricsURL,
		Stages: []diagnosis.Stage{
			{Name: diagnosis.StageExperiment, Status: diagnosis.StatusSucceeded, Artifact: diagnosis.FileExperiment, ExperimentStatus: string(exp.Status)},
			{Name: diagnosis.StageAnalysis, Status: diagnosis.StatusSucceeded, Artifact: diagnosis.FileAnalysis},
			inv,
		},
		Artifacts:   arts,
		Limitations: []string{"fixture"},
	}
}

func buildExperiment(opt diagOpt) experiment.Report {
	phases := make([]experiment.Phase, len(opt.counts))
	phases[0] = experiment.BeforeWorkload
	for i := 1; i < len(opt.counts)-1; i++ {
		phases[i] = experiment.DuringWorkload
	}
	phases[len(phases)-1] = experiment.AfterWorkload
	passed := opt.status != experiment.StatusFailed
	verdict := workload.Verdict{Passed: passed, MaxFailureRate: 0}
	if !passed {
		verdict.Reasons = []string{"failure rate exceeds the allowed rate"}
	}
	r := experiment.Report{
		SchemaVersion: experiment.ReportSchemaVersion,
		Status:        opt.status,
		StartedAt:     t0,
		Config: experiment.ReportConfig{
			TargetURL: "http://127.0.0.1:8090/work", VUs: opt.vus, DurationSeconds: 4,
			MetricsURL: "http://127.0.0.1:8090/metrics", SampleIntervalSeconds: 1, MaxSamples: 1000,
			MaxTelemetryBytes: experiment.DefaultMaxTelemetryBytes, TimeoutSeconds: 64,
		},
		Workload: experiment.WorkloadOutcome{
			Result: &workload.Result{
				Runner: "k6", RunnerVersion: "2.3.0", TargetURL: "http://127.0.0.1:8090/work",
				VUs: opt.vus, ConfiguredDurationSeconds: 4, StartedAt: at(0.1), FinishedAt: at(4.1),
				Metrics: workload.Metrics{
					DurationSeconds: 4, HTTPRequests: 1000, RequestsPerSecond: 250,
					LatencyMs: workload.Latency{Avg: 1, Min: 0.1, P50: 1, P90: 2, P95: 3, Max: 4},
				},
			},
			Verdict: verdict,
		},
		Telemetry: experiment.TelemetryOutcome{
			Samples: []experiment.Sample{}, Errors: []experiment.ScrapeError{}, NotRetained: []experiment.NotRetained{},
		},
	}
	if opt.status != experiment.StatusPassed {
		r.Reasons = []string{"fixture status " + string(opt.status)}
	}
	for i, count := range opt.counts {
		r.Telemetry.Samples = append(r.Telemetry.Samples, experiment.Sample{
			Sequence: i + 1, Phase: phases[i], ScrapedAt: at(float64(i)), OffsetSeconds: float64(i),
			ScrapeDurationSeconds: 0.002, Families: families(count, opt.fast),
		})
	}
	end := float64(len(opt.counts) - 1)
	r.FinishedAt = at(end + 0.25)
	t := &r.Telemetry
	t.Attempts = len(opt.counts)
	t.Succeeded = len(opt.counts)
	t.Completeness = experiment.Complete
	for _, s := range t.Samples {
		size, _ := json.Marshal(s)
		t.RetainedBytes += int64(len(size))
	}
	return r
}

func families(count float64, fast bool) []telemetry.Family {
	value := telemetry.Float(count)
	return []telemetry.Family{
		{Name: analysis.DefaultRequestsCounter, Type: telemetry.Counter, Series: []telemetry.Series{{
			Labels: labels(), Value: &value,
		}}},
		{Name: analysis.DefaultDurationHistogram, Type: telemetry.Histogram, Series: []telemetry.Series{
			histogram(uint64(count), fast),
		}},
	}
}

func labels() map[string]string {
	return map[string]string{"method": "GET", "route": "/work", "status": "200"}
}

func histogram(count uint64, fast bool) telemetry.Series {
	bounds := []float64{0.005, 0.01, 0.025, 0.05, 0.1}
	h := &telemetry.HistogramValue{Count: count, Sum: telemetry.Float(0.002 * float64(count))}
	if !fast {
		h.Sum = telemetry.Float(0.04 * float64(count))
	}
	for _, b := range bounds {
		cum := uint64(0)
		if fast || b >= 0.05 {
			cum = count
		}
		h.Buckets = append(h.Buckets, telemetry.Bucket{UpperBound: telemetry.Float(b), CumulativeCount: cum})
	}
	h.Buckets = append(h.Buckets, telemetry.Bucket{UpperBound: telemetry.Float(math.Inf(1)), CumulativeCount: count})
	return telemetry.Series{Labels: labels(), Histogram: h}
}

func at(s float64) time.Time {
	return t0.Add(time.Duration(math.Round(s * float64(time.Second))))
}

func writeBytes(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
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
			t.Fatalf("unexpected directory %s", e.Name())
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = string(b)
	}
	return out
}
