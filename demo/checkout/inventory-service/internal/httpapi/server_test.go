package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/RajK158/ruptiq/demo/checkout/inventory-service/internal/inventory"
)

// newTestHandler returns a handler over a store with 10 units of LAPTOP-001
// and 5 of PHONE-7. Logs are discarded.
func newTestHandler() http.Handler {
	return NewHandler(inventory.NewStore(map[string]int{"LAPTOP-001": 10, "PHONE-7": 5}), nil)
}

// do sends one request straight to h, without opening a network port.
func do(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// decode parses the JSON response body into T and checks the Content-Type.
func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want %q", got, "application/json")
	}
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("response is not valid JSON: %v (body: %q)", err, rec.Body.String())
	}
	return v
}

// wantError checks for the expected status and a non-empty JSON error message.
func wantError(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Errorf("status = %d, want %d (body: %q)", rec.Code, wantStatus, rec.Body.String())
	}
	if e := decode[errorResponse](t, rec); e.Error == "" {
		t.Errorf("error message is empty (body: %q)", rec.Body.String())
	}
}

func reserveBody(orderID, sku string, qty int) string {
	return fmt.Sprintf(`{"order_id":%q,"sku":%q,"quantity":%d}`, orderID, sku, qty)
}

func stockOf(t *testing.T, h http.Handler, sku string) inventory.Item {
	t.Helper()
	rec := do(h, http.MethodGet, "/inventory/"+sku, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /inventory/%s status = %d, body %q", sku, rec.Code, rec.Body.String())
	}
	return decode[inventory.Item](t, rec)
}

func TestHealthz(t *testing.T) {
	rec := do(newTestHandler(), http.MethodGet, "/healthz", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	got := decode[healthResponse](t, rec)
	if got.Status != "ok" || got.Service != "inventory-service" {
		t.Errorf("body = %+v, want status ok from inventory-service", got)
	}
}

func TestGetInventory(t *testing.T) {
	rec := do(newTestHandler(), http.MethodGet, "/inventory/LAPTOP-001", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %q)", rec.Code, rec.Body.String())
	}
	got := decode[inventory.Item](t, rec)
	if got != (inventory.Item{SKU: "LAPTOP-001", Available: 10, Reserved: 0}) {
		t.Errorf("item = %+v", got)
	}
	// Pin the wire format.
	want := `{"sku":"LAPTOP-001","available":10,"reserved":0}`
	if body := strings.TrimSpace(rec.Body.String()); body != want {
		t.Errorf("body = %s, want %s", body, want)
	}
}

func TestGetInventoryUnknownSKU(t *testing.T) {
	wantError(t, do(newTestHandler(), http.MethodGet, "/inventory/NOPE", ""), http.StatusNotFound)
}

func TestCreateReservation(t *testing.T) {
	h := newTestHandler()
	rec := do(h, http.MethodPost, "/reservations", reserveBody("ord_example123", "LAPTOP-001", 2))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body: %q)", rec.Code, rec.Body.String())
	}
	got := decode[inventory.Reservation](t, rec)
	if !strings.HasPrefix(got.ID, "res_") {
		t.Errorf("ID = %q, want res_ prefix", got.ID)
	}
	if got.OrderID != "ord_example123" || got.SKU != "LAPTOP-001" || got.Quantity != 2 || got.Status != "reserved" {
		t.Errorf("reservation = %+v", got)
	}

	// Remaining stock reflects the reservation.
	if item := stockOf(t, h, "LAPTOP-001"); item.Available != 8 || item.Reserved != 2 {
		t.Errorf("item = %+v, want available 8 reserved 2", item)
	}
	// Other products are untouched.
	if item := stockOf(t, h, "PHONE-7"); item.Available != 5 || item.Reserved != 0 {
		t.Errorf("PHONE-7 = %+v, want untouched", item)
	}
}

func TestCreateReservationInsufficientStock(t *testing.T) {
	h := newTestHandler()
	rec := do(h, http.MethodPost, "/reservations", reserveBody("ord_1", "LAPTOP-001", 11))
	wantError(t, rec, http.StatusConflict)

	if item := stockOf(t, h, "LAPTOP-001"); item.Available != 10 || item.Reserved != 0 {
		t.Errorf("inventory changed after rejected reservation: %+v", item)
	}
}

