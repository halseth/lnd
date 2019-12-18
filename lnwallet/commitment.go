package lnwallet

import (
	"fmt"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
)

// CommitmentKeyRing holds all derived keys needed to construct commitment and
// HTLC transactions. The keys are derived differently depending whether the
// commitment transaction is ours or the remote peer's. Private keys associated
// with each key may belong to the commitment owner or the "other party" which
// is referred to in the field comments, regardless of which is local and which
// is remote.
type CommitmentKeyRing struct {
	// commitPoint is the "per commitment point" used to derive the tweak
	// for each base point.
	CommitPoint *btcec.PublicKey

	// LocalCommitKeyTweak is the tweak used to derive the local public key
	// from the local payment base point or the local private key from the
	// base point secret. This may be included in a SignDescriptor to
	// generate signatures for the local payment key.
	LocalCommitKeyTweak []byte

	// TODO(roasbeef): need delay tweak as well?

	// LocalHtlcKeyTweak is the teak used to derive the local HTLC key from
	// the local HTLC base point. This value is needed in order to
	// derive the final key used within the HTLC scripts in the commitment
	// transaction.
	LocalHtlcKeyTweak []byte

	// LocalHtlcKey is the key that will be used in the "to self" clause of
	// any HTLC scripts within the commitment transaction for this key ring
	// set.
	LocalHtlcKey *btcec.PublicKey

	// RemoteHtlcKey is the key that will be used in clauses within the
	// HTLC script that send money to the remote party.
	RemoteHtlcKey *btcec.PublicKey

	// LocalKey is the commitment transaction owner's key which is included
	// in HTLC success and timeout transaction scripts.
	LocalKey *btcec.PublicKey

	// RemoteKey is the other party's payment key in the commitment tx.
	// This is the key used to generate the unencumbered output within the
	// commitment transaction.
	RemoteKey *btcec.PublicKey

	// RevocationKey is the key that can be used by the other party to
	// redeem outputs from a revoked commitment transaction if it were to
	// be published.
	RevocationKey *btcec.PublicKey
}

// DeriveCommitmentKeys generates a new commitment key set using the base points
// and commitment point. The keys are derived differently depending on the type
// of channel, and whether the commitment transaction is ours or the remote
// peer's.
func DeriveCommitmentKeys(commitPoint *btcec.PublicKey,
	isOurCommit bool, chanType channeldb.ChannelType,
	localChanCfg, remoteChanCfg *channeldb.ChannelConfig) *CommitmentKeyRing {

	tweaklessCommit := chanType.IsTweakless()

	// First, we'll derive all the keys that don't depend on the context of
	// whose commitment transaction this is.
	keyRing := &CommitmentKeyRing{
		CommitPoint: commitPoint,

		LocalCommitKeyTweak: input.SingleTweakBytes(
			commitPoint, localChanCfg.PaymentBasePoint.PubKey,
		),
		LocalHtlcKeyTweak: input.SingleTweakBytes(
			commitPoint, localChanCfg.HtlcBasePoint.PubKey,
		),
		LocalHtlcKey: input.TweakPubKey(
			localChanCfg.HtlcBasePoint.PubKey, commitPoint,
		),
		RemoteHtlcKey: input.TweakPubKey(
			remoteChanCfg.HtlcBasePoint.PubKey, commitPoint,
		),
	}

	// We'll now compute the local, remote, and revocation key based on
	// the current commitment point. All keys are tweaked each state in
	// order to ensure the keys from each state are unlinkable. To create
	// the revocation key, we take the opposite party's revocation base
	// point and combine that with the current commitment point.
	var (
		localBasePoint      *btcec.PublicKey
		remoteBasePoint     *btcec.PublicKey
		revocationBasePoint *btcec.PublicKey
	)
	if isOurCommit {
		localBasePoint = localChanCfg.DelayBasePoint.PubKey
		remoteBasePoint = remoteChanCfg.PaymentBasePoint.PubKey
		revocationBasePoint = remoteChanCfg.RevocationBasePoint.PubKey
	} else {
		localBasePoint = remoteChanCfg.DelayBasePoint.PubKey
		remoteBasePoint = localChanCfg.PaymentBasePoint.PubKey
		revocationBasePoint = localChanCfg.RevocationBasePoint.PubKey
	}

	// With the base points assigned, we can now derive the actual keys
	// using the base point, and the current commitment tweak.
	keyRing.LocalKey = input.TweakPubKey(localBasePoint, commitPoint)
	keyRing.RevocationKey = input.DeriveRevocationPubkey(
		revocationBasePoint, commitPoint,
	)

	// If this commitment should omit the tweak for the remote point, then
	// we'll use that directly, and ignore the commitPoint tweak.
	if tweaklessCommit {
		keyRing.RemoteKey = remoteBasePoint

		// If this is a tweakless remote commitment, then we can safely
		// blank out the SingleTweak value as it isn't needed.
		if !isOurCommit {
			keyRing.LocalCommitKeyTweak = nil
		}
	} else {
		keyRing.RemoteKey = input.TweakPubKey(
			remoteBasePoint, commitPoint,
		)
	}

	return keyRing
}

