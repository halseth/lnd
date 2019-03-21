package channeldb

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/coreos/bbolt"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/routing/route"
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
	ClearForTakeoff(info *CreationInfo) error

	Attempt(lntypes.Hash, *AttemptInfo) error

	// Success transitions an InFlight payment into a Completed payment.
	// After invoking this method, ClearForTakeoff should always return an
	// error to prevent us from making duplicate payments to the same
	// payment hash. The provided info is atomically saved to the DB for
	// record keeping.
	Success(paymentHash lntypes.Hash, info *SettleInfo) error

	// Fail transitions an InFlight payment into a Failed Payment. After
	// invoking this method, ClearForTakeoff should return nil on its next
	// call for this payment hash, allowing the switch to make a subsequent
	// payment.
	Fail(paymentHash lntypes.Hash) error

	// FetchInFlightPayments returns all payments with status InFlight.
	FetchInFlightPayments() ([]*InFlightPayment, error)
}

// paymentControl is persistent implementation of ControlTower to restrict
// double payment sending.
type paymentControl struct {
	strict bool

	db *DB
}

// NewPaymentControl creates a new instance of the paymentControl. The strict
// flag indicates whether the controller should require "strict" state
// transitions, which would be otherwise intolerant to older databases that may
// already have duplicate payments to the same payment hash. It should be
// enabled only after sufficient checks have been made to ensure the db does not
// contain such payments. In the meantime, non-strict mode enforces a superset
// of the state transitions that prevent additional payments to a given payment
// hash from being added.
func NewPaymentControl(strict bool, db *DB) ControlTower {
	return &paymentControl{
		strict: strict,
		db:     db,
	}
}

