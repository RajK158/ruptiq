package payment

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// req builds a valid approve request; tests tweak fields as needed.
func req(order, key string) Request {
	return Request{
		OrderID:        order,
		AmountCents:    24999,
		Currency:       "USD",
		PaymentMethod:  MethodApprove,
		IdempotencyKey: key,
	}
}

func declineReq(order, key string) Request {
	r := req(order, key)
	r.PaymentMethod = MethodDecline
	return r
}

// checkInvariants verifies the store's internal indexes agree with each other.
// It returns the number of approved payments per order.
func checkInvariants(t *testing.T, s *Store) map[string]int {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()

	approved := make(map[string]int)
	for id, p := range s.payments {
		if p.ID != id {
			t.Errorf("payment stored under %q has ID %q", id, p.ID)
		}
		if p.Status == StatusApproved {
			approved[p.OrderID]++
		}
	}
	for order, n := range approved {
		if n > 1 {
			t.Errorf("order %q has %d approved payments", order, n)
		}
		id, ok := s.approvedOrders[order]
		if !ok || s.payments[id].OrderID != order || s.payments[id].Status != StatusApproved {
			t.Errorf("approvedOrders index is wrong for order %q", order)
		}
	}
	if len(s.approvedOrders) != len(approved) {
		t.Errorf("approvedOrders has %d entries, but %d orders have approved payments", len(s.approvedOrders), len(approved))
	}
	// Every payment must be reachable through exactly one idempotency key.
	if len(s.attempts) != len(s.payments) {
		t.Errorf("%d idempotency keys for %d payments", len(s.attempts), len(s.payments))
	}
	for key, a := range s.attempts {
		if _, ok := s.payments[a.paymentID]; !ok {
			t.Errorf("idempotency key %q points at missing payment %q", key, a.paymentID)
		}
	}
	return approved
}

func TestCreateApproved(t *testing.T) {
	s := NewStore()
	before := time.Now()
	p, created, err := s.Create(req("ord_1", "key-1"))
	after := time.Now()

	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if !strings.HasPrefix(p.ID, "pay_") || len(p.ID) <= len("pay_") {
		t.Errorf("ID = %q, want pay_ prefix and a random suffix", p.ID)
	}
	if p.OrderID != "ord_1" || p.AmountCents != 24999 || p.Currency != "USD" || p.Status != StatusApproved {
		t.Errorf("payment = %+v", p)
	}
	if p.CreatedAt.Before(before) || p.CreatedAt.After(after) || p.CreatedAt.Location() != time.UTC {
		t.Errorf("CreatedAt = %v, want a UTC time between %v and %v", p.CreatedAt, before, after)
	}
	checkInvariants(t, s)
}

func TestCreateDeclined(t *testing.T) {
	s := NewStore()
	p, created, err := s.Create(declineReq("ord_1", "key-1"))
	if err != nil || !created {
		t.Fatalf("a decline is a normal outcome: created=%v err=%v", created, err)
	}
	if p.Status != StatusDeclined {
		t.Errorf("Status = %q, want %q", p.Status, StatusDeclined)
	}
	if n := checkInvariants(t, s)["ord_1"]; n != 0 {
		t.Errorf("declined payment counted as approved")
	}
}

func TestGet(t *testing.T) {
	s := NewStore()
	created, _, _ := s.Create(req("ord_1", "key-1"))

	got, err := s.Get(created.ID)
	if err != nil || got != created {
		t.Errorf("Get = %+v, %v; want %+v", got, err, created)
	}
	if _, err := s.Get("pay_missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(missing) error = %v, want ErrNotFound", err)
	}
}

func TestValidation(t *testing.T) {
	tests := []struct {
		name   string
		modify func(*Request)
	}{
		{"missing order_id", func(r *Request) { r.OrderID = "" }},
		{"blank order_id", func(r *Request) { r.OrderID = "  \t" }},
		{"long order_id", func(r *Request) { r.OrderID = strings.Repeat("o", MaxOrderIDLen+1) }},
		{"control char in order_id", func(r *Request) { r.OrderID = "ord\n1" }},
		{"missing idempotency_key", func(r *Request) { r.IdempotencyKey = "" }},
		{"blank idempotency_key", func(r *Request) { r.IdempotencyKey = "   " }},
		{"long idempotency_key", func(r *Request) { r.IdempotencyKey = strings.Repeat("k", MaxIdempotencyKeyLen+1) }},
		{"control char in idempotency_key", func(r *Request) { r.IdempotencyKey = "k\x00" }},
		{"zero amount", func(r *Request) { r.AmountCents = 0 }},
		{"negative amount", func(r *Request) { r.AmountCents = -5 }},
		{"amount over limit", func(r *Request) { r.AmountCents = MaxAmountCents + 1 }},
		{"missing currency", func(r *Request) { r.Currency = "" }},
		{"unsupported currency", func(r *Request) { r.Currency = "EUR" }},
		{"lowercase currency", func(r *Request) { r.Currency = "usd" }},
		{"missing payment method", func(r *Request) { r.PaymentMethod = "" }},
		{"unsupported payment method", func(r *Request) { r.PaymentMethod = "credit_card" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewStore()
			r := req("ord_1", "key-1")
			tt.modify(&r)

			_, created, err := s.Create(r)
			var verr *ValidationError
			if !errors.As(err, &verr) || created {
				t.Fatalf("created=%v err=%v, want *ValidationError", created, err)
			}
			// Nothing was recorded, so the same key is still free.
			if _, created, err := s.Create(req("ord_1", "key-1")); err != nil || !created {
				t.Errorf("valid request after invalid one: created=%v err=%v", created, err)
			}
		})
	}
}

