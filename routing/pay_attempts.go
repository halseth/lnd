package routing

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/coreos/bbolt"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
)

type paymentType uint8

const (
	typeSendPayment paymentType = 0
	typeSendToRoute paymentType = 1
)

var (
	lightningPaymentsBucket = []byte("lightning-payments")
	payAttemptBucket        = []byte("pay-attempts")
	byteOrder               = binary.BigEndian

	ErrNotFound = errors.New("payment hash not found")
)

type payAttemptStore struct {
	DB *channeldb.DB
}

func (s *payAttemptStore) initSendPayment(l *LightningPayment) error {

	var b bytes.Buffer
	if err := channeldb.WriteElements(&b, typeSendPayment); err != nil {
		return err
	}

	if err := serializeLightningPayment(&b, l); err != nil {
		return err
	}

	return s.DB.Update(func(tx *bbolt.Tx) error {
		payments, err := tx.CreateBucketIfNotExists(lightningPaymentsBucket)
		if err != nil {
			return err
		}
		return payments.Put(l.PaymentHash[:], b.Bytes())
	})
}

func (s *payAttemptStore) initSendToRoute(pHash [32]byte, routes []*Route) error {

	var b bytes.Buffer
	if err := channeldb.WriteElements(&b, typeSendToRoute); err != nil {
		return err
	}

	if err := channeldb.WriteElements(&b, uint32(len(routes))); err != nil {
		return err
	}

	for _, r := range routes {
		if err := serializeRoute(&b, r); err != nil {
			return err
		}
	}

	return s.DB.Update(func(tx *bbolt.Tx) error {
		payments, err := tx.CreateBucketIfNotExists(lightningPaymentsBucket)
		if err != nil {
			return err
		}
		return payments.Put(pHash[:], b.Bytes())
	})
}

func (s *payAttemptStore) deletePayment(pHash [32]byte) error {

	return s.DB.Update(func(tx *bbolt.Tx) error {
		payments := tx.Bucket(lightningPaymentsBucket)
		if payments == nil {
			return ErrNotFound
		}
		attempts := tx.Bucket(payAttemptBucket)
		if attempts == nil {
			return nil
		}

		return attempts.Delete(pHash[:])
	})
}

type StoredPayment interface {
	PaymentHash() [32]byte
	InitWithPaymentStore(store *payAttemptStore) error
}

type lightningPayment struct {
	*LightningPayment
}

func (p *lightningPayment) PaymentHash() [32]byte {
	return p.LightningPayment.PaymentHash
}

func (p *lightningPayment) InitWithPaymentStore(store *payAttemptStore) error {
	return store.initSendPayment(p.LightningPayment)
}

type routePayment struct {
	paymentHash [32]byte
	routes      []*Route
}

func (p *routePayment) PaymentHash() [32]byte {
	return p.paymentHash
}

func (p *routePayment) InitWithPaymentStore(store *payAttemptStore) error {
	return store.initSendToRoute(p.paymentHash, p.routes)
}

type storedPayment struct {
	paymentHash [32]byte
	payment     *LightningPayment
	routes      []*Route
}

func (s *payAttemptStore) fetchPayments() ([]StoredPayment, error) {

	var payments []StoredPayment
	err := s.DB.View(func(tx *bbolt.Tx) error {
		paymentsBucket := tx.Bucket(lightningPaymentsBucket)
		if paymentsBucket == nil {
			return nil
		}

		return paymentsBucket.ForEach(func(k, v []byte) error {
			r := bytes.NewReader(v)
			var t paymentType
			err := channeldb.ReadElements(r, &t)
			if err != nil {
				return err
			}

			var payment StoredPayment

			switch t {
			case typeSendPayment:

				l, err := deserializeLightningPayment(r)
				if err != nil {
					return err
				}
				payment = &lightningPayment{l}

			case typeSendToRoute:
				var numRoutes uint32
				err := channeldb.ReadElements(r, &numRoutes)
				if err != nil {
					return err
				}

				var routes []*Route
				for i := uint32(0); i < numRoutes; i++ {
					route, err := deserializeRoute(r)
					if err != nil {
						return err
					}
					routes = append(routes, route)
				}
				r := &routePayment{
					routes: routes,
				}
				copy(r.paymentHash[:], k[:])
				payment = r

			default:
				// TODO: return nil for forwards compat?
				return fmt.Errorf("unknown payment type")
			}

			payments = append(payments, payment)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return payments, nil
}

type payAttempt struct {
	paymentID uint64
	firstHop  lnwire.ShortChannelID
	htlcAdd   *lnwire.UpdateAddHTLC
	route     *Route
	circuit   *sphinx.Circuit
}

func serializePayAttempt(w io.Writer, p *payAttempt) error {
	if err := channeldb.WriteElements(w,
		p.paymentID, p.firstHop,
	); err != nil {
		return err
	}

	if err := p.htlcAdd.Encode(w, 0); err != nil {
		return err
	}

	if err := serializeRoute(w, p.route); err != nil {
		return err
	}

	if err := p.circuit.Encode(w); err != nil {
		return err
	}

	return nil
}

func deserializePayAttempt(r io.Reader) (*payAttempt, error) {
	p := &payAttempt{}
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

	route, err := deserializeRoute(r)
	if err != nil {
		return nil, err
	}
	p.route = route

	circuit := &sphinx.Circuit{}
	if err := circuit.Decode(r); err != nil {
		return nil, err
	}
	p.circuit = circuit

	return p, nil
}

func (s *payAttemptStore) storePayAttempt(pHash lntypes.Hash, p *payAttempt) error {

	var b bytes.Buffer
	if err := serializePayAttempt(&b, p); err != nil {
		return err
	}

	return s.DB.Update(func(tx *bbolt.Tx) error {
		attempts, err := tx.CreateBucketIfNotExists(payAttemptBucket)
		if err != nil {
			return err
		}
		return attempts.Put(pHash[:], b.Bytes())
	})
}

func (s *payAttemptStore) deletePayAttempt(pHash lntypes.Hash) error {
	return s.DB.Update(func(tx *bbolt.Tx) error {
		attempts, err := tx.CreateBucketIfNotExists(payAttemptBucket)
		if err != nil {
			return err
		}
		return attempts.Delete(pHash[:])
	})
}

func (s *payAttemptStore) fetchPayAttempt(pHash lntypes.Hash) (*payAttempt, error) {
	var p *payAttempt
	err := s.DB.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(payAttemptBucket)
		if bucket == nil {
			return ErrNotFound
		}

		v := bucket.Get(pHash[:])
		if v == nil {
			return ErrNotFound
		}

		r := bytes.NewReader(v)
		var err error
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
