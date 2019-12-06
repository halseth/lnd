package commitmenttx

import (
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/input"
)

// ScriptIinfo holds a reedeem script and hash.
type ScriptInfo struct {
	// PkScript is the outputs' PkScript.
	PkScript []byte

	// WitnessScript is the full script required to properly redeem the
	// output with PkSript. This field will only be populated if a PkScript
	// is p2wsh or p2sh.
	WitnessScript []byte
}

// CommitmentType is a type that wraps the type of channel we are dealine with,
// and abstracts the various ways of constructing commitment transactions. It
// can be used to derive keys, scripts and outputs depending on the channel's
// commitment type.
type CommitmentType struct {
	chanType channeldb.ChannelType
}

// NewCommitmentType creates a new CommitmentType based on the ChannelType.
func NewCommitmentType(chanType channeldb.ChannelType) CommitmentType {
	c := CommitmentType{
		chanType: chanType,
	}

	// The anchor chennel type MUST be tweakless.
	if chanType.HasAnchors() && !chanType.IsTweakless() {
		panic("invalid channel type combination")
	}

	return c
}

// DeriveCommitmentKeys generates a new commitment key set using the base
// points and commitment point for the this commitment type.
func (c CommitmentType) DeriveCommitmentKeys(commitPoint *btcec.PublicKey,
	isOurCommit bool, localChanCfg,
	remoteChanCfg *channeldb.ChannelConfig) *KeyRing {

	// Return commitment keys with tweaklessCommit set according to channel
	// type.
	return deriveCommitmentKeys(
		commitPoint, isOurCommit, c.chanType.IsTweakless(),
		localChanCfg, remoteChanCfg,
	)
}

// CommitScriptToRemote creates the script that will pay to the non-owner of
// the commitment transaction, adding a delay to the script based on the
// commitment type.
func (c CommitmentType) CommitScriptToRemote(csvTimeout uint32,
	key *btcec.PublicKey) (*ScriptInfo, error) {

	// If this channel type has anchors, we derive the delayed to_remote
	// script.
	if c.chanType.HasAnchors() {
		script, err := input.CommitScriptToRemote(csvTimeout, key)
		if err != nil {
			return nil, err
		}

		p2wsh, err := input.WitnessScriptHash(script)
		if err != nil {
			return nil, err
		}

		return &ScriptInfo{
			PkScript:      p2wsh,
			WitnessScript: script,
		}, nil
	}

	// Otherwise the te_remote will be a simple p2wkh.
	p2wkh, err := input.CommitScriptUnencumbered(key)
	if err != nil {
		return nil, err
	}

	// Since this is a regular P2WKH, the WitnessScipt doesn't have to be
	// set.
	return &ScriptInfo{
		PkScript: p2wkh,
	}, nil
}

// CommitScriptAnchors return the scripts to use for the local and remote
// anchor. The returned values can be nil to indicate the ouputs shouldn't be
// added.
func (c CommitmentType) CommitScriptAnchors(localChanCfg,
	remoteChanCfg *channeldb.ChannelConfig) (*ScriptInfo, *ScriptInfo, error) {

	// If this channel type has no anchors we can return immediately.
	if c.chanType.HasAnchors() {
		return nil, nil, nil
	}

	// Then the anchor output spendable by the local node.
	localAnchorScript, err := input.CommitScriptAnchor(
		localChanCfg.MultiSigKey.PubKey,
	)
	if err != nil {
		return nil, nil, err
	}

	localAnchorScriptHash, err := input.WitnessScriptHash(localAnchorScript)
	if err != nil {
		return nil, nil, err
	}

	// And the anchor spemdable by the remote.
	remoteAnchorScript, err := input.CommitScriptAnchor(
		remoteChanCfg.MultiSigKey.PubKey,
	)
	if err != nil {
		return nil, nil, err
	}

	remoteAnchorScriptHash, err := input.WitnessScriptHash(
		remoteAnchorScript,
	)
	if err != nil {
		return nil, nil, err
	}

	return &ScriptInfo{
			PkScript:      localAnchorScript,
			WitnessScript: localAnchorScriptHash,
		},
		&ScriptInfo{
			PkScript:      remoteAnchorScript,
			WitnessScript: remoteAnchorScriptHash,
		}, nil
}

