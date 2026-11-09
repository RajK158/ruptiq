package main

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/RajK158/ruptiq/internal/fixvalidation"
)

var analysisFixture = filepath.Join("..", "..", "internal", "analysis", "testdata", "inventory_analysis.golden.json")

type outcome struct {
	code           int
	stdout, stderr string
}

func runCmd(args ...string) outcome {
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return outcome{code, stdout.String(), stderr.String()}
}

func readReport(t *testing.T, path string) fixvalidation.Report {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var r fixvalidation.Report
	if err := dec.Decode(&r); err != nil {
		t.Fatalf("validation JSON: %v", err)
	}
	if _, err := dec.Token(); err != nil && err.Error() != "EOF" {
		t.Fatalf("trailing data: %v", err)
	}
	return r
}

func writeFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func resolve(t *testing.T, doc any, pointer string) map[string]any {
	t.Helper()
	cur := doc
	for _, tok := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		switch v := cur.(type) {
		case map[string]any:
			next, ok := v[tok]
			if !ok {
				t.Fatalf("pointer %q: no key %q", pointer, tok)
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(v) {
				t.Fatalf("pointer %q: bad index %q", pointer, tok)
			}
			cur = v[i]
		default:
			t.Fatalf("pointer %q: %T", pointer, cur)
		}
	}
	m, ok := cur.(map[string]any)
	if !ok {
		t.Fatalf("pointer %q is %T", pointer, cur)
	}
	return m
}

func TestGoldenAnalysesAreStable(t *testing.T) {
	out := filepath.Join(t.TempDir(), "validation.json")
	o := runCmd("-before", analysisFixture, "-after", analysisFixture, "-out", out)
	if o.code != exitCompared || o.stderr != "" {
		t.Fatalf("exit %d, stderr %q", o.code, o.stderr)
	}
	for _, want := range []string{
		"Outcome: no_material_change",
		"GET /inventory/{sku}  no_material_change",
		"POST /reservations  insufficient_evidence",
		"Client-side k6 measurements",
		"not a statistical significance test",
		"Validation written to " + out,
	} {
		if !strings.Contains(o.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, o.stdout)
		}
	}
	r := readReport(t, out)
	if r.SchemaVersion != fixvalidation.SchemaVersion || r.Error != "" || r.Validation == nil {
		t.Fatalf("report error %q validation nil %v", r.Error, r.Validation == nil)
	}
	v := r.Validation
	if v.Outcome != fixvalidation.OutcomeNoMaterialChange || !v.Comparability.Comparable {
		t.Fatalf("outcome %s comparable %v", v.Outcome, v.Comparability.Comparable)
	}
	if len(v.Before.EligibleWindows) != 2 || v.Before.EligibleWindows[0] != 2 || v.Before.EligibleWindows[1] != 3 {
		t.Fatalf("eligible %+v", v.Before.EligibleWindows)
	}
	var get, post fixvalidation.RouteComparison
	for _, route := range v.Routes {
		switch route.Route {
		case "/inventory/{sku}":
			get = route
		case "/reservations":
			post = route
		}
	}
	// Windows 2 and 3: 3000 + 3020 requests over 1.008s + 1.490s.
	if get.Throughput == nil || get.Throughput.Status != fixvalidation.StatusNoMaterialChange {
		t.Fatalf("GET throughput %+v", get.Throughput)
	}
	const wantRequests = 6020
	const wantElapsed = 1.008 + 1.49
	if get.Throughput.Before.Requests != wantRequests || math.Abs(get.Throughput.Before.ElapsedSeconds-wantElapsed) > 1e-9 {
		t.Fatalf("pooled %+v", get.Throughput.Before)
	}
	gotRate := get.Throughput.Before.Value
	if math.Abs(gotRate-wantRequests/wantElapsed) > 1e-6 || gotRate == (2976.190476190476+2026.8456375838925)/2 {
		t.Fatalf("rate %v is not total requests over total time", gotRate)
	}
	if get.ServerErrors.Before.Value != 0 || get.ServerErrors.Before.ClientErrors == nil || *get.ServerErrors.Before.ClientErrors != 20 {
		t.Fatalf("5xx/4xx %+v", get.ServerErrors.Before)
	}
	if get.Throughput.Before.Requests == float64(v.Before.Client.HTTPRequests) {
		t.Fatal("server throughput used the k6 request count")
	}
	if post.Throughput == nil || post.Throughput.Status != fixvalidation.StatusInsufficient || post.Throughput.Before.Requests >= 100 {
		t.Fatalf("POST %+v", post.Throughput)
	}
	doc := map[string]any{}
	if err := json.Unmarshal(mustRead(t, analysisFixture), &doc); err != nil {
		t.Fatal(err)
	}
	if len(get.Evidence) == 0 {
		t.Fatal("no evidence")
	}
	for _, e := range get.Evidence {
		obj := resolve(t, doc, e.Pointer)
		if e.Side != fixvalidation.SideBefore && e.Side != fixvalidation.SideAfter {
			t.Errorf("side %s", e.Side)
		}
		if strings.HasSuffix(e.Pointer, "/requests") && obj["requests"] == nil {
			t.Errorf("%s is not a request object", e.Pointer)
		}
		if strings.HasSuffix(e.Pointer, "/p95") && obj["estimate"] != true {
			t.Errorf("%s is not a p95 estimate: %v", e.Pointer, obj["estimate"])
		}
	}
}

