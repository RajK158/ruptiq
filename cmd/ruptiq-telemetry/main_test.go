package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RajK158/ruptiq/internal/telemetry"
)

const exposition = `# HELP inventory_stock_available_units Units that can still be reserved.
# TYPE inventory_stock_available_units gauge
inventory_stock_available_units{sku="LAPTOP-001"} 8
# HELP inventory_http_requests_total Requests.
# TYPE inventory_http_requests_total counter
inventory_http_requests_total{method="POST",route="/reservations",status="201"} 1
`

func metricsServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Write([]byte(exposition))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func runCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(context.Background(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func readReport(t *testing.T, path string) telemetry.Report {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var r telemetry.Report
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		t.Fatalf("%s is not a valid report: %v\n%s", path, err, data)
	}
	return r
}

func TestSnapshotSummaryAndJSON(t *testing.T) {
	srv := metricsServer(t)
	out := filepath.Join(t.TempDir(), "snapshot.json")

	code, stdout, stderr := runCLI(t, "-url", srv.URL+"/metrics", "-out", out)
	if code != exitOK || stderr != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	for _, want := range []string{
		"Endpoint:    " + srv.URL + "/metrics",
		"Contents:    2 families, 2 series (1 counter, 1 gauge)",
		`inventory_stock_available_units  gauge, 1 series`,
		`{sku="LAPTOP-001"}  8`,
		`{method="POST",route="/reservations",status="201"}  1`,
		"Full snapshot written to " + out,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout missing %q:\n%s", want, stdout)
		}
	}

	r := readReport(t, out)
	if r.SchemaVersion != telemetry.ReportSchemaVersion || r.Error != "" || r.Snapshot == nil || r.Endpoint != srv.URL+"/metrics" {
		t.Fatalf("report = %+v", r)
	}
	f, ok := r.Snapshot.Family("inventory_stock_available_units")
	if !ok || f.Type != telemetry.Gauge || len(f.Series) != 1 || f.Series[0].Labels["sku"] != "LAPTOP-001" || *f.Series[0].Value != 8 {
		t.Errorf("gauge family = %+v", f)
	}
	if entries, _ := os.ReadDir(filepath.Dir(out)); len(entries) != 1 {
		t.Errorf("output directory has %d entries, want only the report (no temporary files)", len(entries))
	}
}

func TestSnapshotWithoutOut(t *testing.T) {
	srv := metricsServer(t)
	code, stdout, _ := runCLI(t, "-url", srv.URL+"/metrics")
	if code != exitOK || strings.Contains(stdout, "written to") {
		t.Errorf("exit %d, stdout:\n%s", code, stdout)
	}
}

func TestFailureIsReportedAndRecorded(t *testing.T) {
	srv := metricsServer(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := "http://" + l.Addr().String() + "/metrics"
	l.Close()

	tests := []struct {
		name, url, wantErr string
	}{
		{"HTTP error", srv.URL + "/wrong", "404 Not Found"},
		{"unreachable", closed, "metrics request failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "snapshot.json")
			// An older successful snapshot must not survive a failed read.
			if code, _, _ := runCLI(t, "-url", srv.URL+"/metrics", "-out", out); code != exitOK {
				t.Fatal("setup run failed")
			}

			code, stdout, stderr := runCLI(t, "-url", tt.url, "-out", out, "-timeout", "5s")
			if code != exitFailed {
				t.Errorf("exit = %d, want %d", code, exitFailed)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want nothing", stdout)
			}
			if !strings.Contains(stderr, "ruptiq-telemetry: no snapshot: ") || !strings.Contains(stderr, tt.wantErr) ||
				!strings.Contains(stderr, "Failure recorded in "+out) {
				t.Errorf("stderr = %q", stderr)
			}
			r := readReport(t, out)
			if r.Snapshot != nil || !strings.Contains(r.Error, tt.wantErr) || r.Endpoint != tt.url {
				t.Errorf("report = %+v", r)
			}
		})
	}
}

func TestOutputRedactsCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, pw, _ := r.BasicAuth(); user != "admin" || pw != "hunter2" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte("up 1\n"))
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	for _, tt := range []struct {
		name     string
		password string
		wantCode int
	}{
		{"accepted", "hunter2", exitOK},
		{"rejected", "wrong-pw", exitFailed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "snapshot.json")
			url := "http://admin:" + tt.password + "@" + host + "/metrics?api_key=k-999"
			code, stdout, stderr := runCLI(t, "-url", url, "-out", out)
			if code != tt.wantCode {
				t.Fatalf("exit %d, stderr %q", code, stderr)
			}
			file, _ := os.ReadFile(out)
			for where, text := range map[string]string{"stdout": stdout, "stderr": stderr, "file": string(file)} {
				for _, secret := range []string{tt.password, "k-999", "api_key", "admin:"} {
					if strings.Contains(text, secret) {
						t.Errorf("%s contains %q:\n%s", where, secret, text)
					}
				}
				if text != "" && where != "stderr" && !strings.Contains(text, "http://xxxxx@"+host+"/metrics?xxxxx") {
					t.Errorf("%s does not show the redacted endpoint:\n%s", where, text)
				}
			}
		})
	}
}

func TestUsageErrors(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"no URL", nil, "-url is required"},
		{"invalid URL", []string{"-url", "localhost:8082/metrics"}, "http:// or https://"},
		{"unknown flag", []string{"-url", "http://h/metrics", "-bogus"}, "flag provided but not defined"},
		{"extra argument", []string{"-url", "http://h/metrics", "extra"}, "unexpected arguments"},
		{"zero timeout", []string{"-url", "http://h/metrics", "-timeout", "0s"}, "-timeout must be positive"},
		{"bad URL hides secrets", []string{"-url", "ftp://u:topsecret@h/metrics"}, "http:// or https://"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := runCLI(t, tt.args...)
			if code != exitUsage || stdout != "" || !strings.Contains(stderr, tt.wantErr) {
				t.Errorf("exit %d, stdout %q, stderr %q; want exit %d mentioning %q", code, stdout, stderr, exitUsage, tt.wantErr)
			}
			if strings.Contains(stderr, "topsecret") {
				t.Errorf("stderr shows the password: %q", stderr)
			}
		})
	}

	if code, _, _ := runCLI(t, "-h"); code != exitUsage {
		t.Errorf("-h exit = %d, want %d", code, exitUsage)
	}
}

func TestCancelledContext(t *testing.T) {
	srv := metricsServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out, errOut bytes.Buffer
	if code := run(ctx, []string{"-url", srv.URL + "/metrics"}, &out, &errOut); code != exitFailed {
		t.Errorf("exit = %d, want %d", code, exitFailed)
	}
	if !strings.Contains(errOut.String(), "context canceled") {
		t.Errorf("stderr = %q", errOut.String())
	}
}
