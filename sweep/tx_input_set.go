package sweep

import (
	"fmt"
	"math"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcwallet/wallet/txrules"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
)

// addConstraints defines the constraints to apply when adding an input.
type addConstraints uint8

const (
	// constraintsRegular is for regular input sweeps that should have a
	// positive yield.
	constraintsRegular addConstraints = iota

	// constraintsWallet is for wallet inputs that are only added to bring
	// up the tx output value.
	constraintsWallet

	// constraintsForce is for inputs that should be swept even with a
	// negative yield at the set fee rate.
	constraintsForce
)

type txInputSetState struct {
	//n feeRate is the fee rate to use for the sweep transaction.
	feeRate chainfee.SatPerKWeight

	// inputs is the set of tx inputs.
	inputs []input.Input

	// inputConstrains keeps track of any constraints assigned to the
	// inputs.
	inputConstraints map[input.Input]addConstraints
}

// weightEstimate is the (worst case) tx weight with the current set of
// inputs.
func (t *txInputSetState) weightEstimate(change bool) *weightEstimator {
	weightEstimate := newWeightEstimator(t.feeRate)
	for _, i := range t.inputs {
		// Can ignore error, because it has already been checked when
		// calculating the yields.
		_ = weightEstimate.add(i)

		r := i.RequiredTxOut()
		if r == nil {
			continue
		}

		weightEstimate.addOutput(r)
	}

	// Add a change output to the weight estimate if requested.
	if change {
		weightEstimate.addP2WKHOutput()
	}

	return weightEstimate
}

// inputTotal returns the total value of all inputs.
func (t *txInputSetState) inputTotal() btcutil.Amount {
	var total btcutil.Amount
	for _, inp := range t.inputs {
		total += btcutil.Amount(inp.SignDesc().Output.Value)
	}

	return total
}

// walletInputTotal returns the total value of inputs coming from the wallet.
func (t *txInputSetState) walletInputTotal() btcutil.Amount {
	var total btcutil.Amount
	for _, inp := range t.inputs {
		c, ok := t.inputConstraints[inp]
		if !ok || c != constraintsWallet {
			continue
		}
		total += btcutil.Amount(inp.SignDesc().Output.Value)
	}

	return total
}

// force indicates that this set must be swept even if the total yield
// is negative.
func (t *txInputSetState) force() bool {
	for _, inp := range t.inputs {
		c, ok := t.inputConstraints[inp]
		if !ok || c != constraintsForce {
			continue
		}

		return true
	}

	return false
}

// requiredOutput is the sum of the outputs committed to by the inputs.
func (t *txInputSetState) requiredOutput() btcutil.Amount {
	var reqOut btcutil.Amount
	for _, inp := range t.inputs {
		r := inp.RequiredTxOut()
		if r == nil {
			continue
		}

		reqOut += btcutil.Amount(r.Value)
	}

	return reqOut
}

// change is the value that is left over after subtracting the requiredOutput
// and the tx fee from the inputTotal.
//
// NOTE: This can be dust, or negative.
func (t *txInputSetState) change() btcutil.Amount {
	inputTotal := t.inputTotal()
	requiredOutput := t.requiredOutput()

	// Recalculate the tx fee with a change output present.
	weightEstimate := t.weightEstimate(true)
	fee := weightEstimate.fee()

	return inputTotal - (requiredOutput + fee)
}

// outputValue is the estimated total value we get out of a sweep tx of this
// input set. This is always estiamted having a change output, since that is
// needed to compare yields when adding a new input to the set.
//
// NOTE: This can be dust, or negative.
func (t *txInputSetState) outputValue() btcutil.Amount {
	inputTotal := t.inputTotal()
	if inputTotal <= 0 {
		return 0
	}

	// Recalculate the tx fee.
	weightEstimate := t.weightEstimate(true)
	fee := weightEstimate.fee()

	// Calculate the new output value.
	return inputTotal - fee
}

func (t *txInputSetState) clone() txInputSetState {
	s := txInputSetState{
		feeRate:          t.feeRate,
		inputs:           make([]input.Input, len(t.inputs)),
		inputConstraints: make(map[input.Input]addConstraints),
	}

	copy(s.inputs, t.inputs)
	for inp, c := range t.inputConstraints {
		s.inputConstraints[inp] = c
	}

	return s
}

// txInputSet is an object that accumulates tx inputs and keeps running counters
// on various properties of the tx.
type txInputSet struct {
	txInputSetState

	// dustLimit is the minimum output value of the tx.
	dustLimit btcutil.Amount

	// maxInputs is the maximum number of inputs that will be accepted in
	// the set.
	maxInputs int

	// wallet contains wallet functionality required by the input set to
	// retrieve utxos.
	wallet Wallet
}

func dustLimit(relayFee chainfee.SatPerKWeight) btcutil.Amount {
	return txrules.GetDustThreshold(
		input.P2WPKHSize,
		btcutil.Amount(relayFee.FeePerKVByte()),
	)
}

