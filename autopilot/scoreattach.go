package autopilot

import (
	"bytes"
	"sort"
	"sync"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcutil"
)

// ScoreAttachment is an implementation of the AttachmentHeuristic
// interface that
type ScoreAttachment struct {
	constraints *HeuristicConstraints

	// TODO: float?
	// TODO: nodes with same score should be randomized.
	nodeScores map[NodeID]uint32

	sync.Mutex
}

// NewScoreAttachment creates a new instance of a
// ScoreAttachment
func NewScoreAttachment(cfg *HeuristicConstraints) *ScoreAttachment {
	return &ScoreAttachment{
		constraints: cfg,
	}
}

// A compile time assertion to ensure ScoreAttachment meets the
// AttachmentHeuristic interface.
var _ AttachmentHeuristic = (*ScoreAttachment)(nil)

// SetNodeScores is used to set the internal map from NodeIDs to scores.
func (s *ScoreAttachment) SetNodeScores(newScores map[NodeID]uint32) {
	s.Lock()
	defer s.Unlock()

	s.nodeScores = newScores
}

// NeedMoreChans is a predicate that should return true if, given the passed
// parameters, and its internal state, more channels should be opened within
// the channel graph. If the heuristic decides that we do indeed need more
// channels, then the second argument returned will represent the amount of
// additional funds to be used towards creating channels.
//
// NOTE: This is a part of the AttachmentHeuristic interface.
func (s *ScoreAttachment) NeedMoreChans(channels []Channel,
	funds btcutil.Amount) (btcutil.Amount, uint32, bool) {

	// First check the heuristic constraints whether we can open more
	// channels.
	fundsAvailable, numAdditionalChans, needMore := s.constraints.moreChans(
		channels, funds,
	)
	if !needMore {
		return 0, 0, false
	}

	s.Lock()
	defer s.Unlock()

	// Check if there are any of the preferred nodes left that we don't
	// have channels to.
	candidates := make(map[NodeID]struct{})
	for node, _ := range s.nodeScores {
		candidates[node] = struct{}{}
	}

	for _, c := range channels {
		delete(candidates, c.Node)
	}

	// If none of the candidates are free, then this heuristic cannot open
	// more channels.
	freeCandidate := len(candidates) > 0
	if !freeCandidate {
		return 0, 0, false
	}

	return fundsAvailable, numAdditionalChans, true
}

// Select returns a candidate set of attachment directives that should be
// executed base on the internal set of node scores, and the current state of
// the channels set.
//
// NOTE: This is a part of the AttachmentHeuristic interface.
func (s *ScoreAttachment) Select(self *btcec.PublicKey, g ChannelGraph,
	fundsAvailable btcutil.Amount, numNewChans uint32,
	skipNodes map[NodeID]struct{}) ([]AttachmentDirective, error) {

	var directives []AttachmentDirective

	// Return immediately if we don't have enough funds to meet the minumum
	// channel size.
	if fundsAvailable < s.constraints.MinChanSize {
		return directives, nil
	}

	selfPubBytes := self.SerializeCompressed()

	s.Lock()
	defer s.Unlock()

	// Gather all of the preferred nodes that shouldn't be skipped.
	var prefNodes []NodeID
	for node, _ := range s.nodeScores {
		if _, ok := skipNodes[node]; ok {
			continue
		}
		if bytes.Equal(node[:], selfPubBytes) {
			continue
		}

		prefNodes = append(prefNodes, node)
	}

	// Sort them, such that the nodes with the highest score will come
	// first in the slice.
	sort.Slice(prefNodes, func(i, j int) bool {
		return s.nodeScores[prefNodes[i]] > s.nodeScores[prefNodes[j]]
	})

	// We'll continue our attachment loop until we've exhausted the current
	// amount of available funds.
	for _, node := range prefNodes {
		if len(directives) >= int(numNewChans) {
			break
		}

		selectedNode, err := g.FetchNode(node)
		if err != nil {
			log.Errorf("Unable to fetch node: %v", err)
			continue
		}

		// With the node selected, we'll add this (node, amount) tuple
		// to out set of recommended directives.
		pubBytes := selectedNode.PubKey()
		pub, err := btcec.ParsePubKey(pubBytes[:], btcec.S256())
		if err != nil {
			return nil, err
		}
		directives = append(directives, AttachmentDirective{
			// TODO(roasbeef): need curve?
			NodeKey: &btcec.PublicKey{
				X: pub.X,
				Y: pub.Y,
			},
			NodeID: NewNodeID(pub),
			Addrs:  selectedNode.Addrs(),
		})
	}

	return s.constraints.distribute(fundsAvailable, directives)
}
