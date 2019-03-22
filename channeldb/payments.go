package channeldb

import (
	"bytes"
	"errors"
	"io"
	"time"

	"github.com/coreos/bbolt"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// outgoingPaymentBucket is the name of the top-level bucket within the
	// database that stores all data related to payments.
	//
	// Within the payments bucket, each payment hash its own sub.bucket
	// keyed by its payment hash
	outgoingPaymentBucket = []byte("outgoing-payments")

	// paymentStatusKey is a key used in the payment's sub-bucket to store
	// the status of the payment.
	paymentStatusKey = []byte("payment-status")

	// paymentCreationInfoKey is a key used in the payment's sub-bucket to store
	// the creation info of the payment.
	paymentCreationInfoKey = []byte("payment-creation-info")

	// paymentSettleInfoKey is a key used in the payment's sub-bucket to store
	// the settle info of the payment.
	paymentSettleInfoKey = []byte("payment-settle-info")
)

// PaymentStatus represent current status of payment
type PaymentStatus byte

const (
	// StatusGrounded is the status where a payment has never been
	// initiated.
	StatusGrounded PaymentStatus = 0

	// StatusInFlight is the status where a payment has been initiated, but
	// a response has not been received.
	StatusInFlight PaymentStatus = 1

	// StatusCompleted is the status where a payment has been initiated and
	// the payment was completed successfully.
	StatusCompleted PaymentStatus = 2

	// StatusFailed is the status where a payment has been initiated and a
	// failure result has come back.
	StatusFailed PaymentStatus = 3

	// TODO(halseth): timeout/cancel state?
)

// Bytes returns status as slice of bytes.
func (ps PaymentStatus) Bytes() []byte {
	return []byte{byte(ps)}
}

// FromBytes sets status from slice of bytes.
func (ps *PaymentStatus) FromBytes(status []byte) error {
	if len(status) != 1 {
		return errors.New("payment status is empty")
	}

	switch PaymentStatus(status[0]) {
	case StatusGrounded, StatusInFlight, StatusCompleted:
		*ps = PaymentStatus(status[0])
	default:
		return errors.New("unknown payment status")
	}

	return nil
}

// String returns readable representation of payment status.
func (ps PaymentStatus) String() string {
	switch ps {
	case StatusGrounded:
		return "Grounded"
	case StatusInFlight:
		return "In Flight"
	case StatusCompleted:
		return "Completed"
	case StatusFailed:
		return "Failed"
	default:
		return "Unknown"
	}
}

// CreationInfo is the information necessary to have ready when initiating a
// payment, moving it into state InFlight.
type CreationInfo struct {
	// PaymentHash is the hash this payment is paying to.
	PaymentHash [32]byte

	// Value is the amount we are paying.
	Value lnwire.MilliSatoshi

	// CreatingDate is the time when this payment was initiated.
	CreationDate time.Time

	// TODO: add payreq.
}

// SettleInfo is the information to provide to settle an in-flight payment.
type SettleInfo struct {
	// Fee is the total fee paid for the payment in milli-satoshis.
	Fee lnwire.MilliSatoshi

	// TotalTimeLock is the total cumulative time-lock in the HTLC extended
	// from the second-to-last hop to the destination.
	TimeLockLength uint32

	// Path encodes the path the payment took through the network. The path
	// excludes the outgoing node and consists of the hex-encoded
	// compressed public key of each of the nodes involved in the payment.
	Path [][33]byte

	// PaymentPreimage is the preImage of a successful payment. This serves
	// as a proof of payment.
	PaymentPreimage [32]byte
}

// OutgoingPayment represents a payment between the daemon and a remote node.
// Details such as the total fee paid, and the time of the payment are stored.
type OutgoingPayment struct {
	// Creation info is populated when the payment is initiated, and is
	// available for all payments.
	CreationInfo

	// SettleInfo is only available when the payment has successfully been
	// settled.
	SettleInfo
}

// AddPayment saves a successful payment to the database. It is assumed that
// all payment are sent using unique payment hashes.
func (db *DB) AddPayment(payment *OutgoingPayment) error {
	// We first serialize the payment before starting the database
	// transaction so we can avoid creating a DB payment in the case of a
	// serialization error.
	var c bytes.Buffer
	if err := serializeCreationInfo(&c, &payment.CreationInfo); err != nil {
		return err
	}

	var s bytes.Buffer
	if err := serializeSettleInfo(&s, &payment.SettleInfo); err != nil {
		return err
	}

	return db.Batch(func(tx *bbolt.Tx) error {
		payments, err := tx.CreateBucketIfNotExists(outgoingPaymentBucket)
		if err != nil {
			return err
		}

		bucket, err := payments.CreateBucketIfNotExists(payment.PaymentHash[:])
		if err != nil {
			return err
		}

		if err := bucket.Put(paymentCreationInfoKey, c.Bytes()); err != nil {
			return err
		}

		if err := bucket.Put(paymentSettleInfoKey, s.Bytes()); err != nil {
			return err
		}

		return nil
	})
}

