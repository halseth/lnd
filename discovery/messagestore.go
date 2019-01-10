package discovery

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/coreos/bbolt"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// messageStoreBucket is a key used to create a top level bucket in the
	// gossiper database, used for storing messages that are to be sent to
	// peers. Upon restarts, these messages will be read and resent to their
	// respective peers.
	//
	// maps:
	//   pubKey (33 bytes) + msgShortChanID (8 bytes) + msgType (2 bytes) -> msg
	messageStoreBucket = []byte("message-store")

	// ErrUnsupportedMessage is an error returned when we attempt to add a
	// message to the store that is not supported.
	ErrUnsupportedMessage = errors.New("unsupported message type")

	// ErrCorruptedMessageStore indicates that the on-disk bucketing
	// structure has altered since the gossip message store instance was
	// initialized.
	ErrCorruptedMessageStore = errors.New("gossip message store has been " +
		"corrupted")
)

// MessageStore is a store responsible for storing gossip messages which we
// should reliably send to our peers. By design, this store will only keep the
// latest version of a message (like in the case of multiple ChannelUpdate's)
// for a channel with a peer.
type MessageStore struct {
	db *channeldb.DB
}

// NewMessageStore creates a new message store backed by a channeldb instance.
func NewMessageStore(db *channeldb.DB) (*MessageStore, error) {
	err := db.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(messageStoreBucket)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("unable to create required buckets: %v",
			err)
	}

	return &MessageStore{db}, nil
}

// oldMessageStoreKey constructs the old message store key for
// AnnounceSignatures messages. This key will be used first when attempting to
// retrieve a AnnounceSignatures message from the store in order to provide
// backwards-compatibility and avoid a database migration.
func oldMessageStoreKey(msg *lnwire.AnnounceSignatures, peerPubKey [33]byte) []byte {
	var k [33 + 8]byte
	copy(k[:33], peerPubKey[:])
	binary.BigEndian.PutUint64(k[33:41], msg.ShortChannelID.ToUint64())

	return k[:]
}

// msgShortChanID retrieves the short channel ID of the message.
func msgShortChanID(msg lnwire.Message) (lnwire.ShortChannelID, error) {
	var shortChanID lnwire.ShortChannelID
	switch msg := msg.(type) {
	case *lnwire.AnnounceSignatures:
		shortChanID = msg.ShortChannelID
	case *lnwire.ChannelUpdate:
		shortChanID = msg.ShortChannelID
	default:
		return shortChanID, ErrUnsupportedMessage
	}

	return shortChanID, nil
}

// messageStoreKey constructs the database key for the message to be stored.
func messageStoreKey(msg lnwire.Message, peerPubKey [33]byte) ([]byte, error) {
	shortChanID, err := msgShortChanID(msg)
	if err != nil {
		return nil, err
	}

	var k [33 + 8 + 2]byte
	copy(k[:33], peerPubKey[:])
	binary.BigEndian.PutUint64(k[33:41], shortChanID.ToUint64())
	binary.BigEndian.PutUint16(k[41:43], uint16(msg.MsgType()))

	return k[:], nil
}

// AddMessage adds a message to the store for this peer.
func (s *MessageStore) AddMessage(msg lnwire.Message, peerPubKey [33]byte) error {
	// Construct the key for which we'll find this message with in the store.
	msgKey, err := messageStoreKey(msg, peerPubKey)
	if err != nil {
		return err
	}

	// Serialize the message with its wire encoding.
	var b bytes.Buffer
	if _, err := lnwire.WriteMessage(&b, msg, 0); err != nil {
		return err
	}

	return s.db.Batch(func(tx *bbolt.Tx) error {
		messageStore := tx.Bucket(messageStoreBucket)
		if messageStore == nil {
			return ErrCorruptedMessageStore
		}

		return messageStore.Put(msgKey, b.Bytes())
	})
}

// DeleteMessage deletes a message from the store for this peer.
func (s *MessageStore) DeleteMessage(msg lnwire.Message,
	peerPubKey [33]byte) error {

	// Construct the key for which we'll find this message with in the
	// store.
	msgKey, err := messageStoreKey(msg, peerPubKey)
	if err != nil {
		return err
	}

	return s.db.Batch(func(tx *bbolt.Tx) error {
		messageStore := tx.Bucket(messageStoreBucket)
		if messageStore == nil {
			return ErrCorruptedMessageStore
		}

		// If we're attempting to delete an AnnounceSignatures message
		// from the store, we'll make sure to also delete its entry with
		// the legacy key format if it exists. This acts as a NOP if it
		// doesn't exist.
		if msg, ok := msg.(*lnwire.AnnounceSignatures); ok {
			oldKey := oldMessageStoreKey(msg, peerPubKey)
			if err := messageStore.Delete(oldKey); err != nil {
				return err
			}
		}

		return messageStore.Delete(msgKey)
	})
}

// readMessage attempts to deserialize a message stored on-disk based on the
// length of it's bucket key.
func readMessage(bucketKey, msgBytes []byte) (lnwire.Message, error) {
	var (
		msg lnwire.Message
		err error
	)

	switch len(bucketKey) {
	// If the length of the bucket key matches that of the legacy format,
	// we'll parse the announce signatures message directly.
	case 33 + 8:
		msg = &lnwire.AnnounceSignatures{}
		err = msg.(*lnwire.AnnounceSignatures).Decode(
			bytes.NewReader(msgBytes), 0,
		)
	// If it matches that of the new format, we can simply use lnwire's
	// serialization.
	case 33 + 8 + 2:
		msg, err = lnwire.ReadMessage(bytes.NewReader(msgBytes), 0)
	default:
		return nil, fmt.Errorf("invalid key with length %d",
			len(bucketKey))
	}

	return msg, err
}

// Messages returns the total set of messages that exist within the store for
// all peers.
func (s *MessageStore) Messages() (map[[33]byte][]lnwire.Message, error) {
	msgs := make(map[[33]byte][]lnwire.Message)
	err := s.db.View(func(tx *bbolt.Tx) error {
		messageStore := tx.Bucket(messageStoreBucket)
		if messageStore == nil {
			return ErrCorruptedMessageStore
		}

		return messageStore.ForEach(func(k, v []byte) error {
			var pubKey [33]byte
			copy(pubKey[:], k[:33])

			msg, err := readMessage(k, v)
			if err != nil {
				return err
			}

			msgs[pubKey] = append(msgs[pubKey], msg)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	return msgs, nil
}

// MessagesForPeer returns the set of messages that exists within the store for
// the given peer.
func (s *MessageStore) MessagesForPeer(
	peerPubKey [33]byte) ([]lnwire.Message, error) {

	var msgs []lnwire.Message
	err := s.db.View(func(tx *bbolt.Tx) error {
		messageStore := tx.Bucket(messageStoreBucket)
		if messageStore == nil {
			return ErrCorruptedMessageStore
		}

		c := messageStore.Cursor()
		k, v := c.Seek(peerPubKey[:])
		for ; bytes.HasPrefix(k, peerPubKey[:]); k, v = c.Next() {
			msg, err := readMessage(k, v)
			if err != nil {
				return err
			}

			msgs = append(msgs, msg)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return msgs, nil
}
