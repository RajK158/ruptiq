package inventoryclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RajK158/ruptiq/demo/checkout/order-service/internal/order"
)

var testReq = order.ReserveRequest{OrderID: "ord_1", SKU: "LAPTOP-001", Quantity: 2}

const okBody = `{"id":"res_ABC","order_id":"ord_1","sku":"LAPTOP-001","quantity":2,"status":"reserved","created_at":"2026-11-05T00:00:00Z"}`

// newTestClient starts a fake Inventory Service running handler and returns
// a client pointed at it.
func newTestClient(t *testing.T, timeout time.Duration, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, timeout)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// respond returns a handler that always answers with status and body.
func respond(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}
}

func TestReserveSendsExpectedRequest(t *testing.T) {
	var gotMethod, gotPath, gotType string
	var gotBody map[string]any
	c := newTestClient(t, time.Second, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotType = r.Method, r.URL.Path, r.Header.Get("Content-Type")
		dec := json.NewDecoder(r.Body)
		if err := dec.Decode(&gotBody); err != nil {
			t.Errorf("decoding request: %v", err)
		}
		respond(http.StatusCreated, okBody)(w, r)
	})

	id, err := c.Reserve(context.Background(), testReq)
	if err != nil || id != "res_ABC" {
		t.Fatalf("Reserve = %q, %v; want res_ABC, nil", id, err)
	}
	if gotMethod != http.MethodPost || gotPath != "/reservations" || gotType != "application/json" {
		t.Errorf("request = %s %s (Content-Type %q), want POST /reservations as JSON", gotMethod, gotPath, gotType)
	}
	// Exactly these three fields: the Inventory Service rejects unknown ones.
	want := map[string]any{"order_id": "ord_1", "sku": "LAPTOP-001", "quantity": float64(2)}
	if len(gotBody) != len(want) {
		t.Errorf("request body = %v, want %v", gotBody, want)
	}
	for k, v := range want {
		if gotBody[k] != v {
			t.Errorf("request body[%q] = %v, want %v", k, gotBody[k], v)
		}
	}
}

func TestReserveIdempotentReplayIsSuccess(t *testing.T) {
	c := newTestClient(t, time.Second, respond(http.StatusOK, okBody))
	if id, err := c.Reserve(context.Background(), testReq); err != nil || id != "res_ABC" {
		t.Errorf("Reserve = %q, %v; want res_ABC, nil", id, err)
	}
}

func TestReserveErrorMapping(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr error
	}{
		{"insufficient stock", http.StatusConflict, `{"error":"insufficient stock"}`, order.ErrInsufficientStock},
		{"order id conflict", http.StatusConflict, `{"error":"order_id already has a reservation with different details"}`, order.ErrInventoryConflict},
		{"unknown sku", http.StatusNotFound, `{"error":"unknown sku"}`, order.ErrUnknownSKU},
		{"bare 404 (wrong URL)", http.StatusNotFound, `{"error":"not found"}`, order.ErrInventoryBadResponse},
		{"404 html", http.StatusNotFound, `<html>nope</html>`, order.ErrInventoryBadResponse},
		{"500", http.StatusInternalServerError, `{"error":"internal server error"}`, order.ErrInventoryUnavailable},
		{"503", http.StatusServiceUnavailable, ``, order.ErrInventoryUnavailable},
		{"422 (request rejected)", http.StatusUnprocessableEntity, `{"error":"quantity must be between 1 and 1000"}`, order.ErrInventoryBadResponse},
		{"unexpected 202", http.StatusAccepted, okBody, order.ErrInventoryBadResponse},
		{"redirect is not followed", http.StatusTemporaryRedirect, ``, order.ErrInventoryBadResponse},
		{"malformed JSON", http.StatusCreated, `{"id":"res_ABC",`, order.ErrInventoryBadResponse},
		{"not JSON", http.StatusCreated, `reserved!`, order.ErrInventoryBadResponse},
		{"empty body", http.StatusCreated, ``, order.ErrInventoryBadResponse},
		{"missing id", http.StatusCreated, `{"order_id":"ord_1","sku":"LAPTOP-001","quantity":2,"status":"reserved"}`, order.ErrInventoryBadResponse},
		{"wrong status", http.StatusCreated, `{"id":"res_ABC","order_id":"ord_1","sku":"LAPTOP-001","quantity":2,"status":"pending"}`, order.ErrInventoryBadResponse},
		{"different order", http.StatusCreated, `{"id":"res_ABC","order_id":"ord_2","sku":"LAPTOP-001","quantity":2,"status":"reserved"}`, order.ErrInventoryBadResponse},
		{"different quantity", http.StatusCreated, `{"id":"res_ABC","order_id":"ord_1","sku":"LAPTOP-001","quantity":3,"status":"reserved"}`, order.ErrInventoryBadResponse},
		{"oversized body", http.StatusCreated, `{"id":"` + strings.Repeat("x", maxResponseBytes) + `"}`, order.ErrInventoryBadResponse},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := respond(tt.status, tt.body)
			if tt.status == http.StatusTemporaryRedirect {
				handler = func(w http.ResponseWriter, r *http.Request) {
					http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
				}
			}
			c := newTestClient(t, time.Second, handler)
			id, err := c.Reserve(context.Background(), testReq)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("err = %v, want %v", err, tt.wantErr)
			}
			if id != "" {
				t.Errorf("id = %q on failure, want empty", id)
			}
		})
	}
}

