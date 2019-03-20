package channeldb

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/coreos/bbolt"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// paymentBucket is the name of the bucket within the database that
	// stores all data related to payments.
	//
	// Within the payments bucket, each invoice is keyed by its invoice ID
	// which is a monotonically increasing uint64.  BoltDB's sequence
	// feature is used for generating monotonically increasing id.
	paymentBucket = []byte("payments")

	// paymentStatusBucket is the name of the bucket within the database that
	// stores the status of a payment indexed by the payment's preimage.
	paymentStatusBucket = []byte("payment-status")

	// paymentInvoiceIDBucket is a bucket where we map payment hashes to
	// invoice IDs. This is needed so we can easily lookup the
	// OutgoingPayment knowing only the payment hash.
	paymentInvoiceIDBucket = []byte("payment-invoice-id")

	// ErrPreimageKnown is an error returned if we try to complete or
	// delete a payment whose preimage is already set in the database.
	ErrPreimageKnown = errors.New("preimage was already found in the" +
		"database")

	// zeroPreimage is the empty preimage, indicating the payment's
	// preimage is not yet known.
	zeroPreimage [32]byte
)

// PaymentStatus represent current status of payment
type PaymentStatus byte

const (
	// StatusGrounded is the status where a payment has never been
	// initiated, or has been initiated and received an intermittent
	// failure.
	StatusGrounded PaymentStatus = 0

	// StatusInFlight is the status where a payment has been initiated, but
	// a response has not been received.
	StatusInFlight PaymentStatus = 1

	// StatusCompleted is the status where a payment has been initiated and
	// the payment was completed successfully.
	StatusCompleted PaymentStatus = 2
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
	default:
		return "Unknown"
	}
}

// OutgoingPayment represents a payment between the daemon and a remote node.
// Details such as the total fee paid, and the time of the payment are stored.
type OutgoingPayment struct {
	Invoice

	// Fee is the total fee paid for the payment in milli-satoshis.
	Fee lnwire.MilliSatoshi

	// TotalTimeLock is the total cumulative time-lock in the HTLC extended
	// from the second-to-last hop to the destination.
	TimeLockLength uint32

	// Path encodes the path the payment took through the network. The path
	// excludes the outgoing node and consists of the hex-encoded
	// compressed public key of each of the nodes involved in the payment.
	// NOTE: this is only set for completed payments.
	Path [][33]byte

	// PaymentPreimage is the preImage of a successful payment. This is used
	// to calculate the PaymentHash as well as serve as a proof of payment.
	// NOTE: This is all-zeroes for payment not (yet) completed.
	PaymentPreimage [32]byte

	// PaymentHash is the has this payment is paying to.
	PaymentHash [32]byte
}

// AddPayment adds an inflight payment to the given hash to the database. It is
// assumed that all payment are sent using unique payment hashes.
func (db *DB) AddPayment(pHash lntypes.Hash) error {
	return db.Batch(func(tx *bbolt.Tx) error {
		return db.AddPaymentTx(tx, pHash)
	})
}

// AddPaymentTx lets the caller call AddPayment with a given database tx to
// ensure atomicity.
func (db *DB) AddPaymentTx(tx *bbolt.Tx, pHash lntypes.Hash) error {
	// Create an OutgoingPayment to store, containing only the payment hash.
	payment := &OutgoingPayment{
		PaymentHash: pHash,
	}

	var b bytes.Buffer
	if err := serializeOutgoingPayment(&b, payment); err != nil {
		return err
	}
	paymentBytes := b.Bytes()

	payments, err := tx.CreateBucketIfNotExists(paymentBucket)
	if err != nil {
		return err
	}

	invoiceIDs, err := tx.CreateBucketIfNotExists(
		paymentInvoiceIDBucket,
	)
	if err != nil {
		return err
	}

	// Ensure this payment hash is not already mapped to an invoice ID.
	v := invoiceIDs.Get(payment.PaymentHash[:])
	if v != nil {
		return fmt.Errorf("payment with hash %x already exists",
			payment.PaymentHash)
	}

	// Obtain the new unique sequence number for this payment.
	invoiceID, err := payments.NextSequence()
	if err != nil {
		return err
	}

	// We use BigEndian for keys as it orders keys in ascending order. This
	// allows bucket scans to order payments in the order in which they
	// were created.
	invoiceIDBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(invoiceIDBytes, invoiceID)

	err = invoiceIDs.Put(payment.PaymentHash[:], invoiceIDBytes)
	if err != nil {
		return err
	}

	return payments.Put(invoiceIDBytes, paymentBytes)
}

