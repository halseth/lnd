package htlcswitch

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/coreos/bbolt"
	"github.com/go-errors/errors"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (

	// maps: pid -> preimage
	preimageBucket = []byte("pid-preimages")

	pendingPaymentKey = []byte("pid-pending-payment")
	//
	successKey = []byte("pid-success")
	failureKey = []byte("pid-failure")

	// ErrPaymentIDNotFound is an error returned if we cannot find the
	// given paymentID in the database.
	ErrPaymentIDNotFound = errors.New("paymentID not found")

	// ErrPreimageKnown is an error returned if we try to complete or
	// delete a payment whose preimage is already set in the database.
	ErrPreimageKnown = errors.New("preimage was already found in the" +
		"database")

	pendingIndicator = []byte{}
)

type pendingPaymentStore struct {
	db *channeldb.DB

	results map[uint64][]chan paymentResult
}

func newPendingPaymentStore(db *channeldb.DB) *pendingPaymentStore {
	return &pendingPaymentStore{
		db:      db,
		results: make(map[uint64][]chan paymentResult),
	}
}

type PendingPayment struct {
	PaymentID uint64
	FirstHop  lnwire.ShortChannelID
	HtlcAdd   *lnwire.UpdateAddHTLC
	Circuit   *sphinx.Circuit
}

func serializePayAttempt(w io.Writer, p *PendingPayment) error {
	if err := channeldb.WriteElements(w,
		p.PaymentID, p.FirstHop,
	); err != nil {
		return err
	}

	if err := p.HtlcAdd.Encode(w, 0); err != nil {
		return err
	}

	if err := p.Circuit.Encode(w); err != nil {
		return err
	}

	return nil
}

func deserializePayAttempt(r io.Reader) (*PendingPayment, error) {
	p := &PendingPayment{}
	if err := channeldb.ReadElements(r,
		&p.PaymentID, &p.FirstHop,
	); err != nil {
		return nil, err
	}

	htlc := &lnwire.UpdateAddHTLC{}
	if err := htlc.Decode(r, 0); err != nil {
		return nil, err
	}
	p.HtlcAdd = htlc

	circuit := &sphinx.Circuit{}
	if err := circuit.Decode(r); err != nil {
		return nil, err
	}
	p.Circuit = circuit

	return p, nil
}

type paymentResult interface {
	Encode(w io.Writer) error
	Decode(r io.Reader) error
}

type paymentFailure struct {
	*ForwardingError
}

func (p *paymentFailure) Encode(w io.Writer) error {
	return channeldb.WriteElements(w,
		p.ErrorSource, p.ExtraMsg, p.FailureMessage,
	)
}

func (p *paymentFailure) Decode(r io.Reader) error {
	return channeldb.ReadElements(r,
		&p.ErrorSource, &p.ExtraMsg, &p.FailureMessage,
	)
}

type paymentSuccess struct {
	preimage [32]byte
}

func (p *paymentSuccess) Encode(w io.Writer) error {
	return channeldb.WriteElements(w,
		p.preimage[:],
	)
}

func (p *paymentSuccess) Decode(r io.Reader) error {
	return channeldb.ReadElements(r,
		p.preimage[:],
	)
}

func (store *pendingPaymentStore) InitiatePayment(paymentID uint64, p *PendingPayment) error {

	var existingErr error
	err := store.db.Batch(func(tx *bbolt.Tx) error {
		existingErr = nil

		preimages, err := tx.CreateBucketIfNotExists(preimageBucket)
		if err != nil {
			return err
		}

		// We use BigEndian for keys as it orders keys in
		// ascending order. This allows bucket scans to order payments
		// in the order in which they were created.
		paymentIDBytes := make([]byte, 8)
		binary.BigEndian.PutUint64(paymentIDBytes, paymentID)

		// NB: should return error if already exists.
		bucket, err := preimages.CreateBucket(paymentIDBytes)
		if err != nil {
			return err
		}

		var b bytes.Buffer
		if err := serializePayAttempt(&b, p); err != nil {
			return err
		}

		return bucket.Put(pendingPaymentKey, b.Bytes())
	})
	if err != nil {
		return err
	}
	return existingErr
}

