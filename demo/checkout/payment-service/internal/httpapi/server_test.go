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
	"time"

	"github.com/RajK158/ruptiq/demo/checkout/payment-service/internal/payment"
)

func newTestHandler() http.Handler {
	return NewHandler(payment.NewStore(), nil)
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

func payBody(order, key, method string, amount int) string {
	return fmt.Sprintf(`{"order_id":%q,"amount_cents":%d,"currency":"USD","payment_method":%q,"idempotency_key":%q}`,
		order, amount, method, key)
}

func approveBody(order, key string) string { return payBody(order, key, "test_approve", 24999) }
func declineBody(order, key string) string { return payBody(order, key, "test_decline", 24999) }

func TestHealthz(t *testing.T) {
	rec := do(newTestHandler(), http.MethodGet, "/healthz", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	got := decode[healthResponse](t, rec)
	if got.Status != "ok" || got.Service != "payment-service" {
		t.Errorf("body = %+v, want status ok from payment-service", got)
	}
}

func TestCreatePaymentApproved(t *testing.T) {
	before := time.Now()
	rec := do(newTestHandler(), http.MethodPost, "/payments", approveBody("ord_example123", "checkout-attempt-001"))
	after := time.Now()

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body: %q)", rec.Code, rec.Body.String())
	}
	got := decode[payment.Payment](t, rec)
	if !strings.HasPrefix(got.ID, "pay_") {
		t.Errorf("ID = %q, want pay_ prefix", got.ID)
	}
	if got.OrderID != "ord_example123" || got.AmountCents != 24999 || got.Currency != "USD" || got.Status != payment.StatusApproved {
		t.Errorf("payment = %+v", got)
	}
	if got.CreatedAt.Before(before) || got.CreatedAt.After(after) {
		t.Errorf("CreatedAt = %v, want between %v and %v", got.CreatedAt, before, after)
	}
	if loc := rec.Header().Get("Location"); loc != "/payments/"+got.ID {
		t.Errorf("Location = %q, want %q", loc, "/payments/"+got.ID)
	}

	// The response contains exactly the documented fields: nothing about
	// the idempotency key or payment method leaks into it.
	var fields map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"id", "order_id", "amount_cents", "currency", "status", "created_at"} {
		if _, ok := fields[name]; !ok {
			t.Errorf("response is missing %q", name)
		}
	}
	if len(fields) != 6 {
		t.Errorf("response has %d fields, want 6: %v", len(fields), fields)
	}
}

func TestCreatePaymentDeclinedIsNotAnError(t *testing.T) {
	rec := do(newTestHandler(), http.MethodPost, "/payments", declineBody("ord_1", "key-1"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body: %q)", rec.Code, rec.Body.String())
	}
	if got := decode[payment.Payment](t, rec); got.Status != payment.StatusDeclined {
		t.Errorf("Status = %q, want declined", got.Status)
	}
}

func TestGetPayment(t *testing.T) {
	h := newTestHandler()
	created := decode[payment.Payment](t, do(h, http.MethodPost, "/payments", approveBody("ord_1", "key-1")))
	declined := decode[payment.Payment](t, do(h, http.MethodPost, "/payments", declineBody("ord_2", "key-2")))

	for _, want := range []payment.Payment{created, declined} {
		rec := do(h, http.MethodGet, "/payments/"+want.ID, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body: %q)", rec.Code, rec.Body.String())
		}
		got := decode[payment.Payment](t, rec)
		if !got.CreatedAt.Equal(want.CreatedAt) {
			t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, want.CreatedAt)
		}
		got.CreatedAt = want.CreatedAt // compare the rest field by field
		if got != want {
			t.Errorf("retrieved = %+v, want %+v", got, want)
		}
	}
}

func TestGetPaymentNotFound(t *testing.T) {
	wantError(t, do(newTestHandler(), http.MethodGet, "/payments/pay_does-not-exist", ""), http.StatusNotFound)
}

func TestIdenticalRetryReturnsOriginal(t *testing.T) {
	h := newTestHandler()
	body := approveBody("ord_1", "key-1")

	first := do(h, http.MethodPost, "/payments", body)
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d, want 201", first.Code)
	}
	want := decode[payment.Payment](t, first)

	for i := 0; i < 3; i++ {
		retry := do(h, http.MethodPost, "/payments", body)
		if retry.Code != http.StatusOK {
			t.Fatalf("retry %d status = %d, want 200 (body: %q)", i, retry.Code, retry.Body.String())
		}
		if got := decode[payment.Payment](t, retry); got.ID != want.ID || !got.CreatedAt.Equal(want.CreatedAt) || got.Status != want.Status {
			t.Errorf("retry %d = %+v, want original %+v", i, got, want)
		}
		if loc := retry.Header().Get("Location"); loc != "" {
			t.Errorf("retry set Location %q; only creations should", loc)
		}
	}
}

