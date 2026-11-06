// Package paymentclient talks to the Payment Service over HTTP/JSON. It
// implements order.PaymentProcessor.
//
// It speaks only the Payment Service's public HTTP API (POST /payments) and
// must never import that service's Go packages: the services are deployed
// independently and share nothing but the API.
package paymentclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/RajK158/ruptiq/demo/checkout/order-service/internal/order"
)

const (
	// DefaultBaseURL is where the Payment Service listens by default.
	DefaultBaseURL = "http://localhost:8083"

	// DefaultTimeout bounds one whole payment request.
	DefaultTimeout = 5 * time.Second

	// maxResponseBytes caps how much of an upstream response is read. Real
	// responses are a few hundred bytes.
	maxResponseBytes = 64 << 10
)

// The Payment Service reports its 409 errors as JSON {"error":"..."} without a
// separate code, so the client tells them apart by message. These strings are
// part of that service's documented API.
const (
	msgOrderAlreadyApproved = "order already has an approved payment"
	msgKeyConflict          = "idempotency_key was already used with different payment details"
)

// Client submits payments through the Payment Service's HTTP API.
// It is safe for concurrent use.
type Client struct {
	baseURL string // no trailing slash
	http    *http.Client
}

// New returns a Client for the Payment Service at baseURL, for example
// "http://localhost:8083". timeout bounds each request and must be positive.
//
// baseURL must be an absolute http or https URL with a host, and no path
// (other than "/"), query, fragment, or credentials.
func New(baseURL string, timeout time.Duration) (*Client, error) {
	base, err := ParseBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("payment client timeout must be positive, got %v", timeout)
	}
	return &Client{
		baseURL: base,
		// No automatic retries, and redirects are not followed: a redirected
		// POST would be replayed or rewritten behind the caller's back.
		http: &http.Client{
			Timeout: timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// BaseURL returns the normalized Payment Service URL the client uses.
func (c *Client) BaseURL() string { return c.baseURL }

// ParseBaseURL validates a Payment Service base URL and returns it normalized
// (without a trailing slash).
func ParseBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("payment service URL is empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid payment service URL %q: %v", raw, err)
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return "", fmt.Errorf("invalid payment service URL %q: scheme must be http or https", raw)
	case u.Hostname() == "":
		return "", fmt.Errorf("invalid payment service URL %q: host is required", raw)
	case u.User != nil:
		return "", fmt.Errorf("invalid payment service URL %q: credentials are not allowed", raw)
	case u.Path != "" && u.Path != "/":
		return "", fmt.Errorf("invalid payment service URL %q: path is not allowed", raw)
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return "", fmt.Errorf("invalid payment service URL %q: query and fragment are not allowed", raw)
	}
	return u.Scheme + "://" + u.Host, nil
}

// paymentRequest and paymentResponse mirror the Payment Service's JSON.
type paymentRequest struct {
	OrderID        string `json:"order_id"`
	AmountCents    int64  `json:"amount_cents"`
	Currency       string `json:"currency"`
	PaymentMethod  string `json:"payment_method"`
	IdempotencyKey string `json:"idempotency_key"`
}

type paymentResponse struct {
	ID          string `json:"id"`
	OrderID     string `json:"order_id"`
	AmountCents int64  `json:"amount_cents"`
	Currency    string `json:"currency"`
	Status      string `json:"status"`
}

type errorResponse struct {
	Error string `json:"error"`
}

