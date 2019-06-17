package lnwallet

import (
	"testing"

	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/input"
)

// TestCoinSelect tests that we pick coins adding up to the expected amount
// when creating a funding transaction, and that a change output is created
// only when necessary.
func TestCoinSelect(t *testing.T) {
	t.Parallel()

	const feeRate = SatPerKWeight(100)
	const dustLimit = btcutil.Amount(1000)
	const dust = btcutil.Amount(100)

	// fee is a helper method that returns the fee estimate used for a tx
	// with the given number of inputs and the optional change output. This
	// matches the estimate done by the wallet.
	fee := func(numInput int, change bool) btcutil.Amount {
		var weightEstimate input.TxWeightEstimator

		// All inputs.
		for i := 0; i < numInput; i++ {
			weightEstimate.AddP2WKHInput()
		}

		// The multisig funding output.
		weightEstimate.AddP2WSHOutput()

		// Optionally count a change output.
		if change {
			weightEstimate.AddP2WKHOutput()
		}

		totalWeight := int64(weightEstimate.Weight())
		return feeRate.FeeForWeight(totalWeight)
	}

	type testCase struct {
		outputValue btcutil.Amount
		coins       []*Utxo

		expectedInput      []btcutil.Amount
		expectedFundingAmt btcutil.Amount
		expectedChange     btcutil.Amount
		expectErr          bool
	}

	testCases := []testCase{
		{
			// We have 1.0 BTC available, and wants to send 0.5.
			// This will obviously lead to a change output being
			// created.
			coins: []*Utxo{
				&Utxo{
					AddressType: WitnessPubKey,
					Value:       1 * btcutil.SatoshiPerBitcoin,
				},
			},
			outputValue: 0.5 * btcutil.SatoshiPerBitcoin,

			// The one and only input will be selected.
			expectedInput: []btcutil.Amount{
				1 * btcutil.SatoshiPerBitcoin,
			},
			expectedFundingAmt: 0.5 * btcutil.SatoshiPerBitcoin,
			// Change will be what's left minus the fee.
			expectedChange: 0.5*btcutil.SatoshiPerBitcoin - fee(1, true),
		},
		{
			// We have 1 BTC available, and we want to send 1 BTC.
			// This should lead to an error, as we don't have
			// enough funds to pay the fee.
			coins: []*Utxo{
				&Utxo{
					AddressType: WitnessPubKey,
					Value:       1 * btcutil.SatoshiPerBitcoin,
				},
			},
			outputValue: 1 * btcutil.SatoshiPerBitcoin,
			expectErr:   true,
		},
		{
			// We have a 1 BTC input, and want to create an output
			// as big as possible, while still having a change
			// output.
			coins: []*Utxo{
				&Utxo{
					AddressType: WitnessPubKey,
					Value:       1 * btcutil.SatoshiPerBitcoin,
				},
			},
			// We tune the output value by subtracting the expected
			// fee and a small dust amount.
			outputValue: 1*btcutil.SatoshiPerBitcoin - fee(1, true) - dust,

			expectedInput: []btcutil.Amount{
				1 * btcutil.SatoshiPerBitcoin,
			},
			expectedFundingAmt: 1*btcutil.SatoshiPerBitcoin - fee(1, true) - dust,

			// Change will be below dust limit, and therefore be 0.
			expectedChange: 0,
		},
	}

	for _, test := range testCases {
		selected, localFundingAmt, changeAmt, err := coinSelect(
			feeRate, test.outputValue, dustLimit, test.coins,
		)
		if !test.expectErr && err != nil {
			t.Fatalf(err.Error())
		}

		if test.expectErr && err == nil {
			t.Fatalf("expected error")
		}

		// If we got an expected error, there is nothing more to test.
		if test.expectErr {
			continue
		}

		// Check that the selected inputs match what we expect.
		if len(selected) != len(test.expectedInput) {
			t.Fatalf("expected %v inputs, got %v",
				len(test.expectedInput), len(selected))
		}

		for i, coin := range selected {
			if coin.Value != test.expectedInput[i] {
				t.Fatalf("expected input %v to have value %v, "+
					"had %v", i, test.expectedInput[i],
					coin.Value)
			}
		}

		// Assert we got the expected change amount.
		if localFundingAmt != test.expectedFundingAmt {
			t.Fatalf("expected %v local funding amt, got %v",
				test.expectedFundingAmt, localFundingAmt)
		}
		if changeAmt != test.expectedChange {
			t.Fatalf("expected %v change amt, got %v",
				test.expectedChange, changeAmt)
		}
	}
}
