package telemetry

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const textFormat = "text/plain; version=0.0.4; charset=utf-8"

// fixture reads testdata/name. Line endings are normalized because Git may
// check text files out with CRLF on Windows.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
}

// serveBody starts a server answering every request with body as the
// Prometheus text format.
func serveBody(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", textFormat)
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newReader(t *testing.T, cfg PrometheusConfig) *PrometheusReader {
	t.Helper()
	r, err := NewPrometheusReader(cfg)
	if err != nil {
		t.Fatalf("NewPrometheusReader: %v", err)
	}
	return r
}

func mustRead(t *testing.T, r *PrometheusReader) Snapshot {
	t.Helper()
	s, err := r.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return s
}

func mustFamily(t *testing.T, s Snapshot, name string, typ MetricType, series int) Family {
	t.Helper()
	f, ok := s.Family(name)
	if !ok {
		t.Fatalf("family %s missing", name)
	}
	if f.Type != typ || len(f.Series) != series {
		t.Fatalf("family %s: type %s with %d series, want %s with %d", name, f.Type, len(f.Series), typ, series)
	}
	return f
}

func wantValue(t *testing.T, s Series, want float64) {
	t.Helper()
	if s.Value == nil || s.Histogram != nil || s.Summary != nil {
		t.Fatalf("series %v: want only a value", s.Labels)
	}
	if got := float64(*s.Value); got != want && !(math.IsNaN(got) && math.IsNaN(want)) {
		t.Errorf("series %v value = %v, want %v", s.Labels, got, want)
	}
}

func TestReadValidExposition(t *testing.T) {
	var accept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", textFormat)
		w.Write(fixture(t, "valid.prom"))
	}))
	defer srv.Close()

	before := time.Now().UTC()
	s := mustRead(t, newReader(t, PrometheusConfig{URL: srv.URL + "/metrics"}))
	after := time.Now().UTC()

	if !strings.HasPrefix(accept, "text/plain;version=0.0.4") {
		t.Errorf("Accept = %q, want the Prometheus text format first", accept)
	}
	if s.Endpoint != srv.URL+"/metrics" {
		t.Errorf("Endpoint = %q", s.Endpoint)
	}
	if s.ScrapedAt.Before(before) || s.ScrapedAt.After(after) || s.ScrapedAt.Location() != time.UTC {
		t.Errorf("ScrapedAt = %v, want UTC between %v and %v", s.ScrapedAt, before, after)
	}
	if s.ScrapeDurationSeconds < 0 || s.ScrapeDurationSeconds > after.Sub(before).Seconds() {
		t.Errorf("ScrapeDurationSeconds = %v", s.ScrapeDurationSeconds)
	}

	var names []string
	for _, f := range s.Families {
		names = append(names, f.Name)
	}
	wantNames := "app_build_info app_gc_pause_seconds app_queue_depth app_ratio app_request_duration_seconds app_requests_total app_temperature_celsius"
	if strings.Join(names, " ") != wantNames {
		t.Errorf("families = %v, want %s", names, wantNames)
	}
	if s.SeriesCount() != 10 {
		t.Errorf("SeriesCount = %d, want 10", s.SeriesCount())
	}

	// Counter: names, help, labels, and values as exposed, sorted by labels.
	requests := mustFamily(t, s, "app_requests_total", Counter, 3)
	if requests.Help != "Requests handled." {
		t.Errorf("help = %q", requests.Help)
	}
	wantLabels := []map[string]string{
		{"method": "GET", "route": "/items/{id}", "status": "200"},
		{"method": "GET", "route": "/items/{id}", "status": "404"},
		{"method": "POST", "route": "/items", "status": "201"},
	}
	for i, want := range []float64{1027, 0, 3} {
		if labelString(requests.Series[i].Labels) != labelString(wantLabels[i]) {
			t.Errorf("series %d labels = %v, want %v", i, requests.Series[i].Labels, wantLabels[i])
		}
		wantValue(t, requests.Series[i], want)
	}

	// Gauges: no labels, negative values, escaped label values, NaN.
	depth := mustFamily(t, s, "app_queue_depth", Gauge, 1)
	if depth.Series[0].Labels != nil {
		t.Errorf("labels = %v, want none", depth.Series[0].Labels)
	}
	wantValue(t, depth.Series[0], 7.5)
	temp := mustFamily(t, s, "app_temperature_celsius", Gauge, 1)
	wantValue(t, temp.Series[0], -3.25)
	if l := temp.Series[0].Labels; l["room"] != `cold "store"` || l["path"] != "C:\\data\nline2" {
		t.Errorf("escaped labels = %q", l)
	}
	wantValue(t, mustFamily(t, s, "app_ratio", Gauge, 1).Series[0], math.NaN())

	// Histogram: bucket bounds, cumulative counts, sum, and count.
	hist := mustFamily(t, s, "app_request_duration_seconds", Histogram, 2)
	wantHist := []struct {
		route  string
		count  uint64
		sum    float64
		counts []uint64
	}{
		{"/items", 3, 0.0625, []uint64{0, 2, 3, 3}},
		{"/items/{id}", 1027, 1.5, []uint64{600, 1000, 1027, 1027}},
	}
	bounds := []float64{0.001, 0.01, 0.1, math.Inf(1)}
	for i, want := range wantHist {
		series := hist.Series[i]
		h := series.Histogram
		if series.Labels["route"] != want.route || len(series.Labels) != 1 || h == nil || series.Value != nil {
			t.Fatalf("histogram series %d = %+v, want route %s", i, series, want.route)
		}
		if h.Count != want.count || float64(h.Sum) != want.sum || len(h.Buckets) != len(bounds) {
			t.Errorf("%s: count %d sum %v buckets %d", want.route, h.Count, h.Sum, len(h.Buckets))
			continue
		}
		for j, b := range h.Buckets {
			if float64(b.UpperBound) != bounds[j] || b.CumulativeCount != want.counts[j] {
				t.Errorf("%s bucket %d = le %v count %d, want le %v count %d",
					want.route, j, b.UpperBound, b.CumulativeCount, bounds[j], want.counts[j])
			}
		}
	}

	// Summary: quantiles, sum, and count.
	pause := mustFamily(t, s, "app_gc_pause_seconds", Summary, 1).Series[0].Summary
	if pause == nil || pause.Count != 400 || pause.Sum != 0.25 || len(pause.Quantiles) != 2 ||
		pause.Quantiles[0] != (Quantile{0.5, 0.0002}) || pause.Quantiles[1] != (Quantile{0.99, 0.004}) {
		t.Errorf("summary = %+v", pause)
	}

	// Untyped, with the sample's own timestamp.
	build := mustFamily(t, s, "app_build_info", Untyped, 1).Series[0]
	wantValue(t, build, 1)
	if build.Timestamp == nil || !build.Timestamp.Equal(time.UnixMilli(1762430400000)) {
		t.Errorf("timestamp = %v", build.Timestamp)
	}
}

func TestReadWithoutContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header()["Content-Type"] = nil // stop net/http from sniffing one
		w.Write([]byte("up 1\n"))
	}))
	defer srv.Close()
	s := mustRead(t, newReader(t, PrometheusConfig{URL: srv.URL}))
	wantValue(t, mustFamily(t, s, "up", Untyped, 1).Series[0], 1)
}

func TestReadInvalidMetrics(t *testing.T) {
	tests := []struct {
		name, contentType, body, wantMsg string
	}{
		{"empty body", textFormat, "", "response body is empty"},
		{"whitespace only", textFormat, " \n\n", "response body is empty"},
		{"comments only", textFormat, "# HELP x y\n# TYPE x counter\n", "no samples"},
		{"syntax error", textFormat, string(fixture(t, "malformed_syntax.prom")), "line 2"},
		{"bad value", textFormat, "x_total abc\n", `line 1: expected float as value, got "abc"`},
		{"decreasing buckets", textFormat, string(fixture(t, "malformed_histogram.prom")), "bucket counts decrease"},
		{"+Inf bucket differs from count", textFormat,
			"# TYPE h histogram\nh_bucket{le=\"1\"} 1\nh_bucket{le=\"+Inf\"} 2\nh_sum 1\nh_count 3\n", "+Inf bucket has 2 observations but count is 3"},
		{"fractional bucket count", textFormat,
			"# TYPE h histogram\nh_bucket{le=\"1\"} 1.5\nh_bucket{le=\"+Inf\"} 2\nh_sum 1\nh_count 2\n", "not a whole"},
		{"HTML page", "text/html; charset=utf-8", "<html><body>login</body></html>", `unsupported Content-Type "text/html"`},
		{"OpenMetrics", "application/openmetrics-text; version=1.0.0", "up 1\n# EOF\n", "unsupported Content-Type"},
		{"JSON", "application/json", `{"up":1}`, "unsupported Content-Type"},
		{"unreadable Content-Type", "text/plain; =broken", "up 1\n", "unreadable Content-Type"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tt.contentType)
				w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			s, err := newReader(t, PrometheusConfig{URL: srv.URL + "/metrics"}).Read(context.Background())
			if !errors.Is(err, ErrInvalidMetrics) {
				t.Fatalf("error = %v, want ErrInvalidMetrics", err)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) || !strings.Contains(err.Error(), srv.URL+"/metrics") {
				t.Errorf("error %q does not mention %q and the endpoint", err, tt.wantMsg)
			}
			if s.Families != nil {
				t.Errorf("partial snapshot returned: %+v", s)
			}
		})
	}
}

