package htlcswitch

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/coreos/bbolt"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (

	// pendingPaymentBucketKey is used for the root level bucket that
	// stores the sub buckets for each pending payment.
	pendingPaymentBucketKey = []byte("pending-payment-store-bucket")

	// pendingPaymentKey is used to index the serialized pending payment in
	// the bucket for the given paymentID.
	pendingPaymentKey = []byte("pending-payment-store-pendingpayment-key")

	// successKey is used to index a PaymentSuccess.
	successKey = []byte("pending-payment-success")

	// failureKey is used to index a PaymentFailure.
	failureKey = []byte("pending-payment-failure")

	// ErrPaymentIDNotFound is an error returned if the given paymentID is
	// not found.
	ErrPaymentIDNotFound = errors.New("paymentID not found")

	// ErrPaymentIDAlreadyExits is returned if we try to write a pending
	// payment whose paymentID already exists.
	ErrPaymentIDAlreadyExists = errors.New("paymentID already exists")
)

// PaymentResult wraps a result received from the network after a payment
// attempt was made.
type PaymentResult interface {
	// Encode serializes the PaymentResult.
	Encode(w io.Writer) error

	// Decode deserializes the PaymentResult.
	Decode(r io.Reader) error
}

// PaymentFailure is returned by the switch in case a HTLC send failed, and the
// HTLC is now irrevocably cancelled.
type PaymentFailure struct {
	Error *ForwardingError
}

// Encode serializes the PaymentFailure.
//
// NOTE: Part of the PaymentResult interface.
func (p *PaymentFailure) Encode(w io.Writer) error {
	return channeldb.WriteElements(w,
		p.Error.ErrorSource, p.Error.ExtraMsg,
		p.Error.FailureMessage,
	)
}

// Decode deserializes the PaymentFailure.
//
// NOTE: Part of the PaymentResult interface.
func (p *PaymentFailure) Decode(r io.Reader) error {
	return channeldb.ReadElements(r,
		&p.Error.ErrorSource, &p.Error.ExtraMsg,
		&p.Error.FailureMessage,
	)
}

// A compile time check to ensure PaymentFailure implements the PaymentResult
// interface.
var _ PaymentResult = (*PaymentFailure)(nil)

// PaymentSuccess is returned by the switch in case a sent HTLC was settled,
// and wraps the obtained preimage.
type PaymentSuccess struct {
	Preimage [32]byte
}

// Encode serializes the PaymentSuccess.
//
// NOTE: Part of the PaymentResult interface.
func (p *PaymentSuccess) Encode(w io.Writer) error {
	return channeldb.WriteElements(w,
		p.Preimage[:],
	)
}

// Decode deserializes the PaymentSuccess.
//
// NOTE: Part of the PaymentResult interface.
func (p *PaymentSuccess) Decode(r io.Reader) error {
	return channeldb.ReadElements(r,
		p.Preimage[:],
	)
}

// A compile time check to ensure PaymentFailure implements the PaymentResult
// interface.
var _ PaymentResult = (*PaymentSuccess)(nil)

// pendingPaymentStore is a persistent store that tracks HTLCs in flight on the
// network. Since payment results are inherently asynchronous, it is used as a
// common access point for senders of HTLCs. The Switch will checkpoint any
// sent HTLC to the store, and any received result. The store will keep results
// and notify the callers about them
type pendingPaymentStore struct {
	db *channeldb.DB

	// results is a map from paymentIDs to channels where subscribers to
	// payment results will be notified.
	results    map[uint64][]chan PaymentResult
	resultsMtx sync.Mutex
}

func newPendingPaymentStore(db *channeldb.DB) *pendingPaymentStore {
	return &pendingPaymentStore{
		db:      db,
		results: make(map[uint64][]chan PaymentResult),
	}
}

// PendingPayment houses the information necessary for forwarding a payment
// attempt on the network. Each payment attempt is peristed to the store, such
// that it can be replayed on startup, ensuring it was forwarded.
type PendingPayment struct {
	paymentID uint64
	firstHop  lnwire.ShortChannelID
	htlcAdd   *lnwire.UpdateAddHTLC
	circuit   *sphinx.Circuit
}

