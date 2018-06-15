package routing

import (
	"testing"
)

var (
	missionControls []MissionControl
)

func init() {

	//	mc := newMissionControl
}

func TestRoutingPerformance(t *testing.T) {

	//	// First we'll set up a test graph for usage within the test.
	//	graph, cleanup, err := makeTestGraph()
	//	if err != nil {
	//		t.Fatalf("unable to create test graph: %v", err)
	//	}
	//	defer cleanup()
	//
	//	sourceNode, err := createTestNode()
	//	if err != nil {
	//		t.Fatalf("unable to create source node: %v", err)
	//	}
	//	if err = graph.SetSourceNode(sourceNode); err != nil {
	//		t.Fatalf("unable to set source node: %v", err)
	//	}
	//
	//	numNodes := 100
	//	var nodes []*channeldb.LightningNode
	//	nodes = append(nodes, sourceNode)
	//	for i := 0; i < numNodes; i++ {
	//		node, err := createTestNode()
	//		if err != nil {
	//			t.Fatalf("unable to create node: %v", err)
	//		}
	//		if err := graph.AddLightningNode(node); err != nil {
	//			t.Fatalf("unable to add node: %v", err)
	//		}
	//		nodes = append(nodes, node)
	//	}
	//
	//	payAmt := btcutil.Amount(100000)
	//	chanCap := payAmt * 2
	//	for _, a := range nodes {
	//		for _, b := range nodes {
	//			chanID := prand.Uint64()
	//			edge := &channeldb.ChannelEdgeInfo{
	//				Capacity:      chanCap,
	//				ChannelID:     chanID,
	//				NodeKey1Bytes: a.PubKeyBytes,
	//				NodeKey2Bytes: b.PubKeyBytes,
	//				AuthProof:     nil,
	//			}
	//			err := graph.AddChannelEdge(edge)
	//			if err != nil {
	//				t.Fatalf("unable to add edge: %v", err)
	//			}
	//		}
	//	}
	//
	//	bandwidth := func(_ *channeldb.ChannelEdgeInfo) lnwire.MilliSatoshi {
	//		return lnwire.NewMSatFromSatoshis(chanCap)
	//	}
	//
	//	mc := newMissionControl(graph, sourceNode, bandwidth)
	//	amt := lnwire.NewMSatFromSatoshis(payAmt)
	//
	//	for _, target := range nodes {
	//		pk, err := target.PubKey()
	//		if err != nil {
	//			t.Fatalf("unable to get pubkey: %v", err)
	//		}
	//		paySession, err := mc.NewPaymentSession(nil, amt, pk)
	//		if err != nil {
	//			t.Fatalf("unable to create PaymentSession: %v", err)
	//		}
	//		fmt.Println(paySession)
	//	}

}