// FetchAllPayments returns all outgoing payments in DB.
func (db *DB) FetchAllPayments() ([]*OutgoingPayment, error) {
	var payments []*OutgoingPayment

	err := db.View(func(tx *bbolt.Tx) error {
		paymentsBucket := tx.Bucket(outgoingPaymentBucket)
		if paymentsBucket == nil {
			return ErrNoPaymentsCreated
		}

		return paymentsBucket.ForEach(func(k, v []byte) error {
			// If the value is nil, then we ignore it as it may be
			// a sub-bucket.
			if v == nil {
				return nil
			}

			bucket := paymentsBucket.Bucket(k)
			if bucket == nil {
				return nil
			}

			p := &OutgoingPayment{}

			b := bucket.Get(paymentCreationInfoKey)
			if b == nil {
				return nil
			}

			r := bytes.NewReader(b)
			c, err := deserializeCreationInfo(r)
			if err != nil {
				return err
			}
			p.CreationInfo = *c

			b = bucket.Get(paymentSettleInfoKey)
			if b != nil {

				r = bytes.NewReader(b)
				s, err := deserializeSettleInfo(r)
				if err != nil {
					return err
				}
				p.SettleInfo = *s
			}

			payments = append(payments, p)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	return payments, nil
}

// DeleteAllPayments deletes all payments from DB.
func (db *DB) DeleteAllPayments() error {
	return db.Update(func(tx *bbolt.Tx) error {
		err := tx.DeleteBucket(outgoingPaymentBucket)
		if err != nil && err != bbolt.ErrBucketNotFound {
			return err
		}

		_, err = tx.CreateBucket(outgoingPaymentBucket)
		return err
	})
}

func serializeCreationInfo(w io.Writer, c *CreationInfo) error {
	var scratch [8]byte

	if _, err := w.Write(c.PaymentHash[:]); err != nil {
		return err
	}

	byteOrder.PutUint64(scratch[:], uint64(c.Value))
	if _, err := w.Write(scratch[:]); err != nil {
		return err
	}

	byteOrder.PutUint64(scratch[:], uint64(c.CreationDate.Unix()))
	if _, err := w.Write(scratch[:]); err != nil {
		return err
	}

	return nil
}

func serializeSettleInfo(w io.Writer, s *SettleInfo) error {
	var scratch [8]byte

	byteOrder.PutUint64(scratch[:], uint64(s.Fee))
	if _, err := w.Write(scratch[:]); err != nil {
		return err
	}

	// First write out the length of the bytes to prefix the value.
	pathLen := uint32(len(s.Path))
	byteOrder.PutUint32(scratch[:4], pathLen)
	if _, err := w.Write(scratch[:4]); err != nil {
		return err
	}

	// Then with the path written, we write out the series of public keys
	// involved in the path.
	for _, hop := range s.Path {
		if _, err := w.Write(hop[:]); err != nil {
			return err
		}
	}

	byteOrder.PutUint32(scratch[:4], s.TimeLockLength)
	if _, err := w.Write(scratch[:4]); err != nil {
		return err
	}

	if _, err := w.Write(s.PaymentPreimage[:]); err != nil {
		return err
	}

	return nil
}

func deserializeCreationInfo(r io.Reader) (*CreationInfo, error) {
	var scratch [8]byte

	s := &CreationInfo{}

	if _, err := r.Read(s.PaymentHash[:]); err != nil {
		return nil, err
	}

	if _, err := r.Read(scratch[:]); err != nil {
		return nil, err
	}
	s.Value = lnwire.MilliSatoshi(byteOrder.Uint64(scratch[:]))

	if _, err := r.Read(scratch[:]); err != nil {
		return nil, err
	}
	s.CreationDate = time.Unix(int64(byteOrder.Uint64(scratch[:])), 0)
	return s, nil
}

func deserializeSettleInfo(r io.Reader) (*SettleInfo, error) {
	var scratch [8]byte
	s := &SettleInfo{}

	if _, err := r.Read(scratch[:]); err != nil {
		return nil, err
	}
	s.Fee = lnwire.MilliSatoshi(byteOrder.Uint64(scratch[:]))

	if _, err := r.Read(scratch[:4]); err != nil {
		return nil, err
	}
	pathLen := byteOrder.Uint32(scratch[:4])

	path := make([][33]byte, pathLen)
	for i := uint32(0); i < pathLen; i++ {
		if _, err := r.Read(path[i][:]); err != nil {
			return nil, err
		}
	}
	s.Path = path

	if _, err := r.Read(scratch[:4]); err != nil {
		return nil, err
	}
	s.TimeLockLength = byteOrder.Uint32(scratch[:4])

	if _, err := r.Read(s.PaymentPreimage[:]); err != nil {
		return nil, err
	}

	return s, nil
}
