package routing

import (
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
)

// MissionControl is an interface that abstracts an implementatation of a
// mechanism for getting PaymentSession for payments in the channel graph.
type MissionControl interface {
	// NewPaymentSession returns a new PaymentSession, that can be used to
	// get possible paths for a particular payment. The passed arguments is
	// used to craft a PaymentSession tailored for this one payment.
	NewPaymentSession(additionalEdges map[Vertex][]*edgePolicyWithSource,
		target *btcec.PublicKey) (PaymentSession, error)

	// CommitDecay is used to tell the MissionControl instance that some
	// time have passed, either in clock time or in blocks. This should be
	// used by the implementation to update its current state of the graph
	// in response to the passed time.
	CommitDecay(t time.Time, blockHeight uint32) error
}

// PaymentSession is an interface type returned from MissionControl, exposing
// all the methods used for carrying out a payment in the dynamically changing
// graph environment.
type PaymentSession interface {
	// ReportRouteSuccess is used to tell the PaymentSession that the
	// provided route successfully carried a payment.
	ReportRouteSuccess(route Route) error

	// ReportEdgeFailure is used to tell the PaymentSession that the
	// provided channel failed to carry a payment of size amt.
	ReportChannelFailure(failedEdge *ChannelHop) error

	// ReportVertexFailure is used to tell the PaymentSession that the
	// provided vertex failed while being used in a route.
	ReportVertexFailure(failedNode Vertex) error

	// EdgeWeight is used to get a weight for the provided edge to carry the
	// amount. This weight should be used by path finding algorithms to find
	// the "best route" to the destination.
	//
	// NOTE: A lower weight is better, to make it easily compatible with
	// standard shortest path algorithms.
	EdgeWeight(amt lnwire.MilliSatoshi,
		e *channeldb.ChannelEdgePolicy) (int64, error)

	// ForEachNode calls the provided function with the nodes in the
	// current channel graph.
	ForEachNode(func(Vertex) error) error

	// ForEachChannel calls the provided function with the channels found
	// towards the given node in the current channel graph.
	ForEachChannel(Vertex, func(fromNode Vertex,
		inEdge *channeldb.ChannelEdgePolicy,
		bandwidth lnwire.MilliSatoshi) error) error
}
