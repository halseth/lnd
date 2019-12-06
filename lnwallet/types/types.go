package types

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwallet/commitmenttx"
	"github.com/lightningnetwork/lnd/lnwire"
)

// updateType is the exact type of an entry within the shared HTLC log.
type updateType uint8

const (
	// Add is an update type that adds a new HTLC entry into the log.
	// Either side can add a new pending HTLC by adding a new Add entry
	// into their update log.
	Add updateType = iota

	// Fail is an update type which removes a prior HTLC entry from the
	// log. Adding a Fail entry to ones log will modify the _remote_
	// parties update log once a new commitment view has been evaluated
	// which contains the Fail entry.
	Fail

	// MalformedFail is an update type which removes a prior HTLC entry
	// from the log. Adding a MalformedFail entry to ones log will modify
	// the _remote_ parties update log once a new commitment view has been
	// evaluated which contains the MalformedFail entry. The difference
	// from Fail type lie in the different data we have to store.
	MalformedFail

	// Settle is an update type which settles a prior HTLC crediting the
	// balance of the receiving node. Adding a Settle entry to a log will
	// result in the settle entry being removed on the log as well as the
	// original add entry from the remote party's log after the next state
	// transition.
	Settle

	// FeeUpdate is an update type sent by the channel initiator that
	// updates the fee rate used when signing the commitment transaction.
	FeeUpdate
)

// String returns a human readable string that uniquely identifies the target
// update type.
func (u updateType) String() string {
	switch u {
	case Add:
		return "Add"
	case Fail:
		return "Fail"
	case MalformedFail:
		return "MalformedFail"
	case Settle:
		return "Settle"
	case FeeUpdate:
		return "FeeUpdate"
	default:
		return "<unknown type>"
	}
}

// PaymentDescriptor represents a commitment state update which either adds,
// settles, or removes an HTLC. PaymentDescriptors encapsulate all necessary
// metadata w.r.t to an HTLC, and additional data pairing a settle message to
// the original added HTLC.
//
// TODO(roasbeef): LogEntry interface??
//  * need to separate attrs for cancel/add/settle/feeupdate
type PaymentDescriptor struct {
	// RHash is the payment hash for this HTLC. The HTLC can be settled iff
	// the preimage to this hash is presented.
	RHash lntypes.Hash

	// RPreimage is the preimage that settles the HTLC pointed to within the
	// log by the ParentIndex.
	RPreimage lntypes.Preimage

	// Timeout is the absolute timeout in blocks, after which this HTLC
	// expires.
	Timeout uint32

	// Amount is the HTLC amount in milli-satoshis.
	Amount lnwire.MilliSatoshi

	// LogIndex is the log entry number that his HTLC update has within the
	// log. Depending on if IsIncoming is true, this is either an entry the
	// remote party added, or one that we added locally.
	LogIndex uint64

	// HtlcIndex is the index within the main update log for this HTLC.
	// Entries within the log of type Add will have this field populated,
	// as other entries will point to the entry via this counter.
	//
	// NOTE: This field will only be populate if EntryType is Add.
	HtlcIndex uint64

	// ParentIndex is the HTLC index of the entry that this update settles or
	// times out.
	//
	// NOTE: This field will only be populate if EntryType is Fail or
	// Settle.
	ParentIndex uint64

	// SourceRef points to an Add update in a forwarding package owned by
	// this channel.
	//
	// NOTE: This field will only be populated if EntryType is Fail or
	// Settle.
	SourceRef *channeldb.AddRef

	// DestRef points to a Fail/Settle update in another link's forwarding
	// package.
	//
	// NOTE: This field will only be populated if EntryType is Fail or
	// Settle, and the forwarded Add successfully included in an outgoing
	// link's commitment txn.
	DestRef *channeldb.SettleFailRef

	// OpenCircuitKey references the incoming Chan/HTLC ID of an Add HTLC
	// packet delivered by the switch.
	//
	// NOTE: This field is only populated for payment descriptors in the
	// *local* update log, and if the Add packet was delivered by the
	// switch.
	OpenCircuitKey *channeldb.CircuitKey

	// ClosedCircuitKey references the incoming Chan/HTLC ID of the Add HTLC
	// that opened the circuit.
	//
	// NOTE: This field is only populated for payment descriptors in the
	// *local* update log, and if settle/fails have a committed circuit in
	// the circuit map.
	ClosedCircuitKey *channeldb.CircuitKey

	// localOutputIndex is the output index of this HTLc output in the
	// commitment transaction of the local node.
	//
	// NOTE: If the output is dust from the PoV of the local commitment
	// chain, then this value will be -1.
	LocalOutputIndex int32

	// remoteOutputIndex is the output index of this HTLC output in the
	// commitment transaction of the remote node.
	//
	// NOTE: If the output is dust from the PoV of the remote commitment
	// chain, then this value will be -1.
	RemoteOutputIndex int32

	// sig is the signature for the second-level HTLC transaction that
	// spends the version of this HTLC on the commitment transaction of the
	// local node. This signature is generated by the remote node and
	// stored by the local node in the case that local node needs to
	// broadcast their commitment transaction.
	Sig *btcec.Signature

	// addCommitHeight[Remote|Local] encodes the height of the commitment
	// which included this HTLC on either the remote or local commitment
	// chain. This value is used to determine when an HTLC is fully
	// "locked-in".
	AddCommitHeightRemote uint64
	AddCommitHeightLocal  uint64

	// removeCommitHeight[Remote|Local] encodes the height of the
	// commitment which removed the parent pointer of this
	// PaymentDescriptor either due to a timeout or a settle. Once both
	// these heights are below the tail of both chains, the log entries can
	// safely be removed.
	RemoveCommitHeightRemote uint64
	RemoveCommitHeightLocal  uint64

	// OnionBlob is an opaque blob which is used to complete multi-hop
	// routing.
	//
	// NOTE: Populated only on add payment descriptor entry types.
	OnionBlob []byte

	// ShaOnionBlob is a sha of the onion blob.
	//
	// NOTE: Populated only in payment descriptor with MalformedFail type.
	ShaOnionBlob [sha256.Size]byte

	// FailReason stores the reason why a particular payment was canceled.
	//
	// NOTE: Populate only in fail payment descriptor entry types.
	FailReason []byte

	// FailCode stores the code why a particular payment was canceled.
	//
	// NOTE: Populated only in payment descriptor with MalformedFail type.
	FailCode lnwire.FailCode

	// [our|their|]PkScript are the raw public key scripts that encodes the
	// redemption rules for this particular HTLC. These fields will only be
	// populated iff the EntryType of this PaymentDescriptor is Add.
	// ourPkScript is the ourPkScript from the context of our local
	// commitment chain. theirPkScript is the latest pkScript from the
	// context of the remote commitment chain.
	//
	// NOTE: These values may change within the logs themselves, however,
	// they'll stay consistent within the commitment chain entries
	// themselves.
	OurPkScript        []byte
	OurWitnessScript   []byte
	TheirPkScript      []byte
	TheirWitnessScript []byte

	// EntryType denotes the exact type of the PaymentDescriptor. In the
	// case of a Timeout, or Settle type, then the Parent field will point
	// into the log to the HTLC being modified.
	EntryType updateType

	// isForwarded denotes if an incoming HTLC has been forwarded to any
	// possible upstream peers in the route.
	IsForwarded bool
}

