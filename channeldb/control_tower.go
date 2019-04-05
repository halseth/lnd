package channeldb

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/coreos/bbolt"
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

// ControlTower tracks all outgoing payments made, whose primary purpose is to
// prevent duplicate payments to the same payment hash. In production, a
// persistent implementation is preferred so that tracking can survive across
// restarts. Payments are transitioned through various payment states, and the
// ControlTower interface provides access to driving the state transitions.
type ControlTower interface {
	// RegisterAttempt atomically moves the payment into the InFlight
	// state. The provided AttemptInfo is recorded in the DB. This method
	// checks that no completed payment exist for this payment hash.
	//
	// NOTE: The CreationInfo should only be set if we expect this payment
	// to not already be InFlight, causing it to return an error if it is.
	// If the CreationInfo is nil, we expect the payment to already be
	// known by the ControlTower, already InFlight.
	RegisterAttempt(lntypes.Hash, *CreationInfo, *AttemptInfo) error

	// Success transitions a payment into the Completed state.  After
	// invoking this method, RegisterAttempt should always return an error
	// to prevent us from making duplicate payments to the same payment
	// hash. The provided preimage is atomically saved to the DB for record
	// keeping.
	//
	// NOTE: The CreationInfo should only be set if we expect this payment
	// to not already be InFlight, causing it to return an error if it is.
	// If the CreationInfo is nil, we expect the payment to already be
	// known by the ControlTower, already InFlight.
	Success(lntypes.Hash, *CreationInfo, lntypes.Preimage) error

	// Fail transitions a payment into the Failed state. After
	// invoking this method, RegisterAttempt should return nil on its next
	// call for this payment hash, allowing the switch to make a subsequent
	// payment.
	//
	// NOTE: The CreationInfo should only be set if we expect this payment
	// to not already be InFlight, causing it to return an error if it is.
	// If the CreationInfo is nil, we expect the payment to already be
	// known by the ControlTower, already InFlight.
	Fail(lntypes.Hash, *CreationInfo) error

	// FetchInFlightPayments returns all payments with status InFlight.
	FetchInFlightPayments() ([]*InFlightPayment, error)
}

// paymentControl is persistent implementation of ControlTower to restrict
// double payment sending.
type paymentControl struct {
	db *DB
}

// NewPaymentControl creates a new instance of the paymentControl.
func NewPaymentControl(db *DB) ControlTower {
	return &paymentControl{
		db: db,
	}
}

// initPayment checks or records the given CreationInfo with the DB, making sure
// it does not already exist in case info is non-nil, and that it does exist if
// info is nil. Then this method returns successfully, the payment is
// guranteeed to be in the InFlight state.
func initPayment(bucket *bbolt.Bucket, paymentHash lntypes.Hash,
	info *CreationInfo) error {

	// Get the existing status of this payment, if any.
	paymentStatus := fetchPaymentStatus(bucket)

	// If into is non-nil this is the first payment attempt made to this
	// payment hash, and we'll record the creation info in the DB.
	firstAttempt := info != nil

	var infoBytes []byte
	var attemptBytes []byte
	if firstAttempt {
		var b bytes.Buffer
		if err := serializeCreationInfo(&b, info); err != nil {
			return err
		}
		infoBytes = b.Bytes()

		emptyAttempt := &AttemptInfo{}

		var a bytes.Buffer
		if err := serializeAttemptInfo(&a, emptyAttempt); err != nil {
			return err
		}
		attemptBytes = a.Bytes()
	}

	switch paymentStatus {

	// We already have an InFlight payment on the network. We will disallow
	// any first payment attempts, but allow updating new attempts.
	case StatusInFlight:
		if firstAttempt {
			return ErrPaymentInFlight
		}

	// We allow retrying failed payments.
	case StatusFailed:

	// It is safe to reattempt a payment if we know that we haven't left
	// one in flight.
	case StatusGrounded:
		// If the payment is grounded, this must be the first
		// attempt, or else it would have a status already in
		// the DB.
		if !firstAttempt {
			return ErrPaymentNotInitiated
		}

	// We've already completed a payment to this payment hash,
	// forbid the switch from sending another.
	case StatusCompleted:
		return ErrAlreadyPaid

	default:
		return ErrUnknownPaymentStatus
	}

	// We'll move it into the InFlight state.
	paymentStatus = StatusInFlight
	err := bucket.Put(paymentStatusKey, StatusInFlight.Bytes())
	if err != nil {
		return err
	}

	// Now record the information in the DB if this is the first attempt.
	if firstAttempt {
		// Add the payment info to the bucket, which contains the
		// static information for this payment
		err := bucket.Put(paymentCreationInfoKey, infoBytes)
		if err != nil {
			return err
		}

		// We'll add an empty attempt info to start with, in case we
		// are initializing a payment that went straight to
		// Failed/Success without an attempt being crafted.
		err = bucket.Put(paymentAttemptInfoKey, attemptBytes)
		if err != nil {
			return err
		}

		// Since this payment is not yet settled, we'll record an all-
		// zero preimage.
		var zero lntypes.Preimage
		err = bucket.Put(paymentSettleInfoKey, zero[:])
		if err != nil {
			return err
		}
	}

	return nil
}

