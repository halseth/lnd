package htlcswitch

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"sync"

	"github.com/coreos/bbolt"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (

	// paymentStoreBucketKey is used for the root level bucket that
	// stores the payment result for each payment ID.
	paymentStoreBucketKey = []byte("payment-result-store-bucket")

	// ErrPaymentIDNotFound is an error returned if the given paymentID is
	// not found.
	ErrPaymentIDNotFound = errors.New("paymentID not found")

	// ErrPaymentIDAlreadyExists is returned if we try to write a pending
	// payment whose paymentID already exists.
	ErrPaymentIDAlreadyExists = errors.New("paymentID already exists")
)

// PaymentResultType indicates whether this is a success, or some error.
type PaymentResultType uint32

const (
	// PaymenrResultSuccess means this payment succeeded, and the preimage
	// is set.
	PaymentResultSuccess PaymentResultType = 1

	// PaymentResultLocalError indicates that a error was encountered
	// locally, and the error Reason will be plaintext.
	PaymentResultLocalError PaymentResultType = 2

	// PaymentResultResolutionError indicates that the payment timed out
	// on-chain, and the channel had to be closed.
	PaymentResultResolutionError PaymentResultType = 3

	// PaymentResultEncryptedError indicates that a multi-hop payment
	// resulted in an error, and we'll need to use the session key to
	// decrypt it from the Reason.
	PaymentResultEncryptedError PaymentResultType = 4
)

// PaymentResult wraps a result received from the network after a payment
// attempt was made.
type PaymentResult struct {
	// Type encodes what kind of result the payment got.
	Type PaymentResultType

	// Preimage is set by the switch in case the Type is
	// PaymentResultSuccess.
	Preimage [32]byte

	// Reason is set for all other result types, and will encode the error
	// encountered.
	Reason lnwire.OpaqueReason
}

// serializePaymentResult serializes the PaymentResult.
func serializePaymentResult(w io.Writer, p *PaymentResult) error {
	var reason []byte = p.Reason
	if err := channeldb.WriteElements(w,
		uint32(p.Type), p.Preimage, reason[:],
	); err != nil {
		return err
	}
	return nil
}

// deserializePaymentResult deserializes the PaymentResult.
func deserializePaymentResult(r io.Reader) (*PaymentResult, error) {
	var (
		t      uint32
		preimg [32]byte
		reason []byte
	)
	if err := channeldb.ReadElements(r,
		&t, &preimg, &reason,
	); err != nil {
		return nil, err
	}

	p := &PaymentResult{
		Type: PaymentResultType(t),
	}
	copy(p.Preimage[:], preimg[:])

	if len(reason) > 0 {
		p.Reason = lnwire.OpaqueReason(reason)
	}

	return p, nil
}

// paymentResultStore is a persistent store that stores any results of HTLCs in
// flight on the network. Since payment results are inherently asynchronous, it
// is used as a common access point for senders of HTLCs, to know when a result
// is back. The Switch will checkpoint any and any received result to the
// store, and the store will keep results and notify the callers about them.
type paymentResultStore struct {
	db *channeldb.DB

	// results is a map from paymentIDs to channels where subscribers to
	// payment results will be notified.
	results    map[uint64][]chan *PaymentResult
	resultsMtx sync.Mutex
}

func newPaymentResultStore(db *channeldb.DB) *paymentResultStore {
	return &paymentResultStore{
		db:      db,
		results: make(map[uint64][]chan *PaymentResult),
	}
}

// storePaymentResult stores the PaymentResult for the given paymentID, and
// notifies any subscribers.
func (store *paymentResultStore) storePaymentResult(paymentID uint64,
	result *PaymentResult) error {

	// Serialize the payment result.
	var b bytes.Buffer
	if err := serializePaymentResult(&b, result); err != nil {
		return err
	}

	var paymentIDBytes [8]byte
	binary.BigEndian.PutUint64(paymentIDBytes[:], paymentID)

	err := store.db.Batch(func(tx *bbolt.Tx) error {
		paymentResults, err := tx.CreateBucketIfNotExists(
			paymentStoreBucketKey,
		)
		if err != nil {
			return err
		}

		return paymentResults.Put(paymentIDBytes[:], b.Bytes())
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

// subscribePaymentResult is used to get the payment result for the given
// payment ID. It returns a channel on which the result will be delivered when
// ready.
func (store *paymentResultStore) subscribePaymentResult(pid uint64) (
	<-chan *PaymentResult, error) {

	var (
		result     *PaymentResult
		err        error
		resultChan = make(chan *PaymentResult, 1)
	)

	err = store.db.View(func(tx *bbolt.Tx) error {
		result, err = fetchPaymentResult(tx, pid)
		switch {

		// Result not yet available, we will notify once a result is
		// available.
		case err == ErrPaymentIDNotFound:
			// NOTE: we do this inside the db transaction, to avoid
			// a race where the payment result is written to the db
			// between this db tx and we add the resultChan to the
			// map.
			// TODO: enought having the db mutex here?
			store.resultsMtx.Lock()
			store.results[pid] = append(
				store.results[pid], resultChan,
			)
			store.resultsMtx.Unlock()
			return nil

		case err != nil:
			return err

		// The result was found, and will be returned immediately.
		default:
			return nil
		}
	})
	if err != nil {
		return nil, err
	}

	// If the result was found, we can send it on the result channel
	// imemdiately.
	if result != nil {
		resultChan <- result
	}

	return resultChan, nil
}

func (store *paymentResultStore) getPaymentResult(pid uint64) (
	*PaymentResult, error) {

	var result *PaymentResult
	var err error

	err = store.db.View(func(tx *bbolt.Tx) error {
		result, err = fetchPaymentResult(tx, pid)
		return err
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}

func fetchPaymentResult(tx *bbolt.Tx, pid uint64) (*PaymentResult, error) {

	var paymentIDBytes [8]byte
	binary.BigEndian.PutUint64(paymentIDBytes[:], pid)

	paymentResults := tx.Bucket(paymentStoreBucketKey)
	if paymentResults == nil {
		return nil, ErrPaymentIDNotFound
	}

	// Check whether a result is already available.
	resultBytes := paymentResults.Get(paymentIDBytes[:])
	if resultBytes == nil {
		return nil, ErrPaymentIDNotFound
	}

	// Decode the result we found.
	r := bytes.NewReader(resultBytes)

	return deserializePaymentResult(r)
}
