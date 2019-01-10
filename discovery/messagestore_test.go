package discovery

import (
	"bytes"
	"io/ioutil"
	"math/rand"
	"os"
	"reflect"
	"testing"

	"github.com/btcsuite/btcd/btcec"
	"github.com/coreos/bbolt"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
)

func createTestMessageStore(t *testing.T) (*MessageStore, func()) {
	t.Helper()

	tempDir, err := ioutil.TempDir("", "channeldb")
	if err != nil {
		t.Fatalf("unable to create temp dir: %v", err)
	}
	db, err := channeldb.Open(tempDir)
	if err != nil {
		os.RemoveAll(tempDir)
		t.Fatalf("unable to open db: %v", err)
	}

	cleanUp := func() {
		db.Close()
		os.RemoveAll(tempDir)
	}

	messageStore, err := NewMessageStore(db)
	if err != nil {
		cleanUp()
		t.Fatalf("unable to initialize message store: %v", err)
	}

	return messageStore, cleanUp
}

func randPubKey(t *testing.T) [33]byte {
	t.Helper()

	priv, err := btcec.NewPrivateKey(btcec.S256())
	if err != nil {
		t.Fatalf("unable to create private key: %v", err)
	}

	var pubKey [33]byte
	copy(pubKey[:], priv.PubKey().SerializeCompressed())
	return pubKey
}

func randAnnounceSignatures(t *testing.T) *lnwire.AnnounceSignatures {
	t.Helper()

	return &lnwire.AnnounceSignatures{
		ShortChannelID: lnwire.NewShortChanIDFromInt(rand.Uint64()),
	}
}

func randChannelUpdate(t *testing.T) *lnwire.ChannelUpdate {
	t.Helper()

	return &lnwire.ChannelUpdate{
		ShortChannelID: lnwire.NewShortChanIDFromInt(rand.Uint64()),
	}
}

// TestMessageStore ensures that messages can be properly added, deleted, and
// queried from the store.
func TestMessageStore(t *testing.T) {
	t.Parallel()

	// We'll start by creating our test message store.
	msgStore, cleanUp := createTestMessageStore(t)
	defer cleanUp()

	// We'll then create some test messages for two test peers, and none for
	// an additional test peer.
	channelUpdate1 := randChannelUpdate(t)
	announceSignatures1 := randAnnounceSignatures(t)
	peer1 := randPubKey(t)
	if err := msgStore.AddMessage(channelUpdate1, peer1); err != nil {
		t.Fatalf("unable to add message: %v", err)
	}
	if err := msgStore.AddMessage(announceSignatures1, peer1); err != nil {
		t.Fatalf("unable to add message: %v", err)
	}
	expectedPeerMsgs1 := map[uint64]lnwire.MessageType{
		channelUpdate1.ShortChannelID.ToUint64():      channelUpdate1.MsgType(),
		announceSignatures1.ShortChannelID.ToUint64(): announceSignatures1.MsgType(),
	}

	channelUpdate2 := randChannelUpdate(t)
	peer2 := randPubKey(t)
	if err := msgStore.AddMessage(channelUpdate2, peer2); err != nil {
		t.Fatalf("unable to add message: %v", err)
	}
	expectedPeerMsgs2 := map[uint64]lnwire.MessageType{
		channelUpdate2.ShortChannelID.ToUint64(): channelUpdate2.MsgType(),
	}

	peer3 := randPubKey(t)
	expectedPeerMsgs3 := map[uint64]lnwire.MessageType{}

	// assertPeerMsgs is a helper closure that we'll use to ensure we
	// retrieve the correct set of messages for a given peer.
	assertPeerMsgs := func(peerMsgs []lnwire.Message,
		expected map[uint64]lnwire.MessageType) {

		t.Helper()

		if len(peerMsgs) != len(expected) {
			t.Fatalf("expected %d pending messages, got %d",
				len(expected), len(peerMsgs))
		}
		for _, msg := range peerMsgs {
			var shortChanID uint64
			switch msg := msg.(type) {
			case *lnwire.AnnounceSignatures:
				shortChanID = msg.ShortChannelID.ToUint64()
			case *lnwire.ChannelUpdate:
				shortChanID = msg.ShortChannelID.ToUint64()
			default:
				t.Fatalf("found unexpected message type %T", msg)
			}

			msgType, ok := expected[shortChanID]
			if !ok {
				t.Fatalf("retrieved message with unexpected ID "+
					"%d from store", shortChanID)
			}
			if msgType != msg.MsgType() {
				t.Fatalf("expected message of type %v, got %v",
					msg.MsgType(), msgType)
			}
		}
	}

	// Then, we'll query the store for the set of messages for each peer and
	// ensure it matches what we expect.
	peers := [][33]byte{peer1, peer2, peer3}
	expectedPeerMsgs := []map[uint64]lnwire.MessageType{
		expectedPeerMsgs1, expectedPeerMsgs2, expectedPeerMsgs3,
	}
	for i, peer := range peers {
		peerMsgs, err := msgStore.MessagesForPeer(peer)
		if err != nil {
			t.Fatalf("unable to retrieve messages: %v", err)
		}
		assertPeerMsgs(peerMsgs, expectedPeerMsgs[i])
	}

	// Finally, we'll query the store for all of its messages of every peer.
	// Again, each peer should have a set of messages that match what we
	// expect.
	//
	// We'll construct the expected response. Only the first two peers will
	// have messages.
	totalPeerMsgs := make(map[[33]byte]map[uint64]lnwire.MessageType, 2)
	for i := 0; i < 2; i++ {
		totalPeerMsgs[peers[i]] = expectedPeerMsgs[i]
	}

	msgs, err := msgStore.Messages()
	if err != nil {
		t.Fatalf("unable to retrieve all peers with pending messages: "+
			"%v", err)
	}
	if len(msgs) != len(totalPeerMsgs) {
		t.Fatalf("expected %d peers with messages, got %d",
			len(totalPeerMsgs), len(msgs))
	}
	for peer, peerMsgs := range msgs {
		expected, ok := totalPeerMsgs[peer]
		if !ok {
			t.Fatalf("expected to find pending messages for peer %x",
				peer)
		}

		assertPeerMsgs(peerMsgs, expected)
	}
}

