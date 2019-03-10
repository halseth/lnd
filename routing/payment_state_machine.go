package routing

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/coreos/bbolt"
	"github.com/lightningnetwork/lnd/channeldb"
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
	// the payment is in the state paymentStateAttemptSent.
	paymentAttemptKey = []byte("payment-attempt-key")

	// paymentErrorKey is a key that accesses the last encountered error
	// for payment attempts for the active payment. It is populated when
	// the active payment is in the state paymentStateFailed, and sometimes
	// in other states if the previous payment attempt failed.
	paymentErrorKey = []byte("payment-error-key")

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
	// paymentStateIntended is the starting state of a payment. A payment
	// in this state is intended for sending, but haven't yet had a
	// paymentAttempt sent to the Switch..
	paymentStateIntended paymentState = 1

	// paymentStateAttemptSent is the state a payment enters after we have
	// crafted a paymentAttempt for it and sent it to the Switch. In this
	// state we expect to find the paymentAttempt in the DB, such that we
	// have the information necessary to handle any results received from
	// the Switch. After a restart the paymentAttempt can poentially have
	// been lost by the Switch, in case we can go back to the initial state
	// and craft a new paymentAttempt to retry.
	paymentStateAttemptSent paymentState = 2

	// paymentStateSuccess is the state a payment enters after it has
	// completed successfully. In this state we expect to find the
	// completed OutgoingPayment in the payments DB. The payment is kept in
	// this state by the payment state machine to avoid mistakenly
	// resending a payment to the same payment hash.
	paymentStateSuccess paymentState = 3

	// paymentStateFailed is the state a payment enters when it has failed
	// permanently. In this state we will find the error encounterd in the
	// DB. In this state it is safe to attempt to resend a payment to the
	// same payment hash.
	paymentStateFailed paymentState = 4

	// TODO(halseth): timeout/cancel state?
)

// initPayment registers the given payment hash with the payment state machine.
// It ensures that the hash is either new, or that the existing payment to this
// hash is in a state where re-sending it is safe.
//
// When this method returns the payment will be in the state
// paymentStateIntended.
func (s *paymentStateMachine) initPayment(pHash lntypes.Hash) error {
	return s.db.Batch(func(tx *bbolt.Tx) error {
		bucket, err := s.getWriteBucket(tx, pHash)
		if err != nil {
			return err
		}

		// Check if this pHash already has a payment state. We cannot
		// move it to the intended state if it is already in flight.
		// Only allow it if this is the first time, or it failed
		// earlier.
		v := bucket.Get(paymentStateKey)
		if v != nil {
			state := paymentState(v[0])
			if state != paymentStateFailed {
				return fmt.Errorf("cannot re-init payment "+
					"in state %v", state)
			}
		}

		return s.putPaymentState(bucket, paymentStateIntended)
	})
}

// setStateAttemptSent moves the payment to the state paymentStateAttemptSent,
// and stores the given paymentID and route for use in case a restart happens
// before the result of the payment is back.
//
// When this method returns the payment will be in the state
// paymentStateAttemptSent.
func (s *paymentStateMachine) setStateAttemptSent(pHash lntypes.Hash,
	paymentID uint64, route *Route) (paymentState, error) {

	// Serialize the information before opening the db transaction.
	var b bytes.Buffer
	if err := channeldb.WriteElements(&b, paymentID); err != nil {
		return 0, err
	}

	if err := serializeRoute(&b, route); err != nil {
		return 0, err
	}

	nextState := paymentStateAttemptSent
	err := s.db.Batch(func(tx *bbolt.Tx) error {
		bucket, err := s.getWriteBucket(tx, pHash)
		if err != nil {
			return err
		}

		err = s.putPaymentState(bucket, nextState)
		if err != nil {
			return err
		}

		// We put the serialized information under the
		// paymentAttemptKey, such that it can be fetched when a result
		// for this payment attempt comes back.
		return bucket.Put(paymentAttemptKey, b.Bytes())
	})
	if err != nil {
		return 0, err
	}

	return nextState, nil
}

// setStateIntermediateFailure records the given error and deletes the current
// paymentAttempt from the DB. When this method returns a new paymentAttempt
// can be created and sent to the Switch.
//
// When this method returns the payment will be in the state
// paymentStateIntended.
func (s *paymentStateMachine) setStateIntermediateFailure(pHash lntypes.Hash,
	paymentErr error) (paymentState, error) {

	// We store the error as a string.
	errMsg := []byte(paymentErr.Error())

	// Go back to initial state, to allow new payment attempts to be made.
	nextState := paymentStateIntended
	err := s.db.Batch(func(tx *bbolt.Tx) error {
		bucket, err := s.getWriteBucket(tx, pHash)
		if err != nil {
			return err
		}

		err = s.putPaymentState(bucket, nextState)
		if err != nil {
			return err
		}

		// Since a result for the payment attempt came back, and we are
		// moving back to the paymentStateIntended state, we can safely
		// remove the previous paymentAttempt information.
		if err := bucket.Delete(paymentAttemptKey); err != nil {
			return err
		}

		// Store the error for record keeping.
		return bucket.Put(paymentErrorKey, errMsg)
	})
	if err != nil {
		return 0, err
	}

	return nextState, nil
}

