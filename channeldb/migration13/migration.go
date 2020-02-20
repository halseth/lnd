package migration13

import (
	"fmt"

	"github.com/coreos/bbolt"
)

var (
	paymentsRootBucket = []byte("payments-root-bucket")

	// paymentFailInfoKey is a key used in the payment's sub-bucket to
	// store information about the reason a payment failed.
	paymentFailInfoKey = []byte("payment-fail-info")

	// paymentAttemptInfoKey is a key used in the payment's sub-bucket to
	// store the info about the latest attempt that was done for the
	// payment in question.
	paymentAttemptInfoKey = []byte("payment-attempt-info")

	// paymentSettleInfoKey is a key used in the payment's sub-bucket to
	// store the settle info of the payment.
	paymentSettleInfoKey = []byte("payment-settle-info")

	// paymentHtlcsBucket is a bucket where we'll store the information
	// about the HTLCs that were attempted for a payment.
	paymentHtlcsBucket = []byte("payment-htlcs-bucket")

	// htlcAttemptInfoKey is a key used in a HTLC's sub-bucket to store the
	// info about the attempt that was done for the HTLC in question.
	htlcAttemptInfoKey = []byte("htlc-attempt-info")

	// htlcSettleInfoKey is a key used in a HTLC's sub-bucket to store the
	// settle info, if any.
	htlcSettleInfoKey = []byte("htlc-settle-info")

	// htlcFailInfoKey is a key used in a HTLC's sub-bucket to store
	// failure information, if any.
	htlcFailInfoKey = []byte("htlc-fail-info")
)

// MigrateMPP migrates the payments to a new structure that accommodates for mpp
// payments.
func MigrateMPP(tx *bbolt.Tx) error {
	log.Infof("Migrating payments to mpp structure")

	// Iterate over all payments and store their indexing keys. This is
	// needed, because no modifications are allowed inside a Bucket.ForEach
	// loop.
	paymentsBucket := tx.Bucket(paymentsRootBucket)
	if paymentsBucket == nil {
		return nil
	}

	var paymentKeys [][]byte
	err := paymentsBucket.ForEach(func(k, v []byte) error {
		paymentKeys = append(paymentKeys, k)
		return nil
	})
	if err != nil {
		return err
	}

	// With all keys retrieved, start the migration.
	for _, k := range paymentKeys {
		bucket := paymentsBucket.Bucket(k)

		// We only expect sub-buckets to be found in
		// this top-level bucket.
		if bucket == nil {
			return fmt.Errorf("non bucket element in " +
				"payments bucket")
		}

		// No migration needed if there is no attempt stored.
		attemptInfo := bucket.Get(paymentAttemptInfoKey)
		if attemptInfo == nil {
			continue
		}

		// Delete attempt info on the payment level.
		if err := bucket.Delete(paymentAttemptInfoKey); err != nil {
			return err
		}

		// Save attempt id for later use.
		attemptID := attemptInfo[:8]

		// Discard attempt id. It will become a bucket key in the new
		// structure.
		attemptInfo = attemptInfo[8:]

		// Append unknown (zero) attempt time.
		var zero [8]byte
		attemptInfo = append(attemptInfo, zero[:]...)

		// Create bucket that contains all htlcs.
		htlcsBucket, err := bucket.CreateBucket(paymentHtlcsBucket)
		if err != nil {
			return err
		}

		// Create an htlc for this attempt.
		htlcBucket, err := htlcsBucket.CreateBucket(attemptID)
		if err != nil {
			return err
		}

		// Save migrated attempt info.
		err = htlcBucket.Put(htlcAttemptInfoKey, attemptInfo)
		if err != nil {
			return err
		}

		// Migrate settle info.
		settleInfo := bucket.Get(paymentSettleInfoKey)
		if settleInfo != nil {
			// Payment-level settle info can be deleted.
			err := bucket.Delete(paymentSettleInfoKey)
			if err != nil {
				return err
			}

			// Append unknown (zero) settle time.
			settleInfo = append(settleInfo, zero[:]...)

			// Save settle info.
			err = htlcBucket.Put(htlcSettleInfoKey, settleInfo)
			if err != nil {
				return err
			}

			// Migration for settled htlc completed.
			continue
		}

		// If there is no payment-level failure reason, the payment is
		// still in flight and nothing else needs to be migrated.
		// Otherwise the payment-level failure reason can remain
		// unchanged.
		inFlight := bucket.Get(paymentFailInfoKey) == nil
		if inFlight {
			continue
		}

		// The htlc failed. Add htlc fail info with reason unknown. We
		// don't have access to the original failure reason anymore.
		failInfo := []byte{
			// Fail time unknown.
			0, 0, 0, 0, 0, 0, 0, 0,

			// Zero length wire message.
			0,

			// Failure reason unknown.
			0,

			// Failure source index zero.
			0, 0, 0, 0,
		}

		// Save fail info.
		err = htlcBucket.Put(htlcFailInfoKey, failInfo)
		if err != nil {
			return err
		}
	}

	log.Infof("Migration of payments to mpp structure complete!")

	return nil
}