// htlcTimeoutFee returns the fee in satoshis required for an HTLC timeout
// transaction based on the current fee rate.
func HtlcTimeoutFee(feePerKw chainfee.SatPerKWeight) btcutil.Amount {
	return feePerKw.FeeForWeight(input.HtlcTimeoutWeight)
}

// htlcSuccessFee returns the fee in satoshis required for an HTLC success
// transaction based on the current fee rate.
func HtlcSuccessFee(feePerKw chainfee.SatPerKWeight) btcutil.Amount {
	return feePerKw.FeeForWeight(input.HtlcSuccessWeight)
}

// htlcIsDust determines if an HTLC output is dust or not depending on two
// bits: if the HTLC is incoming and if the HTLC will be placed on our
// commitment transaction, or theirs. These two pieces of information are
// require as we currently used second-level HTLC transactions as off-chain
// covenants. Depending on the two bits, we'll either be using a timeout or
// success transaction which have different weights.
func HtlcIsDust(incoming, ourCommit bool, feePerKw chainfee.SatPerKWeight,
	htlcAmt, dustLimit btcutil.Amount) bool {

	// First we'll determine the fee required for this HTLC based on if this is
	// an incoming HTLC or not, and also on whose commitment transaction it
	// will be placed on.
	var htlcFee btcutil.Amount
	switch {

	// If this is an incoming HTLC on our commitment transaction, then the
	// second-level transaction will be a success transaction.
	case incoming && ourCommit:
		htlcFee = HtlcSuccessFee(feePerKw)

	// If this is an incoming HTLC on their commitment transaction, then
	// we'll be using a second-level timeout transaction as they've added
	// this HTLC.
	case incoming && !ourCommit:
		htlcFee = HtlcTimeoutFee(feePerKw)

	// If this is an outgoing HTLC on our commitment transaction, then
	// we'll be using a timeout transaction as we're the sender of the
	// HTLC.
	case !incoming && ourCommit:
		htlcFee = HtlcTimeoutFee(feePerKw)

	// If this is an outgoing HTLC on their commitment transaction, then
	// we'll be using an HTLC success transaction as they're the receiver
	// of this HTLC.
	case !incoming && !ourCommit:
		htlcFee = HtlcSuccessFee(feePerKw)
	}

	return (htlcAmt - htlcFee) < dustLimit
}

// htlcView represents the "active" HTLCs at a particular point within the
// history of the HTLC update log.
type HtlcView struct {
	OurUpdates   []*PaymentDescriptor
	TheirUpdates []*PaymentDescriptor
	FeePerKw     chainfee.SatPerKWeight
}

