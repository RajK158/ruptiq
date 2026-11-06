package httpapi

import (
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"

	"github.com/RajK158/ruptiq/demo/checkout/inventory-service/internal/inventory"
)

const (
	requestsTotal   = "inventory_http_requests_total"
	requestDuration = "inventory_http_request_duration_seconds"
	inFlight        = "inventory_http_requests_in_flight"
	stockAvailable  = "inventory_stock_available_units"
	stockReserved   = "inventory_stock_reserved_units"
)

// scrape fetches /metrics from h and parses it, failing the test unless the
// response is valid Prometheus text exposition.
func scrape(t *testing.T, h http.Handler) map[string]*dto.MetricFamily {
	t.Helper()
	families, err := scrapeErr(h)
	if err != nil {
		t.Fatal(err)
	}
	return families
}

// scrapeErr is scrape for use outside the test goroutine.
func scrapeErr(h http.Handler) (map[string]*dto.MetricFamily, error) {
	rec := do(h, http.MethodGet, metricsPath, "")
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

// series returns the sample of family name whose labels are exactly the
// given name/value pairs, or nil.
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
			if v, ok := want[l.GetName()]; !ok || v != l.GetValue() {
				match = false
			}
		}
		if match {
			return m
		}
	}
	return nil
}

// requestCounts returns every inventory_http_requests_total series as
// "METHOD route status" -> count.
func requestCounts(families map[string]*dto.MetricFamily) map[string]float64 {
	counts := make(map[string]float64)
	for _, m := range families[requestsTotal].GetMetric() {
		l := labelMap(m)
		counts[l["method"]+" "+l["route"]+" "+l["status"]] = m.GetCounter().GetValue()
	}
	return counts
}

func labelMap(m *dto.Metric) map[string]string {
	l := make(map[string]string)
	for _, p := range m.GetLabel() {
		l[p.GetName()] = p.GetValue()
	}
	return l
}

func wantStock(t *testing.T, families map[string]*dto.MetricFamily, sku string, available, reserved float64) {
	t.Helper()
	a := series(families, stockAvailable, "sku", sku)
	r := series(families, stockReserved, "sku", sku)
	if a == nil || r == nil {
		t.Fatalf("no stock gauges for %s", sku)
	}
	if got := a.GetGauge().GetValue(); got != available {
		t.Errorf("%s{sku=%q} = %v, want %v", stockAvailable, sku, got, available)
	}
	if got := r.GetGauge().GetValue(); got != reserved {
		t.Errorf("%s{sku=%q} = %v, want %v", stockReserved, sku, got, reserved)
	}
}

func wantInFlight(t *testing.T, families map[string]*dto.MetricFamily, want float64) {
	t.Helper()
	m := series(families, inFlight)
	if m == nil {
		t.Fatalf("%s missing", inFlight)
	}
	if got := m.GetGauge().GetValue(); got != want {
		t.Errorf("%s = %v, want %v", inFlight, got, want)
	}
}

func TestMetricsExposition(t *testing.T) {
	h := newTestHandler()
	do(h, http.MethodGet, "/healthz", "")

	families := scrape(t, h)
	wantTypes := map[string]dto.MetricType{
		requestsTotal:   dto.MetricType_COUNTER,
		requestDuration: dto.MetricType_HISTOGRAM,
		inFlight:        dto.MetricType_GAUGE,
		stockAvailable:  dto.MetricType_GAUGE,
		stockReserved:   dto.MetricType_GAUGE,
	}
	for name, typ := range wantTypes {
		f := families[name]
		if f == nil {
			t.Errorf("metric %s missing", name)
			continue
		}
		if f.GetType() != typ {
			t.Errorf("%s type = %v, want %v", name, f.GetType(), typ)
		}
		if f.GetHelp() == "" {
			t.Errorf("%s has no HELP text", name)
		}
	}
	if len(families) != len(wantTypes) {
		var names []string
		for name := range families {
			names = append(names, name)
		}
		slices.Sort(names)
		t.Errorf("metric families = %v, want exactly %d", names, len(wantTypes))
	}
}

