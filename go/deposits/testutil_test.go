package deposits

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	txtypes "github.com/cosmos/cosmos-sdk/types/tx"
	"github.com/cosmos/gogoproto/proto"
	"github.com/stretchr/testify/require"

	"github.com/sovrn-tech/sovren-exchange-integration/go/address"
	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage/sqlite"
	"github.com/sovrn-tech/sovren-exchange-integration/go/tx"
)

const (
	testChainID = "sovr-fixture-1"
	// Standard BIP39 test vector mnemonic — UNSAFE_TEST_ONLY.
	testMnemonic = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"
)

var testBlockTime = time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)

type testAccount struct {
	Bech32  string
	PrivKey *secp256k1.PrivKey
	PubKey  []byte
}

func privKeyFromBytes(secret []byte) *secp256k1.PrivKey {
	key := make([]byte, len(secret))
	copy(key, secret)
	return &secp256k1.PrivKey{Key: key}
}

func deriveAccount(t *testing.T, index int) testAccount {
	t.Helper()
	a, err := address.DeriveAddress(testMnemonic, fmt.Sprintf("m/44'/118'/0'/0/%d", index))
	require.NoError(t, err)
	return testAccount{
		Bech32:  a.Bech32,
		PrivKey: &secp256k1.PrivKey{Key: a.PrivateKey},
		PubKey:  a.PublicKeyCompressed,
	}
}

// testAccounts: 0,1 external; 2 customer deposit; 3 omnibus (memo required);
// 4 hot wallet; 5 fee wallet.
func testWatchSet(t *testing.T) (WatchSet, []testAccount) {
	t.Helper()
	accts := make([]testAccount, 6)
	for i := range accts {
		accts[i] = deriveAccount(t, i)
	}
	ws := NewWatchSet(testWatchedAddresses(accts))
	return ws, accts
}

func testWatchedAddresses(accts []testAccount) []storage.WatchedAddress {
	return []storage.WatchedAddress{
		{ChainID: testChainID, Address: accts[2].Bech32, Kind: storage.WatchCustomerDeposit, Active: true},
		{ChainID: testChainID, Address: accts[3].Bech32, Kind: storage.WatchOmnibus, MemoRequired: true, Active: true},
		{ChainID: testChainID, Address: accts[4].Bech32, Kind: storage.WatchHotWallet, Active: true},
		{ChainID: testChainID, Address: accts[5].Bech32, Kind: storage.WatchFeeWallet, Active: true},
	}
}

// rawTx assembles TxRaw bytes directly with gogoproto — the adversarial /
// multi-message shapes the tx package deliberately refuses to build.
func rawTx(t *testing.T, msgs []proto.Message, memo string, fee *txtypes.Fee) []byte {
	t.Helper()
	anys := make([]*codectypes.Any, 0, len(msgs))
	for _, m := range msgs {
		a, err := codectypes.NewAnyWithValue(m)
		require.NoError(t, err)
		anys = append(anys, a)
	}
	return rawTxFromAnys(t, anys, memo, fee)
}

func rawTxFromAnys(t *testing.T, anys []*codectypes.Any, memo string, fee *txtypes.Fee) []byte {
	t.Helper()
	bodyBytes, err := proto.Marshal(&txtypes.TxBody{Messages: anys, Memo: memo})
	require.NoError(t, err)
	authBytes, err := proto.Marshal(&txtypes.AuthInfo{Fee: fee})
	require.NoError(t, err)
	rawBytes, err := proto.Marshal(&txtypes.TxRaw{
		BodyBytes:     bodyBytes,
		AuthInfoBytes: authBytes,
		Signatures:    [][]byte{{0x01}},
	})
	require.NoError(t, err)
	return rawBytes
}

// signedSendTx builds a real signed single-MsgSend via the kit's own tx
// package (BuildMsgSend → SignDoc → Assemble).
func signedSendTx(t *testing.T, from testAccount, to, amount, memo, feeAmount string, gas uint64) []byte {
	t.Helper()
	u, err := tx.BuildMsgSend(from.Bech32, to, amount, memo)
	require.NoError(t, err)
	signDoc, _, err := u.SignDoc(testChainID, 7, 0, tx.Fee{AmountBaseUnits: feeAmount, GasLimit: gas}, from.PubKey)
	require.NoError(t, err)
	sig, err := from.PrivKey.Sign(signDoc)
	require.NoError(t, err)
	signed, _, err := tx.Assemble(u, tx.SignatureResponse{Signature: sig, PubKeyCompressed: from.PubKey})
	require.NoError(t, err)
	return signed
}

func feeEvent(fee, payer string) client.Event {
	return client.Event{Type: eventTypeTx, Attributes: []client.EventAttribute{
		{Key: attrFee, Value: fee},
		{Key: attrFeePayer, Value: payer},
	}}
}

func okResult(events ...client.Event) client.TxExecResult {
	return client.TxExecResult{Code: 0, Events: events}
}

func makeBlock(height int64, hash, lastHash byte, txs ...[]byte) *client.Block {
	return &client.Block{
		ChainID:       testChainID,
		Height:        height,
		Hash:          []byte{hash},
		LastBlockHash: []byte{lastHash},
		Time:          testBlockTime,
		Txs:           txs,
	}
}

func makeResults(height int64, results ...client.TxExecResult) *client.BlockResults {
	return &client.BlockResults{Height: height, TxResults: results}
}

func openTestStore(t *testing.T) storage.Store {
	t.Helper()
	s, err := sqlite.Open(filepath.Join(t.TempDir(), "deposits-test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}
