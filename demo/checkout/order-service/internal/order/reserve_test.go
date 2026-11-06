package order

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeInventory is an in-memory InventoryReserver. By default it succeeds
// and, like the real Inventory Service, returns the same reservation ID for
// repeated calls with the same order ID.
type fakeInventory struct {
	mu    sync.Mutex
	calls int
	ids   map[string]string // order ID -> reservation ID
	err   error             // if set, every call fails with it
	last  ReserveRequest
}

func (f *fakeInventory) Reserve(_ context.Context, req ReserveRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.last = req
	if f.err != nil {
		return "", f.err
	}
	if f.ids == nil {
		f.ids = make(map[string]string)
	}
	id, ok := f.ids[req.OrderID]
	if !ok {
		id = fmt.Sprintf("res_%d", len(f.ids)+1)
		f.ids[req.OrderID] = id
	}
	return id, nil
}

func (f *fakeInventory) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// newOrder stores a fresh order and returns it.
func newOrder(t *testing.T, s *Store) Order {
	t.Helper()
	o, err := New("LAPTOP-001", 2)
	if err != nil {
		t.Fatal(err)
	}
	s.Add(o)
	return o
}

func TestServiceReserve(t *testing.T) {
	store := NewStore()
	inv := &fakeInventory{}
	svc := NewService(store, inv)
	o := newOrder(t, store)

	got, err := svc.Reserve(context.Background(), o.ID)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if got.Status != StatusReserved || got.ReservationID != "res_1" {
		t.Errorf("order = %+v, want reserved with res_1", got)
	}
	// The order's own details went to the Inventory Service, keyed by order ID.
	if inv.last != (ReserveRequest{OrderID: o.ID, SKU: "LAPTOP-001", Quantity: 2}) {
		t.Errorf("inventory request = %+v", inv.last)
	}
	// The change is stored, and nothing else about the order changed.
	stored, _ := store.Get(o.ID)
	if stored != got {
		t.Errorf("stored = %+v, want %+v", stored, got)
	}
	want := o
	want.Status, want.ReservationID = StatusReserved, "res_1"
	if got != want {
		t.Errorf("order = %+v, want %+v", got, want)
	}
}

func TestServiceReserveAlreadyReservedSkipsInventory(t *testing.T) {
	store := NewStore()
	inv := &fakeInventory{}
	svc := NewService(store, inv)
	o := newOrder(t, store)

	first, _ := svc.Reserve(context.Background(), o.ID)
	for i := 0; i < 3; i++ {
		again, err := svc.Reserve(context.Background(), o.ID)
		if err != nil || again != first {
			t.Fatalf("repeat %d = %+v, %v; want %+v", i, again, err, first)
		}
	}
	if n := inv.callCount(); n != 1 {
		t.Errorf("inventory called %d times, want 1", n)
	}
}

func TestServiceReserveNotFound(t *testing.T) {
	inv := &fakeInventory{}
	_, err := NewService(NewStore(), inv).Reserve(context.Background(), "ord_missing")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if inv.callCount() != 0 {
		t.Error("inventory was called for a missing order")
	}
}

func TestServiceReserveNotConfigured(t *testing.T) {
	store := NewStore()
	o := newOrder(t, store)
	if _, err := NewService(store, nil).Reserve(context.Background(), o.ID); !errors.Is(err, ErrInventoryNotConfigured) {
		t.Errorf("err = %v, want ErrInventoryNotConfigured", err)
	}
}

func TestServiceReserveFailureLeavesOrderUnchanged(t *testing.T) {
	for _, upstreamErr := range []error{
		ErrInsufficientStock, ErrUnknownSKU, ErrInventoryConflict,
		ErrInventoryUnavailable, ErrInventoryTimeout, ErrInventoryBadResponse,
	} {
		t.Run(upstreamErr.Error(), func(t *testing.T) {
			store := NewStore()
			o := newOrder(t, store)
			inv := &fakeInventory{err: upstreamErr}

			_, err := NewService(store, inv).Reserve(context.Background(), o.ID)
			if !errors.Is(err, upstreamErr) {
				t.Errorf("err = %v, want %v", err, upstreamErr)
			}
			if stored, _ := store.Get(o.ID); stored != o {
				t.Errorf("order changed after failure: %+v", stored)
			}
		})
	}
}

