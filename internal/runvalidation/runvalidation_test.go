package runvalidation

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RajK158/ruptiq/internal/diagnosis"
	"github.com/RajK158/ruptiq/internal/experiment"
	"github.com/RajK158/ruptiq/internal/fixvalidation"
	"github.com/RajK158/ruptiq/internal/investigation"
)

func TestComparisonOutcomes(t *testing.T) {
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
			before, after, out := t.TempDir(), t.TempDir(), t.TempDir()
			tc.before.invOutcome = investigation.OutcomeInsufficientEvidence
			tc.after.invOutcome = investigation.OutcomeInsufficientEvidence
			writeDiagnosis(t, before, tc.before)
			writeDiagnosis(t, after, tc.after)
			beforeSnap, afterSnap := snap(t, before), snap(t, after)
			res, err := Run(Config{BeforeDir: before, AfterDir: after, OutputDir: out}, Inputs{})
			if err != nil {
				t.Fatal(err)
			}
			if res.Manifest.Outcome != tc.want || res.Report.Validation.Outcome != tc.want {
				t.Fatalf("outcome manifest %s report %s, want %s", res.Manifest.Outcome, res.Report.Validation.Outcome, tc.want)
			}
			if res.Manifest.Comparable != (tc.want != fixvalidation.OutcomeNotComparable) {
				t.Fatalf("comparable = %v", res.Manifest.Comparable)
			}
			if !sameSnap(beforeSnap, snap(t, before)) || !sameSnap(afterSnap, snap(t, after)) {
				t.Fatal("an input directory changed")
			}
			if _, err := os.Stat(filepath.Join(out, reservationFile)); !os.IsNotExist(err) {
				t.Fatal("reservation left behind")
			}
			blob := string(mustRead(t, out, FileManifest)) + string(mustRead(t, out, FileValidation))
			if strings.Contains(blob, before) || strings.Contains(blob, after) || strings.Contains(blob, out) {
				t.Fatal("report contains a filesystem path")
			}
			var buf bytes.Buffer
			if err := WriteSummary(&buf, res); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(buf.String(), tc.want) {
				t.Fatalf("summary missing outcome:\n%s", buf.String())
			}
		})
	}
}

