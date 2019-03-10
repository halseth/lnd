package routing

import (
	"bytes"
	"reflect"
	"testing"
	"time"

	"github.com/davecgh/go-spew/spew"
)

var (
//	priv1, _ = btcec.NewPrivateKey(btcec.S256())
//	pub      = priv1.PubKey()
//
//	testHash = [32]byte{
//		0xb7, 0x94, 0x38, 0x5f, 0x2d, 0x1e, 0xf7, 0xab,
//		0x4d, 0x92, 0x73, 0xd1, 0x90, 0x63, 0x81, 0xb4,
//		0x4f, 0x2f, 0x6f, 0x25, 0x88, 0xa3, 0xef, 0xb9,
//		0x6a, 0x49, 0x18, 0x83, 0x31, 0x98, 0x47, 0x53,
//	}
)

func TestLightningPaymentSerialization(t *testing.T) {
	t.Parallel()

	hint1 := HopHint{
		NodeID:                    bitcoinKey1,
		ChannelID:                 1232,
		FeeBaseMSat:               999,
		FeeProportionalMillionths: 123,
		CLTVExpiryDelta:           111,
	}
	hint2 := HopHint{
		NodeID:                    bitcoinKey2,
		ChannelID:                 232,
		FeeBaseMSat:               99,
		FeeProportionalMillionths: 23,
		CLTVExpiryDelta:           11,
	}

	payment := &LightningPayment{
		Target:            bitcoinKey1,
		Amount:            100,
		FeeLimit:          noFeeLimit,
		PaymentHash:       testHash,
		FinalCLTVDelta:    23,
		PayAttemptTimeout: 10 * time.Second,
		OutgoingChannelID: 333,
		RouteHints: [][]HopHint{
			[]HopHint{
				hint1,
				hint2,
			},
			[]HopHint{
				hint2,
				hint1,
			},
		},
	}

	var b bytes.Buffer
	if err := serializeLightningPayment(&b, payment); err != nil {
		t.Fatal(err)
	}

	r := bytes.NewReader(b.Bytes())
	payment2, err := deserializeLightningPayment(r)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(payment, payment2) {
		t.Fatalf("payments not equal: \n%v vs \n%v",
			spew.Sdump(payment), spew.Sdump(payment2))
	}

}