// TestMessageStoreUnsupportedMessage ensures that we are not able to add a
// message which is unsupported.
func TestMessageStoreUnsupportedMessage(t *testing.T) {
	t.Parallel()

	// We'll start by creating our test message store.
	msgStore, cleanUp := createTestMessageStore(t)
	defer cleanUp()

	// Create a message that is known to not be supported by the store.
	peer := randPubKey(t)
	unsupportedMsg := &lnwire.NodeAnnouncement{}

	// Attempting to add it to the store should result in
	// ErrUnsupportedMessage.
	err := msgStore.AddMessage(unsupportedMsg, peer)
	if err != ErrUnsupportedMessage {
		t.Fatalf("expected ErrUnsupportedMessage, got %v", err)
	}
}

// TestLegacyMessageStore ensures a message with a legacy key format within the
// store can be properly removed from it.
func TestLegacyMessageStore(t *testing.T) {
	t.Parallel()

	// We'll start by creating our test message store.
	msgStore, cleanUp := createTestMessageStore(t)
	defer cleanUp()

	// Create a message which we'll insert into the store with a legacy key.
	peerPubKey := randPubKey(t)
	msg := randAnnounceSignatures(t)
	legacyMsgKey := oldMessageStoreKey(msg, peerPubKey)
	err := msgStore.db.Update(func(tx *bbolt.Tx) error {
		messageStore := tx.Bucket(messageStoreBucket)
		var b bytes.Buffer
		if err := msg.Encode(&b, 0); err != nil {
			return err
		}
		return messageStore.Put(legacyMsgKey, b.Bytes())
	})
	if err != nil {
		t.Fatalf("unable to add message with legacy key: %v", err)
	}

	// Then, we'll ensure we can properly retrieve it.
	msgs, err := msgStore.MessagesForPeer(peerPubKey)
	if err != nil {
		t.Fatalf("unable to retrieve messages for peer: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message for peer, got %d", len(msgs))
	}
	if !reflect.DeepEqual(msgs[0], msg) {
		t.Fatalf("expected message: %v\ngot message: %v",
			spew.Sdump(msg), spew.Sdump(msgs[0]))
	}

	// Finally, ensure we can properly remove the message from the store.
	if err := msgStore.DeleteMessage(msg, peerPubKey); err != nil {
		t.Fatalf("unable to delete legacy message: %v", err)
	}
	msgs, err = msgStore.MessagesForPeer(peerPubKey)
	if err != nil {
		t.Fatalf("unable to retrieve messages for peer: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("expected 0 messages for peer, got %d", len(msgs))
	}
}