func serializePendingPayment(w io.Writer, p *PendingPayment) error {
	if err := channeldb.WriteElements(w,
		p.paymentID, p.firstHop,
	); err != nil {
		return err
	}

	if err := p.htlcAdd.Encode(w, 0); err != nil {
		return err
	}

	if err := p.circuit.Encode(w); err != nil {
		return err
	}

	return nil
}

func deserializePendingPayment(r io.Reader) (*PendingPayment, error) {
	p := &PendingPayment{}
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

	circuit := &sphinx.Circuit{}
	if err := circuit.Decode(r); err != nil {
		return nil, err
	}
	p.circuit = circuit

	return p, nil
}

// initiatePayment adds the PendingPayment to the store, indexed by the
// paymentID. If the paymentID already exists, ErrPaymentIDAlreadyExists will be
// returned.
func (store *pendingPaymentStore) initiatePayment(p *PendingPayment) error {
	var b bytes.Buffer
	if err := serializePendingPayment(&b, p); err != nil {
		return err
	}

	var paymentIDBytes [8]byte
	binary.BigEndian.PutUint64(paymentIDBytes[:], p.paymentID)

	var existingErr error
	err := store.db.Batch(func(tx *bbolt.Tx) error {
		existingErr = nil

		pendingPayments, err := tx.CreateBucketIfNotExists(
			pendingPaymentBucketKey,
		)
		if err != nil {
			return err
		}

		// If the bucket already exists, we return.
		bucket, err := pendingPayments.CreateBucket(paymentIDBytes[:])
		if err != nil {
			existingErr = ErrPaymentIDAlreadyExists
			return nil
		}

		return bucket.Put(pendingPaymentKey, b.Bytes())
	})

	if err != nil {
		return err
	}
	return existingErr
}

// completePayment stores the PaymentResult for the given paymentID, and
// notifies any subscribers.
func (store *pendingPaymentStore) completePayment(paymentID uint64,
	result PaymentResult) error {

	// Serialize the payment result.
	var b bytes.Buffer
	if err := result.Encode(&b); err != nil {
		return err
	}

	// Put the result in the bucket indexed by the result type.
	var typeKey []byte
	switch result := result.(type) {
	case *PaymentFailure:
		typeKey = failureKey

	case *PaymentSuccess:
		typeKey = successKey

	default:
		return fmt.Errorf("unknown result type: %T", result)
	}

	var paymentIDBytes [8]byte
	binary.BigEndian.PutUint64(paymentIDBytes[:], paymentID)

	err := store.db.Batch(func(tx *bbolt.Tx) error {
		pendingPayments := tx.Bucket(pendingPaymentBucketKey)
		if pendingPayments == nil {
			return ErrPaymentIDNotFound
		}

		// The payment must already be in the DB.
		bucket := pendingPayments.Bucket(paymentIDBytes[:])
		if bucket == nil {
			return ErrPaymentIDNotFound
		}

		return bucket.Put(typeKey, b.Bytes())
	})
	if err != nil {
		return err
	}

	// Now that the result is stored in the database, we can notify any
	// active subscribers.
	store.resultsMtx.Lock()
	for _, res := range store.results[paymentID] {
		res <- result
	}
	delete(store.results, paymentID)
	store.resultsMtx.Unlock()

	return nil
}

// DeletePayment deletes the payment with the given paymentID from the
// database. It only allows deleting payments where the preimage is not known,
// and should only be used to delete payments that failed.
//func (store *pendingPaymentStore) DeletePayment(pid uint64) error {
//	return store.db.Batch(func(tx *bbolt.Tx) error {
//		preimages := tx.Bucket(pendingPaymentBucketKey)
//		if preimages == nil {
//			return nil
//		}
//
//		paymentIDBytes := make([]byte, 8)
//		binary.BigEndian.PutUint64(paymentIDBytes, pid)
//
//		// Ensure the payment exists, and that the preimage is not
//		// known.
//		v := preimages.Bucket(paymentIDBytes)
//		if v == nil {
//			return ErrPaymentIDNotFound
//		}
//
//		return preimages.DeleteBucket(paymentIDBytes)
//	})
//}

