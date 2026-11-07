package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	serviceName = "regression-lab"
	metricsPath = "/metrics"
	workPath    = "/work"
	healthPath  = "/healthz"

	unmatchedRoute = "unmatched"
	otherMethod    = "OTHER"

	requestsMetric   = "regression_lab_http_requests_total"
	durationMetric   = "regression_lab_http_request_duration_seconds"
	inFlightMetric   = "regression_lab_http_requests_in_flight"
	degradedMetric   = "regression_lab_degraded"
	iterationsMetric = "regression_lab_work_iterations"
)

var durationBuckets = []float64{
	0.0001, 0.00025, 0.0005,
	0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5,
	1, 2.5, 5, 10,
}

var routeLabels = map[string]bool{
	healthPath: true,
	workPath:   true,
}

var methodLabels = map[string]bool{
	http.MethodGet: true, http.MethodHead: true, http.MethodPost: true,
	http.MethodPut: true, http.MethodPatch: true, http.MethodDelete: true,
	http.MethodOptions: true, http.MethodConnect: true, http.MethodTrace: true,
}

type healthResponse struct {
	Status  string `json:"status"`
	Service string `json:"service"`
}

type workResponse struct {
	Status     string `json:"status"`
	Mode       string `json:"mode"`
	Iterations int    `json:"iterations"`
	Mix        uint64 `json:"mix"`
}

type errorResponse struct {
	Error string `json:"error"`
}

type httpMetrics struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	inFlight prometheus.Gauge
}

func newHTTPMetrics(reg prometheus.Registerer) *httpMetrics {
	labels := []string{"method", "route", "status"}
	m := &httpMetrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: requestsMetric,
			Help: "HTTP requests completed by the regression lab, by method, route template, and response status. Excludes /metrics.",
		}, labels),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    durationMetric,
			Help:    "Time the regression lab spent handling an HTTP request, in seconds. Excludes /metrics.",
			Buckets: durationBuckets,
		}, labels),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: inFlightMetric,
			Help: "HTTP requests the regression lab is handling right now. Excludes /metrics.",
		}),
	}
	reg.MustRegister(m.requests, m.duration, m.inFlight)
	// Create the known GET /work 200 series before any traffic so the first
	// snapshot is comparable with later ones.
	m.requests.WithLabelValues(http.MethodGet, workPath, "200")
	m.duration.WithLabelValues(http.MethodGet, workPath, "200")
	return m
}

func (m *httpMetrics) observe(method, pattern string, status int, elapsed time.Duration) {
	labels := prometheus.Labels{
		"method": methodLabel(method),
		"route":  routeLabel(pattern),
		"status": strconv.Itoa(status),
	}
	m.requests.With(labels).Inc()
	m.duration.With(labels).Observe(elapsed.Seconds())
}

func methodLabel(method string) string {
	if methodLabels[method] {
		return method
	}
	return otherMethod
}

func routeLabel(pattern string) string {
	if _, path, ok := strings.Cut(pattern, " "); ok {
		pattern = path
	}
	if routeLabels[pattern] {
		return pattern
	}
	return unmatchedRoute
}

func newModeMetrics(reg prometheus.Registerer) (degraded, iters prometheus.Gauge) {
	degraded = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: degradedMetric,
		Help: "1 while the lab is in degraded mode (heavier CPU work per GET /work), 0 in baseline mode.",
	})
	iters = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: iterationsMetric,
		Help: "CPU-loop iterations currently executed for each GET /work.",
	})
	reg.MustRegister(degraded, iters)
	return degraded, iters
}

// newHandler returns the lab's HTTP handler, including Prometheus metrics.
// Each call has its own registry. A nil logger discards logs.
func newHandler(lab *Lab, logger *slog.Logger, metrics *httpMetrics, metricsHandler http.Handler) http.Handler {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+healthPath, handleHealthz)
	mux.HandleFunc(healthPath, methodNotAllowed("GET, HEAD"))
	mux.HandleFunc("GET "+workPath, handleWork(lab))
	mux.HandleFunc(workPath, methodNotAllowed("GET, HEAD"))
	mux.Handle("GET "+metricsPath, metricsHandler)
	mux.HandleFunc(metricsPath, methodNotAllowed("GET, HEAD"))
	mux.HandleFunc("/", notFound)
	return observeRequests(logger, metrics, mux)
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{Status: "ok", Service: serviceName})
}

func handleWork(lab *Lab) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		mode, iters, mix := lab.onWork()
		writeJSON(w, http.StatusOK, workResponse{Status: "ok", Mode: mode, Iterations: iters, Mix: mix})
	}
}

func methodNotAllowed(allow string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", allow)
		writeError(w, http.StatusMethodNotAllowed, fmt.Sprintf("method %s not allowed; use %s", r.Method, allow))
	}
}

func notFound(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotFound, "not found")
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, errorResponse{Error: message})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("writing response", "error", err)
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	if r.status == 0 && status >= 200 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

func (r *statusRecorder) statusOrOK() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

func observeRequests(logger *slog.Logger, metrics *httpMetrics, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		measured := r.URL.Path != metricsPath
		if measured {
			metrics.inFlight.Inc()
			defer metrics.inFlight.Dec()
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		elapsed := time.Since(start)
		status := rec.statusOrOK()
		if measured {
			metrics.observe(r.Method, r.Pattern, status, elapsed)
		}
		logger.Info("request",
			"method", r.Method,
			"pattern", r.Pattern,
			"status", status,
			"duration_ms", float64(elapsed.Microseconds())/1000,
		)
	})
}

func newLabHandler(labCfg labConfig, logger *slog.Logger) (*Lab, http.Handler) {
	reg := prometheus.NewRegistry()
	httpMetrics := newHTTPMetrics(reg)
	degraded, iters := newModeMetrics(reg)
	lab := newLab(labCfg.degradeAfter, labCfg.baselineIters, labCfg.degradedIters, degraded, iters)
	h := newHandler(lab, logger, httpMetrics, promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	return lab, h
}
