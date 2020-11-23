package contractcourt

import (
	"encoding/binary"
	"io"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/labels"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/sweep"
)

// htlcSuccessResolver is a resolver that's capable of sweeping an incoming
// HTLC output on-chain. If this is the remote party's commitment, we'll sweep
// it directly from the commitment output *immediately*. If this is our
// commitment, we'll first broadcast the success transaction, then send it to
// the incubator for sweeping. That's it, no need to send any clean up
// messages.
//
// TODO(roasbeef): don't need to broadcast?
type htlcSuccessResolver struct {
	// htlcResolution is the incoming HTLC resolution for this HTLC. It
	// contains everything we need to properly resolve this HTLC.
	htlcResolution lnwallet.IncomingHtlcResolution

	// outputIncubating returns true if we've sent the output to the output
	// incubator (utxo nursery).
	outputIncubating bool

	// resolved reflects if the contract has been fully resolved or not.
	resolved bool

	// broadcastHeight is the height that the original contract was
	// broadcast to the main-chain at. We'll use this value to bound any
	// historical queries to the chain for spends/confirmations.
	broadcastHeight uint32

	// sweepTx will be non-nil if we've already crafted a transaction to
	// sweep a direct HTLC output. This is only a concern if we're sweeping
	// from the commitment transaction of the remote party.
	//
	// TODO(roasbeef): send off to utxobundler
	sweepTx *wire.MsgTx

	// htlc contains information on the htlc that we are resolving on-chain.
	htlc channeldb.HTLC

	contractResolverKit
}

// newSuccessResolver instanties a new htlc success resolver.
func newSuccessResolver(res lnwallet.IncomingHtlcResolution,
	broadcastHeight uint32, htlc channeldb.HTLC,
	resCfg ResolverConfig) *htlcSuccessResolver {

	return &htlcSuccessResolver{
		contractResolverKit: *newContractResolverKit(resCfg),
		htlcResolution:      res,
		broadcastHeight:     broadcastHeight,
		htlc:                htlc,
	}
}

// ResolverKey returns an identifier which should be globally unique for this
// particular resolver within the chain the original contract resides within.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcSuccessResolver) ResolverKey() []byte {
	// The primary key for this resolver will be the outpoint of the HTLC
	// on the commitment transaction itself. If this is our commitment,
	// then the output can be found within the signed success tx,
	// otherwise, it's just the ClaimOutpoint.
	var op wire.OutPoint
	if h.htlcResolution.SignedSuccessTx != nil {
		op = h.htlcResolution.SignedSuccessTx.TxIn[0].PreviousOutPoint
	} else {
		op = h.htlcResolution.ClaimOutpoint
	}

	key := newResolverID(op)
	return key[:]
}

// waitForSpend waits for the given outpoint to be spent, and returns the
// details of the spending tx.
func (h *htlcSuccessResolver) waitForSpend(op *wire.OutPoint,
	pkScript []byte) (*chainntnfs.SpendDetail, error) {

	spendNtfn, err := h.Notifier.RegisterSpendNtfn(
		op, pkScript, h.broadcastHeight,
	)
	if err != nil {
		return nil, err
	}

	select {
	case spendDetail, ok := <-spendNtfn.Spend:
		if !ok {
			return nil, errResolverShuttingDown
		}

		return spendDetail, nil

	case <-h.quit:
		return nil, errResolverShuttingDown
	}
}

