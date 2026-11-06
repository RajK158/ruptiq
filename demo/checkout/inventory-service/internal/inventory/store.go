package inventory

import (
	"slices"
	"strings"
	"sync"
	"time"
)

// stock is the mutable counters for one SKU. Only Store touches it, always
// while holding Store.mu.
type stock struct {
	available int
	reserved  int
}

// Store keeps stock and reservations in memory. It is safe for concurrent use.
//
// A single mutex guards all state. Reserve holds it for the whole sequence of
// "look for an existing reservation, check stock, decrement stock, record the
// reservation", so no other request can interleave between those steps. That
// is what prevents overselling and double decrements.
//
// Everything is lost when the process exits.
type Store struct {
	mu           sync.RWMutex           // guards every field below
	stock        map[string]*stock      // by SKU
	reservations map[string]Reservation // by order ID; doubles as the idempotency record
}

// NewStore returns a Store holding the given starting units per SKU.
// Negative amounts are treated as zero.
func NewStore(initial map[string]int) *Store {
	s := &Store{
		stock:        make(map[string]*stock, len(initial)),
		reservations: make(map[string]Reservation),
	}
	for sku, units := range initial {
		if units < 0 {
			units = 0
		}
		s.stock[sku] = &stock{available: units}
	}
	return s
}

// Get returns the stock for sku, or ErrUnknownSKU.
func (s *Store) Get(sku string) (Item, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.stock[sku]
	if !ok {
		return Item{}, ErrUnknownSKU
	}
	return Item{SKU: sku, Available: st.available, Reserved: st.reserved}, nil
}

// Snapshot returns the stock of every SKU in the catalog, sorted by SKU. All
// items are read under one lock, so they describe the same moment: no
// reservation is ever half-reflected.
func (s *Store) Snapshot() []Item {
	s.mu.RLock()
	items := make([]Item, 0, len(s.stock))
	for sku, st := range s.stock {
		items = append(items, Item{SKU: sku, Available: st.available, Reserved: st.reserved})
	}
	s.mu.RUnlock()
	slices.SortFunc(items, func(a, b Item) int { return strings.Compare(a.SKU, b.SKU) })
	return items
}

// Reserve holds req.Quantity units of req.SKU for req.OrderID.
//
// The order ID is the idempotency key:
//   - First request for an order ID: stock is decremented, the reservation is
//     recorded, and created is true.
//   - Same order ID with the same SKU and quantity: the existing reservation
//     is returned, stock is untouched, and created is false. This holds even
//     if stock has run out since.
//   - Same order ID with a different SKU or quantity: ErrOrderConflict.
//
// Other errors: *ValidationError for a bad request, ErrUnknownSKU, and
// ErrInsufficientStock. On any error, stock and reservations are unchanged.
func (s *Store) Reserve(req ReserveRequest) (res Reservation, created bool, err error) {
	req, err = req.normalize()
	if err != nil {
		return Reservation{}, false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.reservations[req.OrderID]; ok {
		if existing.SKU == req.SKU && existing.Quantity == req.Quantity {
			return existing, false, nil
		}
		return Reservation{}, false, ErrOrderConflict
	}

	st, ok := s.stock[req.SKU]
	if !ok {
		return Reservation{}, false, ErrUnknownSKU
	}
	if st.available < req.Quantity {
		return Reservation{}, false, ErrInsufficientStock
	}

	// Every check has passed; nothing below can fail, so the changes are
	// applied together or not at all.
	st.available -= req.Quantity
	st.reserved += req.Quantity
	res = Reservation{
		ID:        newReservationID(),
		OrderID:   req.OrderID,
		SKU:       req.SKU,
		Quantity:  req.Quantity,
		Status:    StatusReserved,
		CreatedAt: time.Now().UTC(),
	}
	s.reservations[req.OrderID] = res
	return res, true, nil
}