func TestDifferentVUsAreNotComparable(t *testing.T) {
	valid := mustRead(t, analysisFixture)
	changed := bytes.Replace(valid, []byte(`"vus": 2`), []byte(`"vus": 4`), 1)
	after := writeFile(t, "after.json", changed)
	out := filepath.Join(t.TempDir(), "validation.json")
	o := runCmd("-before", analysisFixture, "-after", after, "-out", out)
	if o.code != exitCompared || !strings.Contains(o.stdout, "Outcome: not_comparable") || !strings.Contains(o.stdout, "VUs differ") {
		t.Fatalf("exit %d\n%s\n%s", o.code, o.stdout, o.stderr)
	}
	r := readReport(t, out)
	if r.Validation.Outcome != fixvalidation.OutcomeNotComparable || len(r.Validation.Routes) != 0 {
		t.Fatalf("%+v", r.Validation.Outcome)
	}
}

func TestBadInputWritesAnErrorReport(t *testing.T) {
	valid := mustRead(t, analysisFixture)
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"malformed", []byte("{"), "not valid JSON"},
		{"unsupported", bytes.Replace(valid, []byte(`"schema_version": 1`), []byte(`"schema_version": 9`), 1), "unsupported analysis schema"},
		{"failed analysis", []byte("{\"schema_version\":1,\"error\":\"boom\"}\n"), "failed analysis"},
		{"non-finite", bytes.Replace(valid, []byte(`"per_second": 2144`), []byte(`"per_second": NaN`), 1), "not valid JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := writeFile(t, "analysis.json", tc.data)
			out := filepath.Join(t.TempDir(), "validation.json")
			if err := os.WriteFile(out, []byte("old result\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			o := runCmd("-before", in, "-after", analysisFixture, "-out", out)
			if o.code != exitBadInput || !strings.Contains(o.stderr, tc.want) {
				t.Fatalf("exit %d, stderr %q", o.code, o.stderr)
			}
			r := readReport(t, out)
			if r.Validation != nil || r.Error == "" || !strings.Contains(r.Error, tc.want) {
				t.Fatalf("report %+v", r)
			}
			if strings.Contains(string(mustRead(t, out)), "old result") {
				t.Fatal("old validation was left in place")
			}
		})
	}
}

