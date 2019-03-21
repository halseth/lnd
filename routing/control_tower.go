package routing

import (
	"errors"

	"github.com/coreos/bbolt"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lntypes"
)

var (
	// ErrAlreadyPaid signals we have already paid this payment hash.
	ErrAlreadyPaid = errors.New("invoice is already paid")

	// ErrPaymentInFlight signals that payment for this payment hash is
	// already "in flight" on the network.
	ErrPaymentInFlight = errors.New("payment is in transition")

	// ErrPaymentNotInitiated is returned  if payment wasn't initiated in
	// switch.
	ErrPaymentNotInitiated = errors.New("payment isn't initiated")

	// ErrPaymentAlreadyCompleted is returned in the event we attempt to
	// recomplete a completed payment.
	ErrPaymentAlreadyCompleted = errors.New("payment is already completed")

	// ErrPaymentAlreadyFailed is returned in the event we attempt to
	// re-fail a failed payment.
	ErrPaymentAlreadyFailed = errors.New("payment has already failed")

	// ErrUnknownPaymentStatus is returned when we do not recognize the
	// existing state of a payment.
	ErrUnknownPaymentStatus = errors.New("unknown payment status")
)

// ControlTower tracks all outgoing payments made by the switch, whose primary
// purpose is to prevent duplicate payments to the same payment hash. In
// production, a persistent implementation is preferred so that tracking can
// survive across restarts. Payments are transition through various payment
// states, and the ControlTower interface provides access to driving the state
// transitions.
type ControlTower interface {
	// ClearForTakeoff atomically checks that no inflight or completed
	// payments exist for this payment hash. If none are found, this method
	// atomically transitions the status for this payment hash as InFlight.
	ClearForTakeoff(paymentHash lntypes.Hash) error

	// Success transitions an InFlight payment into a Completed payment.
	// After invoking this method, ClearForTakeoff should always return an
	// error to prevent us from making duplicate payments to the same
	// payment hash. The provided OutgoingPayment is atomically saved to
	// the DB for record keeping.
	Success(paymentHash lntypes.Hash,
		payment *channeldb.OutgoingPayment) error

	// Fail transitions an InFlight payment into a Failed Payment. After
	// invoking this method, ClearForTakeoff should return nil on its next
	// call for this payment hash, allowing the switch to make a subsequent
	// payment.
	Fail(paymentHash lntypes.Hash) error
}

// paymentControl is persistent implementation of ControlTower to restrict
// double payment sending.
type paymentControl struct {
	strict bool

	db *channeldb.DB
}

// NewPaymentControl creates a new instance of the paymentControl. The strict
// flag indicates whether the controller should require "strict" state
// transitions, which would be otherwise intolerant to older databases that may
// already have duplicate payments to the same payment hash. It should be
// enabled only after sufficient checks have been made to ensure the db does not
// contain such payments. In the meantime, non-strict mode enforces a superset
// of the state transitions that prevent additional payments to a given payment
// hash from being added.
func NewPaymentControl(strict bool, db *channeldb.DB) ControlTower {
	return &paymentControl{
		strict: strict,
		db:     db,
	}
}

// ClearForTakeoff checks that we don't already have an InFlight or Completed
// payment identified by the same payment hash.
func (p *paymentControl) ClearForTakeoff(paymentHash lntypes.Hash) error {
	var takeoffErr error
	err := p.db.Batch(func(tx *bbolt.Tx) error {
		// Retrieve current status of payment from local database.
		paymentStatus, err := channeldb.FetchPaymentStatusTx(
			tx, paymentHash,
		)
		if err != nil {
			return err
		}

		// Reset the takeoff error, to avoid carrying over an error
		// from a previous execution of the batched db transaction.
		takeoffErr = nil

		switch paymentStatus {

		// We allow retrying failed payments.
		case channeldb.StatusFailed:
			fallthrough

		case channeldb.StatusGrounded:
			// Add the payment to the payments bucket, which will
			// make it show up in "listpayments".
			err := channeldb.AddPaymentTx(tx, paymentHash)
			if err != nil {
				return err
			}

			// It is safe to reattempt a payment if we know that we
			// haven't left one in flight. Since this one is
			// grounded or failed, transition the payment status
			// to InFlight to prevent others.
			return channeldb.UpdatePaymentStatusTx(
				tx, paymentHash, channeldb.StatusInFlight,
			)

		case channeldb.StatusInFlight:
			// We already have an InFlight payment on the network. We will
			// disallow any more payment until a response is received.
			takeoffErr = ErrPaymentInFlight

		case channeldb.StatusCompleted:
			// We've already completed a payment to this payment hash,
			// forbid the switch from sending another.
			takeoffErr = ErrAlreadyPaid

		default:
			takeoffErr = ErrUnknownPaymentStatus
		}

		return nil
	})
	if err != nil {
		return err
	}

	return takeoffErr
}