func TestMetricsHandlersAreIndependent(t *testing.T) {
	a, b := newTestHandler(), newTestHandler()
	do(a, http.MethodGet, "/healthz", "")
	do(a, http.MethodPost, "/reservations", reserveBody("ord_1", "LAPTOP-001", 2))

	families := scrape(t, b)
	if _, ok := families[requestsTotal]; ok {
		t.Errorf("handler b reports requests made to handler a: %v", requestCounts(families))
	}
	wantStock(t, families, "LAPTOP-001", 10, 0)
}

func TestRequestMetricsByRouteMethodAndStatus(t *testing.T) {
	h := newTestHandler()
	requests := []struct {
		method, path, body string
		wantStatus         int
	}{
		{http.MethodGet, "/healthz", "", 200},
		{http.MethodHead, "/healthz", "", 200},
		{http.MethodGet, "/inventory/LAPTOP-001", "", 200},
		{http.MethodGet, "/inventory/PHONE-7", "", 200},
		{http.MethodGet, "/inventory/NOPE", "", 404},
		{http.MethodPost, "/reservations", reserveBody("ord_1", "LAPTOP-001", 1), 201},
		{http.MethodPost, "/reservations", reserveBody("ord_1", "LAPTOP-001", 1), 200},
		{http.MethodPost, "/reservations", reserveBody("ord_2", "LAPTOP-001", 50), 409},
		{http.MethodPost, "/reservations", `{"order_id":`, 400},
		{http.MethodPost, "/reservations", reserveBody("ord_3", "LAPTOP-001", 0), 422},
		{http.MethodPost, "/inventory/LAPTOP-001", "", 405},
		{http.MethodDelete, "/reservations", "", 405},
		{http.MethodGet, "/nope", "", 404},
	}
	for _, r := range requests {
		if rec := do(h, r.method, r.path, r.body); rec.Code != r.wantStatus {
			t.Fatalf("%s %s status = %d, want %d", r.method, r.path, rec.Code, r.wantStatus)
		}
	}
	// Scrapes in between must not show up in the counts.
	scrape(t, h)

	families := scrape(t, h)
	want := map[string]float64{
		"GET /healthz 200":          1,
		"HEAD /healthz 200":         1,
		"GET /inventory/{sku} 200":  2,
		"GET /inventory/{sku} 404":  1,
		"POST /reservations 201":    1,
		"POST /reservations 200":    1,
		"POST /reservations 409":    1,
		"POST /reservations 400":    1,
		"POST /reservations 422":    1,
		"POST /inventory/{sku} 405": 1,
		"DELETE /reservations 405":  1,
		"GET unmatched 404":         1,
	}
	got := requestCounts(families)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("request counts:\n got  %v\n want %v", got, want)
	}

	// The histogram has one observation per request, under the same labels.
	for _, m := range families[requestDuration].GetMetric() {
		l := labelMap(m)
		key := l["method"] + " " + l["route"] + " " + l["status"]
		if got := float64(m.GetHistogram().GetSampleCount()); got != want[key] {
			t.Errorf("duration observations for %s = %v, want %v", key, got, want[key])
		}
	}
	if n := len(families[requestDuration].GetMetric()); n != len(want) {
		t.Errorf("duration series = %d, want %d", n, len(want))
	}
	wantInFlight(t, families, 0)
}

