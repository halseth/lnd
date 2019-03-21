package routing

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/davecgh/go-spew/spew"
)

func TestRouteSerialization(t *testing.T) {
	t.Parallel()

	hop := &Hop{
		PubKeyBytes:      NewVertex(bitcoinKey1),
		ChannelID:        12345,
		OutgoingTimeLock: 111,
		AmtToForward:     555,
	}

	route := &Route{
		TotalTimeLock: 123,
		TotalFees:     999,
		TotalAmount:   1234567,
		SourcePubKey:  NewVertex(bitcoinKey1),
		Hops: []*Hop{
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