// CompletePayment overwrites the OutgoingPayment stored in the DB for the
// corresponding payment hash with the completed one.
func (db *DB) CompletePayment(payment *OutgoingPayment) error {
	return db.Batch(func(tx *bbolt.Tx) error {
		return db.CompletePaymentTx(tx, payment)
	})
}

// CompletePaymentTx lets the caller call CompletePayment with a given database
// tx to ensure atomicity.
func (db *DB) CompletePaymentTx(tx *bbolt.Tx, payment *OutgoingPayment) error {
	// Validate the field of the inner voice within the outgoing payment,
	// these must also adhere to the same constraints as regular invoices.
	if err := validateInvoice(&payment.Invoice); err != nil {
		return err
	}

	// We first serialize the payment before starting the database
	// transaction so we can avoid creating a DB payment in the case of a
	// serialization error.
	var b bytes.Buffer
	if err := serializeOutgoingPayment(&b, payment); err != nil {
		return err
	}
	paymentBytes := b.Bytes()
	pHash := payment.PaymentHash

	payments := tx.Bucket(paymentBucket)
	if payments == nil {
		return ErrNoPaymentsCreated
	}

	invoiceIDs := tx.Bucket(paymentInvoiceIDBucket)
	if invoiceIDs == nil {
		return ErrNoPaymentsCreated
	}

	invoiceIDBytes := invoiceIDs.Get(pHash[:])
	if invoiceIDBytes == nil {
		return fmt.Errorf("payment with hash %v not found",
			pHash)
	}

	// If the payment is already in the DB, it must be a
	// non-completed one (zero preimage)
	v := payments.Get(invoiceIDBytes)
	if v != nil {
		r := bytes.NewReader(v)
		oldPayment, err := deserializeOutgoingPayment(r)
		if err != nil {
			return err
		}

		// The existing preimage stored for this payment MUST
		// be the zero preimage.
		if oldPayment.PaymentPreimage != zeroPreimage {
			return ErrPreimageKnown
		}
	}

	// Overwrite any existing payment with the completed one.
	return payments.Put(invoiceIDBytes, paymentBytes)
}

