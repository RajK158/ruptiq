// Package order holds the Order Service's domain model: what an order is and
// which orders are valid. It knows nothing about HTTP.
package order

import (
	"crypto/rand"
	"fmt"
	"strings"
	"time"
)

// Limits for a valid order.
const (
	MinQuantity = 1
	MaxQuantity = 10
	MaxSKULen   = 64
)

// Status describes where an order is in its life.
type Status string

// StatusCreated means ONLY that the order record was created. It does not mean
// inventory was reserved, payment was taken, or checkout completed.
const StatusCreated Status = "created"

// StatusReserved means the Inventory Service confirmed a stock reservation for
// this order. It does NOT mean payment was taken or checkout completed:
// payment integration does not exist yet. Orders only ever move forward, from
// created to reserved; nothing moves them back.
const StatusReserved Status = "reserved"

// Order is a request to buy Quantity units of SKU.
//
// Order must stay comparable with == (no slices, maps or pointers); tests rely
// on it.
type Order struct {
	ID       string `json:"id"`
	SKU      string `json:"sku"`
	Quantity int    `json:"quantity"`
	Status   Status `json:"status"`
	// ReservationID is the Inventory Service's reservation ID. It is set
	// only once the order is reserved, and omitted from JSON before that.
	ReservationID string    `json:"reservation_id,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// ValidationError reports why an order request was rejected. Its message is
// safe to show to API clients.
type ValidationError struct {
	Message string
}

func (e *ValidationError) Error() string { return e.Message }

// New validates the request and returns a new order with a unique ID, status
// "created", and the current time. It does not store the order.
func New(sku string, quantity int) (Order, error) {
	sku = strings.TrimSpace(sku)
	if sku == "" {
		return Order{}, &ValidationError{Message: "sku is required"}
	}
	if len(sku) > MaxSKULen {
		return Order{}, &ValidationError{Message: fmt.Sprintf("sku must be at most %d characters", MaxSKULen)}
	}
	if quantity < MinQuantity || quantity > MaxQuantity {
		return Order{}, &ValidationError{Message: fmt.Sprintf("quantity must be between %d and %d", MinQuantity, MaxQuantity)}
	}

	return Order{
		ID:        newID(),
		SKU:       sku,
		Quantity:  quantity,
		Status:    StatusCreated,
		CreatedAt: time.Now().UTC(),
	}, nil
}

// newID returns an unpredictable, collision-resistant ID such as
// "ord_7XQ2K5M3...". rand.Text draws 128 bits from crypto/rand, so two IDs
// colliding is not a realistic concern.
func newID() string {
	return "ord_" + rand.Text()
}
