package routing

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/coreos/bbolt"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lntypes"
)

var (
	// paymentsBucketKey is the key used to access the top level bucket of
	// the payment state machine. This bucket contains sub-buckets indexed
	// by the payment hash of the active payments.
	paymentsBucketKey = []byte("router-payment-state-machine")

	// paymentStateKey is a key used in an active payment bucket to access
	// the current payment state.
	paymentStateKey = []byte("payment-state-key")

	// paymentAttemptKey is used to access the information of the current
	// payment attempt sent to the switch. This key is only populated when
	// the payment is in the state paymentStateInFlight.
	paymentIDKey    = []byte("payment-id-key")
	paymentRouteKey = []byte("payment-route-key")

	// ErrNotFound is an error returned if the given payment hash was not
	// found.
	ErrNotFound = errors.New("payment hash not found")
)

// paymentStateMachine is the persistent state machine used by the
// ChannelRouter to track the progress of active payments. It is used to keep
// information across restarts, needed to properly retry failed paymets, and
// handle payment errors and successes encountered for a payment sent before
// the ChannelRouter was restarted.
type paymentStateMachine struct {
	db *channeldb.DB
}

// paymentState is the type used to indicate which state a given payment has in
// the payment state machine.
type paymentState byte

const (
	// paymentStateInFlight is the state a payment enters after we have
	// crafted a paymentAttempt for it and sent it to the Switch. In this
	// state we expect to find the paymentAttempt in the DB, such that we
	// have the information necessary to handle any results received from
	// the Switch. After a restart the paymentAttempt can poentially have
	// been lost by the Switch, in case we can go back to the initial state
	// and craft a new paymentAttempt to retry.
	paymentStateInFlight paymentState = 1

	// paymentStateSuccess is the state a payment enters after it has
	// completed successfully. In this state we expect to find the
	// completed OutgoingPayment in the payments DB. The payment is kept in
	// this state by the payment state machine to avoid mistakenly
	// resending a payment to the same payment hash.
	paymentStateSuccess paymentState = 2

	// paymentStateFailed is the state a payment enters when it has failed
	// permanently. In this state we will find the error encounterd in the
	// DB. In this state it is safe to attempt to resend a payment to the
	// same payment hash.
	paymentStateFailed paymentState = 3

	// TODO(halseth): timeout/cancel state?
)

// initPayment registers the given payment hash with the payment state machine.
// It ensures that the hash is either new, or that the existing payment to this
// hash is in a state where re-sending it is safe.
//
// When this method returns the payment will be in the state
// paymentStateIntended.
func (s *paymentStateMachine) initPayment(pHash lntypes.Hash) error {
	var batchErr error
	err := s.db.Batch(func(tx *bbolt.Tx) error {
		bucket, err := s.getWriteBucket(tx, pHash)
		if err != nil {
			return err
		}

		// Reset the error, to avoid carrying over an error from a
		// previous execution of the batched db transaction.
		batchErr = nil

		// Check if this pHash already has a payment state. We cannot
		// move it to the intended state if it is already in flight.
		// Only allow it if this is the first time, or it failed
		// earlier.
		v := bucket.Get(paymentStateKey)
		if v != nil {
			state := paymentState(v[0])
			switch state {

			// If the payment is in a permanent failure state, it
			// is safe to re-init and retry the payment.
			case paymentStateFailed:
				break

			// Disallow payment to a hash already paid.
			case paymentStateSuccess:
				// Reuse ControlTower errors in case callers
				// are expecting those errors.
				batchErr = htlcswitch.ErrAlreadyPaid
				return nil

			// Otherwise the payment is in a state where it is
			// already in-flight.
			default:
				// Reuse ControlTower errors in case callers
				// are expecting those errors.
				batchErr = htlcswitch.ErrPaymentInFlight
				return nil
			}
		}

		// Add the payment to the payments bucket, which will make it
		// show up in "listpayments".
		if err := s.db.AddPaymentTx(tx, pHash); err != nil {
			return err
		}

		return s.putPaymentState(bucket, paymentStateInFlight)
	})
	if err != nil {
		return err
	}

	return batchErr
}

func (s *paymentStateMachine) storeRoute(pHash lntypes.Hash, paymentID uint64,
	route *Route) error {

	// Serialize the information before opening the db transaction.
	var routeBuf bytes.Buffer
	if err := serializeRoute(&routeBuf, route); err != nil {
		return err
	}

	var pidBuf bytes.Buffer
	if err := channeldb.WriteElements(&pidBuf, paymentID); err != nil {
		return err
	}

	return s.db.Batch(func(tx *bbolt.Tx) error {
		bucket, err := s.getWriteBucket(tx, pHash)
		if err != nil {
			return err
		}

		// We put the serialized information under the
		// paymentAttemptKey, such that it can be fetched when a result
		// for this payment attempt comes back.
		if err := bucket.Put(paymentRouteKey, routeBuf.Bytes()); err != nil {
			return err
		}

		return bucket.Put(paymentIDKey, pidBuf.Bytes())
	})
}

