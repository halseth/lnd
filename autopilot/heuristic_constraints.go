package autopilot

import (
	"fmt"

	"github.com/btcsuite/btcutil"
)

// HeuristicConstraints is a struct that indicate the constraints an autopilot
// heuristic must adhere to when opening channels.
type HeuristicConstraints struct {
	// MinChanSize is the smallest channel that the autopilot agent should
	// create.
	MinChanSize btcutil.Amount

	// MaxChanSize the largest channel that the autopilot agent should
	// create.
	MaxChanSize btcutil.Amount

	// ChanLimit the maximum number of channels that should be created.
	ChanLimit uint16

	// Allocation the percentage of total funds that should be committed to
	// automatic channel establishment.
	Allocation float64
}

// moreChans returns whether we can open more channels and still be within the
// set constraints. In the case that is possible, the funds available for new
// channels, and the number of additional channels that can opened is also
// returned.
func (h *HeuristicConstraints) moreChans(channels []Channel,
	funds btcutil.Amount) (btcutil.Amount, uint32, bool) {

	// If we're already over our maximum allowed number of channels, then
	// we'll instruct the controller not to create any more channels.
	if len(channels) >= int(h.ChanLimit) {
		return 0, 0, false
	}

	// The number of additional channels that should be opened is the
	// difference between the channel limit, and the number of channels we
	// already have open.
	numAdditionalChans := uint32(h.ChanLimit) - uint32(len(channels))

	// First, we'll tally up the total amount of funds that are currently
	// present within the set of active channels.
	var totalChanAllocation btcutil.Amount
	for _, channel := range channels {
		totalChanAllocation += channel.Capacity
	}

	// With this value known, we'll now compute the total amount of fund
	// allocated across regular utxo's and channel utxo's.
	totalFunds := funds + totalChanAllocation

	// Once the total amount has been computed, we then calculate the
	// fraction of funds currently allocated to channels.
	fundsFraction := float64(totalChanAllocation) / float64(totalFunds)

	// If this fraction is below our threshold, then we'll return true, to
	// indicate the controller should call Select to obtain a candidate set
	// of channels to attempt to open.
	needMore := fundsFraction < h.Allocation
	if !needMore {
		return 0, 0, false
	}

	// Now that we know we need more funds, we'll compute the amount of
	// additional funds we should allocate towards channels.
	targetAllocation := btcutil.Amount(float64(totalFunds) * h.Allocation)
	fundsAvailable := targetAllocation - totalChanAllocation
	return fundsAvailable, numAdditionalChans, true
}

// distribute will take the available funds and passed directives, and return a
// slice of directives where these funds are properly distributed.
func (h *HeuristicConstraints) distribute(fundsAvailable btcutil.Amount,
	directives []AttachmentDirective) ([]AttachmentDirective, error) {

	numSelectedNodes := int64(len(directives))
	switch {
	// If we have enough available funds to distribute the maximum channel
	// size for each of the selected peers to attach to, then we'll
	// allocate the maximum amount to each peer.
	case int64(fundsAvailable) >= numSelectedNodes*int64(h.MaxChanSize):
		for i := 0; i < int(numSelectedNodes); i++ {
			directives[i].ChanAmt = h.MaxChanSize
		}

		return directives, nil

	// Otherwise, we'll greedily allocate our funds to the channels
	// successively until we run out of available funds, or can't create a
	// channel above the min channel size.
	case int64(fundsAvailable) < numSelectedNodes*int64(h.MaxChanSize):
		i := 0
		for fundsAvailable > h.MinChanSize {
			// We'll attempt to allocate the max channel size
			// initially. If we don't have enough funds to do this,
			// then we'll allocate the remainder of the funds
			// available to the channel.
			delta := h.MaxChanSize
			if fundsAvailable-delta < 0 {
				delta = fundsAvailable
			}

			directives[i].ChanAmt = delta

			fundsAvailable -= delta
			i++
		}

		// We'll slice the initial set of directives to properly
		// reflect the amount of funds we were able to allocate.
		return directives[:i:i], nil

	default:
		return nil, fmt.Errorf("err")
	}
}
