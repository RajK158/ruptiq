package paymentclient

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

var approveReq = order.PaymentRequest{
	OrderID:        "ord_1",
	AmountCents:    259998,
	Currency:       "USD",
	PaymentMethod:  order.MethodApprove,
	IdempotencyKey: "ord_1:attempt-1",
}

const approvedBody = `{"id":"pay_ABC","order_id":"ord_1","amount_cents":259998,"currency":"USD","status":"approved","created_at":"2026-11-06T00:00:00Z"}`
const declinedBody = `{"id":"pay_DEF","order_id":"ord_1","amount_cents":259998,"currency":"USD","status":"declined","created_at":"2026-11-06T00:00:00Z"}`

func declineReq() order.PaymentRequest {
	r := approveReq
	r.PaymentMethod = order.MethodDecline
	return r
}

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

func respond(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}
}

func TestPaySendsExpectedRequest(t *testing.T) {
	var gotMethod, gotPath, gotType string
	var gotBody map[string]any
	c := newTestClient(t, time.Second, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotType = r.Method, r.URL.Path, r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decoding request: %v", err)
		}
		respond(http.StatusCreated, approvedBody)(w, r)
	})

	res, err := c.Pay(context.Background(), approveReq)
	if err != nil {
		t.Fatalf("Pay: %v", err)
	}
	if res != (order.PaymentResult{ID: "pay_ABC", Status: order.PaymentApproved, AmountCents: 259998, Currency: "USD"}) {
		t.Errorf("result = %+v", res)
	}
	if gotMethod != http.MethodPost || gotPath != "/payments" || gotType != "application/json" {
		t.Errorf("request = %s %s (%q), want POST /payments as JSON", gotMethod, gotPath, gotType)
	}
	// Exactly the five fields the Payment Service accepts; it rejects others.
	want := map[string]any{
		"order_id": "ord_1", "amount_cents": float64(259998), "currency": "USD",
		"payment_method": "test_approve", "idempotency_key": "ord_1:attempt-1",
	}
	if len(gotBody) != len(want) {
		t.Errorf("request body = %v, want %v", gotBody, want)
	}
	for k, v := range want {
		if gotBody[k] != v {
			t.Errorf("request body[%q] = %v, want %v", k, gotBody[k], v)
		}
	}
}

func TestPayReplayAndDecline(t *testing.T) {
	// 200 is a replay of the same key; it is as valid as 201.
	c := newTestClient(t, time.Second, respond(http.StatusOK, approvedBody))
	if res, err := c.Pay(context.Background(), approveReq); err != nil || res.ID != "pay_ABC" {
		t.Errorf("replay = %+v, %v", res, err)
	}

	// A decline is a successful call with a declined result, not an error.
	c = newTestClient(t, time.Second, respond(http.StatusCreated, declinedBody))
	res, err := c.Pay(context.Background(), declineReq())
	if err != nil || res.Status != order.PaymentDeclined || res.ID != "pay_DEF" {
		t.Errorf("decline = %+v, %v", res, err)
	}
}

