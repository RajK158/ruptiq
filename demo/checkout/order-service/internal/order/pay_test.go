package order

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// fakePayments is an in-memory PaymentProcessor that behaves like the Payment
// Service: global idempotency keys, at most one approval per order, and the
// outcome selected by the payment method.
type fakePayments struct {
	mu       sync.Mutex
	calls    int
	byKey    map[string]fakePayment
	approved map[string]string // order ID -> payment ID
	err      error             // if set, every call fails with it
	nextID   int
}

type fakePayment struct {
	req PaymentRequest
	res PaymentResult
}

func (f *fakePayments) Pay(_ context.Context, req PaymentRequest) (PaymentResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return PaymentResult{}, f.err
	}
	if f.byKey == nil {
		f.byKey = make(map[string]fakePayment)
		f.approved = make(map[string]string)
	}
	if prev, ok := f.byKey[req.IdempotencyKey]; ok {
		if prev.req == req {
			return prev.res, nil
		}
		return PaymentResult{}, ErrPaymentKeyConflict
	}
	if _, paid := f.approved[req.OrderID]; paid {
		return PaymentResult{}, ErrPaymentApprovedElsewhere
	}
	f.nextID++
	status := PaymentApproved
	if req.PaymentMethod == MethodDecline {
		status = PaymentDeclined
	}
	res := PaymentResult{ID: fmt.Sprintf("pay_%d", f.nextID), Status: status, AmountCents: req.AmountCents, Currency: req.Currency}
	f.byKey[req.IdempotencyKey] = fakePayment{req: req, res: res}
	if status == PaymentApproved {
		f.approved[req.OrderID] = res.ID
	}
	return res, nil
}

func (f *fakePayments) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakePayments) approvals() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.byKey {
		if p.res.Status == PaymentApproved {
			n++
		}
	}
	return n
}

