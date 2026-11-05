package order

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNew(t *testing.T) {
	before := time.Now()
	o, err := New("  LAPTOP-001  ", 2)
	after := time.Now()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if !strings.HasPrefix(o.ID, "ord_") || len(o.ID) <= len("ord_") {
		t.Errorf("ID = %q, want non-empty with prefix %q", o.ID, "ord_")
	}
	if o.SKU != "LAPTOP-001" {
		t.Errorf("SKU = %q, want surrounding whitespace trimmed", o.SKU)
	}
	if o.Quantity != 2 {
		t.Errorf("Quantity = %d, want 2", o.Quantity)
	}
	if o.Status != StatusCreated {
		t.Errorf("Status = %q, want %q", o.Status, StatusCreated)
	}
	if o.CreatedAt.Before(before) || o.CreatedAt.After(after) {
		t.Errorf("CreatedAt = %v, want between %v and %v", o.CreatedAt, before, after)
	}
}

func TestNewValidation(t *testing.T) {
	tests := []struct {
		name     string
		sku      string
		quantity int
		wantErr  bool
	}{
		{name: "minimum quantity", sku: "A", quantity: MinQuantity},
		{name: "maximum quantity", sku: "A", quantity: MaxQuantity},
		{name: "longest sku", sku: strings.Repeat("A", MaxSKULen), quantity: 1},
		{name: "empty sku", sku: "", quantity: 1, wantErr: true},
		{name: "whitespace sku", sku: "   ", quantity: 1, wantErr: true},
		{name: "sku too long", sku: strings.Repeat("A", MaxSKULen+1), quantity: 1, wantErr: true},
		{name: "quantity zero", sku: "A", quantity: 0, wantErr: true},
		{name: "quantity negative", sku: "A", quantity: -1, wantErr: true},
		{name: "quantity above maximum", sku: "A", quantity: MaxQuantity + 1, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.sku, tt.quantity)
			if (err != nil) != tt.wantErr {
				t.Fatalf("New(%q, %d) error = %v, wantErr %v", tt.sku, tt.quantity, err, tt.wantErr)
			}
			if err != nil {
				var verr *ValidationError
				if !errors.As(err, &verr) {
					t.Errorf("error type = %T, want *ValidationError", err)
				}
			}
		})
	}
}

func TestNewGeneratesDistinctIDs(t *testing.T) {
	seen := make(map[string]bool)
	for range 1000 {
		o, err := New("A", 1)
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		if seen[o.ID] {
			t.Fatalf("duplicate ID %q", o.ID)
		}
		seen[o.ID] = true
	}
}