func TestValidationBoundaries(t *testing.T) {
	for _, amount := range []int64{MinAmountCents, MaxAmountCents} {
		r := req("ord_1", "key-1")
		r.AmountCents = amount
		if _, _, err := NewStore().Create(r); err != nil {
			t.Errorf("amount %d rejected: %v", amount, err)
		}
	}
	r := req(strings.Repeat("o", MaxOrderIDLen), strings.Repeat("k", MaxIdempotencyKeyLen))
	if _, _, err := NewStore().Create(r); err != nil {
		t.Errorf("longest identifiers rejected: %v", err)
	}
}

func TestIdenticalRetryReturnsOriginal(t *testing.T) {
	s := NewStore()
	first, _, _ := s.Create(req("ord_1", "key-1"))

	for i := 0; i < 5; i++ {
		again, created, err := s.Create(req("ord_1", "key-1"))
		if err != nil || created || again != first {
			t.Fatalf("retry %d: created=%v err=%v payment=%+v, want original %+v", i, created, err, again, first)
		}
	}
	s.mu.RLock()
	n := len(s.payments)
	s.mu.RUnlock()
	if n != 1 {
		t.Errorf("%d payments stored, want 1", n)
	}
}

func TestRetryTreatsWhitespaceAsSameRequest(t *testing.T) {
	s := NewStore()
	first, _, _ := s.Create(req("ord_1", "key-1"))
	r := req(" ord_1 ", " key-1 ")
	r.Currency = " USD "
	again, created, err := s.Create(r)
	if err != nil || created || again != first {
		t.Errorf("created=%v err=%v again=%+v", created, err, again)
	}
}

func TestRetryOfDeclinedReturnsOriginalDecline(t *testing.T) {
	s := NewStore()
	first, _, _ := s.Create(declineReq("ord_1", "key-1"))
	again, created, err := s.Create(declineReq("ord_1", "key-1"))
	if err != nil || created || again != first || again.Status != StatusDeclined {
		t.Errorf("created=%v err=%v again=%+v", created, err, again)
	}
}

func TestConflictingIdempotencyKey(t *testing.T) {
	tests := []struct {
		name   string
		modify func(*Request)
	}{
		{"different order", func(r *Request) { r.OrderID = "ord_other" }},
		{"different amount", func(r *Request) { r.AmountCents++ }},
		{"different payment method", func(r *Request) { r.PaymentMethod = MethodDecline }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewStore()
			first, _, _ := s.Create(req("ord_1", "key-1"))
			r := req("ord_1", "key-1")
			tt.modify(&r)

			if _, created, err := s.Create(r); !errors.Is(err, ErrIdempotencyConflict) || created {
				t.Fatalf("created=%v err=%v, want ErrIdempotencyConflict", created, err)
			}
			// The original is untouched and still retrievable by retry.
			if again, created, err := s.Create(req("ord_1", "key-1")); err != nil || created || again != first {
				t.Errorf("original changed: created=%v err=%v again=%+v", created, err, again)
			}
			checkInvariants(t, s)
		})
	}
}

func TestConflictingKeyIsReportedBeforeOrderAlreadyPaid(t *testing.T) {
	// The key check runs first: reusing key-1 with a different amount is an
	// idempotency conflict even though the order is also paid.
	s := NewStore()
	s.Create(req("ord_1", "key-1"))
	r := req("ord_1", "key-1")
	r.AmountCents = 1
	if _, _, err := s.Create(r); !errors.Is(err, ErrIdempotencyConflict) {
		t.Errorf("err = %v, want ErrIdempotencyConflict", err)
	}
}

