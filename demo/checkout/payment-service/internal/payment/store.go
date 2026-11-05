package payment

import (
	"sync"
	"time"
)

// attempt remembers what a payment attempt asked for, so a later request using
// the same idempotency key can be compared against it.
type attempt struct {
	paymentID string
	request   Request // normalized; compared as a whole
}

// Store keeps payment attempts in memory. It is safe for concurrent use.
//
// One mutex guards all four maps. Create holds it for the whole sequence of
// "check the idempotency key, check the order's approval, create the payment,
// update every index", so no other request can interleave. The maps therefore
// always agree with each other.
//
// Guarantees hold only inside ONE running process. Everything is lost on a
// crash or restart, and two replicas would each have their own, independent
// state, so they could each approve the same order.
type Store struct {
	mu             sync.RWMutex       // guards every field below
	payments       map[string]Payment // by payment ID
	attempts       map[string]attempt // by idempotency key
	approvedOrders map[string]string  // order ID -> ID of its approved payment
}

// NewStore returns an empty Store.
func NewStore() *Store {
	return &Store{
		payments:       make(map[string]Payment),
		attempts:       make(map[string]attempt),
		approvedOrders: make(map[string]string),
	}
}

// Get returns the payment with the given ID, or ErrNotFound.
func (s *Store) Get(id string) (Payment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.payments[id]
	if !ok {
		return Payment{}, ErrNotFound
	}
	return p, nil
}

// Create records a simulated payment attempt.
//
// Rules, applied in this order:
//  1. If the idempotency key was used before with identical parameters, the
//     original payment is returned and created is false. Nothing changes.
//     This runs first so legitimate retries still work after the order has
//     been paid.
//  2. If the key was used before with different parameters, the result is
//     ErrIdempotencyConflict.
//  3. If the order already has an approved payment, the result is
//     ErrOrderAlreadyPaid, whatever the new attempt's payment method.
//  4. Otherwise a new payment is created with the outcome the payment method
//     selects (approved or declined) and created is true. An approval marks
//     the order as paid; a decline does not, so the order can be retried with
//     a new key.
//
// A *ValidationError is returned for a malformed request. On any error the
// store is unchanged, and rejected attempts are not recorded.
func (s *Store) Create(req Request) (p Payment, created bool, err error) {
	req, err = req.normalize()
	if err != nil {
		return Payment{}, false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if prev, ok := s.attempts[req.IdempotencyKey]; ok {
		if prev.request == req {
			return s.payments[prev.paymentID], false, nil
		}
		return Payment{}, false, ErrIdempotencyConflict
	}

	// Any new attempt on an already-paid order is rejected, approve or decline.
	if _, paid := s.approvedOrders[req.OrderID]; paid {
		return Payment{}, false, ErrOrderAlreadyPaid
	}

	status := outcome(req.PaymentMethod)
	p = Payment{
		ID:          newPaymentID(),
		OrderID:     req.OrderID,
		AmountCents: req.AmountCents,
		Currency:    req.Currency,
		Status:      status,
		CreatedAt:   time.Now().UTC(),
	}
	// All checks have passed; update every index together.
	s.payments[p.ID] = p
	s.attempts[req.IdempotencyKey] = attempt{paymentID: p.ID, request: req}
	if status == StatusApproved {
		s.approvedOrders[req.OrderID] = p.ID
	}
	return p, true, nil
}