// commitment represents a commitment to a new state within an active channel.
// New commitments can be initiated by either side. Commitments are ordered
// into a commitment chain, with one existing for both parties. Each side can
// independently extend the other side's commitment chain, up to a certain
// "revocation window", which once reached, disallows new commitments until
// the local nodes receives the revocation for the remote node's chain tail.
type Commitment struct {
	// height represents the commitment height of this commitment, or the
	// update number of this commitment.
	Height uint64

	// isOurs indicates whether this is the local or remote node's version
	// of the commitment.
	isOurs bool

	// [our|their]MessageIndex are indexes into the HTLC log, up to which
	// this commitment transaction includes. These indexes allow both sides
	// to independently, and concurrent send create new commitments. Each
	// new commitment sent to the remote party includes an index in the
	// shared log which details which of their updates we're including in
	// this new commitment.
	ourMessageIndex   uint64
	theirMessageIndex uint64

	// [our|their]HtlcIndex are the current running counters for the HTLC's
	// offered by either party. This value is incremented each time a party
	// offers a new HTLC. The log update methods that consume HTLC's will
	// reference these counters, rather than the running cumulative message
	// counters.
	ourHtlcIndex   uint64
	theirHtlcIndex uint64

	// txn is the commitment transaction generated by including any HTLC
	// updates whose index are below the two indexes listed above. If this
	// commitment is being added to the remote chain, then this txn is
	// their version of the commitment transactions. If the local commit
	// chain is being modified, the opposite is true.
	txn *wire.MsgTx

	// sig is a signature for the above commitment transaction.
	sig []byte

	// [our|their]Balance represents the settled balances at this point
	// within the commitment chain. This balance is computed by properly
	// evaluating all the add/remove/settle log entries before the listed
	// indexes.
	//
	// NOTE: This is the balance *after* subtracting any commitment fee,
	// AND anchor outputs.
	OurBalance   lnwire.MilliSatoshi
	TheirBalance lnwire.MilliSatoshi

	// fee is the amount that will be paid as fees for this commitment
	// transaction. The fee is recorded here so that it can be added back
	// and recalculated for each new update to the channel state.
	fee btcutil.Amount

	// feePerKw is the fee per kw used to calculate this commitment
	// transaction's fee.
	feePerKw chainfee.SatPerKWeight

	// dustLimit is the limit on the commitment transaction such that no
	// output values should be below this amount.
	dustLimit btcutil.Amount

	// OutgoingHTLCs is a slice of all the outgoing HTLC's (from our PoV)
	// on this commitment transaction.
	OutgoingHTLCs []PaymentDescriptor

	// IncomingHTLCs is a slice of all the incoming HTLC's (from our PoV)
	// on this commitment transaction.
	IncomingHTLCs []PaymentDescriptor

	// [outgoing|incoming]HTLCIndex is an index that maps an output index
	// on the commitment transaction to the payment descriptor that
	// represents the HTLC output.
	//
	// NOTE: that these fields are only populated if this commitment state
	// belongs to the local node. These maps are used when validating any
	// HTLC signatures which are part of the local commitment state. We use
	// this map in order to locate the details needed to validate an HTLC
	// signature while iterating of the outputs in the local commitment
	// view.
	outgoingHTLCIndex map[int32]*PaymentDescriptor
	incomingHTLCIndex map[int32]*PaymentDescriptor
}

// populateHtlcIndexes modifies the set of HTLC's locked-into the target view
// to have full indexing information populated. This information is required as
// we need to keep track of the indexes of each HTLC in order to properly write
// the current state to disk, and also to locate the types.PaymentDescriptor
// corresponding to HTLC outputs in the commitment transaction.
func (c *Commitment) PopulateHtlcIndexes() error {
	// First, we'll set up some state to allow us to locate the output
	// index of the all the HTLC's within the commitment transaction. We
	// must keep this index so we can validate the HTLC signatures sent to
	// us.
	dups := make(map[lntypes.Hash][]int32)
	c.outgoingHTLCIndex = make(map[int32]*PaymentDescriptor)
	c.incomingHTLCIndex = make(map[int32]*PaymentDescriptor)

	// populateIndex is a helper function that populates the necessary
	// indexes within the commitment view for a particular HTLC.
	populateIndex := func(htlc *PaymentDescriptor, incoming bool) error {
		isDust := HtlcIsDust(incoming, c.isOurs, c.feePerKw,
			htlc.Amount.ToSatoshis(), c.dustLimit)

		var err error
		switch {

		// If this is our commitment transaction, and this is a dust
		// output then we mark it as such using a -1 index.
		case c.isOurs && isDust:
			htlc.LocalOutputIndex = -1

		// If this is the commitment transaction of the remote party,
		// and this is a dust output then we mark it as such using a -1
		// index.
		case !c.isOurs && isDust:
			htlc.RemoteOutputIndex = -1

		// If this is our commitment transaction, then we'll need to
		// locate the output and the index so we can verify an HTLC
		// signatures.
		case c.isOurs:
			htlc.LocalOutputIndex, err = locateOutputIndex(
				htlc, c.txn, c.isOurs, dups,
			)
			if err != nil {
				return err
			}

			// As this is our commitment transactions, we need to
			// keep track of the locations of each output on the
			// transaction so we can verify any HTLC signatures
			// sent to us after we construct the HTLC view.
			if incoming {
				c.incomingHTLCIndex[htlc.LocalOutputIndex] = htlc
			} else {
				c.outgoingHTLCIndex[htlc.LocalOutputIndex] = htlc
			}

		// Otherwise, this is there remote party's commitment
		// transaction and we only need to populate the remote output
		// index within the HTLC index.
		case !c.isOurs:
			htlc.RemoteOutputIndex, err = locateOutputIndex(
				htlc, c.txn, c.isOurs, dups,
			)
			if err != nil {
				return err
			}

		default:
			return fmt.Errorf("invalid commitment configuration")
		}

		return nil
	}

	// Finally, we'll need to locate the index within the commitment
	// transaction of all the HTLC outputs. This index will be required
	// later when we write the commitment state to disk, and also when
	// generating signatures for each of the HTLC transactions.
	for i := 0; i < len(c.OutgoingHTLCs); i++ {
		htlc := &c.OutgoingHTLCs[i]
		if err := populateIndex(htlc, false); err != nil {
			return err
		}
	}
	for i := 0; i < len(c.IncomingHTLCs); i++ {
		htlc := &c.IncomingHTLCs[i]
		if err := populateIndex(htlc, true); err != nil {
			return err
		}
	}

	return nil
}