func TestUsage(t *testing.T) {
	in := writeFile(t, "analysis.json", []byte("{}"))
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "-before and -after are required"},
		{[]string{"-before", in, "more"}, "unexpected arguments"},
		{[]string{"-before", in, "-after", in, "-out", in}, "-out must not replace"},
		{[]string{"-before", in, "-after", in, "-max-input-bytes", "10"}, "-max-input-bytes must be between"},
		{[]string{"-before", in, "-after", in, "-min-windows", "0"}, "min windows must be between"},
		{[]string{"-before", in, "-after", in, "-throughput-change", "NaN"}, "throughput change must be"},
		{[]string{"-before", in, "-after", in, "-diagnose"}, "flag provided but not defined"},
	} {
		o := runCmd(tc.args...)
		if o.code != exitUsage || !strings.Contains(o.stderr, tc.want) {
			t.Errorf("args %q: exit %d, stderr %q", tc.args, o.code, o.stderr)
		}
	}
	if o := runCmd("-h"); o.code != exitUsage || !strings.Contains(o.stderr, "Usage: ruptiq-validate-fix") {
		t.Errorf("-h: exit %d, stderr %q", o.code, o.stderr)
	}
}

func TestOutRejectsAnotherPathToAnInput(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "before.json")
	data := mustRead(t, analysisFixture)
	if err := os.WriteFile(in, data, 0o644); err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(in)
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{abs, filepath.Join(dir, ".", "before.json")}
	if runtime.GOOS == "windows" {
		paths = append(paths, filepath.Join(dir, "BEFORE.JSON"))
	}
	for _, out := range paths {
		o := runCmd("-before", in, "-after", in, "-out", out)
		if o.code != exitUsage || !strings.Contains(o.stderr, "-out must not replace") {
			t.Errorf("out %s: exit %d, stderr %q", out, o.code, o.stderr)
		}
	}
	if got := mustRead(t, in); !bytes.Equal(got, data) {
		t.Fatal("an alternate output path modified the input")
	}
}

func TestMissingAndUnwritable(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.json")
	o := runCmd("-before", missing, "-after", analysisFixture)
	if o.code != exitBadInput || !strings.Contains(o.stderr, "before:") {
		t.Fatalf("exit %d stderr %q", o.code, o.stderr)
	}
	out := filepath.Join(t.TempDir(), "missing-dir", "validation.json")
	o = runCmd("-before", analysisFixture, "-after", analysisFixture, "-out", out)
	if o.code != exitBadInput || !strings.Contains(o.stderr, "writing validation") {
		t.Fatalf("exit %d stderr %q", o.code, o.stderr)
	}
}

func TestCredentialsAreNotShown(t *testing.T) {
	valid := mustRead(t, analysisFixture)
	leaky := bytes.ReplaceAll(valid, []byte("http://127.0.0.1:8082/metrics"), []byte("http://scraper:hunter2@127.0.0.1:8082/metrics?token=topsecret"))
	leaky = bytes.ReplaceAll(leaky, []byte("http://127.0.0.1:8082/inventory/LAPTOP-001"), []byte("http://loaduser:hunter2@127.0.0.1:8082/inventory/LAPTOP-001?api_key=topsecret"))
	leaky = bytes.ReplaceAll(leaky, []byte(`"/reservations"`), []byte(`"https://u:hunter2@h/reservations?k=topsecret"`))
	in := writeFile(t, "analysis.json", leaky)
	out := filepath.Join(t.TempDir(), "validation.json")
	o := runCmd("-before", in, "-after", in, "-out", out)
	if o.code != exitCompared {
		t.Fatalf("exit %d: %s", o.code, o.stderr)
	}
	written := string(mustRead(t, out))
	for _, text := range []string{o.stdout, o.stderr, written} {
		for _, s := range []string{"hunter2", "topsecret", "scraper", "loaduser"} {
			if strings.Contains(text, s) {
				t.Errorf("output contains %q", s)
			}
		}
	}
	r := readReport(t, out)
	if r.Validation.Before.TargetURL != "http://xxxxx@127.0.0.1:8082/inventory/LAPTOP-001?xxxxx" {
		t.Errorf("target %q", r.Validation.Before.TargetURL)
	}
}
