package order

import (
	"errors"
	"fmt"
	"math"
)

// Currency is the only currency the demo charges in.
const Currency = "USD"

// ErrNoPrice means the price catalog has no price for the order's SKU.
var ErrNoPrice = errors.New("no price for sku")

// unitPricesCents is the price catalog, in US cents per unit.
//
// TEST DATA ONLY. It is fixed and deterministic so payments are reproducible
// in tests and demos; it is not real pricing and is not loaded from anywhere.
// It covers the Inventory Service's demo SKUs.
var unitPricesCents = map[string]int64{
	"LAPTOP-001":    129_999, // 1,299.99 USD
	"PHONE-7":       79_999,  //   799.99 USD
	"HEADPHONES-01": 14_999,  //   149.99 USD
}

// UnitPriceCents returns the catalog price for sku, and whether it has one.
func UnitPriceCents(sku string) (int64, bool) {
	p, ok := unitPricesCents[sku]
	return p, ok
}

// AmountCents returns what an order costs: unit price times quantity, in
// cents. The amount is always computed here, never taken from a client.
//
// It returns ErrNoPrice for a SKU without a price, and an error if the inputs
// are not positive or the result would overflow int64.
func AmountCents(sku string, quantity int) (int64, error) {
	price, ok := UnitPriceCents(sku)
	if !ok {
		return 0, fmt.Errorf("%w %q", ErrNoPrice, sku)
	}
	return multiplyCents(price, quantity)
}

// multiplyCents returns price*quantity, rejecting non-positive inputs and
// int64 overflow.
func multiplyCents(price int64, quantity int) (int64, error) {
	if price <= 0 || quantity <= 0 {
		return 0, fmt.Errorf("price (%d) and quantity (%d) must be positive", price, quantity)
	}
	if price > math.MaxInt64/int64(quantity) {
		return 0, fmt.Errorf("amount overflows: %d x %d cents", price, quantity)
	}
	return price * int64(quantity), nil
}
