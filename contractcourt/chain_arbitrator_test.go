package contractcourt

import (
	"bytes"
	"io/ioutil"
	"math/rand"
	"net"
	"os"
	"testing"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/shachain"
)

var (
	key = [chainhash.HashSize]byte{
		0x81, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
		0x68, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
		0xd, 0xe7, 0x93, 0xe4, 0xb7, 0x25, 0xb8, 0x4d,
		0x1e, 0xb, 0x4c, 0xf9, 0x9e, 0xc5, 0x8c, 0xe9,
	}

	privKey, pubKey = btcec.PrivKeyFromBytes(btcec.S256(), key[:])
	testTx          = &wire.MsgTx{
		Version: 1,
		TxIn: []*wire.TxIn{
			{
				PreviousOutPoint: wire.OutPoint{
					Hash:  chainhash.Hash{},
					Index: 0xffffffff,
				},
				SignatureScript: []byte{0x04, 0x31, 0xdc, 0x00, 0x1b, 0x01, 0x62},
				Sequence:        0xffffffff,
			},
		},
		TxOut: []*wire.TxOut{
			{
				Value: 5000000000,
				PkScript: []byte{
					0x41, // OP_DATA_65
					0x04, 0xd6, 0x4b, 0xdf, 0xd0, 0x9e, 0xb1, 0xc5,
					0xfe, 0x29, 0x5a, 0xbd, 0xeb, 0x1d, 0xca, 0x42,
					0x81, 0xbe, 0x98, 0x8e, 0x2d, 0xa0, 0xb6, 0xc1,
					0xc6, 0xa5, 0x9d, 0xc2, 0x26, 0xc2, 0x86, 0x24,
					0xe1, 0x81, 0x75, 0xe8, 0x51, 0xc9, 0x6b, 0x97,
					0x3d, 0x81, 0xb0, 0x1c, 0xc3, 0x1f, 0x04, 0x78,
					0x34, 0xbc, 0x06, 0xd6, 0xd6, 0xed, 0xf6, 0x20,
					0xd1, 0x84, 0x24, 0x1a, 0x6a, 0xed, 0x8b, 0x63,
					0xa6, // 65-byte signature
					0xac, // OP_CHECKSIG
				},
			},
		},
		LockTime: 5,
	}
)

func createTestChannelState(cdb *channeldb.DB) (*channeldb.OpenChannel, error) {
	producer, err := shachain.NewRevocationProducerFromBytes(key[:])
	if err != nil {
		return nil, err
	}
	store := shachain.NewRevocationStore()

	localCfg := channeldb.ChannelConfig{
		ChannelConstraints: channeldb.ChannelConstraints{
			DustLimit:        btcutil.Amount(rand.Int63()),
			MaxPendingAmount: lnwire.MilliSatoshi(rand.Int63()),
			ChanReserve:      btcutil.Amount(rand.Int63()),
			MinHTLC:          lnwire.MilliSatoshi(rand.Int63()),
			MaxAcceptedHtlcs: uint16(rand.Int31()),
			CsvDelay:         uint16(rand.Int31()),
		},
		MultiSigKey: keychain.KeyDescriptor{
			PubKey: privKey.PubKey(),
		},
		RevocationBasePoint: keychain.KeyDescriptor{
			PubKey: privKey.PubKey(),
		},
		PaymentBasePoint: keychain.KeyDescriptor{
			PubKey: privKey.PubKey(),
		},
		DelayBasePoint: keychain.KeyDescriptor{
			PubKey: privKey.PubKey(),
		},
		HtlcBasePoint: keychain.KeyDescriptor{
			PubKey: privKey.PubKey(),
		},
	}
	remoteCfg := channeldb.ChannelConfig{
		ChannelConstraints: channeldb.ChannelConstraints{
			DustLimit:        btcutil.Amount(rand.Int63()),
			MaxPendingAmount: lnwire.MilliSatoshi(rand.Int63()),
			ChanReserve:      btcutil.Amount(rand.Int63()),
			MinHTLC:          lnwire.MilliSatoshi(rand.Int63()),
			MaxAcceptedHtlcs: uint16(rand.Int31()),
			CsvDelay:         uint16(rand.Int31()),
		},
		MultiSigKey: keychain.KeyDescriptor{
			PubKey: privKey.PubKey(),
			KeyLocator: keychain.KeyLocator{
				Family: keychain.KeyFamilyMultiSig,
				Index:  9,
			},
		},
		RevocationBasePoint: keychain.KeyDescriptor{
			PubKey: privKey.PubKey(),
			KeyLocator: keychain.KeyLocator{
				Family: keychain.KeyFamilyRevocationBase,
				Index:  8,
			},
		},
		PaymentBasePoint: keychain.KeyDescriptor{
			PubKey: privKey.PubKey(),
			KeyLocator: keychain.KeyLocator{
				Family: keychain.KeyFamilyPaymentBase,
				Index:  7,
			},
		},
		DelayBasePoint: keychain.KeyDescriptor{
			PubKey: privKey.PubKey(),
			KeyLocator: keychain.KeyLocator{
				Family: keychain.KeyFamilyDelayBase,
				Index:  6,
			},
		},
		HtlcBasePoint: keychain.KeyDescriptor{
			PubKey: privKey.PubKey(),
			KeyLocator: keychain.KeyLocator{
				Family: keychain.KeyFamilyHtlcBase,
				Index:  5,
			},
		},
	}

	chanID := lnwire.NewShortChanIDFromInt(uint64(rand.Int63()))

	return &channeldb.OpenChannel{
		ChanType:          channeldb.SingleFunder,
		ChainHash:         key,
		FundingOutpoint:   wire.OutPoint{Hash: key, Index: rand.Uint32()},
		ShortChannelID:    chanID,
		IsInitiator:       true,
		IsPending:         true,
		IdentityPub:       pubKey,
		Capacity:          btcutil.Amount(10000),
		LocalChanCfg:      localCfg,
		RemoteChanCfg:     remoteCfg,
		TotalMSatSent:     8,
		TotalMSatReceived: 2,
		LocalCommitment: channeldb.ChannelCommitment{
			CommitHeight:  0,
			LocalBalance:  lnwire.MilliSatoshi(9000),
			RemoteBalance: lnwire.MilliSatoshi(3000),
			CommitFee:     btcutil.Amount(rand.Int63()),
			FeePerKw:      btcutil.Amount(5000),
			CommitTx:      testTx,
			CommitSig:     bytes.Repeat([]byte{1}, 71),
		},
		RemoteCommitment: channeldb.ChannelCommitment{
			CommitHeight:  0,
			LocalBalance:  lnwire.MilliSatoshi(3000),
			RemoteBalance: lnwire.MilliSatoshi(9000),
			CommitFee:     btcutil.Amount(rand.Int63()),
			FeePerKw:      btcutil.Amount(5000),
			CommitTx:      testTx,
			CommitSig:     bytes.Repeat([]byte{1}, 71),
		},
		NumConfsRequired:        4,
		RemoteCurrentRevocation: privKey.PubKey(),
		RemoteNextRevocation:    privKey.PubKey(),
		RevocationProducer:      producer,
		RevocationStore:         store,
		Db:                      cdb,
		Packager:                channeldb.NewChannelPackager(chanID),
		FundingTxn:              testTx,
	}, nil
}