func TestCreateReservationInsufficientAfterPartialUse(t *testing.T) {
	h := newTestHandler()
	if rec := do(h, http.MethodPost, "/reservations", reserveBody("ord_1", "PHONE-7", 4)); rec.Code != http.StatusCreated {
		t.Fatalf("first reservation status = %d", rec.Code)
	}
	wantError(t, do(h, http.MethodPost, "/reservations", reserveBody("ord_2", "PHONE-7", 2)), http.StatusConflict)
	if item := stockOf(t, h, "PHONE-7"); item.Available != 1 || item.Reserved != 4 {
		t.Errorf("PHONE-7 = %+v, want available 1 reserved 4", item)
	}
}

func TestCreateReservationUnknownSKU(t *testing.T) {
	wantError(t, do(newTestHandler(), http.MethodPost, "/reservations", reserveBody("ord_1", "NOPE", 1)), http.StatusNotFound)
}

func TestCreateReservationIdenticalRetry(t *testing.T) {
	h := newTestHandler()
	body := reserveBody("ord_retry", "LAPTOP-001", 3)

	first := do(h, http.MethodPost, "/reservations", body)
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d, want 201", first.Code)
	}
	firstRes := decode[inventory.Reservation](t, first)

	for i := 0; i < 3; i++ {
		retry := do(h, http.MethodPost, "/reservations", body)
		if retry.Code != http.StatusOK {
			t.Fatalf("retry %d status = %d, want 200 (body: %q)", i, retry.Code, retry.Body.String())
		}
		got := decode[inventory.Reservation](t, retry)
		if got.ID != firstRes.ID || got.Status != "reserved" {
			t.Errorf("retry %d returned %+v, want same reservation as %+v", i, got, firstRes)
		}
	}
	if item := stockOf(t, h, "LAPTOP-001"); item.Available != 7 || item.Reserved != 3 {
		t.Errorf("item = %+v, want available 7 reserved 3 (decremented once)", item)
	}
}

func TestCreateReservationConflictingRetry(t *testing.T) {
	h := newTestHandler()
	if rec := do(h, http.MethodPost, "/reservations", reserveBody("ord_1", "LAPTOP-001", 2)); rec.Code != http.StatusCreated {
		t.Fatalf("first status = %d", rec.Code)
	}

	wantError(t, do(h, http.MethodPost, "/reservations", reserveBody("ord_1", "LAPTOP-001", 3)), http.StatusConflict)
	wantError(t, do(h, http.MethodPost, "/reservations", reserveBody("ord_1", "PHONE-7", 2)), http.StatusConflict)

	if item := stockOf(t, h, "LAPTOP-001"); item.Available != 8 || item.Reserved != 2 {
		t.Errorf("LAPTOP-001 = %+v, want available 8 reserved 2", item)
	}
	if item := stockOf(t, h, "PHONE-7"); item.Available != 5 {
		t.Errorf("PHONE-7 = %+v, want untouched", item)
	}
}

func TestCreateReservationInvalid(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		// 400: the body is not the JSON we can read.
		{"empty body", ``, http.StatusBadRequest},
		{"malformed JSON", `{"order_id":`, http.StatusBadRequest},
		{"not an object", `[1,2]`, http.StatusBadRequest},
		{"unknown field", `{"order_id":"o","sku":"LAPTOP-001","quantity":1,"price":5}`, http.StatusBadRequest},
		{"quantity is a string", `{"order_id":"o","sku":"LAPTOP-001","quantity":"2"}`, http.StatusBadRequest},
		{"quantity is a fraction", `{"order_id":"o","sku":"LAPTOP-001","quantity":1.5}`, http.StatusBadRequest},
		{"order_id is a number", `{"order_id":5,"sku":"LAPTOP-001","quantity":1}`, http.StatusBadRequest},
		{"trailing object", `{"order_id":"o","sku":"LAPTOP-001","quantity":1}{"x":1}`, http.StatusBadRequest},
		{"trailing garbage", `{"order_id":"o","sku":"LAPTOP-001","quantity":1} xyz`, http.StatusBadRequest},

		// 413: body too large.
		{"too large", `{"order_id":"o","sku":"` + strings.Repeat("a", maxBodyBytes) + `","quantity":1}`, http.StatusRequestEntityTooLarge},

		// 422: well-formed JSON, unacceptable values.
		{"missing order_id", `{"sku":"LAPTOP-001","quantity":1}`, http.StatusUnprocessableEntity},
		{"blank order_id", `{"order_id":"  ","sku":"LAPTOP-001","quantity":1}`, http.StatusUnprocessableEntity},
		{"missing sku", `{"order_id":"o","quantity":1}`, http.StatusUnprocessableEntity},
		{"blank sku", `{"order_id":"o","sku":"","quantity":1}`, http.StatusUnprocessableEntity},
		{"missing quantity", `{"order_id":"o","sku":"LAPTOP-001"}`, http.StatusUnprocessableEntity},
		{"zero quantity", `{"order_id":"o","sku":"LAPTOP-001","quantity":0}`, http.StatusUnprocessableEntity},
		{"negative quantity", `{"order_id":"o","sku":"LAPTOP-001","quantity":-3}`, http.StatusUnprocessableEntity},
		{"huge quantity", `{"order_id":"o","sku":"LAPTOP-001","quantity":100000}`, http.StatusUnprocessableEntity},
		{"order_id too long", `{"order_id":"` + strings.Repeat("o", 65) + `","sku":"LAPTOP-001","quantity":1}`, http.StatusUnprocessableEntity},
		{"sku too long", `{"order_id":"o","sku":"` + strings.Repeat("s", 65) + `","quantity":1}`, http.StatusUnprocessableEntity},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTestHandler()
			wantError(t, do(h, http.MethodPost, "/reservations", tt.body), tt.wantStatus)
			if item := stockOf(t, h, "LAPTOP-001"); item.Available != 10 || item.Reserved != 0 {
				t.Errorf("inventory changed after rejected request: %+v", item)
			}
		})
	}
}

