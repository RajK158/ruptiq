// Package httpapi exposes the Order Service over HTTP. It translates between
// HTTP/JSON and the order package; business rules live in package order.
package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/RajK158/ruptiq/demo/checkout/order-service/internal/order"
)

const (
	serviceName = "order-service"

	// maxBodyBytes caps POST /orders request bodies. A valid request is a few
	// dozen bytes, so 4 KiB is generous while still stopping abuse.
	maxBodyBytes = 4 << 10
)

// createOrderRequest is the JSON body accepted by POST /orders.
type createOrderRequest struct {
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
	store  *order.Store
	orders *order.Service
}

// NewHandler returns the HTTP handler serving all Order Service routes.
//
// Every route is registered twice: once with its allowed method, and once
// without. A request using any other method falls through to the second
// registration and gets a JSON 405 with an Allow header. (Go's default 405
// response is plain text.) Unknown paths get a JSON 404 from the "/" route.
//
// inventory is how the Order Service reserves stock; see order.InventoryReserver.
// It may be nil, in which case POST /orders/{id}/reserve answers 503.
func NewHandler(store *order.Store, inventory order.InventoryReserver) http.Handler {
	h := &handler{store: store, orders: order.NewService(store, inventory)}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("/healthz", methodNotAllowed("GET, HEAD"))
	mux.HandleFunc("POST /orders", h.createOrder)
	mux.HandleFunc("/orders", methodNotAllowed("POST"))
	mux.HandleFunc("GET /orders/{id}", h.getOrder)
	mux.HandleFunc("/orders/{id}", methodNotAllowed("GET, HEAD"))
	mux.HandleFunc("POST /orders/{id}/reserve", h.reserveOrder)
	mux.HandleFunc("/orders/{id}/reserve", methodNotAllowed("POST"))
	mux.HandleFunc("/", notFound)
	return mux
}

// NewServer returns an http.Server for addr with conservative timeouts, so
// slow or stalled clients cannot hold connections open forever.
func NewServer(addr string, store *order.Store, inventory order.InventoryReserver) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           NewHandler(store, inventory),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{Status: "ok", Service: serviceName})
}

// createOrder handles POST /orders.
func (h *handler) createOrder(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var req createOrderRequest
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

	o, err := order.New(req.SKU, req.Quantity)
	if err != nil {
		var verr *order.ValidationError
		if errors.As(err, &verr) {
			writeError(w, http.StatusUnprocessableEntity, verr.Message)
			return
		}
		slog.Error("creating order", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	h.store.Add(o)
	w.Header().Set("Location", "/orders/"+o.ID)
	writeJSON(w, http.StatusCreated, o)
}

// getOrder handles GET /orders/{id}.
func (h *handler) getOrder(w http.ResponseWriter, r *http.Request) {
	o, err := h.store.Get(r.PathValue("id"))
	if errors.Is(err, order.ErrNotFound) {
		writeError(w, http.StatusNotFound, "order not found")
		return
	}
	if err != nil {
		slog.Error("getting order", "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	writeJSON(w, http.StatusOK, o)
}

// reserveOrder handles POST /orders/{id}/reserve. It takes no request body.
//
// Success is always 200 with the updated order, whether this call reserved the
// stock or the order was already reserved: reserving is idempotent. The order
// is "reserved", not paid; nothing here completes checkout.
func (h *handler) reserveOrder(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// r.Context() ends when the client disconnects or the server shuts down,
	// which also cancels the call to the Inventory Service.
	o, err := h.orders.Reserve(r.Context(), id)
	if err == nil {
		slog.Info("order reserved", "order_id", o.ID, "reservation_id", o.ReservationID)
		writeJSON(w, http.StatusOK, o)
		return
	}

	switch {
	case errors.Is(err, order.ErrNotFound):
		writeError(w, http.StatusNotFound, "order not found")
	case errors.Is(err, order.ErrInsufficientStock):
		writeError(w, http.StatusConflict, "insufficient stock")
	case errors.Is(err, order.ErrInventoryConflict):
		writeError(w, http.StatusConflict, "inventory already holds a conflicting reservation for this order")
	case errors.Is(err, order.ErrUnknownSKU):
		// The order exists, but its SKU is not in the catalog. 422 rather than
		// 404, so clients cannot confuse it with "order not found".
		writeError(w, http.StatusUnprocessableEntity, "order sku is unknown to the inventory service")
	case errors.Is(err, order.ErrInventoryTimeout):
		slog.Warn("inventory reservation timed out", "order_id", id, "error", err)
		writeError(w, http.StatusGatewayTimeout, "inventory service timed out; the order is unchanged and the request can be retried")
	case errors.Is(err, order.ErrInventoryUnavailable), errors.Is(err, order.ErrInventoryNotConfigured):
		slog.Warn("inventory service unavailable", "order_id", id, "error", err)
		writeError(w, http.StatusServiceUnavailable, "inventory service unavailable; the order is unchanged and the request can be retried")
	case errors.Is(err, order.ErrInventoryBadResponse):
		slog.Error("unexpected inventory response", "order_id", id, "error", err)
		writeError(w, http.StatusBadGateway, "unexpected response from inventory service; the order is unchanged and the request can be retried")
	default:
		slog.Error("reserving order", "order_id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
	}
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