func TestAbstentionIsCompared(t *testing.T) {
	before, after, out := t.TempDir(), t.TempDir(), t.TempDir()
	slow := []float64{0, 0, 1000, 2000, 2000}
	writeDiagnosis(t, before, diagOpt{counts: slow, invOutcome: investigation.OutcomeInsufficientEvidence})
	writeDiagnosis(t, after, diagOpt{counts: slow, fast: true, invOutcome: investigation.OutcomeChangesObserved})
	res, err := Run(Config{BeforeDir: before, AfterDir: after, OutputDir: out}, Inputs{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Manifest.Before.InvestigationOutcome != investigation.OutcomeInsufficientEvidence {
		t.Fatalf("before investigation = %s", res.Manifest.Before.InvestigationOutcome)
	}
	if res.Manifest.After.InvestigationOutcome != investigation.OutcomeChangesObserved {
		t.Fatalf("after investigation = %s", res.Manifest.After.InvestigationOutcome)
	}
	if res.Manifest.Outcome == "" {
		t.Fatal("missing comparison outcome")
	}
}

func TestFailedExperimentIsStillCompared(t *testing.T) {
	before, after, out := t.TempDir(), t.TempDir(), t.TempDir()
	writeDiagnosis(t, before, diagOpt{counts: []float64{0, 0, 1000, 2000, 2000}, status: experiment.StatusFailed})
	writeDiagnosis(t, after, diagOpt{counts: []float64{0, 0, 1000, 2000, 2000}, fast: true})
	res, err := Run(Config{BeforeDir: before, AfterDir: after, OutputDir: out}, Inputs{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Manifest.Before.ExperimentStatus != string(experiment.StatusFailed) {
		t.Fatalf("status = %s", res.Manifest.Before.ExperimentStatus)
	}
	if res.Manifest.Outcome != fixvalidation.OutcomeNoMaterialChange && res.Manifest.Outcome != fixvalidation.OutcomeImprovement {
		t.Fatalf("outcome = %s", res.Manifest.Outcome)
	}
	found := false
	for _, line := range res.Manifest.Limitations {
		if strings.Contains(line, "experiment status is failed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("limitations = %#v", res.Manifest.Limitations)
	}
}

func TestInputRefusals(t *testing.T) {
	valid := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		writeDiagnosis(t, dir, diagOpt{counts: []float64{0, 0, 1000, 2000, 2000}, fast: true})
		return dir
	}
	t.Run("missing manifest", func(t *testing.T) {
		dir := valid(t)
		if err := os.Remove(filepath.Join(dir, diagnosis.FileManifest)); err != nil {
			t.Fatal(err)
		}
		expectInput(t, dir, valid(t))
	})
	t.Run("bad schema", func(t *testing.T) {
		dir := valid(t)
		replace(t, filepath.Join(dir, diagnosis.FileManifest), `"schema_version": 1`, `"schema_version": 9`)
		expectInput(t, dir, valid(t))
	})
	t.Run("missing analysis", func(t *testing.T) {
		dir := valid(t)
		if err := os.Remove(filepath.Join(dir, diagnosis.FileAnalysis)); err != nil {
			t.Fatal(err)
		}
		expectInput(t, dir, valid(t))
	})
	t.Run("corrupt analysis", func(t *testing.T) {
		dir := valid(t)
		writeBytes(t, filepath.Join(dir, diagnosis.FileAnalysis), []byte("{"))
		expectInput(t, dir, valid(t))
	})
	t.Run("inconsistent artifact", func(t *testing.T) {
		dir := valid(t)
		replace(t, filepath.Join(dir, diagnosis.FileManifest), diagnosis.FileAnalysis, "other.json")
		expectInput(t, dir, valid(t))
	})
	t.Run("modified analysis", func(t *testing.T) {
		dir := valid(t)
		other := t.TempDir()
		writeDiagnosis(t, other, diagOpt{counts: []float64{0, 0, 2000, 4000, 4000}, fast: true})
		src, err := os.ReadFile(filepath.Join(other, diagnosis.FileAnalysis))
		if err != nil {
			t.Fatal(err)
		}
		writeBytes(t, filepath.Join(dir, diagnosis.FileAnalysis), src)
		_, err = Run(Config{BeforeDir: dir, AfterDir: valid(t), OutputDir: t.TempDir()}, Inputs{})
		var ierr *InputError
		if !errors.As(err, &ierr) || !strings.Contains(err.Error(), "does not match") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("active reservation", func(t *testing.T) {
		dir := valid(t)
		path := filepath.Join(dir, diagnoseReservation)
		writeBytes(t, path, []byte("pid 1\n"))
		old := time.Now().Add(-48 * time.Hour)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		_, err := Run(Config{BeforeDir: dir, AfterDir: valid(t), OutputDir: t.TempDir()}, Inputs{})
		var verr *ValidationError
		if !errors.As(err, &verr) {
			t.Fatalf("err = %v", err)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != "pid 1\n" {
			t.Fatalf("reservation = %q, err %v", got, err)
		}
	})
}

func TestOutputCollisionAndWriteFailure(t *testing.T) {
	before, after := t.TempDir(), t.TempDir()
	opt := diagOpt{counts: []float64{0, 0, 1000, 2000, 2000}, fast: true}
	writeDiagnosis(t, before, opt)
	writeDiagnosis(t, after, opt)
	beforeSnap, afterSnap := snap(t, before), snap(t, after)

	t.Run("collision", func(t *testing.T) {
		out := t.TempDir()
		path := filepath.Join(out, FileValidation)
		writeBytes(t, path, []byte("{}\n"))
		_, err := Run(Config{BeforeDir: before, AfterDir: after, OutputDir: out}, Inputs{})
		var verr *ValidationError
		if !errors.As(err, &verr) {
			t.Fatalf("err = %v", err)
		}
		if got := string(mustRead(t, out, FileValidation)); got != "{}\n" {
			t.Fatalf("validation.json = %s", got)
		}
		if !sameSnap(beforeSnap, snap(t, before)) || !sameSnap(afterSnap, snap(t, after)) {
			t.Fatal("inputs changed")
		}
	})
	t.Run("write failure", func(t *testing.T) {
		out := t.TempDir()
		_, err := Run(Config{BeforeDir: before, AfterDir: after, OutputDir: out}, Inputs{
			write: func(string, string, any) error { return errors.New("disk full") },
		})
		if err == nil || !strings.Contains(err.Error(), "disk full") {
			t.Fatalf("err = %v", err)
		}
		if _, statErr := os.Stat(filepath.Join(out, FileValidation)); !os.IsNotExist(statErr) {
			t.Fatal("partial validation.json was left")
		}
		if _, statErr := os.Stat(filepath.Join(out, reservationFile)); !os.IsNotExist(statErr) {
			t.Fatal("reservation left after write failure")
		}
		if !sameSnap(beforeSnap, snap(t, before)) || !sameSnap(afterSnap, snap(t, after)) {
			t.Fatal("inputs changed")
		}
	})
	t.Run("stale output reservation", func(t *testing.T) {
		out := t.TempDir()
		path := filepath.Join(out, reservationFile)
		writeBytes(t, path, []byte("pid 9\n"))
		_, err := Run(Config{BeforeDir: before, AfterDir: after, OutputDir: out}, Inputs{})
		var verr *ValidationError
		if !errors.As(err, &verr) {
			t.Fatalf("err = %v", err)
		}
		if got := string(mustRead(t, out, reservationFile)); got != "pid 9\n" {
			t.Fatalf("reservation = %s", got)
		}
	})
}

func TestConcurrentRunIsRejected(t *testing.T) {
	before, after, out := t.TempDir(), t.TempDir(), t.TempDir()
	opt := diagOpt{counts: []float64{0, 0, 1000, 2000, 2000}, fast: true}
	writeDiagnosis(t, before, opt)
	writeDiagnosis(t, after, opt)
	entered := make(chan struct{})
	release := make(chan struct{})
	var unblock sync.Once
	stop := func() { unblock.Do(func() { close(release) }) }
	defer stop()
	errCh := make(chan error, 1)
	go func() {
		_, err := Run(Config{BeforeDir: before, AfterDir: after, OutputDir: out}, Inputs{
			write: func(dir, name string, v any) error {
				if name == FileValidation {
					close(entered)
					<-release
				}
				return writeJSONFile(dir, name, v)
			},
		})
		errCh <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first run did not reach the output write")
	}
	secondWrote := false
	_, err := Run(Config{BeforeDir: before, AfterDir: after, OutputDir: out}, Inputs{
		write: func(string, string, any) error {
			secondWrote = true
			return errors.New("second write")
		},
	})
	var verr *ValidationError
	if secondWrote || !errors.As(err, &verr) || !strings.Contains(err.Error(), reservationFile) {
		t.Fatalf("second err=%v wrote=%v", err, secondWrote)
	}
	stop()
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, reservationFile)); !os.IsNotExist(err) {
		t.Fatal("reservation left after success")
	}
}

func TestEvidencePointersResolve(t *testing.T) {
	before, after, out := t.TempDir(), t.TempDir(), t.TempDir()
	writeDiagnosis(t, before, diagOpt{counts: []float64{0, 0, 1000, 2000, 2000}})
	writeDiagnosis(t, after, diagOpt{counts: []float64{0, 0, 2000, 4000, 4000}, fast: true})
	res, err := Run(Config{BeforeDir: before, AfterDir: after, OutputDir: out}, Inputs{})
	if err != nil {
		t.Fatal(err)
	}
	docs := map[string]any{
		"before": decodeJSON(t, mustRead(t, before, diagnosis.FileAnalysis)),
		"after":  decodeJSON(t, mustRead(t, after, diagnosis.FileAnalysis)),
	}
	n := 0
	for _, route := range res.Report.Validation.Routes {
		for _, ref := range route.Evidence {
			if ref.Role != fixvalidation.RoleMeasurement {
				continue
			}
			n++
			doc, ok := docs[ref.Side]
			if !ok {
				t.Fatalf("side %s", ref.Side)
			}
			got := resolve(t, doc, ref.Pointer)
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
		t.Fatal("no measurement pointers")
	}
	if res.Manifest.Before.AnalysisSHA256 == "" || res.Manifest.Before.AnalysisSHA256 == res.Manifest.After.AnalysisSHA256 {
		t.Fatal("analysis hashes were not recorded")
	}
}

func TestDirectoryAliasesCannotBypassIsolation(t *testing.T) {
	slow := diagOpt{counts: []float64{0, 0, 1000, 2000, 2000}}
	fast := diagOpt{counts: []float64{0, 0, 2000, 4000, 4000}, fast: true}
	before, after := t.TempDir(), t.TempDir()
	writeDiagnosis(t, before, slow)
	writeDiagnosis(t, after, fast)
	beforeSnap := snap(t, before)

	t.Run("same directory", func(t *testing.T) {
		_, err := Run(Config{BeforeDir: before, AfterDir: before, OutputDir: t.TempDir()}, Inputs{})
		expectValidation(t, err, "must be different")
		if !sameSnap(beforeSnap, snap(t, before)) {
			t.Fatal("input changed")
		}
	})
	t.Run("junction to the same input", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "before-alias")
		linkDir(t, before, link)
		_, err := Run(Config{BeforeDir: before, AfterDir: link, OutputDir: t.TempDir()}, Inputs{})
		expectValidation(t, err, "must be different")
		if !sameSnap(beforeSnap, snap(t, before)) {
			t.Fatal("input changed")
		}
	})
	t.Run("output is an input alias", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "out-alias")
		linkDir(t, before, link)
		_, err := Run(Config{BeforeDir: before, AfterDir: after, OutputDir: link}, Inputs{})
		expectValidation(t, err, "output directory")
		if !sameSnap(beforeSnap, snap(t, before)) {
			t.Fatal("input changed")
		}
	})
	t.Run("output is inside an input through an alias", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "input-alias")
		linkDir(t, before, link)
		_, err := Run(Config{BeforeDir: before, AfterDir: after, OutputDir: filepath.Join(link, "nested")}, Inputs{})
		expectValidation(t, err, "output directory")
		if _, statErr := os.Stat(filepath.Join(before, "nested")); !os.IsNotExist(statErr) {
			t.Fatal("nested output was created in the input")
		}
		if !sameSnap(beforeSnap, snap(t, before)) {
			t.Fatal("input changed")
		}
	})
	t.Run("output alias of an existing result", func(t *testing.T) {
		real := t.TempDir()
		writeBytes(t, filepath.Join(real, FileValidation), []byte("keep\n"))
		link := filepath.Join(t.TempDir(), "result-alias")
		linkDir(t, real, link)
		_, err := Run(Config{BeforeDir: before, AfterDir: after, OutputDir: link}, Inputs{})
		expectValidation(t, err, "already contains")
		if got := string(mustRead(t, real, FileValidation)); got != "keep\n" {
			t.Fatalf("existing validation.json = %q", got)
		}
	})
	t.Run("reservation through an alias", func(t *testing.T) {
		real := t.TempDir()
		writeBytes(t, filepath.Join(real, reservationFile), []byte("pid 4\n"))
		link := filepath.Join(t.TempDir(), "reserved-alias")
		linkDir(t, real, link)
		_, err := Run(Config{BeforeDir: before, AfterDir: after, OutputDir: link}, Inputs{})
		expectValidation(t, err, reservationFile)
		if got := string(mustRead(t, real, reservationFile)); got != "pid 4\n" {
			t.Fatalf("reservation = %q", got)
		}
	})
}

