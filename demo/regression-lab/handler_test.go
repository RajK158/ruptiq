package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

func testLab(t *testing.T, degradeAfter time.Duration, baseline, degraded int) (*Lab, http.Handler) {
	t.Helper()
	lab, h := newLabHandler(labConfig{
		degradeAfter:  degradeAfter,
		baselineIters: baseline,
		degradedIters: degraded,
	}, nil)
	return lab, h
}

func do(h http.Handler, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	h.ServeHTTP(rec, req)
	return rec
}

func scrapeErr(h http.Handler) (map[string]*dto.MetricFamily, error) {
	rec := do(h, http.MethodGet, metricsPath)
	if rec.Code != http.StatusOK {
		return nil, fmt.Errorf("GET /metrics status = %d, body %q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		return nil, fmt.Errorf("GET /metrics Content-Type = %q, want Prometheus text format", ct)
	}
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(rec.Body.String()))
	if err != nil {
		return nil, fmt.Errorf("invalid exposition: %v\n%s", err, rec.Body.String())
	}
	return families, nil
}

func scrape(t *testing.T, h http.Handler) map[string]*dto.MetricFamily {
	t.Helper()
	f, err := scrapeErr(h)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func series(families map[string]*dto.MetricFamily, name string, labelPairs ...string) *dto.Metric {
	f := families[name]
	if f == nil {
		return nil
	}
	want := make(map[string]string)
	for i := 0; i+1 < len(labelPairs); i += 2 {
		want[labelPairs[i]] = labelPairs[i+1]
	}
	for _, m := range f.GetMetric() {
		if len(m.GetLabel()) != len(want) {
			continue
		}
		match := true
		for _, l := range m.GetLabel() {
			if v, ok := want[l.GetName()]; !ok || l.GetValue() != v {
				match = false
			}
		}
		if match {
			return m
		}
	}
	return nil
}

func gaugeValue(families map[string]*dto.MetricFamily, name string) float64 {
	f := families[name]
	if f == nil || len(f.GetMetric()) != 1 {
		return -1
	}
	return f.GetMetric()[0].GetGauge().GetValue()
}

func TestHealthz(t *testing.T) {
	_, h := testLab(t, time.Hour, 100, 1000)
	rec := do(h, http.MethodGet, healthPath)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body healthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "ok" || body.Service != serviceName {
		t.Errorf("body = %+v", body)
	}
	// Health checks must not start the degrade timer.
	if gaugeValue(scrape(t, h), degradedMetric) != 0 {
		t.Fatal("healthz started degraded mode")
	}
}

func TestHealthzAndMetricsDoNotStartTimer(t *testing.T) {
	lab, h := testLab(t, 25*time.Millisecond, 100, 1_000)
	if do(h, http.MethodGet, healthPath).Code != http.StatusOK {
		t.Fatal("healthz")
	}
	if do(h, http.MethodGet, metricsPath).Code != http.StatusOK {
		t.Fatal("metrics")
	}
	time.Sleep(80 * time.Millisecond)
	if lab.degradedMode() {
		t.Fatal("healthz or metrics started the degrade timer")
	}
	if gaugeValue(scrape(t, h), degradedMetric) != 0 {
		t.Fatal("degraded gauge set without GET /work")
	}
	rec := do(h, http.MethodGet, workPath)
	var body workResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Mode != modeBaseline {
		t.Fatalf("first /work mode = %q, want baseline", body.Mode)
	}
}

func TestWorkBaseline(t *testing.T) {
	_, h := testLab(t, time.Hour, 1_000, 50_000)
	before := sink.Load()
	rec := do(h, http.MethodGet, workPath)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var body workResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "ok" || body.Mode != modeBaseline || body.Iterations != 1_000 || body.Mix == 0 {
		t.Errorf("body = %+v", body)
	}
	if sink.Load() == before {
		t.Fatal("CPU loop result was not observed")
	}
}

func TestWorkUsesRealCountersAndHistograms(t *testing.T) {
	_, h := testLab(t, time.Hour, 80_000, 800_000)
	fam := scrape(t, h)
	pre := series(fam, requestsMetric, "method", "GET", "route", workPath, "status", "200")
	if pre == nil || pre.GetCounter().GetValue() != 0 {
		t.Fatalf("GET /work 200 series missing or non-zero before traffic: %v", pre)
	}
	hist := series(fam, durationMetric, "method", "GET", "route", workPath, "status", "200")
	if hist == nil || hist.GetHistogram().GetSampleCount() != 0 {
		t.Fatalf("histogram series missing or already observed: %v", hist)
	}

	const n = 7
	for range n {
		if rec := do(h, http.MethodGet, workPath); rec.Code != http.StatusOK {
			t.Fatalf("GET /work: %d", rec.Code)
		}
	}
	fam = scrape(t, h)
	got := series(fam, requestsMetric, "method", "GET", "route", workPath, "status", "200")
	if got.GetCounter().GetValue() != n {
		t.Errorf("requests = %v, want %d", got.GetCounter().GetValue(), n)
	}
	hgot := series(fam, durationMetric, "method", "GET", "route", workPath, "status", "200")
	if hgot.GetHistogram().GetSampleCount() != n {
		t.Errorf("histogram count = %d, want %d", hgot.GetHistogram().GetSampleCount(), n)
	}
}

func TestModeSwitch(t *testing.T) {
	lab, h := testLab(t, 40*time.Millisecond, 200, 2_000)
	rec := do(h, http.MethodGet, workPath)
	var first workResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &first)
	if first.Mode != modeBaseline {
		t.Fatalf("first mode = %q", first.Mode)
	}
	if gaugeValue(scrape(t, h), degradedMetric) != 0 {
		t.Fatal("degraded gauge set before the delay")
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if lab.degradedMode() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !lab.degradedMode() {
		t.Fatal("did not enter degraded mode")
	}
	rec = do(h, http.MethodGet, workPath)
	var second workResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &second)
	if second.Mode != modeDegraded || second.Iterations != 2_000 {
		t.Errorf("second = %+v", second)
	}
	fam := scrape(t, h)
	if gaugeValue(fam, degradedMetric) != 1 {
		t.Errorf("degraded gauge = %v", gaugeValue(fam, degradedMetric))
	}
	if gaugeValue(fam, iterationsMetric) != 2_000 {
		t.Errorf("iterations gauge = %v", gaugeValue(fam, iterationsMetric))
	}
}

