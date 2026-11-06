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

	// maxBodyBytes caps JSON request bodies. A valid request is a few
	// dozen bytes, so 4 KiB is generous while still stopping abuse.
	maxBodyBytes = 4 << 10
)

// createOrderRequest is the JSON body accepted by POST /orders.
type createOrderRequest struct {
	SKU      string `json:"sku"`
	Quantity int    `json:"quantity"`
}

// payOrderRequest is the JSON body accepted by POST /orders/{id}/pay. There is
// deliberately no amount (it is computed server-side) and no card data:
// unknown fields are rejected.
type payOrderRequest struct {
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
	store  *order.Store
	orders *order.Service
	payer  *order.Payer
}

// NewHandler returns the HTTP handler serving all Order Service routes.
//
// Every route is registered twice: once with its allowed method, and once
// without. A request using any other method falls through to the second
// registration and gets a JSON 405 with an Allow header. (Go's default 405
// response is plain text.) Unknown paths get a JSON 404 from the "/" route.
//
// inventory is how the Order Service reserves stock (order.InventoryReserver),
// and payments how it pays (order.PaymentProcessor). Either may be nil, in
// which case the endpoint that needs it answers 503.
func NewHandler(store *order.Store, inventory order.InventoryReserver, payments order.PaymentProcessor) http.Handler {
	h := &handler{
		store:  store,
		orders: order.NewService(store, inventory),
		payer:  order.NewPayer(store, payments),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.HandleFunc("/healthz", methodNotAllowed("GET, HEAD"))
	mux.HandleFunc("POST /orders", h.createOrder)
	mux.HandleFunc("/orders", methodNotAllowed("POST"))
	mux.HandleFunc("GET /orders/{id}", h.getOrder)
	mux.HandleFunc("/orders/{id}", methodNotAllowed("GET, HEAD"))
	mux.HandleFunc("POST /orders/{id}/reserve", h.reserveOrder)
	mux.HandleFunc("/orders/{id}/reserve", methodNotAllowed("POST"))
	mux.HandleFunc("POST /orders/{id}/pay", h.payOrder)
	mux.HandleFunc("/orders/{id}/pay", methodNotAllowed("POST"))
	mux.HandleFunc("/", notFound)
	return mux
}

// NewServer returns an http.Server for addr with conservative timeouts, so
// slow or stalled clients cannot hold connections open forever.
func NewServer(addr string, store *order.Store, inventory order.InventoryReserver, payments order.PaymentProcessor) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           NewHandler(store, inventory, payments),
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
	var req createOrderRequest
	if !decodeJSON(w, r, &req) {
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

// payOrder handles POST /orders/{id}/pay.
//
// Every confirmed outcome, approved or declined, is 200 with
// {"order": ..., "payment": ...}; check payment.status. A decline is a normal
// business result: the order stays reserved and can be paid with a new key.
func (h *handler) payOrder(w http.ResponseWriter, r *http.Request) {
	var req payOrderRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	id := r.PathValue("id")
	res, err := h.payer.Pay(r.Context(), id, req.PaymentMethod, req.IdempotencyKey)
	if err == nil {
		slog.Info("order payment", "order_id", res.Order.ID, "payment_id", res.Payment.ID,
			"payment_status", res.Payment.Status, "order_status", res.Order.Status)
		writeJSON(w, http.StatusOK, res)
		return
	}

	const unchanged = "; the order is unchanged and the request can be retried with the same idempotency_key"
	var verr *order.ValidationError
	switch {
	case errors.As(err, &verr):
		writeError(w, http.StatusUnprocessableEntity, verr.Message)
	case errors.Is(err, order.ErrNotFound):
		writeError(w, http.StatusNotFound, "order not found")
	case errors.Is(err, order.ErrNoPrice):
		writeError(w, http.StatusUnprocessableEntity, "order sku has no price")
	case errors.Is(err, order.ErrOrderNotReserved):
		writeError(w, http.StatusConflict, "order inventory must be reserved before payment")
	case errors.Is(err, order.ErrOrderAlreadyPaid):
		writeError(w, http.StatusConflict, "order is already paid")
	case errors.Is(err, order.ErrPaymentKeyConflict):
		writeError(w, http.StatusConflict, "idempotency_key was already used for this order with a different payment_method")
	case errors.Is(err, order.ErrPaymentApprovedElsewhere):
		slog.Warn("payment service reports an approval the order service has not recorded", "order_id", id)
		writeError(w, http.StatusConflict, "the payment service already has an approved payment for this order that is not recorded here; retry with the idempotency_key that produced it to recover it")
	case errors.Is(err, order.ErrPaymentRecordConflict):
		writeError(w, http.StatusConflict, "the order is already recorded as paid by a different payment")
	case errors.Is(err, order.ErrPaymentTimeout):
		slog.Warn("payment timed out", "order_id", id, "error", err)
		writeError(w, http.StatusGatewayTimeout, "payment service timed out; the payment outcome is unknown"+unchanged)
	case errors.Is(err, order.ErrPaymentNotConfigured):
		writeError(w, http.StatusServiceUnavailable, "payment service is not configured")
	case errors.Is(err, order.ErrPaymentUnavailable):
		slog.Warn("payment service unavailable", "order_id", id, "error", err)
		writeError(w, http.StatusServiceUnavailable, "payment service unavailable; the payment outcome is unknown"+unchanged)
	case errors.Is(err, order.ErrPaymentBadResponse):
		slog.Error("unexpected payment response", "order_id", id, "error", err)
		writeError(w, http.StatusBadGateway, "unexpected response from payment service; the payment outcome is unknown"+unchanged)
	default:
		slog.Error("paying for order", "order_id", id, "error", err)
		writeError(w, http.StatusInternalServerError, "internal server error")
	}
}

// decodeJSON reads r's body into v with the service's strict rules: at most
// maxBodyBytes, no unknown fields, exactly one JSON value. On failure it
// writes the error response and returns false.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(v); err != nil {
		writeDecodeError(w, err)
		return false
	}
	// Decode stops after the first JSON value. Make sure nothing follows it,
	// such as a second object or stray text.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			writeError(w, http.StatusBadRequest, "request body must contain a single JSON object")
			return false
		}
		writeDecodeError(w, err)
		return false
	}
	return true
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
