package sweep

import (
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// MockSweeperStore is a mock implementation of sweeper store. This type is
// exported, because it is currently used in nursery tests too.
type MockSweeperStore struct {
	lastTx  *wire.MsgTx
	ourTxes map[chainhash.Hash]struct{}
}

// NewMockSweeperStore returns a new instance.
func NewMockSweeperStore() *MockSweeperStore {
	return &MockSweeperStore{
		ourTxes: make(map[chainhash.Hash]struct{}),
	}
}

// IsOurTx determines whether a tx is published by us, based on its
// hash.
func (s *MockSweeperStore) IsOurTx(hash chainhash.Hash) (bool, error) {
	_, ok := s.ourTxes[hash]
	return ok, nil
}

// NotifyPublishTx signals that we are about to publish a tx.
func (s *MockSweeperStore) NotifyPublishTx(tx *wire.MsgTx) error {
	txHash := tx.TxHash()
	s.ourTxes[txHash] = struct{}{}
	s.lastTx = tx

	return nil
}

// GetLastPublishedTx returns the last tx that we called NotifyPublishTx
// for.
func (s *MockSweeperStore) GetLastPublishedTx() (*wire.MsgTx, error) {
	return s.lastTx, nil
}

// Compile-time constraint to ensure MockSweeperStore implements SweeperStore.
var _ SweeperStore = (*MockSweeperStore)(nil)

func RunStoreTests(t *testing.T, createStore func() (SweeperStore, error)) {
	store, err := createStore()
	if err != nil {
		t.Fatal(err)
	}

	// Initially we expect the store not to have a last published tx.
	retrievedTx, err := store.GetLastPublishedTx()
	if err != nil {
		t.Fatal(err)
	}
	if retrievedTx != nil {
		t.Fatal("expected no last published tx")
	}

	// Notify publication of tx1
	tx1 := wire.MsgTx{}
	tx1.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Index: 1,
		},
	})

	err = store.NotifyPublishTx(&tx1)
	if err != nil {
		t.Fatal(err)
	}

	// Notify publication of tx2
	tx2 := wire.MsgTx{}
	tx2.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Index: 2,
		},
	})

	err = store.NotifyPublishTx(&tx2)
	if err != nil {
		t.Fatal(err)
	}

	// Recreate the sweeper store
	store, err = createStore()
	if err != nil {
		t.Fatal(err)
	}

	// Assert that last published tx2 is present.
	retrievedTx, err = store.GetLastPublishedTx()
	if err != nil {
		t.Fatal(err)
	}

	if tx2.TxHash() != retrievedTx.TxHash() {
		t.Fatal("txes do not match")
	}

	// Assert that both txes are recognized as our own.
	ours, err := store.IsOurTx(tx1.TxHash())
	if err != nil {
		t.Fatal(err)
	}
	if !ours {
		t.Fatal("expected tx to be ours")
	}

	ours, err = store.IsOurTx(tx2.TxHash())
	if err != nil {
		t.Fatal(err)
	}
	if !ours {
		t.Fatal("expected tx to be ours")
	}

	// An different hash should be reported on as not being ours.
	var unknownHash chainhash.Hash
	ours, err = store.IsOurTx(unknownHash)
	if err != nil {
		t.Fatal(err)
	}
	if ours {
		t.Fatal("expected tx to be not ours")
	}
}
