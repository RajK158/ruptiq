package pairworkflow

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

var fixtureT0 = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

type diagOpt struct {
	vus        int
	counts     []float64
	fast       bool
	invOutcome string
}

func writeDiagnosis(t *testing.T, dir string, opt diagOpt) {
	t.Helper()
	if opt.vus == 0 {
		opt.vus = 2
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
	invData, err := json.MarshalIndent(inv, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeBytes(t, filepath.Join(dir, diagnosis.FileInvestigation), append(invData, '\n'))
	man := diagnosis.Manifest{
		SchemaVersion: diagnosis.SchemaVersion,
		Versions: diagnosis.Versions{
			Diagnosis:     diagnosis.SchemaVersion,
			Experiment:    experiment.ReportSchemaVersion,
			Analysis:      analysis.SchemaVersion,
			Investigation: investigation.SchemaVersion,
		},
		Outcome:    diagnosis.OutcomeCompleted,
		StartedAt:  exp.StartedAt,
		FinishedAt: exp.FinishedAt,
		TargetURL:  exp.Config.TargetURL,
		MetricsURL: exp.Config.MetricsURL,
		Stages: []diagnosis.Stage{
			{Name: diagnosis.StageExperiment, Status: diagnosis.StatusSucceeded, Artifact: diagnosis.FileExperiment, ExperimentStatus: string(exp.Status)},
			{Name: diagnosis.StageAnalysis, Status: diagnosis.StatusSucceeded, Artifact: diagnosis.FileAnalysis},
			{Name: diagnosis.StageInvestigation, Status: diagnosis.StatusSucceeded, Artifact: diagnosis.FileInvestigation, InvestigationOutcome: opt.invOutcome},
		},
		Artifacts:   []string{diagnosis.FileExperiment, diagnosis.FileAnalysis, diagnosis.FileInvestigation, diagnosis.FileManifest},
		Limitations: []string{"fixture"},
	}
	manData, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeBytes(t, filepath.Join(dir, diagnosis.FileManifest), append(manData, '\n'))
}

func buildExperiment(opt diagOpt) experiment.Report {
	phases := make([]experiment.Phase, len(opt.counts))
	phases[0] = experiment.BeforeWorkload
	for i := 1; i < len(opt.counts)-1; i++ {
		phases[i] = experiment.DuringWorkload
	}
	phases[len(phases)-1] = experiment.AfterWorkload
	r := experiment.Report{
		SchemaVersion: experiment.ReportSchemaVersion,
		Status:        experiment.StatusPassed,
		StartedAt:     fixtureT0,
		Config: experiment.ReportConfig{
			TargetURL: "http://127.0.0.1:8090/work", VUs: opt.vus, DurationSeconds: 4,
			MetricsURL: "http://127.0.0.1:8090/metrics", SampleIntervalSeconds: 1, MaxSamples: 1000,
			MaxTelemetryBytes: experiment.DefaultMaxTelemetryBytes, TimeoutSeconds: 64,
		},
		Workload: experiment.WorkloadOutcome{
			Result: &workload.Result{
				Runner: "k6", RunnerVersion: "2.3.0", TargetURL: "http://127.0.0.1:8090/work",
				VUs: opt.vus, ConfiguredDurationSeconds: 4, StartedAt: fixtureAt(0.1), FinishedAt: fixtureAt(4.1),
				Metrics: workload.Metrics{
					DurationSeconds: 4, HTTPRequests: 1000, RequestsPerSecond: 250,
					LatencyMs: workload.Latency{Avg: 1, Min: 0.1, P50: 1, P90: 2, P95: 3, Max: 4},
				},
			},
			Verdict: workload.Verdict{Passed: true},
		},
		Telemetry: experiment.TelemetryOutcome{
			Samples: []experiment.Sample{}, Errors: []experiment.ScrapeError{}, NotRetained: []experiment.NotRetained{},
		},
	}
	for i, count := range opt.counts {
		r.Telemetry.Samples = append(r.Telemetry.Samples, experiment.Sample{
			Sequence: i + 1, Phase: phases[i], ScrapedAt: fixtureAt(float64(i)), OffsetSeconds: float64(i),
			ScrapeDurationSeconds: 0.002, Families: fixtureFamilies(count, opt.fast),
		})
	}
	r.FinishedAt = fixtureAt(float64(len(opt.counts)-1) + 0.25)
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

func fixtureFamilies(count float64, fast bool) []telemetry.Family {
	value := telemetry.Float(count)
	return []telemetry.Family{
		{Name: analysis.DefaultRequestsCounter, Type: telemetry.Counter, Series: []telemetry.Series{{
			Labels: fixtureLabels(), Value: &value,
		}}},
		{Name: analysis.DefaultDurationHistogram, Type: telemetry.Histogram, Series: []telemetry.Series{
			fixtureHistogram(uint64(count), fast),
		}},
	}
}

func fixtureLabels() map[string]string {
	return map[string]string{"method": "GET", "route": "/work", "status": "200"}
}

func fixtureHistogram(count uint64, fast bool) telemetry.Series {
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
	return telemetry.Series{Labels: fixtureLabels(), Histogram: h}
}

func fixtureAt(s float64) time.Time {
	return fixtureT0.Add(time.Duration(math.Round(s * float64(time.Second))))
}

func writeBytes(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
