package autopilot

import (
	"sync"

	"github.com/btcsuite/btcutil"
)

// ExternalScoreAttachment is an implementation of the AttachmentHeuristic
// interface that allows an external source to provide it with node scores.
type ExternalScoreAttachment struct {
	nodeScores map[NodeID]float64
	sync.Mutex
}

// NewExternalScoreAttachment creates a new instance of an
// ExternalScoreAttachment.
func NewExternalScoreAttachment() AttachmentHeuristic {
	return &ExternalScoreAttachment{}
}

// A compile time assertion to ensure ExternalScoreAttachment meets the
// AttachmentHeuristic interface.
var _ AttachmentHeuristic = (*ExternalScoreAttachment)(nil)

// SetNodeScores is used to set the internal map from NodeIDs to scores.
func (s *ExternalScoreAttachment) SetNodeScores(
	newScores map[NodeID]float64) error {

	s.Lock()
	defer s.Unlock()

	s.nodeScores = newScores
	return nil
}

// Name returns the name of this heuristic.
//
// NOTE: This is a part of the AttachmentHeuristic interface.
func (c *ExternalScoreAttachment) Name() string {
	return "externalscore"
}

// NodeScores is a method that given the current channel graph and current set
// of local channels, scores the given nodes according to the preference of
// opening a channel of the given size with them. The returned channel
// candidates maps the NodeID to a NodeScore for the node.
//
// The scores are determined by checking the internal node scores list.
// Nodes with no known addresses will be scored 0. The scores returned are
// floats in the range in the range [0, M], where 0 indicates no improvement in
// connectivity if a channel is opened to this node, while M is the maximum
// possible improvement in connectivity.

// NOTE: This is a part of the AttachmentHeuristic interface.
func (s *ExternalScoreAttachment) NodeScores(g ChannelGraph, chans []Channel,
	chanSize btcutil.Amount, nodes map[NodeID]struct{}) (
	map[NodeID]*NodeScore, error) {

	existingPeers := make(map[NodeID]struct{})
	for _, c := range chans {
		existingPeers[c.Node] = struct{}{}
	}

	s.Lock()
	defer s.Unlock()

	// Fill the map of candidates to return.
	candidates := make(map[NodeID]*NodeScore)
	for nID := range nodes {
		_, ok := existingPeers[nID]
		score := s.nodeScores[nID]

		switch {

		// If the node is among our existing channel peers, we don't
		// need another channel.
		case ok:
			continue

		// Instead of adding a node with score 0 to the returned set,
		// we just skip it.
		case score == 0:
			continue
		}

		candidates[nID] = &NodeScore{
			NodeID: nID,
			Score:  score,
		}
	}

	return candidates, nil
}