// setStateTerminalFailure records the given error and deletes the current
// paymentAttempt from the DB. A payment in this state is safe to attempt to
// resend. It is kept in the database in this state to be available for
// historical lookups.
//
// When this method returns the payment will be in the state
// paymentStateFailed.
func (s *paymentStateMachine) setStateTerminalFailure(pHash lntypes.Hash,
	paymentErr error) (paymentState, error) {

	// We store the error as a string.
	errMsg := []byte(paymentErr.Error())

	// Go to the failed state, where new payment attempts are allowed.
	nextState := paymentStateFailed
	err := s.db.Batch(func(tx *bbolt.Tx) error {
		bucket, err := s.getWriteBucket(tx, pHash)
		if err != nil {
			return err
		}

		err = s.putPaymentState(bucket, nextState)
		if err != nil {
			return err
		}

		// Since a result for the payment attempt came back, and we are
		// moving to the paymentStateFailed state, we can safely
		// remove the previous paymentAttempt information.
		if err := bucket.Delete(paymentAttemptKey); err != nil {
			return err
		}

		// Store the error for record keeping.
		return bucket.Put(paymentErrorKey, errMsg)
	})
	if err != nil {
		return 0, err
	}

	return nextState, nil
}

// setStateSuccess records the successful payment in the database, and removes
// any associated errors and payment attempts. The payment is kept in the
// database in this state to avoid re-sending a payment to the same payment
// hash.
//
// When this method returns the payment will be in the state
// paymentStateSuccess.
func (s *paymentStateMachine) setStateSuccess(pHash lntypes.Hash,
	payment *channeldb.OutgoingPayment) (paymentState, error) {

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
		if err := bucket.Delete(paymentErrorKey); err != nil {
			return err
		}

		if err := bucket.Delete(paymentAttemptKey); err != nil {
			return err
		}

		// Finally, record the successful payment atomically to the
		// payments record.
		return s.db.AddPaymentTx(tx, payment)
	})
	if err != nil {
		return 0, err
	}

	return nextState, nil
}

// fetchPaymentState fetches the payment state for the given payment hash from
// the database.
func (s *paymentStateMachine) fetchPaymentState(pHash lntypes.Hash) (
	paymentState, error) {

	var state paymentState
	err := s.db.View(func(tx *bbolt.Tx) error {
		paymentsBucket := tx.Bucket(paymentsBucketKey)
		if paymentsBucket == nil {
			return ErrNotFound
		}

		bucket := paymentsBucket.Bucket(pHash[:])
		if bucket == nil {
			return ErrNotFound
		}

		v := bucket.Get(paymentStateKey)
		if v == nil {
			return fmt.Errorf("no payment state set for hash %v",
				pHash)
		}

		state = paymentState(v[0])
		return nil
	})
	if err != nil {
		return 0, err
	}

	return state, nil
}

// fetchPaymentAttempt fetches the information associated with the current
// payment attempt from the database. It will only be found when the payment is
// in the state paymentStateAttemptSent.
func (s *paymentStateMachine) fetchPaymentAttempt(pHash lntypes.Hash) (
	uint64, *Route, error) {

	var (
		paymentID uint64
		route     *Route
	)

	err := s.db.View(func(tx *bbolt.Tx) error {
		paymentsBucket := tx.Bucket(paymentsBucketKey)
		if paymentsBucket == nil {
			return ErrNotFound
		}

		bucket := paymentsBucket.Bucket(pHash[:])
		if bucket == nil {
			return ErrNotFound
		}

		v := bucket.Get(paymentAttemptKey)
		if v == nil {
			return ErrNotFound
		}

		r := bytes.NewReader(v)
		err := channeldb.ReadElements(r, &paymentID)
		if err != nil {
			return err
		}

		route, err = deserializeRoute(r)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return 0, nil, err
	}

	return paymentID, route, nil
}

// fetchLastError fetches the last recorded error for this payment from the
// database.
func (s *paymentStateMachine) fetchLastError(pHash lntypes.Hash) (
	error, error) {

	var lastError error
	err := s.db.View(func(tx *bbolt.Tx) error {
		paymentsBucket := tx.Bucket(paymentsBucketKey)
		if paymentsBucket == nil {
			return ErrNotFound
		}

		bucket := paymentsBucket.Bucket(pHash[:])
		if bucket == nil {
			return ErrNotFound
		}

		v := bucket.Get(paymentErrorKey)
		if v == nil {
			return ErrNotFound
		}

		lastError = errors.New(string(v))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return lastError, nil
}

// fetchActivePayments fetches all payments that are active (in states
// paymentStateIntended or paymentStateAttemptSent).
func (s *paymentStateMachine) fetchActivePayments() ([]lntypes.Hash, error) {
	var payments []lntypes.Hash

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
			if state == paymentStateIntended || state == paymentStateAttemptSent {
				payments = append(payments, pHash)
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