func (f *fakePayments) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// reservedOrder stores an order for 2 laptops already marked reserved.
func reservedOrder(t *testing.T, s *Store) Order {
	t.Helper()
	o := newOrder(t, s)
	r, _, err := s.MarkReserved(o.ID, "res_1")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

const laptopX2 = 2 * 129_999

func TestAmountCents(t *testing.T) {
	if got, err := AmountCents("LAPTOP-001", 2); err != nil || got != laptopX2 {
		t.Errorf("AmountCents(LAPTOP-001, 2) = %d, %v; want %d", got, err, laptopX2)
	}
	for sku, price := range map[string]int64{"LAPTOP-001": 129_999, "PHONE-7": 79_999, "HEADPHONES-01": 14_999} {
		if got, err := AmountCents(sku, 1); err != nil || got != price {
			t.Errorf("AmountCents(%s, 1) = %d, %v; want %d", sku, got, err, price)
		}
	}
	if _, err := AmountCents("TABLET-9", 1); !errors.Is(err, ErrNoPrice) {
		t.Errorf("unpriced sku: err = %v, want ErrNoPrice", err)
	}
	if _, err := AmountCents("LAPTOP-001", 0); err == nil {
		t.Error("zero quantity: want error")
	}
}

func TestMultiplyCentsOverflow(t *testing.T) {
	if _, err := multiplyCents(math.MaxInt64/2+1, 2); err == nil {
		t.Error("overflow: want error")
	}
	if got, err := multiplyCents(math.MaxInt64/2, 2); err != nil || got != math.MaxInt64-1 {
		t.Errorf("largest product = %d, %v", got, err)
	}
	if _, err := multiplyCents(-1, 2); err == nil {
		t.Error("negative price: want error")
	}
}

func TestPayApproved(t *testing.T) {
	store := NewStore()
	o := reservedOrder(t, store)
	pay := &fakePayments{}

	res, err := NewPayer(store, pay).Pay(context.Background(), o.ID, MethodApprove, "attempt-1")
	if err != nil {
		t.Fatalf("Pay: %v", err)
	}
	if res.Payment != (PaymentResult{ID: "pay_1", Status: PaymentApproved, AmountCents: laptopX2, Currency: "USD"}) {
		t.Errorf("payment = %+v", res.Payment)
	}
	want := o
	want.Status, want.PaymentID, want.PaymentIdempotencyKey = StatusPaid, "pay_1", "attempt-1"
	if res.Order != want {
		t.Errorf("order = %+v, want %+v", res.Order, want)
	}
	if stored, _ := store.Get(o.ID); stored != want {
		t.Errorf("stored = %+v, want %+v", stored, want)
	}
	// The upstream request used the computed amount and an order-scoped key.
	pay.mu.Lock()
	p := pay.byKey[o.ID+":attempt-1"]
	pay.mu.Unlock()
	if p.req != (PaymentRequest{OrderID: o.ID, AmountCents: laptopX2, Currency: "USD", PaymentMethod: MethodApprove, IdempotencyKey: o.ID + ":attempt-1"}) {
		t.Errorf("upstream request = %+v", p.req)
	}
}

func TestPayDeclinedKeepsOrderReserved(t *testing.T) {
	store := NewStore()
	o := reservedOrder(t, store)
	res, err := NewPayer(store, &fakePayments{}).Pay(context.Background(), o.ID, MethodDecline, "attempt-1")
	if err != nil {
		t.Fatalf("decline must not be an error: %v", err)
	}
	if res.Payment.Status != PaymentDeclined || res.Order != o {
		t.Errorf("result = %+v; want declined payment and unchanged reserved order", res)
	}
	if stored, _ := store.Get(o.ID); stored != o {
		t.Errorf("stored order changed: %+v", stored)
	}
}

func TestPayDeclineThenApproveWithNewKey(t *testing.T) {
	store := NewStore()
	o := reservedOrder(t, store)
	payer := NewPayer(store, &fakePayments{})

	if res, _ := payer.Pay(context.Background(), o.ID, MethodDecline, "attempt-1"); res.Payment.Status != PaymentDeclined {
		t.Fatalf("first attempt = %+v", res)
	}
	// Replaying the declined key replays the decline.
	if res, err := payer.Pay(context.Background(), o.ID, MethodDecline, "attempt-1"); err != nil || res.Payment.Status != PaymentDeclined || res.Payment.ID != "pay_1" {
		t.Errorf("decline replay = %+v, %v", res, err)
	}
	// Reusing the declined key to approve is a conflict.
	if _, err := payer.Pay(context.Background(), o.ID, MethodApprove, "attempt-1"); !errors.Is(err, ErrPaymentKeyConflict) {
		t.Errorf("declined key reused for approval: err = %v, want ErrPaymentKeyConflict", err)
	}
	// A new key can approve.
	res, err := payer.Pay(context.Background(), o.ID, MethodApprove, "attempt-2")
	if err != nil || res.Payment.Status != PaymentApproved || res.Order.Status != StatusPaid || res.Order.PaymentID != res.Payment.ID {
		t.Errorf("approval with new key = %+v, %v", res, err)
	}
}

func TestPayPaidOrderIsAnsweredLocally(t *testing.T) {
	store := NewStore()
	o := reservedOrder(t, store)
	pay := &fakePayments{}
	payer := NewPayer(store, pay)
	first, _ := payer.Pay(context.Background(), o.ID, MethodApprove, "attempt-1")
	calls := pay.callCount()

	// Same key and method: replay of the recorded approval.
	again, err := payer.Pay(context.Background(), o.ID, MethodApprove, " attempt-1 ")
	if err != nil || again != first {
		t.Errorf("replay = %+v, %v; want %+v", again, err, first)
	}
	// Same key, other method: conflict.
	if _, err := payer.Pay(context.Background(), o.ID, MethodDecline, "attempt-1"); !errors.Is(err, ErrPaymentKeyConflict) {
		t.Errorf("same key other method: err = %v, want ErrPaymentKeyConflict", err)
	}
	// Any other key, approve or decline: already paid.
	for _, method := range []string{MethodApprove, MethodDecline} {
		if _, err := payer.Pay(context.Background(), o.ID, method, "attempt-2"); !errors.Is(err, ErrOrderAlreadyPaid) {
			t.Errorf("%s with new key: err = %v, want ErrOrderAlreadyPaid", method, err)
		}
	}
	if pay.callCount() != calls {
		t.Errorf("payment service called %d more times for a paid order", pay.callCount()-calls)
	}
	if pay.approvals() != 1 {
		t.Errorf("approvals = %d, want 1", pay.approvals())
	}
}

func TestPayRejectsBeforeCallingPaymentService(t *testing.T) {
	store := NewStore()
	created := newOrder(t, store)
	reserved := reservedOrder(t, store)
	unpriced, _ := New("TABLET-9", 1)
	store.Add(unpriced)
	store.MarkReserved(unpriced.ID, "res_x")

	tests := []struct {
		name    string
		orderID string
		method  string
		key     string
		wantErr error
	}{
		{"unknown order", "ord_missing", MethodApprove, "k", ErrNotFound},
		{"not reserved", created.ID, MethodApprove, "k", ErrOrderNotReserved},
		{"no price", unpriced.ID, MethodApprove, "k", ErrNoPrice},
	}
	pay := &fakePayments{}
	payer := NewPayer(store, pay)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := payer.Pay(context.Background(), tt.orderID, tt.method, tt.key); !errors.Is(err, tt.wantErr) {
				t.Errorf("err = %v, want %v", err, tt.wantErr)
			}
		})
	}

	invalid := []struct{ name, method, key string }{
		{"missing method", "", "k"},
		{"unknown method", "credit_card", "k"},
		{"missing key", MethodApprove, ""},
		{"blank key", MethodApprove, "   "},
		{"long key", MethodApprove, strings.Repeat("k", MaxPaymentKeyLen+1)},
		{"control char in key", MethodApprove, "k\n1"},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			var verr *ValidationError
			if _, err := payer.Pay(context.Background(), reserved.ID, tt.method, tt.key); !errors.As(err, &verr) {
				t.Errorf("err = %v, want *ValidationError", err)
			}
		})
	}
	if pay.callCount() != 0 {
		t.Errorf("payment service called %d times for rejected requests", pay.callCount())
	}
}

