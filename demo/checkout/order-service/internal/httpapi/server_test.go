package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RajK158/ruptiq/demo/checkout/order-service/internal/order"
)

// newTestHandler returns a handler backed by a fresh, empty store.
func newTestHandler() http.Handler {
	return NewHandler(order.NewStore(), nil, nil)
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

func TestHealthz(t *testing.T) {
	rec := do(newTestHandler(), http.MethodGet, "/healthz", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	got := decode[healthResponse](t, rec)
	if got.Status != "ok" || got.Service != "order-service" {
		t.Errorf("body = %+v, want status ok from order-service", got)
	}
}

func TestCreateOrder(t *testing.T) {
	before := time.Now()
	rec := do(newTestHandler(), http.MethodPost, "/orders", `{"sku":"LAPTOP-001","quantity":2}`)
	after := time.Now()

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d (body: %q)", rec.Code, http.StatusCreated, rec.Body.String())
	}
	got := decode[order.Order](t, rec)

	if got.ID == "" {
		t.Error("ID is empty")
	}
	if got.SKU != "LAPTOP-001" || got.Quantity != 2 {
		t.Errorf("order = %+v, want sku LAPTOP-001 and quantity 2", got)
	}
	if got.Status != order.StatusCreated {
		t.Errorf("Status = %q, want %q", got.Status, order.StatusCreated)
	}
	if got.CreatedAt.Before(before) || got.CreatedAt.After(after) {
		t.Errorf("CreatedAt = %v, want between %v and %v", got.CreatedAt, before, after)
	}
	if loc := rec.Header().Get("Location"); loc != "/orders/"+got.ID {
		t.Errorf("Location = %q, want %q", loc, "/orders/"+got.ID)
	}
}

func TestGetOrder(t *testing.T) {
	h := newTestHandler()

	created := decode[order.Order](t, do(h, http.MethodPost, "/orders", `{"sku":"PHONE-7","quantity":1}`))

	rec := do(h, http.MethodGet, "/orders/"+created.ID, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %q)", rec.Code, http.StatusOK, rec.Body.String())
	}
	got := decode[order.Order](t, rec)
	if !got.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, created.CreatedAt)
	}
	got.CreatedAt = created.CreatedAt // compare the rest field by field
	if got != created {
		t.Errorf("retrieved order = %+v, want %+v", got, created)
	}
}

func TestGetOrderNotFound(t *testing.T) {
	rec := do(newTestHandler(), http.MethodGet, "/orders/ord_does-not-exist", "")
	wantError(t, rec, http.StatusNotFound)
}

func TestCreateOrderInvalid(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{name: "empty body", body: ``, wantStatus: http.StatusBadRequest},
		{name: "malformed JSON", body: `{"sku":`, wantStatus: http.StatusBadRequest},
		{name: "not an object", body: `[1,2]`, wantStatus: http.StatusBadRequest},
		{name: "unknown field", body: `{"sku":"A","quantity":1,"price":5}`, wantStatus: http.StatusBadRequest},
		{name: "quantity is a string", body: `{"sku":"A","quantity":"2"}`, wantStatus: http.StatusBadRequest},
		{name: "quantity is fractional", body: `{"sku":"A","quantity":1.5}`, wantStatus: http.StatusBadRequest},
		{name: "sku is a number", body: `{"sku":123,"quantity":1}`, wantStatus: http.StatusBadRequest},
		{name: "two JSON objects", body: `{"sku":"A","quantity":1}{"sku":"B","quantity":1}`, wantStatus: http.StatusBadRequest},
		{name: "trailing text", body: `{"sku":"A","quantity":1} nope`, wantStatus: http.StatusBadRequest},
		{name: "missing sku", body: `{"quantity":1}`, wantStatus: http.StatusUnprocessableEntity},
		{name: "empty sku", body: `{"sku":"","quantity":1}`, wantStatus: http.StatusUnprocessableEntity},
		{name: "whitespace sku", body: `{"sku":"   ","quantity":1}`, wantStatus: http.StatusUnprocessableEntity},
		{name: "sku too long", body: `{"sku":"` + strings.Repeat("A", order.MaxSKULen+1) + `","quantity":1}`, wantStatus: http.StatusUnprocessableEntity},
		{name: "missing quantity", body: `{"sku":"A"}`, wantStatus: http.StatusUnprocessableEntity},
		{name: "quantity zero", body: `{"sku":"A","quantity":0}`, wantStatus: http.StatusUnprocessableEntity},
		{name: "quantity negative", body: `{"sku":"A","quantity":-3}`, wantStatus: http.StatusUnprocessableEntity},
		{name: "quantity too high", body: `{"sku":"A","quantity":11}`, wantStatus: http.StatusUnprocessableEntity},
		{name: "body too large", body: `{"sku":"` + strings.Repeat("A", maxBodyBytes) + `","quantity":1}`, wantStatus: http.StatusRequestEntityTooLarge},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTestHandler()
			wantError(t, do(h, http.MethodPost, "/orders", tt.body), tt.wantStatus)
		})
	}
}