func TestMethodNotAllowed(t *testing.T) {
	tests := []struct {
		method, path, allow string
	}{
		{http.MethodPost, "/healthz", "GET, HEAD"},
		{http.MethodPost, "/inventory/LAPTOP-001", "GET, HEAD"},
		{http.MethodPut, "/inventory/LAPTOP-001", "GET, HEAD"},
		{http.MethodGet, "/reservations", "POST"},
		{http.MethodDelete, "/reservations", "POST"},
		{http.MethodPost, "/metrics", "GET, HEAD"},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			rec := do(newTestHandler(), tt.method, tt.path, "")
			wantError(t, rec, http.StatusMethodNotAllowed)
			if got := rec.Header().Get("Allow"); got != tt.allow {
				t.Errorf("Allow = %q, want %q", got, tt.allow)
			}
		})
	}
}

func TestNotFound(t *testing.T) {
	for _, path := range []string{"/", "/nope", "/inventory", "/inventory/", "/inventory/a/b", "/reservations/res_1", "/metrics/x"} {
		t.Run(path, func(t *testing.T) {
			wantError(t, do(newTestHandler(), http.MethodGet, path, ""), http.StatusNotFound)
		})
	}
}

func TestRequestsAreLogged(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	h := NewHandler(inventory.NewStore(map[string]int{"LAPTOP-001": 10}), logger)

	do(h, http.MethodPost, "/reservations", reserveBody("ord_1", "LAPTOP-001", 1))

	out := buf.String()
	for _, want := range []string{`"msg":"reservation created"`, `"order_id":"ord_1"`, `"msg":"request"`, `"status":201`} {
		if !strings.Contains(out, want) {
			t.Errorf("log output missing %s:\n%s", want, out)
		}
	}
}

// TestConcurrentReservationsOverHTTP sends 150 simultaneous reservation
// requests through a real HTTP server for 10 units of stock.
func TestConcurrentReservationsOverHTTP(t *testing.T) {
	const (
		units    = 10
		attempts = 150
	)
	h := NewHandler(inventory.NewStore(map[string]int{"LAPTOP-001": units}), nil)
	srv := httptest.NewServer(h)
	defer srv.Close()

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		statuses = make(map[int]int)
	)
	start := make(chan struct{})
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			resp, err := http.Post(srv.URL+"/reservations", "application/json",
				strings.NewReader(reserveBody(fmt.Sprintf("ord_%d", i), "LAPTOP-001", 1)))
			code := 0
			if err != nil {
				t.Errorf("request %d: %v", i, err)
			} else {
				code = resp.StatusCode
				resp.Body.Close()
			}
			mu.Lock()
			statuses[code]++
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()

	if statuses[http.StatusCreated] != units {
		t.Errorf("201 responses = %d, want exactly %d (all statuses: %v)", statuses[http.StatusCreated], units, statuses)
	}
	if statuses[http.StatusConflict] != attempts-units {
		t.Errorf("409 responses = %d, want %d (all statuses: %v)", statuses[http.StatusConflict], attempts-units, statuses)
	}
	if item := stockOf(t, h, "LAPTOP-001"); item.Available != 0 || item.Reserved != units {
		t.Errorf("final item = %+v, want available 0 reserved %d", item, units)
	}
}
