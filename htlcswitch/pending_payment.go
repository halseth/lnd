package htlcswitch

import (
	"errors"
	"io"

	"github.com/lightningnetwork/lnd/channeldb"
)

var (
	// ErrPaymentIDNotFound is an error returned if the given paymentID is
	// not found.
	ErrPaymentIDNotFound = errors.New("paymentID not found")
)

// PaymentResult wraps a result received from the network after a payment
// attempt was made.
type PaymentResult interface {
	// Encode serializes the PaymentResult.
	Encode(w io.Writer) error

	// Decode deserializes the PaymentResult.
	Decode(r io.Reader) error
}

// PaymentFailure is returned by the switch in case a HTLC send failed, and the
// HTLC is now irrevocably cancelled.
type PaymentFailure struct {
	Error *ForwardingError
}

// Encode serializes the PaymentFailure.
//
// NOTE: Part of the PaymentResult interface.
func (p *PaymentFailure) Encode(w io.Writer) error {
	return channeldb.WriteElements(w,
		p.Error.ErrorSource, p.Error.ExtraMsg,
		p.Error.FailureMessage,
	)
}

// Decode deserializes the PaymentFailure.
//
// NOTE: Part of the PaymentResult interface.
func (p *PaymentFailure) Decode(r io.Reader) error {
	return channeldb.ReadElements(r,
		&p.Error.ErrorSource, &p.Error.ExtraMsg,
		&p.Error.FailureMessage,
	)
}

// A compile time check to ensure PaymentFailure implements the PaymentResult
// interface.
var _ PaymentResult = (*PaymentFailure)(nil)

// PaymentSuccess is returned by the switch in case a sent HTLC was settled,
// and wraps the obtained preimage.
type PaymentSuccess struct {
	Preimage [32]byte
}

// Encode serializes the PaymentSuccess.
//
// NOTE: Part of the PaymentResult interface.
func (p *PaymentSuccess) Encode(w io.Writer) error {
	return channeldb.WriteElements(w,
		p.Preimage[:],
	)
}

// Decode deserializes the PaymentSuccess.
//
// NOTE: Part of the PaymentResult interface.
func (p *PaymentSuccess) Decode(r io.Reader) error {
	return channeldb.ReadElements(r,
		p.Preimage[:],
	)
}

// A compile time check to ensure PaymentFailure implements the PaymentResult
// interface.
var _ PaymentResult = (*PaymentSuccess)(nil)