// ClearForTakeoff checks that we don't already have an InFlight or Completed
// payment identified by the same payment hash.
func (p *paymentControl) ClearForTakeoff(info *CreationInfo) error {
	paymentHash := info.PaymentHash

	var b bytes.Buffer
	if err := serializeCreationInfo(&b, info); err != nil {
		return err
	}
	infoBytes := b.Bytes()

	var takeoffErr error
	err := p.db.Batch(func(tx *bbolt.Tx) error {
		payments, err := tx.CreateBucketIfNotExists(outgoingPaymentBucket)
		if err != nil {
			return err
		}

		bucket, err := payments.CreateBucketIfNotExists(paymentHash[:])
		if err != nil {
			return err
		}

		// Get the existing status of this payment, if any.
		paymentStatus := fetchPaymentStatus(bucket)

		// Reset the takeoff error, to avoid carrying over an error
		// from a previous execution of the batched db transaction.
		takeoffErr = nil

		switch paymentStatus {

		// We allow retrying failed payments.
		case StatusFailed:
			fallthrough

		case StatusGrounded:
			// Add the payment info to the bucket, which contains
			// the static information for this payment
			err := bucket.Put(paymentCreationInfoKey, infoBytes)
			if err != nil {
				return err
			}

			// It is safe to reattempt a payment if we know that we
			// haven't left one in flight. Since this one is
			// grounded or failed, transition the payment status
			// to InFlight to prevent others.
			return bucket.Put(paymentStatusKey, StatusInFlight.Bytes())

		case StatusInFlight:
			// We already have an InFlight payment on the network. We will
			// disallow any more payment until a response is received.
			takeoffErr = ErrPaymentInFlight

		case StatusCompleted:
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

type AttemptInfo struct {
	PaymentID uint64
	Route     *route.Route
}

func (p *paymentControl) Attempt(paymentHash lntypes.Hash,
	a *AttemptInfo) error {

	// Serialize the information before opening the db transaction.
	var b bytes.Buffer
	if err := serializeRoute(&b, a.Route); err != nil {
		return err
	}

	if err := WriteElements(&b, a.PaymentID); err != nil {
		return err
	}

	return p.db.Batch(func(tx *bbolt.Tx) error {

		payments, err := tx.CreateBucketIfNotExists(outgoingPaymentBucket)
		if err != nil {
			return err
		}

		bucket, err := payments.CreateBucketIfNotExists(paymentHash[:])
		if err != nil {
			return err
		}

		// Add the payment to the payments bucket, which will
		// make it show up in "listpayments".
		return bucket.Put(paymentAttemptInfoKey, b.Bytes())
	})
}

// Success transitions an InFlight payment to Completed, otherwise it returns an
// error. After calling Success, ClearForTakeoff should prevent any further
// attempts for the same payment hash.
func (p *paymentControl) Success(paymentHash lntypes.Hash, info *SettleInfo) error {
	var b bytes.Buffer
	if err := serializeSettleInfo(&b, info); err != nil {
		return err
	}
	settledBytes := b.Bytes()

	var updateErr error
	err := p.db.Batch(func(tx *bbolt.Tx) error {
		payments, err := tx.CreateBucketIfNotExists(outgoingPaymentBucket)
		if err != nil {
			return err
		}

		bucket, err := payments.CreateBucketIfNotExists(paymentHash[:])
		if err != nil {
			return err
		}

		// Get the existing status, if any.
		paymentStatus := fetchPaymentStatus(bucket)

		// Reset the update error, to avoid carrying over an error
		// from a previous execution of the batched db transaction.
		updateErr = nil

		switch {

		case paymentStatus == StatusGrounded && p.strict:
			// Our records show the payment as still being grounded,
			// meaning it never should have left the switch.
			updateErr = ErrPaymentNotInitiated

		case paymentStatus == StatusGrounded && !p.strict:
			// Though our records show the payment as still being
			// grounded, meaning it never should have left the
			// switch, we permit this transition in non-strict mode
			// to handle inconsistent db states.
			fallthrough

		case paymentStatus == StatusFailed:
			// Though our records show the payment as failed,
			// meaning we didn't expect this result, we permit this
			// transition in non-strict mode to handle inconsistent
			// db states.
			fallthrough

		case paymentStatus == StatusInFlight:
			// Record the successful payment info atomically to the
			// payments record.
			err := bucket.Put(paymentSettleInfoKey, settledBytes)
			if err != nil {
				return err
			}

			// A successful response was received for an InFlight
			// payment, mark it as completed to prevent sending to
			// this payment hash again.
			return bucket.Put(paymentStatusKey, StatusCompleted.Bytes())

		case paymentStatus == StatusCompleted:
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
		payments, err := tx.CreateBucketIfNotExists(outgoingPaymentBucket)
		if err != nil {
			return err
		}

		bucket, err := payments.CreateBucketIfNotExists(paymentHash[:])
		if err != nil {
			return err
		}

		paymentStatus := fetchPaymentStatus(bucket)

		// Reset the update error, to avoid carrying over an error
		// from a previous execution of the batched db transaction.
		updateErr = nil

		switch {

		case paymentStatus == StatusGrounded && p.strict:
			// Our records show the payment as still being grounded,
			// meaning it never should have left the switch.
			updateErr = ErrPaymentNotInitiated

		case paymentStatus == StatusGrounded && !p.strict:
			// Though our records show the payment as still being
			// grounded, meaning it never should have left the
			// switch, we permit this transition in non-strict mode
			// to handle inconsistent db states.
			fallthrough

		case paymentStatus == StatusInFlight:
			// A failed response was received for an InFlight
			// payment, mark it as Failed to allow subsequent
			// attempts.
			return bucket.Put(paymentStatusKey, StatusFailed.Bytes())

		case paymentStatus == StatusCompleted:
			// The payment was completed previously, and we are now
			// reporting that it has failed. Leave the status as
			// completed, but alert the user that something is
			// wrong.
			updateErr = ErrPaymentAlreadyCompleted

		case paymentStatus == StatusFailed:
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

// FetchPaymentStatus fetches the payment status from the bucket.  If the
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

type InFlightPayment struct {
	PaymentHash lntypes.Hash

	// might be nil
	Attempt *AttemptInfo
}

// FetchInFlightPayments returns all payments with status InFlight.
func (p *paymentControl) FetchInFlightPayments() ([]*InFlightPayment, error) {
	var inFlights []*InFlightPayment
	err := p.db.View(func(tx *bbolt.Tx) error {
		payments := tx.Bucket(outgoingPaymentBucket)
		if payments == nil {
			return nil
		}

		return payments.ForEach(func(k, _ []byte) error {
			bucket := payments.Bucket(k)
			if bucket == nil {
				return fmt.Errorf("non bucket element")
			}

			var paymentStatus = StatusGrounded
			paymentStatusBytes := bucket.Get(paymentStatusKey)
			if paymentStatusBytes != nil {
				paymentStatus.FromBytes(paymentStatusBytes)
			}

			paymentHash, err := lntypes.MakeHash(k)
			if err != nil {
				return err
			}

			if paymentStatus != StatusInFlight {
				return nil
			}

			inFlight := &InFlightPayment{
				PaymentHash: paymentHash,
			}

			attempt := bucket.Get(paymentAttemptInfoKey)
			if attempt != nil {
				a := &AttemptInfo{}
				r := bytes.NewReader(attempt)
				a.Route, err = deserializeRoute(r)
				if err != nil {
					return err
				}

				err = ReadElements(r, &a.PaymentID)
				if err != nil {
					return err
				}

				inFlight.Attempt = a
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

func serializeHop(w io.Writer, h *route.Hop) error {
	if err := WriteElements(w,
		h.PubKeyBytes[:], h.ChannelID, h.OutgoingTimeLock,
		h.AmtToForward,
	); err != nil {
		return err
	}

	return nil
}

func deserializeHop(r io.Reader) (*route.Hop, error) {
	h := &route.Hop{}

	var pub []byte
	if err := ReadElements(r, &pub); err != nil {
		return nil, err
	}
	copy(h.PubKeyBytes[:], pub)

	if err := ReadElements(r,
		&h.ChannelID, &h.OutgoingTimeLock, &h.AmtToForward,
	); err != nil {
		return nil, err
	}

	return h, nil
}

func serializeRoute(w io.Writer, r *route.Route) error {
	if err := WriteElements(w,
		r.TotalTimeLock, r.TotalFees, r.TotalAmount, r.SourcePubKey[:],
	); err != nil {
		return err
	}

	if err := WriteElements(w, uint32(len(r.Hops))); err != nil {
		return err
	}

	for _, h := range r.Hops {
		if err := serializeHop(w, h); err != nil {
			return err
		}
	}

	return nil
}

func deserializeRoute(r io.Reader) (*route.Route, error) {
	rt := &route.Route{}
	if err := ReadElements(r,
		&rt.TotalTimeLock, &rt.TotalFees, &rt.TotalAmount,
	); err != nil {
		return nil, err
	}

	var pub []byte
	if err := ReadElements(r, &pub); err != nil {
		return nil, err
	}
	copy(rt.SourcePubKey[:], pub)

	var numHops uint32
	if err := ReadElements(r, &numHops); err != nil {
		return nil, err
	}

	var hops []*route.Hop
	for i := uint32(0); i < numHops; i++ {
		hop, err := deserializeHop(r)
		if err != nil {
			return nil, err
		}
		hops = append(hops, hop)
	}
	rt.Hops = hops

	return rt, nil
}