func TestIdenticalRetryOfDeclineReturnsDecline(t *testing.T) {
	h := newTestHandler()
	body := declineBody("ord_1", "key-1")
	first := decode[payment.Payment](t, do(h, http.MethodPost, "/payments", body))
	retry := do(h, http.MethodPost, "/payments", body)
	if retry.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", retry.Code)
	}
	if got := decode[payment.Payment](t, retry); got.ID != first.ID || got.Status != payment.StatusDeclined {
		t.Errorf("retry = %+v, want original decline %+v", got, first)
	}
}

func TestConflictingIdempotencyKey(t *testing.T) {
	h := newTestHandler()
	if rec := do(h, http.MethodPost, "/payments", approveBody("ord_1", "key-1")); rec.Code != http.StatusCreated {
		t.Fatalf("first status = %d", rec.Code)
	}
	wantError(t, do(h, http.MethodPost, "/payments", payBody("ord_1", "key-1", "test_approve", 100)), http.StatusConflict)
	wantError(t, do(h, http.MethodPost, "/payments", payBody("ord_1", "key-1", "test_decline", 24999)), http.StatusConflict)
	wantError(t, do(h, http.MethodPost, "/payments", approveBody("ord_other", "key-1")), http.StatusConflict)
}

func TestDeclineThenApproveWithNewKey(t *testing.T) {
	h := newTestHandler()
	declined := decode[payment.Payment](t, do(h, http.MethodPost, "/payments", declineBody("ord_1", "key-1")))

	rec := do(h, http.MethodPost, "/payments", approveBody("ord_1", "key-2"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body: %q)", rec.Code, rec.Body.String())
	}
	approved := decode[payment.Payment](t, rec)
	if approved.Status != payment.StatusApproved || approved.ID == declined.ID {
		t.Errorf("approved = %+v, declined = %+v", approved, declined)
	}
}

func TestSecondApprovalForSameOrderRejected(t *testing.T) {
	h := newTestHandler()
	first := decode[payment.Payment](t, do(h, http.MethodPost, "/payments", approveBody("ord_1", "key-1")))

	wantError(t, do(h, http.MethodPost, "/payments", approveBody("ord_1", "key-2")), http.StatusConflict)

	// The original retry still works, and the original is unchanged.
	retry := do(h, http.MethodPost, "/payments", approveBody("ord_1", "key-1"))
	if retry.Code != http.StatusOK {
		t.Errorf("retry status = %d, want 200", retry.Code)
	}
	got := decode[payment.Payment](t, do(h, http.MethodGet, "/payments/"+first.ID, ""))
	if got.Status != payment.StatusApproved {
		t.Errorf("original status = %q", got.Status)
	}
}

