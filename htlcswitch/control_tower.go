package htlcswitch

import (
	"errors"

	"github.com/coreos/bbolt"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// ErrAlreadyPaid signals we have already paid this payment hash.
	ErrAlreadyPaid = errors.New("invoice is already paid")

	// ErrConflictingPaymentInFlight is returned if a payment with a
	// different payment ID paying to the same payment hash is in flight on
	// the network.
	ErrConflictingPaymentInFlight = errors.New("conflicting payment with " +
		"same hash is in transition")

	// ErrPaymentNotInitiated is returned  if payment wasn't initiated in
	// switch.
	ErrPaymentNotInitiated = errors.New("payment isn't initiated")

	// ErrPaymentAlreadyCompleted is returned in the event we attempt to
	// recomplete a completed payment.
	ErrPaymentAlreadyCompleted = errors.New("payment is already completed")

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
	// payments exist for this payment hash with a different paymentID. If
	// none are found, this method atomically transitions the status for
	// this payment hash paymentID as InFlight. If the exact payment (same
	// ID and hash) is already found, no error is returned, as it is
	// assumed safe to let the switch handle a duplicate replay. If a
	// conflicting payment paying to the same hash but having a different
	// payment ID is found, ErrConflictingPaymentInFlight is returned. If a
	// payment to this hash has already succeeded, ErrAlreadyPaid is
	// returned. In case of ErrAlready paid, the preimage obtained will
	// also be returned.
	ClearForTakeoff(paymentID uint64, htlc *lnwire.UpdateAddHTLC) (
		[32]byte, error)

	// Success transitions an InFlight payment into a Completed payment.
	// After invoking this method, ClearForTakeoff should always return an
	// error to prevent us from making duplicate payments to the same
	// payment hash. On success the preimage corresponding to the payment
	// hash should be supplied, for easy retrieval later.
	Success(paymentHash, preImage [32]byte) error

	// Fail transitions an InFlight payment into a Grounded Payment. After
	// invoking this method, ClearForTakeoff should return nil on its next
	// call for this payment hash, allowing the switch to make a subsequent
	// payment.
	Fail(paymentHash [32]byte) error
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
func (p *paymentControl) ClearForTakeoff(paymentID uint64,
	htlc *lnwire.UpdateAddHTLC) ([32]byte, error) {

	var takeoffErr error
	var preimage [32]byte
	err := p.db.Batch(func(tx *bbolt.Tx) error {
		// Retrieve current status of payment from local database.
		paymentStatus, err := channeldb.FetchPaymentStatusTx(
			tx, htlc.PaymentHash,
		)
		if err != nil {
			return err
		}

		// Reset the takeoff error, to avoid carrying over an error
		// from a previous execution of the batched db transaction.
		takeoffErr = nil
		preimage = [32]byte{}

		switch paymentStatus {

		case channeldb.StatusGrounded:
			// It is safe to reattempt a payment if we know that we
			// haven't left one in flight. Since this one is
			// grounded, Transition the payment status to InFlight
			// to prevent others.
			err := channeldb.UpdatePaymentStatusTx(
				tx, htlc.PaymentHash, channeldb.StatusInFlight,
			)
			if err != nil {
				return err
			}

			return channeldb.UpdatePaymentIDTx(
				tx, htlc.PaymentHash, paymentID,
			)

		case channeldb.StatusInFlight:
			// We already have an InFlight payment on the network.
			// Check whether it is a conflicting payment with a
			// different paymentID, or the same one already cleared
			// for takeoff.
			pid, err := channeldb.FetchPaymentIDTx(
				tx, htlc.PaymentHash,
			)
			if err != nil {
				return err
			}

			// We will disallow any more payment until a response
			// is received if a conflicting payment is paying to
			// this hash..
			if pid != paymentID {
				takeoffErr = ErrConflictingPaymentInFlight
			}

			return nil

		case channeldb.StatusCompleted:
			// We've already completed a payment to this payment hash,
			// forbid the switch from sending another.
			takeoffErr = ErrAlreadyPaid

			// We've already completed a payment to this payment
			// hash, forbid the switch from sending another.
			preimage, err = channeldb.FetchPaymentPreimageTx(
				tx, htlc.PaymentHash,
			)
			return err

		default:
			takeoffErr = ErrUnknownPaymentStatus
		}

		return nil
	})
	if err != nil {
		return preimage, err
	}

	return preimage, takeoffErr
}

// Success transitions an InFlight payment to Completed, otherwise it returns an
// error. After calling Success, ClearForTakeoff should prevent any further
// attempts for the same payment hash.
func (p *paymentControl) Success(paymentHash, preimage [32]byte) error {
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
			// A successful response was received for an InFlight
			// payment, mark it as completed to prevent sending to
			// this payment hash again.
			err := channeldb.UpdatePaymentStatusTx(
				tx, paymentHash, channeldb.StatusCompleted,
			)
			if err != nil {
				return err
			}

			// Also store the preimage in the database.
			return channeldb.UpdatePaymentPreimageTx(
				tx, paymentHash, preimage,
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

// Fail transitions an InFlight payment to Grounded, otherwise it returns an
// error. After calling Fail, ClearForTakeoff should fail any further attempts
// for the same payment hash.
func (p *paymentControl) Fail(paymentHash [32]byte) error {
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
			// payment, mark it as Grounded again to allow
			// subsequent attempts.
			return channeldb.UpdatePaymentStatusTx(
				tx, paymentHash, channeldb.StatusGrounded,
			)

		case paymentStatus == channeldb.StatusCompleted:
			// The payment was completed previously, and we are now
			// reporting that it has failed. Leave the status as
			// completed, but alert the user that something is
			// wrong.
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