func (s *paymentStateMachine) fetchRoute(pHash lntypes.Hash) (
	*Route, error) {

	var route *Route
	err := s.db.View(func(tx *bbolt.Tx) error {
		paymentsBucket := tx.Bucket(paymentsBucketKey)
		if paymentsBucket == nil {
			return ErrNotFound
		}

		bucket := paymentsBucket.Bucket(pHash[:])
		if bucket == nil {
			return ErrNotFound
		}

		v := bucket.Get(paymentRouteKey)
		if v == nil {
			return ErrNotFound
		}

		var err error
		r := bytes.NewReader(v)
		route, err = deserializeRoute(r)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return route, nil
}

// setPaymentFailed records the given error and deletes the current
// paymentAttempt from the DB. A payment in this state is safe to attempt to
// resend. It is kept in the database in this state to be available for
// historical lookups.
//
// When this method returns the payment will be in the state
// paymentStateFailed.
func (s *paymentStateMachine) setPaymentFailed(pHash lntypes.Hash) error {
	// Go to the failed state, where new payment attempts are allowed.
	return s.db.Batch(func(tx *bbolt.Tx) error {
		bucket, err := s.getWriteBucket(tx, pHash)
		if err != nil {
			return err
		}

		err = s.putPaymentState(bucket, paymentStateFailed)
		if err != nil {
			return err
		}

		// Since a result for the payment attempt came back, and we are
		// moving to the next state, we can safely
		// remove the previous paymentAttempt information.
		return bucket.Delete(paymentRouteKey)
	})
}

// setPaymentSuccess records the successful payment in the database, and removes
// any associated errors and payment attempts. The payment is kept in the
// database in this state to avoid re-sending a payment to the same payment
// hash.
//
// When this method returns the payment will be in the state
// paymentStateSuccess.
func (s *paymentStateMachine) setPaymentSuccess(pHash lntypes.Hash,
	payment *channeldb.OutgoingPayment) error {

	// To avoid sending any payments to this payment hash again, keep the
	// payment in the success state in the DB.
	nextState := paymentStateSuccess
	err := s.db.Batch(func(tx *bbolt.Tx) error {
		bucket, err := s.getWriteBucket(tx, pHash)
		if err != nil {
			return err
		}

		err = s.putPaymentState(bucket, nextState)
		if err != nil {
			return err
		}

		// Since we successfully received what we need to consider this
		// payment done, we can delete any previous payment errors and
		// payment attempts from the database.
		if err := bucket.Delete(paymentRouteKey); err != nil {
			return err
		}

		// Finally, record the successful payment atomically to the
		// payments record.
		return s.db.CompletePaymentTx(tx, payment)
	})
	if err != nil {
		return err
	}

	return nil
}

type inFlightPayment struct {
	hash lntypes.Hash
	pid  uint64
}

// fetchActivePayments fetches all payments that are active (in states
// paymentStateIntended or paymentStateInFlight).
func (s *paymentStateMachine) fetchActivePayments() ([]inFlightPayment, error) {
	var payments []inFlightPayment

	err := s.db.View(func(tx *bbolt.Tx) error {
		paymentsBucket := tx.Bucket(paymentsBucketKey)
		if paymentsBucket == nil {
			return nil
		}

		return paymentsBucket.ForEach(func(k, _ []byte) error {
			var pHash lntypes.Hash
			copy(pHash[:], k[:])

			bucket := paymentsBucket.Bucket(k)
			if bucket == nil {
				return fmt.Errorf("could not get bucket")
			}

			v := bucket.Get(paymentStateKey)
			if v == nil {
				return fmt.Errorf("no payment state set for "+
					"hash %v", pHash)
			}

			state := paymentState(v[0])

			// Only return payments in states that should be
			// retried.
			if state == paymentStateInFlight {
				v := bucket.Get(paymentIDKey)
				if v == nil {
					return ErrNotFound
				}

				var pid uint64
				r := bytes.NewReader(v)
				err := channeldb.ReadElements(r, &pid)
				if err != nil {
					return err
				}

				payments = append(payments, inFlightPayment{pHash, pid})
			}

			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	return payments, nil
}

func (s *paymentStateMachine) putPaymentState(bucket *bbolt.Bucket,
	state paymentState) error {

	return bucket.Put(paymentStateKey, []byte{byte(state)})
}

func (s *paymentStateMachine) getWriteBucket(tx *bbolt.Tx,
	pHash lntypes.Hash) (*bbolt.Bucket, error) {

	paymentsBucket, err := tx.CreateBucketIfNotExists(paymentsBucketKey)
	if err != nil {
		return nil, err
	}

	bucket, err := paymentsBucket.CreateBucketIfNotExists(pHash[:])
	if err != nil {
		return nil, err
	}

	return bucket, nil
}
