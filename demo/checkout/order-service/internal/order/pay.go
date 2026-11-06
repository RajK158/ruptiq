package order

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode"
)

// Payment methods. They are test switches understood by the Payment Service,
// not real instruments; no card data is ever accepted.
const (
	MethodApprove = "test_approve"
	MethodDecline = "test_decline"
)

// MaxPaymentKeyLen limits the client's idempotency key, in bytes.
const MaxPaymentKeyLen = 64

// MaxUpstreamKeyLen is the Payment Service's limit, in bytes, on the key it
// receives: "<order ID>:<client key>". Pay checks the whole combined key.
const MaxUpstreamKeyLen = 128

// PaymentStatus is a payment attempt's outcome.
type PaymentStatus string

const (
	PaymentApproved PaymentStatus = "approved"
	PaymentDeclined PaymentStatus = "declined"
)

// Errors from Payer.Pay. Use errors.Is to test for them.
var (
	// ErrOrderNotReserved: only orders with confirmed inventory can be paid.
	ErrOrderNotReserved = errors.New("order inventory is not reserved")
	// ErrOrderAlreadyPaid: the order already has a recorded approved payment
	// under a different idempotency key.
	ErrOrderAlreadyPaid = errors.New("order is already paid")

	// ErrPaymentKeyConflict: the idempotency key was already used for this
	// order with a different payment method.
	ErrPaymentKeyConflict = errors.New("idempotency key was already used with different payment details")
	// ErrPaymentApprovedElsewhere: the Payment Service holds an approved
	// payment for this order that the Order Service has not recorded (for
	// example, its response was lost). It cannot be looked up by order, so
	// recovery requires retrying with the idempotency key that produced it.
	ErrPaymentApprovedElsewhere = errors.New("payment service already has an approved payment for this order")
	// ErrPaymentRecordConflict: the Payment Service approved a payment, but
	// the order is already recorded as paid by a different payment. This
	// should be impossible while the Payment Service keeps its state.
	ErrPaymentRecordConflict = errors.New("order is recorded as paid by a different payment")

	// ErrPaymentUnavailable: the Payment Service could not be reached, or
	// answered with a server error. The outcome is unknown.
	ErrPaymentUnavailable = errors.New("payment service unavailable")
	// ErrPaymentTimeout: the Payment Service did not answer in time. The
	// payment may or may not have been recorded.
	ErrPaymentTimeout = errors.New("payment service timed out")
	// ErrPaymentBadResponse: the Payment Service answered with something not
	// understood or not matching the request. The outcome is unknown.
	ErrPaymentBadResponse = errors.New("unexpected response from payment service")
	// ErrPaymentNotConfigured: no Payment Service client was provided.
	ErrPaymentNotConfigured = errors.New("payment service is not configured")
)

// PaymentRequest is what the Order Service asks the Payment Service to do.
type PaymentRequest struct {
	OrderID       string
	AmountCents   int64
	Currency      string
	PaymentMethod string
	// IdempotencyKey is the key sent upstream (already scoped to the order).
	IdempotencyKey string
}

// PaymentResult is a payment attempt as confirmed by the Payment Service.
type PaymentResult struct {
	ID          string        `json:"id"`
	Status      PaymentStatus `json:"status"`
	AmountCents int64         `json:"amount_cents"`
	Currency    string        `json:"currency"`
}

// PaymentProcessor is the one thing the Order Service needs from the Payment
// Service: submit a payment attempt and get its outcome.
//
// It is defined here, where it is used; package paymentclient implements it
// over HTTP, tests use fakes. Implementations must be safe for concurrent use.
//
// A nil error means the Payment Service confirmed the result, approved or
// declined, and the result matches the request (order, amount, currency, and
// the outcome the payment method selects). Failures must match one of the
// ErrPayment* values above.
type PaymentProcessor interface {
	Pay(ctx context.Context, req PaymentRequest) (PaymentResult, error)
}

// PayResult is the outcome of Payer.Pay: the order after the attempt, and the
// payment that was approved or declined.
type PayResult struct {
	Order   Order         `json:"order"`
	Payment PaymentResult `json:"payment"`
}

// Payer runs the "pay for an order" use case.
type Payer struct {
	store    *Store
	payments PaymentProcessor
}

// NewPayer returns a Payer. payments may be nil, in which case Pay fails with
// ErrPaymentNotConfigured for orders that would need the Payment Service.
func NewPayer(store *Store, payments PaymentProcessor) *Payer {
	return &Payer{store: store, payments: payments}
}