func TestDurationHistogramObservations(t *testing.T) {
	m, metricsHandler := newMetrics(inventory.NewStore(nil))
	// Fixed durations, so the bucket counts are exact.
	for _, d := range []time.Duration{
		50 * time.Microsecond,
		300 * time.Microsecond,
		300 * time.Microsecond,
		2 * time.Millisecond,
		40 * time.Millisecond,
		3 * time.Second,
	} {
		m.observe(http.MethodGet, "GET /inventory/{sku}", http.StatusOK, d)
	}

	families := scrape(t, metricsHandler)
	s := series(families, requestDuration, "method", "GET", "route", "/inventory/{sku}", "status", "200")
	if s == nil {
		t.Fatal("histogram series missing")
	}
	hist := s.GetHistogram()
	if hist.GetSampleCount() != 6 {
		t.Errorf("sample count = %d, want 6", hist.GetSampleCount())
	}
	wantSum := 0.00005 + 0.0003 + 0.0003 + 0.002 + 0.04 + 3
	if diff := hist.GetSampleSum() - wantSum; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("sample sum = %v, want %v", hist.GetSampleSum(), wantSum)
	}

	// Cumulative counts at each upper bound.
	wantCumulative := map[float64]uint64{
		0.0001: 1, 0.00025: 1, 0.0005: 3,
		0.001: 3, 0.0025: 4, 0.005: 4, 0.01: 4, 0.025: 4, 0.05: 5, 0.1: 5, 0.25: 5, 0.5: 5,
		1: 5, 2.5: 5, 5: 6, 10: 6, math.Inf(1): 6,
	}
	var bounds []float64
	for _, b := range hist.GetBucket() {
		bounds = append(bounds, b.GetUpperBound())
		if want, ok := wantCumulative[b.GetUpperBound()]; !ok || b.GetCumulativeCount() != want {
			t.Errorf("bucket le=%v count = %d, want %d", b.GetUpperBound(), b.GetCumulativeCount(), want)
		}
	}
	if !slices.Equal(bounds, append(slices.Clone(durationBuckets), math.Inf(1))) {
		t.Errorf("bucket bounds = %v, want %v", bounds, durationBuckets)
	}
}

func TestInFlightGauge(t *testing.T) {
	const blocked = 3
	m, metricsHandler := newMetrics(inventory.NewStore(nil))
	entered := make(chan struct{})
	release := make(chan struct{})

	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(w http.ResponseWriter, _ *http.Request) {
		entered <- struct{}{}
		<-release
		w.WriteHeader(http.StatusNoContent)
	})
	mux.Handle("GET "+metricsPath, metricsHandler)
	h := observeRequests(slog.New(slog.DiscardHandler), m, mux)

	var wg sync.WaitGroup
	for range blocked {
		wg.Add(1)
		go func() {
			defer wg.Done()
			do(h, http.MethodGet, "/slow", "")
		}()
	}
	for range blocked {
		<-entered
	}

	// The scrape itself goes through h but must not count as in flight.
	wantInFlight(t, scrape(t, h), blocked)

	close(release)
	wg.Wait()
	families := scrape(t, h)
	wantInFlight(t, families, 0)
	if got := requestCounts(families); got["GET unmatched 204"] != blocked || len(got) != 1 {
		t.Errorf("request counts = %v, want only %d x GET unmatched 204", got, blocked)
	}
}

func TestInventoryGaugesInitialValues(t *testing.T) {
	families := scrape(t, NewHandler(inventory.NewStore(inventory.SeedStock()), nil))
	wantStock(t, families, "LAPTOP-001", 10, 0)
	wantStock(t, families, "PHONE-7", 50, 0)
	wantStock(t, families, "HEADPHONES-01", 100, 0)
	for _, name := range []string{stockAvailable, stockReserved} {
		if n := len(families[name].GetMetric()); n != 3 {
			t.Errorf("%s has %d series, want one per catalog SKU (3)", name, n)
		}
	}
}

func TestInventoryGaugesAfterReservation(t *testing.T) {
	h := newTestHandler()
	if rec := do(h, http.MethodPost, "/reservations", reserveBody("ord_1", "LAPTOP-001", 2)); rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	families := scrape(t, h)
	wantStock(t, families, "LAPTOP-001", 8, 2)
	wantStock(t, families, "PHONE-7", 5, 0)
}

func TestInventoryGaugesAfterIdempotentReplay(t *testing.T) {
	h := newTestHandler()
	body := reserveBody("ord_retry", "LAPTOP-001", 3)
	if rec := do(h, http.MethodPost, "/reservations", body); rec.Code != http.StatusCreated {
		t.Fatalf("first status = %d, want 201", rec.Code)
	}
	for i := 0; i < 3; i++ {
		if rec := do(h, http.MethodPost, "/reservations", body); rec.Code != http.StatusOK {
			t.Fatalf("replay %d status = %d, want 200", i, rec.Code)
		}
	}

	families := scrape(t, h)
	wantStock(t, families, "LAPTOP-001", 7, 3)
	counts := requestCounts(families)
	if counts["POST /reservations 201"] != 1 || counts["POST /reservations 200"] != 3 {
		t.Errorf("request counts = %v, want 1 created and 3 replayed", counts)
	}
}