// newTxInputSet constructs a new, empty input set.
func newTxInputSet(wallet Wallet, feePerKW,
	relayFee chainfee.SatPerKWeight, maxInputs int) *txInputSet {

	dustLimit := dustLimit(relayFee)

	state := txInputSetState{
		feeRate: feePerKW,
	}

	b := txInputSet{
		dustLimit:       dustLimit,
		maxInputs:       maxInputs,
		wallet:          wallet,
		txInputSetState: state,
	}

	return &b
}

// enoughInput returns true if we've accumulated enough inputs to pay the fees
// and have at least one output that meets the dust limit.
func (t *txInputSet) enoughInput() bool {
	// The sum of our inputs musy be larger than the sum of our required
	// outputs.
	input := t.inputTotal()
	reqOut := t.requiredOutput()
	if input < reqOut {
		return false
	}

	// We need at least one output above the dustlimit. Check if the change
	// output is.
	if t.change() >= t.dustLimit {
		return true
	}

	// We did not have enough input for a change output. Check if we have
	// enough input to pay the fees for a transaction with no change
	// output.
	weightEstimate := t.weightEstimate(false)
	fee := weightEstimate.fee()
	if input < reqOut+fee {
		return false
	}

	// We could pay the fees, but we still need at least one output to be
	// above the dust limit for the tx to be valid (we assume that these
	// required outputs only get added if they are above dust)
	for _, inp := range t.inputs {
		if inp.RequiredTxOut() != nil {
			return true
		}
	}

	return false
}

// add adds a new input to the set. It returns a bool indicating whether the
// input was added to the set. An input is rejected if it decreases the tx
// output value after paying fees.
func (t *txInputSet) addToState(inp input.Input, constraints addConstraints) *txInputSetState {
	value := btcutil.Amount(inp.SignDesc().Output.Value)
	log.Infof("johan adding input of value %v", value)

	// Stop if max inputs is reached. Do not count additional wallet inputs,
	// because we don't know in advance how many we may need.
	if constraints != constraintsWallet &&
		len(t.inputs) >= t.maxInputs {

		return nil
	}

	// If the input comes with a required tx out that is below dust, we
	// won't add it.
	reqOut := inp.RequiredTxOut()
	if reqOut != nil && btcutil.Amount(reqOut.Value) < t.dustLimit {
		log.Infof("johan dust req out")
		return nil
	}

	// Clone the current set state.
	s := t.clone()

	// Add the new input.
	s.inputs = append(s.inputs, inp)
	s.inputConstraints[inp] = constraints

	log.Infof("johan output value is %v", s.outputValue())

	// Can ignore error, because it has already been checked when
	// calculating the yields.
	weightEstimate := s.weightEstimate(true)

	// TODO: remove ref to weight estimate, only keep inputs.
	//	_ = s.weightEstimate.add(inp)

	// Add the value of the new input.
	log.Infof("johan new input total %v", s.inputTotal())

	// Calculate the new output value.
	// If the new input commits to an output, add that to our weight
	// estimate.
	if reqOut != nil {
		log.Infof("johan adding req out of val %v", btcutil.Amount(reqOut.Value))
	}

	log.Infof("johan required output %v", s.requiredOutput())

	// Recalculate the tx fee.
	fee := weightEstimate.fee()
	log.Infof("johan fee %v", fee)
	log.Infof("johan toatal output %v", s.outputValue())

	// Calculate the yield of this input from the change in tx output value.
	inputYield := s.outputValue() - t.outputValue()

	// If the change output is negative at this point, it means the input
	// cannot pay for its own fee. It would never be economical to add!
	//	if s.changeOutput < 0 {
	//		log.Infof("johan negative change output %v", s.changeOutput)
	//		return nil
	//	}

	switch constraints {

	// Don't sweep inputs that cost us more to sweep than they give us.
	case constraintsRegular:
		if inputYield <= 0 {
			return nil
		}

	// For force adds, no further constraints apply.
	case constraintsForce:

	// We are attaching a wallet input to raise the tx output value above
	// the dust limit.
	case constraintsWallet:
		// Skip this wallet input if adding it would lower the output
		// value.
		if inputYield <= 0 {
			return nil
		}

		// Calculate the total value that we spend in this tx from the
		// wallet if we'd add this wallet input.
		log.Infof("johan new wallet input total %v", s.walletInputTotal())

		// In any case, we don't want to lose money by sweeping. If we
		// don't get more out of the tx then we put in ourselves, do not
		// add this wallet input. If there is at least one force sweep
		// in the set, this does no longer apply.
		//
		// We should only add wallet inputs to get the tx output value
		// above the dust limit, otherwise we'd only burn into fees.
		// This is guarded by tryAddWalletInputsIfNeeded.
		//
		// TODO(joostjager): Possibly require a max ratio between the
		// value of the wallet input and what we get out of this
		// transaction. To prevent attaching and locking a big utxo for
		// very little benefit.
		if !s.force() && s.walletInputTotal() >= s.outputValue() {
			value := btcutil.Amount(inp.SignDesc().Output.Value)
			log.Debugf("Rejecting wallet input of %v, because it "+
				"would make a negative yielding transaction "+
				"(%v)",
				value, s.outputValue()-s.walletInputTotal())

			return nil
		}
	}

	// add test cases: anchor + extra wallet input
	// in.out with output smaller than input:
	//	- that cannot pay its own fee (not economical)
	//	- that is econimocail but needs extrra input for fee.
	// in.out with output equal to input:
	//	- smalle output so not comocmical
	//	- large output so economical
	// in-out with output larger than input
	//	- smalle output so not comocmical
	//	- large output so economical

	log.Infof("johan returning new state")
	return &s
}