// getPayment is used to query the store for a pending payment for the given
// payment ID. If the pid is not found, ErrPaymentIDNotFound will be returned.
func (store *pendingPaymentStore) getPayment(pid uint64) (*PendingPayment,
	error) {

	var paymentIDBytes [8]byte
	binary.BigEndian.PutUint64(paymentIDBytes[:], pid)

	var p *PendingPayment
	err := store.db.View(func(tx *bbolt.Tx) error {
		pendingPayments := tx.Bucket(pendingPaymentBucketKey)
		if pendingPayments == nil {
			return ErrPaymentIDNotFound
		}

		bucket := pendingPayments.Bucket(paymentIDBytes[:])
		if bucket == nil {
			return ErrPaymentIDNotFound
		}

		pendingBytes := bucket.Get(pendingPaymentKey)
		if pendingBytes == nil {
			return ErrPaymentIDNotFound
		}

		var err error
		r := bytes.NewReader(pendingBytes)
		p, err = deserializePendingPayment(r)
		if err != nil {
			return err
		}

		return nil

	})
	if err != nil {
		return nil, err
	}

	return p, nil
}

// getPaymentResult is used to get the payment result for the given payment ID.
// It returns a channel on which the result will be delivered when ready. If
// the payment ID is not found in the store, ErrPaymentIDNotFound is returned.
func (store *pendingPaymentStore) getPaymentResult(pid uint64) (
	<-chan PaymentResult, error) {

	var paymentIDBytes [8]byte
	binary.BigEndian.PutUint64(paymentIDBytes[:], pid)

	resultChan := make(chan PaymentResult, 1)
	err := store.db.View(func(tx *bbolt.Tx) error {
		pendingPayments := tx.Bucket(pendingPaymentBucketKey)
		if pendingPayments == nil {
			return ErrPaymentIDNotFound
		}

		bucket := pendingPayments.Bucket(paymentIDBytes[:])
		if bucket == nil {
			return ErrPaymentIDNotFound
		}

		// Check whether a result is already available.
		var result PaymentResult = &PaymentSuccess{}
		resultBytes := bucket.Get(successKey)
		if resultBytes == nil {
			// If no success result was available, check for
			// failure.
			result = &PaymentFailure{}
			resultBytes = bucket.Get(failureKey)
		}

		if resultBytes == nil {
			// Result not yet available, we will notify once a
			// result is available.
			// NOTE: we do this inside the db transaction, to avoid
			// a race where the payment result is written to the db
			// between this db tx and we add the resultChan to the
			// map.
			store.resultsMtx.Lock()
			store.results[pid] = append(
				store.results[pid], resultChan,
			)
			store.resultsMtx.Unlock()
			return nil
		}

		// Decode the result we found, and send it on the result
		// channel.
		r := bytes.NewReader(resultBytes)
		if err := result.Decode(r); err != nil {
			return err
		}

		resultChan <- result
		return nil

	})
	if err != nil {
		return nil, err
	}

	return resultChan, nil
}

func (store *pendingPaymentStore) FetchPendingPayments() (
	[]*PendingPayment, error) {

	var ps []*PendingPayment
	err := store.db.View(func(tx *bbolt.Tx) error {
		preimages := tx.Bucket(preimageBucket)
		if preimages == nil {
			return nil
		}

		preimages.ForEach(func(k, v []byte) error {
			// Skip non-buckets.
			if v != nil {
				return nil
			}

			bucket := preimages.Bucket(k)
			if bucket == nil {
				// wat
				return nil
			}

			pendingBytes := bucket.Get(pendingPaymentKey)
			if pendingBytes == nil {
				return ErrPaymentIDNotFound
			}

			var err error
			r := bytes.NewReader(pendingBytes)
			p, err := deserializePayAttempt(r)
			if err != nil {
				return err
			}

			ps = append(ps, p)
			return nil
		})
		return nil

	})
	if err != nil {
		return nil, err
	}

	return ps, nil
}
