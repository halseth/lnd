package routing

import (
	"bytes"
	"errors"

	"github.com/coreos/bbolt"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lntypes"
)

var (
	// paymentsBucketKey is the key used to access the top level bucket of
	// the payment store. This bucket contains sub-buckets indexed by the
	// payment hash of the active payments.
	paymentsBucketKey = []byte("router-payment-store")

	// paymentIDKey is used to access the paymentID of the current payment
	// attempt sent to the switch. This key is only populated when the
	// payment has status InFlight.
	paymentIDKey = []byte("payment-id-key")

	// paymentRouteKey is used to access the route of the current payment
	// attempt sent to the switch. This key is only populated when the
	// payment has status InFlight.
	paymentRouteKey = []byte("payment-route-key")

	// ErrNotFound is an error returned if the given payment hash was not
	// found.
	ErrNotFound = errors.New("payment hash not found")
)

// paymentStore is the persistent state machine used by the ChannelRouter to
// track information about active payments. It is used to keep information
// across restarts, needed to properly retry failed paymets, and handle payment
// errors and successes encountered for a payment sent before the ChannelRouter
// was restarted.
type paymentStore struct {
	db *channeldb.DB
}

// storeRoute checkpoints the current route and payment ID used before handing
// it over to the switch.
func (s *paymentStore) storeRoute(pHash lntypes.Hash, paymentID uint64,
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
		paymentsBucket, err := tx.CreateBucketIfNotExists(paymentsBucketKey)
		if err != nil {
			return err
		}

		bucket, err := paymentsBucket.CreateBucketIfNotExists(pHash[:])
		if err != nil {
			return err
		}

		// We put the serialized information under the respective keys,
		// such that it can be fetched when a result for this payment
		// attempt comes back.
		if err := bucket.Put(paymentRouteKey, routeBuf.Bytes()); err != nil {
			return err
		}

		return bucket.Put(paymentIDKey, pidBuf.Bytes())
	})
}

// FetchRoute fetches the last route stored for the given payment hash.
func (s *paymentStore) fetchRoute(pHash lntypes.Hash) (*Route, error) {
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

// fetchPaymentID fetches the last stored payment ID for the given payment
// hash.
func (s *paymentStore) fetchPaymentID(pHash lntypes.Hash) (uint64, error) {
	var pid uint64
	err := s.db.View(func(tx *bbolt.Tx) error {
		paymentsBucket := tx.Bucket(paymentsBucketKey)
		if paymentsBucket == nil {
			return nil
		}

		bucket := paymentsBucket.Bucket(pHash[:])
		if bucket == nil {
			return ErrNotFound
		}

		v := bucket.Get(paymentIDKey)
		if v == nil {
			return ErrNotFound
		}

		r := bytes.NewReader(v)
		return channeldb.ReadElements(r, &pid)
	})
	if err != nil {
		return 0, err
	}

	return pid, nil
}
