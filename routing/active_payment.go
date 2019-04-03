package routing

import (
	"time"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
)

// ActivePayment is a wrapper around a ControlTower that is used to handle its
// state transitions in the DB.
type ActivePayment struct {
	control channeldb.ControlTower

	// inFlight indicates whether this payment is already in-flight. This
	// is set after the first attempt is registered with the control tower,
	// or if this is a payment fetcehd from the DB after a restart.
	inFlight bool

	// info is the static CreationInfo that is valid for the lifetime of
	// the payment.
	Info channeldb.CreationInfo

	// Attempt is the latest recorded payment attempt.
	//
	// NOTE: can be nil.
	Attempt *channeldb.AttemptInfo
}

// NewActivePayment is called to create a new active payment to the given
// payment hash. This payment hash will be expected to not already be InFlight.
func NewActivePayment(control channeldb.ControlTower, paymentHash lntypes.Hash,
	amt lnwire.MilliSatoshi, payReq []byte) *ActivePayment {

	return &ActivePayment{
		control: control,
		Info: channeldb.CreationInfo{
			PaymentHash:    paymentHash,
			Value:          amt,
			CreationDate:   time.Now(),
			PaymentRequest: payReq,
		},
		inFlight: false,
		Attempt:  nil,
	}
}

// NewAttempt records a new payment attempt with the underlying ControlTower.
func (p *ActivePayment) NewAttempt(a *channeldb.AttemptInfo) error {
	var c *channeldb.CreationInfo

	// If this payment is not in flight, we record its CreationInfo with
	// the ControlTower.
	if !p.inFlight {
		c = &p.Info
	}

	err := p.control.RegisterAttempt(p.Info.PaymentHash, c, a)
	if err != nil {
		return err
	}

	// Now keep the latest attempt, and note that it is considered
	// InFlight, such that successive calls to NewAttempt won't record the
	// CreationInfo.
	p.Attempt = a
	p.inFlight = true
	return nil
}

// Success marks the payment succeeded with the given preimage.
func (p *ActivePayment) Success(preimage lntypes.Preimage) error {
	var c *channeldb.CreationInfo

	// If this payment is not in flight, we record its CreationInfo with
	// the ControlTower.
	if !p.inFlight {
		c = &p.Info
	}
	return p.control.Success(p.Info.PaymentHash, c, preimage)
}

// Fail records the payment as failed.
func (p *ActivePayment) Fail() error {
	var c *channeldb.CreationInfo

	// If this payment is not in flight, we record its CreationInfo with
	// the ControlTower.
	if !p.inFlight {
		c = &p.Info
	}

	return p.control.Fail(p.Info.PaymentHash, c)
}
