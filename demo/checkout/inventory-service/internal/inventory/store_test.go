package inventory

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func newTestStore(units int) *Store {
	return NewStore(map[string]int{"SKU-A": units, "SKU-B": 5})
}

// mustItem fetches stock and fails the test if the SKU is missing.
func mustItem(t *testing.T, s *Store, sku string) Item {
	t.Helper()
	item, err := s.Get(sku)
	if err != nil {
		t.Fatalf("Get(%q) error = %v", sku, err)
	}
	return item
}

func TestSeedStock(t *testing.T) {
	want := map[string]int{"LAPTOP-001": 10, "PHONE-7": 50, "HEADPHONES-01": 100}
	got := SeedStock()
	if len(got) != len(want) {
		t.Fatalf("SeedStock() = %v, want %v", got, want)
	}
	for sku, units := range want {
		if got[sku] != units {
			t.Errorf("SeedStock()[%q] = %d, want %d", sku, got[sku], units)
		}
	}
	// Each call must return a fresh map so one test cannot affect another.
	got["LAPTOP-001"] = 0
	if SeedStock()["LAPTOP-001"] != 10 {
		t.Error("SeedStock returned a shared map")
	}
}

func TestGet(t *testing.T) {
	s := newTestStore(10)

	item := mustItem(t, s, "SKU-A")
	if item != (Item{SKU: "SKU-A", Available: 10, Reserved: 0}) {
		t.Errorf("Get = %+v", item)
	}

	if _, err := s.Get("NOPE"); !errors.Is(err, ErrUnknownSKU) {
		t.Errorf("Get(unknown) error = %v, want ErrUnknownSKU", err)
	}
}

func TestReserve(t *testing.T) {
	s := newTestStore(10)

	res, created, err := s.Reserve(ReserveRequest{OrderID: "ord_1", SKU: "SKU-A", Quantity: 3})
	if err != nil {
		t.Fatalf("Reserve error = %v", err)
	}
	if !created {
		t.Error("created = false, want true")
	}
	if !strings.HasPrefix(res.ID, "res_") || len(res.ID) <= len("res_") {
		t.Errorf("ID = %q, want res_ prefix and a random suffix", res.ID)
	}
	if res.OrderID != "ord_1" || res.SKU != "SKU-A" || res.Quantity != 3 || res.Status != StatusReserved {
		t.Errorf("reservation = %+v", res)
	}
	if res.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}

	if item := mustItem(t, s, "SKU-A"); item.Available != 7 || item.Reserved != 3 {
		t.Errorf("after reserve: %+v, want available 7 reserved 3", item)
	}
	// Other SKUs are untouched.
	if item := mustItem(t, s, "SKU-B"); item.Available != 5 || item.Reserved != 0 {
		t.Errorf("SKU-B changed: %+v", item)
	}
}

func TestReserveExactRemainingStock(t *testing.T) {
	s := newTestStore(10)
	if _, _, err := s.Reserve(ReserveRequest{OrderID: "ord_1", SKU: "SKU-A", Quantity: 10}); err != nil {
		t.Fatalf("reserving all stock: %v", err)
	}
	if item := mustItem(t, s, "SKU-A"); item.Available != 0 || item.Reserved != 10 {
		t.Errorf("item = %+v, want available 0 reserved 10", item)
	}
}

func TestReserveInsufficientStockLeavesStateUnchanged(t *testing.T) {
	s := newTestStore(10)

	_, created, err := s.Reserve(ReserveRequest{OrderID: "ord_1", SKU: "SKU-A", Quantity: 11})
	if !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("error = %v, want ErrInsufficientStock", err)
	}
	if created {
		t.Error("created = true on failure")
	}
	if item := mustItem(t, s, "SKU-A"); item.Available != 10 || item.Reserved != 0 {
		t.Errorf("item changed after failed reserve: %+v", item)
	}

	// The failed attempt must not have been recorded: the same order ID can
	// still reserve a smaller amount.
	if _, created, err := s.Reserve(ReserveRequest{OrderID: "ord_1", SKU: "SKU-A", Quantity: 1}); err != nil || !created {
		t.Errorf("reserve after failed attempt: created=%v err=%v", created, err)
	}
}

func TestReserveUnknownSKU(t *testing.T) {
	s := newTestStore(10)
	_, _, err := s.Reserve(ReserveRequest{OrderID: "ord_1", SKU: "NOPE", Quantity: 1})
	if !errors.Is(err, ErrUnknownSKU) {
		t.Errorf("error = %v, want ErrUnknownSKU", err)
	}
}