func TestReadHTTPErrorStatus(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantMsg string
	}{
		{"not found", http.StatusNotFound, "404 page not found\n", "404 Not Found: 404 page not found"},
		{"server error, no body", http.StatusInternalServerError, "", "500 Internal Server Error"},
		{"long body is cut", http.StatusServiceUnavailable, strings.Repeat("busy\n", 100), "503 Service Unavailable: busy busy busy"},
		{"success without content", http.StatusNoContent, "", "204 No Content"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			_, err := newReader(t, PrometheusConfig{URL: srv.URL}).Read(context.Background())
			if !errors.Is(err, ErrHTTPStatus) {
				t.Fatalf("error = %v, want ErrHTTPStatus", err)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error %q does not contain %q", err, tt.wantMsg)
			}
			if len(err.Error()) > 400 {
				t.Errorf("error is %d bytes long", len(err.Error()))
			}
			if cut := len(tt.body) > 200; cut != strings.HasSuffix(err.Error(), " ...") {
				t.Errorf("error %q: cut marker present = %v, want %v", err, !cut, cut)
			}
		})
	}
}

// closedURL returns a URL on a local port that nothing listens on.
func closedURL(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return "http://" + addr + "/metrics"
}

func TestReadUnreachable(t *testing.T) {
	endpoint := closedURL(t)
	_, err := newReader(t, PrometheusConfig{URL: endpoint, Timeout: 5 * time.Second}).Read(context.Background())
	if !errors.Is(err, ErrFetch) {
		t.Fatalf("error = %v, want ErrFetch", err)
	}
	var opErr *net.OpError
	if !errors.As(err, &opErr) {
		t.Errorf("error %v does not wrap the network error", err)
	}
	if !strings.Contains(err.Error(), "GET "+endpoint) {
		t.Errorf("error %q does not name the endpoint", err)
	}
}

func TestReadTimeout(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"no response", func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }},
		{"body stalls", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", textFormat)
			w.Write([]byte("# TYPE x counter\nx_total 1\n"))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()

			start := time.Now()
			_, err := newReader(t, PrometheusConfig{URL: srv.URL, Timeout: 100 * time.Millisecond}).Read(context.Background())
			if !errors.Is(err, ErrFetch) || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error = %v, want ErrFetch and context.DeadlineExceeded", err)
			}
			if !strings.Contains(err.Error(), "timeout 100ms") {
				t.Errorf("error %q does not state the timeout", err)
			}
			if elapsed := time.Since(start); elapsed > 5*time.Second {
				t.Errorf("Read took %v; the timeout did not stop it", elapsed)
			}
		})
	}
}

func TestReadCancelled(t *testing.T) {
	entered := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := newReader(t, PrometheusConfig{URL: srv.URL}).Read(ctx)
		errc <- err
	}()
	<-entered
	cancel()

	select {
	case err := <-errc:
		if !errors.Is(err, ErrFetch) || !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want ErrFetch and context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Read did not return after cancellation")
	}
}

func TestReadResponseSizeLimit(t *testing.T) {
	body := fixture(t, "valid.prom")
	srv := serveBody(t, body)

	// Exactly at the limit is accepted.
	mustRead(t, newReader(t, PrometheusConfig{URL: srv.URL, MaxBytes: int64(len(body))}))

	// One byte over is rejected.
	_, err := newReader(t, PrometheusConfig{URL: srv.URL, MaxBytes: int64(len(body)) - 1}).Read(context.Background())
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("error = %v, want ErrResponseTooLarge", err)
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error = %q", err)
	}
}

func TestReadResponseSizeLimitAppliesAfterDecompression(t *testing.T) {
	// 1 MiB of comments compresses to about 1 KiB.
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	zw.Write([]byte(strings.Repeat("# padding\n", 100_000) + "up 1\n"))
	zw.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", textFormat)
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(compressed.Bytes())
	}))
	defer srv.Close()

	_, err := newReader(t, PrometheusConfig{URL: srv.URL, MaxBytes: 64 << 10}).Read(context.Background())
	if !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("error = %v, want ErrResponseTooLarge (compressed size %d bytes)", err, compressed.Len())
	}
}