// toDiskCommit converts the target commitment into a format suitable to be
// written to disk after an accepted state transition.
func (c *Commitment) toDiskCommit(ourCommit bool) *channeldb.ChannelCommitment {
	numHtlcs := len(c.OutgoingHTLCs) + len(c.IncomingHTLCs)

	commit := &channeldb.ChannelCommitment{
		CommitHeight:    c.Height,
		LocalLogIndex:   c.ourMessageIndex,
		LocalHtlcIndex:  c.ourHtlcIndex,
		RemoteLogIndex:  c.theirMessageIndex,
		RemoteHtlcIndex: c.theirHtlcIndex,
		LocalBalance:    c.OurBalance,
		RemoteBalance:   c.TheirBalance,
		CommitFee:       c.fee,
		FeePerKw:        btcutil.Amount(c.feePerKw),
		CommitTx:        c.txn,
		CommitSig:       c.sig,
		Htlcs:           make([]channeldb.HTLC, 0, numHtlcs),
	}

	for _, htlc := range c.OutgoingHTLCs {
		outputIndex := htlc.LocalOutputIndex
		if !ourCommit {
			outputIndex = htlc.RemoteOutputIndex
		}

		h := channeldb.HTLC{
			RHash:         htlc.RHash,
			Amt:           htlc.Amount,
			RefundTimeout: htlc.Timeout,
			OutputIndex:   outputIndex,
			HtlcIndex:     htlc.HtlcIndex,
			LogIndex:      htlc.LogIndex,
			Incoming:      false,
		}
		h.OnionBlob = make([]byte, len(htlc.OnionBlob))
		copy(h.OnionBlob[:], htlc.OnionBlob)

		if ourCommit && htlc.Sig != nil {
			h.Signature = htlc.Sig.Serialize()
		}

		commit.Htlcs = append(commit.Htlcs, h)
	}

	for _, htlc := range c.IncomingHTLCs {
		outputIndex := htlc.LocalOutputIndex
		if !ourCommit {
			outputIndex = htlc.RemoteOutputIndex
		}

		h := channeldb.HTLC{
			RHash:         htlc.RHash,
			Amt:           htlc.Amount,
			RefundTimeout: htlc.Timeout,
			OutputIndex:   outputIndex,
			HtlcIndex:     htlc.HtlcIndex,
			LogIndex:      htlc.LogIndex,
			Incoming:      true,
		}
		h.OnionBlob = make([]byte, len(htlc.OnionBlob))
		copy(h.OnionBlob[:], htlc.OnionBlob)

		if ourCommit && htlc.Sig != nil {
			h.Signature = htlc.Sig.Serialize()
		}

		commit.Htlcs = append(commit.Htlcs, h)
	}

	return commit
}

// locateOutputIndex is a small helper function to locate the output index of a
// particular HTLC within the current commitment transaction. The duplicate map
// massed in is to be retained for each output within the commitment
// transition.  This ensures that we don't assign multiple HTLC's to the same
// index within the commitment transaction.
func locateOutputIndex(p *PaymentDescriptor, tx *wire.MsgTx, ourCommit bool,
	dups map[lntypes.Hash][]int32) (int32, error) {

	// Checks to see if element (e) exists in slice (s).
	contains := func(s []int32, e int32) bool {
		for _, a := range s {
			if a == e {
				return true
			}
		}
		return false
	}

	// If this their commitment transaction, we'll be trying to locate
	// their pkScripts, otherwise we'll be looking for ours. This is
	// required as the commitment states are asymmetric in order to ascribe
	// blame in the case of a contract breach.
	pkScript := p.TheirPkScript
	if ourCommit {
		pkScript = p.OurPkScript
	}

	for i, txOut := range tx.TxOut {
		if bytes.Equal(txOut.PkScript, pkScript) &&
			txOut.Value == int64(p.Amount.ToSatoshis()) {

			// If this payment hash and index has already been
			// found, then we'll continue in order to avoid any
			// duplicate indexes.
			if contains(dups[p.RHash], int32(i)) {
				continue
			}

			idx := int32(i)
			dups[p.RHash] = append(dups[p.RHash], idx)
			return idx, nil
		}
	}

	return 0, fmt.Errorf("unable to find htlc: script=%x, value=%v",
		pkScript, p.Amount)
}

