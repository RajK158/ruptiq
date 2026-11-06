package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RajK158/ruptiq/demo/checkout/order-service/internal/inventoryclient"
	"github.com/RajK158/ruptiq/demo/checkout/order-service/internal/order"
	"github.com/RajK158/ruptiq/demo/checkout/order-service/internal/paymentclient"
)

// fakePaymentService simulates the Payment Service's POST /payments over real
// HTTP: global idempotency keys, at most one approval per order, outcome
// chosen by payment_method, and the same status codes and error messages. It
// is deliberately independent of the real service's code.
type fakePaymentService struct {
	mu       sync.Mutex
	byKey    map[string]fakePayment
	approved map[string]string // order_id -> payment id
	requests int

	// dropNextResponse makes the next request be processed normally, then
	// drops the connection before any response is sent.
	dropNextResponse bool
	// delay holds every response this long (to provoke client timeouts).
	delay time.Duration
	// rawResponse, if set, is sent instead of real handling.
	rawStatus   int
	rawResponse string
}

type fakePaymentRequest struct {
	OrderID        string `json:"order_id"`
	AmountCents    int64  `json:"amount_cents"`
	Currency       string `json:"currency"`
	PaymentMethod  string `json:"payment_method"`
	IdempotencyKey string `json:"idempotency_key"`
}

type fakePayment struct {
	ID          string `json:"id"`
	OrderID     string `json:"order_id"`
	AmountCents int64  `json:"amount_cents"`
	Currency    string `json:"currency"`
	Status      string `json:"status"`
	CreatedAt   string `json:"created_at"`

	req fakePaymentRequest
}

func newFakePaymentService() *fakePaymentService {
	return &fakePaymentService{byKey: make(map[string]fakePayment), approved: make(map[string]string)}
}

func (f *fakePaymentService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/payments" {
		http.NotFound(w, r)
		return
	}
	var req fakePaymentRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeFake(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	if req.AmountCents < 1 || req.Currency != "USD" || req.IdempotencyKey == "" || len(req.IdempotencyKey) > 128 ||
		(req.PaymentMethod != "test_approve" && req.PaymentMethod != "test_decline") {
		writeFake(w, http.StatusUnprocessableEntity, map[string]string{"error": "invalid payment"})
		return
	}

	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-r.Context().Done():
			return
		}
	}

	f.mu.Lock()
	f.requests++
	if f.rawResponse != "" || f.rawStatus != 0 {
		status, body := f.rawStatus, f.rawResponse
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
		return
	}
	status, body := f.payLocked(req)
	drop := f.dropNextResponse
	f.dropNextResponse = false
	f.mu.Unlock()

	if drop {
		// The payment is committed; now lose the answer.
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
		return
	}
	writeFake(w, status, body)
}

func (f *fakePaymentService) payLocked(req fakePaymentRequest) (int, any) {
	if prev, ok := f.byKey[req.IdempotencyKey]; ok {
		if prev.req == req {
			return http.StatusOK, prev
		}
		return http.StatusConflict, map[string]string{"error": "idempotency_key was already used with different payment details"}
	}
	if _, ok := f.approved[req.OrderID]; ok {
		return http.StatusConflict, map[string]string{"error": "order already has an approved payment"}
	}
	status := "approved"
	if req.PaymentMethod == "test_decline" {
		status = "declined"
	}
	p := fakePayment{
		ID:          fmt.Sprintf("pay_%d", len(f.byKey)+1),
		OrderID:     req.OrderID,
		AmountCents: req.AmountCents,
		Currency:    req.Currency,
		Status:      status,
		CreatedAt:   "2026-11-06T00:00:00Z",
		req:         req,
	}
	f.byKey[req.IdempotencyKey] = p
	if status == "approved" {
		f.approved[req.OrderID] = p.ID
	}
	return http.StatusCreated, p
}

func (f *fakePaymentService) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

func (f *fakePaymentService) paymentCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.byKey)
}

func (f *fakePaymentService) approvalsFor(orderID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.byKey {
		if p.OrderID == orderID && p.Status == "approved" {
			n++
		}
	}
	return n
}

