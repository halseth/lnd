package channeldb

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"io/ioutil"
	"reflect"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/fastsha256"
	"github.com/coreos/bbolt"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/routing/route"
)

func initDB() (*DB, error) {
	tempPath, err := ioutil.TempDir("", "switchdb")
	if err != nil {
		return nil, err
	}

	db, err := Open(tempPath)
	if err != nil {
		return nil, err
	}

	return db, err
}

func genPreimage() ([32]byte, error) {
	var preimage [32]byte
	if _, err := io.ReadFull(rand.Reader, preimage[:]); err != nil {
		return preimage, err
	}
	return preimage, nil
}

func genInfo() (*CreationInfo, *SettleInfo, error) {
	preimage, err := genPreimage()
	if err != nil {
		return nil, nil, fmt.Errorf("unable to generate preimage: %v", err)
	}

	rhash := fastsha256.Sum256(preimage[:])
	return &CreationInfo{
			PaymentHash:  rhash,
			Value:        1,
			CreationDate: time.Now(),
		}, &SettleInfo{
			Fee:             1,
			TimeLockLength:  155,
			Path:            nil,
			PaymentPreimage: preimage,
		}, nil
}

type paymentControlTestCase func(*testing.T, bool)

var paymentControlTests = []struct {
	name     string
	strict   bool
	testcase paymentControlTestCase
}{
	{
		name:     "fail-strict",
		strict:   true,
		testcase: testPaymentControlSwitchFail,
	},
	{
		name:     "double-send-strict",
		strict:   true,
		testcase: testPaymentControlSwitchDoubleSend,
	},
	{
		name:     "double-pay-strict",
		strict:   true,
		testcase: testPaymentControlSwitchDoublePay,
	},
	{
		name:     "fail-not-strict",
		strict:   false,
		testcase: testPaymentControlSwitchFail,
	},
	{
		name:     "double-send-not-strict",
		strict:   false,
		testcase: testPaymentControlSwitchDoubleSend,
	},
	{
		name:     "double-pay-not-strict",
		strict:   false,
		testcase: testPaymentControlSwitchDoublePay,
	},
}

// TestPaymentControls runs a set of common tests against both the strict and
// non-strict payment control instances. This ensures that the two both behave
// identically when making the expected state-transitions of the stricter
// implementation. Behavioral differences in the strict and non-strict
// implementations are tested separately.
func TestPaymentControls(t *testing.T) {
	for _, test := range paymentControlTests {
		t.Run(test.name, func(t *testing.T) {
			test.testcase(t, test.strict)
		})
	}
}

// testPaymentControlSwitchFail checks that payment status returns to Grounded
// status after failing, and that ClearForTakeoff allows another HTLC for the
// same payment hash.
func testPaymentControlSwitchFail(t *testing.T, strict bool) {
	t.Parallel()

	db, err := initDB()
	if err != nil {
		t.Fatalf("unable to init db: %v", err)
	}

	pControl := NewPaymentControl(strict, db)

	info, settle, err := genInfo()
	if err != nil {
		t.Fatalf("unable to generate htlc message: %v", err)
	}

	// Sends base htlc message which initiate StatusInFlight.
	if err := pControl.ClearForTakeoff(info); err != nil {
		t.Fatalf("unable to send htlc message: %v", err)
	}

	assertPaymentStatus(t, db, info.PaymentHash, StatusInFlight)

	// Fail the payment, which should moved it to Grounded.
	if err := pControl.Fail(info.PaymentHash); err != nil {
		t.Fatalf("unable to fail payment hash: %v", err)
	}

	// Verify the status is indeed Grounded.
	assertPaymentStatus(t, db, info.PaymentHash, StatusGrounded)

	// Sends the htlc again, which should succeed since the prior payment
	// failed.
	if err := pControl.ClearForTakeoff(info); err != nil {
		t.Fatalf("unable to send htlc message: %v", err)
	}

	assertPaymentStatus(t, db, info.PaymentHash, StatusInFlight)

	// Verifies that status was changed to StatusCompleted.
	if err := pControl.Success(info.PaymentHash, settle); err != nil {
		t.Fatalf("error shouldn't have been received, got: %v", err)
	}

	assertPaymentStatus(t, db, info.PaymentHash, StatusCompleted)

	// Attempt a final payment, which should now fail since the prior
	// payment succeed.
	if err := pControl.ClearForTakeoff(info); err != ErrAlreadyPaid {
		t.Fatalf("unable to send htlc message: %v", err)
	}
}