func TestPayErrorMapping(t *testing.T) {
	tests := []struct {
		name    string
		req     order.PaymentRequest
		status  int
		body    string
		wantErr error
	}{
		{"order already approved", approveReq, 409, `{"error":"order already has an approved payment"}`, order.ErrPaymentApprovedElsewhere},
		{"key conflict", approveReq, 409, `{"error":"idempotency_key was already used with different payment details"}`, order.ErrPaymentKeyConflict},
		{"unrecognized 409", approveReq, 409, `{"error":"something else"}`, order.ErrPaymentBadResponse},
		{"500", approveReq, 500, `{"error":"internal server error"}`, order.ErrPaymentUnavailable},
		{"503 empty", approveReq, 503, ``, order.ErrPaymentUnavailable},
		{"422 rejected", approveReq, 422, `{"error":"currency must be USD"}`, order.ErrPaymentBadResponse},
		{"404", approveReq, 404, `{"error":"not found"}`, order.ErrPaymentBadResponse},
		{"unexpected 202", approveReq, 202, approvedBody, order.ErrPaymentBadResponse},
		{"malformed JSON", approveReq, 201, `{"id":"pay_ABC",`, order.ErrPaymentBadResponse},
		{"not JSON", approveReq, 201, `approved`, order.ErrPaymentBadResponse},
		{"empty body", approveReq, 201, ``, order.ErrPaymentBadResponse},
		{"missing id", approveReq, 201, `{"order_id":"ord_1","amount_cents":259998,"currency":"USD","status":"approved"}`, order.ErrPaymentBadResponse},
		{"other order", approveReq, 201, `{"id":"pay_ABC","order_id":"ord_2","amount_cents":259998,"currency":"USD","status":"approved"}`, order.ErrPaymentBadResponse},
		{"other amount", approveReq, 201, `{"id":"pay_ABC","order_id":"ord_1","amount_cents":1,"currency":"USD","status":"approved"}`, order.ErrPaymentBadResponse},
		{"other currency", approveReq, 201, `{"id":"pay_ABC","order_id":"ord_1","amount_cents":259998,"currency":"EUR","status":"approved"}`, order.ErrPaymentBadResponse},
		{"approve answered with decline", approveReq, 201, declinedBody, order.ErrPaymentBadResponse},
		{"decline answered with approval", declineReq(), 201, approvedBody, order.ErrPaymentBadResponse},
		{"unknown status", approveReq, 201, `{"id":"pay_ABC","order_id":"ord_1","amount_cents":259998,"currency":"USD","status":"pending"}`, order.ErrPaymentBadResponse},
		{"oversized body", approveReq, 201, `{"id":"` + strings.Repeat("x", maxResponseBytes) + `"}`, order.ErrPaymentBadResponse},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestClient(t, time.Second, respond(tt.status, tt.body))
			res, err := c.Pay(context.Background(), tt.req)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("err = %v, want %v", err, tt.wantErr)
			}
			if res != (order.PaymentResult{}) {
				t.Errorf("result = %+v on failure, want zero", res)
			}
		})
	}
}

func TestPayRedirectNotFollowed(t *testing.T) {
	followed := false
	c := newTestClient(t, time.Second, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			followed = true
		}
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	})
	if _, err := c.Pay(context.Background(), approveReq); !errors.Is(err, order.ErrPaymentBadResponse) {
		t.Errorf("err = %v, want ErrPaymentBadResponse", err)
	}
	if followed {
		t.Error("redirect was followed")
	}
}

func TestPayTimeout(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	c := newTestClient(t, 50*time.Millisecond, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	start := time.Now()
	if _, err := c.Pay(context.Background(), approveReq); !errors.Is(err, order.ErrPaymentTimeout) {
		t.Fatalf("err = %v, want ErrPaymentTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Pay took %v; the timeout did not bound it", elapsed)
	}
}

func TestPayRespectsCallerContext(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	c := newTestClient(t, 10*time.Second, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.Pay(ctx, approveReq); !errors.Is(err, order.ErrPaymentTimeout) {
		t.Errorf("deadline: err = %v, want ErrPaymentTimeout", err)
	}

	cancelled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if _, err := c.Pay(cancelled, approveReq); !errors.Is(err, order.ErrPaymentUnavailable) {
		t.Errorf("cancelled: err = %v, want ErrPaymentUnavailable", err)
	}
}

func TestPayConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(respond(201, approvedBody))
	url := srv.URL
	srv.Close()
	c, err := New(url, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Pay(context.Background(), approveReq); !errors.Is(err, order.ErrPaymentUnavailable) {
		t.Errorf("err = %v, want ErrPaymentUnavailable", err)
	}
}

func TestPayTruncatedResponseIsAmbiguousFailure(t *testing.T) {
	c := newTestClient(t, time.Second, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "500")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, approvedBody[:20])
	})
	res, err := c.Pay(context.Background(), approveReq)
	if !errors.Is(err, order.ErrPaymentUnavailable) || res.ID != "" {
		t.Errorf("Pay = %+v, %v; want ErrPaymentUnavailable and no result", res, err)
	}
}

func TestParseBaseURL(t *testing.T) {
	tests := []struct {
		in, want string
		wantErr  bool
	}{
		{in: "http://localhost:8083", want: "http://localhost:8083"},
		{in: "http://localhost:8083/", want: "http://localhost:8083"},
		{in: " https://payments.internal ", want: "https://payments.internal"},
		{in: "", wantErr: true},
		{in: "localhost:8083", wantErr: true},
		{in: "ftp://localhost:8083", wantErr: true},
		{in: "http://", wantErr: true},
		{in: "http://localhost:8083/api", wantErr: true},
		{in: "http://localhost:8083?x=1", wantErr: true},
		{in: "http://localhost:8083#f", wantErr: true},
		{in: "http://user:pw@localhost:8083", wantErr: true},
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