func (f *fakePaymentService) set(fn func(f *fakePaymentService)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// setupCheckout wires an Order Service handler to fake Inventory and Payment
// Services over real HTTP, using the production clients.
func setupCheckout(t *testing.T, pay *fakePaymentService, timeout time.Duration) http.Handler {
	t.Helper()
	inv := newFakeInventoryService(map[string]int{"LAPTOP-001": 1000, "PHONE-7": 1000, "HEADPHONES-01": 1000})
	invSrv := httptest.NewServer(inv)
	t.Cleanup(invSrv.Close)
	paySrv := httptest.NewServer(pay)
	t.Cleanup(paySrv.Close)

	invClient := mustInventoryClient(t, invSrv.URL)
	payClient, err := paymentclient.New(paySrv.URL, timeout)
	if err != nil {
		t.Fatal(err)
	}
	return NewHandler(order.NewStore(), invClient, payClient)
}

func mustInventoryClient(t *testing.T, url string) *inventoryclient.Client {
	t.Helper()
	c, err := inventoryclient.New(url, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// reservedOrder creates an order for 2 laptops and reserves it.
func reservedOrder(t *testing.T, h http.Handler) order.Order {
	t.Helper()
	o := createOrder(t, h, "LAPTOP-001", 2)
	if rec := reserve(h, o.ID); rec.Code != http.StatusOK {
		t.Fatalf("reserve: status %d (body %q)", rec.Code, rec.Body.String())
	}
	return getOrder(t, h, o.ID)
}

func pay(h http.Handler, id, method, key string) *httptest.ResponseRecorder {
	return do(h, http.MethodPost, "/orders/"+id+"/pay",
		fmt.Sprintf(`{"payment_method":%q,"idempotency_key":%q}`, method, key))
}

const laptopX2Cents = 2 * 129_999

func TestPayOrderApproved(t *testing.T) {
	ps := newFakePaymentService()
	h := setupCheckout(t, ps, time.Second)
	o := reservedOrder(t, h)

	rec := pay(h, o.ID, "test_approve", "attempt-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	got := decode[order.PayResult](t, rec)
	if got.Payment != (order.PaymentResult{ID: "pay_1", Status: order.PaymentApproved, AmountCents: laptopX2Cents, Currency: "USD"}) {
		t.Errorf("payment = %+v", got.Payment)
	}
	if got.Order.Status != order.StatusPaid || got.Order.PaymentID != "pay_1" ||
		got.Order.PaymentIdempotencyKey != "attempt-1" || got.Order.ReservationID != o.ReservationID {
		t.Errorf("order = %+v", got.Order)
	}

	// GET /orders/{id} shows the paid state.
	stored := getOrder(t, h, o.ID)
	if stored.Status != order.StatusPaid || stored.PaymentID != "pay_1" {
		t.Errorf("stored order = %+v, want paid with pay_1", stored)
	}

	// The Payment Service got the server-computed amount and an
	// order-scoped key.
	ps.mu.Lock()
	p, ok := ps.byKey[o.ID+":attempt-1"]
	ps.mu.Unlock()
	if !ok || p.req.AmountCents != laptopX2Cents || p.req.OrderID != o.ID {
		t.Errorf("upstream payment = %+v (found %v)", p.req, ok)
	}
}

func TestPayOrderDeclineKeepsReservedThenApproveWithNewKey(t *testing.T) {
	ps := newFakePaymentService()
	h := setupCheckout(t, ps, time.Second)
	o := reservedOrder(t, h)

	rec := pay(h, o.ID, "test_decline", "attempt-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("decline: status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	got := decode[order.PayResult](t, rec)
	if got.Payment.Status != order.PaymentDeclined || got.Order.Status != order.StatusReserved || got.Order.PaymentID != "" {
		t.Errorf("decline result = %+v", got)
	}
	if stored := getOrder(t, h, o.ID); stored.Status != order.StatusReserved {
		t.Errorf("after decline: status %q, want reserved", stored.Status)
	}

	// The declined key cannot be reused to approve.
	wantError(t, pay(h, o.ID, "test_approve", "attempt-1"), http.StatusConflict)

	rec = pay(h, o.ID, "test_approve", "attempt-2")
	if rec.Code != http.StatusOK {
		t.Fatalf("approve: status = %d (body %q)", rec.Code, rec.Body.String())
	}
	got = decode[order.PayResult](t, rec)
	if got.Payment.Status != order.PaymentApproved || got.Order.Status != order.StatusPaid || got.Order.PaymentID != got.Payment.ID {
		t.Errorf("approve result = %+v", got)
	}
	if ps.approvalsFor(o.ID) != 1 {
		t.Errorf("approvals = %d, want 1", ps.approvalsFor(o.ID))
	}
}

func TestPayOrderIdenticalReplay(t *testing.T) {
	ps := newFakePaymentService()
	h := setupCheckout(t, ps, time.Second)
	o := reservedOrder(t, h)

	first := pay(h, o.ID, "test_approve", "attempt-1")
	if first.Code != http.StatusOK {
		t.Fatalf("first: status %d", first.Code)
	}
	requests, payments := ps.requestCount(), ps.paymentCount()
	for i := 0; i < 3; i++ {
		rec := pay(h, o.ID, "test_approve", "attempt-1")
		if rec.Code != http.StatusOK || rec.Body.String() != first.Body.String() {
			t.Errorf("replay %d: status %d, body %q; want 200 and %q", i, rec.Code, rec.Body.String(), first.Body.String())
		}
	}
	if ps.paymentCount() != payments || ps.requestCount() != requests {
		t.Errorf("replays reached the payment service: payments %d->%d, requests %d->%d",
			payments, ps.paymentCount(), requests, ps.requestCount())
	}
}

func TestPayOrderConflictingAttemptsAfterPayment(t *testing.T) {
	ps := newFakePaymentService()
	h := setupCheckout(t, ps, time.Second)
	o := reservedOrder(t, h)
	if rec := pay(h, o.ID, "test_approve", "attempt-1"); rec.Code != http.StatusOK {
		t.Fatalf("approve: status %d", rec.Code)
	}

	wantError(t, pay(h, o.ID, "test_approve", "attempt-2"), http.StatusConflict) // new key
	wantError(t, pay(h, o.ID, "test_decline", "attempt-3"), http.StatusConflict) // new key, decline
	wantError(t, pay(h, o.ID, "test_decline", "attempt-1"), http.StatusConflict) // same key, other method

	if stored := getOrder(t, h, o.ID); stored.Status != order.StatusPaid || stored.PaymentID != "pay_1" {
		t.Errorf("stored order = %+v, want still paid with pay_1", stored)
	}
	if ps.paymentCount() != 1 {
		t.Errorf("payments = %d, want 1", ps.paymentCount())
	}
	// Reserving a paid order does not move it backward.
	if rec := reserve(h, o.ID); rec.Code != http.StatusOK || decode[order.Order](t, rec).Status != order.StatusPaid {
		t.Errorf("reserve on paid order: status %d, body %q", rec.Code, rec.Body.String())
	}
}

func TestPayOrderKeysAreScopedPerOrder(t *testing.T) {
	ps := newFakePaymentService()
	h := setupCheckout(t, ps, time.Second)
	a, b := reservedOrder(t, h), reservedOrder(t, h)
	for _, o := range []order.Order{a, b} {
		if rec := pay(h, o.ID, "test_approve", "attempt-1"); rec.Code != http.StatusOK {
			t.Errorf("order %s: status %d (body %q)", o.ID, rec.Code, rec.Body.String())
		}
	}
	if ps.paymentCount() != 2 {
		t.Errorf("payments = %d, want 2", ps.paymentCount())
	}
}

func TestPayOrderConcurrentDistinctKeys(t *testing.T) {
	ps := newFakePaymentService()
	h := setupCheckout(t, ps, 5*time.Second)
	o := reservedOrder(t, h)

	const workers = 100
	codes := make([]int, workers)
	bodies := make([]string, workers)
	runConcurrently(workers, func(i int) {
		rec := pay(h, o.ID, "test_approve", fmt.Sprintf("attempt-%d", i))
		codes[i], bodies[i] = rec.Code, rec.Body.String()
	})

	ok, conflict := 0, 0
	for i, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
			conflict++
		default:
			t.Errorf("request %d: status %d (body %q)", i, c, bodies[i])
		}
	}
	if ok != 1 || conflict != workers-1 {
		t.Errorf("200s = %d, 409s = %d; want 1 and %d", ok, conflict, workers-1)
	}
	if ps.approvalsFor(o.ID) != 1 {
		t.Errorf("approvals = %d, want 1", ps.approvalsFor(o.ID))
	}
	stored := getOrder(t, h, o.ID)
	if stored.Status != order.StatusPaid || stored.PaymentID == "" {
		t.Errorf("stored order = %+v", stored)
	}
}

func TestPayOrderConcurrentIdenticalRetries(t *testing.T) {
	ps := newFakePaymentService()
	h := setupCheckout(t, ps, 5*time.Second)
	o := reservedOrder(t, h)

	const workers = 100
	results := make([]order.PayResult, workers)
	codes := make([]int, workers)
	runConcurrently(workers, func(i int) {
		rec := pay(h, o.ID, "test_approve", "attempt-1")
		codes[i] = rec.Code
		json.Unmarshal(rec.Body.Bytes(), &results[i])
	})
	for i := range codes {
		if codes[i] != http.StatusOK {
			t.Fatalf("request %d: status %d", i, codes[i])
		}
		if results[i] != results[0] || results[i].Payment.ID != "pay_1" || results[i].Order.Status != order.StatusPaid {
			t.Fatalf("result %d = %+v, want identical paid result with pay_1", i, results[i])
		}
	}
	if ps.paymentCount() != 1 {
		t.Errorf("payments = %d, want 1", ps.paymentCount())
	}
}

func TestPayOrderTimeout(t *testing.T) {
	ps := newFakePaymentService()
	ps.delay = 2 * time.Second
	h := setupCheckout(t, ps, 50*time.Millisecond)
	o := reservedOrder(t, h)

	rec := pay(h, o.ID, "test_approve", "attempt-1")
	wantError(t, rec, http.StatusGatewayTimeout)
	if !strings.Contains(rec.Body.String(), "same idempotency_key") {
		t.Errorf("error does not explain how to retry: %q", rec.Body.String())
	}
	if stored := getOrder(t, h, o.ID); stored.Status != order.StatusReserved {
		t.Errorf("status = %q after timeout, want reserved", stored.Status)
	}
}

func TestPayOrderPaymentServiceDown(t *testing.T) {
	inv := newFakeInventoryService(map[string]int{"LAPTOP-001": 10})
	invSrv := httptest.NewServer(inv)
	t.Cleanup(invSrv.Close)
	paySrv := httptest.NewServer(newFakePaymentService())
	url := paySrv.URL
	paySrv.Close()
	payClient, err := paymentclient.New(url, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(order.NewStore(), mustInventoryClient(t, invSrv.URL), payClient)
	o := reservedOrder(t, h)

	wantError(t, pay(h, o.ID, "test_approve", "attempt-1"), http.StatusServiceUnavailable)
	if stored := getOrder(t, h, o.ID); stored.Status != order.StatusReserved {
		t.Errorf("status = %q, want reserved", stored.Status)
	}
}

func TestPayOrderNotConfigured(t *testing.T) {
	invSrv := httptest.NewServer(newFakeInventoryService(map[string]int{"LAPTOP-001": 10}))
	t.Cleanup(invSrv.Close)
	h := NewHandler(order.NewStore(), mustInventoryClient(t, invSrv.URL), nil)

	// Unreserved orders are rejected before the missing client matters.
	created := createOrder(t, h, "LAPTOP-001", 1)
	wantError(t, pay(h, created.ID, "test_approve", "k"), http.StatusConflict)

	o := reservedOrder(t, h)
	rec := pay(h, o.ID, "test_approve", "k")
	wantError(t, rec, http.StatusServiceUnavailable)
	if !strings.Contains(rec.Body.String(), "not configured") {
		t.Errorf("body = %q, want not configured", rec.Body.String())
	}
}

func TestPayOrderDroppedResponseThenRecovery(t *testing.T) {
	ps := newFakePaymentService()
	h := setupCheckout(t, ps, time.Second)
	o := reservedOrder(t, h)

	// The Payment Service approves, but the answer is lost.
	ps.set(func(f *fakePaymentService) { f.dropNextResponse = true })
	wantError(t, pay(h, o.ID, "test_approve", "attempt-1"), http.StatusServiceUnavailable)
	if stored := getOrder(t, h, o.ID); stored.Status != order.StatusReserved {
		t.Fatalf("status = %q after ambiguous failure, want reserved", stored.Status)
	}
	if ps.approvalsFor(o.ID) != 1 {
		t.Fatalf("upstream approvals = %d, want 1", ps.approvalsFor(o.ID))
	}

	// A different key cannot create a second approval, and is told how to
	// recover.
	rec := pay(h, o.ID, "test_approve", "attempt-2")
	wantError(t, rec, http.StatusConflict)
	if !strings.Contains(rec.Body.String(), "idempotency_key that produced it") {
		t.Errorf("error does not explain recovery: %q", rec.Body.String())
	}
	if stored := getOrder(t, h, o.ID); stored.Status != order.StatusReserved {
		t.Fatalf("status = %q, want reserved (approval not yet confirmed locally)", stored.Status)
	}

	// Retrying with the original key recovers the approval.
	rec = pay(h, o.ID, "test_approve", "attempt-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("recovery: status %d (body %q)", rec.Code, rec.Body.String())
	}
	got := decode[order.PayResult](t, rec)
	if got.Order.Status != order.StatusPaid || got.Payment.ID != "pay_1" || got.Order.PaymentID != "pay_1" {
		t.Errorf("recovered = %+v", got)
	}
	if ps.approvalsFor(o.ID) != 1 {
		t.Errorf("approvals = %d, want 1", ps.approvalsFor(o.ID))
	}
}

func TestPayOrderBadUpstreamResponses(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   func(orderID string) string
		want   int
	}{
		{"malformed JSON", 201, func(string) string { return `{"id":` }, http.StatusBadGateway},
		{"wrong order", 201, func(string) string {
			return `{"id":"pay_X","order_id":"ord_other","amount_cents":259998,"currency":"USD","status":"approved"}`
		}, http.StatusBadGateway},
		{"wrong amount", 201, func(id string) string {
			return `{"id":"pay_X","order_id":"` + id + `","amount_cents":1,"currency":"USD","status":"approved"}`
		}, http.StatusBadGateway},
		{"wrong currency", 201, func(id string) string {
			return `{"id":"pay_X","order_id":"` + id + `","amount_cents":259998,"currency":"EUR","status":"approved"}`
		}, http.StatusBadGateway},
		{"declined for approve", 201, func(id string) string {
			return `{"id":"pay_X","order_id":"` + id + `","amount_cents":259998,"currency":"USD","status":"declined"}`
		}, http.StatusBadGateway},
		{"missing id", 201, func(id string) string {
			return `{"order_id":"` + id + `","amount_cents":259998,"currency":"USD","status":"approved"}`
		}, http.StatusBadGateway},
		{"server error", 500, func(string) string { return `{"error":"boom"}` }, http.StatusServiceUnavailable},
		{"rejected request", 422, func(string) string { return `{"error":"invalid"}` }, http.StatusBadGateway},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ps := newFakePaymentService()
			h := setupCheckout(t, ps, time.Second)
			o := reservedOrder(t, h)
			ps.set(func(f *fakePaymentService) { f.rawStatus, f.rawResponse = tt.status, tt.body(o.ID) })

			wantError(t, pay(h, o.ID, "test_approve", "attempt-1"), tt.want)
			if stored := getOrder(t, h, o.ID); stored.Status != order.StatusReserved || stored.PaymentID != "" {
				t.Errorf("order = %+v, want unchanged reserved", stored)
			}
		})
	}
}

func TestPayOrderRejections(t *testing.T) {
	ps := newFakePaymentService()
	h := setupCheckout(t, ps, time.Second)
	created := createOrder(t, h, "LAPTOP-001", 1)
	reserved := reservedOrder(t, h)
	path := "/orders/" + reserved.ID + "/pay"

	t.Run("unknown order", func(t *testing.T) {
		wantError(t, pay(h, "ord_missing", "test_approve", "k"), http.StatusNotFound)
	})
	t.Run("unreserved order", func(t *testing.T) {
		wantError(t, pay(h, created.ID, "test_approve", "k"), http.StatusConflict)
		if got := getOrder(t, h, created.ID); got.Status != order.StatusCreated {
			t.Errorf("status = %q, want created", got.Status)
		}
	})

	bodies := []struct {
		name string
		body string
		want int
	}{
		{"empty body", ``, http.StatusBadRequest},
		{"malformed JSON", `{"payment_method":`, http.StatusBadRequest},
		{"trailing data", `{"payment_method":"test_approve","idempotency_key":"k"} {}`, http.StatusBadRequest},
		{"client amount", `{"payment_method":"test_approve","idempotency_key":"k","amount_cents":1}`, http.StatusBadRequest},
		{"card number", `{"payment_method":"test_approve","idempotency_key":"k","card_number":"4111111111111111"}`, http.StatusBadRequest},
		{"missing method", `{"idempotency_key":"k"}`, http.StatusUnprocessableEntity},
		{"unknown method", `{"payment_method":"visa","idempotency_key":"k"}`, http.StatusUnprocessableEntity},
		{"missing key", `{"payment_method":"test_approve"}`, http.StatusUnprocessableEntity},
		{"long key", `{"payment_method":"test_approve","idempotency_key":"` + strings.Repeat("k", 65) + `"}`, http.StatusUnprocessableEntity},
		{"oversized body", `{"payment_method":"test_approve","idempotency_key":"` + strings.Repeat("k", 5000) + `"}`, http.StatusRequestEntityTooLarge},
	}
	for _, tt := range bodies {
		t.Run(tt.name, func(t *testing.T) {
			wantError(t, do(h, http.MethodPost, path, tt.body), tt.want)
		})
	}
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		t.Run(m, func(t *testing.T) {
			rec := do(h, m, path, "")
			wantError(t, rec, http.StatusMethodNotAllowed)
			if rec.Header().Get("Allow") != "POST" {
				t.Errorf("Allow = %q, want POST", rec.Header().Get("Allow"))
			}
		})
	}

	if ps.requestCount() != 0 {
		t.Errorf("payment service received %d requests for rejected calls", ps.requestCount())
	}
	if got := getOrder(t, h, reserved.ID); got.Status != order.StatusReserved {
		t.Errorf("reserved order changed: %+v", got)
	}
}