func TestCreatePaymentInvalid(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		// 400: the body is not JSON we can read.
		{"empty body", ``, http.StatusBadRequest},
		{"malformed JSON", `{"order_id":`, http.StatusBadRequest},
		{"not an object", `[1,2]`, http.StatusBadRequest},
		{"unknown field", `{"order_id":"o","amount_cents":100,"currency":"USD","payment_method":"test_approve","idempotency_key":"k","tip":5}`, http.StatusBadRequest},
		{"card number field", `{"order_id":"o","amount_cents":100,"currency":"USD","payment_method":"test_approve","idempotency_key":"k","card_number":"4111111111111111"}`, http.StatusBadRequest},
		{"amount is a string", `{"order_id":"o","amount_cents":"100","currency":"USD","payment_method":"test_approve","idempotency_key":"k"}`, http.StatusBadRequest},
		{"amount is fractional", `{"order_id":"o","amount_cents":10.5,"currency":"USD","payment_method":"test_approve","idempotency_key":"k"}`, http.StatusBadRequest},
		{"amount overflows int64", `{"order_id":"o","amount_cents":99999999999999999999,"currency":"USD","payment_method":"test_approve","idempotency_key":"k"}`, http.StatusBadRequest},
		{"order_id is a number", `{"order_id":5,"amount_cents":100,"currency":"USD","payment_method":"test_approve","idempotency_key":"k"}`, http.StatusBadRequest},
		{"trailing object", payBody("o", "k", "test_approve", 100) + `{"x":1}`, http.StatusBadRequest},
		{"trailing garbage", payBody("o", "k", "test_approve", 100) + ` xyz`, http.StatusBadRequest},

		// 413: body too large.
		{"too large", `{"order_id":"` + strings.Repeat("a", maxBodyBytes) + `"}`, http.StatusRequestEntityTooLarge},

		// 422: well-formed JSON, unacceptable values.
		{"missing order_id", `{"amount_cents":100,"currency":"USD","payment_method":"test_approve","idempotency_key":"k"}`, http.StatusUnprocessableEntity},
		{"blank order_id", payBody("  ", "k", "test_approve", 100), http.StatusUnprocessableEntity},
		{"missing idempotency_key", `{"order_id":"o","amount_cents":100,"currency":"USD","payment_method":"test_approve"}`, http.StatusUnprocessableEntity},
		{"blank idempotency_key", payBody("o", " ", "test_approve", 100), http.StatusUnprocessableEntity},
		{"missing amount", `{"order_id":"o","currency":"USD","payment_method":"test_approve","idempotency_key":"k"}`, http.StatusUnprocessableEntity},
		{"zero amount", payBody("o", "k", "test_approve", 0), http.StatusUnprocessableEntity},
		{"negative amount", payBody("o", "k", "test_approve", -100), http.StatusUnprocessableEntity},
		{"amount over limit", payBody("o", "k", "test_approve", payment.MaxAmountCents+1), http.StatusUnprocessableEntity},
		{"missing currency", `{"order_id":"o","amount_cents":100,"payment_method":"test_approve","idempotency_key":"k"}`, http.StatusUnprocessableEntity},
		{"unsupported currency", `{"order_id":"o","amount_cents":100,"currency":"EUR","payment_method":"test_approve","idempotency_key":"k"}`, http.StatusUnprocessableEntity},
		{"missing payment method", `{"order_id":"o","amount_cents":100,"currency":"USD","idempotency_key":"k"}`, http.StatusUnprocessableEntity},
		{"unsupported payment method", payBody("o", "k", "credit_card", 100), http.StatusUnprocessableEntity},
		{"order_id too long", payBody(strings.Repeat("o", payment.MaxOrderIDLen+1), "k", "test_approve", 100), http.StatusUnprocessableEntity},
		{"idempotency_key too long", payBody("o", strings.Repeat("k", payment.MaxIdempotencyKeyLen+1), "test_approve", 100), http.StatusUnprocessableEntity},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTestHandler()
			wantError(t, do(h, http.MethodPost, "/payments", tt.body), tt.wantStatus)
		})
	}
}

func TestRejectedRequestsCreateNothing(t *testing.T) {
	h := newTestHandler()
	do(h, http.MethodPost, "/payments", payBody("ord_1", "key-1", "credit_card", 100)) // 422
	do(h, http.MethodPost, "/payments", `{"order_id":`)                                // 400

	// Key and order are still free: the first valid request creates.
	if rec := do(h, http.MethodPost, "/payments", approveBody("ord_1", "key-1")); rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want 201", rec.Code)
	}
}