// Resolve attempts to resolve an unresolved incoming HTLC that we know the
// preimage to. If the HTLC is on the commitment of the remote party, then we'll
// simply sweep it directly. Otherwise, we'll hand this off to the utxo nursery
// to do its duty. There is no need to make a call to the invoice registry
// anymore. Every HTLC has already passed through the incoming contest resolver
// and in there the invoice was already marked as settled.
//
// TODO(roasbeef): create multi to batch
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcSuccessResolver) Resolve() (ContractResolver, error) {
	// If we're already resolved, then we can exit early.
	if h.resolved {
		return nil, nil
	}

	// If we don't have a success transaction, then this means that this is
	// an output on the remote party's commitment transaction.
	if h.htlcResolution.SignedSuccessTx == nil {
		return h.resolveRemoteCommitOutput()
	}

	log.Infof("%T(%x): broadcasting second-layer transition tx: %v",
		h, h.htlc.RHash[:], spew.Sdump(h.htlcResolution.SignedSuccessTx))

	// We'll now broadcast the second layer transaction so we can kick off
	// the claiming process.
	//
	// TODO(roasbeef): after changing sighashes send to tx bundler
	label := labels.MakeLabel(
		labels.LabelTypeChannelClose, &h.ShortChanID,
	)
	err := h.PublishTx(h.htlcResolution.SignedSuccessTx, label)
	if err != nil {
		return nil, err
	}

	// Otherwise, this is an output on our commitment transaction. In this
	// case, we'll send it to the incubator, but only if we haven't already
	// done so.
	if !h.outputIncubating {
		log.Infof("%T(%x): incubating incoming htlc output",
			h, h.htlc.RHash[:])

		err := h.IncubateOutputs(
			h.ChanPoint, nil, &h.htlcResolution,
			h.broadcastHeight,
		)
		if err != nil {
			return nil, err
		}

		h.outputIncubating = true

		if err := h.Checkpoint(h); err != nil {
			log.Errorf("unable to Checkpoint: %v", err)
			return nil, err
		}
	}

	// To wrap this up, we'll wait until the second-level transaction has
	// been spent, then fully resolve the contract.
	log.Infof("%T(%x): waiting for second-level HTLC output to be spent "+
		"after csv_delay=%v", h, h.htlc.RHash[:], h.htlcResolution.CsvDelay)

	spend, err := h.waitForSpend(
		&h.htlcResolution.ClaimOutpoint,
		h.htlcResolution.SweepSignDesc.Output.PkScript,
	)
	if err != nil {
		return nil, err
	}

	h.resolved = true
	return nil, h.checkpointClaim(
		spend.SpenderTxHash, channeldb.ResolverOutcomeClaimed,
	)
}

// resolveRemoteCommitOutput handles sweeping an HTLC output on the remote
// commitment with the preimage. In this case we can sweep the output directly,
// and don't have to broadcast a second-level transaction.
func (h *htlcSuccessResolver) resolveRemoteCommitOutput() (
	ContractResolver, error) {

	// If we don't already have the sweep transaction constructed,
	// we'll do so and broadcast it.
	if h.sweepTx == nil {
		log.Infof("%T(%x): crafting sweep tx for "+
			"incoming+remote htlc confirmed", h,
			h.htlc.RHash[:])

		// Before we can craft out sweeping transaction, we
		// need to create an input which contains all the items
		// required to add this input to a sweeping transaction,
		// and generate a witness.
		inp := input.MakeHtlcSucceedInput(
			&h.htlcResolution.ClaimOutpoint,
			&h.htlcResolution.SweepSignDesc,
			h.htlcResolution.Preimage[:],
			h.broadcastHeight,
			h.htlcResolution.CsvDelay,
		)

		// With the input created, we can now generate the full
		// sweep transaction, that we'll use to move these
		// coins back into the backing wallet.
		//
		// TODO: Set tx lock time to current block height
		// instead of zero. Will be taken care of once sweeper
		// implementation is complete.
		//
		// TODO: Use time-based sweeper and result chan.
		var err error
		h.sweepTx, err = h.Sweeper.CreateSweepTx(
			[]input.Input{&inp},
			sweep.FeePreference{
				ConfTarget: sweepConfTarget,
			}, 0,
		)
		if err != nil {
			return nil, err
		}

		log.Infof("%T(%x): crafted sweep tx=%v", h,
			h.htlc.RHash[:], spew.Sdump(h.sweepTx))

		// With the sweep transaction signed, we'll now
		// Checkpoint our state.
		if err := h.Checkpoint(h); err != nil {
			log.Errorf("unable to Checkpoint: %v", err)
			return nil, err
		}
	}

	// Regardless of whether an existing transaction was found or newly
	// constructed, we'll broadcast the sweep transaction to the
	// network.
	label := labels.MakeLabel(
		labels.LabelTypeChannelClose, &h.ShortChanID,
	)
	err := h.PublishTx(h.sweepTx, label)
	if err != nil {
		log.Infof("%T(%x): unable to publish tx: %v",
			h, h.htlc.RHash[:], err)
		return nil, err
	}

	// With the sweep transaction broadcast, we'll wait for its
	// confirmation.
	sweepTXID := h.sweepTx.TxHash()
	sweepScript := h.sweepTx.TxOut[0].PkScript
	confNtfn, err := h.Notifier.RegisterConfirmationsNtfn(
		&sweepTXID, sweepScript, 1, h.broadcastHeight,
	)
	if err != nil {
		return nil, err
	}

	log.Infof("%T(%x): waiting for sweep tx (txid=%v) to be "+
		"confirmed", h, h.htlc.RHash[:], sweepTXID)

	select {
	case _, ok := <-confNtfn.Confirmed:
		if !ok {
			return nil, errResolverShuttingDown
		}

	case <-h.quit:
		return nil, errResolverShuttingDown
	}

	// Once the transaction has received a sufficient number of
	// confirmations, we'll mark ourselves as fully resolved and exit.
	h.resolved = true

	// Checkpoint the resolver, and write the outcome to disk.
	return nil, h.checkpointClaim(
		&sweepTXID,
		channeldb.ResolverOutcomeClaimed,
	)
}