func TestInventoryGaugesAfterRejectedReservations(t *testing.T) {
	h := newTestHandler()
	if rec := do(h, http.MethodPost, "/reservations", reserveBody("ord_1", "LAPTOP-001", 2)); rec.Code != http.StatusCreated {
		t.Fatalf("setup status = %d, want 201", rec.Code)
	}
	rejected := []struct {
		name, body string
		wantStatus int
	}{
		{"insufficient stock", reserveBody("ord_2", "LAPTOP-001", 9), http.StatusConflict},
		{"conflicting retry", reserveBody("ord_1", "LAPTOP-001", 3), http.StatusConflict},
		{"unknown sku", reserveBody("ord_3", "NOPE", 1), http.StatusNotFound},
		{"invalid quantity", reserveBody("ord_4", "PHONE-7", 0), http.StatusUnprocessableEntity},
		{"malformed body", `{"order_id":`, http.StatusBadRequest},
	}
	for _, r := range rejected {
		if rec := do(h, http.MethodPost, "/reservations", r.body); rec.Code != r.wantStatus {
			t.Fatalf("%s: status = %d, want %d", r.name, rec.Code, r.wantStatus)
		}
	}

	families := scrape(t, h)
	wantStock(t, families, "LAPTOP-001", 8, 2)
	wantStock(t, families, "PHONE-7", 5, 0)
	if n := len(families[stockAvailable].GetMetric()); n != 2 {
		t.Errorf("%s has %d series, want 2: an unknown SKU must not add one", stockAvailable, n)
	}
}

// TestConcurrentRequestsAndScrapes sends 150 simultaneous reservations for
// 10 units through a real HTTP server while /metrics is scraped repeatedly.
// Mainly useful under `go test -race`.
func TestConcurrentRequestsAndScrapes(t *testing.T) {
	const (
		units    = 10
		attempts = 150
		scrapes  = 30
	)
	h := NewHandler(inventory.NewStore(map[string]int{"LAPTOP-001": units}), nil)
	srv := httptest.NewServer(h)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			resp, err := http.Post(srv.URL+"/reservations", "application/json",
				strings.NewReader(reserveBody(fmt.Sprintf("ord_%d", i), "LAPTOP-001", 1)))
			if err != nil {
				t.Errorf("request %d: %v", i, err)
				return
			}
			resp.Body.Close()
		}()
	}
	for range scrapes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			families, err := scrapeErr(h)
			if err != nil {
				t.Error(err)
				return
			}
			// Each scrape sees whole reservations only.
			a := series(families, stockAvailable, "sku", "LAPTOP-001").GetGauge().GetValue()
			r := series(families, stockReserved, "sku", "LAPTOP-001").GetGauge().GetValue()
			if a+r != units || a < 0 {
				t.Errorf("inconsistent scrape: available %v reserved %v", a, r)
			}
		}()
	}
	close(start)
	wg.Wait()
	srv.Close() // waits for every request to finish, including its metrics

	families := scrape(t, h)
	wantStock(t, families, "LAPTOP-001", 0, units)
	wantInFlight(t, families, 0)
	want := map[string]float64{
		"POST /reservations 201": units,
		"POST /reservations 409": attempts - units,
	}
	if got := requestCounts(families); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("request counts = %v, want %v", got, want)
	}
	var observations uint64
	for _, m := range families[requestDuration].GetMetric() {
		observations += m.GetHistogram().GetSampleCount()
	}
	if observations != attempts {
		t.Errorf("duration observations = %d, want %d", observations, attempts)
	}
}

