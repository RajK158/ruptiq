package httpapi

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/RajK158/ruptiq/demo/checkout/inventory-service/internal/inventory"
)

// metricsPath serves Prometheus metrics. Requests to it are not recorded in
// the HTTP metrics, so scrapes never look like application traffic.
const metricsPath = "/metrics"

// Label values used when the real value could be unbounded.
const (
	// unmatchedRoute labels requests that matched no route template: unknown
	// paths, and requests ServeMux redirected before routing.
	unmatchedRoute = "unmatched"
	// otherMethod labels requests with a nonstandard HTTP method.
	otherMethod = "OTHER"
)

// routeLabels are the only route values the HTTP metrics can carry: the
// templates registered in NewHandler, never a raw request path.
var routeLabels = map[string]bool{
	"/healthz":         true,
	"/inventory/{sku}": true,
	"/reservations":    true,
}

var methodLabels = map[string]bool{
	http.MethodGet: true, http.MethodHead: true, http.MethodPost: true,
	http.MethodPut: true, http.MethodPatch: true, http.MethodDelete: true,
	http.MethodOptions: true, http.MethodConnect: true, http.MethodTrace: true,
}

// durationBuckets, in seconds, run from 100µs to 10s (the server's write
// timeout). Most requests to this in-memory service finish in well under a
// millisecond, so the low end is fine-grained.
var durationBuckets = []float64{
	0.0001, 0.00025, 0.0005,
	0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5,
	1, 2.5, 5, 10,
}

// httpMetrics records server-side measurements of each HTTP request.
type httpMetrics struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	inFlight prometheus.Gauge
}

func newHTTPMetrics(reg prometheus.Registerer) *httpMetrics {
	labels := []string{"method", "route", "status"}
	m := &httpMetrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "inventory_http_requests_total",
			Help: "HTTP requests completed by the Inventory Service, by method, route template, and response status. Excludes /metrics.",
		}, labels),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "inventory_http_request_duration_seconds",
			Help:    "Time the Inventory Service spent handling an HTTP request, from receiving it to the handler returning, in seconds. Excludes /metrics.",
			Buckets: durationBuckets,
		}, labels),
		inFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "inventory_http_requests_in_flight",
			Help: "HTTP requests the Inventory Service is handling right now. Excludes /metrics.",
		}),
	}
	reg.MustRegister(m.requests, m.duration, m.inFlight)
	return m
}

// observe records one finished request. pattern is the ServeMux pattern that
// matched it, such as "GET /inventory/{sku}", or "" if none did.
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

// routeLabel turns a pattern such as "GET /inventory/{sku}" into its route
// template, "/inventory/{sku}". Anything else, including the catch-all "/"
// pattern, becomes unmatchedRoute.
func routeLabel(pattern string) string {
	if _, path, ok := strings.Cut(pattern, " "); ok {
		pattern = path
	}
	if routeLabels[pattern] {
		return pattern
	}
	return unmatchedRoute
}

// inventoryCollector reports the store's current stock at scrape time. It
// keeps no state of its own, so the gauges cannot drift from the store.
type inventoryCollector struct {
	store     *inventory.Store
	available *prometheus.Desc
	reserved  *prometheus.Desc
}

func newInventoryCollector(store *inventory.Store) *inventoryCollector {
	return &inventoryCollector{
		store: store,
		available: prometheus.NewDesc("inventory_stock_available_units",
			"Units of a SKU that can still be reserved, read from the store at scrape time.",
			[]string{"sku"}, nil),
		reserved: prometheus.NewDesc("inventory_stock_reserved_units",
			"Units of a SKU held by reservations, read from the store at scrape time.",
			[]string{"sku"}, nil),
	}
}

func (c *inventoryCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.available
	ch <- c.reserved
}

// Collect takes one consistent snapshot; the store lock is released before
// any metric is sent or written to the response.
func (c *inventoryCollector) Collect(ch chan<- prometheus.Metric) {
	for _, item := range c.store.Snapshot() {
		ch <- prometheus.MustNewConstMetric(c.available, prometheus.GaugeValue, float64(item.Available), item.SKU)
		ch <- prometheus.MustNewConstMetric(c.reserved, prometheus.GaugeValue, float64(item.Reserved), item.SKU)
	}
}

// newMetrics creates a registry for one handler, holding its HTTP metrics and
// the store's stock gauges, and the handler that serves it.
func newMetrics(store *inventory.Store) (*httpMetrics, http.Handler) {
	reg := prometheus.NewRegistry()
	m := newHTTPMetrics(reg)
	reg.MustRegister(newInventoryCollector(store))
	return m, promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}
