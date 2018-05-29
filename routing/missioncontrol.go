package routing

import (
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/coreos/bbolt"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
)

const (
	// vertexDecay is the decay period of colored vertexes added to
	// missionControl. Once vertexDecay passes after an entry has been
	// added to the prune view, it is garbage collected. This value is
	// larger than edgeDecay as an edge failure typical indicates an
	// unbalanced channel, while a vertex failure indicates a node is not
	// online and active.
	vertexDecay = time.Duration(time.Minute * 5)

	// edgeDecay is the decay period of colored edges added to
	// missionControl. Once edgeDecay passed after an entry has been added,
	// it is garbage collected. This value is smaller than vertexDecay as
	// an edge related failure during payment sending typically indicates
	// that a channel was unbalanced, a condition which may quickly change.
	//
	// TODO(roasbeef): instead use random delay on each?
	edgeDecay = time.Duration(time.Second * 5)

	// riskFactorBillionths controls the influence of time lock delta
	// of a channel on route selection. It is expressed as billionths
	// of msat per msat sent through the channel per time lock delta
	// block. See EdgeWeight function below for more details.
	// The chosen value is based on the previous incorrect weight function
	// 1 + timelock + fee * fee. In this function, the fee penalty
	// diminishes the time lock penalty for all but the smallest amounts.
	// To not change the behaviour of path finding too drastically, a
	// relatively small value is chosen which is still big enough to give
	// some effect with smaller time lock values. The value may need
	// tweaking and/or be made configurable in the future.
	riskFactorBillionths = 15
)

// missionControl is an implementation of the MissionControl interface.
//
// It contains state which summarizes the past attempts of HTLC
// routing by external callers when sending payments throughout the network.
// missionControl remembers the outcome of these past routing attempts (success
// and failure), and is able to provide hints/guidance to future HTLC routing
// attempts.  missionControl maintains a decaying network view of the
// edges/vertexes that should be marked as "pruned" during path finding. This
// graph view acts as a shared memory during HTLC payment routing attempts.
// With each execution, if an error is encountered, based on the type of error
// and the location of the error within the route, an edge or vertex is added
// to the view. Later sending attempts will then query the view for all the
// vertexes/edges that should be ignored. Items in the view decay after a set
// period of time, allowing the view to be dynamic w.r.t network changes.
type missionControl struct {
	// failedEdges maps a short channel ID to be pruned, to the time that
	// it was added to the prune view. Edges are added to this map if a
	// caller reports to missionControl a failure localized to that edge
	// when sending a payment.
	failedEdges map[uint64]time.Time

	// failedVertexes maps a node's public key that should be pruned, to
	// the time that it was added to the prune view. Vertexes are added to
	// this map if a caller reports to missionControl a failure localized
	// to that particular vertex.
	failedVertexes map[Vertex]time.Time

	graph *channeldb.ChannelGraph

	selfNode *channeldb.LightningNode

	queryBandwidth func(*channeldb.ChannelEdgeInfo) lnwire.MilliSatoshi

	sync.Mutex

	// TODO(roasbeef): further counters, if vertex continually unavailable,
	// add to another generation

	// TODO(roasbeef): also add favorable metrics for nodes
}

// newMissionControl returns a new instance of missionControl.
//
// TODO(roasbeef): persist memory
func newMissionControl(g *channeldb.ChannelGraph, selfNode *channeldb.LightningNode,
	qb func(*channeldb.ChannelEdgeInfo) lnwire.MilliSatoshi) *missionControl {

	return &missionControl{
		failedEdges:    make(map[uint64]time.Time),
		failedVertexes: make(map[Vertex]time.Time),
		selfNode:       selfNode,
		queryBandwidth: qb,
		graph:          g,
	}
}

// CommitDecay is used to tell the MissionControl instance that some time have
// passed, either in clock time or in blocks. This should be used by the
// implementation to update its current state of the graph in response to the
// passed time.
//
// NOTE: Part of the MissionControl interface.
func (m *missionControl) CommitDecay(_ time.Time, _ uint32) error {
	return nil
}