func TestSecondApprovalForSameOrderRejected(t *testing.T) {
	s := NewStore()
	first, _, _ := s.Create(req("ord_1", "key-1"))

	_, created, err := s.Create(req("ord_1", "key-2"))
	if !errors.Is(err, ErrOrderAlreadyPaid) || created {
		t.Fatalf("created=%v err=%v, want ErrOrderAlreadyPaid", created, err)
	}
	// A decline attempt on a paid order is rejected too.
	if _, _, err := s.Create(declineReq("ord_1", "key-3")); !errors.Is(err, ErrOrderAlreadyPaid) {
		t.Errorf("decline on paid order: err = %v, want ErrOrderAlreadyPaid", err)
	}
	// Rejected attempts are not recorded.
	s.mu.RLock()
	n := len(s.payments)
	s.mu.RUnlock()
	if n != 1 {
		t.Errorf("%d payments stored, want 1", n)
	}
	if got, _ := s.Get(first.ID); got != first {
		t.Errorf("original payment changed: %+v", got)
	}
	checkInvariants(t, s)
}

func TestRetryStillWorksAfterOrderIsPaid(t *testing.T) {
	s := NewStore()
	first, _, _ := s.Create(req("ord_1", "key-1"))
	s.Create(req("ord_1", "key-2")) // rejected

	again, created, err := s.Create(req("ord_1", "key-1"))
	if err != nil || created || again != first {
		t.Errorf("legitimate retry blocked: created=%v err=%v again=%+v", created, err, again)
	}
}

func TestRejectedKeyCanBeReusedForADifferentOrder(t *testing.T) {
	// A rejected attempt records nothing, so its key is still free.
	s := NewStore()
	s.Create(req("ord_1", "key-1"))
	if _, _, err := s.Create(req("ord_1", "key-2")); !errors.Is(err, ErrOrderAlreadyPaid) {
		t.Fatal(err)
	}
	if _, created, err := s.Create(req("ord_2", "key-2")); err != nil || !created {
		t.Errorf("created=%v err=%v", created, err)
	}
}

func TestDeclineThenApproveWithNewKey(t *testing.T) {
	s := NewStore()
	declined, _, _ := s.Create(declineReq("ord_1", "key-1"))
	approved, created, err := s.Create(req("ord_1", "key-2"))
	if err != nil || !created {
		t.Fatalf("approval after decline: created=%v err=%v", created, err)
	}
	if declined.Status != StatusDeclined || approved.Status != StatusApproved || declined.ID == approved.ID {
		t.Errorf("declined=%+v approved=%+v", declined, approved)
	}
	// And now the order is paid.
	if _, _, err := s.Create(req("ord_1", "key-3")); !errors.Is(err, ErrOrderAlreadyPaid) {
		t.Errorf("err = %v, want ErrOrderAlreadyPaid", err)
	}
	checkInvariants(t, s)
}

func TestMultipleDeclinesAllowed(t *testing.T) {
	s := NewStore()
	for i := 0; i < 3; i++ {
		if _, created, err := s.Create(declineReq("ord_1", fmt.Sprintf("key-%d", i))); err != nil || !created {
			t.Fatalf("decline %d: created=%v err=%v", i, created, err)
		}
	}
}

func TestDifferentOrdersCanEachBeApproved(t *testing.T) {
	s := NewStore()
	for i := 0; i < 3; i++ {
		if _, created, err := s.Create(req(fmt.Sprintf("ord_%d", i), fmt.Sprintf("key-%d", i))); err != nil || !created {
			t.Fatalf("order %d: created=%v err=%v", i, created, err)
		}
	}
	checkInvariants(t, s)
}

func TestPaymentIDsAreUnique(t *testing.T) {
	s := NewStore()
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		p, _, err := s.Create(req(fmt.Sprintf("ord_%d", i), fmt.Sprintf("key-%d", i)))
		if err != nil {
			t.Fatal(err)
		}
		if seen[p.ID] {
			t.Fatalf("duplicate payment ID %q", p.ID)
		}
		seen[p.ID] = true
	}
}

// startTogether runs fn(i) for i in [0, n) in separate goroutines, releasing
// them all at the same moment to maximise contention.
func startTogether(n int, fn func(i int)) {
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			fn(i)
		}()
	}
	close(start)
	wg.Wait()
}