// RegisterAttempt atomically moves the payment into the InFlight state. The
// provided AttemptInfo is recorded in the DB. This method checks that no
// completed payment exist for this payment hash.
func (p *paymentControl) RegisterAttempt(paymentHash lntypes.Hash,
	info *CreationInfo, attempt *AttemptInfo) error {

	// Serialize the information before opening the db transaction.
	var a bytes.Buffer
	if err := serializeAttemptInfo(&a, attempt); err != nil {
		return err
	}
	attemptBytes := a.Bytes()

	return p.db.Batch(func(tx *bbolt.Tx) error {
		bucket, err := fetchPaymentBucket(tx, paymentHash)
		if err != nil {
			return err
		}

		// Init the payment to make sure we are not owerwriting a n
		// InFlight attempt if we expect this to be the first attempt.
		err = initPayment(bucket, paymentHash, info)
		if err != nil {
			return err
		}

		// We already have an InFlight payment on the network. We will
		// disallow any first payment attempts, but allow updating the
		// until a response is received.
		// Add the payment attempt to the payments bucket.
		err = bucket.Put(paymentAttemptInfoKey, attemptBytes)
		if err != nil {
			return err
		}

		return nil

	})
}

// Success transitions a payment into the Completed state.  After invoking this
// method, RegisterAttempt should always return an error to prevent us from
// making duplicate payments to the same payment hash. The provided preimage is
// atomically saved to the DB for record keeping.
func (p *paymentControl) Success(paymentHash lntypes.Hash, c *CreationInfo,
	preimage lntypes.Preimage) error {

	return p.db.Batch(func(tx *bbolt.Tx) error {
		bucket, err := fetchPaymentBucket(tx, paymentHash)
		if err != nil {
			return err
		}

		// Init the payment to make sure we are not owerwriting an
		// InFlight attempt if we expect this payment to not be
		// InFlight.
		err = initPayment(bucket, paymentHash, c)
		if err != nil {
			return err
		}

		// A successful response was received for an InFlight payment,
		// mark it as completed to prevent sending to this payment hash
		// again.
		// Record the successful payment info atomically to the
		// payments record.
		err = bucket.Put(paymentSettleInfoKey, preimage[:])
		if err != nil {
			return err
		}
		return bucket.Put(paymentStatusKey, StatusCompleted.Bytes())
	})
}

// Fail transitions a payment into the Failed state. After invoking this
// method, RegisterAttempt should return nil on its next call for this payment
// hash, allowing the switch to make a subsequent payment.
func (p *paymentControl) Fail(paymentHash lntypes.Hash, c *CreationInfo) error {
	return p.db.Batch(func(tx *bbolt.Tx) error {
		bucket, err := fetchPaymentBucket(tx, paymentHash)
		if err != nil {
			return err
		}

		// Init the payment to make sure we are not owerwriting an
		// InFlight attempt if we expect this payment to not be
		// InFlight.
		err = initPayment(bucket, paymentHash, c)
		if err != nil {
			return err
		}

		// A failed response was received for an InFlight payment, mark
		// it as Failed to allow subsequent attempts.
		return bucket.Put(paymentStatusKey, StatusFailed.Bytes())
	})
}

// fetchPaymentBucket fetches or creates the sub-bucket assigned to this
// payment hash.
func fetchPaymentBucket(tx *bbolt.Tx, paymentHash lntypes.Hash) (
	*bbolt.Bucket, error) {

	payments, err := tx.CreateBucketIfNotExists(sentPaymentsBucket)
	if err != nil {
		return nil, err
	}

	return payments.CreateBucketIfNotExists(paymentHash[:])
}

// fetchPaymentStatus fetches the payment status from the bucket.  If the
// status isn't found, it will default to "StatusGrounded".
func fetchPaymentStatus(bucket *bbolt.Bucket) PaymentStatus {
	// The default status for all payments that aren't recorded in
	// database.
	var paymentStatus = StatusGrounded

	paymentStatusBytes := bucket.Get(paymentStatusKey)
	if paymentStatusBytes != nil {
		paymentStatus.FromBytes(paymentStatusBytes)
	}

	return paymentStatus
}

// InFlightPayment is a wrapper around a payment that has status InFlight.
type InFlightPayment struct {
	// PaymentHash is the hash of the in-flight payment.
	Info *CreationInfo

	// Attempt contains information about the last payment attempt that was
	// made to this payment hash.
	Attempt *AttemptInfo
}

// FetchInFlightPayments returns all payments with status InFlight.
func (p *paymentControl) FetchInFlightPayments() ([]*InFlightPayment, error) {
	var inFlights []*InFlightPayment
	err := p.db.View(func(tx *bbolt.Tx) error {
		payments := tx.Bucket(sentPaymentsBucket)
		if payments == nil {
			return nil
		}

		return payments.ForEach(func(k, _ []byte) error {
			bucket := payments.Bucket(k)
			if bucket == nil {
				return fmt.Errorf("non bucket element")
			}

			// If the status is not InFlight, we can return early.
			paymentStatus := fetchPaymentStatus(bucket)
			if paymentStatus != StatusInFlight {
				return nil
			}

			// Get the CreationInfo.
			b := bucket.Get(paymentCreationInfoKey)
			if b == nil {
				return fmt.Errorf("unable to find creation " +
					"info for inflight payment")
			}

			r := bytes.NewReader(b)
			c, err := deserializeCreationInfo(r)
			if err != nil {
				return err

			}

			// Now get the attempt info.
			attempt := bucket.Get(paymentAttemptInfoKey)
			if attempt == nil {
				return fmt.Errorf("unable to find attempt " +
					"info for inflight payment")
			}

			r = bytes.NewReader(attempt)
			a, err := deserializeAttemptInfo(r)
			if err != nil {
				return err
			}

			// We return an ActivePayment with inFlight=true. This
			// is used to indicate that we expect this payment to
			// have already had the attempt forwarded onto the
			// network.
			inFlight := &InFlightPayment{
				Info:    c,
				Attempt: a,
			}

			inFlights = append(inFlights, inFlight)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	return inFlights, nil
}