// diskHtlcToPayDesc converts an HTLC previously written to disk within a
// commitment state to the form required to manipulate in memory within the
// commitment struct and updateLog. This function is used when we need to
// restore commitment state written do disk back into memory once we need to
// restart a channel session.
func diskHtlcToPayDesc(feeRate chainfee.SatPerKWeight,
	localChanCfg, remoteChanCfg *channeldb.ChannelConfig,
	commitHeight uint64, htlc *channeldb.HTLC, localCommitKeys,
	remoteCommitKeys *commitmenttx.KeyRing) (PaymentDescriptor, error) {

	// The proper pkScripts for this types.PaymentDescriptor must be
	// generated so we can easily locate them within the commitment
	// transaction in the future.
	var (
		ourP2WSH, theirP2WSH                 []byte
		ourWitnessScript, theirWitnessScript []byte
		pd                                   PaymentDescriptor
		err                                  error
	)

	// If the either outputs is dust from the local or remote node's
	// perspective, then we don't need to generate the scripts as we only
	// generate them in order to locate the outputs within the commitment
	// transaction. As we'll mark dust with a special output index in the
	// on-disk state snapshot.
	isDustLocal := HtlcIsDust(htlc.Incoming, true, feeRate,
		htlc.Amt.ToSatoshis(), localChanCfg.DustLimit)
	if !isDustLocal && localCommitKeys != nil {
		ourP2WSH, ourWitnessScript, err = GenHtlcScript(
			htlc.Incoming, true, htlc.RefundTimeout, htlc.RHash,
			localCommitKeys)
		if err != nil {
			return pd, err
		}
	}
	isDustRemote := HtlcIsDust(htlc.Incoming, false, feeRate,
		htlc.Amt.ToSatoshis(), remoteChanCfg.DustLimit)
	if !isDustRemote && remoteCommitKeys != nil {
		theirP2WSH, theirWitnessScript, err = GenHtlcScript(
			htlc.Incoming, false, htlc.RefundTimeout, htlc.RHash,
			remoteCommitKeys)
		if err != nil {
			return pd, err
		}
	}

	// With the scripts reconstructed (depending on if this is our commit
	// vs theirs or a pending commit for the remote party), we can now
	// re-create the original payment descriptor.
	pd = PaymentDescriptor{
		RHash:              htlc.RHash,
		Timeout:            htlc.RefundTimeout,
		Amount:             htlc.Amt,
		EntryType:          Add,
		HtlcIndex:          htlc.HtlcIndex,
		LogIndex:           htlc.LogIndex,
		OnionBlob:          htlc.OnionBlob,
		OurPkScript:        ourP2WSH,
		OurWitnessScript:   ourWitnessScript,
		TheirPkScript:      theirP2WSH,
		TheirWitnessScript: theirWitnessScript,
	}

	return pd, nil
}

// extractPayDescs will convert all HTLC's present within a disk commit state
// to a set of incoming and outgoing payment descriptors. Once reconstructed,
// these payment descriptors can be re-inserted into the in-memory updateLog
// for each side.
func extractPayDescs(commitHeight uint64,
	localChanCfg, remoteChanCfg *channeldb.ChannelConfig,
	feeRate chainfee.SatPerKWeight, htlcs []channeldb.HTLC, localCommitKeys,
	remoteCommitKeys *commitmenttx.KeyRing) ([]PaymentDescriptor, []PaymentDescriptor, error) {

	var (
		incomingHtlcs []PaymentDescriptor
		outgoingHtlcs []PaymentDescriptor
	)

	// For each included HTLC within this commitment state, we'll convert
	// the disk format into our in memory types.PaymentDescriptor format,
	// partitioning based on if we offered or received the HTLC.
	for _, htlc := range htlcs {
		// TODO(roasbeef): set IsForwarded to false for all? need to
		// persist state w.r.t to if forwarded or not, or can
		// inadvertently trigger replays

		payDesc, err := diskHtlcToPayDesc(feeRate,
			localChanCfg, remoteChanCfg,
			commitHeight, &htlc,
			localCommitKeys, remoteCommitKeys,
		)
		if err != nil {
			return incomingHtlcs, outgoingHtlcs, err
		}

		if htlc.Incoming {
			incomingHtlcs = append(incomingHtlcs, payDesc)
		} else {
			outgoingHtlcs = append(outgoingHtlcs, payDesc)
		}
	}

	return incomingHtlcs, outgoingHtlcs, nil
}