// TestChainArbitratorRepulishCommitment testst that the chain arbitrator will
// republish closing transactions for channels marked CommitementBroadcast in
// the database at startup.
func TestChainArbitratorRepublishCommitment(t *testing.T) {
	t.Parallel()

	tempPath, err := ioutil.TempDir("", "testdb")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempPath)

	db, err := channeldb.Open(tempPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Create 10 test channels and sync them to the database.
	const numChans = 10
	var channels []*channeldb.OpenChannel
	for i := 0; i < numChans; i++ {
		channel, err := createTestChannelState(db)
		if err != nil {
			t.Fatal(err)
		}

		addr := &net.TCPAddr{
			IP:   net.ParseIP("127.0.0.1"),
			Port: 18556,
		}
		if err := channel.SyncPending(addr, 101); err != nil {
			t.Fatal(err)
		}

		channels = append(channels, channel)
	}

	// Mark half of the channels as commitment broadcasted.
	for i := 0; i < numChans/2; i++ {
		closeTx := testTx.Copy()
		closeTx.TxIn[0].PreviousOutPoint = channels[i].FundingOutpoint
		err := channels[i].MarkCommitmentBroadcasted(closeTx)
		if err != nil {
			t.Fatal(err)
		}
	}

	// We keep track of the transactions published by the ChainArbitrator
	// at startup.
	published := make(map[chainhash.Hash]struct{})

	chainArbCfg := ChainArbitratorConfig{
		ChainIO:  &mockChainIO{},
		Notifier: &mockNotifier{},
		PublishTx: func(tx *wire.MsgTx) error {
			published[tx.TxHash()] = struct{}{}
			return nil
		},
	}
	chainArb := NewChainArbitrator(
		chainArbCfg, db,
	)

	if err := chainArb.Start(); err != nil {
		t.Fatal(err)
	}
	defer chainArb.Stop()

	// Half of the channels should have had their closing tx re-published.
	if len(published) != numChans/2 {
		t.Fatalf("expected %d re-published transactions, got %d",
			numChans/2, len(published))
	}

	// And make sure the published transactions are correct, and unique.
	for i := 0; i < numChans/2; i++ {
		closeTx := testTx.Copy()
		closeTx.TxIn[0].PreviousOutPoint = channels[i].FundingOutpoint

		_, ok := published[closeTx.TxHash()]
		if !ok {
			t.Fatalf("closing tx not re-published")
		}

		delete(published, closeTx.TxHash())
	}

	if len(published) != 0 {
		t.Fatalf("unexpected tx published")
	}
}