// TestMetricLabelsAreBounded sends requests whose paths, methods, and bodies
// contain IDs and secrets. None of them may become a label value.
func TestMetricLabelsAreBounded(t *testing.T) {
	h := newTestHandler()
	sent := 0
	send := func(method, path, body string) {
		do(h, method, path, body)
		sent++
	}
	for i := range 50 {
		send(http.MethodGet, fmt.Sprintf("/inventory/SKU-%d", i), "")
		send(http.MethodGet, fmt.Sprintf("/orders/ord_%d", i), "")
		send(http.MethodGet, fmt.Sprintf("/reservations/res_%d", i), "")
	}
	send(http.MethodGet, "/inventory/a/b", "")
	send(http.MethodGet, "/inventory/LAPTOP-001?api_key=hunter2", "")
	send(http.MethodGet, "//inventory/LAPTOP-001", "")
	send(http.MethodGet, "/inventory/../healthz", "")
	send("PURGE", "/inventory/LAPTOP-001", "")
	send("X-SECRET-METHOD", "/healthz", "")
	send(http.MethodPost, "/reservations", reserveBody("ord_secret_42", "LAPTOP-001", 1))
	send(http.MethodPost, "/reservations", reserveBody("ord_secret_43", "SKU-UNKNOWN-9", 1))
	send(http.MethodPost, "/reservations", `{"order_id":"ord_x","password":"hunter2"}`)
	send(http.MethodGet, "/metrics/extra", "")
	scrape(t, h)

	rec := do(h, http.MethodGet, metricsPath, "")
	body := rec.Body.String()
	for _, leaked := range []string{"ord_", "res_", "SKU-1", "SKU-UNKNOWN", "hunter2", "api_key", "secret", "SECRET", "/orders", "/inventory/a", `route="/metrics`} {
		if strings.Contains(body, leaked) {
			t.Errorf("metrics contain %q:\n%s", leaked, body)
		}
	}

	families := scrape(t, h)
	allowedRoutes := []string{"/healthz", "/inventory/{sku}", "/reservations", unmatchedRoute}
	allowedMethods := []string{"GET", "POST", otherMethod}
	var total float64
	for _, m := range families[requestsTotal].GetMetric() {
		l := labelMap(m)
		if !slices.Contains(allowedRoutes, l["route"]) {
			t.Errorf("unexpected route label %q", l["route"])
		}
		if !slices.Contains(allowedMethods, l["method"]) {
			t.Errorf("unexpected method label %q", l["method"])
		}
		total += m.GetCounter().GetValue()
	}
	if total != float64(sent) {
		t.Errorf("requests counted = %v, want %d (every request except scrapes)", total, sent)
	}
	for _, name := range []string{stockAvailable, stockReserved} {
		for _, m := range families[name].GetMetric() {
			if sku := labelMap(m)["sku"]; sku != "LAPTOP-001" && sku != "PHONE-7" {
				t.Errorf("%s has non-catalog sku label %q", name, sku)
			}
		}
	}
}

func TestStatusRecorder(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		want    int
	}{
		{"nothing written", func(http.ResponseWriter, *http.Request) {}, 200},
		{"body without WriteHeader", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("x")) }, 200},
		{"explicit status", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }, 418},
		{"first status wins", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTeapot)
			w.WriteHeader(http.StatusInternalServerError)
		}, 418},
		{"informational status is skipped", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusEarlyHints)
			w.WriteHeader(http.StatusAccepted)
		}, 202},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, metricsHandler := newMetrics(inventory.NewStore(nil))
			observeRequests(slog.New(slog.DiscardHandler), m, tt.handler).
				ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
			counts := requestCounts(scrape(t, metricsHandler))
			if key := fmt.Sprintf("GET unmatched %d", tt.want); counts[key] != 1 || len(counts) != 1 {
				t.Errorf("request counts = %v, want one %s", counts, key)
			}
		})
	}
}

func TestStatusRecorderKeepsOptionalInterfaces(t *testing.T) {
	m, _ := newMetrics(inventory.NewStore(nil))
	var flushErr error
	h := observeRequests(slog.New(slog.DiscardHandler), m, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("partial"))
		flushErr = http.NewResponseController(w).Flush()
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if flushErr != nil {
		t.Errorf("Flush through the recorder: %v", flushErr)
	}
	if !rec.Flushed {
		t.Error("underlying writer was not flushed")
	}
}
