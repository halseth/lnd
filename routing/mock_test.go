package routing

import (
	"encoding/binary"
	"fmt"
	"sync/atomic"

	"github.com/btcsuite/btcd/btcec"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// mockSphinxGenerator is a mock implementation of the SphinxGenerator
// interface.
type mockSphinxGenerator struct {
	errIndex uint64 // to be used atomically
	errors   map[uint64]*htlcswitch.ForwardingError
}

var _ SphinxGenerator = (*mockSphinxGenerator)(nil)

func newMockSphinxGenerator() *mockSphinxGenerator {
	return &mockSphinxGenerator{
		errors: make(map[uint64]*htlcswitch.ForwardingError),
	}
}

func (s *mockSphinxGenerator) GenerateSphinxPacket(rt *route.Route, paymentHash []byte,
	sessionKey *btcec.PrivateKey) ([]byte, error) {

	return nil, nil
}

func (s *mockSphinxGenerator) DecryptError(rt *route.Route, sessionKey *btcec.PrivateKey,
	reason lnwire.OpaqueReason) (*htlcswitch.ForwardingError, error) {

	key := binary.BigEndian.Uint64(reason[:])
	fwdErr, ok := s.errors[key]
	if !ok {
		return nil, fmt.Errorf("unknown error")
	}
	return fwdErr, nil
}

// craftErrorResult will create a PaymentResult with the given error source and
// failure, that can later be decrypted by this mockSphinxGenerator.
func (s *mockSphinxGenerator) craftErrorResult(errSource *btcec.PublicKey,
	failure lnwire.FailureMessage) (*htlcswitch.PaymentResult, error) {

	fwdErr := &htlcswitch.ForwardingError{
		ErrorSource:    errSource,
		FailureMessage: failure,
	}

	// Add the error to the internal map, such that we will know to return
	// it if we are later asked to decrypt it.
	key := atomic.AddUint64(&s.errIndex, 1)
	s.errors[key] = fwdErr

	// We'll use the key as our OpaqueReason, so we recognize this error
	// when we are asked to decrypt it.
	var reason [8]byte
	binary.BigEndian.PutUint64(reason[:], key)

	res := &htlcswitch.PaymentResult{
		Type:   htlcswitch.PaymentResultEncryptedError,
		Reason: reason[:],
	}

	return res, nil
}