func TestServiceReserveRetryAfterAmbiguousFailure(t *testing.T) {
	store := NewStore()
	o := newOrder(t, store)
	inv := &fakeInventory{err: ErrInventoryTimeout}
	svc := NewService(store, inv)

	if _, err := svc.Reserve(context.Background(), o.ID); !errors.Is(err, ErrInventoryTimeout) {
		t.Fatalf("first attempt err = %v", err)
	}
	inv.mu.Lock()
	inv.err = nil
	inv.mu.Unlock()

	got, err := svc.Reserve(context.Background(), o.ID)
	if err != nil || got.Status != StatusReserved {
		t.Errorf("retry = %+v, %v; want reserved", got, err)
	}
}

func TestServiceReserveConcurrent(t *testing.T) {
	store := NewStore()
	o := newOrder(t, store)
	inv := &fakeInventory{}
	svc := NewService(store, inv)

	const workers = 100
	results := make([]Order, workers)
	var failures atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got, err := svc.Reserve(context.Background(), o.ID)
			if err != nil {
				failures.Add(1)
				return
			}
			results[i] = got
		}()
	}
	close(start)
	wg.Wait()

	if failures.Load() != 0 {
		t.Fatalf("%d concurrent reserves failed", failures.Load())
	}
	for i, r := range results {
		if r.Status != StatusReserved || r.ReservationID != "res_1" {
			t.Fatalf("result %d = %+v, want reserved with res_1", i, r)
		}
	}
	inv.mu.Lock()
	distinct := len(inv.ids)
	inv.mu.Unlock()
	if distinct != 1 {
		t.Errorf("inventory holds %d reservations, want 1", distinct)
	}
}

func TestMarkReserved(t *testing.T) {
	s := NewStore()
	o := newOrder(t, s)

	got, changed, err := s.MarkReserved(o.ID, "res_1")
	if err != nil || !changed || got.Status != StatusReserved || got.ReservationID != "res_1" {
		t.Fatalf("first MarkReserved = %+v, %v, %v", got, changed, err)
	}
	// A second call never replaces the reservation or changes the status.
	again, changed, err := s.MarkReserved(o.ID, "res_2")
	if err != nil || changed || again != got {
		t.Errorf("second MarkReserved = %+v, %v, %v; want unchanged %+v", again, changed, err, got)
	}
	if _, _, err := s.MarkReserved("ord_missing", "res_1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing order err = %v, want ErrNotFound", err)
	}
}

func TestMarkReservedConcurrentFirstWriterWins(t *testing.T) {
	// Many writers race with different reservation IDs. Exactly one changes
	// the order; every caller then sees that same winner, and readers never
	// see the order go back to created.
	s := NewStore()
	o := newOrder(t, s)

	const workers = 100
	var changedCount atomic.Int64
	seen := make([]string, workers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			got, changed, err := s.MarkReserved(o.ID, fmt.Sprintf("res_%d", i))
			if err != nil {
				t.Errorf("MarkReserved: %v", err)
				return
			}
			if changed {
				changedCount.Add(1)
			}
			seen[i] = got.ReservationID
		}()
		go func() {
			defer wg.Done()
			<-start
			got, _ := s.Get(o.ID)
			if got.Status == StatusReserved && got.ReservationID == "" {
				t.Error("reserved order without a reservation ID")
			}
		}()
	}
	close(start)
	wg.Wait()

	if changedCount.Load() != 1 {
		t.Errorf("%d writers changed the order, want 1", changedCount.Load())
	}
	final, _ := s.Get(o.ID)
	if final.Status != StatusReserved {
		t.Fatalf("final status = %q", final.Status)
	}
	for i, id := range seen {
		if id != final.ReservationID {
			t.Errorf("writer %d saw %q, final is %q", i, id, final.ReservationID)
		}
	}
}
