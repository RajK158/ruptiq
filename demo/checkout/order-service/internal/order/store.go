package order

import (
	"errors"
	"sync"
)

// ErrNotFound is returned by Store.Get when no order has the given ID.
var ErrNotFound = errors.New("order not found")

// Store keeps orders in memory. It is safe for concurrent use.
//
// Orders are lost when the process exits. A real database will replace this
// later; keeping it behind Add and Get makes that swap small.
type Store struct {
	mu     sync.RWMutex // guards orders
	orders map[string]Order
}

// NewStore returns an empty Store.
func NewStore() *Store {
	return &Store{orders: make(map[string]Order)}
}

// Add saves o, keyed by its ID.
func (s *Store) Add(o Order) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.orders[o.ID] = o
}

// MarkReserved records that the Inventory Service reserved stock for the order
// and returns the stored order. changed is true only when this call moved the
// order from created to reserved.
//
// The update is a compare-and-set under the store's lock, so it is safe when
// several requests finish at once: the first one wins and later ones change
// nothing, so an order can never move backward or have its reservation ID
// replaced. If the order is already reserved, the stored order is returned
// unchanged, whatever reservationID was passed.
//
// It returns ErrNotFound if the order does not exist.
func (s *Store) MarkReserved(id, reservationID string) (o Order, changed bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[id]
	if !ok {
		return Order{}, false, ErrNotFound
	}
	if o.Status != StatusCreated {
		return o, false, nil
	}
	o.Status = StatusReserved
	o.ReservationID = reservationID
	s.orders[id] = o
	return o, true, nil
}

// Get returns the order with the given ID, or ErrNotFound.
// Order contains no pointers or slices, so the returned copy is independent of
// the stored one.
func (s *Store) Get(id string) (Order, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, ok := s.orders[id]
	if !ok {
		return Order{}, ErrNotFound
	}
	return o, nil
}