// add adds a new input to the set. It returns a bool indicating whether the
// input was added to the set. An input is rejected if it decreases the tx
// output value after paying fees.
func (t *txInputSet) add(input input.Input, constraints addConstraints) bool {
	log.Infof("johan adding input to state")

	newState := t.addToState(input, constraints)
	if newState == nil {
		log.Infof("johan input NOT added to state")
		return false
	}

	log.Infof("johan input added to state!")
	t.txInputSetState = *newState

	return true
}

// addPositiveYieldInputs adds sweepableInputs that have a positive yield to the
// input set. This function assumes that the list of inputs is sorted descending
// by yield.
//
// TODO(roasbeef): Consider including some negative yield inputs too to clean
// up the utxo set even if it costs us some fees up front.  In the spirit of
// minimizing any negative externalities we cause for the Bitcoin system as a
// whole.
func (t *txInputSet) addPositiveYieldInputs(sweepableInputs []txInput) {
	log.Infof("johan adding positive yield inputs ")

	for _, input := range sweepableInputs {
		// Apply relaxed constraints for force sweeps.
		constraints := constraintsRegular
		if input.parameters().Force {
			constraints = constraintsForce
			log.Infof("adding force contraint")
		}

		// Try to add the input to the transaction. If that doesn't
		// succeed because it wouldn't increase the output value,
		// return. Assuming inputs are sorted by yield, any further
		// inputs wouldn't increase the output value either.
		if !t.add(input, constraints) {
			log.Infof("johan adding positive yield inputs returning")
			return
		}
	}

	// We managed to add all inputs to the set.
	log.Infof("johan added ALL positive yield inputs")
}

// tryAddWalletInputsIfNeeded retrieves utxos from the wallet and tries adding as
// many as required to bring the tx output value above the given minimum.
func (t *txInputSet) tryAddWalletInputsIfNeeded() error {
	log.Infof("johan try add wallet inputs")
	// If we've already have enough to pay the transaction fees and have at
	// least one output materialize, no action is needed.
	if t.enoughInput() {
		log.Infof("johan had enough input!")
		return nil
	}

	// Retrieve wallet utxos. Only consider confirmed utxos to prevent
	// problems around RBF rules for unconfirmed inputs.
	utxos, err := t.wallet.ListUnspentWitness(1, math.MaxInt32)
	if err != nil {
		return err
	}

	for _, utxo := range utxos {
		input, err := createWalletTxInput(utxo)
		if err != nil {
			return err
		}

		// If the wallet input isn't positively-yielding at this fee
		// rate, skip it.
		log.Infof("johan adding wallet input")
		if !t.add(input, constraintsWallet) {
			log.Infof("johan wallet input not added")
			continue
		}

		// Return if we've reached the minimum output amount.
		if t.enoughInput() {
			log.Infof("johan NOW we have enough input")
			return nil
		}
	}

	// We were not able to reach the minimum output amount.
	return nil
}

// createWalletTxInput converts a wallet utxo into an object that can be added
// to the other inputs to sweep.
func createWalletTxInput(utxo *lnwallet.Utxo) (input.Input, error) {
	var witnessType input.WitnessType
	switch utxo.AddressType {
	case lnwallet.WitnessPubKey:
		witnessType = input.WitnessKeyHash
	case lnwallet.NestedWitnessPubKey:
		witnessType = input.NestedWitnessKeyHash
	default:
		return nil, fmt.Errorf("unknown address type %v",
			utxo.AddressType)
	}

	signDesc := &input.SignDescriptor{
		Output: &wire.TxOut{
			PkScript: utxo.PkScript,
			Value:    int64(utxo.Value),
		},
		HashType: txscript.SigHashAll,
	}

	// A height hint doesn't need to be set, because we don't monitor these
	// inputs for spend.
	heightHint := uint32(0)

	return input.NewBaseInput(
		&utxo.OutPoint, witnessType, signDesc, heightHint,
	), nil
}