// CommitWeight returns the base commitment weight before adding HTLCs.
func (c CommitmentType) CommitWeight() int64 {
	// If this commitment has anchors, it will be slightly heavier.
	if c.chanType.HasAnchors() {
		return input.AnchorCommitWeight
	}

	return input.CommitWeight
}

// CreateCommitTx creates a commitment transaction, spending from specified
// funding output. The commitment transaction contains two outputs: one local
// output paying to the "owner" of the commitment transaction which can be
// spent after a relative block delay or revocation event, and a remote output
// paying the counterparty within the channel, which can be spent immediately
// or after a delay depending on the commitment type..
func CreateCommitTx(commitType CommitmentType,
	fundingOutput wire.TxIn, keyRing *KeyRing,
	localChanCfg, remoteChanCfg *channeldb.ChannelConfig,
	amountToLocal, amountToRemote, anchorSize btcutil.Amount,
	hasHtlcs bool) (*wire.MsgTx, error) {

	// First, we create the script for the delayed "pay-to-self" output.
	// This output has 2 main redemption clauses: either we can redeem the
	// output after a relative block delay, or the remote node can claim
	// the funds with the revocation key if we broadcast a revoked
	// commitment transaction.
	toLocalRedeemScript, err := input.CommitScriptToSelf(
		uint32(localChanCfg.CsvDelay), keyRing.LocalKey,
		keyRing.RevocationKey,
	)
	if err != nil {
		return nil, err
	}
	toLocalScriptHash, err := input.WitnessScriptHash(
		toLocalRedeemScript,
	)
	if err != nil {
		return nil, err
	}

	// Next, we create the script paying to the remote.
	toRemoteScript, err := commitType.CommitScriptToRemote(
		uint32(remoteChanCfg.CsvDelay), keyRing.RemoteKey,
	)
	if err != nil {
		return nil, err
	}

	// Get the anchor outputs if any.
	localAnchor, remoteAnchor, err := commitType.CommitScriptAnchors(
		localChanCfg, remoteChanCfg,
	)
	if err != nil {
		return nil, err
	}

	// Now that both output scripts have been created, we can finally create
	// the transaction itself. We use a transaction version of 2 since CSV
	// will fail unless the tx version is >= 2.
	commitTx := wire.NewMsgTx(2)
	commitTx.AddTxIn(&fundingOutput)

	// Avoid creating dust outputs within the commitment transaction.
	if amountToLocal >= localChanCfg.DustLimit {
		commitTx.AddTxOut(&wire.TxOut{
			PkScript: toLocalScriptHash,
			Value:    int64(amountToLocal),
		})
	}

	if amountToRemote >= localChanCfg.DustLimit {
		commitTx.AddTxOut(&wire.TxOut{
			PkScript: toRemoteScript.PkScript,
			Value:    int64(amountToRemote),
		})
	}

	// Add local anchor output only if we have a commitment output
	// or there are HTLCs.
	localStake := amountToLocal >= localChanCfg.DustLimit || hasHtlcs

	// Some commitment types don't have anchors, so check if it is non-nil.
	if localAnchor != nil && localStake {
		commitTx.AddTxOut(&wire.TxOut{
			PkScript: localAnchor.PkScript,
			Value:    int64(anchorSize),
		})
	}

	// Add anchor output to remote only if they have a commitment output or
	// there are HTLCs.
	remoteStake := amountToRemote >= localChanCfg.DustLimit || hasHtlcs
	if remoteAnchor != nil && remoteStake {
		commitTx.AddTxOut(&wire.TxOut{
			PkScript: remoteAnchor.PkScript,
			Value:    int64(anchorSize),
		})
	}

	return commitTx, nil
}