func TestImmediateDegrade(t *testing.T) {
	_, h := testLab(t, 0, 100, 500)
	rec := do(h, http.MethodGet, workPath)
	var body workResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Mode != modeDegraded {
		t.Errorf("mode = %q, want degraded when -degrade-after is 0", body.Mode)
	}
}

func TestConcurrentWorkAndModeChange(t *testing.T) {
	_, h := testLab(t, 10*time.Millisecond, 300, 3_000)
	var wg sync.WaitGroup
	errCh := make(chan error, 32)
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				rec := do(h, http.MethodGet, workPath)
				if rec.Code != http.StatusOK {
					errCh <- fmt.Errorf("status %d", rec.Code)
					return
				}
				var body workResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					errCh <- err
					return
				}
				if body.Mode != modeBaseline && body.Mode != modeDegraded {
					errCh <- fmt.Errorf("mode %q", body.Mode)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
	if !strings.Contains(scrape(t, h)[requestsMetric].String(), workPath) {
		t.Error("requests metric missing after concurrent load")
	}
}

func TestBoundedLabels(t *testing.T) {
	_, h := testLab(t, time.Hour, 100, 200)
	do(h, http.MethodGet, workPath)
	do(h, http.MethodGet, "/work/extra")
	do(h, http.MethodGet, "/secret/path?token=topsecret")
	do(h, http.MethodPost, workPath)
	do(h, "NONSTANDARD", workPath)

	rec := do(h, http.MethodGet, metricsPath)
	body := rec.Body.String()
	for _, leaked := range []string{"/work/extra", "/secret/path", "topsecret", "NONSTANDARD", `route="/metrics`} {
		if strings.Contains(body, leaked) {
			t.Errorf("metrics contain %q:\n%s", leaked, body)
		}
	}
	fam := scrape(t, h)
	if series(fam, requestsMetric, "method", "GET", "route", unmatchedRoute, "status", "404") == nil {
		t.Error("unmatched GET not counted")
	}
	if series(fam, requestsMetric, "method", "POST", "route", workPath, "status", "405") == nil {
		t.Error("POST /work 405 not counted")
	}
	if series(fam, requestsMetric, "method", otherMethod, "route", workPath, "status", "405") == nil {
		t.Error("nonstandard method not counted as OTHER")
	}
}

