package k6

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/RajK158/ruptiq/internal/workload"
)

// The test binary doubles as a fake k6: with fakeModeEnv set, TestMain runs
// fakeK6 instead of the tests. It needs neither k6 nor a network.
const (
	fakeModeEnv    = "RUPTIQ_FAKE_K6_MODE"
	fakeFixtureEnv = "RUPTIQ_FAKE_K6_FIXTURE"
	fakeRecordEnv  = "RUPTIQ_FAKE_K6_RECORD"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(fakeModeEnv); mode != "" {
		os.Exit(fakeK6(mode))
	}
	os.Exit(m.Run())
}

// invocation is what the fake k6 saw.
type invocation struct {
	Args   []string `json:"args"`
	Env    []string `json:"env"`
	Script string   `json:"script"`
	Dir    string   `json:"dir"`
}

func fakeK6(mode string) int {
	args := os.Args[1:]
	var summaryPath string
	if i := slices.Index(args, "--summary-export"); i >= 0 && i+1 < len(args) {
		summaryPath = args[i+1]
	}
	if record := os.Getenv(fakeRecordEnv); record != "" {
		inv := invocation{Args: args, Env: os.Environ()}
		if len(args) > 0 {
			script, _ := os.ReadFile(args[len(args)-1])
			inv.Script = string(script)
		}
		inv.Dir, _ = os.Getwd()
		data, _ := json.Marshal(inv)
		if err := os.WriteFile(record, data, 0o600); err != nil {
			fmt.Fprintln(os.Stderr, "fake k6:", err)
			return 90
		}
	}

	switch mode {
	case "ok":
		data, err := os.ReadFile(os.Getenv(fakeFixtureEnv))
		if err == nil {
			err = os.WriteFile(summaryPath, data, 0o600)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake k6:", err)
			return 90
		}
		return 0
	case "script-error":
		// What k6 prints and returns when the script fails to load.
		fmt.Fprintln(os.Stderr, `level=error msg="SyntaxError: file:///tmp/http_get.js: Unexpected token (3:0)"`)
		return 107
	case "no-summary":
		fmt.Fprintln(os.Stdout, "summary disabled")
		return 0
	case "malformed":
		_ = os.WriteFile(summaryPath, []byte(`{"version":"1.0.0","config":`), 0o600)
		return 0
	case "noisy":
		for i := 0; i < 5000; i++ {
			fmt.Fprintf(os.Stderr, "level=warning msg=\"Request Failed\" error=\"dial tcp: connection refused\" n=%d\n", i)
		}
		fmt.Fprintln(os.Stderr, "FINAL LINE")
		return 99
	case "echo-url", "echo-url-no-summary":
		// The target as k6 prints it for a failed request (Go's HTTP client
		// replaces only the password), and verbatim.
		target := os.Getenv(targetEnv)
		shown := target
		if u, err := url.Parse(target); err == nil {
			if _, ok := u.User.Password(); ok {
				u.User = url.UserPassword(u.User.Username(), "***")
			}
			shown = u.String()
		}
		fmt.Fprintf(os.Stderr, "level=warning msg=\"Request Failed\" error=\"Get \\\"%s\\\": dial tcp: connection refused\"\n", shown)
		fmt.Fprintf(os.Stderr, "target %s\n", target)
		if mode == "echo-url" {
			return 1
		}
		return 0
	case "sleep":
		time.Sleep(time.Minute)
		return 0
	}
	fmt.Fprintln(os.Stderr, "fake k6: unknown mode", mode)
	return 90
}

// fakeRunner returns a Runner that runs the fake k6 in mode, and the path the
// fake records its invocation to.
func fakeRunner(t *testing.T, mode, fixture string) (Runner, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(t.TempDir(), "invocation.json")
	t.Setenv(fakeModeEnv, mode)
	t.Setenv(fakeRecordEnv, record)
	if fixture != "" {
		abs, err := filepath.Abs(filepath.Join("testdata", fixture))
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv(fakeFixtureEnv, abs)
	}
	return Runner{Executable: exe}, record
}

func readInvocation(t *testing.T, record string) invocation {
	t.Helper()
	data, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("fake k6 did not run: %v", err)
	}
	var inv invocation
	if err := json.Unmarshal(data, &inv); err != nil {
		t.Fatal(err)
	}
	return inv
}

func spec() workload.Spec {
	return workload.Spec{TargetURL: "http://127.0.0.1:8082/inventory/LAPTOP-001", VUs: 3, Duration: 7 * time.Second}
}