// testPaymentControlSwitchDoubleSend checks the ability of payment control to
// prevent double sending of htlc message, when message is in StatusInFlight.
func testPaymentControlSwitchDoubleSend(t *testing.T, strict bool) {
	t.Parallel()

	db, err := initDB()
	if err != nil {
		t.Fatalf("unable to init db: %v", err)
	}

	pControl := NewPaymentControl(strict, db)

	info, _, err := genInfo()
	if err != nil {
		t.Fatalf("unable to generate htlc message: %v", err)
	}

	// Sends base htlc message which initiate base status and move it to
	// StatusInFlight and verifies that it was changed.
	if err := pControl.ClearForTakeoff(info); err != nil {
		t.Fatalf("unable to send htlc message: %v", err)
	}

	assertPaymentStatus(t, db, info.PaymentHash, StatusInFlight)

	// Try to initiate double sending of htlc message with the same
	// payment hash, should result in error indicating that payment has
	// already been sent.
	if err := pControl.ClearForTakeoff(info); err != ErrPaymentInFlight {
		t.Fatalf("payment control wrong behaviour: " +
			"double sending must trigger ErrPaymentInFlight error")
	}
}

// TestPaymentControlSwitchDoublePay checks the ability of payment control to
// prevent double payment.
func testPaymentControlSwitchDoublePay(t *testing.T, strict bool) {
	t.Parallel()

	db, err := initDB()
	if err != nil {
		t.Fatalf("unable to init db: %v", err)
	}

	pControl := NewPaymentControl(strict, db)

	info, settle, err := genInfo()
	if err != nil {
		t.Fatalf("unable to generate htlc message: %v", err)
	}

	// Sends base htlc message which initiate StatusInFlight.
	if err := pControl.ClearForTakeoff(info); err != nil {
		t.Fatalf("unable to send htlc message: %v", err)
	}

	// Verify that payment is InFlight.
	assertPaymentStatus(t, db, info.PaymentHash, StatusInFlight)

	// Move payment to completed status, second payment should return error.
	if err := pControl.Success(info.PaymentHash, settle); err != nil {
		t.Fatalf("error shouldn't have been received, got: %v", err)
	}

	// Verify that payment is Completed.
	assertPaymentStatus(t, db, info.PaymentHash, StatusCompleted)

	if err := pControl.ClearForTakeoff(info); err != ErrAlreadyPaid {
		t.Fatalf("payment control wrong behaviour:" +
			" double payment must trigger ErrAlreadyPaid")
	}
}

// TestPaymentControlNonStrictSuccessesWithoutInFlight checks that a non-strict
// payment control will allow calls to Success when no payment is in flight. This
// is necessary to gracefully handle the case in which the switch already sent
// out a payment for a particular payment hash in a prior db version that didn't
// have payment statuses.
func TestPaymentControlNonStrictSuccessesWithoutInFlight(t *testing.T) {
	t.Parallel()

	db, err := initDB()
	if err != nil {
		t.Fatalf("unable to init db: %v", err)
	}

	pControl := NewPaymentControl(false, db)

	info, settle, err := genInfo()
	if err != nil {
		t.Fatalf("unable to generate htlc message: %v", err)
	}

	if err := pControl.Success(info.PaymentHash, settle); err != nil {
		t.Fatalf("unable to mark payment hash success: %v", err)
	}

	assertPaymentStatus(t, db, info.PaymentHash, StatusCompleted)

	err = pControl.Success(info.PaymentHash, settle)
	if err != ErrPaymentAlreadyCompleted {
		t.Fatalf("unable to remark payment hash failed: %v", err)
	}
}

