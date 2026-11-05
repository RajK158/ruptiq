// Package httpapi exposes the Payment Service over HTTP. It translates between
// HTTP/JSON and the payment package; business rules live in package payment.
package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/RajK158/ruptiq/demo/checkout/payment-service/internal/payment"
)

const (
	serviceName = "payment-service"

	// maxBodyBytes caps POST /payments request bodies. A valid request is
	// about 200 bytes, so 4 KiB is generous while still stopping abuse.
	maxBodyBytes = 4 << 10
)

// createPaymentRequest is the JSON body accepted by POST /payments. Unknown
// fields are rejected, so card numbers or other sensitive data sent by
// mistake are refused rather than silently accepted.
type createPaymentRequest struct {
	OrderID        string `json:"order_id"`
	AmountCents    int64  `json:"amount_cents"`
	Currency       string `json:"currency"`
	PaymentMethod  string `json:"payment_method"`
	IdempotencyKey string `json:"idempotency_key"`
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
	store  *payment.Store
	logger *slog.Logger
}

// NewHandler returns the HTTP handler serving all Payment Service routes,
// wrapped in request logging. A nil logger discards logs.
//
// Every route is registered twice: once with its allowed method, and once
// without. A request using any other method falls through to the second
// registration and gets a JSON 405 with an Allow header. (Go's default 405
// response is plain text.) Unknown paths get a JSON 404 from the "/" route.
func NewHandler(store *payment.Store, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	h := &handler{store: store, logger: logger}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("/healthz", methodNotAllowed("GET, HEAD"))
	mux.HandleFunc("POST /payments", h.createPayment)
	mux.HandleFunc("/payments", methodNotAllowed("POST"))
	mux.HandleFunc("GET /payments/{id}", h.getPayment)
	mux.HandleFunc("/payments/{id}", methodNotAllowed("GET, HEAD"))
	mux.HandleFunc("/", notFound)
	return logRequests(logger, mux)
}

// NewServer returns an http.Server for addr with conservative timeouts, so
// slow or stalled clients cannot hold connections open forever.
func NewServer(addr string, store *payment.Store, logger *slog.Logger) *http.Server {
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

// createPayment handles POST /payments.
func (h *handler) createPayment(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var req createPaymentRequest
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

	p, created, err := h.store.Create(payment.Request{
		OrderID:        req.OrderID,
		AmountCents:    req.AmountCents,
		Currency:       req.Currency,
		PaymentMethod:  req.PaymentMethod,
		IdempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		h.writeCreateError(w, err)
		return
	}

	if created {
		// Log identifiers and the outcome only; never the request body.
		h.logger.Info("payment created", "payment_id", p.ID, "order_id", p.OrderID, "status", p.Status)
		w.Header().Set("Location", "/payments/"+p.ID)
		writeJSON(w, http.StatusCreated, p)
		return
	}
	h.logger.Info("payment replayed", "payment_id", p.ID, "order_id", p.OrderID, "status", p.Status)
	writeJSON(w, http.StatusOK, p)
}

// writeCreateError maps a Store.Create failure to an HTTP response.
func (h *handler) writeCreateError(w http.ResponseWriter, err error) {
	var verr *payment.ValidationError
	switch {
	case errors.As(err, &verr):
		writeError(w, http.StatusUnprocessableEntity, verr.Message)
	case errors.Is(err, payment.ErrIdempotencyConflict):
		h.logger.Info("payment rejected: idempotency conflict")
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, payment.ErrOrderAlreadyPaid):
		h.logger.Info("payment rejected: order already paid")
		writeError(w, http.StatusConflict, err.Error())
	default:
		h.internalError(w, "creating payment", err)
	}
}

// getPayment handles GET /payments/{id}.
func (h *handler) getPayment(w http.ResponseWriter, r *http.Request) {
	p, err := h.store.Get(r.PathValue("id"))
	if errors.Is(err, payment.ErrNotFound) {
		writeError(w, http.StatusNotFound, "payment not found")
		return
	}
	if err != nil {
		h.internalError(w, "getting payment", err)
		return
	}
	writeJSON(w, http.StatusOK, p)
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
		// for example: json: unknown field "card_number". They name fields but
		// do not repeat field values.
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

// statusRecorder remembers the status code a handler wrote, for logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// logRequests logs one structured line per request after it finishes. It logs
// the method, path, status and duration only: never headers or bodies.
func logRequests(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", float64(time.Since(start).Microseconds())/1000,
		)
	})
}