// Pay submits a simulated payment for a reserved order.
//
// method is MethodApprove or MethodDecline; key is the client's idempotency
// key for this attempt, scoped to the order. Rules:
//
//   - The amount is computed from the price catalog, never supplied by the
//     client.
//   - Only a reserved order can be paid. Created orders get
//     ErrOrderNotReserved.
//   - A paid order is answered locally, without calling the Payment Service:
//     the same key and method return the recorded approval (a safe replay);
//     the same key with another method is ErrPaymentKeyConflict; any other
//     key is ErrOrderAlreadyPaid. So a paid order never triggers another
//     payment attempt.
//   - Approved: the order moves from reserved to paid with the payment ID and
//     key, by compare-and-set; nothing overwrites an existing approval.
//   - Declined: the order stays reserved (its stock is still held) and the
//     declined payment is returned with a nil error. Another attempt needs a
//     new key; reusing the declined key replays the decline.
//   - Any failure, including ambiguous ones (timeout, dropped connection,
//     malformed response), leaves the order unchanged. Retrying with the same
//     key is safe and recovers the recorded result, because the Payment
//     Service is idempotent. No retry happens automatically.
//   - No lock is held while calling the Payment Service.
func (p *Payer) Pay(ctx context.Context, orderID, method, key string) (PayResult, error) {
	method = strings.TrimSpace(method)
	key = strings.TrimSpace(key)
	if err := validatePayment(method, key); err != nil {
		return PayResult{}, err
	}

	o, err := p.store.Get(orderID)
	if err != nil {
		return PayResult{}, err
	}
	if n := len(upstreamKey(o.ID, key)); n > MaxUpstreamKeyLen {
		return PayResult{}, &ValidationError{Message: fmt.Sprintf(
			"idempotency_key is too long for this order: with the order ID it is %d bytes, the limit is %d",
			n, MaxUpstreamKeyLen)}
	}
	if o.Status == StatusCreated {
		return PayResult{}, ErrOrderNotReserved
	}
	amount, err := AmountCents(o.SKU, o.Quantity)
	if err != nil {
		return PayResult{}, err
	}
	if o.Status == StatusPaid {
		return paidReplay(o, amount, method, key)
	}

	if p.payments == nil {
		return PayResult{}, ErrPaymentNotConfigured
	}
	res, err := p.payments.Pay(ctx, PaymentRequest{
		OrderID:        o.ID,
		AmountCents:    amount,
		Currency:       Currency,
		PaymentMethod:  method,
		IdempotencyKey: upstreamKey(o.ID, key),
	})
	if err != nil {
		if errors.Is(err, ErrPaymentApprovedElsewhere) {
			// Maybe a concurrent request for this order has just recorded
			// its approval. If so, report the order as paid; otherwise the
			// approval is unknown locally and the caller must recover it.
			if cur, gerr := p.store.Get(o.ID); gerr == nil && cur.Status == StatusPaid {
				return paidReplay(cur, amount, method, key)
			}
		}
		return PayResult{}, fmt.Errorf("paying for %s: %w", o.ID, err)
	}

	if res.Status == PaymentDeclined {
		cur, err := p.store.Get(o.ID)
		if err != nil {
			return PayResult{}, err
		}
		return PayResult{Order: cur, Payment: res}, nil
	}

	updated, _, err := p.store.MarkPaid(o.ID, res.ID, key)
	if err != nil {
		return PayResult{}, err
	}
	if updated.PaymentID != res.ID {
		// The order was already paid by another payment. With a working
		// Payment Service (one approval per order) this cannot happen; it
		// could if that service lost its state. Keep the recorded payment
		// and refuse to report this one as the order's payment.
		slog.Error("approved payment does not match the order's recorded payment",
			"order_id", o.ID, "recorded_payment_id", updated.PaymentID, "received_payment_id", res.ID)
		return PayResult{}, ErrPaymentRecordConflict
	}
	return PayResult{Order: updated, Payment: res}, nil
}

// paidReplay answers a payment request for an order that is already paid.
func paidReplay(o Order, amount int64, method, key string) (PayResult, error) {
	if key != o.PaymentIdempotencyKey {
		return PayResult{}, ErrOrderAlreadyPaid
	}
	if method != MethodApprove {
		return PayResult{}, ErrPaymentKeyConflict
	}
	return PayResult{
		Order: o,
		Payment: PaymentResult{
			ID:          o.PaymentID,
			Status:      PaymentApproved,
			AmountCents: amount,
			Currency:    Currency,
		},
	}, nil
}

// upstreamKey scopes a client's idempotency key to one order. The Payment
// Service's keys are global, so without this two orders that both use
// "attempt-1" would collide. Order IDs contain no ':', so the mapping is
// unambiguous.
func upstreamKey(orderID, key string) string {
	return orderID + ":" + key
}

func validatePayment(method, key string) error {
	if method == "" {
		return &ValidationError{Message: "payment_method is required"}
	}
	if method != MethodApprove && method != MethodDecline {
		return &ValidationError{Message: fmt.Sprintf("payment_method must be %s or %s", MethodApprove, MethodDecline)}
	}
	if key == "" {
		return &ValidationError{Message: "idempotency_key is required"}
	}
	if len(key) > MaxPaymentKeyLen {
		return &ValidationError{Message: fmt.Sprintf("idempotency_key must be at most %d characters", MaxPaymentKeyLen)}
	}
	if strings.IndexFunc(key, unicode.IsControl) >= 0 {
		return &ValidationError{Message: "idempotency_key must not contain control characters"}
	}
	return nil
}