// graphPruneView is a filter of sorts that path finding routines should
// consult during the execution. Any edges or vertexes within the view should
// be ignored during path finding. The contents of the view reflect the current
// state of the wider network from the PoV of mission control compiled via HTLC
// routing attempts in the past.
type graphPruneView struct {
	edges map[uint64]struct{}

	vertexes map[Vertex]struct{}
}

// GraphPruneView returns a new graphPruneView instance which is to be
// consulted during path finding. If a vertex/edge is found within the returned
// prune view, it is to be ignored as a goroutine has had issues routing
// through it successfully. Within this method the main view of the
// missionControl is garbage collected as entries are detected to be "stale".
func (m *missionControl) GraphPruneView() graphPruneView {
	// First, we'll grab the current time, this value will be used to
	// determine if an entry is stale or not.
	now := time.Now()

	m.Lock()

	// For each of the vertexes that have been added to the prune view, if
	// it is now "stale", then we'll ignore it and avoid adding it to the
	// view we'll return.
	vertexes := make(map[Vertex]struct{})
	for vertex, pruneTime := range m.failedVertexes {
		if now.Sub(pruneTime) >= vertexDecay {
			log.Tracef("Pruning decayed failure report for vertex %v "+
				"from Mission Control", vertex)

			delete(m.failedVertexes, vertex)
			continue
		}

		vertexes[vertex] = struct{}{}
	}

	// We'll also do the same for edges, but use the edgeDecay this time
	// rather than the decay for vertexes.
	edges := make(map[uint64]struct{})
	for edge, pruneTime := range m.failedEdges {
		if now.Sub(pruneTime) >= edgeDecay {
			log.Tracef("Pruning decayed failure report for edge %v "+
				"from Mission Control", edge)

			delete(m.failedEdges, edge)
			continue
		}

		edges[edge] = struct{}{}
	}

	m.Unlock()

	log.Debugf("Mission Control returning prune view of %v edges, %v "+
		"vertexes", len(edges), len(vertexes))

	return graphPruneView{
		edges:    edges,
		vertexes: vertexes,
	}
}

// staticPaymentSession is an implementation of the PaymentSession interface
// that uses the current snapshot of the graph to create routes using a
// "cheapest first" approach.
//
// It used during an HTLC routings session to prune the local
// chain view in response to failures, and also report those failures back to
// missionControl. The snapshot copied for this session will only ever grow,
// and will now be pruned after a decay like the main view within mission
// control. We do this as we want to avoid the case where we continually try a
// bad edge or route multiple times in a session. This can lead to an infinite
// loop if payment attempts take long enough. An additional set of edges can
// also be provided to assist in reaching the payment's destination.
type staticPaymentSession struct {
	pruneViewSnapshot graphPruneView

	additionalEdges map[Vertex][]*edgePolicyWithSource

	bandwidthHints map[uint64]lnwire.MilliSatoshi

	mc *missionControl

	haveRoutes     bool
	preBuiltRoutes []*Route
}

// NewPaymentSession creates a new payment session backed by the latest prune
// view from missionControl. An optional set of routing hints can be provided
// in order to populate additional edges to explore when finding a path to the
// payment's destination.
//
// NOTE: Part of the MissionControl interface.
func (m *missionControl) NewPaymentSession(additionalEdges map[Vertex][]*edgePolicyWithSource,
	target *btcec.PublicKey) (PaymentSession, error) {

	viewSnapshot := m.GraphPruneView()

	// We'll also obtain a set of bandwidthHints from the lower layer for
	// each of our outbound channels. This will allow the path finding to
	// skip any links that aren't active or just don't have enough
	// bandwidth to carry the payment.
	sourceNode, err := m.graph.SourceNode()
	if err != nil {
		return nil, err
	}
	bandwidthHints, err := generateBandwidthHints(
		sourceNode, m.queryBandwidth,
	)
	if err != nil {
		return nil, err
	}

	return &staticPaymentSession{
		pruneViewSnapshot: viewSnapshot,
		additionalEdges:   additionalEdges,
		bandwidthHints:    bandwidthHints,
		mc:                m,
	}, nil
}

