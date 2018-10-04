package autopilot

import (
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btcd/btcec"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing"
)

// RPCServerCfg houses a set of values and method that must be passed to the
// RPCServer for it to function properly.
type RPCServerCfg struct {
	// Self is the public key of the lnd instance. It is used to making
	// sure the autopilot is not opening channels to itself.
	Self *btcec.PublicKey

	// PilotCfg is the config of the autopilot agent managed by the
	// RPCServer.
	PilotCfg *Config

	// ChannelState is a function closure that returns the current set of
	// channels managed by this node.
	ChannelState func() ([]Channel, error)

	// SubscribeTransactions is used to get a subscription for transactions
	// relevant to this node's wallet.
	SubscribeTransactions func() (lnwallet.TransactionSubscription, error)

	// SubscribeTopology is used to get a subscription for topology changes
	// on the network.
	SubscribeTopology func() (*routing.TopologyClient, error)
}

// RPCServer is struct that manages an autopilot agent, making it possible to
// enable and disable it at will, and handle it relevant external information.
type RPCServer struct {
	started uint32 // To be used atomically.
	stopped uint32 // To be used atomically.

	cfg *RPCServerCfg

	// pilot is the current autopilot agent. It will be nil if the agent is
	// disabled.
	pilot *Agent

	quit chan struct{}
	wg   sync.WaitGroup
	sync.Mutex
}

// NewRPCServer creates a new instance of the RPCServer from the passed config.
func NewRPCServer(cfg *RPCServerCfg) (*RPCServer, error) {
	return &RPCServer{
		cfg:  cfg,
		quit: make(chan struct{}),
	}, nil
}

// Start starts the RPCServer.
func (r *RPCServer) Start() error {
	if !atomic.CompareAndSwapUint32(&r.started, 0, 1) {
		return nil
	}

	return nil
}

// Stop stops the RPCServer. If a autopilot instance is active, it will also be
// stopped.
func (r *RPCServer) Stop() error {
	if !atomic.CompareAndSwapUint32(&r.stopped, 0, 1) {
		return nil
	}

	if err := r.StopAgent(); err != nil {
		log.Errorf("Unable to stop pilot: %v", err)
	}

	close(r.quit)
	r.wg.Wait()

	return nil
}

// StartAgent creates and starts an autopilot agent from the RPCServer's
// config.
func (r *RPCServer) StartAgent() error {
	r.Lock()
	defer r.Unlock()

	// Already active.
	if r.pilot != nil {
		return nil
	}

	// Next, we'll fetch the current state of open channels from the
	// database to use as initial state for the auto-pilot agent.
	initialChanState, err := r.cfg.ChannelState()
	if err != nil {
		return err
	}

	// Now that we have all the initial dependencies, we can create the
	// auto-pilot instance itself.
	r.pilot, err = New(*r.cfg.PilotCfg, initialChanState)
	if err != nil {
		return err
	}

	if err := r.pilot.Start(); err != nil {
		return err
	}

	// Finally, we'll need to subscribe to two things: incoming
	// transactions that modify the wallet's balance, and also any graph
	// topology updates.
	txnSubscription, err := r.cfg.SubscribeTransactions()
	if err != nil {
		defer r.pilot.Stop()
		return err
	}
	graphSubscription, err := r.cfg.SubscribeTopology()
	if err != nil {
		defer r.pilot.Stop()
		defer txnSubscription.Cancel()
		return err
	}

	// We'll launch a goroutine to provide the agent with notifications
	// whenever the balance of the wallet changes.
	// TODO(halseth): can lead to panic if in process of shutting down.
	r.wg.Add(2)
	go func() {
		defer txnSubscription.Cancel()
		defer r.wg.Done()

		for {
			select {
			case <-txnSubscription.ConfirmedTransactions():
				r.pilot.OnBalanceChange()

			case <-r.pilot.quit:
				return
			case <-r.quit:
				return
			}
		}

	}()
	go func() {
		defer r.wg.Done()

		for {
			select {
			// We won't act upon new unconfirmed transaction, as
			// we'll only use confirmed outputs when funding.
			// However, we will still drain this request in order
			// to avoid goroutine leaks, and ensure we promptly
			// read from the channel if available.
			case <-txnSubscription.UnconfirmedTransactions():

			case <-r.pilot.quit:
				return
			case <-r.quit:
				return
			}
		}

	}()

	// We'll also launch a goroutine to provide the agent with
	// notifications for when the graph topology controlled by the node
	// changes.
	r.wg.Add(1)
	go func() {
		defer graphSubscription.Cancel()
		defer r.wg.Done()

		for {
			select {
			case topChange, ok := <-graphSubscription.TopologyChanges:
				// If the router is shutting down, then we will
				// as well.
				if !ok {
					return
				}

				for _, edgeUpdate := range topChange.ChannelEdgeUpdates {
					// If this isn't an advertisement by
					// the backing lnd node, then we'll
					// continue as we only want to add
					// channels that we've created
					// ourselves.
					if !edgeUpdate.AdvertisingNode.IsEqual(r.cfg.Self) {
						continue
					}

					// If this is indeed a channel we
					// opened, then we'll convert it to the
					// autopilot.Channel format, and notify
					// the pilot of the new channel.
					chanNode := NewNodeID(
						edgeUpdate.ConnectingNode,
					)
					chanID := lnwire.NewShortChanIDFromInt(
						edgeUpdate.ChanID,
					)
					edge := Channel{
						ChanID:   chanID,
						Capacity: edgeUpdate.Capacity,
						Node:     chanNode,
					}
					r.pilot.OnChannelOpen(edge)
				}

				// For each closed channel, we'll obtain
				// the chanID of the closed channel and send it
				// to the pilot.
				for _, chanClose := range topChange.ClosedChannels {
					chanID := lnwire.NewShortChanIDFromInt(
						chanClose.ChanID,
					)

					r.pilot.OnChannelClose(chanID)
				}

				// If new nodes were added to the graph, or nod
				// information has changed, we'll poke autopilot
				// to see if it can make use of them.
				if len(topChange.NodeUpdates) > 0 {
					r.pilot.OnNodeUpdates()
				}

			case <-r.pilot.quit:
				return
			case <-r.quit:
				return
			}
		}
	}()

	return nil
}

// StopAgent stops any active autopilot agent.
func (r *RPCServer) StopAgent() error {
	r.Lock()
	defer r.Unlock()

	// Not active, so we can return early.
	if r.pilot == nil {
		return nil
	}

	if err := r.pilot.Stop(); err != nil {
		return err
	}

	// Make sure to nil the current agent, indicating it is no longer
	// active.
	r.pilot = nil

	return nil
}