// storeReservedWithID stores a reserved order with a chosen ID, to reach
// combined-key lengths that generated IDs (always 30 bytes) cannot.
func storeReservedWithID(s *Store, id string) {
	s.Add(Order{ID: id, SKU: "LAPTOP-001", Quantity: 1, Status: StatusReserved, ReservationID: "res_1"})
}

func TestPayUpstreamKeyLengthBoundary(t *testing.T) {
	key := strings.Repeat("k", MaxPaymentKeyLen)
	// Order ID length such that "<id>:<key>" is exactly MaxUpstreamKeyLen.
	fitLen := MaxUpstreamKeyLen - 1 - len(key)
	fits := "ord_" + strings.Repeat("A", fitLen-len("ord_"))
	tooLong := fits + "A"

	store := NewStore()
	storeReservedWithID(store, fits)
	storeReservedWithID(store, tooLong)
	pay := &fakePayments{}
	payer := NewPayer(store, pay)

	var verr *ValidationError
	if _, err := payer.Pay(context.Background(), tooLong, MethodApprove, key); !errors.As(err, &verr) {
		t.Fatalf("combined key of %d bytes: err = %v, want *ValidationError", MaxUpstreamKeyLen+1, err)
	}
	if pay.callCount() != 0 {
		t.Fatalf("payment service called for an over-long key")
	}
	if got, _ := store.Get(tooLong); got.Status != StatusReserved {
		t.Errorf("order changed after rejected key: %+v", got)
	}

	res, err := payer.Pay(context.Background(), fits, MethodApprove, key)
	if err != nil || res.Order.Status != StatusPaid {
		t.Fatalf("combined key of exactly %d bytes: %+v, %v", MaxUpstreamKeyLen, res, err)
	}
	pay.mu.Lock()
	defer pay.mu.Unlock()
	for k := range pay.byKey {
		if len(k) != MaxUpstreamKeyLen {
			t.Errorf("upstream key is %d bytes, want %d", len(k), MaxUpstreamKeyLen)
		}
	}
}

func TestPayClientKeyLengthIsBytes(t *testing.T) {
	tests := []struct {
		name string
		key  string
		ok   bool
	}{
		{"64 ASCII bytes", strings.Repeat("k", 64), true},
		{"65 ASCII bytes", strings.Repeat("k", 65), false},
		{"32 two-byte runes (64 bytes)", strings.Repeat("é", 32), true},
		{"32 two-byte runes plus 1 (65 bytes)", strings.Repeat("é", 32) + "k", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := NewStore()
			o := reservedOrder(t, store)
			if len(o.ID) != 30 {
				t.Fatalf("generated order ID is %d bytes; the key limits assume 30", len(o.ID))
			}
			pay := &fakePayments{}
			_, err := NewPayer(store, pay).Pay(context.Background(), o.ID, MethodApprove, tt.key)
			if tt.ok {
				if err != nil {
					t.Fatalf("err = %v, want success", err)
				}
				if want := len(o.ID) + 1 + len(tt.key); want > MaxUpstreamKeyLen {
					t.Errorf("accepted key makes a %d-byte upstream key", want)
				}
				return
			}
			var verr *ValidationError
			if !errors.As(err, &verr) || pay.callCount() != 0 {
				t.Errorf("err = %v, calls = %d; want *ValidationError and no call", err, pay.callCount())
			}
		})
	}
}

