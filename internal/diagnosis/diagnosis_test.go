package diagnosis

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RajK158/ruptiq/internal/analysis"
	"github.com/RajK158/ruptiq/internal/experiment"
	"github.com/RajK158/ruptiq/internal/investigation"
	"github.com/RajK158/ruptiq/internal/telemetry"
	"github.com/RajK158/ruptiq/internal/workload"
)

const secret = "s3cret-token"

func TestSuccessfulDiagnosis(t *testing.T) {
	dir := t.TempDir()
	reader := &scriptReader{}
	res, err := Run(context.Background(), passedConfig(dir), Inputs{
		Runner: &gateRunner{reader: reader, want: 9},
		Reader: reader,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Manifest.Outcome != OutcomeCompleted {
		t.Fatalf("outcome = %s, want completed", res.Manifest.Outcome)
	}
	if res.Experiment == nil || res.Experiment.Status != experiment.StatusPassed {
		t.Fatalf("experiment status = %v", statusOf(res.Experiment))
	}
	if res.Analysis == nil || res.Investigation == nil {
		t.Fatal("analysis or investigation missing")
	}
	switch res.Investigation.Outcome {
	case investigation.OutcomeChangesObserved, investigation.OutcomeNoMaterialChange, investigation.OutcomeInsufficientEvidence:
	default:
		t.Fatalf("investigation outcome = %q", res.Investigation.Outcome)
	}
	for _, f := range res.Investigation.Findings {
		if f.Type == investigation.FindingInsufficientEvidence || f.Designation == investigation.DesignationAbstention {
			continue
		}
		if f.Designation != investigation.DesignationObservation && f.Designation != investigation.DesignationHypothesis {
			t.Errorf("finding %s designation %s", f.ID, f.Designation)
		}
	}
	assertFiles(t, dir, FileExperiment, FileAnalysis, FileInvestigation, FileManifest)
	assertSchemas(t, dir)
	assertPointers(t, dir, res.Investigation, false)
	assertManifest(t, dir, res, OutcomeCompleted)
	assertNoTemps(t, dir)
	if strings.Contains(mustRead(t, dir, FileManifest), secret) {
		t.Fatal("manifest contains a secret")
	}
}

func TestAbstentionIsNotAFinding(t *testing.T) {
	dir := t.TempDir()
	res, err := Run(context.Background(), passedConfig(dir), Inputs{
		Runner: gateRunner{},
		Reader: &scriptReader{},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Manifest.Outcome != OutcomeCompleted {
		t.Fatalf("outcome = %s, want completed for an abstention", res.Manifest.Outcome)
	}
	if res.Experiment.Status != experiment.StatusIncomplete {
		t.Fatalf("status = %s, want incomplete (no sample during the workload)", res.Experiment.Status)
	}
	if res.Investigation.Outcome != investigation.OutcomeInsufficientEvidence {
		t.Fatalf("outcome = %s, want insufficient_evidence", res.Investigation.Outcome)
	}
	for _, f := range res.Investigation.Findings {
		if f.Type != investigation.FindingInsufficientEvidence || f.Designation != investigation.DesignationAbstention {
			t.Errorf("abstention produced %s/%s", f.Type, f.Designation)
		}
	}
	var buf bytes.Buffer
	if err := WriteSummary(&buf, res); err != nil {
		t.Fatal(err)
	}
	text := buf.String()
	if !strings.Contains(text, "abstained") {
		t.Fatalf("summary does not name the abstention:\n%s", text)
	}
	if strings.Contains(text, "root cause is") {
		t.Fatalf("summary states a root cause:\n%s", text)
	}
	assertPointers(t, dir, res.Investigation, true)
}

func TestMissingTelemetryAbstains(t *testing.T) {
	dir := t.TempDir()
	res, err := Run(context.Background(), passedConfig(dir), Inputs{
		Runner: gateRunner{},
		Reader: &scriptReader{err: errors.New("http://user:" + secret + "@127.0.0.1:8090/metrics?" + secret + " refused")},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Experiment.Telemetry.Completeness != experiment.None {
		t.Fatalf("completeness = %s, want none", res.Experiment.Telemetry.Completeness)
	}
	if res.Investigation == nil || res.Investigation.Outcome != investigation.OutcomeInsufficientEvidence {
		t.Fatalf("investigation = %#v", res.Investigation)
	}
	for _, f := range res.Investigation.Findings {
		if f.Designation != investigation.DesignationAbstention {
			t.Errorf("designation = %s", f.Designation)
		}
	}
	blob := allFiles(t, dir)
	if strings.Contains(blob, secret) {
		t.Fatal("saved reports contain the metrics secret")
	}
}

func TestWorkloadExecutionFailure(t *testing.T) {
	dir := t.TempDir()
	res, err := Run(context.Background(), passedConfig(dir), Inputs{
		Runner: gateRunner{err: errors.New("k6: http://user:" + secret + "@127.0.0.1:8090/work?token=" + secret)},
		Reader: &scriptReader{},
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if res.Manifest.Outcome != OutcomeFailed {
		t.Fatalf("outcome = %s", res.Manifest.Outcome)
	}
	if res.Experiment == nil || res.Experiment.Status != experiment.StatusError {
		t.Fatalf("status = %v", statusOf(res.Experiment))
	}
	assertFiles(t, dir, FileExperiment, FileManifest)
	assertAbsent(t, dir, FileAnalysis, FileInvestigation)
	if res.Manifest.Stages[1].Status != StatusSkipped || res.Manifest.Stages[2].Status != StatusSkipped {
		t.Fatalf("stages = %#v", res.Manifest.Stages)
	}
	if strings.Contains(allFiles(t, dir), secret) || strings.Contains(err.Error(), secret) {
		t.Fatal("workload secret leaked")
	}
}

func TestCancellationSkipsAnalysis(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := Run(ctx, passedConfig(dir), Inputs{Runner: &gateRunner{block: true}, Reader: &scriptReader{}})
	if err == nil {
		t.Fatal("expected an error")
	}
	if res.Experiment == nil || res.Experiment.Status != experiment.StatusCancelled {
		t.Fatalf("status = %v", statusOf(res.Experiment))
	}
	assertFiles(t, dir, FileExperiment, FileManifest)
	assertAbsent(t, dir, FileAnalysis, FileInvestigation)
	if res.Manifest.Outcome != OutcomeFailed {
		t.Fatalf("outcome = %s", res.Manifest.Outcome)
	}
	if res.Manifest.Stages[1].Status != StatusSkipped {
		t.Fatalf("analysis stage = %#v", res.Manifest.Stages[1])
	}
}

func TestTimeoutSkipsAnalysis(t *testing.T) {
	dir := t.TempDir()
	cfg := passedConfig(dir)
	cfg.Experiment.Workload.Duration = time.Second
	cfg.Experiment.Timeout = time.Second + 200*time.Millisecond
	cfg.Experiment.Interval = 100 * time.Millisecond
	start := time.Now()
	res, err := Run(context.Background(), cfg, Inputs{Runner: &gateRunner{block: true}, Reader: &scriptReader{}})
	if err == nil {
		t.Fatal("expected an error")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("timeout took %s", time.Since(start))
	}
	if res.Experiment == nil || res.Experiment.Status != experiment.StatusTimedOut {
		t.Fatalf("status = %v", statusOf(res.Experiment))
	}
	assertAbsent(t, dir, FileAnalysis, FileInvestigation)
	assertFiles(t, dir, FileExperiment, FileManifest)
}

func TestInvalidConfig(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		_, err := Run(context.Background(), Config{}, Inputs{Runner: gateRunner{}, Reader: &scriptReader{}})
		var verr *ValidationError
		if !errors.As(err, &verr) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("bad url and thresholds", func(t *testing.T) {
		cfg := passedConfig(t.TempDir())
		cfg.Experiment.Workload.TargetURL = "ftp://example"
		cfg.Analysis = analysis.DefaultConfig()
		cfg.Analysis.RequestsCounter = cfg.Analysis.DurationHistogram
		cfg.Investigation = investigation.DefaultConfig()
		cfg.Investigation.MinWindows = 0
		_, err := Run(context.Background(), cfg, Inputs{Runner: gateRunner{}, Reader: &scriptReader{}})
		var verr *ValidationError
		if !errors.As(err, &verr) {
			t.Fatal(err)
		}
		text := err.Error()
		for _, part := range []string{"http://", "different families", "min windows"} {
			if !strings.Contains(text, part) {
				t.Errorf("error %q missing %q", text, part)
			}
		}
	})
	t.Run("nil runner", func(t *testing.T) {
		_, err := Run(context.Background(), passedConfig(t.TempDir()), Inputs{Reader: &scriptReader{}})
		var verr *ValidationError
		if !errors.As(err, &verr) {
			t.Fatal(err)
		}
	})
}

func TestOutputCollision(t *testing.T) {
	dir := t.TempDir()
	original := []byte("{\n  \"keep\": true\n}\n")
	if err := os.WriteFile(filepath.Join(dir, FileExperiment), original, 0o644); err != nil {
		t.Fatal(err)
	}
	reader := &scriptReader{}
	_, err := Run(context.Background(), passedConfig(dir), Inputs{Runner: &gateRunner{reader: reader, want: 1}, Reader: reader})
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("err = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, FileExperiment))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("existing report changed: %s", got)
	}
	if reader.calls() != 0 {
		t.Fatalf("runner/reader started despite a collision (%d reads)", reader.calls())
	}
	assertAbsent(t, dir, FileAnalysis, FileInvestigation, FileManifest)

	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Run(context.Background(), passedConfig(file), Inputs{Runner: gateRunner{}, Reader: &scriptReader{}})
	if !errors.As(err, &verr) {
		t.Fatalf("file path err = %v", err)
	}
}

func TestAtomicWriteAndFailedStage(t *testing.T) {
	dir := t.TempDir()
	payload := map[string]string{"ok": "yes"}
	if err := writeJSONFile(dir, FileManifest, payload); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, FileManifest))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(got, []byte("\n")) || !json.Valid(got) {
		t.Fatalf("written file is not complete JSON: %s", got)
	}
	kept := append([]byte(nil), got...)
	if err := writeJSONFile(dir, FileManifest, map[string]string{"ok": "no"}); err == nil {
		t.Fatal("overwrite succeeded")
	}
	again, err := os.ReadFile(filepath.Join(dir, FileManifest))
	if err != nil || !bytes.Equal(again, kept) {
		t.Fatalf("existing file changed: %s", again)
	}
	assertNoTemps(t, dir)

	if _, err := artifactPath(dir, filepath.Join("..", "escape.json")); err == nil {
		t.Fatal("escaping artifact name was accepted")
	}

	out := t.TempDir()
	res, err := Run(context.Background(), passedConfig(out), Inputs{
		Runner: gateRunner{},
		Reader: &scriptReader{},
		write: func(dir, name string, v any) error {
			if name == FileAnalysis {
				return errors.New("disk full")
			}
			return writeJSONFile(dir, name, v)
		},
	})
	if err == nil {
		t.Fatal("expected analysis write to fail")
	}
	assertFiles(t, out, FileExperiment, FileManifest)
	assertAbsent(t, out, FileAnalysis, FileInvestigation)
	exp, err := os.ReadFile(filepath.Join(out, FileExperiment))
	if err != nil || !json.Valid(exp) {
		t.Fatalf("experiment artifact is not valid JSON: %v", err)
	}
	if _, err := analysis.DecodeExperiment(exp); err != nil {
		t.Fatal(err)
	}
	if res.Manifest.Stages[0].Status != StatusSucceeded || res.Manifest.Stages[1].Status != StatusFailed || res.Manifest.Stages[2].Status != StatusSkipped {
		t.Fatalf("stages = %#v", res.Manifest.Stages)
	}
	if res.Manifest.Outcome != OutcomeFailed {
		t.Fatalf("outcome = %s", res.Manifest.Outcome)
	}
	assertNoTemps(t, out)
}

func TestSecretsRedacted(t *testing.T) {
	dir := t.TempDir()
	cfg := passedConfig(dir)
	cfg.Experiment.Workload.TargetURL = "http://user:" + secret + "@127.0.0.1:8090/work?token=" + secret
	cfg.Experiment.MetricsURL = "http://user:" + secret + "@127.0.0.1:8090/metrics?token=" + secret
	res, err := Run(context.Background(), cfg, Inputs{
		Runner: gateRunner{err: errors.New("dial http://user:" + secret + "@127.0.0.1:8090/work?token=" + secret)},
		Reader: &scriptReader{},
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	var buf bytes.Buffer
	if err := WriteSummary(&buf, res); err != nil {
		t.Fatal(err)
	}
	blob := allFiles(t, dir) + buf.String() + err.Error()
	if strings.Contains(blob, secret) || strings.Contains(blob, "user:"+secret) {
		t.Fatal("secret leaked into the diagnosis output")
	}
	if !strings.Contains(res.Manifest.TargetURL, "xxxxx") || !strings.Contains(res.Manifest.MetricsURL, "xxxxx") {
		t.Fatalf("urls = %s %s", res.Manifest.TargetURL, res.Manifest.MetricsURL)
	}
}

func TestManifestIsRelativeAndVersioned(t *testing.T) {
	dir := t.TempDir()
	res, err := Run(context.Background(), passedConfig(dir), Inputs{Runner: gateRunner{}, Reader: &scriptReader{}})
	if err != nil {
		t.Fatal(err)
	}
	assertManifest(t, dir, res, OutcomeCompleted)
	raw := mustRead(t, dir, FileManifest)
	if strings.Contains(raw, dir) {
		t.Fatal("manifest contains the output directory path")
	}
	for _, name := range res.Manifest.Artifacts {
		if name != filepath.Base(name) || strings.Contains(name, "/") || strings.Contains(name, "\\") {
			t.Errorf("artifact path %q is not a file name", name)
		}
	}
	if res.Manifest.Versions != currentVersions() || res.Manifest.SchemaVersion != SchemaVersion {
		t.Fatalf("versions = %#v", res.Manifest.Versions)
	}
}

func passedConfig(dir string) Config {
	return Config{
		OutputDir: dir,
		Experiment: experiment.Config{
			Workload:          workload.Spec{TargetURL: "http://127.0.0.1:8090/work", VUs: 2, Duration: 30 * time.Second},
			MetricsURL:        "http://127.0.0.1:8090/metrics",
			Interval:          150 * time.Millisecond,
			Timeout:           45 * time.Second,
			MaxFailureRate:    0,
			MaxSamples:        1000,
			MaxTelemetryBytes: experiment.DefaultMaxTelemetryBytes,
		},
	}
}

type scriptReader struct {
	mu   sync.Mutex
	n    int
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
			break
		}
		r.mu.Unlock()
		time.Sleep(time.Millisecond)
		r.mu.Lock()
	}
	r.n++
	n := r.n
	at := r.last
	r.mu.Unlock()
	snap := snapshot(n)
	snap.ScrapedAt = at
	return snap, nil
}

func (r *scriptReader) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n
}

func snapshot(n int) telemetry.Snapshot {
	var fast, slow uint64
	for step := 2; step <= n; step++ {
		if step <= 6 {
			fast += 500
		} else {
			slow += 500
		}
	}
	req := telemetry.Float(fast + slow)
	return telemetry.Snapshot{
		Endpoint:              "http://127.0.0.1:8090/metrics",
		ScrapedAt:             time.Now().UTC(),
		ScrapeDurationSeconds: 0.001,
		Families: []telemetry.Family{
			{
				Name: "inventory_http_request_duration_seconds", Type: telemetry.Histogram,
				Series: []telemetry.Series{{Labels: map[string]string{"method": "GET", "route": "/work"}, Histogram: hist(fast, slow)}},
			},
			{
				Name: "inventory_http_requests_total", Type: telemetry.Counter,
				Series: []telemetry.Series{{Labels: map[string]string{"method": "GET", "route": "/work", "status": "200"}, Value: &req}},
			},
		},
	}
}

func hist(fast, slow uint64) *telemetry.HistogramValue {
	count := fast + slow
	h := &telemetry.HistogramValue{Count: count, Sum: telemetry.Float(float64(fast)*0.001 + float64(slow)*0.04)}
	bounds := []float64{0.005, 0.01, 0.025, 0.05, 0.1}
	cum := []uint64{fast, fast, fast, count, count}
	for i, b := range bounds {
		h.Buckets = append(h.Buckets, telemetry.Bucket{UpperBound: telemetry.Float(b), CumulativeCount: cum[i]})
	}
	h.Buckets = append(h.Buckets, telemetry.Bucket{UpperBound: telemetry.Float(math.Inf(1)), CumulativeCount: count})
	return h
}

type gateRunner struct {
	reader *scriptReader
	want   int
	err    error
	block  bool
}

func (g gateRunner) Run(ctx context.Context, spec workload.Spec) (workload.Result, error) {
	if g.err != nil {
		return workload.Result{}, g.err
	}
	if g.block {
		<-ctx.Done()
		return workload.Result{}, ctx.Err()
	}
	if g.want > 0 {
		deadline := time.NewTimer(10 * time.Second)
		defer deadline.Stop()
		for g.reader.calls() < g.want {
			select {
			case <-ctx.Done():
				return workload.Result{}, ctx.Err()
			case <-deadline.C:
				return workload.Result{}, errors.New("timed out waiting for telemetry")
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
	return workload.Result{
		Runner: "fake", RunnerVersion: "0",
		VUs: spec.VUs, ConfiguredDurationSeconds: spec.Duration.Seconds(),
		StartedAt: time.Now().Add(-time.Second).UTC(), FinishedAt: time.Now().UTC(),
		Metrics: workload.Metrics{
			DurationSeconds: 1, HTTPRequests: 1000, RequestsPerSecond: 1000,
			LatencyMs: workload.Latency{Avg: 1, Min: 0.1, P50: 1, P90: 2, P95: 3, Max: 4},
		},
	}, nil
}

func assertFiles(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}
}

func assertAbsent(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s exists (%v)", name, err)
		}
	}
}

func assertNoTemps(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".ruptiq-diagnose-") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
}

func assertSchemas(t *testing.T, dir string) {
	t.Helper()
	exp := []byte(mustRead(t, dir, FileExperiment))
	if _, err := analysis.DecodeExperiment(exp); err != nil {
		t.Fatal(err)
	}
	an := []byte(mustRead(t, dir, FileAnalysis))
	if _, err := investigation.DecodeAnalysis(an); err != nil {
		t.Fatal(err)
	}
	var head struct {
		SchemaVersion int `json:"schema_version"`
	}
	for _, name := range []string{FileExperiment, FileAnalysis, FileInvestigation, FileManifest} {
		if err := json.Unmarshal([]byte(mustRead(t, dir, name)), &head); err != nil {
			t.Fatal(err)
		}
		if head.SchemaVersion != 1 {
			t.Errorf("%s schema_version = %d", name, head.SchemaVersion)
		}
	}
}

func assertPointers(t *testing.T, dir string, inv *investigation.Investigation, required bool) {
	t.Helper()
	doc := decodeJSON(t, []byte(mustRead(t, dir, FileAnalysis)))
	n := 0
	for _, f := range inv.Findings {
		for _, ref := range f.Evidence {
			n++
			resolve(t, doc, ref.Pointer)
		}
	}
	if required && n == 0 {
		t.Fatal("investigation has no evidence pointers")
	}
}

func assertManifest(t *testing.T, dir string, res Result, outcome string) {
	t.Helper()
	var got Manifest
	if err := json.Unmarshal([]byte(mustRead(t, dir, FileManifest)), &got); err != nil {
		t.Fatal(err)
	}
	if got.Outcome != outcome || got.SchemaVersion != SchemaVersion {
		t.Fatalf("saved manifest outcome %s schema %d", got.Outcome, got.SchemaVersion)
	}
	if len(got.Stages) != 3 || len(got.Limitations) == 0 || len(got.Artifacts) == 0 {
		t.Fatalf("manifest = %#v", got)
	}
	for _, name := range got.Artifacts {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("manifest lists missing %s", name)
		}
	}
	if got.TargetURL != res.Manifest.TargetURL || strings.Contains(got.TargetURL, secret) {
		t.Errorf("target %q", got.TargetURL)
	}
}

func allFiles(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b.WriteString(mustRead(t, dir, e.Name()))
	}
	return b.String()
}

func mustRead(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func statusOf(r *experiment.Report) experiment.Status {
	if r == nil {
		return ""
	}
	return r.Status
}

func decodeJSON(t *testing.T, data []byte) any {
	t.Helper()
	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func resolve(t *testing.T, doc any, pointer string) any {
	t.Helper()
	if !strings.HasPrefix(pointer, "/") {
		t.Fatalf("pointer %q does not start with /", pointer)
	}
	cur := doc
	for _, tok := range strings.Split(pointer[1:], "/") {
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		switch v := cur.(type) {
		case map[string]any:
			next, ok := v[tok]
			if !ok {
				t.Fatalf("pointer %q: no key %q", pointer, tok)
			}
			cur = next
		case []any:
			i := 0
			for _, r := range tok {
				if r < '0' || r > '9' {
					t.Fatalf("pointer %q: bad index %q", pointer, tok)
				}
				i = i*10 + int(r-'0')
			}
			if i < 0 || i >= len(v) {
				t.Fatalf("pointer %q: index %d out of range", pointer, i)
			}
			cur = v[i]
		default:
			t.Fatalf("pointer %q: cannot descend into %T", pointer, cur)
		}
	}
	return cur
}
