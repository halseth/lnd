package routing

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// BlockPadding is used to increment the finalCltvDelta value for the last hop
// to prevent an HTLC being failed if some blocks are mined while it's in-flight.
const BlockPadding uint16 = 3

var (
	// errpreBuiltRouteTried is returned when the single pre-built route
	// failed and there is nothing more we can do.
	errPrebuiltRouteTried = errors.New("pre-built route already tried")

	errShutdown = errors.New("payment session shutting down")
)

type ShardResult struct {
	Preimage lntypes.Preimage
	Err      error
}

type PaymentShard struct {
	Route      *route.Route
	ResultChan chan *ShardResult
}

// PaymentSession is used during SendPayment attempts to provide routes to
// attempt. It also defines methods to give the PaymentSession additional
// information learned during the previous attempts.
type PaymentSession interface {
	// RequestRoutes intructs the PaymentSession to find more routes that
	// in total send up to amt.
	RequestRoute(amt lnwire.MilliSatoshi, payment *LightningPayment,
		height uint32, finalCltvDelta uint16) (chan *PaymentShard, chan error)

	AddRoute(*route.Route) (lntypes.Preimage, error)

	Start()
	Stop()
}

type paymentSessionBuilder struct {
	routes  chan *PaymentShard
	timeout time.Duration

	errChan chan error
	quit    chan struct{}

	addRouteSignal chan struct{}

	wg sync.WaitGroup
	sync.Mutex
}

func (p *paymentSessionBuilder) Start() {
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()

		for {
			select {
			case <-time.After(p.timeout):
				select {
				case p.errChan <- fmt.Errorf("pays session timeout"):
				default:
				}
				return
			case <-p.addRouteSignal:
			case <-p.quit:
				return
			}
		}
	}()
}

func (p *paymentSessionBuilder) Stop() {
	close(p.quit)
	p.wg.Wait()
}

// AddRoute can be used to manually add routes to the PaymentSession.
func (p *paymentSessionBuilder) AddRoute(r *route.Route) (lntypes.Preimage, error) {

	select {
	case p.addRouteSignal <- struct{}{}:
	default:
	}

	result := make(chan *ShardResult, 1)
	shard := &PaymentShard{
		Route:      r,
		ResultChan: result,
	}

	select {
	case p.routes <- shard:
	case <-p.quit:
		return lntypes.Preimage{}, errShutdown
	}

	select {
	case res := <-result:
		return res.Preimage, res.Err
	case <-p.quit:
		return lntypes.Preimage{}, errShutdown
	}
}

func (p *paymentSessionBuilder) RequestRoute(amt lnwire.MilliSatoshi,
	payment *LightningPayment, height uint32, finalCltvDelta uint16) (
	chan *PaymentShard, chan error) {

	return p.routes, p.errChan
}

// paymentSession is used during an HTLC routings session to prune the local
// chain view in response to failures, and also report those failures back to
// MissionControl. The snapshot copied for this session will only ever grow,
// and will now be pruned after a decay like the main view within mission
// control. We do this as we want to avoid the case where we continually try a
// bad edge or route multiple times in a session. This can lead to an infinite
// loop if payment attempts take long enough. An additional set of edges can
// also be provided to assist in reaching the payment's destination.
type paymentSession struct {
	additionalEdges map[route.Vertex][]*channeldb.ChannelEdgePolicy

	getBandwidthHints func() (map[uint64]lnwire.MilliSatoshi, error)

	sessionSource *SessionSource

	pathFinder pathFinder
	wg         sync.WaitGroup
}

func (p *paymentSession) Start() {
}

func (p *paymentSession) Stop() {
	p.wg.Wait()
}

func (p *paymentSession) AddRoute(r *route.Route) (lntypes.Preimage, error) {
	return lntypes.Preimage{}, fmt.Errorf("unimplemented")
}

// RequestRoute returns a route which is likely to be capable for successfully
// routing the specified HTLC payment to the target node. Initially the first
// set of paths returned from this method may encounter routing failure along
// the way, however as more payments are sent, mission control will start to
// build an up to date view of the network itself. With each payment a new area
// will be explored, which feeds into the recommendations made for routing.
//
// NOTE: This function is safe for concurrent access.
// NOTE: Part of the PaymentSession interface.
func (p *paymentSession) RequestRoute(amt lnwire.MilliSatoshi,
	payment *LightningPayment,
	height uint32, finalCltvDelta uint16) (chan *PaymentShard, chan error) {

	respChan := make(chan *PaymentShard, 1)
	errChan := make(chan error, 1)

	p.wg.Add(1)
	go p.requestRoute(amt, payment, height, finalCltvDelta, respChan, errChan)

	return respChan, errChan
}

func (p *paymentSession) requestRoute(amt lnwire.MilliSatoshi,
	payment *LightningPayment,
	height uint32, finalCltvDelta uint16, respChan chan *PaymentShard, errChan chan error) {

	defer p.wg.Done()

	// Add BlockPadding to the finalCltvDelta so that the receiving node
	// does not reject the HTLC if some blocks are mined while it's in-flight.
	finalCltvDelta += BlockPadding

	// We need to subtract the final delta before passing it into path
	// finding. The optimal path is independent of the final cltv delta and
	// the path finding algorithm is unaware of this value.
	cltvLimit := payment.CltvLimit - uint32(finalCltvDelta)

	// TODO(roasbeef): sync logic amongst dist sys

	// Taking into account this prune view, we'll attempt to locate a path
	// to our destination, respecting the recommendations from
	// MissionControl.
	ss := p.sessionSource

	restrictions := &RestrictParams{
		ProbabilitySource: ss.MissionControl.GetProbability,
		FeeLimit:          payment.FeeLimit,
		OutgoingChannelID: payment.OutgoingChannelID,
		LastHop:           payment.LastHop,
		CltvLimit:         cltvLimit,
		DestCustomRecords: payment.DestCustomRecords,
		DestFeatures:      payment.DestFeatures,
		PaymentAddr:       payment.PaymentAddr,
	}

	// We'll also obtain a set of bandwidthHints from the lower layer for
	// each of our outbound channels. This will allow the path finding to
	// skip any links that aren't active or just don't have enough bandwidth
	// to carry the payment. New bandwidth hints are queried for every new
	// path finding attempt, because concurrent payments may change
	// balances.
	bandwidthHints, err := p.getBandwidthHints()
	if err != nil {
		errChan <- err
		return
	}

	finalHtlcExpiry := int32(height) + int32(finalCltvDelta)

	path, err := p.pathFinder(
		&graphParams{
			graph:           ss.Graph,
			additionalEdges: p.additionalEdges,
			bandwidthHints:  bandwidthHints,
		},
		restrictions, &ss.PathFindingConfig,
		ss.SelfNode.PubKeyBytes, payment.Target,
		amt, finalHtlcExpiry,
	)
	if err != nil {
		errChan <- err
		return

	}

	// With the next candidate path found, we'll attempt to turn this into
	// a route by applying the time-lock and fee requirements.
	sourceVertex := route.Vertex(ss.SelfNode.PubKeyBytes)
	rt, err := newRoute(
		sourceVertex, path, height,
		finalHopParams{
			amt:         amt,
			cltvDelta:   finalCltvDelta,
			records:     payment.DestCustomRecords,
			paymentAddr: payment.PaymentAddr,
		},
	)
	if err != nil {
		// TODO(roasbeef): return which edge/vertex didn't work
		// out
		errChan <- err
		return

	}

	s := &PaymentShard{
		Route:      rt,
		ResultChan: make(chan *ShardResult, 1), // ignored
	}

	respChan <- s
}