func TestReserveValidation(t *testing.T) {
	long := strings.Repeat("x", 65)
	tests := []struct {
		name string
		req  ReserveRequest
	}{
		{"missing order_id", ReserveRequest{SKU: "SKU-A", Quantity: 1}},
		{"blank order_id", ReserveRequest{OrderID: "   ", SKU: "SKU-A", Quantity: 1}},
		{"long order_id", ReserveRequest{OrderID: long, SKU: "SKU-A", Quantity: 1}},
		{"missing sku", ReserveRequest{OrderID: "ord_1", Quantity: 1}},
		{"blank sku", ReserveRequest{OrderID: "ord_1", SKU: " ", Quantity: 1}},
		{"long sku", ReserveRequest{OrderID: "ord_1", SKU: long, Quantity: 1}},
		{"zero quantity", ReserveRequest{OrderID: "ord_1", SKU: "SKU-A", Quantity: 0}},
		{"negative quantity", ReserveRequest{OrderID: "ord_1", SKU: "SKU-A", Quantity: -1}},
		{"huge quantity", ReserveRequest{OrderID: "ord_1", SKU: "SKU-A", Quantity: MaxQuantity + 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestStore(10)
			_, _, err := s.Reserve(tt.req)
			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("error = %v, want *ValidationError", err)
			}
			if item := mustItem(t, s, "SKU-A"); item.Available != 10 {
				t.Errorf("stock changed on invalid request: %+v", item)
			}
		})
	}
}

func TestReserveIdenticalRetryDoesNotDecrementAgain(t *testing.T) {
	s := newTestStore(10)
	req := ReserveRequest{OrderID: "ord_1", SKU: "SKU-A", Quantity: 4}

	first, created, err := s.Reserve(req)
	if err != nil || !created {
		t.Fatalf("first: created=%v err=%v", created, err)
	}
	for i := 0; i < 5; i++ {
		again, created, err := s.Reserve(req)
		if err != nil {
			t.Fatalf("retry %d: %v", i, err)
		}
		if created {
			t.Errorf("retry %d: created = true, want false", i)
		}
		if again != first {
			t.Errorf("retry %d: reservation = %+v, want %+v", i, again, first)
		}
	}
	if item := mustItem(t, s, "SKU-A"); item.Available != 6 || item.Reserved != 4 {
		t.Errorf("item = %+v, want available 6 reserved 4", item)
	}
}

func TestReserveRetryTreatsWhitespaceAsSameRequest(t *testing.T) {
	s := newTestStore(10)
	first, _, _ := s.Reserve(ReserveRequest{OrderID: "ord_1", SKU: "SKU-A", Quantity: 2})
	again, created, err := s.Reserve(ReserveRequest{OrderID: " ord_1 ", SKU: " SKU-A ", Quantity: 2})
	if err != nil || created || again != first {
		t.Errorf("created=%v err=%v again=%+v", created, err, again)
	}
}

func TestReserveRetrySucceedsEvenWhenStockIsNowGone(t *testing.T) {
	s := newTestStore(10)
	req := ReserveRequest{OrderID: "ord_1", SKU: "SKU-A", Quantity: 6}
	first, _, _ := s.Reserve(req)
	// Another order takes the rest.
	if _, _, err := s.Reserve(ReserveRequest{OrderID: "ord_2", SKU: "SKU-A", Quantity: 4}); err != nil {
		t.Fatal(err)
	}

	again, created, err := s.Reserve(req)
	if err != nil || created || again != first {
		t.Errorf("retry with no stock left: created=%v err=%v", created, err)
	}
}

func TestReserveConflictingRetry(t *testing.T) {
	s := newTestStore(10)
	first, _, err := s.Reserve(ReserveRequest{OrderID: "ord_1", SKU: "SKU-A", Quantity: 2})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		req  ReserveRequest
	}{
		{"different quantity", ReserveRequest{OrderID: "ord_1", SKU: "SKU-A", Quantity: 3}},
		{"different sku", ReserveRequest{OrderID: "ord_1", SKU: "SKU-B", Quantity: 2}},
		// Conflict wins over an unknown SKU: the order ID is already taken.
		{"unknown sku", ReserveRequest{OrderID: "ord_1", SKU: "NOPE", Quantity: 2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := s.Reserve(tt.req); !errors.Is(err, ErrOrderConflict) {
				t.Errorf("error = %v, want ErrOrderConflict", err)
			}
		})
	}

	if item := mustItem(t, s, "SKU-A"); item.Available != 8 || item.Reserved != 2 {
		t.Errorf("SKU-A = %+v, want available 8 reserved 2", item)
	}
	if item := mustItem(t, s, "SKU-B"); item.Available != 5 || item.Reserved != 0 {
		t.Errorf("SKU-B = %+v, want untouched", item)
	}
	// The original reservation is unchanged.
	if again, _, _ := s.Reserve(ReserveRequest{OrderID: "ord_1", SKU: "SKU-A", Quantity: 2}); again != first {
		t.Errorf("original reservation changed: %+v", again)
	}
}