func TestPayOrderKeyLengthBoundary(t *testing.T) {
	ps := newFakePaymentService()
	h := setupCheckout(t, ps, time.Second)

	for _, key := range []string{strings.Repeat("k", 65), strings.Repeat("é", 32) + "k"} {
		o := reservedOrder(t, h)
		wantError(t, pay(h, o.ID, "test_approve", key), http.StatusUnprocessableEntity)
	}
	if ps.requestCount() != 0 {
		t.Fatalf("payment service received %d requests for over-long keys", ps.requestCount())
	}

	o := reservedOrder(t, h)
	key := strings.Repeat("k", 64)
	if rec := pay(h, o.ID, "test_approve", key); rec.Code != http.StatusOK {
		t.Fatalf("64-byte key: status %d (body %q)", rec.Code, rec.Body.String())
	}
	ps.mu.Lock()
	_, ok := ps.byKey[o.ID+":"+key]
	ps.mu.Unlock()
	if !ok {
		t.Errorf("payment service did not receive the order-scoped key %q", o.ID+":"+key)
	}
}

func TestPayOrderResponseNeverContainsCardFields(t *testing.T) {
	ps := newFakePaymentService()
	h := setupCheckout(t, ps, time.Second)
	o := reservedOrder(t, h)
	rec := pay(h, o.ID, "test_approve", "attempt-1")
	for _, field := range []string{"card", "cvv", "pan"} {
		if bytes.Contains(bytes.ToLower(rec.Body.Bytes()), []byte(`"`+field)) {
			t.Errorf("response contains a %q field: %s", field, rec.Body.String())
		}
	}
}

func runConcurrently(n int, fn func(i int)) {
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			fn(i)
		}()
	}
	close(start)
	wg.Wait()
}
