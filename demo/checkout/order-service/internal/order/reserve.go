package order

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

// Errors describing why an inventory reservation did not happen. The first
// three are business answers from the Inventory Service; the rest mean the
// answer could not be obtained or understood. Use errors.Is to test for them.
var (
	// ErrInsufficientStock: the Inventory Service has too few units.
	ErrInsufficientStock = errors.New("insufficient stock")
	// ErrUnknownSKU: the Inventory Service does not stock this SKU.
	ErrUnknownSKU = errors.New("unknown sku")
	// ErrInventoryConflict: the Inventory Service already holds a reservation
	// for this order ID with different details.
	ErrInventoryConflict = errors.New("inventory already has a reservation for this order with different details")

	// ErrInventoryUnavailable: the Inventory Service could not be reached, or
	// answered with a server error.
	ErrInventoryUnavailable = errors.New("inventory service unavailable")
	// ErrInventoryTimeout: the Inventory Service did not answer in time. The
	// reservation may or may not have been made.
	ErrInventoryTimeout = errors.New("inventory service timed out")
	// ErrInventoryBadResponse: the Inventory Service answered, but not with
	// anything this service understands.
	ErrInventoryBadResponse = errors.New("unexpected response from inventory service")
	// ErrInventoryNotConfigured: no Inventory Service client was provided.
	ErrInventoryNotConfigured = errors.New("inventory service is not configured")
)

// ReserveRequest is what Order Service asks the Inventory Service to hold.
type ReserveRequest struct {
	// OrderID is also the inventory idempotency key: asking twice for the same
	// order never reserves stock twice.
	OrderID  string
	SKU      string
	Quantity int
}

// InventoryReserver is the one thing the Order Service needs from the
// Inventory Service: reserve stock and return the reservation ID.
//
// It is defined here, where it is used, so the business logic does not depend
// on HTTP. The real implementation (package inventoryclient) talks HTTP/JSON;
// tests use fakes.
//
// Implementations must be safe for concurrent use and must return an error
// matching one of the Err* values above on failure. A nil error means the
// Inventory Service confirmed the reservation.
type InventoryReserver interface {
	Reserve(ctx context.Context, req ReserveRequest) (reservationID string, err error)
}

// Service holds the order use cases that involve other services.
type Service struct {
	store     *Store
	inventory InventoryReserver
}

// NewService returns a Service. inventory may be nil, in which case Reserve
// fails with ErrInventoryNotConfigured.
func NewService(store *Store, inventory InventoryReserver) *Service {
	return &Service{store: store, inventory: inventory}
}

// Reserve asks the Inventory Service to reserve stock for the order, and on
// confirmation marks the order reserved. It returns the updated order.
//
// Safety rules:
//   - The order is only marked reserved after the Inventory Service
//     explicitly confirmed. Any error, including an ambiguous one such as a
//     timeout, leaves the order as it was.
//   - The order ID is the idempotency key, so retrying after any failure is
//     safe: the Inventory Service returns the original reservation instead of
//     reserving again. No retry happens automatically.
//   - An order that is already reserved is returned as is, without calling
//     the Inventory Service again.
//   - No lock is held during the network call. Several concurrent requests
//     for one order may all call the Inventory Service; it deduplicates them,
//     and the store's compare-and-set lets exactly one of them record the
//     result.
//
// It returns ErrNotFound if the order does not exist.
func (s *Service) Reserve(ctx context.Context, orderID string) (Order, error) {
	o, err := s.store.Get(orderID)
	if err != nil {
		return Order{}, err
	}
	if o.Status != StatusCreated {
		// Reserved, or already further along (paid): reservation is done.
		return o, nil
	}
	if s.inventory == nil {
		return Order{}, ErrInventoryNotConfigured
	}

	reservationID, err := s.inventory.Reserve(ctx, ReserveRequest{
		OrderID:  o.ID,
		SKU:      o.SKU,
		Quantity: o.Quantity,
	})
	if err != nil {
		return Order{}, fmt.Errorf("reserving inventory for %s: %w", o.ID, err)
	}

	updated, changed, err := s.store.MarkReserved(o.ID, reservationID)
	if err != nil {
		return Order{}, err
	}
	if !changed && updated.ReservationID != reservationID {
		// Another request recorded a different reservation ID first. With a
		// working Inventory Service this cannot happen (same order ID, same
		// reservation). It could if Inventory restarted and lost its state in
		// between. Keep the first one, but make the anomaly visible.
		slog.Warn("order already reserved under a different reservation ID",
			"order_id", o.ID, "recorded", updated.ReservationID, "received", reservationID)
	}
	return updated, nil
}