// Pay implements order.PaymentProcessor with POST /payments. It never retries.
//
// Outcomes, all matching an order.Err* value on failure:
//   - 201 (new attempt) or 200 (replay of the same key): success, if the body
//     describes this order, amount, and currency, and its status is the one
//     the payment method selects (test_approve: approved, test_decline:
//     declined). Otherwise order.ErrPaymentBadResponse.
//   - 409 "order already has an approved payment":
//     order.ErrPaymentApprovedElsewhere.
//   - 409 idempotency conflict: order.ErrPaymentKeyConflict.
//   - network failure, cancellation, or 5xx: order.ErrPaymentUnavailable.
//   - timeout: order.ErrPaymentTimeout.
//   - anything else: order.ErrPaymentBadResponse.
func (c *Client) Pay(ctx context.Context, req order.PaymentRequest) (order.PaymentResult, error) {
	body, err := json.Marshal(paymentRequest{
		OrderID:        req.OrderID,
		AmountCents:    req.AmountCents,
		Currency:       req.Currency,
		PaymentMethod:  req.PaymentMethod,
		IdempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		return order.PaymentResult{}, fmt.Errorf("encoding payment request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/payments", bytes.NewReader(body))
	if err != nil {
		return order.PaymentResult{}, fmt.Errorf("building payment request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return order.PaymentResult{}, classifyTransportError(err)
	}
	defer resp.Body.Close()

	// A failure while reading the body is ambiguous: the payment may have been
	// recorded. Callers recover by retrying with the same idempotency key.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return order.PaymentResult{}, classifyTransportError(err)
	}
	if len(data) > maxResponseBytes {
		return order.PaymentResult{}, fmt.Errorf("%w: response larger than %d bytes", order.ErrPaymentBadResponse, maxResponseBytes)
	}

	switch {
	case resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK:
		return parsePayment(data, req)
	case resp.StatusCode == http.StatusConflict:
		switch errorMessage(data) {
		case msgOrderAlreadyApproved:
			return order.PaymentResult{}, order.ErrPaymentApprovedElsewhere
		case msgKeyConflict:
			return order.PaymentResult{}, order.ErrPaymentKeyConflict
		}
		return order.PaymentResult{}, fmt.Errorf("%w: unrecognized 409", order.ErrPaymentBadResponse)
	case resp.StatusCode >= 500:
		return order.PaymentResult{}, fmt.Errorf("%w: status %d", order.ErrPaymentUnavailable, resp.StatusCode)
	default:
		return order.PaymentResult{}, fmt.Errorf("%w: status %d", order.ErrPaymentBadResponse, resp.StatusCode)
	}
}

// parsePayment validates a success body against the request. Anything that
// does not describe exactly this attempt is rejected rather than trusted.
func parsePayment(data []byte, req order.PaymentRequest) (order.PaymentResult, error) {
	var p paymentResponse
	if err := json.Unmarshal(data, &p); err != nil {
		return order.PaymentResult{}, fmt.Errorf("%w: invalid JSON: %v", order.ErrPaymentBadResponse, err)
	}

	want := order.PaymentApproved
	if req.PaymentMethod == order.MethodDecline {
		want = order.PaymentDeclined
	}
	switch {
	case p.ID == "":
		return order.PaymentResult{}, fmt.Errorf("%w: payment has no id", order.ErrPaymentBadResponse)
	case p.OrderID != req.OrderID:
		return order.PaymentResult{}, fmt.Errorf("%w: payment is for a different order", order.ErrPaymentBadResponse)
	case p.AmountCents != req.AmountCents || p.Currency != req.Currency:
		return order.PaymentResult{}, fmt.Errorf("%w: payment amount or currency does not match", order.ErrPaymentBadResponse)
	case order.PaymentStatus(p.Status) != want:
		return order.PaymentResult{}, fmt.Errorf("%w: payment status %q, want %q", order.ErrPaymentBadResponse, p.Status, want)
	}
	return order.PaymentResult{
		ID:          p.ID,
		Status:      want,
		AmountCents: p.AmountCents,
		Currency:    p.Currency,
	}, nil
}

// errorMessage extracts {"error":"..."} from an error body, or "".
func errorMessage(data []byte) string {
	var e errorResponse
	if err := json.Unmarshal(data, &e); err != nil {
		return ""
	}
	return e.Error
}

// classifyTransportError maps a failed HTTP exchange to an order error.
func classifyTransportError(err error) error {
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return fmt.Errorf("%w: %v", order.ErrPaymentTimeout, err)
	}
	return fmt.Errorf("%w: %v", order.ErrPaymentUnavailable, err)
}