// ScriptInfo holds a redeem script and hash.
type ScriptInfo struct {
	// PkScript is the outputs' PkScript.
	PkScript []byte

	// WitnessScript is the full script required to properly redeem the
	// output with PkSript. This field will only be populated if a PkScript
	// is p2wsh or p2sh.
	WitnessScript []byte
}

// CommitScriptToRemote creates the script that will pay to the non-owner of
// the commitment transaction, adding a delay to the script based on the
// channel type.
func CommitScriptToRemote(_ channeldb.ChannelType, csvTimeout uint32,
	key *btcec.PublicKey) (*ScriptInfo, error) {

	p2wkh, err := input.CommitScriptUnencumbered(key)
	if err != nil {
		return nil, err
	}

	// Since this is a regular P2WKH, the WitnessScipt and PkScript should
	// be both seth to the script hash.
	return &ScriptInfo{
		WitnessScript: p2wkh,
		PkScript:      p2wkh,
	}, nil
}

// CommitmentBuilder is a type that wraps the type of channel we are dealing
// with, and abstracts the various ways of constructing commitment
// transactions.
type CommitmentBuilder struct {
	// chanState is the underlying channels's state struct, used to
	// determine the type of channel we are dealing with, and relevant
	// parameters.
	chanState *channeldb.OpenChannel

	// obfuscator is a 48-bit state hint that's used to obfuscate the
	// current state number on the commitment transactions.
	obfuscator [StateHintSize]byte
}

// NewCommitmentBuilder creates a new CommitmentBuilder from chanState.
func NewCommitmentBuilder(chanState *channeldb.OpenChannel) *CommitmentBuilder {
	return &CommitmentBuilder{
		chanState:  chanState,
		obfuscator: createStateHintObfuscator(chanState),
	}
}

// createStateHintObfuscator derives and assigns the state hint obfuscator for
// the channel, which is used to encode the commitment height in the sequence
// number of commitment transaction inputs.
func createStateHintObfuscator(state *channeldb.OpenChannel) [StateHintSize]byte {
	if state.IsInitiator {
		return DeriveStateHintObfuscator(
			state.OurChanCfg.PaymentBasePoint.PubKey,
			state.TheirChanCfg.PaymentBasePoint.PubKey,
		)
	}

	return DeriveStateHintObfuscator(
		state.TheirChanCfg.PaymentBasePoint.PubKey,
		state.OurChanCfg.PaymentBasePoint.PubKey,
	)
}

// commitmentView is the final commitment created from evaluating an HTLC view
// at a given height.
type commitmentView struct {
	// txn is the final, unsigned commitment transaction for this view.
	txn *wire.MsgTx

	// fee is the total fee of the commitment transaction.
	fee btcutil.Amount

	// ourBalance|theirBalance is the balances of this commitment. This can
	// be different than the balances before creating the commitment
	// transaction as one party must pay the commitment fee.
	ourBalance   lnwire.MilliSatoshi
	theirBalance lnwire.MilliSatoshi
}