func TestConcurrentIdenticalRetriesCreateOneApprovedPayment(t *testing.T) {
	const attempts = 200
	s := NewStore()

	var createdCount, replayed, failed atomic.Int64
	ids := make(chan Payment, attempts)
	startTogether(attempts, func(int) {
		p, created, err := s.Create(req("ord_1", "key-same"))
		switch {
		case err != nil:
			failed.Add(1)
		case created:
			createdCount.Add(1)
			ids <- p
		default:
			replayed.Add(1)
			ids <- p
		}
	})
	close(ids)

	if createdCount.Load() != 1 || replayed.Load() != attempts-1 || failed.Load() != 0 {
		t.Errorf("created=%d replayed=%d failed=%d, want 1/%d/0", createdCount.Load(), replayed.Load(), failed.Load(), attempts-1)
	}
	var first Payment
	for p := range ids {
		if first.ID == "" {
			first = p
		}
		if p != first {
			t.Fatalf("callers saw different results: %+v and %+v", first, p)
		}
	}
	s.mu.RLock()
	n := len(s.payments)
	s.mu.RUnlock()
	if n != 1 {
		t.Errorf("%d payment records, want exactly 1", n)
	}
	checkInvariants(t, s)
}

func TestConcurrentIdenticalDeclinedRetriesCreateOneRecord(t *testing.T) {
	s := NewStore()
	var createdCount atomic.Int64
	startTogether(100, func(int) {
		if _, created, err := s.Create(declineReq("ord_1", "key-same")); err == nil && created {
			createdCount.Add(1)
		}
	})
	s.mu.RLock()
	n := len(s.payments)
	s.mu.RUnlock()
	if createdCount.Load() != 1 || n != 1 {
		t.Errorf("created=%d records=%d, want 1/1", createdCount.Load(), n)
	}
}

func TestConcurrentApprovalsForSameOrderOnlyOneWins(t *testing.T) {
	const attempts = 200
	s := NewStore()

	var approved, alreadyPaid, other atomic.Int64
	startTogether(attempts, func(i int) {
		// Distinct idempotency keys, same order.
		p, created, err := s.Create(req("ord_1", fmt.Sprintf("key-%d", i)))
		switch {
		case err == nil && created && p.Status == StatusApproved:
			approved.Add(1)
		case errors.Is(err, ErrOrderAlreadyPaid):
			alreadyPaid.Add(1)
		default:
			other.Add(1)
		}
	})

	if approved.Load() != 1 || alreadyPaid.Load() != attempts-1 || other.Load() != 0 {
		t.Errorf("approved=%d alreadyPaid=%d other=%d, want 1/%d/0", approved.Load(), alreadyPaid.Load(), other.Load(), attempts-1)
	}
	if n := checkInvariants(t, s)["ord_1"]; n != 1 {
		t.Errorf("ord_1 has %d approved payments, want exactly 1", n)
	}
	s.mu.RLock()
	n := len(s.payments)
	s.mu.RUnlock()
	if n != 1 {
		t.Errorf("%d payment records, want 1 (rejected attempts must not be recorded)", n)
	}
}

func TestConcurrentMixedWorkloadKeepsInvariants(t *testing.T) {
	// 10 orders; each gets approve attempts, decline attempts and retries,
	// all at once. Every order must end with at most one approval.
	const orders = 10
	s := NewStore()
	startTogether(orders*30, func(i int) {
		key := fmt.Sprintf("key-%d", i)
		switch i % 3 {
		case 0:
			s.Create(req(fmt.Sprintf("ord_%d", i%orders), key))
		case 1:
			s.Create(declineReq(fmt.Sprintf("ord_%d", i%orders), key))
		default:
			// Two identical retries of the approve attempt i-2 (same order,
			// same key, same details), racing with that attempt itself.
			j := i - 2
			s.Create(req(fmt.Sprintf("ord_%d", j%orders), fmt.Sprintf("key-%d", j)))
			s.Create(req(fmt.Sprintf("ord_%d", j%orders), fmt.Sprintf("key-%d", j)))
		}
	})
	approved := checkInvariants(t, s)
	for order, n := range approved {
		if n > 1 {
			t.Errorf("order %s has %d approvals", order, n)
		}
	}
}

func TestConcurrentReadsAndWrites(t *testing.T) {
	// Writers create payments while readers fetch them; mainly useful under
	// `go test -race`.
	s := NewStore()
	ids := make(chan string, 100)
	startTogether(200, func(i int) {
		if i%2 == 0 {
			p, _, err := s.Create(req(fmt.Sprintf("ord_%d", i), fmt.Sprintf("key-%d", i)))
			if err != nil {
				t.Errorf("create: %v", err)
				return
			}
			ids <- p.ID
			return
		}
		select {
		case id := <-ids:
			if _, err := s.Get(id); err != nil {
				t.Errorf("Get(%q) = %v", id, err)
			}
		default:
			if _, err := s.Get("pay_missing"); !errors.Is(err, ErrNotFound) {
				t.Errorf("Get(missing) = %v", err)
			}
		}
	})
	checkInvariants(t, s)
}
