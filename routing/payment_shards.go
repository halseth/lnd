package routing

import (
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
)

// paymentShard is a type that wraps an attempt that is part of a (potentially)
// larger payment.
type paymentShard struct {
	*channeldb.HTLCAttemptInfo

	ResultChan chan *RouteResult
}

// PaymentShards holds a set of active payment shards.
type PaymentShards struct {
	PaymentHash lntypes.Hash
	Control     ControlTower

	shards     map[uint64]*paymentShard
	totalValue lnwire.MilliSatoshi
}

// Num returns the number of active shard in our set.
func (p *PaymentShards) Num() int {
	return len(p.shards)
}

// RegisterNewShard checkpoints the passed attempt to the ControlTower, and
// adds it to the set of payment shards.
func (p *PaymentShards) RegisterNewShard(attempt *channeldb.HTLCAttemptInfo,
	resultChan chan *RouteResult) error {

	err := p.Control.RegisterAttempt(p.PaymentHash, attempt)
	if err != nil {
		return err
	}

	return p.AddShard(attempt, resultChan)
}

// AddShard adds the given shard to the set of active payment shards.
func (p *PaymentShards) AddShard(attempt *channeldb.HTLCAttemptInfo,
	resultChan chan *RouteResult) error {

	if p.shards == nil {
		p.shards = make(map[uint64]*paymentShard)
	}

	s := &paymentShard{
		HTLCAttemptInfo: attempt,
		ResultChan:      resultChan,
	}

	// Add the shard and update the total value of the set.
	p.shards[s.AttemptID] = s
	p.totalValue += s.Route.Amt()

	return nil
}

// SettleShard settles the shard with the control tower and removes it from the
// set.
func (p *PaymentShards) SettleShard(attempt *channeldb.HTLCAttemptInfo,
	preimage lntypes.Preimage) error {

	err := p.Control.SettleAttempt(
		p.PaymentHash, attempt.AttemptID, preimage,
	)
	if err != nil {
		return err
	}

	s := p.shards[attempt.AttemptID]
	s.ResultChan <- &RouteResult{
		Preimage: preimage,
	}

	// Remove and updat the total value.
	delete(p.shards, attempt.AttemptID)
	p.totalValue -= attempt.Route.Amt()

	return nil
}

// FailShard fails the shard with the control tower and removes it the set.
func (p *PaymentShards) FailShard(attempt *channeldb.HTLCAttemptInfo,
	sendErr error) error {

	err := p.Control.FailAttempt(
		p.PaymentHash, attempt.AttemptID, sendErr,
	)
	if err != nil {
		return err
	}

	s := p.shards[attempt.AttemptID]
	s.ResultChan <- &RouteResult{
		Err: sendErr,
	}

	// Remove and update the total value.
	delete(p.shards, attempt.AttemptID)
	p.totalValue -= attempt.Route.Amt()
	return nil
}
