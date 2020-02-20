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

	// Iterate over all payments.
	paymentsBucket := tx.Bucket(paymentsRootBucket)
	if paymentsBucket == nil {
		return nil
	}

	err := paymentsBucket.ForEach(func(k, v []byte) error {
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
			return nil
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
		attemptInfo = append(
			attemptInfo, 0, 0, 0, 0, 0, 0, 0, 0,
		)

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
			settleInfo = append(
				settleInfo, 0, 0, 0, 0, 0, 0, 0, 0,
			)

			// Save settle info.
			err = htlcBucket.Put(htlcSettleInfoKey, settleInfo)
			if err != nil {
				return err
			}

			// Migration for settled htlc completed.
			return nil
		}

		// If there is no payment-level failure reason, the payment is
		// still in flight and nothing else needs to be migrated.
		inFlight := bucket.Get(paymentFailInfoKey) == nil
		if inFlight {
			return nil
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
		return htlcBucket.Put(htlcFailInfoKey, failInfo)
	})
	if err != nil {
		return err
	}

	log.Infof("Migration of payments to mpp structure complete!")

	return nil
}
