// Package payment holds the Payment Service's business rules: what a payment
// attempt is, which requests are valid, and how attempts are recorded. It
// knows nothing about HTTP.
//
// This is a SIMULATION. No real payment provider is involved, and no card
// numbers or credentials are accepted: the payment method is a test switch
// that decides the outcome.
package payment

import (
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// Limits and supported values for a valid payment request.
const (
	// Currency is the only supported currency in the MVP.
	Currency = "USD"

	MinAmountCents = 1
	// MaxAmountCents is 1,000,000.00 USD. It is a sanity limit for the demo,
	// far below int64 overflow, not a business rule from a real provider.
	MaxAmountCents = 100_000_000

	MaxOrderIDLen        = 64
	MaxIdempotencyKeyLen = 128
)

// Payment methods. They are test switches, not real instruments.
const (
	// MethodApprove always produces an approved payment.
	MethodApprove = "test_approve"
	// MethodDecline always produces a declined payment.
	MethodDecline = "test_decline"
)

// Status is the outcome of a payment attempt.
type Status string

const (
	// StatusApproved means the simulated payment was approved.
	StatusApproved Status = "approved"
	// StatusDeclined means the simulated payment was declined. A decline is a
	// normal business outcome, not an error.
	StatusDeclined Status = "declined"
)

// Errors returned by Store. Use errors.Is to test for them.
var (
	// ErrNotFound means no payment has the requested ID.
	ErrNotFound = errors.New("payment not found")
	// ErrIdempotencyConflict means the idempotency key was already used with
	// different payment parameters.
	ErrIdempotencyConflict = errors.New("idempotency_key was already used with different payment details")
	// ErrOrderAlreadyPaid means the order already has an approved payment.
	ErrOrderAlreadyPaid = errors.New("order already has an approved payment")
)

// ValidationError reports why a request was rejected. Its message is safe to
// show to API clients.
type ValidationError struct {
	Message string
}

func (e *ValidationError) Error() string { return e.Message }

// Request asks for a simulated payment attempt.
type Request struct {
	OrderID        string
	AmountCents    int64
	Currency       string
	PaymentMethod  string
	IdempotencyKey string
}

// Payment is a recorded payment attempt.
type Payment struct {
	ID          string    `json:"id"`
	OrderID     string    `json:"order_id"`
	AmountCents int64     `json:"amount_cents"`
	Currency    string    `json:"currency"`
	Status      Status    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
}

// normalize trims whitespace and validates the request. The trimmed values are
// what get compared and stored, so " k1 " and "k1" are the same key.
func (r Request) normalize() (Request, error) {
	r.OrderID = strings.TrimSpace(r.OrderID)
	r.IdempotencyKey = strings.TrimSpace(r.IdempotencyKey)
	r.Currency = strings.TrimSpace(r.Currency)
	r.PaymentMethod = strings.TrimSpace(r.PaymentMethod)

	if err := checkIdentifier("order_id", r.OrderID, MaxOrderIDLen); err != nil {
		return r, err
	}
	if err := checkIdentifier("idempotency_key", r.IdempotencyKey, MaxIdempotencyKeyLen); err != nil {
		return r, err
	}
	if r.AmountCents < MinAmountCents || r.AmountCents > MaxAmountCents {
		return r, &ValidationError{Message: fmt.Sprintf("amount_cents must be between %d and %d", MinAmountCents, MaxAmountCents)}
	}
	if r.Currency != Currency {
		return r, &ValidationError{Message: "currency must be " + Currency}
	}
	if r.PaymentMethod != MethodApprove && r.PaymentMethod != MethodDecline {
		return r, &ValidationError{Message: fmt.Sprintf("payment_method must be %s or %s", MethodApprove, MethodDecline)}
	}
	return r, nil
}

// checkIdentifier requires a non-empty value of at most max bytes with no
// control characters (so identifiers are safe to put in logs).
func checkIdentifier(name, value string, max int) error {
	if value == "" {
		return &ValidationError{Message: name + " is required"}
	}
	if len(value) > max {
		return &ValidationError{Message: fmt.Sprintf("%s must be at most %d characters", name, max)}
	}
	if strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return &ValidationError{Message: name + " must not contain control characters"}
	}
	return nil
}

// outcome maps a payment method to the status it produces.
func outcome(method string) Status {
	if method == MethodApprove {
		return StatusApproved
	}
	return StatusDeclined
}

// newPaymentID returns an unpredictable, collision-resistant ID such as
// "pay_7XQ2K5M3...". rand.Text draws 128 bits from crypto/rand.
func newPaymentID() string {
	return "pay_" + rand.Text()
}