// Success transitions an InFlight payment to Completed, otherwise it returns an
// error. After calling Success, ClearForTakeoff should prevent any further
// attempts for the same payment hash.
func (p *paymentControl) Success(paymentHash lntypes.Hash,
	payment *channeldb.OutgoingPayment) error {

	var updateErr error
	err := p.db.Batch(func(tx *bbolt.Tx) error {
		paymentStatus, err := channeldb.FetchPaymentStatusTx(
			tx, paymentHash,
		)
		if err != nil {
			return err
		}

		// Reset the update error, to avoid carrying over an error
		// from a previous execution of the batched db transaction.
		updateErr = nil

		switch {

		case paymentStatus == channeldb.StatusGrounded && p.strict:
			// Our records show the payment as still being grounded,
			// meaning it never should have left the switch.
			updateErr = ErrPaymentNotInitiated

		case paymentStatus == channeldb.StatusGrounded && !p.strict:
			// Though our records show the payment as still being
			// grounded, meaning it never should have left the
			// switch, we permit this transition in non-strict mode
			// to handle inconsistent db states.
			fallthrough

		case paymentStatus == channeldb.StatusFailed:
			// Though our records show the payment as failed,
			// meaning we didn't expect this result, we permit this
			// transition in non-strict mode to handle inconsistent
			// db states.
			fallthrough

		case paymentStatus == channeldb.StatusInFlight:
			// Record the successful payment atomically to the
			// payments record.
			err := channeldb.CompletePaymentTx(tx, payment)
			if err != nil {
				return err
			}

			// A successful response was received for an InFlight
			// payment, mark it as completed to prevent sending to
			// this payment hash again.
			return channeldb.UpdatePaymentStatusTx(
				tx, paymentHash, channeldb.StatusCompleted,
			)

		case paymentStatus == channeldb.StatusCompleted:
			// The payment was completed previously, alert the
			// caller that this may be a duplicate call.
			updateErr = ErrPaymentAlreadyCompleted

		default:
			updateErr = ErrUnknownPaymentStatus
		}

		return nil
	})
	if err != nil {
		return err
	}

	return updateErr
}

// Fail transitions an InFlight payment to Failed, otherwise it returns an
// error. After calling Fail, ClearForTakeoff should allow further attempts
// for the same payment hash.
func (p *paymentControl) Fail(paymentHash lntypes.Hash) error {
	var updateErr error
	err := p.db.Batch(func(tx *bbolt.Tx) error {
		paymentStatus, err := channeldb.FetchPaymentStatusTx(
			tx, paymentHash,
		)
		if err != nil {
			return err
		}

		// Reset the update error, to avoid carrying over an error
		// from a previous execution of the batched db transaction.
		updateErr = nil

		switch {

		case paymentStatus == channeldb.StatusGrounded && p.strict:
			// Our records show the payment as still being grounded,
			// meaning it never should have left the switch.
			updateErr = ErrPaymentNotInitiated

		case paymentStatus == channeldb.StatusGrounded && !p.strict:
			// Though our records show the payment as still being
			// grounded, meaning it never should have left the
			// switch, we permit this transition in non-strict mode
			// to handle inconsistent db states.
			fallthrough

		case paymentStatus == channeldb.StatusInFlight:
			// A failed response was received for an InFlight
			// payment, mark it as Failed to allow subsequent
			// attempts.
			return channeldb.UpdatePaymentStatusTx(
				tx, paymentHash, channeldb.StatusFailed,
			)

		case paymentStatus == channeldb.StatusCompleted:
			// The payment was completed previously, and we are now
			// reporting that it has failed. Leave the status as
			// completed, but alert the user that something is
			// wrong.
			updateErr = ErrPaymentAlreadyCompleted

		case paymentStatus == channeldb.StatusFailed:
			// The payment was already failed , and we are now
			// reporting that it has failed again . Leave the
			// status as failed, but alert the user that something
			// is wrong.
			updateErr = ErrPaymentAlreadyFailed

		default:
			updateErr = ErrUnknownPaymentStatus
		}

		return nil
	})
	if err != nil {
		return err
	}

	return updateErr
}