func TestPayNotConfigured(t *testing.T) {
	store := NewStore()
	o := reservedOrder(t, store)
	if _, err := NewPayer(store, nil).Pay(context.Background(), o.ID, MethodApprove, "k"); !errors.Is(err, ErrPaymentNotConfigured) {
		t.Errorf("err = %v, want ErrPaymentNotConfigured", err)
	}
}

func TestPayFailureLeavesOrderUnchanged(t *testing.T) {
	for _, upstream := range []error{ErrPaymentUnavailable, ErrPaymentTimeout, ErrPaymentBadResponse, ErrPaymentKeyConflict, ErrPaymentApprovedElsewhere} {
		t.Run(upstream.Error(), func(t *testing.T) {
			store := NewStore()
			o := reservedOrder(t, store)
			_, err := NewPayer(store, &fakePayments{err: upstream}).Pay(context.Background(), o.ID, MethodApprove, "k")
			if !errors.Is(err, upstream) {
				t.Errorf("err = %v, want %v", err, upstream)
			}
			if stored, _ := store.Get(o.ID); stored != o {
				t.Errorf("order changed after failure: %+v", stored)
			}
		})
	}
}

func TestPayRecoversFromApprovalWhoseResponseWasLost(t *testing.T) {
	// The Payment Service records an approval but its answer never arrives.
	store := NewStore()
	o := reservedOrder(t, store)
	pay := &fakePayments{}
	payer := NewPayer(store, pay)
	if _, err := pay.Pay(context.Background(), PaymentRequest{
		OrderID: o.ID, AmountCents: laptopX2, Currency: "USD", PaymentMethod: MethodApprove, IdempotencyKey: o.ID + ":attempt-1",
	}); err != nil {
		t.Fatal(err)
	}

	// A new key cannot approve again, and the order is not marked paid
	// without evidence; the error says how to recover.
	if _, err := payer.Pay(context.Background(), o.ID, MethodApprove, "attempt-2"); !errors.Is(err, ErrPaymentApprovedElsewhere) {
		t.Fatalf("new key: err = %v, want ErrPaymentApprovedElsewhere", err)
	}
	if stored, _ := store.Get(o.ID); stored.Status != StatusReserved {
		t.Fatalf("order = %+v, want still reserved", stored)
	}
	// Retrying with the original key recovers the approval.
	res, err := payer.Pay(context.Background(), o.ID, MethodApprove, "attempt-1")
	if err != nil || res.Order.Status != StatusPaid || res.Order.PaymentID != "pay_1" {
		t.Errorf("recovery = %+v, %v", res, err)
	}
	if pay.approvals() != 1 {
		t.Errorf("approvals = %d, want 1", pay.approvals())
	}
}

// racingPayments records a different approval for the order while the call
// is in flight, then approves the request too: what a Payment Service that
// lost its state could do.
type racingPayments struct{ store *Store }

func (r racingPayments) Pay(_ context.Context, req PaymentRequest) (PaymentResult, error) {
	r.store.MarkPaid(req.OrderID, "pay_other", "other-key")
	return PaymentResult{ID: "pay_new", Status: PaymentApproved, AmountCents: req.AmountCents, Currency: req.Currency}, nil
}

func TestPayRecordConflictKeepsRecordedPayment(t *testing.T) {
	store := NewStore()
	o := reservedOrder(t, store)
	_, err := NewPayer(store, racingPayments{store}).Pay(context.Background(), o.ID, MethodApprove, "attempt-1")
	if !errors.Is(err, ErrPaymentRecordConflict) {
		t.Errorf("err = %v, want ErrPaymentRecordConflict", err)
	}
	if stored, _ := store.Get(o.ID); stored.PaymentID != "pay_other" {
		t.Errorf("recorded payment = %q, want pay_other kept", stored.PaymentID)
	}
}

func TestPayConcurrentDistinctKeys(t *testing.T) {
	store := NewStore()
	o := reservedOrder(t, store)
	pay := &fakePayments{}
	payer := NewPayer(store, pay)

	const workers = 100
	var approved, alreadyPaid, other atomic.Int64
	run(workers, func(i int) {
		res, err := payer.Pay(context.Background(), o.ID, MethodApprove, fmt.Sprintf("attempt-%d", i))
		switch {
		case err == nil && res.Payment.Status == PaymentApproved && res.Order.PaymentIdempotencyKey == fmt.Sprintf("attempt-%d", i):
			approved.Add(1)
		case errors.Is(err, ErrOrderAlreadyPaid), errors.Is(err, ErrPaymentApprovedElsewhere):
			alreadyPaid.Add(1)
		default:
			other.Add(1)
		}
	})
	if approved.Load() != 1 || alreadyPaid.Load() != workers-1 || other.Load() != 0 {
		t.Errorf("approved=%d alreadyPaid=%d other=%d, want 1/%d/0", approved.Load(), alreadyPaid.Load(), other.Load(), workers-1)
	}
	if pay.approvals() != 1 {
		t.Errorf("payment service approvals = %d, want 1", pay.approvals())
	}
	if stored, _ := store.Get(o.ID); stored.Status != StatusPaid || stored.PaymentID != "pay_1" {
		t.Errorf("stored = %+v", stored)
	}
}