// CompletePayment adds the given preimage to the OutgoingPayment stored in the
// DB for the given paymentID.
func (store *pendingPaymentStore) CompletePayment(paymentID uint64, result paymentResult) error {
	return store.db.Batch(func(tx *bbolt.Tx) error {
		preimages := tx.Bucket(preimageBucket)
		if preimages == nil {
			return ErrPaymentIDNotFound
		}

		paymentIDBytes := make([]byte, 8)
		binary.BigEndian.PutUint64(paymentIDBytes, paymentID)

		// The payment must already be in the DB.
		bucket := preimages.Bucket(paymentIDBytes)
		if bucket == nil {
			return ErrPaymentIDNotFound
		}

		var b bytes.Buffer
		if err := result.Encode(&b); err != nil {
			return err
		}

		switch result := result.(type) {
		case *paymentFailure:
			err := bucket.Put(failureKey, b.Bytes())
			if err != nil {
				return err
			}

		case *paymentSuccess:
			err := bucket.Put(successKey, b.Bytes())
			if err != nil {
				return err
			}

		default:
			return fmt.Errorf("unknown result type: %T", result)
		}

		// TODO: can move out of db tx?
		for _, res := range store.results[paymentID] {
			res <- result
		}
		delete(store.results, paymentID)
		return nil
	})
}

// DeletePayment deletes the payment with the given paymentID from the
// database. It only allows deleting payments where the preimage is not known,
// and should only be used to delete payments that failed.
func (store *pendingPaymentStore) DeletePayment(pid uint64) error {
	return store.db.Batch(func(tx *bbolt.Tx) error {
		preimages := tx.Bucket(preimageBucket)
		if preimages == nil {
			return nil
		}

		paymentIDBytes := make([]byte, 8)
		binary.BigEndian.PutUint64(paymentIDBytes, pid)

		// Ensure the payment exists, and that the preimage is not
		// known.
		v := preimages.Bucket(paymentIDBytes)
		if v == nil {
			return ErrPaymentIDNotFound
		}

		return preimages.DeleteBucket(paymentIDBytes)
	})
}

func (store *pendingPaymentStore) GetPayment(pid uint64) (
	*PendingPayment, error) {

	var p *PendingPayment
	err := store.db.View(func(tx *bbolt.Tx) error {
		preimages := tx.Bucket(preimageBucket)
		if preimages == nil {
			return ErrPaymentIDNotFound
		}

		paymentIDBytes := make([]byte, 8)
		binary.BigEndian.PutUint64(paymentIDBytes, pid)

		// Ensure the payment exists, and that the preimage is not
		// known.
		bucket := preimages.Bucket(paymentIDBytes)
		if bucket == nil {
			return ErrPaymentIDNotFound
		}

		pendingBytes := bucket.Get(pendingPaymentKey)
		if pendingBytes == nil {
			return ErrPaymentIDNotFound
		}

		var err error
		r := bytes.NewReader(pendingBytes)
		p, err = deserializePayAttempt(r)
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

func (store *pendingPaymentStore) GetPaymentResult(pid uint64) (
	<-chan paymentResult, error) {

	resultChan := make(chan paymentResult, 1)
	err := store.db.View(func(tx *bbolt.Tx) error {
		preimages := tx.Bucket(preimageBucket)
		if preimages == nil {
			return ErrPaymentIDNotFound
		}

		paymentIDBytes := make([]byte, 8)
		binary.BigEndian.PutUint64(paymentIDBytes, pid)

		// Ensure the payment exists, and that the preimage is not
		// known.
		bucket := preimages.Bucket(paymentIDBytes)
		if bucket == nil {
			return ErrPaymentIDNotFound
		}

		var result paymentResult = &paymentSuccess{}
		resultBytes := bucket.Get(successKey)
		if resultBytes == nil {
			result = &paymentFailure{}
			resultBytes = bucket.Get(failureKey)
		}

		if resultBytes == nil {
			// Result not yet available.
			store.results[pid] = append(store.results[pid], resultChan)
			return nil
		}

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
