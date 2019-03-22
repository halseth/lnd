package channeldb

import (
	"io"

	"github.com/lightningnetwork/lnd/lnwire"
)

// deserializeCloseChannelSummaryV6 reads the v6 database format for
// ChannelCloseSummary.
//
// NOTE: deprecated, only for migration.
func deserializeCloseChannelSummaryV6(r io.Reader) (*ChannelCloseSummary, error) {
	c := &ChannelCloseSummary{}

	err := ReadElements(r,
		&c.ChanPoint, &c.ShortChanID, &c.ChainHash, &c.ClosingTXID,
		&c.CloseHeight, &c.RemotePub, &c.Capacity, &c.SettledBalance,
		&c.TimeLockedBalance, &c.CloseType, &c.IsPending,
	)
	if err != nil {
		return nil, err
	}

	// We'll now check to see if the channel close summary was encoded with
	// any of the additional optional fields.
	err = ReadElements(r, &c.RemoteCurrentRevocation)
	switch {
	case err == io.EOF:
		return c, nil

	// If we got a non-eof error, then we know there's an actually issue.
	// Otherwise, it may have been the case that this summary didn't have
	// the set of optional fields.
	case err != nil:
		return nil, err
	}

	if err := readChanConfig(r, &c.LocalChanConfig); err != nil {
		return nil, err
	}

	// Finally, we'll attempt to read the next unrevoked commitment point
	// for the remote party. If we closed the channel before receiving a
	// funding locked message, then this can be nil. As a result, we'll use
	// the same technique to read the field, only if there's still data
	// left in the buffer.
	err = ReadElements(r, &c.RemoteNextRevocation)
	if err != nil && err != io.EOF {
		// If we got a non-eof error, then we know there's an actually
		// issue. Otherwise, it may have been the case that this
		// summary didn't have the set of optional fields.
		return nil, err
	}

	return c, nil
}

// outgoingPaymentV8 is the OutgoingPayment format up to db versino o.
type outgoingPaymentV8 struct {
	Invoice
	Fee             lnwire.MilliSatoshi
	TimeLockLength  uint32
	Path            [][33]byte
	PaymentPreimage [32]byte
}

// deserializeOutgoingPaymentV8 reads an OutgoingPayment having the
// v8 DB format.
//
// NOTE: only for migration.
func deserializeOutgoingPaymentV8(r io.Reader) (*outgoingPaymentV8, error) {
	var scratch [8]byte

	p := &outgoingPaymentV8{}

	inv, err := deserializeInvoice(r)
	if err != nil {
		return nil, err
	}
	p.Invoice = inv

	if _, err := r.Read(scratch[:]); err != nil {
		return nil, err
	}
	p.Fee = lnwire.MilliSatoshi(byteOrder.Uint64(scratch[:]))

	if _, err = r.Read(scratch[:4]); err != nil {
		return nil, err
	}
	pathLen := byteOrder.Uint32(scratch[:4])

	path := make([][33]byte, pathLen)
	for i := uint32(0); i < pathLen; i++ {
		if _, err := r.Read(path[i][:]); err != nil {
			return nil, err
		}
	}
	p.Path = path

	if _, err = r.Read(scratch[:4]); err != nil {
		return nil, err
	}
	p.TimeLockLength = byteOrder.Uint32(scratch[:4])

	if _, err := r.Read(p.PaymentPreimage[:]); err != nil {
		return nil, err
	}

	return p, nil
}
