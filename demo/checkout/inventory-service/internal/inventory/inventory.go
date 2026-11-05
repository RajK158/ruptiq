// Package inventory holds the Inventory Service's business rules: what stock
// is, what a valid reservation request is, and how reservations change stock.
// It knows nothing about HTTP.
package inventory

import (
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Limits for a valid reservation request.
const (
	MinQuantity = 1
	MaxQuantity = 1000
	MaxSKULen   = 64
	MaxOrderLen = 64
)

// StatusReserved means ONLY that stock is held for an order. It is not a
// completed purchase: payment and order finalization do not exist yet, and
// nothing releases or confirms a reservation.
const StatusReserved = "reserved"

// Errors returned by Store. Use errors.Is to test for them.
var (
	// ErrUnknownSKU means no product in the catalog has the requested SKU.
	ErrUnknownSKU = errors.New("unknown sku")
	// ErrInsufficientStock means fewer units are available than requested.
	ErrInsufficientStock = errors.New("insufficient stock")
	// ErrOrderConflict means the order ID already has a reservation with
	// different details (another SKU or quantity).
	ErrOrderConflict = errors.New("order_id already has a reservation with different details")
)

// ValidationError reports why a request was rejected. Its message is safe to
// show to API clients.
type ValidationError struct {
	Message string
}

func (e *ValidationError) Error() string { return e.Message }

// Item is a point-in-time view of one product's stock.
type Item struct {
	SKU string `json:"sku"`
	// Available units can still be reserved.
	Available int `json:"available"`
	// Reserved units are held for orders. Available + Reserved never changes
	// while the service runs, because reservations are never released.
	Reserved int `json:"reserved"`
}

// ReserveRequest asks to hold Quantity units of SKU for OrderID.
type ReserveRequest struct {
	OrderID  string
	SKU      string
	Quantity int
}

// Reservation is a recorded hold on stock.
type Reservation struct {
	ID        string    `json:"id"`
	OrderID   string    `json:"order_id"`
	SKU       string    `json:"sku"`
	Quantity  int       `json:"quantity"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

// SeedStock returns the demo catalog: SKU to starting units.
//
// This data is for testing and demos only. It is fixed and deterministic so
// that load tests and manual checks start from the same known state. A real
// service would load stock from a database.
func SeedStock() map[string]int {
	return map[string]int{
		"LAPTOP-001":    10,
		"PHONE-7":       50,
		"HEADPHONES-01": 100,
	}
}

// normalize trims whitespace and validates the request. The trimmed values are
// what get compared and stored, so "ord_1" and " ord_1 " are the same order.
func (r ReserveRequest) normalize() (ReserveRequest, error) {
	r.OrderID = strings.TrimSpace(r.OrderID)
	r.SKU = strings.TrimSpace(r.SKU)

	if r.OrderID == "" {
		return r, &ValidationError{Message: "order_id is required"}
	}
	if len(r.OrderID) > MaxOrderLen {
		return r, &ValidationError{Message: fmt.Sprintf("order_id must be at most %d characters", MaxOrderLen)}
	}
	if r.SKU == "" {
		return r, &ValidationError{Message: "sku is required"}
	}
	if len(r.SKU) > MaxSKULen {
		return r, &ValidationError{Message: fmt.Sprintf("sku must be at most %d characters", MaxSKULen)}
	}
	if r.Quantity < MinQuantity || r.Quantity > MaxQuantity {
		return r, &ValidationError{Message: fmt.Sprintf("quantity must be between %d and %d", MinQuantity, MaxQuantity)}
	}
	return r, nil
}

// newReservationID returns an unpredictable, collision-resistant ID such as
// "res_7XQ2K5M3...". rand.Text draws 128 bits from crypto/rand, so two IDs
// colliding is not a realistic concern.
func newReservationID() string {
	return "res_" + rand.Text()
}