// DiskCommitToMemCommit converts the on-disk commitment format to our
// in-memory commitment format which is needed in order to properly resume
// channel operations after a restart.
func DiskCommitToMemCommit(commitType commitmenttx.CommitmentType, isLocal bool,
	localChanCfg, remoteChanCfg *channeldb.ChannelConfig,
	channelState *channeldb.OpenChannel,
	diskCommit *channeldb.ChannelCommitment, localCommitPoint,
	remoteCommitPoint *btcec.PublicKey) (*Commitment, error) {

	// First, we'll need to re-derive the commitment key ring for each
	// party used within this particular state. If this is a pending commit
	// (we extended but weren't able to complete the commitment dance
	// before shutdown), then the localCommitPoint won't be set as we
	// haven't yet received a responding commitment from the remote party.
	var localCommitKeys, remoteCommitKeys *commitmenttx.KeyRing
	if localCommitPoint != nil {
		localCommitKeys = commitType.DeriveCommitmentKeys(
			localCommitPoint, true, localChanCfg,
			remoteChanCfg,
		)
	}
	if remoteCommitPoint != nil {
		remoteCommitKeys = commitType.DeriveCommitmentKeys(
			remoteCommitPoint, false, localChanCfg,
			remoteChanCfg,
		)
	}

	// With the key rings re-created, we'll now convert all the on-disk
	// HTLC"s into types.PaymentDescriptor's so we can re-insert them into our
	// update log.
	incomingHtlcs, outgoingHtlcs, err := extractPayDescs(
		diskCommit.CommitHeight,
		localChanCfg, remoteChanCfg,
		chainfee.SatPerKWeight(diskCommit.FeePerKw),
		diskCommit.Htlcs, localCommitKeys, remoteCommitKeys,
	)
	if err != nil {
		return nil, err
	}

	// With the necessary items generated, we'll now re-construct the
	// commitment state as it was originally present in memory.
	commit := &Commitment{
		Height:            diskCommit.CommitHeight,
		isOurs:            isLocal,
		OurBalance:        diskCommit.LocalBalance,
		TheirBalance:      diskCommit.RemoteBalance,
		ourMessageIndex:   diskCommit.LocalLogIndex,
		ourHtlcIndex:      diskCommit.LocalHtlcIndex,
		theirMessageIndex: diskCommit.RemoteLogIndex,
		theirHtlcIndex:    diskCommit.RemoteHtlcIndex,
		txn:               diskCommit.CommitTx,
		sig:               diskCommit.CommitSig,
		fee:               diskCommit.CommitFee,
		feePerKw:          chainfee.SatPerKWeight(diskCommit.FeePerKw),
		IncomingHTLCs:     incomingHtlcs,
		OutgoingHTLCs:     outgoingHtlcs,
	}
	if isLocal {
		commit.dustLimit = channelState.LocalChanCfg.DustLimit
	} else {
		commit.dustLimit = channelState.RemoteChanCfg.DustLimit
	}

	// Finally, we'll re-populate the HTLC index for this state so we can
	// properly locate each HTLC within the commitment transaction.
	if err := commit.PopulateHtlcIndexes(); err != nil {
		return nil, err
	}

	return commit, nil
}

// genHtlcScript generates the proper P2WSH public key scripts for the HTLC
// output modified by two-bits denoting if this is an incoming HTLC, and if the
// HTLC is being applied to their commitment transaction or ours.
func GenHtlcScript(isIncoming, ourCommit bool, timeout uint32, rHash [32]byte,
	keyRing *commitmenttx.KeyRing) ([]byte, []byte, error) {

	var (
		witnessScript []byte
		err           error
	)

	// Generate the proper redeem scripts for the HTLC output modified by
	// two-bits denoting if this is an incoming HTLC, and if the HTLC is
	// being applied to their commitment transaction or ours.
	switch {
	// The HTLC is paying to us, and being applied to our commitment
	// transaction. So we need to use the receiver's version of HTLC the
	// script.
	case isIncoming && ourCommit:
		witnessScript, err = input.ReceiverHTLCScript(timeout,
			keyRing.RemoteHtlcKey, keyRing.LocalHtlcKey,
			keyRing.RevocationKey, rHash[:])

	// We're being paid via an HTLC by the remote party, and the HTLC is
	// being added to their commitment transaction, so we use the sender's
	// version of the HTLC script.
	case isIncoming && !ourCommit:
		witnessScript, err = input.SenderHTLCScript(keyRing.RemoteHtlcKey,
			keyRing.LocalHtlcKey, keyRing.RevocationKey, rHash[:])

	// We're sending an HTLC which is being added to our commitment
	// transaction. Therefore, we need to use the sender's version of the
	// HTLC script.
	case !isIncoming && ourCommit:
		witnessScript, err = input.SenderHTLCScript(keyRing.LocalHtlcKey,
			keyRing.RemoteHtlcKey, keyRing.RevocationKey, rHash[:])

	// Finally, we're paying the remote party via an HTLC, which is being
	// added to their commitment transaction. Therefore, we use the
	// receiver's version of the HTLC script.
	case !isIncoming && !ourCommit:
		witnessScript, err = input.ReceiverHTLCScript(timeout, keyRing.LocalHtlcKey,
			keyRing.RemoteHtlcKey, keyRing.RevocationKey, rHash[:])
	}
	if err != nil {
		return nil, nil, err
	}

	// Now that we have the redeem scripts, create the P2WSH public key
	// script for the output itself.
	htlcP2WSH, err := input.WitnessScriptHash(witnessScript)
	if err != nil {
		return nil, nil, err
	}

	return htlcP2WSH, witnessScript, nil
}

