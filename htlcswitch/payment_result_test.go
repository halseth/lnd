package htlcswitch

import (
	"bytes"
	"math/rand"
	"reflect"
	"testing"

	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/lntypes"
)

// TestPaymentResultSerialization checks that PaymentResults are properly
// (de)serialized.
func TestPaymentResultSerialization(t *testing.T) {
	t.Parallel()

	var preimage lntypes.Preimage
	if _, err := rand.Read(preimage[:]); err != nil {
		t.Fatalf("unable gen rand preimag: %v", err)
	}

	testCases := []*PaymentResult{
		{
			Type:     PaymentResultSuccess,
			Preimage: preimage,
		},
		{
			Type:   PaymentResultLocalError,
			Reason: preimage[:],
		},
		{
			Type:     PaymentResultLocalError,
			Preimage: preimage,
			Reason:   preimage[:],
		},
	}

	for _, p := range testCases {
		var buf bytes.Buffer
		if err := serializePaymentResult(&buf, p); err != nil {
			t.Fatalf("serialize failed: %v", err)
		}

		r := bytes.NewReader(buf.Bytes())
		p1, err := deserializePaymentResult(r)
		if err != nil {
			t.Fatalf("unable to deserizlize: %v", err)
		}

		if !reflect.DeepEqual(p, p1) {
			t.Fatalf("not equal. %v vs %v", spew.Sdump(p), spew.Sdump(p1))
		}
	}
}
