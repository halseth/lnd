package machine

import "github.com/lightningnetwork/lnd/lnwire"

type DirectedEdge struct {
	ChanID    lnwire.ChannelID
	Direction int
}

type MachineCfg struct {
	OnGraphUpdate <-chan struct{}
}

type ProbabilityMachine interface {
	Prob(DirectedEdge, lnwire.MilliSatoshi) (float64, error)
}

type pruneMachine struct {
}

var _ ProbabilityMachine = (*pruneMachine)(nil)

func New(cfg *MachineCfg) *pruneMachine {
	return &pruneMachine{}
}

func (p *pruneMachine) Prob(DirectedEdge, lnwire.MilliSatoshi) (float64, error) {
	return 0, nil
}
