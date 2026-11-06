// Package httpapi exposes the Inventory Service over HTTP. It translates
// between HTTP/JSON and the inventory package; business rules live in package
// inventory.
package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/RajK158/ruptiq/demo/checkout/inventory-service/internal/inventory"
)

const (
	serviceName = "inventory-service"

	// maxBodyBytes caps POST /reservations request bodies. A valid request is
	// under 150 bytes, so 4 KiB is generous while still stopping abuse.
	maxBodyBytes = 4 << 10
)

// reserveRequest is the JSON body accepted by POST /reservations.
type reserveRequest struct {
	OrderID  string `json:"order_id"`
	SKU      string `json:"sku"`
	Quantity int    `json:"quantity"`
}

type healthResponse struct {
	Status  string `json:"status"`
	Service string `json:"service"`
}

type errorResponse struct {
	Error string `json:"error"`
}

// handler holds the dependencies the route handlers need.
type handler struct {
	store  *inventory.Store
	logger *slog.Logger
}

// NewHandler returns the HTTP handler serving all Inventory Service routes,
// including Prometheus metrics at /metrics, wrapped in request logging and
// HTTP metrics. A nil logger discards logs.
//
// Every route is registered twice: once with its allowed method, and once
// without. A request using any other method falls through to the second
// registration and gets a JSON 405 with an Allow header. (Go's default 405
// response is plain text.) Unknown paths get a JSON 404 from the "/" route.
//
// Each call creates its own metrics registry, so handlers never share metrics.
// Adding a route here means adding its template to routeLabels; until then its
// requests are labeled "unmatched".
func NewHandler(store *inventory.Store, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	h := &handler{store: store, logger: logger}
	metrics, metricsHandler := newMetrics(store)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("/healthz", methodNotAllowed("GET, HEAD"))
	mux.HandleFunc("GET /inventory/{sku}", h.getInventory)
	mux.HandleFunc("/inventory/{sku}", methodNotAllowed("GET, HEAD"))
	mux.HandleFunc("POST /reservations", h.createReservation)
	mux.HandleFunc("/reservations", methodNotAllowed("POST"))
	mux.Handle("GET "+metricsPath, metricsHandler)
	mux.HandleFunc(metricsPath, methodNotAllowed("GET, HEAD"))
	mux.HandleFunc("/", notFound)
	return observeRequests(logger, metrics, mux)
}

// NewServer returns an http.Server for addr with conservative timeouts, so
// slow or stalled clients cannot hold connections open forever.
func NewServer(addr string, store *inventory.Store, logger *slog.Logger) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           NewHandler(store, logger),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{Status: "ok", Service: serviceName})
}

// getInventory handles GET /inventory/{sku}.
func (h *handler) getInventory(w http.ResponseWriter, r *http.Request) {
	item, err := h.store.Get(r.PathValue("sku"))
	if errors.Is(err, inventory.ErrUnknownSKU) {
		writeError(w, http.StatusNotFound, "unknown sku")
		return
	}
	if err != nil {
		h.internalError(w, "getting inventory", err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

// createReservation handles POST /reservations.
func (h *handler) createReservation(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var req reserveRequest
	if err := dec.Decode(&req); err != nil {
		writeDecodeError(w, err)
		return
	}
	// Decode stops after the first JSON value. Make sure nothing follows it,
	// such as a second object or stray text.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			writeError(w, http.StatusBadRequest, "request body must contain a single JSON object")
			return
		}
		writeDecodeError(w, err)
		return
	}

	res, created, err := h.store.Reserve(inventory.ReserveRequest{
		OrderID:  req.OrderID,
		SKU:      req.SKU,
		Quantity: req.Quantity,
	})
	if err != nil {
		h.writeReserveError(w, req, err)
		return
	}

	if created {
		h.logger.Info("reservation created",
			"reservation_id", res.ID, "order_id", res.OrderID, "sku", res.SKU, "quantity", res.Quantity)
		writeJSON(w, http.StatusCreated, res)
		return
	}
	h.logger.Info("reservation replayed",
		"reservation_id", res.ID, "order_id", res.OrderID, "sku", res.SKU, "quantity", res.Quantity)
	writeJSON(w, http.StatusOK, res)
}

// writeReserveError maps a Store.Reserve failure to an HTTP response.
func (h *handler) writeReserveError(w http.ResponseWriter, req reserveRequest, err error) {
	var verr *inventory.ValidationError
	switch {
	case errors.As(err, &verr):
		writeError(w, http.StatusUnprocessableEntity, verr.Message)
	case errors.Is(err, inventory.ErrUnknownSKU):
		writeError(w, http.StatusNotFound, "unknown sku")
	case errors.Is(err, inventory.ErrInsufficientStock):
		h.logger.Info("reservation rejected: insufficient stock",
			"order_id", req.OrderID, "sku", req.SKU, "quantity", req.Quantity)
		writeError(w, http.StatusConflict, "insufficient stock")
	case errors.Is(err, inventory.ErrOrderConflict):
		h.logger.Info("reservation rejected: order_id conflict", "order_id", req.OrderID)
		writeError(w, http.StatusConflict, err.Error())
	default:
		h.internalError(w, "creating reservation", err)
	}
}

func (h *handler) internalError(w http.ResponseWriter, what string, err error) {
	h.logger.Error(what, "error", err)
	writeError(w, http.StatusInternalServerError, "internal server error")
}

// methodNotAllowed returns a handler that answers 405 and lists allowed methods.
func methodNotAllowed(allow string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", allow)
		writeError(w, http.StatusMethodNotAllowed, fmt.Sprintf("method %s not allowed; use %s", r.Method, allow))
	}
}

func notFound(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotFound, "not found")
}

// writeDecodeError turns a JSON decoding failure into a client-facing error.
func writeDecodeError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("request body too large (limit %d bytes)", tooLarge.Limit))
	case errors.Is(err, io.EOF):
		writeError(w, http.StatusBadRequest, "request body is empty")
	default:
		// The messages from encoding/json are short and describe the problem,
		// for example: json: unknown field "price".
		writeError(w, http.StatusBadRequest, "invalid JSON request body: "+err.Error())
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, errorResponse{Error: message})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status line is already sent, so all we can do is log.
		slog.Error("writing response", "error", err)
	}
}

// statusRecorder remembers the status code a handler sent, for logging and
// metrics.
type statusRecorder struct {
	http.ResponseWriter
	status int // 0 until a final (non-1xx) status is sent
}

func (r *statusRecorder) WriteHeader(status int) {
	// Like net/http, keep the first final status; later calls are no-ops there.
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

// Unwrap lets http.ResponseController reach the underlying writer's optional
// features, such as flushing and deadlines.
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// statusOrOK returns the recorded status, or 200 if the handler sent nothing,
// which is what net/http then sends.
func (r *statusRecorder) statusOrOK() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

// observeRequests logs one structured line per request after it finishes and
// records it in the HTTP metrics. Requests to the metrics endpoint are logged
// but not measured, so they don't count as application traffic.
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
			// ServeMux sets r.Pattern on the request it was given, so the
			// matched route template is visible here after it returns.
			metrics.observe(r.Method, r.Pattern, status, elapsed)
		}
		logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"duration_ms", float64(elapsed.Microseconds())/1000,
		)
	})
}