// NewPaymentSessionFromRoutes creates a new paymentSession instance that will
// skip all path finding, and will instead utilize a set of pre-built routes.
// This constructor allows callers to specify their own routes which can be
// used for things like channel rebalancing, and swaps.
func (m *missionControl) NewPaymentSessionFromRoutes(routes []*Route) PaymentSession {
	return &staticPaymentSession{
		pruneViewSnapshot: m.GraphPruneView(),
		haveRoutes:        true,
		preBuiltRoutes:    routes,
		mc:                m,
	}
}

// generateBandwidthHints is a helper function that's utilized the main
// findPath function in order to obtain hints from the lower layer w.r.t to the
// available bandwidth of edges on the network. Currently, we'll only obtain
// bandwidth hints for the edges we directly have open ourselves. Obtaining
// these hints allows us to reduce the number of extraneous attempts as we can
// skip channels that are inactive, or just don't have enough bandwidth to
// carry the payment.
func generateBandwidthHints(sourceNode *channeldb.LightningNode,
	queryBandwidth func(*channeldb.ChannelEdgeInfo) lnwire.MilliSatoshi) (map[uint64]lnwire.MilliSatoshi, error) {

	// First, we'll collect the set of outbound edges from the target
	// source node.
	var localChans []*channeldb.ChannelEdgeInfo
	err := sourceNode.ForEachChannel(nil, func(tx *bolt.Tx,
		edgeInfo *channeldb.ChannelEdgeInfo,
		_, _ *channeldb.ChannelEdgePolicy) error {

		localChans = append(localChans, edgeInfo)
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Now that we have all of our outbound edges, we'll populate the set
	// of bandwidth hints, querying the lower switch layer for the most up
	// to date values.
	bandwidthHints := make(map[uint64]lnwire.MilliSatoshi)
	for _, localChan := range localChans {
		bandwidthHints[localChan.ChannelID] = queryBandwidth(localChan)
	}

	return bandwidthHints, nil
}

// ReportVertexFailure adds a vertex to the graph prune view after a client
// reports a routing failure localized to the vertex. The time the vertex was
// added is noted, as it'll be pruned from the shared view after a period of
// vertexDecay. However, the vertex will remain pruned for the *local* session.
// This ensures we don't retry this vertex during the payment attempt.
//
// NOTE: Part of the PaymentSession interface.
func (p *staticPaymentSession) ReportVertexFailure(v Vertex) error {
	log.Debugf("Reporting vertex %v failure to Mission Control", v)

	// First, we'll add the failed vertex to our local prune view snapshot.
	p.pruneViewSnapshot.vertexes[v] = struct{}{}

	// With the vertex added, we'll now report back to the global prune
	// view, with this new piece of information so it can be utilized for
	// new payment sessions.
	p.mc.Lock()
	p.mc.failedVertexes[v] = time.Now()
	p.mc.Unlock()
	return nil
}

// ReportChannelFailure adds a channel to the graph prune view. The time the
// channel was added is noted, as it'll be pruned from the global view after a
// period of edgeDecay. However, the edge will remain pruned for the duration
// of the *local* session. This ensures that we don't flap by continually
// retrying an edge after its pruning has expired.
//
// NOTE: Part of the PaymentSession interface.
// TODO(roasbeef): also add value attempted to send and capacity of channel
func (p *staticPaymentSession) ReportChannelFailure(hop *ChannelHop) error {
	e := hop.ChannelID
	log.Debugf("Reporting edge %v failure to Mission Control", e)

	// First, we'll add the failed edge to our local prune view snapshot.
	p.pruneViewSnapshot.edges[e] = struct{}{}

	// With the edge added, we'll now report back to the global prune view,
	// with this new piece of information so it can be utilized for new
	// payment sessions.
	p.mc.Lock()
	p.mc.failedEdges[e] = time.Now()
	p.mc.Unlock()
	return nil
}

// ReportRouteSuccess is used to tell the PaymentSession that the provided
// route successfully carried a payment.
//
// NOTE: Part of the PaymentSession interface.
func (p *staticPaymentSession) ReportRouteSuccess(route Route) error {
	return nil
}

// RequestRoute returns a route which is likely to be capable for successfully
// routing the specified HTLC payment to the target node. Initially the first
// set of paths returned from this method may encounter routing failure along
// the way, however as more payments are sent, mission control will start to
// build an up to date view of the network itself. With each payment a new area
// will be explored, which feeds into the recommendations made for routing.
//
// NOTE: This function is safe for concurrent access.
func RequestRoute(r PaymentSession, payment *LightningPayment,
	height uint32, finalCltvDelta uint16) (*Route, error) {

	p := r.(*staticPaymentSession)

	switch {
	// If we have a set of pre-built routes, then we'll just pop off the
	// next route from the queue, and use it directly.
	case p.haveRoutes && len(p.preBuiltRoutes) > 0:
		nextRoute := p.preBuiltRoutes[0]
		p.preBuiltRoutes[0] = nil // Set to nil to avoid GC leak.
		p.preBuiltRoutes = p.preBuiltRoutes[1:]

		return nextRoute, nil

	// If we were instantiated with a set of pre-built routes, and we've
	// run out, then we'll return a terminal error.
	case p.haveRoutes && len(p.preBuiltRoutes) == 0:
		return nil, fmt.Errorf("pre-built routes exhausted")
	}

	// Otherwise we actually need to perform path finding, so we'll obtain
	// our current prune view snapshot. This view will only ever grow
	// during the duration of this payment session, never shrinking.
	pruneView := p.pruneViewSnapshot

	log.Debugf("Mission Control session using prune view of %v "+
		"edges, %v vertexes", len(pruneView.edges),
		len(pruneView.vertexes))

	// TODO(roasbeef): sync logic amongst dist sys

	// Taking into account this prune view, we'll attempt to locate a path
	// to our destination, respecting the recommendations from
	// missionControl.
	path, err := findPath(r,
		p.mc.selfNode,
		payment.Target, pruneView.vertexes, pruneView.edges,
		payment.Amount, payment.FeeLimit,
	)
	if err != nil {
		return nil, err
	}

	// With the next candidate path found, we'll attempt to turn this into
	// a route by applying the time-lock and fee requirements.
	sourceVertex := Vertex(p.mc.selfNode.PubKeyBytes)
	route, err := newRoute(
		payment.Amount, payment.FeeLimit, sourceVertex, path, height,
		finalCltvDelta,
	)
	if err != nil {
		// TODO(roasbeef): return which edge/vertex didn't work
		// out
		return nil, err
	}

	return route, err
}

// EdgeWeight computes the weight of an edge. This value is used when searching
// for the shortest path within the channel graph between two nodes. Weight is
// is the fee itself plus a time lock penalty added to it. This benefits
// channels with shorter time lock deltas and shorter (hops) routes in general.
// RiskFactor controls the influence of time lock on route selection. This is
// currently a fixed value, but might be configurable in the future.
//
// NOTE: Part of the PaymentSession interface.
func (p *staticPaymentSession) EdgeWeight(amt lnwire.MilliSatoshi,
	e *channeldb.ChannelEdgePolicy) (int64, error) {
	// If the edge is in the prune view, return max score to ensure it will
	// be ignored.
	if _, ok := p.pruneViewSnapshot.edges[e.ChannelID]; ok {
		return math.MaxInt64, nil
	}

	fee := computeFee(amt, e)
	timeLockDelta := e.TimeLockDelta

	// timeLockPenalty is the penalty for the time lock delta of this channel.
	// It is controlled by RiskFactorBillionths and scales proportional
	// to the amount that will pass through channel. Rationale is that it if
	// a twice as large amount gets locked up, it is twice as bad.
	timeLockPenalty := int64(amt) * int64(timeLockDelta) *
		riskFactorBillionths / 1000000000

	return int64(fee) + timeLockPenalty, nil
}

// ResetHistory resets the history of missionControl returning it to a state as
// if no payment attempts have been made.
func (m *missionControl) ResetHistory() {
	m.Lock()
	m.failedEdges = make(map[uint64]time.Time)
	m.failedVertexes = make(map[Vertex]time.Time)
	m.Unlock()
}

// ForEachNode calls the provided function with the nodes in the current
// channel graph.
//
// NOTE: Part of the PaymentSession interface.
func (p *staticPaymentSession) ForEachNode(f func(Vertex) error) error {
	tx, err := p.mc.graph.Database().Begin(false)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	seen := make(map[Vertex]struct{})

	err = p.mc.graph.ForEachNode(
		tx,
		func(_ *bolt.Tx, node *channeldb.LightningNode) error {
			v := Vertex(node.PubKeyBytes)
			seen[v] = struct{}{}
			return f(v)
		},
	)
	if err != nil {
		return err
	}

	var addVertexes []Vertex
	for v, edges := range p.additionalEdges {
		addVertexes = append(addVertexes, v)
		for _, e := range edges {
			addVertexes = append(addVertexes, e.sourceNode)
		}
	}

	for _, v := range addVertexes {
		if _, ok := seen[v]; !ok {
			seen[v] = struct{}{}
			if err := f(v); err != nil {
				return err
			}
		}
	}
	return nil
}

// ForEachChannel calls the provided function with the channels found towards
// the given node in the current channel graph.
//
// NOTE: Part of the PaymentSession interface.
func (p *staticPaymentSession) ForEachChannel(toNode Vertex,
	f func(Vertex, *channeldb.ChannelEdgePolicy, lnwire.MilliSatoshi) error) error {

	tx, err := p.mc.graph.Database().Begin(false)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	returnWithBandwidth := func(fromNode Vertex,
		e *channeldb.ChannelEdgePolicy,
		capacity lnwire.MilliSatoshi) (Vertex,
		*channeldb.ChannelEdgePolicy, lnwire.MilliSatoshi) {

		edgeBandwidth, ok := p.bandwidthHints[e.ChannelID]
		if !ok {
			// If we don't have a hint for this edge, then
			// we'll just use the known Capacity as the
			// available bandwidth.
			edgeBandwidth = capacity
		}

		return fromNode, e, edgeBandwidth
	}

	for _, e := range p.additionalEdges[toNode] {
		err := f(returnWithBandwidth(e.sourceNode, e.edge, e.bandwidth))
		if err != nil {
			return err
		}
	}

	pubKey, err := btcec.ParsePubKey(toNode[:], btcec.S256())
	if err != nil {
		return err
	}

	node, err := p.mc.graph.FetchLightningNode(pubKey)
	if err != nil && err != channeldb.ErrGraphNodeNotFound {
		return err
	} else if err == channeldb.ErrGraphNodeNotFound {
		return nil
	}

	// TODO: make ForEachChannel just take pubkey instead?
	err = node.ForEachChannel(
		tx,
		func(_ *bolt.Tx, edgeInfo *channeldb.ChannelEdgeInfo,
			_, inEdge *channeldb.ChannelEdgePolicy) error {
			fromNode, err := edgeInfo.FetchOtherNode(
				tx, toNode[:],
			)
			if err != nil {
				return err
			}

			return f(
				returnWithBandwidth(
					fromNode.PubKeyBytes, inEdge, lnwire.NewMSatFromSatoshis(edgeInfo.Capacity),
				),
			)
		},
	)
	if err != nil {
		return err
	}

	return nil
}

var _ MissionControl = (*missionControl)(nil)
var _ PaymentSession = (*staticPaymentSession)(nil)