func TestReadRedactsCredentialsAndQuery(t *testing.T) {
	const (
		user, password = "scraper", "s3cret-pw"
		query          = "token=tok-123&x=1"
	)
	var gotUser, gotPassword, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPassword, _ = r.BasicAuth()
		gotQuery = r.URL.RawQuery
		if r.URL.Path == "/denied" {
			// A careless server that echoes the request back.
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte("denied: " + user + ":" + password + " " + r.URL.String()))
			return
		}
		w.Header().Set("Content-Type", textFormat)
		w.Write([]byte("up 1\n"))
	}))
	defer srv.Close()

	withSecrets := func(base, path string) string {
		return strings.Replace(base, "http://", "http://"+user+":"+password+"@", 1) + path + "?" + query
	}
	secrets := []string{password, "tok-123", "token=", user + ":"}
	wantHidden := func(t *testing.T, what, text string) {
		t.Helper()
		for _, s := range secrets {
			if strings.Contains(text, s) {
				t.Errorf("%s contains %q: %s", what, s, text)
			}
		}
	}

	// Success: the request carries everything; the snapshot shows none of it.
	s := mustRead(t, newReader(t, PrometheusConfig{URL: withSecrets(srv.URL, "/metrics")}))
	if gotUser != user || gotPassword != password || gotQuery != query {
		t.Errorf("request had user %q password %q query %q, want the configured ones", gotUser, gotPassword, gotQuery)
	}
	wantEndpoint := strings.Replace(srv.URL, "http://", "http://xxxxx@", 1) + "/metrics?xxxxx"
	if s.Endpoint != wantEndpoint {
		t.Errorf("Endpoint = %q, want %q", s.Endpoint, wantEndpoint)
	}
	wantHidden(t, "snapshot endpoint", s.Endpoint)

	// Error status with the secrets echoed in the body.
	_, err := newReader(t, PrometheusConfig{URL: withSecrets(srv.URL, "/denied")}).Read(context.Background())
	if !errors.Is(err, ErrHTTPStatus) {
		t.Fatalf("error = %v, want ErrHTTPStatus", err)
	}
	wantHidden(t, "status error", err.Error())

	// Unreachable endpoint.
	_, err = newReader(t, PrometheusConfig{URL: withSecrets(strings.TrimSuffix(closedURL(t), "/metrics"), "/metrics")}).Read(context.Background())
	if !errors.Is(err, ErrFetch) {
		t.Fatalf("error = %v, want ErrFetch", err)
	}
	wantHidden(t, "network error", err.Error())

	// Timeout.
	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer slow.Close()
	_, err = newReader(t, PrometheusConfig{URL: withSecrets(slow.URL, "/metrics"), Timeout: 50 * time.Millisecond}).Read(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want a timeout", err)
	}
	wantHidden(t, "timeout error", err.Error())

	// Invalid configuration.
	_, err = NewPrometheusReader(PrometheusConfig{URL: "ftp://" + user + ":" + password + "@host/metrics?" + query})
	if !errors.Is(err, ErrInvalidEndpoint) {
		t.Fatalf("error = %v, want ErrInvalidEndpoint", err)
	}
	wantHidden(t, "configuration error", err.Error())
}

func TestNewPrometheusReaderValidation(t *testing.T) {
	tests := []struct {
		name string
		cfg  PrometheusConfig
		want string
	}{
		{"missing URL", PrometheusConfig{}, "URL is required"},
		{"no scheme", PrometheusConfig{URL: "localhost:8082/metrics"}, "http:// or https://"},
		{"other scheme", PrometheusConfig{URL: "ftp://host/metrics"}, "http:// or https://"},
		{"no host", PrometheusConfig{URL: "http:///metrics"}, "must include a host"},
		{"port zero", PrometheusConfig{URL: "http://host:0/metrics"}, "port"},
		{"port too large", PrometheusConfig{URL: "http://host:70000/metrics"}, "port"},
		{"fragment", PrometheusConfig{URL: "http://host/metrics#x"}, "fragment"},
		{"space", PrometheusConfig{URL: "http://host/me trics"}, "spaces"},
		{"too long", PrometheusConfig{URL: "http://host/" + strings.Repeat("a", MaxEndpointLen)}, "at most"},
		{"negative timeout", PrometheusConfig{URL: "http://host/metrics", Timeout: -time.Second}, "timeout"},
		{"negative size", PrometheusConfig{URL: "http://host/metrics", MaxBytes: -1}, "size"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewPrometheusReader(tt.cfg)
			if !errors.Is(err, ErrInvalidEndpoint) || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want ErrInvalidEndpoint mentioning %q", err, tt.want)
			}
		})
	}

	r := newReader(t, PrometheusConfig{URL: "https://host:9090/metrics"})
	if r.timeout != DefaultTimeout || r.maxBytes != DefaultMaxBytes || r.client == nil {
		t.Errorf("defaults not applied: timeout %v maxBytes %d client %v", r.timeout, r.maxBytes, r.client)
	}
}
