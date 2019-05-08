package htlcswitch

import (
	"errors"

	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// ErrPaymentIDNotFound is an error returned if the given paymentID is
	// not found.
	ErrPaymentIDNotFound = errors.New("paymentID not found")

	// ErrPaymentIDAlreadyExists is returned if we try to write a pending
	// payment whose paymentID already exists.
	ErrPaymentIDAlreadyExists = errors.New("paymentID already exists")
)

// PaymentResultType indicates whether this is a success, or some error.
type PaymentResultType uint32

const (
	// PaymenrResultSuccess means this payment succeeded, and the preimage
	// is set.
	PaymentResultSuccess PaymentResultType = 1

	// PaymentResultLocalError indicates that a error was encountered
	// locally, and the error Reason will be plaintext.
	PaymentResultLocalError PaymentResultType = 2

	// PaymentResultResolutionError indicates that the payment timed out
	// on-chain, and the channel had to be closed.
	PaymentResultResolutionError PaymentResultType = 3

	// PaymentResultEncryptedError indicates that a multi-hop payment
	// resulted in an error, and we'll need to use the session key to
	// decrypt it from the Reason.
	PaymentResultEncryptedError PaymentResultType = 4
)

// PaymentResult wraps a result received from the network after a payment
// attempt was made.
type PaymentResult struct {
	// Type encodes what kind of result the payment got.
	Type PaymentResultType

	// Preimage is set by the switch in case the Type is
	// PaymentResultSuccess.
	Preimage [32]byte

	// Reason is set for all other result types, and will encode the error
	// encountered.
	Reason lnwire.OpaqueReason
}