// createCommitmentTx generates the unsigned commitment transaction for a
// commitment view and assigns to txn field.
func CreateCommitmentTx(c *Commitment,
	commitType commitmenttx.CommitmentType,
	channelState *channeldb.OpenChannel,
	filteredHTLCView *HtlcView, keyRing *commitmenttx.KeyRing) error {

	ourBalance := c.OurBalance
	theirBalance := c.TheirBalance

	numHTLCs := int64(0)
	for _, htlc := range filteredHTLCView.OurUpdates {
		if HtlcIsDust(false, c.isOurs, c.feePerKw,
			htlc.Amount.ToSatoshis(), c.dustLimit) {

			continue
		}

		numHTLCs++
	}
	for _, htlc := range filteredHTLCView.TheirUpdates {
		if HtlcIsDust(true, c.isOurs, c.feePerKw,
			htlc.Amount.ToSatoshis(), c.dustLimit) {

			continue
		}

		numHTLCs++
	}

	// Next, we'll calculate the fee for the commitment transaction based
	// on its total weight. Once we have the total weight, we'll multiply
	// by the current fee-per-kw, then divide by 1000 to get the proper
	// fee.
	totalCommitWeight := commitType.CommitWeight() +
		input.HTLCWeight*numHTLCs

	// With the weight known, we can now calculate the commitment fee,
	// ensuring that we account for any dust outputs trimmed above.
	commitFee := c.feePerKw.FeeForWeight(totalCommitWeight)

	// If the current commitment type has anchors (AnchorSize is non-zero)
	// it will also be paid by the initiator.
	initiatorFee := commitFee + 2*channelState.AnchorSize
	initiatorFeeMSat := lnwire.NewMSatFromSatoshis(initiatorFee)

	// The commitment has one to_local_anchor output, one timelocked
	// to_local output, and one to_remote output.  The commitment will have
	// a minimum fee, and this fee is always paid by the initiator. This
	// fee is subracted from the node's output, resulting in it being set
	// to 0 if below the dust limit.
	switch {
	case channelState.IsInitiator && initiatorFee > ourBalance.ToSatoshis():
		ourBalance = 0

	case channelState.IsInitiator:
		ourBalance -= initiatorFeeMSat

	case !channelState.IsInitiator && initiatorFee > theirBalance.ToSatoshis():
		theirBalance = 0

	case !channelState.IsInitiator:
		theirBalance -= initiatorFeeMSat
	}

	var (
		localCfg, remoteCfg         *channeldb.ChannelConfig
		localBalance, remoteBalance btcutil.Amount
	)
	if c.isOurs {
		localCfg = &channelState.LocalChanCfg
		remoteCfg = &channelState.RemoteChanCfg
		localBalance = ourBalance.ToSatoshis()
		remoteBalance = theirBalance.ToSatoshis()
	} else {
		localCfg = &channelState.RemoteChanCfg
		remoteCfg = &channelState.LocalChanCfg
		localBalance = theirBalance.ToSatoshis()
		remoteBalance = ourBalance.ToSatoshis()
	}

	// Check whether there are any htlcs left in the view. We need this to
	// determing whether to add anchor outputs.
	hasHtlcs := false
	for _, htlc := range filteredHTLCView.OurUpdates {
		if HtlcIsDust(false, c.isOurs, c.feePerKw,
			htlc.Amount.ToSatoshis(), localCfg.DustLimit) {
			continue
		}

		hasHtlcs = true
		break
	}
	for _, htlc := range filteredHTLCView.TheirUpdates {
		if HtlcIsDust(true, c.isOurs, c.feePerKw,
			htlc.Amount.ToSatoshis(), localCfg.DustLimit) {
			continue
		}

		hasHtlcs = true
		break
	}

	fundingTxIn := func() wire.TxIn {
		return *wire.NewTxIn(&channelState.FundingOutpoint, nil, nil)
	}

	// Generate a new commitment transaction with all the latest
	// unsettled/un-timed out HTLCs.
	commitTx, err := commitmenttx.CreateCommitTx(
		commitType, fundingTxIn(), keyRing, localCfg, remoteCfg,
		localBalance, remoteBalance, channelState.AnchorSize, hasHtlcs,
	)
	if err != nil {
		return err
	}

	// We'll now add all the HTLC outputs to the commitment transaction.
	// Each output includes an off-chain 2-of-2 covenant clause, so we'll
	// need the objective local/remote keys for this particular commitment
	// as well. For any non-dust HTLCs that are manifested on the commitment
	// transaction, we'll also record its CLTV which is required to sort the
	// commitment transaction below. The slice is initially sized to the
	// number of existing outputs, since any outputs already added are
	// commitment outputs and should correspond to zero values for the
	// purposes of sorting.
	cltvs := make([]uint32, len(commitTx.TxOut))
	for _, htlc := range filteredHTLCView.OurUpdates {
		if HtlcIsDust(false, c.isOurs, c.feePerKw,
			htlc.Amount.ToSatoshis(), c.dustLimit) {
			continue
		}

		err := addHTLC(commitTx, c.isOurs, false, htlc, keyRing)
		if err != nil {
			return err
		}
		cltvs = append(cltvs, htlc.Timeout)
	}
	for _, htlc := range filteredHTLCView.TheirUpdates {
		if HtlcIsDust(true, c.isOurs, c.feePerKw,
			htlc.Amount.ToSatoshis(), c.dustLimit) {
			continue
		}

		err := addHTLC(commitTx, c.isOurs, true, htlc, keyRing)
		if err != nil {
			return err
		}
		cltvs = append(cltvs, htlc.Timeout)
	}

	// Set the state hint of the commitment transaction to facilitate
	// quickly recovering the necessary penalty state in the case of an
	// uncooperative broadcast.
	err = SetStateNumHint(commitTx, c.height, lc.stateHintObfuscator)
	if err != nil {
		return err
	}

	// Sort the transactions according to the agreed upon canonical
	// ordering. This lets us skip sending the entire transaction over,
	// instead we'll just send signatures.
	InPlaceCommitSort(commitTx, cltvs)

	// Next, we'll ensure that we don't accidentally create a commitment
	// transaction which would be invalid by consensus.
	uTx := btcutil.NewTx(commitTx)
	if err := blockchain.CheckTransactionSanity(uTx); err != nil {
		return err
	}

	// Finally, we'll assert that were not attempting to draw more out of
	// the channel that was originally placed within it.
	var totalOut btcutil.Amount
	for _, txOut := range commitTx.TxOut {
		totalOut += btcutil.Amount(txOut.Value)
	}
	if totalOut > lc.channelState.Capacity {
		return fmt.Errorf("height=%v, for ChannelPoint(%v) attempts "+
			"to consume %v while channel capacity is %v",
			c.height, lc.channelState.FundingOutpoint,
			totalOut, lc.channelState.Capacity)
	}

	c.txn = commitTx
	c.fee = commitFee
	c.ourBalance = ourBalance
	c.theirBalance = theirBalance
	return nil
}