func assertCleanedUp(t *testing.T, inv invocation) {
	t.Helper()
	if inv.Dir == "" || !strings.Contains(filepath.Base(inv.Dir), "ruptiq-k6-") {
		t.Fatalf("k6 ran in %q, want a ruptiq-k6- temporary directory", inv.Dir)
	}
	if _, err := os.Stat(inv.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary directory %s still exists (stat err %v)", inv.Dir, err)
	}
}

func TestArgs(t *testing.T) {
	got := args(spec(), "/tmp/x/http_get.js", "/tmp/x/summary.json")
	want := []string{
		"run", "--quiet", "--no-color", "--no-usage-report",
		"--summary-mode", "compact",
		"--new-machine-readable-summary",
		"--summary-export", "/tmp/x/summary.json",
		"--include-system-env-vars=true",
		"--vus", "3", "--duration", "7s",
		"/tmp/x/http_get.js",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("args =\n%q\nwant\n%q", got, want)
	}
	for _, a := range got {
		if strings.Contains(a, "127.0.0.1") {
			t.Fatalf("args contain the target URL: %q", got)
		}
	}
}

func TestEnviron(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin",
		"K6_SUMMARY_MODE=disabled",
		"k6_vus=500",
		"K6_CLOUD_TOKEN=secret",
		"RUPTIQ_TARGET_URL=http://stale.example/",
		"HOME=/home/u",
	}
	got := environ(parent, "http://target.example/x")
	want := []string{"PATH=/usr/bin", "HOME=/home/u", "RUPTIQ_TARGET_URL=http://target.example/x"}
	if !slices.Equal(got, want) {
		t.Fatalf("environ = %q, want %q", got, want)
	}
}