// createCommitmentTx generates the unsigned commitment transaction for a
// commitment view and assigns to txn field. The passed in balances should be
// balances *before* subtracting any commitment fees.
func (cb *CommitmentBuilder) createCommitmentTx(ourBalance,
	theirBalance lnwire.MilliSatoshi, isOurs bool,
	feePerKw chainfee.SatPerKWeight, height uint64,
	filteredHTLCView *htlcView,
	keyRing *CommitmentKeyRing) (*commitmentView, error) {

	dustLimit := cb.chanState.OurChanCfg.DustLimit
	if !isOurs {
		dustLimit = cb.chanState.TheirChanCfg.DustLimit
	}

	numHTLCs := int64(0)
	for _, htlc := range filteredHTLCView.ourUpdates {
		if htlcIsDust(false, isOurs, feePerKw,
			htlc.Amount.ToSatoshis(), dustLimit) {

			continue
		}

		numHTLCs++
	}
	for _, htlc := range filteredHTLCView.theirUpdates {
		if htlcIsDust(true, isOurs, feePerKw,
			htlc.Amount.ToSatoshis(), dustLimit) {

			continue
		}

		numHTLCs++
	}

	// Next, we'll calculate the fee for the commitment transaction based
	// on its total weight. Once we have the total weight, we'll multiply
	// by the current fee-per-kw, then divide by 1000 to get the proper
	// fee.
	totalCommitWeight := input.CommitWeight + (input.HtlcWeight * numHTLCs)

	// With the weight known, we can now calculate the commitment fee,
	// ensuring that we account for any dust outputs trimmed above.
	commitFee := feePerKw.FeeForWeight(totalCommitWeight)
	commitFeeMSat := lnwire.NewMSatFromSatoshis(commitFee)

	// Currently, within the protocol, the initiator always pays the fees.
	// So we'll subtract the fee amount from the balance of the current
	// initiator. If the initiator is unable to pay the fee fully, then
	// their entire output is consumed.
	switch {
	case cb.chanState.IsInitiator && commitFee > ourBalance.ToSatoshis():
		ourBalance = 0

	case cb.chanState.IsInitiator:
		ourBalance -= commitFeeMSat

	case !cb.chanState.IsInitiator && commitFee > theirBalance.ToSatoshis():
		theirBalance = 0

	case !cb.chanState.IsInitiator:
		theirBalance -= commitFeeMSat
	}

	var (
		localCfg, remoteCfg         *channeldb.ChannelConfig
		localBalance, remoteBalance btcutil.Amount
	)
	if isOurs {
		localCfg = &cb.chanState.OurChanCfg
		remoteCfg = &cb.chanState.TheirChanCfg
		localBalance = ourBalance.ToSatoshis()
		remoteBalance = theirBalance.ToSatoshis()
	} else {
		localCfg = &cb.chanState.TheirChanCfg
		remoteCfg = &cb.chanState.OurChanCfg
		localBalance = theirBalance.ToSatoshis()
		remoteBalance = ourBalance.ToSatoshis()
	}

	fundingTxIn := *wire.NewTxIn(&cb.chanState.FundingOutpoint, nil, nil)

	// Generate a new commitment transaction with all the latest
	// unsettled/un-timed out HTLCs.
	commitTx, err := CreateCommitTx(
		cb.chanState.ChanType, fundingTxIn, keyRing, localCfg,
		remoteCfg, localBalance, remoteBalance,
	)
	if err != nil {
		return nil, err
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
	for _, htlc := range filteredHTLCView.ourUpdates {
		if htlcIsDust(false, isOurs, feePerKw,
			htlc.Amount.ToSatoshis(), dustLimit) {
			continue
		}

		err := addHTLC(commitTx, isOurs, false, htlc, keyRing)
		if err != nil {
			return nil, err
		}
		cltvs = append(cltvs, htlc.Timeout)
	}
	for _, htlc := range filteredHTLCView.theirUpdates {
		if htlcIsDust(true, isOurs, feePerKw,
			htlc.Amount.ToSatoshis(), dustLimit) {
			continue
		}

		err := addHTLC(commitTx, isOurs, true, htlc, keyRing)
		if err != nil {
			return nil, err
		}
		cltvs = append(cltvs, htlc.Timeout)
	}

	// Set the state hint of the commitment transaction to facilitate
	// quickly recovering the necessary penalty state in the case of an
	// uncooperative broadcast.
	err = SetStateNumHint(commitTx, height, cb.obfuscator)
	if err != nil {
		return nil, err
	}

	// Sort the transactions according to the agreed upon canonical
	// ordering. This lets us skip sending the entire transaction over,
	// instead we'll just send signatures.
	InPlaceCommitSort(commitTx, cltvs)

	// Next, we'll ensure that we don't accidentally create a commitment
	// transaction which would be invalid by consensus.
	uTx := btcutil.NewTx(commitTx)
	if err := blockchain.CheckTransactionSanity(uTx); err != nil {
		return nil, err
	}

	// Finally, we'll assert that were not attempting to draw more out of
	// the channel that was originally placed within it.
	var totalOut btcutil.Amount
	for _, txOut := range commitTx.TxOut {
		totalOut += btcutil.Amount(txOut.Value)
	}
	if totalOut > cb.chanState.Capacity {
		return nil, fmt.Errorf("height=%v, for ChannelPoint(%v) "+
			"attempts to consume %v while channel capacity is %v",
			height, cb.chanState.FundingOutpoint,
			totalOut, cb.chanState.Capacity)
	}

	return &commitmentView{
		txn:          commitTx,
		fee:          commitFee,
		ourBalance:   ourBalance,
		theirBalance: theirBalance,
	}, nil
}

// CreateCommitTx creates a commitment transaction, spending from specified
// funding output. The commitment transaction contains two outputs: one local
// output paying to the "owner" of the commitment transaction which can be
// spent after a relative block delay or revocation event, and a remote output
// paying the counterparty within the channel, which can be spent immediately
// or after a delay depending on the commitment type..
func CreateCommitTx(chanType channeldb.ChannelType,
	fundingOutput wire.TxIn, keyRing *CommitmentKeyRing,
	localChanCfg, remoteChanCfg *channeldb.ChannelConfig,
	amountToLocal, amountToRemote btcutil.Amount) (*wire.MsgTx, error) {

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
	toRemoteScript, err := CommitScriptToRemote(
		chanType, uint32(remoteChanCfg.CsvDelay), keyRing.RemoteKey,
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

	return commitTx, nil
}

// genHtlcScript generates the proper P2WSH public key scripts for the HTLC
// output modified by two-bits denoting if this is an incoming HTLC, and if the
// HTLC is being applied to their commitment transaction or ours.
func genHtlcScript(isIncoming, ourCommit bool, timeout uint32, rHash [32]byte,
	keyRing *CommitmentKeyRing) ([]byte, []byte, error) {

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

// addHTLC adds a new HTLC to the passed commitment transaction. One of four
// full scripts will be generated for the HTLC output depending on if the HTLC
// is incoming and if it's being applied to our commitment transaction or that
// of the remote node's. Additionally, in order to be able to efficiently
// locate the added HTLC on the commitment transaction from the
// PaymentDescriptor that generated it, the generated script is stored within
// the descriptor itself.
func addHTLC(commitTx *wire.MsgTx, ourCommit bool,
	isIncoming bool, paymentDesc *PaymentDescriptor,
	keyRing *CommitmentKeyRing) error {

	timeout := paymentDesc.Timeout
	rHash := paymentDesc.RHash

	p2wsh, witnessScript, err := genHtlcScript(isIncoming, ourCommit,
		timeout, rHash, keyRing)
	if err != nil {
		return err
	}

	// Add the new HTLC outputs to the respective commitment transactions.
	amountPending := int64(paymentDesc.Amount.ToSatoshis())
	commitTx.AddTxOut(wire.NewTxOut(amountPending, p2wsh))

	// Store the pkScript of this particular PaymentDescriptor so we can
	// quickly locate it within the commitment transaction later.
	if ourCommit {
		paymentDesc.ourPkScript = p2wsh
		paymentDesc.ourWitnessScript = witnessScript
	} else {
		paymentDesc.theirPkScript = p2wsh
		paymentDesc.theirWitnessScript = witnessScript
	}

	return nil
}