// FetchAllPayments returns all outgoing payments in DB.
func (db *DB) FetchAllPayments() ([]*OutgoingPayment, error) {
	var payments []*OutgoingPayment

	err := db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(paymentBucket)
		if bucket == nil {
			return ErrNoPaymentsCreated
		}

		return bucket.ForEach(func(k, v []byte) error {
			// If the value is nil, then we ignore it as it may be
			// a sub-bucket.
			if v == nil {
				return nil
			}

			r := bytes.NewReader(v)
			payment, err := deserializeOutgoingPayment(r)
			if err != nil {
				return err
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

// DeleteAllPayments deletes all payments from DB.
func (db *DB) DeleteAllPayments() error {
	return db.Update(func(tx *bbolt.Tx) error {
		err := tx.DeleteBucket(paymentBucket)
		if err != nil && err != bbolt.ErrBucketNotFound {
			return err
		}

		_, err = tx.CreateBucket(paymentBucket)
		return err
	})
}

// UpdatePaymentStatus sets the payment status for outgoing/finished payments in
// local database.
func (db *DB) UpdatePaymentStatus(paymentHash [32]byte, status PaymentStatus) error {
	return db.Batch(func(tx *bbolt.Tx) error {
		return UpdatePaymentStatusTx(tx, paymentHash, status)
	})
}

// UpdatePaymentStatusTx is a helper method that sets the payment status for
// outgoing/finished payments in the local database. This method accepts a
// boltdb transaction such that the operation can be composed into other
// database transactions.
func UpdatePaymentStatusTx(tx *bbolt.Tx,
	paymentHash [32]byte, status PaymentStatus) error {

	paymentStatuses, err := tx.CreateBucketIfNotExists(paymentStatusBucket)
	if err != nil {
		return err
	}

	return paymentStatuses.Put(paymentHash[:], status.Bytes())
}

// FetchPaymentStatus returns the payment status for outgoing payment.
// If status of the payment isn't found, it will default to "StatusGrounded".
func (db *DB) FetchPaymentStatus(paymentHash [32]byte) (PaymentStatus, error) {
	var paymentStatus = StatusGrounded
	err := db.View(func(tx *bbolt.Tx) error {
		var err error
		paymentStatus, err = FetchPaymentStatusTx(tx, paymentHash)
		return err
	})
	if err != nil {
		return StatusGrounded, err
	}

	return paymentStatus, nil
}

// FetchPaymentStatusTx is a helper method that returns the payment status for
// outgoing payment.  If status of the payment isn't found, it will default to
// "StatusGrounded". It accepts the boltdb transactions such that this method
// can be composed into other atomic operations.
func FetchPaymentStatusTx(tx *bbolt.Tx, paymentHash [32]byte) (PaymentStatus, error) {
	// The default status for all payments that aren't recorded in database.
	var paymentStatus = StatusGrounded

	bucket := tx.Bucket(paymentStatusBucket)
	if bucket == nil {
		return paymentStatus, nil
	}

	paymentStatusBytes := bucket.Get(paymentHash[:])
	if paymentStatusBytes == nil {
		return paymentStatus, nil
	}

	paymentStatus.FromBytes(paymentStatusBytes)

	return paymentStatus, nil
}

func serializeOutgoingPayment(w io.Writer, p *OutgoingPayment) error {
	var scratch [8]byte

	if err := serializeInvoice(w, &p.Invoice); err != nil {
		return err
	}

	byteOrder.PutUint64(scratch[:], uint64(p.Fee))
	if _, err := w.Write(scratch[:]); err != nil {
		return err
	}

	// First write out the length of the bytes to prefix the value.
	pathLen := uint32(len(p.Path))
	byteOrder.PutUint32(scratch[:4], pathLen)
	if _, err := w.Write(scratch[:4]); err != nil {
		return err
	}

	// Then with the path written, we write out the series of public keys
	// involved in the path.
	for _, hop := range p.Path {
		if _, err := w.Write(hop[:]); err != nil {
			return err
		}
	}

	byteOrder.PutUint32(scratch[:4], p.TimeLockLength)
	if _, err := w.Write(scratch[:4]); err != nil {
		return err
	}

	if _, err := w.Write(p.PaymentPreimage[:]); err != nil {
		return err
	}

	if _, err := w.Write(p.PaymentHash[:]); err != nil {
		return err
	}

	return nil
}

func deserializeOutgoingPayment(r io.Reader) (*OutgoingPayment, error) {
	var scratch [8]byte

	p := &OutgoingPayment{}

	inv, err := deserializeInvoice(r)
	if err != nil {
		return nil, err
	}
	p.Invoice = inv

	if _, err := r.Read(scratch[:]); err != nil {
		return nil, err
	}
	p.Fee = lnwire.MilliSatoshi(byteOrder.Uint64(scratch[:]))

	if _, err = r.Read(scratch[:4]); err != nil {
		return nil, err
	}
	pathLen := byteOrder.Uint32(scratch[:4])

	path := make([][33]byte, pathLen)
	for i := uint32(0); i < pathLen; i++ {
		if _, err := r.Read(path[i][:]); err != nil {
			return nil, err
		}
	}
	p.Path = path

	if _, err = r.Read(scratch[:4]); err != nil {
		return nil, err
	}
	p.TimeLockLength = byteOrder.Uint32(scratch[:4])

	if _, err := r.Read(p.PaymentPreimage[:]); err != nil {
		return nil, err
	}

	// Before we started storing the payment hash explicitly, only the
	// preimage was stored. For these payments the preimage was always
	// stored, so can calculate the payment hash from it.
	_, err = r.Read(p.PaymentHash[:])
	if err == io.EOF {
		p.PaymentHash = sha256.Sum256(p.PaymentPreimage[:])
	} else if err != nil {
		return nil, err
	}

	return p, nil
}
