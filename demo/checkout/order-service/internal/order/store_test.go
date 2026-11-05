package order

import (
	"errors"
	"sync"
	"testing"
)

func TestStoreAddAndGet(t *testing.T) {
	s := NewStore()
	o, err := New("LAPTOP-001", 3)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	s.Add(o)

	got, err := s.Get(o.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got != o {
		t.Errorf("Get() = %+v, want %+v", got, o)
	}
}

func TestStoreGetMissing(t *testing.T) {
	_, err := NewStore().Get("ord_does-not-exist")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get() error = %v, want ErrNotFound", err)
	}
}

// TestStoreConcurrentUse is most valuable under the race detector
// (go test -race), which flags unsynchronized access to the map.
func TestStoreConcurrentUse(t *testing.T) {
	const workers = 50
	s := NewStore()

	var wg sync.WaitGroup
	ids := make([]string, workers)
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o, err := New("A", 1)
			if err != nil {
				t.Errorf("New() error = %v", err)
				return
			}
			s.Add(o)
			ids[i] = o.ID
			// Read while other goroutines are writing.
			if _, err := s.Get(o.ID); err != nil {
				t.Errorf("Get(%q) error = %v", o.ID, err)
			}
		}()
	}
	wg.Wait()

	for _, id := range ids {
		if _, err := s.Get(id); err != nil {
			t.Errorf("Get(%q) after all writes: %v", id, err)
		}
	}
}