func TestRunSuccess(t *testing.T) {
	r, record := fakeRunner(t, "ok", "summary_ok.json")
	t.Setenv("K6_SUMMARY_MODE", "disabled")
	s := spec()
	s.TargetURL = "http://user:s3cret@127.0.0.1:8082/inventory/LAPTOP-001"

	before := time.Now().UTC()
	res, err := r.Run(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	inv := readInvocation(t, record)

	if res.Runner != "k6" || res.RunnerVersion != "2.3.0" || res.VUs != 3 || res.ConfiguredDurationSeconds != 7 {
		t.Errorf("result = %+v", res)
	}
	if res.TargetURL != "http://xxxxx@127.0.0.1:8082/inventory/LAPTOP-001" {
		t.Errorf("result target = %q, want redacted", res.TargetURL)
	}
	if res.StartedAt.Before(before) || res.FinishedAt.Before(res.StartedAt) {
		t.Errorf("times: started %v finished %v (test began %v)", res.StartedAt, res.FinishedAt, before)
	}
	if res.Metrics.HTTPRequests != 12355 || res.Metrics.LatencyMs.P95 != 0.6818 {
		t.Errorf("metrics = %+v", res.Metrics)
	}

	if !slices.Equal(inv.Args, args(s, filepath.Join(inv.Dir, "http_get.js"), filepath.Join(inv.Dir, "summary.json"))) {
		t.Errorf("k6 args = %q", inv.Args)
	}
	if inv.Script != string(httpGetScript) || !strings.Contains(inv.Script, "__ENV.RUPTIQ_TARGET_URL") {
		t.Errorf("k6 script = %q, want the embedded script", inv.Script)
	}
	var targets []string
	for _, kv := range inv.Env {
		if strings.HasPrefix(strings.ToUpper(kv), "K6_") {
			t.Errorf("k6 inherited %q", kv)
		}
		if v, ok := strings.CutPrefix(kv, targetEnv+"="); ok {
			targets = append(targets, v)
		}
	}
	if !slices.Equal(targets, []string{s.TargetURL}) {
		t.Errorf("k6 saw %s = %q, want exactly the target URL", targetEnv, targets)
	}
	assertCleanedUp(t, inv)
}

func TestRunEveryRequestFailedIsAResult(t *testing.T) {
	r, _ := fakeRunner(t, "ok", "summary_all_failed.json")
	res, err := r.Run(context.Background(), spec())
	if err != nil {
		t.Fatalf("Run = %v; a completed run with failed requests must still be reported", err)
	}
	if res.Metrics.FailureRate != 1 || workload.Evaluate(res.Metrics, 0).Passed {
		t.Fatalf("metrics = %+v, want every request failed and the verdict failed", res.Metrics)
	}
}

func TestRunErrors(t *testing.T) {
	for _, tc := range []struct {
		mode, fixture string
		check         func(t *testing.T, err error)
	}{
		{"script-error", "", func(t *testing.T, err error) {
			var exitErr *ExitError
			if !errors.As(err, &exitErr) || exitErr.Code != 107 || !strings.Contains(exitErr.Output, "SyntaxError") {
				t.Fatalf("err = %v, want ExitError 107 with k6's message", err)
			}
		}},
		{"no-summary", "", func(t *testing.T, err error) {
			if !errors.Is(err, ErrSummary) || !strings.Contains(err.Error(), "did not write a summary") ||
				!strings.Contains(err.Error(), "summary disabled") {
				t.Fatalf("err = %v, want ErrSummary with k6's output", err)
			}
		}},
		{"malformed", "", func(t *testing.T, err error) {
			if !errors.Is(err, ErrSummary) || !strings.Contains(err.Error(), "not valid JSON") {
				t.Fatalf("err = %v, want ErrSummary for malformed JSON", err)
			}
		}},
		{"ok", "summary_no_requests.json", func(t *testing.T, err error) {
			if !errors.Is(err, ErrNoRequests) {
				t.Fatalf("err = %v, want ErrNoRequests", err)
			}
		}},
	} {
		t.Run(tc.mode+tc.fixture, func(t *testing.T) {
			r, record := fakeRunner(t, tc.mode, tc.fixture)
			res, err := r.Run(context.Background(), spec())
			if res != (workload.Result{}) {
				t.Errorf("Run returned a partial result %+v alongside error", res)
			}
			tc.check(t, err)
			assertCleanedUp(t, readInvocation(t, record))
		})
	}
}

func TestRunBoundsOutput(t *testing.T) {
	r, _ := fakeRunner(t, "noisy", "")
	_, err := r.Run(context.Background(), spec())
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 99 {
		t.Fatalf("err = %v, want ExitError 99", err)
	}
	if len(exitErr.Output) > outputTailBytes+64 {
		t.Errorf("output is %d bytes, want at most about %d", len(exitErr.Output), outputTailBytes)
	}
	if !strings.HasSuffix(exitErr.Output, "FINAL LINE") || !strings.HasPrefix(exitErr.Output, "[earlier output omitted]") {
		t.Errorf("output should keep the end and mark the omission; got start %q end %q",
			exitErr.Output[:40], exitErr.Output[len(exitErr.Output)-20:])
	}
}

// sensitiveTargets are target URLs with the secrets they contain.
var sensitiveTargets = []struct {
	name, url string
	secrets   []string
}{
	{"user and password", "http://alice:pw-SECRET1@127.0.0.1:8082/inventory/LAPTOP-001", []string{"alice", "pw-SECRET1"}},
	{"api_key query", "http://127.0.0.1:8082/inventory/LAPTOP-001?api_key=KEY-SECRET2", []string{"KEY-SECRET2"}},
	{"token query", "http://127.0.0.1:8082/healthz?token=TOK-SECRET3", []string{"TOK-SECRET3"}},
	{"several query parameters", "http://127.0.0.1:8082/p?q=1&api_key=KEY-SECRET2&token=TOK-SECRET3", []string{"KEY-SECRET2", "TOK-SECRET3"}},
	{"everything", "https://alice:pw-SECRET1@example.com:8443/p?api_key=KEY-SECRET2&token=TOK-SECRET3",
		[]string{"alice", "pw-SECRET1", "KEY-SECRET2", "TOK-SECRET3"}},
}

func assertNoSecrets(t *testing.T, where, text string, secrets []string) {
	t.Helper()
	for _, s := range secrets {
		if strings.Contains(text, s) {
			t.Errorf("%s leaks %q:\n%s", where, s, text)
		}
	}
}

func TestRunPassesFullURLOnlyThroughEnvironment(t *testing.T) {
	for _, tc := range sensitiveTargets {
		t.Run(tc.name, func(t *testing.T) {
			r, record := fakeRunner(t, "ok", "summary_ok.json")
			s := spec()
			s.TargetURL = tc.url
			res, err := r.Run(context.Background(), s)
			if err != nil {
				t.Fatal(err)
			}
			inv := readInvocation(t, record)
			if !slices.Contains(inv.Env, targetEnv+"="+tc.url) {
				t.Errorf("k6 did not receive the complete original URL in %s", targetEnv)
			}
			assertNoSecrets(t, "k6 command line", strings.Join(inv.Args, " "), tc.secrets)
			assertNoSecrets(t, "Result.TargetURL", res.TargetURL, tc.secrets)
			if res.TargetURL != workload.RedactURL(tc.url) {
				t.Errorf("Result.TargetURL = %q, want %q", res.TargetURL, workload.RedactURL(tc.url))
			}
		})
	}
}

func TestRunRedactsSecretsInErrors(t *testing.T) {
	for _, mode := range []string{"echo-url", "echo-url-no-summary"} {
		for _, tc := range sensitiveTargets {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				r, _ := fakeRunner(t, mode, "")
				s := spec()
				s.TargetURL = tc.url
				_, err := r.Run(context.Background(), s)
				if err == nil {
					t.Fatal("Run succeeded, want an error")
				}
				if !strings.Contains(err.Error(), "connection refused") {
					t.Errorf("error lost k6's output: %v", err)
				}
				assertNoSecrets(t, "error", err.Error(), tc.secrets)
			})
		}
	}
}

