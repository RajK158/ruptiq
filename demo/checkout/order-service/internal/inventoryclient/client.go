// Package inventoryclient talks to the Inventory Service over HTTP/JSON. It
// implements order.InventoryReserver.
//
// It speaks only the Inventory Service's public HTTP API (POST /reservations).
// It must never import the Inventory Service's Go packages: the two services
// are deployed independently and share nothing but that API.
package inventoryclient

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
	// DefaultBaseURL is where the Inventory Service listens by default.
	DefaultBaseURL = "http://localhost:8082"

	// DefaultTimeout bounds one whole reservation request: connecting,
	// sending, and reading the answer.
	DefaultTimeout = 5 * time.Second

	// maxResponseBytes caps how much of an upstream response is read. Real
	// responses are a few hundred bytes.
	maxResponseBytes = 64 << 10
)

// The Inventory Service reports its business errors as JSON {"error":"..."}
// with no separate error code, so the client tells them apart by status and
// message. These strings are part of that service's documented API.
const (
	msgInsufficientStock = "insufficient stock"
	msgUnknownSKU        = "unknown sku"
)

// Client reserves stock through the Inventory Service's HTTP API.
// It is safe for concurrent use.
type Client struct {
	baseURL string // no trailing slash
	http    *http.Client
}

// New returns a Client for the Inventory Service at baseURL, for example
// "http://localhost:8082". timeout bounds each request and must be positive.
//
// baseURL must be an absolute http or https URL with a host, and no path
// (other than "/"), query, fragment, or credentials.
func New(baseURL string, timeout time.Duration) (*Client, error) {
	base, err := ParseBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("inventory client timeout must be positive, got %v", timeout)
	}
	return &Client{
		baseURL: base,
		// The client never retries on its own, and does not follow redirects:
		// a POST that is redirected would be silently replayed or rewritten.
		http: &http.Client{
			Timeout: timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// BaseURL returns the normalized Inventory Service URL the client uses.
func (c *Client) BaseURL() string { return c.baseURL }

// ParseBaseURL validates an Inventory Service base URL and returns it
// normalized (without a trailing slash).
func ParseBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("inventory service URL is empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid inventory service URL %q: %v", raw, err)
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return "", fmt.Errorf("invalid inventory service URL %q: scheme must be http or https", raw)
	case u.Hostname() == "":
		return "", fmt.Errorf("invalid inventory service URL %q: host is required", raw)
	case u.User != nil:
		return "", fmt.Errorf("invalid inventory service URL %q: credentials are not allowed", raw)
	case u.Path != "" && u.Path != "/":
		return "", fmt.Errorf("invalid inventory service URL %q: path is not allowed", raw)
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return "", fmt.Errorf("invalid inventory service URL %q: query and fragment are not allowed", raw)
	}
	return u.Scheme + "://" + u.Host, nil
}

// reserveRequest and reservationResponse mirror the Inventory Service's JSON.
type reserveRequest struct {
	OrderID  string `json:"order_id"`
	SKU      string `json:"sku"`
	Quantity int    `json:"quantity"`
}

type reservationResponse struct {
	ID       string `json:"id"`
	OrderID  string `json:"order_id"`
	SKU      string `json:"sku"`
	Quantity int    `json:"quantity"`
	Status   string `json:"status"`
}

type errorResponse struct {
	Error string `json:"error"`
}

// Reserve implements order.InventoryReserver with POST /reservations.
//
// The request carries the order ID as the Inventory Service's idempotency
// key, so repeating a call never reserves twice. Reserve itself never retries.
//
// Errors always match one of the order.Err* values:
//   - 201 or 200 with a valid, matching body: success.
//   - 409 "insufficient stock": order.ErrInsufficientStock.
//   - 404 "unknown sku": order.ErrUnknownSKU.
//   - any other 409: order.ErrInventoryConflict.
//   - network failure, or a 5xx status: order.ErrInventoryUnavailable.
//   - timeout: order.ErrInventoryTimeout.
//   - anything else, including malformed or mismatched bodies:
//     order.ErrInventoryBadResponse.
func (c *Client) Reserve(ctx context.Context, req order.ReserveRequest) (string, error) {
	body, err := json.Marshal(reserveRequest{OrderID: req.OrderID, SKU: req.SKU, Quantity: req.Quantity})
	if err != nil {
		return "", fmt.Errorf("encoding reservation request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/reservations", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("building reservation request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", classifyTransportError(err)
	}
	defer resp.Body.Close()

	// A failure while reading the body is ambiguous: the Inventory Service
	// may have reserved stock even though we never saw the answer. Callers
	// recover by retrying with the same order ID.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return "", classifyTransportError(err)
	}
	if len(data) > maxResponseBytes {
		return "", fmt.Errorf("%w: response larger than %d bytes", order.ErrInventoryBadResponse, maxResponseBytes)
	}

	switch {
	case resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK:
		return parseReservation(data, req)

	case resp.StatusCode == http.StatusConflict:
		if errorMessage(data) == msgInsufficientStock {
			return "", order.ErrInsufficientStock
		}
		return "", order.ErrInventoryConflict

	case resp.StatusCode == http.StatusNotFound:
		// A bare 404 (for example a wrong base URL) is not the same as
		// "unknown sku", so require the message.
		if errorMessage(data) == msgUnknownSKU {
			return "", order.ErrUnknownSKU
		}
		return "", fmt.Errorf("%w: status 404 without an unknown-sku error", order.ErrInventoryBadResponse)

	case resp.StatusCode >= 500:
		return "", fmt.Errorf("%w: status %d", order.ErrInventoryUnavailable, resp.StatusCode)

	default:
		return "", fmt.Errorf("%w: status %d", order.ErrInventoryBadResponse, resp.StatusCode)
	}
}

// parseReservation validates a success body. It must describe exactly the
// reservation that was asked for; anything else is treated as a bad response
// rather than trusted.
func parseReservation(data []byte, req order.ReserveRequest) (string, error) {
	var r reservationResponse
	if err := json.Unmarshal(data, &r); err != nil {
		return "", fmt.Errorf("%w: invalid JSON: %v", order.ErrInventoryBadResponse, err)
	}
	switch {
	case r.ID == "":
		return "", fmt.Errorf("%w: reservation has no id", order.ErrInventoryBadResponse)
	case r.Status != "reserved":
		return "", fmt.Errorf("%w: reservation status is %q", order.ErrInventoryBadResponse, r.Status)
	case r.OrderID != req.OrderID || r.SKU != req.SKU || r.Quantity != req.Quantity:
		return "", fmt.Errorf("%w: reservation does not match the request", order.ErrInventoryBadResponse)
	}
	return r.ID, nil
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
		return fmt.Errorf("%w: %v", order.ErrInventoryTimeout, err)
	}
	// Includes connection refused, DNS failure, connection reset mid-response,
	// and the caller cancelling the request.
	return fmt.Errorf("%w: %v", order.ErrInventoryUnavailable, err)
}