func TestReservationIDsAreUnique(t *testing.T) {
	s := NewStore(map[string]int{"SKU-A": 1000})
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		res, _, err := s.Reserve(ReserveRequest{OrderID: fmt.Sprintf("ord_%d", i), SKU: "SKU-A", Quantity: 1})
		if err != nil {
			t.Fatal(err)
		}
		if seen[res.ID] {
			t.Fatalf("duplicate reservation ID %q", res.ID)
		}
		seen[res.ID] = true
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

func TestConcurrentReservationsNeverOversell(t *testing.T) {
	const (
		units    = 10
		attempts = 200
	)
	s := NewStore(map[string]int{"SKU-A": units})

	var succeeded, insufficient, other atomic.Int64
	ids := make(chan string, attempts)

	startTogether(attempts, func(i int) {
		// Each attempt is a different order wanting one unit.
		res, created, err := s.Reserve(ReserveRequest{OrderID: fmt.Sprintf("ord_%d", i), SKU: "SKU-A", Quantity: 1})
		switch {
		case err == nil && created:
			succeeded.Add(1)
			ids <- res.ID
		case errors.Is(err, ErrInsufficientStock):
			insufficient.Add(1)
		default:
			other.Add(1)
		}
	})
	close(ids)

	if got := succeeded.Load(); got != units {
		t.Errorf("successful reservations = %d, want exactly %d", got, units)
	}
	if got := insufficient.Load(); got != attempts-units {
		t.Errorf("insufficient-stock rejections = %d, want %d", got, attempts-units)
	}
	if got := other.Load(); got != 0 {
		t.Errorf("unexpected outcomes = %d, want 0", got)
	}
	if item := mustItem(t, s, "SKU-A"); item.Available != 0 || item.Reserved != units {
		t.Errorf("final item = %+v, want available 0 reserved %d", item, units)
	}
	seen := make(map[string]bool)
	for id := range ids {
		if seen[id] {
			t.Errorf("duplicate reservation ID %q", id)
		}
		seen[id] = true
	}
}

func TestConcurrentMultiUnitReservationsNeverOversell(t *testing.T) {
	// 100 attempts of 3 units against 10 units: exactly 3 can succeed (9
	// units), leaving 1 that nobody can take.
	s := NewStore(map[string]int{"SKU-A": 10})
	var succeeded atomic.Int64
	startTogether(100, func(i int) {
		_, created, err := s.Reserve(ReserveRequest{OrderID: fmt.Sprintf("ord_%d", i), SKU: "SKU-A", Quantity: 3})
		if err == nil && created {
			succeeded.Add(1)
		}
	})
	if got := succeeded.Load(); got != 3 {
		t.Errorf("successful reservations = %d, want 3", got)
	}
	if item := mustItem(t, s, "SKU-A"); item.Available != 1 || item.Reserved != 9 {
		t.Errorf("final item = %+v, want available 1 reserved 9", item)
	}
}

func TestConcurrentRetriesOfSameOrderDecrementOnce(t *testing.T) {
	s := newTestStore(10)
	req := ReserveRequest{OrderID: "ord_same", SKU: "SKU-A", Quantity: 2}

	var createdCount, replayed, failed atomic.Int64
	idsSeen := make(chan string, 100)
	startTogether(100, func(int) {
		res, created, err := s.Reserve(req)
		switch {
		case err != nil:
			failed.Add(1)
		case created:
			createdCount.Add(1)
			idsSeen <- res.ID
		default:
			replayed.Add(1)
			idsSeen <- res.ID
		}
	})
	close(idsSeen)

	if createdCount.Load() != 1 || replayed.Load() != 99 || failed.Load() != 0 {
		t.Errorf("created=%d replayed=%d failed=%d, want 1/99/0", createdCount.Load(), replayed.Load(), failed.Load())
	}
	if item := mustItem(t, s, "SKU-A"); item.Available != 8 || item.Reserved != 2 {
		t.Errorf("final item = %+v, want available 8 reserved 2 (decremented once)", item)
	}
	var first string
	for id := range idsSeen {
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatalf("callers saw different reservation IDs: %q and %q", first, id)
		}
	}
}

func TestConcurrentReadsAndWrites(t *testing.T) {
	// Mixed readers and writers; mainly useful under `go test -race`.
	s := NewStore(map[string]int{"SKU-A": 50})
	startTogether(100, func(i int) {
		if i%2 == 0 {
			_, _, _ = s.Reserve(ReserveRequest{OrderID: fmt.Sprintf("ord_%d", i), SKU: "SKU-A", Quantity: 1})
			return
		}
		item, err := s.Get("SKU-A")
		if err != nil || item.Available < 0 || item.Available+item.Reserved != 50 {
			t.Errorf("inconsistent read: %+v err=%v", item, err)
		}
	})
}