func TestDuplicateAnalysisIsDisclosed(t *testing.T) {
	before, after, out := t.TempDir(), t.TempDir(), t.TempDir()
	opt := diagOpt{counts: []float64{0, 0, 1000, 2000, 2000}, fast: true}
	writeDiagnosis(t, before, opt)
	writeDiagnosis(t, after, opt)
	res, err := Run(Config{BeforeDir: before, AfterDir: after, OutputDir: out}, Inputs{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Manifest.Before.AnalysisSHA256 != res.Manifest.After.AnalysisSHA256 {
		t.Fatal("fixture analyses differ")
	}
	if res.Manifest.Outcome != fixvalidation.OutcomeNoMaterialChange {
		t.Fatalf("outcome = %s", res.Manifest.Outcome)
	}
	if !hasLimitation(res.Manifest, "same SHA-256") {
		t.Fatalf("limitations = %#v", res.Manifest.Limitations)
	}
	var buf bytes.Buffer
	if err := WriteSummary(&buf, res); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "not evidence of a change") || !strings.Contains(buf.String(), res.Manifest.Outcome) {
		t.Fatalf("summary =\n%s", buf.String())
	}
}

func TestCLIIndependentImprovementAndReversal(t *testing.T) {
	before, after := t.TempDir(), t.TempDir()
	writeDiagnosis(t, before, diagOpt{counts: []float64{0, 0, 1000, 2000, 2000}})
	writeDiagnosis(t, after, diagOpt{counts: []float64{0, 0, 2000, 4000, 4000}, fast: true})
	beforeSnap, afterSnap := snap(t, before), snap(t, after)

	forward := filepath.Join(t.TempDir(), "forward")
	runCLI(t, before, after, forward)
	checkPair(t, before, after, forward, fixvalidation.OutcomeImprovement)

	reverse := filepath.Join(t.TempDir(), "reverse")
	runCLI(t, after, before, reverse)
	checkPair(t, after, before, reverse, fixvalidation.OutcomeRegression)

	if !sameSnap(beforeSnap, snap(t, before)) || !sameSnap(afterSnap, snap(t, after)) {
		t.Fatal("CLI changed an input directory")
	}
}

func runCLI(t *testing.T, before, after, out string) {
	t.Helper()
	cmd := exec.Command("go", "run", "./cmd/ruptiq-validate-runs",
		"-before-dir", before, "-after-dir", after, "-out-dir", out)
	cmd.Dir = filepath.Clean(filepath.Join("..", ".."))
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("cli: %v\n%s\n%s", err, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "Outcome:") {
		t.Fatalf("stdout =\n%s", stdout.String())
	}
}

func checkPair(t *testing.T, before, after, out, want string) {
	t.Helper()
	var manifest Manifest
	if err := json.Unmarshal(mustRead(t, out, FileManifest), &manifest); err != nil {
		t.Fatal(err)
	}
	var report fixvalidation.Report
	if err := json.Unmarshal(mustRead(t, out, FileValidation), &report); err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != SchemaVersion || manifest.Outcome != want || report.SchemaVersion != fixvalidation.SchemaVersion || report.Validation == nil || report.Validation.Outcome != want {
		t.Fatalf("manifest %d %s, report %#v", manifest.SchemaVersion, manifest.Outcome, report.Validation)
	}
	t.Logf("outcome %s comparable %v before %s after %s", manifest.Outcome, manifest.Comparable, manifest.Before.AnalysisSHA256, manifest.After.AnalysisSHA256)
	if manifest.Before.AnalysisSHA256 == "" || manifest.Before.AnalysisSHA256 == manifest.After.AnalysisSHA256 {
		t.Fatal("sources were not distinguished")
	}
	docs := map[string]any{
		"before": decodeJSON(t, mustRead(t, before, diagnosis.FileAnalysis)),
		"after":  decodeJSON(t, mustRead(t, after, diagnosis.FileAnalysis)),
	}
	n := 0
	for _, route := range report.Validation.Routes {
		for _, ref := range route.Evidence {
			if ref.Role != fixvalidation.RoleMeasurement {
				continue
			}
			n++
			got := resolve(t, docs[ref.Side], ref.Pointer)
			if got == nil {
				t.Fatalf("unresolved %s %s", ref.Side, ref.Pointer)
			}
		}
	}
	if n == 0 {
		t.Fatal("no evidence pointers")
	}
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

func expectInput(t *testing.T, before, after string) {
	t.Helper()
	_, err := Run(Config{BeforeDir: before, AfterDir: after, OutputDir: t.TempDir()}, Inputs{})
	var ierr *InputError
	if !errors.As(err, &ierr) {
		t.Fatalf("err = %v", err)
	}
}

func replace(t *testing.T, path, old, new string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(old)) {
		t.Fatalf("pattern %q not in %s", old, path)
	}
	if err := os.WriteFile(path, bytes.Replace(b, []byte(old), []byte(new), 1), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, dir, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
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
			t.Fatalf("pointer %s at %T", pointer, cur)
		}
	}
	return cur
}
