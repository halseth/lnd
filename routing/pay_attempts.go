package routing

import (
	"io"

	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
)

type payAttempt struct {
	paymentID uint64
	firstHop  lnwire.ShortChannelID
	htlcAdd   *lnwire.UpdateAddHTLC
	route     *Route
	circuit   *sphinx.Circuit
}

func serializePayAttempt(w io.Writer, p *payAttempt) error {
	if err := channeldb.WriteElements(w,
		p.paymentID, p.firstHop,
	); err != nil {
		return err
	}

	if err := p.htlcAdd.Encode(w, 0); err != nil {
		return err
	}

	if err := serializeRoute(w, p.route); err != nil {
		return err
	}

	if err := p.circuit.Encode(w); err != nil {
		return err
	}

	return nil
}

func deserializePayAttempt(r io.Reader) (*payAttempt, error) {
	p := &payAttempt{}
	if err := channeldb.ReadElements(r,
		&p.paymentID, &p.firstHop,
	); err != nil {
		return nil, err
	}

	htlc := &lnwire.UpdateAddHTLC{}
	if err := htlc.Decode(r, 0); err != nil {
		return nil, err
	}
	p.htlcAdd = htlc

	route, err := deserializeRoute(r)
	if err != nil {
		return nil, err
	}
	p.route = route

	circuit := &sphinx.Circuit{}
	if err := circuit.Decode(r); err != nil {
		return nil, err
	}
	p.circuit = circuit

	return p, nil
}