func TestUnsupportedMethods(t *testing.T) {
	tests := []struct {
		method    string
		path      string
		wantAllow string
	}{
		{http.MethodPost, "/healthz", "GET, HEAD"},
		{http.MethodDelete, "/healthz", "GET, HEAD"},
		{http.MethodGet, "/orders", "POST"},
		{http.MethodPut, "/orders", "POST"},
		{http.MethodDelete, "/orders", "POST"},
		{http.MethodPost, "/orders/ord_anything", "GET, HEAD"},
		{http.MethodPut, "/orders/ord_anything", "GET, HEAD"},
		{http.MethodDelete, "/orders/ord_anything", "GET, HEAD"},
		{http.MethodPatch, "/orders/ord_anything", "GET, HEAD"},
	}

	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			rec := do(newTestHandler(), tt.method, tt.path, "")
			wantError(t, rec, http.StatusMethodNotAllowed)
			if got := rec.Header().Get("Allow"); got != tt.wantAllow {
				t.Errorf("Allow = %q, want %q", got, tt.wantAllow)
			}
		})
	}
}

func TestUnknownPath(t *testing.T) {
	wantError(t, do(newTestHandler(), http.MethodGet, "/nope", ""), http.StatusNotFound)
}

// TestConcurrentCreateOrder sends many requests at once through a real HTTP
// server. Every request must succeed with its own unique ID, and every created
// order must be readable afterwards. It is most valuable under the race
// detector (go test -race).
func TestConcurrentCreateOrder(t *testing.T) {
	const workers = 50

	srv := httptest.NewServer(newTestHandler())
	defer srv.Close()

	var wg sync.WaitGroup
	ids := make([]string, workers)
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Post(srv.URL+"/orders", "application/json", strings.NewReader(`{"sku":"LAPTOP-001","quantity":1}`))
			if err != nil {
				t.Errorf("request %d: %v", i, err)
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusCreated {
				t.Errorf("request %d: status = %d, want %d", i, resp.StatusCode, http.StatusCreated)
				return
			}
			var o order.Order
			if err := json.NewDecoder(resp.Body).Decode(&o); err != nil {
				t.Errorf("request %d: decoding response: %v", i, err)
				return
			}
			ids[i] = o.ID
		}()
	}
	wg.Wait()

	seen := make(map[string]bool, workers)
	for _, id := range ids {
		if id == "" {
			t.Fatal("a request produced no order ID (see errors above)")
		}
		if seen[id] {
			t.Fatalf("duplicate order ID %q", id)
		}
		seen[id] = true

		resp, err := http.Get(srv.URL + "/orders/" + id)
		if err != nil {
			t.Fatalf("GET /orders/%s: %v", id, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET /orders/%s: status = %d, want %d", id, resp.StatusCode, http.StatusOK)
		}
	}
}