// checkpointClaim checkpoints the success resolver with the reports it needs.
// If this htlc was claimed two stages, it will write reports for both stages,
// otherwise it will just write for the single htlc claim.
func (h *htlcSuccessResolver) checkpointClaim(spendTx *chainhash.Hash,
	outcome channeldb.ResolverOutcome) error {

	// Create a resolver report for claiming of the htlc itself.
	amt := btcutil.Amount(h.htlcResolution.SweepSignDesc.Output.Value)
	reports := []*channeldb.ResolverReport{
		{
			OutPoint:        h.htlcResolution.ClaimOutpoint,
			Amount:          amt,
			ResolverType:    channeldb.ResolverTypeIncomingHtlc,
			ResolverOutcome: outcome,
			SpendTxID:       spendTx,
		},
	}

	// If we have a success tx, we append a report to represent our first
	// stage claim.
	if h.htlcResolution.SignedSuccessTx != nil {
		// If the SignedSuccessTx is not nil, we are claiming the htlc
		// in two stages, so we need to create a report for the first
		// stage transaction as well.
		spendTx := h.htlcResolution.SignedSuccessTx
		spendTxID := spendTx.TxHash()

		report := &channeldb.ResolverReport{
			OutPoint:        spendTx.TxIn[0].PreviousOutPoint,
			Amount:          h.htlc.Amt.ToSatoshis(),
			ResolverType:    channeldb.ResolverTypeIncomingHtlc,
			ResolverOutcome: channeldb.ResolverOutcomeFirstStage,
			SpendTxID:       &spendTxID,
		}
		reports = append(reports, report)
	}

	// Finally, we checkpoint the resolver with our report(s).
	return h.Checkpoint(h, reports...)
}

// Stop signals the resolver to cancel any current resolution processes, and
// suspend.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcSuccessResolver) Stop() {
	close(h.quit)
}

// IsResolved returns true if the stored state in the resolve is fully
// resolved. In this case the target output can be forgotten.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcSuccessResolver) IsResolved() bool {
	return h.resolved
}

// Encode writes an encoded version of the ContractResolver into the passed
// Writer.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcSuccessResolver) Encode(w io.Writer) error {
	// First we'll encode our inner HTLC resolution.
	if err := encodeIncomingResolution(w, &h.htlcResolution); err != nil {
		return err
	}

	// Next, we'll write out the fields that are specified to the contract
	// resolver.
	if err := binary.Write(w, endian, h.outputIncubating); err != nil {
		return err
	}
	if err := binary.Write(w, endian, h.resolved); err != nil {
		return err
	}
	if err := binary.Write(w, endian, h.broadcastHeight); err != nil {
		return err
	}
	if _, err := w.Write(h.htlc.RHash[:]); err != nil {
		return err
	}

	return nil
}

// newSuccessResolverFromReader attempts to decode an encoded ContractResolver
// from the passed Reader instance, returning an active ContractResolver
// instance.
func newSuccessResolverFromReader(r io.Reader, resCfg ResolverConfig) (
	*htlcSuccessResolver, error) {

	h := &htlcSuccessResolver{
		contractResolverKit: *newContractResolverKit(resCfg),
	}

	// First we'll decode our inner HTLC resolution.
	if err := decodeIncomingResolution(r, &h.htlcResolution); err != nil {
		return nil, err
	}

	// Next, we'll read all the fields that are specified to the contract
	// resolver.
	if err := binary.Read(r, endian, &h.outputIncubating); err != nil {
		return nil, err
	}
	if err := binary.Read(r, endian, &h.resolved); err != nil {
		return nil, err
	}
	if err := binary.Read(r, endian, &h.broadcastHeight); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(r, h.htlc.RHash[:]); err != nil {
		return nil, err
	}

	return h, nil
}

// Supplement adds additional information to the resolver that is required
// before Resolve() is called.
//
// NOTE: Part of the htlcContractResolver interface.
func (h *htlcSuccessResolver) Supplement(htlc channeldb.HTLC) {
	h.htlc = htlc
}

// HtlcPoint returns the htlc's outpoint on the commitment tx.
//
// NOTE: Part of the htlcContractResolver interface.
func (h *htlcSuccessResolver) HtlcPoint() wire.OutPoint {
	return h.htlcResolution.HtlcPoint()
}

// A compile time assertion to ensure htlcSuccessResolver meets the
// ContractResolver interface.
var _ htlcContractResolver = (*htlcSuccessResolver)(nil)