// TestPaymentControlNonStrictFailsWithoutInFlight checks that a non-strict
// payment control will allow calls to Fail when no payment is in flight. This
// is necessary to gracefully handle the case in which the switch already sent
// out a payment for a particular payment hash in a prior db version that didn't
// have payment statuses.
func TestPaymentControlNonStrictFailsWithoutInFlight(t *testing.T) {
	t.Parallel()

	db, err := initDB()
	if err != nil {
		t.Fatalf("unable to init db: %v", err)
	}

	pControl := NewPaymentControl(false, db)

	info, settle, err := genInfo()
	if err != nil {
		t.Fatalf("unable to generate htlc message: %v", err)
	}

	if err := pControl.Fail(info.PaymentHash); err != nil {
		t.Fatalf("unable to mark payment hash failed: %v", err)
	}

	assertPaymentStatus(t, db, info.PaymentHash, StatusGrounded)

	err = pControl.Fail(info.PaymentHash)
	if err != nil {
		t.Fatalf("unable to remark payment hash failed: %v", err)
	}

	assertPaymentStatus(t, db, info.PaymentHash, StatusGrounded)

	err = pControl.Success(info.PaymentHash, settle)
	if err != nil {
		t.Fatalf("unable to remark payment hash success: %v", err)
	}

	assertPaymentStatus(t, db, info.PaymentHash, StatusCompleted)

	err = pControl.Fail(info.PaymentHash)
	if err != ErrPaymentAlreadyCompleted {
		t.Fatalf("unable to remark payment hash failed: %v", err)
	}

	assertPaymentStatus(t, db, info.PaymentHash, StatusCompleted)
}

// TestPaymentControlStrictSuccessesWithoutInFlight checks that a strict payment
// control will disallow calls to Success when no payment is in flight.
func TestPaymentControlStrictSuccessesWithoutInFlight(t *testing.T) {
	t.Parallel()

	db, err := initDB()
	if err != nil {
		t.Fatalf("unable to init db: %v", err)
	}

	pControl := NewPaymentControl(true, db)

	info, settle, err := genInfo()
	if err != nil {
		t.Fatalf("unable to generate htlc message: %v", err)
	}

	err = pControl.Success(info.PaymentHash, settle)
	if err != ErrPaymentNotInitiated {
		t.Fatalf("expected ErrPaymentNotInitiated, got %v", err)
	}

	assertPaymentStatus(t, db, info.PaymentHash, StatusGrounded)
}

// TestPaymentControlStrictFailsWithoutInFlight checks that a strict payment
// control will disallow calls to Fail when no payment is in flight.
func TestPaymentControlStrictFailsWithoutInFlight(t *testing.T) {
	t.Parallel()

	db, err := initDB()
	if err != nil {
		t.Fatalf("unable to init db: %v", err)
	}

	pControl := NewPaymentControl(true, db)

	info, _, err := genInfo()
	if err != nil {
		t.Fatalf("unable to generate htlc message: %v", err)
	}

	err = pControl.Fail(info.PaymentHash)
	if err != ErrPaymentNotInitiated {
		t.Fatalf("expected ErrPaymentNotInitiated, got %v", err)
	}

	assertPaymentStatus(t, db, info.PaymentHash, StatusGrounded)
}

func assertPaymentStatus(t *testing.T, db *DB,
	hash [32]byte, expStatus PaymentStatus) {

	t.Helper()

	var paymentStatus = StatusGrounded
	err := db.View(func(tx *bbolt.Tx) error {
		payments := tx.Bucket(outgoingPaymentBucket)
		if payments == nil {
			return nil
		}

		bucket := payments.Bucket(hash[:])
		if bucket == nil {
			return nil
		}

		// Get the existing status of this payment, if any.
		paymentStatusBytes := bucket.Get(paymentStatusKey)
		if paymentStatusBytes != nil {
			paymentStatus.FromBytes(paymentStatusBytes)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unable to fetch payment status: %v", err)
	}

	if paymentStatus != expStatus {
		t.Fatalf("payment status mismatch: expected %v, got %v",
			expStatus, paymentStatus)
	}
}

func TestRouteSerialization(t *testing.T) {
	t.Parallel()

	priv, _ := btcec.NewPrivateKey(btcec.S256())
	pub := priv.PubKey()

	hop := &route.Hop{
		PubKeyBytes:      route.NewVertex(pub),
		ChannelID:        12345,
		OutgoingTimeLock: 111,
		AmtToForward:     555,
	}

	route := &route.Route{
		TotalTimeLock: 123,
		TotalFees:     999,
		TotalAmount:   1234567,
		SourcePubKey:  route.NewVertex(pub),
		Hops: []*route.Hop{
			hop,
			hop,
		},
	}

	var b bytes.Buffer
	if err := serializeRoute(&b, route); err != nil {
		t.Fatal(err)
	}

	r := bytes.NewReader(b.Bytes())
	route2, err := deserializeRoute(r)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(route, route2) {
		t.Fatalf("routes not equal: \n%v vs \n%v",
			spew.Sdump(route), spew.Sdump(route2))
	}

}