func TestSensitiveDataIsNotEchoedOrLogged(t *testing.T) {
	const card = "4111111111111111"
	var logs bytes.Buffer
	h := NewHandler(payment.NewStore(), slog.New(slog.NewJSONHandler(&logs, nil)))

	rec := do(h, http.MethodPost, "/payments",
		`{"order_id":"o","amount_cents":100,"currency":"USD","payment_method":"test_approve","idempotency_key":"k","card_number":"`+card+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if strings.Contains(rec.Body.String(), card) || strings.Contains(logs.String(), card) {
		t.Errorf("card number leaked.\nresponse: %s\nlogs: %s", rec.Body.String(), logs.String())
	}
}

func TestSuccessfulPaymentLogsContainNoRequestBody(t *testing.T) {
	var logs bytes.Buffer
	h := NewHandler(payment.NewStore(), slog.New(slog.NewJSONHandler(&logs, nil)))

	rec := do(h, http.MethodPost, "/payments", approveBody("ord_1", "secret-looking-key"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d", rec.Code)
	}
	out := logs.String()
	for _, want := range []string{`"msg":"payment created"`, `"order_id":"ord_1"`, `"status":"approved"`, `"msg":"request"`} {
		if !strings.Contains(out, want) {
			t.Errorf("log output missing %s:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"secret-looking-key", "amount_cents", "test_approve"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("log output contains %q:\n%s", unwanted, out)
		}
	}
}

func TestUnsupportedMethods(t *testing.T) {
	tests := []struct {
		method, path, allow string
	}{
		{http.MethodPost, "/healthz", "GET, HEAD"},
		{http.MethodDelete, "/healthz", "GET, HEAD"},
		{http.MethodGet, "/payments", "POST"},
		{http.MethodPut, "/payments", "POST"},
		{http.MethodDelete, "/payments", "POST"},
		{http.MethodPost, "/payments/pay_anything", "GET, HEAD"},
		{http.MethodPut, "/payments/pay_anything", "GET, HEAD"},
		{http.MethodDelete, "/payments/pay_anything", "GET, HEAD"},
		{http.MethodPatch, "/payments/pay_anything", "GET, HEAD"},
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

func TestUnknownPath(t *testing.T) {
	for _, path := range []string{"/", "/nope", "/payments/", "/payments/a/b"} {
		t.Run(path, func(t *testing.T) {
			wantError(t, do(newTestHandler(), http.MethodGet, path, ""), http.StatusNotFound)
		})
	}
}

// post sends a POST through a real HTTP connection and returns the status and
// decoded payment (zero if the response is not a payment).
func post(t *testing.T, url, body string) (int, payment.Payment) {
	t.Helper()
	resp, err := http.Post(url+"/payments", "application/json", strings.NewReader(body))
	if err != nil {
		t.Errorf("request failed: %v", err)
		return 0, payment.Payment{}
	}
	defer resp.Body.Close()
	var p payment.Payment
	_ = json.NewDecoder(resp.Body).Decode(&p) // error bodies decode to a zero Payment
	return resp.StatusCode, p
}

// runConcurrently sends n requests at the same moment; body(i) builds each one.
func runConcurrently(t *testing.T, url string, n int, body func(i int) string) (statuses map[int]int, payments []payment.Payment) {
	t.Helper()
	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)
	statuses = make(map[int]int)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			code, p := post(t, url, body(i))
			mu.Lock()
			statuses[code]++
			if p.ID != "" {
				payments = append(payments, p)
			}
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()
	return statuses, payments
}

func TestConcurrentIdenticalRetriesOverHTTP(t *testing.T) {
	const attempts = 150
	srv := httptest.NewServer(newTestHandler())
	defer srv.Close()

	body := approveBody("ord_1", "key-same")
	statuses, payments := runConcurrently(t, srv.URL, attempts, func(int) string { return body })

	if statuses[http.StatusCreated] != 1 || statuses[http.StatusOK] != attempts-1 {
		t.Errorf("statuses = %v, want one 201 and %d x 200", statuses, attempts-1)
	}
	if len(payments) != attempts {
		t.Fatalf("got %d payments, want %d", len(payments), attempts)
	}
	for _, p := range payments {
		if p.ID != payments[0].ID || p.Status != payment.StatusApproved || !p.CreatedAt.Equal(payments[0].CreatedAt) {
			t.Fatalf("responses differ: %+v vs %+v", p, payments[0])
		}
	}
}

func TestConcurrentApprovalsForSameOrderOverHTTP(t *testing.T) {
	const attempts = 150
	srv := httptest.NewServer(newTestHandler())
	defer srv.Close()

	statuses, payments := runConcurrently(t, srv.URL, attempts, func(i int) string {
		return approveBody("ord_1", fmt.Sprintf("key-%d", i)) // distinct keys, same order
	})

	if statuses[http.StatusCreated] != 1 || statuses[http.StatusConflict] != attempts-1 {
		t.Errorf("statuses = %v, want one 201 and %d x 409", statuses, attempts-1)
	}
	approved := 0
	for _, p := range payments {
		if p.Status == payment.StatusApproved {
			approved++
		}
	}
	if len(payments) != 1 || approved != 1 {
		t.Errorf("%d payment bodies, %d approved; want exactly 1 and 1", len(payments), approved)
	}
}

func TestConcurrentDeclinesForSameOrderAreAllRecorded(t *testing.T) {
	// Declines do not mark an order as paid, so distinct keys all succeed.
	const attempts = 100
	srv := httptest.NewServer(newTestHandler())
	defer srv.Close()

	statuses, payments := runConcurrently(t, srv.URL, attempts, func(i int) string {
		return declineBody("ord_1", fmt.Sprintf("key-%d", i))
	})
	if statuses[http.StatusCreated] != attempts {
		t.Errorf("statuses = %v, want %d x 201", statuses, attempts)
	}
	ids := make(map[string]bool)
	for _, p := range payments {
		if p.Status != payment.StatusDeclined {
			t.Errorf("status = %q, want declined", p.Status)
		}
		ids[p.ID] = true
	}
	if len(ids) != attempts {
		t.Errorf("%d distinct payment IDs, want %d", len(ids), attempts)
	}
}