func TestReserveTimeout(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	c := newTestClient(t, 50*time.Millisecond, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})

	start := time.Now()
	_, err := c.Reserve(context.Background(), testReq)
	if !errors.Is(err, order.ErrInventoryTimeout) {
		t.Fatalf("err = %v, want ErrInventoryTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Reserve took %v; the timeout did not bound it", elapsed)
	}
}

func TestReserveRespectsCallerDeadline(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	// Client timeout is long; the caller's context is short and must win.
	c := newTestClient(t, 10*time.Second, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if _, err := c.Reserve(ctx, testReq); !errors.Is(err, order.ErrInventoryTimeout) {
		t.Errorf("err = %v, want ErrInventoryTimeout", err)
	}
}

func TestReserveCallerCancelled(t *testing.T) {
	c := newTestClient(t, time.Second, respond(http.StatusCreated, okBody))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Reserve(ctx, testReq); !errors.Is(err, order.ErrInventoryUnavailable) {
		t.Errorf("err = %v, want ErrInventoryUnavailable", err)
	}
}

func TestReserveConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(respond(http.StatusCreated, okBody))
	url := srv.URL
	srv.Close() // nothing listens on url any more

	c, err := New(url, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Reserve(context.Background(), testReq); !errors.Is(err, order.ErrInventoryUnavailable) {
		t.Errorf("err = %v, want ErrInventoryUnavailable", err)
	}
}

func TestReserveTruncatedResponseIsAmbiguousFailure(t *testing.T) {
	// The server promises 500 bytes but sends fewer and drops the connection:
	// the reservation may exist, but the client never saw a confirmation.
	c := newTestClient(t, time.Second, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "500")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, okBody[:20])
	})
	id, err := c.Reserve(context.Background(), testReq)
	if !errors.Is(err, order.ErrInventoryUnavailable) || id != "" {
		t.Errorf("Reserve = %q, %v; want ErrInventoryUnavailable and no id", id, err)
	}
}

func TestParseBaseURL(t *testing.T) {
	tests := []struct {
		in, want string
		wantErr  bool
	}{
		{in: "http://localhost:8082", want: "http://localhost:8082"},
		{in: "http://localhost:8082/", want: "http://localhost:8082"},
		{in: "  https://inventory.internal  ", want: "https://inventory.internal"},
		{in: "http://127.0.0.1:9092", want: "http://127.0.0.1:9092"},
		{in: "", wantErr: true},
		{in: "   ", wantErr: true},
		{in: "localhost:8082", wantErr: true},
		{in: "ftp://localhost:8082", wantErr: true},
		{in: "http://", wantErr: true},
		{in: "http://:8082", wantErr: true},
		{in: "http://localhost:8082/api", wantErr: true},
		{in: "http://localhost:8082?x=1", wantErr: true},
		{in: "http://localhost:8082#frag", wantErr: true},
		{in: "http://user:pass@localhost:8082", wantErr: true},
		{in: "http://local host", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseBaseURL(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseBaseURL(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("ParseBaseURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestNewRejectsBadTimeout(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		if _, err := New(DefaultBaseURL, d); err == nil {
			t.Errorf("New with timeout %v: want error", d)
		}
	}
}