func TestPayConcurrentIdenticalRetries(t *testing.T) {
	store := NewStore()
	o := reservedOrder(t, store)
	pay := &fakePayments{}
	payer := NewPayer(store, pay)

	const workers = 100
	results := make([]PayResult, workers)
	var failures atomic.Int64
	run(workers, func(i int) {
		res, err := payer.Pay(context.Background(), o.ID, MethodApprove, "attempt-1")
		if err != nil {
			failures.Add(1)
			return
		}
		results[i] = res
	})
	if failures.Load() != 0 {
		t.Fatalf("%d identical retries failed", failures.Load())
	}
	for i, r := range results {
		if r != results[0] || r.Order.Status != StatusPaid || r.Payment.ID != "pay_1" {
			t.Fatalf("result %d = %+v, want identical paid result with pay_1", i, r)
		}
	}
	if pay.approvals() != 1 {
		t.Errorf("approvals = %d, want 1", pay.approvals())
	}
}

func TestMarkPaid(t *testing.T) {
	s := NewStore()
	created := newOrder(t, s)
	if _, _, err := s.MarkPaid(created.ID, "pay_1", "k"); !errors.Is(err, ErrOrderNotReserved) {
		t.Errorf("created order: err = %v, want ErrOrderNotReserved", err)
	}
	if _, _, err := s.MarkPaid("ord_missing", "pay_1", "k"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing order: err = %v, want ErrNotFound", err)
	}

	o := reservedOrder(t, s)
	paid, changed, err := s.MarkPaid(o.ID, "pay_1", "k1")
	if err != nil || !changed || paid.Status != StatusPaid || paid.PaymentID != "pay_1" || paid.PaymentIdempotencyKey != "k1" || paid.ReservationID != "res_1" {
		t.Fatalf("MarkPaid = %+v, %v, %v", paid, changed, err)
	}
	// Never overwritten, never moved backward.
	again, changed, err := s.MarkPaid(o.ID, "pay_2", "k2")
	if err != nil || changed || again != paid {
		t.Errorf("second MarkPaid = %+v, %v, %v; want unchanged", again, changed, err)
	}
	if r, changed, err := s.MarkReserved(o.ID, "res_2"); err != nil || changed || r != paid {
		t.Errorf("MarkReserved on paid order = %+v, %v, %v; want unchanged", r, changed, err)
	}
}

func TestMarkPaidConcurrentFirstWriterWins(t *testing.T) {
	s := NewStore()
	o := reservedOrder(t, s)
	const workers = 100
	var changedCount atomic.Int64
	run(workers, func(i int) {
		if i%2 == 1 {
			// Readers must never see a paid order without its payment ID, or
			// an order moving back to reserved after it was paid.
			got, _ := s.Get(o.ID)
			if got.Status == StatusPaid && got.PaymentID == "" {
				t.Error("paid order without payment ID")
			}
			return
		}
		if _, changed, _ := s.MarkPaid(o.ID, fmt.Sprintf("pay_%d", i), fmt.Sprintf("k%d", i)); changed {
			changedCount.Add(1)
		}
	})
	if changedCount.Load() != 1 {
		t.Errorf("%d writers changed the order, want 1", changedCount.Load())
	}
	if got, _ := s.Get(o.ID); got.Status != StatusPaid {
		t.Errorf("final status = %q", got.Status)
	}
}

func TestReserveOnPaidOrderDoesNotCallInventory(t *testing.T) {
	s := NewStore()
	o := reservedOrder(t, s)
	s.MarkPaid(o.ID, "pay_1", "k")
	inv := &fakeInventory{}
	got, err := NewService(s, inv).Reserve(context.Background(), o.ID)
	if err != nil || got.Status != StatusPaid || inv.callCount() != 0 {
		t.Errorf("Reserve on paid order = %+v, %v; inventory calls %d", got, err, inv.callCount())
	}
}

// run calls fn(i) for i in [0, n) concurrently, releasing all goroutines at
// once.
func run(n int, fn func(i int)) {
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
