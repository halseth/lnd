package routing

import (
	"bytes"

	"github.com/btcsuite/btcd/btcec"
	"github.com/davecgh/go-spew/spew"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// SphinxGenerator is an interface used to prepare the onion blob to send for a
// payment attempt, as well as decrypting any error returned for the same
// attempt.
type SphinxGenerator interface {
	// GenerateSphinxPacket generates then encodes a sphinx packet which
	// encodes the onion route specified by the passed layer 3 route, and
	// sesison key. The blob returned from this function can immediately be
	// included within an HTLC add packet to
	// be sent to the first hop within the route.
	GenerateSphinxPacket(rt *route.Route, paymentHash []byte,
		sessionKey *btcec.PrivateKey) ([]byte, error)

	// DecryptError decrypts a received error encountered when paying to
	// the given route using the session key.
	DecryptError(rt *route.Route, sessionKey *btcec.PrivateKey,
		reason lnwire.OpaqueReason) (*htlcswitch.ForwardingError, error)
}

// NewSphinxGenerator returns a new sphinxGenerator.
func NewSphinxGenerator() *sphinxGenerator {
	return &sphinxGenerator{}
}

// sphinxGenerator is the BOLT compatible inplementation of the SphinxGenerator
// interface.
type sphinxGenerator struct{}

// A compile time assertion to ensure sphinxGenerator meets the SphinxGenerator
// interface.
var _ SphinxGenerator = (*sphinxGenerator)(nil)

// GenerateSphinxPacket generates then encodes a sphinx packet which encodes
// the onion route specified by the passed layer 3 route, and session key. The
// blob returned from this function can immediately be included within an HTLC
// add packet to be sent to the first hop within the route.
func (s *sphinxGenerator) GenerateSphinxPacket(rt *route.Route, paymentHash []byte,
	sessionKey *btcec.PrivateKey) ([]byte, error) {

	// As a sanity check, we'll ensure that the set of hops has been
	// properly filled in, otherwise, we won't actually be able to
	// construct a route.
	if len(rt.Hops) == 0 {
		return nil, route.ErrNoRouteHopsProvided
	}

	// Now that we know we have an actual route, we'll map the route into a
	// sphinx payument path which includes per-hop paylods for each hop
	// that give each node within the route the necessary information
	// (fees, CLTV value, etc) to properly forward the payment.
	sphinxPath, err := rt.ToSphinxPath()
	if err != nil {
		return nil, err
	}

	log.Tracef("Constructed per-hop payloads for payment_hash=%x: %v",
		paymentHash[:], newLogClosure(func() string {
			return spew.Sdump(sphinxPath[:sphinxPath.TrueRouteLength()])
		}),
	)

	// Next generate the onion routing packet which allows us to perform
	// privacy preserving source routing across the network.
	sphinxPacket, err := sphinx.NewOnionPacket(
		sphinxPath, sessionKey, paymentHash,
	)
	if err != nil {
		return nil, err
	}

	// Finally, encode Sphinx packet using its wire representation to be
	// included within the HTLC add packet.
	var onionBlob bytes.Buffer
	if err := sphinxPacket.Encode(&onionBlob); err != nil {
		return nil, err
	}

	log.Tracef("Generated sphinx packet: %v",
		newLogClosure(func() string {
			// We unset the internal curve here in order to keep
			// the logs from getting noisy.
			sphinxPacket.EphemeralKey.Curve = nil
			return spew.Sdump(sphinxPacket)
		}),
	)

	return onionBlob.Bytes(), nil
}

// DecryptError decrypts a received error encountered when paying to the given
// route using the session key.
func (s *sphinxGenerator) DecryptError(rt *route.Route, sessionKey *btcec.PrivateKey,
	reason lnwire.OpaqueReason) (*htlcswitch.ForwardingError, error) {

	// Re-derive the sphinx circuit, and use that to decrypt the error.
	sphinxPath, err := rt.ToSphinxPath()
	if err != nil {
		return nil, err
	}

	circuit := &sphinx.Circuit{
		SessionKey:  sessionKey,
		PaymentPath: sphinxPath.NodeKeys(),
	}
	deobfuscator := &htlcswitch.SphinxErrorDecrypter{
		OnionErrorDecrypter: sphinx.NewOnionErrorDecrypter(circuit),
	}

	return deobfuscator.DecryptError(reason)
}
