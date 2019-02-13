package routing

import (
	"io"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
)

// LightningPayment describes a payment to be sent through the network to the
// final destination.
type LightningPayment struct {
	// Target is the node in which the payment should be routed towards.
	Target *btcec.PublicKey

	// Amount is the value of the payment to send through the network in
	// milli-satoshis.
	Amount lnwire.MilliSatoshi

	// FeeLimit is the maximum fee in millisatoshis that the payment should
	// accept when sending it through the network. The payment will fail
	// if there isn't a route with lower fees than this limit.
	FeeLimit lnwire.MilliSatoshi

	// PaymentHash is the r-hash value to use within the HTLC extended to
	// the first hop.
	PaymentHash [32]byte

	// FinalCLTVDelta is the CTLV expiry delta to use for the _final_ hop
	// in the route. This means that the final hop will have a CLTV delta
	// of at least: currentHeight + FinalCLTVDelta. If this value is
	// unspecified, then a default value of DefaultFinalCLTVDelta will be
	// used.
	FinalCLTVDelta uint16

	// PayAttemptTimeout is a timeout value that we'll use to determine
	// when we should should abandon the payment attempt after consecutive
	// payment failure. This prevents us from attempting to send a payment
	// indefinitely.
	PayAttemptTimeout time.Duration

	// RouteHints represents the different routing hints that can be used to
	// assist a payment in reaching its destination successfully. These
	// hints will act as intermediate hops along the route.
	//
	// NOTE: This is optional unless required by the payment. When providing
	// multiple routes, ensure the hop hints within each route are chained
	// together and sorted in forward order in order to reach the
	// destination successfully.
	RouteHints [][]HopHint

	// OutgoingChannelID is the channel that needs to be taken to the first
	// hop. If zero, any channel may be used.
	OutgoingChannelID uint64

	// TODO(roasbeef): add e2e message?
}

func serializeLightningPayment(w io.Writer, l *LightningPayment) error {
	if err := channeldb.WriteElements(w,
		l.Target, l.Amount, l.FeeLimit, l.PaymentHash,
		l.FinalCLTVDelta, l.PayAttemptTimeout,
	); err != nil {
		return err
	}

	if err := channeldb.WriteElements(w, uint32(len(l.RouteHints))); err != nil {
		return err
	}

	for _, r := range l.RouteHints {
		if err := serializeRouteHint(w, r); err != nil {
			return err
		}
	}

	return nil
}

func deserializeLightningPayment(r io.Reader) (*LightningPayment, error) {
	l := &LightningPayment{}
	if err := channeldb.ReadElements(r,
		&l.Target, &l.Amount, &l.FeeLimit, &l.PaymentHash,
		&l.FinalCLTVDelta, &l.PayAttemptTimeout,
	); err != nil {
		return nil, err
	}

	var numRouteHints uint32
	if err := channeldb.ReadElements(r, &numRouteHints); err != nil {
		return nil, err
	}

	var routeHints [][]HopHint
	for i := uint32(0); i < numRouteHints; i++ {
		routeHint, err := deserializeRouteHint(r)
		if err != nil {
			return nil, err
		}
		routeHints = append(routeHints, routeHint)
	}
	l.RouteHints = routeHints

	return l, nil

}

func serializeRouteHint(w io.Writer, routeHint []HopHint) error {

	if err := channeldb.WriteElements(w, uint32(len(routeHint))); err != nil {
		return err
	}

	for _, h := range routeHint {
		if err := serializeHopHint(w, h); err != nil {
			return err
		}
	}

	return nil
}

func deserializeRouteHint(r io.Reader) ([]HopHint, error) {

	var numHopHints uint32
	if err := channeldb.ReadElements(r, &numHopHints); err != nil {
		return nil, err
	}

	var routeHint []HopHint
	for i := uint32(0); i < numHopHints; i++ {
		hopHint, err := deserializeHopHint(r)
		if err != nil {
			return nil, err
		}
		routeHint = append(routeHint, hopHint)
	}

	return routeHint, nil
}

func serializeHopHint(w io.Writer, h HopHint) error {
	if err := channeldb.WriteElements(w,
		h.NodeID, h.ChannelID, h.FeeBaseMSat,
		h.FeeProportionalMillionths, h.CLTVExpiryDelta,
	); err != nil {
		return err
	}

	return nil
}

func deserializeHopHint(r io.Reader) (HopHint, error) {
	h := HopHint{}
	if err := channeldb.ReadElements(r,
		&h.NodeID, &h.ChannelID, &h.FeeBaseMSat,
		&h.FeeProportionalMillionths, &h.CLTVExpiryDelta,
	); err != nil {
		return h, err
	}

	return h, nil
}