// addHTLC adds a new HTLC to the passed commitment transaction. One of four
// full scripts will be generated for the HTLC output depending on if the HTLC
// is incoming and if it's being applied to our commitment transaction or that
// of the remote node's. Additionally, in order to be able to efficiently
// locate the added HTLC on the commitment transaction from the
// types.PaymentDescriptor that generated it, the generated script is stored within
// the descriptor itself.
func addHTLC(commitTx *wire.MsgTx, ourCommit bool,
	isIncoming bool, paymentDesc *PaymentDescriptor,
	keyRing *commitmenttx.KeyRing) error {

	timeout := paymentDesc.Timeout
	rHash := paymentDesc.RHash

	p2wsh, witnessScript, err := GenHtlcScript(isIncoming, ourCommit,
		timeout, rHash, keyRing)
	if err != nil {
		return err
	}

	// Add the new HTLC outputs to the respective commitment transactions.
	amountPending := int64(paymentDesc.Amount.ToSatoshis())
	commitTx.AddTxOut(wire.NewTxOut(amountPending, p2wsh))

	// Store the pkScript of this particular types.PaymentDescriptor so we can
	// quickly locate it within the commitment transaction later.
	if ourCommit {
		paymentDesc.OurPkScript = p2wsh
		paymentDesc.OurWitnessScript = witnessScript
	} else {
		paymentDesc.TheirPkScript = p2wsh
		paymentDesc.TheirWitnessScript = witnessScript
	}

	return nil
}

// SetStateNumHint encodes the current state number within the passed
// commitment transaction by re-purposing the locktime and sequence fields in
// the commitment transaction to encode the obfuscated state number.  The state
// number is encoded using 48 bits. The lower 24 bits of the lock time are the
// lower 24 bits of the obfuscated state number and the lower 24 bits of the
// sequence field are the higher 24 bits. Finally before encoding, the
// obfuscator is XOR'd against the state number in order to hide the exact
// state number from the PoV of outside parties.
func SetStateNumHint(commitTx *wire.MsgTx, stateNum uint64,
	obfuscator [StateHintSize]byte) error {

	// With the current schema we are only able to encode state num
	// hints up to 2^48. Therefore if the passed height is greater than our
	// state hint ceiling, then exit early.
	if stateNum > maxStateHint {
		return fmt.Errorf("unable to encode state, %v is greater "+
			"state num that max of %v", stateNum, maxStateHint)
	}

	if len(commitTx.TxIn) != 1 {
		return fmt.Errorf("commitment tx must have exactly 1 input, "+
			"instead has %v", len(commitTx.TxIn))
	}

	// Convert the obfuscator into a uint64, then XOR that against the
	// targeted height in order to obfuscate the state number of the
	// commitment transaction in the case that either commitment
	// transaction is broadcast directly on chain.
	var obfs [8]byte
	copy(obfs[2:], obfuscator[:])
	xorInt := binary.BigEndian.Uint64(obfs[:])

	stateNum = stateNum ^ xorInt

	// Set the height bit of the sequence number in order to disable any
	// sequence locks semantics.
	commitTx.TxIn[0].Sequence = uint32(stateNum>>24) | wire.SequenceLockTimeDisabled
	commitTx.LockTime = uint32(stateNum&0xFFFFFF) | TimelockShift

	return nil
}
