package httpapi

import (
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
)

// fakeInventoryService simulates the Inventory Service's POST /reservations
// over real HTTP: stock per SKU, idempotency by order_id, and the same status
// codes and error messages. It is deliberately independent of the real
// service's code.
type fakeInventoryService struct {
	mu           sync.Mutex
	available    map[string]int
	reservations map[string]fakeReservation // by order_id
	requests     int

	// dropNextResponse makes the next request reserve stock normally, then
	// drop the connection before any response is sent: an ambiguous failure.
	dropNextResponse bool
	// delay holds every response this long (to provoke client timeouts).
	delay time.Duration
	// rawResponse, if set, is sent instead of real handling.
	rawStatus   int
	rawResponse string
}

type fakeReservation struct {
	ID       string `json:"id"`
	OrderID  string `json:"order_id"`
	SKU      string `json:"sku"`
	Quantity int    `json:"quantity"`
	Status   string `json:"status"`
}

func newFakeInventoryService(stock map[string]int) *fakeInventoryService {
	return &fakeInventoryService{available: stock, reservations: make(map[string]fakeReservation)}
}

func (f *fakeInventoryService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/reservations" {
		http.NotFound(w, r)
		return
	}
	var req struct {
		OrderID  string `json:"order_id"`
		SKU      string `json:"sku"`
		Quantity int    `json:"quantity"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeFake(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
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
	status, body := f.reserveLocked(req.OrderID, req.SKU, req.Quantity)
	drop := f.dropNextResponse
	f.dropNextResponse = false
	f.mu.Unlock()

	if drop {
		// The reservation is committed; now lose the answer.
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
		return
	}
	writeFake(w, status, body)
}

func (f *fakeInventoryService) reserveLocked(orderID, sku string, qty int) (int, any) {
	if existing, ok := f.reservations[orderID]; ok {
		if existing.SKU == sku && existing.Quantity == qty {
			return http.StatusOK, existing
		}
		return http.StatusConflict, map[string]string{"error": "order_id already has a reservation with different details"}
	}
	avail, ok := f.available[sku]
	if !ok {
		return http.StatusNotFound, map[string]string{"error": "unknown sku"}
	}
	if avail < qty {
		return http.StatusConflict, map[string]string{"error": "insufficient stock"}
	}
	f.available[sku] = avail - qty
	res := fakeReservation{
		ID:       fmt.Sprintf("res_%d", len(f.reservations)+1),
		OrderID:  orderID,
		SKU:      sku,
		Quantity: qty,
		Status:   "reserved",
	}
	f.reservations[orderID] = res
	return http.StatusCreated, res
}

func writeFake(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (f *fakeInventoryService) stock(sku string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.available[sku]
}

func (f *fakeInventoryService) reservationCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reservations)
}

// setup wires an Order Service handler to a fake Inventory Service over real
// HTTP, using the production inventory client.
func setup(t *testing.T, inv *fakeInventoryService, timeout time.Duration) http.Handler {
	t.Helper()
	srv := httptest.NewServer(inv)
	t.Cleanup(srv.Close)
	client, err := inventoryclient.New(srv.URL, timeout)
	if err != nil {
		t.Fatal(err)
	}
	return NewHandler(order.NewStore(), client, nil)
}

func createOrder(t *testing.T, h http.Handler, sku string, qty int) order.Order {
	t.Helper()
	rec := do(h, http.MethodPost, "/orders", fmt.Sprintf(`{"sku":%q,"quantity":%d}`, sku, qty))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create order: status %d (body %q)", rec.Code, rec.Body.String())
	}
	return decode[order.Order](t, rec)
}

func getOrder(t *testing.T, h http.Handler, id string) order.Order {
	t.Helper()
	rec := do(h, http.MethodGet, "/orders/"+id, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get order: status %d", rec.Code)
	}
	return decode[order.Order](t, rec)
}

func reserve(h http.Handler, id string) *httptest.ResponseRecorder {
	return do(h, http.MethodPost, "/orders/"+id+"/reserve", "")
}

func TestReserveOrder(t *testing.T) {
	inv := newFakeInventoryService(map[string]int{"LAPTOP-001": 10})
	h := setup(t, inv, time.Second)
	o := createOrder(t, h, "LAPTOP-001", 2)

	rec := reserve(h, o.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	got := decode[order.Order](t, rec)
	if got.Status != order.StatusReserved || got.ReservationID != "res_1" {
		t.Errorf("order = %+v, want reserved with res_1", got)
	}
	if got.ID != o.ID || got.SKU != o.SKU || got.Quantity != o.Quantity || !got.CreatedAt.Equal(o.CreatedAt) {
		t.Errorf("order details changed: got %+v, created %+v", got, o)
	}
	if inv.stock("LAPTOP-001") != 8 {
		t.Errorf("inventory stock = %d, want 8", inv.stock("LAPTOP-001"))
	}

	// GET /orders/{id} returns the stored reservation.
	stored := getOrder(t, h, o.ID)
	if stored.Status != order.StatusReserved || stored.ReservationID != "res_1" {
		t.Errorf("GET after reserve = %+v", stored)
	}
	if !strings.Contains(rec.Body.String(), `"reservation_id":"res_1"`) {
		t.Errorf("response JSON lacks reservation_id: %s", rec.Body.String())
	}
}

func TestCreatedOrderHasNoReservationID(t *testing.T) {
	h := setup(t, newFakeInventoryService(nil), time.Second)
	rec := do(h, http.MethodPost, "/orders", `{"sku":"LAPTOP-001","quantity":1}`)
	if strings.Contains(rec.Body.String(), "reservation_id") {
		t.Errorf("unreserved order JSON contains reservation_id: %s", rec.Body.String())
	}
}

func TestReserveOrderIdenticalRetry(t *testing.T) {
	inv := newFakeInventoryService(map[string]int{"LAPTOP-001": 10})
	h := setup(t, inv, time.Second)
	o := createOrder(t, h, "LAPTOP-001", 2)

	first := decode[order.Order](t, reserve(h, o.ID))
	for i := 0; i < 3; i++ {
		rec := reserve(h, o.ID)
		if rec.Code != http.StatusOK {
			t.Fatalf("retry %d: status %d", i, rec.Code)
		}
		if got := decode[order.Order](t, rec); got.ReservationID != first.ReservationID || got.Status != order.StatusReserved {
			t.Errorf("retry %d = %+v, want %+v", i, got, first)
		}
	}
	if inv.stock("LAPTOP-001") != 8 || inv.reservationCount() != 1 {
		t.Errorf("stock = %d, reservations = %d; want 8 and 1", inv.stock("LAPTOP-001"), inv.reservationCount())
	}
}

func TestReserveOrderConcurrentRetries(t *testing.T) {
	inv := newFakeInventoryService(map[string]int{"LAPTOP-001": 10})
	h := setup(t, inv, 5*time.Second)
	o := createOrder(t, h, "LAPTOP-001", 2)

	const workers = 100
	var wg sync.WaitGroup
	start := make(chan struct{})
	codes := make([]int, workers)
	ids := make([]string, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rec := reserve(h, o.ID)
			codes[i] = rec.Code
			var got order.Order
			json.Unmarshal(rec.Body.Bytes(), &got)
			ids[i] = got.ReservationID
		}()
	}
	close(start)
	wg.Wait()

	for i := range codes {
		if codes[i] != http.StatusOK || ids[i] != "res_1" {
			t.Fatalf("request %d: status %d, reservation %q; want 200 and res_1", i, codes[i], ids[i])
		}
	}
	if inv.reservationCount() != 1 || inv.stock("LAPTOP-001") != 8 {
		t.Errorf("reservations = %d, stock = %d; want 1 and 8", inv.reservationCount(), inv.stock("LAPTOP-001"))
	}
	if got := getOrder(t, h, o.ID); got.Status != order.StatusReserved || got.ReservationID != "res_1" {
		t.Errorf("final order = %+v", got)
	}
}

func TestReserveOrderErrors(t *testing.T) {
	tests := []struct {
		name       string
		sku        string
		qty        int
		configure  func(*fakeInventoryService)
		wantStatus int
	}{
		{name: "insufficient stock", sku: "LAPTOP-001", qty: 5, configure: func(f *fakeInventoryService) { f.available["LAPTOP-001"] = 3 }, wantStatus: http.StatusConflict},
		{name: "unknown sku", sku: "NOPE-1", qty: 1, wantStatus: http.StatusUnprocessableEntity},
		{name: "inventory 500", sku: "LAPTOP-001", qty: 1, configure: func(f *fakeInventoryService) { f.rawStatus = 500; f.rawResponse = `{"error":"boom"}` }, wantStatus: http.StatusServiceUnavailable},
		{name: "malformed response", sku: "LAPTOP-001", qty: 1, configure: func(f *fakeInventoryService) { f.rawStatus = 201; f.rawResponse = `{"id":` }, wantStatus: http.StatusBadGateway},
		{name: "mismatched response", sku: "LAPTOP-001", qty: 1, configure: func(f *fakeInventoryService) {
			f.rawStatus = 201
			f.rawResponse = `{"id":"res_X","order_id":"ord_other","sku":"LAPTOP-001","quantity":1,"status":"reserved"}`
		}, wantStatus: http.StatusBadGateway},
		{name: "conflicting reservation", sku: "LAPTOP-001", qty: 1, configure: func(f *fakeInventoryService) {
			f.rawStatus = 409
			f.rawResponse = `{"error":"order_id already has a reservation with different details"}`
		}, wantStatus: http.StatusConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inv := newFakeInventoryService(map[string]int{"LAPTOP-001": 10})
			if tt.configure != nil {
				tt.configure(inv)
			}
			h := setup(t, inv, time.Second)
			o := createOrder(t, h, tt.sku, tt.qty)

			wantError(t, reserve(h, o.ID), tt.wantStatus)
			if got := getOrder(t, h, o.ID); got.Status != order.StatusCreated || got.ReservationID != "" {
				t.Errorf("order changed after failure: %+v", got)
			}
		})
	}
}

func TestReserveOrderInsufficientStockKeepsInventory(t *testing.T) {
	inv := newFakeInventoryService(map[string]int{"LAPTOP-001": 1})
	h := setup(t, inv, time.Second)
	o := createOrder(t, h, "LAPTOP-001", 2)

	wantError(t, reserve(h, o.ID), http.StatusConflict)
	if inv.stock("LAPTOP-001") != 1 || inv.reservationCount() != 0 {
		t.Errorf("stock = %d, reservations = %d; want unchanged", inv.stock("LAPTOP-001"), inv.reservationCount())
	}
}

func TestReserveOrderNotFound(t *testing.T) {
	inv := newFakeInventoryService(map[string]int{"LAPTOP-001": 10})
	h := setup(t, inv, time.Second)
	wantError(t, reserve(h, "ord_missing"), http.StatusNotFound)
	inv.mu.Lock()
	requests := inv.requests
	inv.mu.Unlock()
	if requests != 0 {
		t.Error("inventory was called for a missing order")
	}
}

func TestReserveOrderTimeout(t *testing.T) {
	inv := newFakeInventoryService(map[string]int{"LAPTOP-001": 10})
	inv.delay = 2 * time.Second
	h := setup(t, inv, 100*time.Millisecond)
	o := createOrder(t, h, "LAPTOP-001", 2)

	start := time.Now()
	wantError(t, reserve(h, o.ID), http.StatusGatewayTimeout)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("reserve took %v, want it bounded by the client timeout", elapsed)
	}
	if got := getOrder(t, h, o.ID); got.Status != order.StatusCreated {
		t.Errorf("order status = %q after timeout, want created", got.Status)
	}
}

func TestReserveOrderConnectionFailure(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	client, err := inventoryclient.New(url, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(order.NewStore(), client, nil)
	o := createOrder(t, h, "LAPTOP-001", 2)

	wantError(t, reserve(h, o.ID), http.StatusServiceUnavailable)
	if got := getOrder(t, h, o.ID); got.Status != order.StatusCreated {
		t.Errorf("order status = %q after connection failure, want created", got.Status)
	}
}

func TestReserveOrderNotConfigured(t *testing.T) {
	h := NewHandler(order.NewStore(), nil, nil)
	o := createOrder(t, h, "LAPTOP-001", 2)
	wantError(t, reserve(h, o.ID), http.StatusServiceUnavailable)
}

// TestReserveOrderRecoversFromAmbiguousFailure: the Inventory Service reserves
// stock but the connection drops before the answer arrives. The order must
// stay "created" (no confirmation was seen); a retry must then succeed using
// the existing reservation, without reserving stock a second time.
func TestReserveOrderRecoversFromAmbiguousFailure(t *testing.T) {
	inv := newFakeInventoryService(map[string]int{"LAPTOP-001": 10})
	inv.dropNextResponse = true
	h := setup(t, inv, time.Second)
	o := createOrder(t, h, "LAPTOP-001", 2)

	wantError(t, reserve(h, o.ID), http.StatusServiceUnavailable)
	if got := getOrder(t, h, o.ID); got.Status != order.StatusCreated || got.ReservationID != "" {
		t.Fatalf("order after ambiguous failure = %+v, want created", got)
	}
	// Inventory did reserve, even though nobody saw the answer.
	if inv.stock("LAPTOP-001") != 8 || inv.reservationCount() != 1 {
		t.Fatalf("stock = %d, reservations = %d; want 8 and 1", inv.stock("LAPTOP-001"), inv.reservationCount())
	}

	rec := reserve(h, o.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry status = %d (body %q)", rec.Code, rec.Body.String())
	}
	if got := decode[order.Order](t, rec); got.Status != order.StatusReserved || got.ReservationID != "res_1" {
		t.Errorf("retry = %+v, want reserved with res_1", got)
	}
	if inv.stock("LAPTOP-001") != 8 || inv.reservationCount() != 1 {
		t.Errorf("after retry: stock = %d, reservations = %d; want still 8 and 1", inv.stock("LAPTOP-001"), inv.reservationCount())
	}
}

func TestReserveOrderMethodNotAllowed(t *testing.T) {
	h := NewHandler(order.NewStore(), nil, nil)
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		rec := do(h, method, "/orders/ord_x/reserve", "")
		wantError(t, rec, http.StatusMethodNotAllowed)
		if got := rec.Header().Get("Allow"); got != "POST" {
			t.Errorf("%s: Allow = %q, want POST", method, got)
		}
	}
}