func TestMetricsPathNotCounted(t *testing.T) {
	_, h := testLab(t, time.Hour, 100, 200)
	do(h, http.MethodGet, metricsPath)
	do(h, http.MethodGet, metricsPath)
	fam := scrape(t, h)
	if s := series(fam, requestsMetric, "method", "GET", "route", unmatchedRoute, "status", "200"); s != nil && s.GetCounter().GetValue() != 0 {
		t.Errorf("scrapes counted as unmatched: %v", s.GetCounter().GetValue())
	}
	if series(fam, requestsMetric, "method", "GET", "route", metricsPath, "status", "200") != nil {
		t.Error("/metrics recorded as a route")
	}
}

func TestStartupAndShutdown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, handler := testLab(t, time.Hour, 100, 200)
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()

	addr := "http://" + ln.Addr().String()
	res, err := http.Get(addr + healthPath)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d", res.StatusCode)
	}

	if err := srv.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != http.ErrServerClosed {
		t.Errorf("Serve = %v, want ErrServerClosed", err)
	}
}

func TestRequireLoopback(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:8090", ":8090", "192.168.1.1:8090", "example.com:8090"} {
		if err := requireLoopback(addr); err == nil {
			t.Errorf("%s: want error", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:8090", "localhost:8090", "[::1]:8090"} {
		if err := requireLoopback(addr); err != nil {
			t.Errorf("%s: %v", addr, err)
		}
	}
}

func TestParseArgs(t *testing.T) {
	c, err := parseArgs(nil)
	if err != nil || c.addr != defaultAddr || c.degradeAfter != defaultDegradeAfter {
		t.Fatalf("defaults: %+v, %v", c, err)
	}
	if _, err := parseArgs([]string{"-addr", "0.0.0.0:80"}); err == nil {
		t.Fatal("non-loopback addr accepted")
	}
	if _, err := parseArgs([]string{"-degrade-after", "-1s"}); err == nil {
		t.Fatal("negative delay accepted")
	}
	if _, err := parseArgs([]string{"-baseline-iters", "-1"}); err == nil {
		t.Fatal("negative baseline-iters accepted")
	}
	if _, err := parseArgs([]string{"-degraded-iters", fmt.Sprintf("%d", maxIters+1)}); err == nil {
		t.Fatal("over-cap degraded-iters accepted")
	}
	if _, err := parseArgs([]string{"-baseline-iters", fmt.Sprintf("%d", maxIters+1)}); err == nil {
		t.Fatal("over-cap baseline-iters accepted")
	}
	c, err = parseArgs([]string{"-baseline-iters", fmt.Sprintf("%d", maxIters), "-degraded-iters", fmt.Sprintf("%d", maxIters)})
	if err != nil {
		t.Fatalf("max iters rejected: %v", err)
	}
	if c.baselineIters != maxIters || c.degradedIters != maxIters {
		t.Fatalf("max iters: %+v", c)
	}
}
