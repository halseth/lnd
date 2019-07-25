package sweep

import (
	"io/ioutil"
	"os"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/channeldb"
)

// makeTestDB creates a new instance of the ChannelDB for testing purposes. A
// callback which cleans up the created temporary directories is also returned
// and intended to be executed after the test completes.
func makeTestDB() (*channeldb.DB, func(), error) {
	// First, create a temporary directory to be used for the duration of
	// this test.
	tempDirName, err := ioutil.TempDir("", "channeldb")
	if err != nil {
		return nil, nil, err
	}

	// Next, create channeldb for the first time.
	cdb, err := channeldb.Open(tempDirName)
	if err != nil {
		return nil, nil, err
	}

	cleanUp := func() {
		cdb.Close()
		os.RemoveAll(tempDirName)
	}

	return cdb, cleanUp, nil
}

// TestStore asserts that the store persists the presented data to disk and is
// able to retrieve it again.
func TestStore(t *testing.T) {
	t.Run("bolt", func(t *testing.T) {

		// Create new store.
		cdb, cleanUp, err := makeTestDB()
		if err != nil {
			t.Fatalf("unable to open channel db: %v", err)
		}
		defer cleanUp()

		if err != nil {
			t.Fatal(err)
		}

		RunStoreTests(t, func() (SweeperStore, error) {
			var chain chainhash.Hash
			return NewSweeperStore(cdb, &chain)
		})
	})
	t.Run("mock", func(t *testing.T) {
		store := NewMockSweeperStore()

		RunStoreTests(t, func() (SweeperStore, error) {
			// Return same store, because the mock has no real
			// persistence.
			return store, nil
		})
	})
}
