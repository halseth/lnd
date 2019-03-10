package htlcswitch

import "errors"

var (
	// ErrPaymentIDNotFound is an error returned if the given paymentID is
	// not found.
	ErrPaymentIDNotFound = errors.New("paymentID not found")
)

// PaymentResult wraps a result received from the network after a payment
// attempt was made.
type PaymentResult interface {
}

// PaymentFailure is returned by the switch in case a HTLC send failed, and the
// HTLC is now irrevocably cancelled.
type PaymentFailure struct {
	Error *ForwardingError
}

// A compile time check to ensure PaymentFailure implements the PaymentResult
// interface.
var _ PaymentResult = (*PaymentFailure)(nil)

// PaymentSuccess is returned by the switch in case a sent HTLC was settled,
// and wraps the obtained preimage.
type PaymentSuccess struct {
	Preimage [32]byte
}

// A compile time check to ensure PaymentFailure implements the PaymentResult
// interface.
var _ PaymentResult = (*PaymentSuccess)(nil)