func TestRunMissingExecutable(t *testing.T) {
	for _, exe := range []string{
		filepath.Join(t.TempDir(), "no-such-k6"),
		"ruptiq-no-such-k6-executable",
	} {
		_, err := Runner{Executable: exe}.Run(context.Background(), spec())
		if !errors.Is(err, ErrNotInstalled) || !strings.Contains(err.Error(), "install k6") {
			t.Errorf("Run with %q = %v, want ErrNotInstalled with install hint", exe, err)
		}
	}
}

func TestRunValidatesBeforeStarting(t *testing.T) {
	r, record := fakeRunner(t, "ok", "summary_ok.json")
	_, err := r.Run(context.Background(), workload.Spec{TargetURL: "ftp://example.com", VUs: 1, Duration: time.Second})
	var verr *workload.ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("err = %v, want ValidationError", err)
	}
	if _, err := os.Stat(record); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("k6 was started for an invalid spec")
	}
}

func TestRunCanceled(t *testing.T) {
	r, record := fakeRunner(t, "sleep", "")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Run(ctx, spec()); !errors.Is(err, context.Canceled) {
		t.Fatalf("already-canceled Run = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(record); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("k6 was started with a canceled context")
	}

	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(500*time.Millisecond, cancel)
	start := time.Now()
	_, err := r.Run(ctx, spec())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("Run took %v after cancellation", elapsed)
	}
	assertCleanedUp(t, readInvocation(t, record))
}

func TestRunTimeout(t *testing.T) {
	r, record := fakeRunner(t, "sleep", "")
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := r.Run(ctx, spec())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("Run took %v after the deadline", elapsed)
	}
	assertCleanedUp(t, readInvocation(t, record))
}

func TestTailBuffer(t *testing.T) {
	tb := &tailBuffer{max: 10}
	fmt.Fprint(tb, "hello")
	if tb.String() != "hello" {
		t.Fatalf("String = %q", tb.String())
	}
	fmt.Fprint(tb, "\nworld!")
	if got := tb.String(); got != "[earlier output omitted]\nworld!" {
		t.Fatalf("String = %q", got)
	}
	tb = &tailBuffer{max: 4}
	fmt.Fprint(tb, "abcdefgh")
	if got := tb.String(); got != "[earlier output omitted]" {
		t.Fatalf("String = %q, want the partial line dropped", got)
	}
	if !bytes.Equal(tb.buf, []byte("efgh")) {
		t.Fatalf("buf = %q", tb.buf)
	}
}

func TestTailBufferNeverKeepsPartOfASecret(t *testing.T) {
	const target = "http://alice:pw-SECRET1@127.0.0.1:1/p?api_key=KEY-SECRET2&token=TOK-SECRET3"
	line := `level=warning msg="Request Failed" error="Get \"http://alice:***@127.0.0.1:1/p?api_key=KEY-SECRET2&token=TOK-SECRET3\": dial tcp 127.0.0.1:1: connection refused"` + "\n"
	// Try every cut position within one line.
	for cut := 0; cut < len(line); cut++ {
		tb := &tailBuffer{max: 3*len(line) - cut}
		for i := 0; i < 5; i++ {
			fmt.Fprint(tb, line)
		}
		got := workload.RedactText(tb.String(), target)
		for _, frag := range []string{"alice", "SECRET", "CRET", "ET1", "ET2", "ET3"} {
			if strings.Contains(got, frag) {
				t.Fatalf("cut %d: output leaks %q:\n%s", cut, frag, got)
			}
		}
		if !strings.Contains(got, "connection refused") {
			t.Fatalf("cut %d: output lost the kept lines:\n%s", cut, got)
		}
	}
}
