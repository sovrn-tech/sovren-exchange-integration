package reconcile

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	sdkmath "cosmossdk.io/math"
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"

	"github.com/sovrn-tech/sovren-exchange-integration/go/address"
	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
	"github.com/sovrn-tech/sovren-exchange-integration/go/deposits"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage/sqlite"
	"github.com/sovrn-tech/sovren-exchange-integration/go/tx"
)

const (
	testChainID = "sovr-fixture-1"
	// Standard BIP39 test vector mnemonic — UNSAFE_TEST_ONLY.
	testMnemonic = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"
)

var testNow = time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)

type testAccount struct {
	Bech32  string
	PrivKey *secp256k1.PrivKey
	PubKey  []byte
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

func openTestStore(t *testing.T) storage.Store {
	t.Helper()
	s, err := sqlite.Open(filepath.Join(t.TempDir(), "reconcile-test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// seedWatch registers accounts 2 (customer deposit) and 4 (hot wallet).
func seedWatch(t *testing.T, s storage.Store) (customer, hot testAccount) {
	t.Helper()
	customer, hot = deriveAccount(t, 2), deriveAccount(t, 4)
	ctx := context.Background()
	require.NoError(t, s.Watch().Upsert(ctx, storage.WatchedAddress{
		ChainID: testChainID, Address: customer.Bech32,
		Kind: storage.WatchCustomerDeposit, Active: true,
	}))
	require.NoError(t, s.Watch().Upsert(ctx, storage.WatchedAddress{
		ChainID: testChainID, Address: hot.Bech32,
		Kind: storage.WatchHotWallet, Active: true,
	}))
	return customer, hot
}

// appendLedger inserts one TX-kind ledger row.
func appendLedger(t *testing.T, s storage.Store, addr, txHash string, msgIdx, opIdx uint32, height uint64,
	dir storage.LedgerDirection, class storage.Classification, amount int64, txCode uint32) storage.LedgerEntry {
	t.Helper()
	e, err := s.Ledger().Append(context.Background(), storage.LedgerEntry{
		ChainID: testChainID, Kind: storage.LedgerKindTx,
		TxHash: txHash, MessageIndex: msgIdx, OpIndex: opIdx,
		BlockHeight: height, Direction: dir, Address: addr,
		CounterpartySet: []string{"sovr1counterparty"},
		AmountBaseUnits: sdkmath.NewInt(amount), Denom: storage.BaseDenom,
		TxCode: txCode, Classification: class, CreatedAt: testNow,
	})
	require.NoError(t, err)
	return e
}

func appendFee(t *testing.T, s storage.Store, payer, txHash string, height uint64, fee int64, txCode uint32) {
	t.Helper()
	_, err := s.Ledger().AppendFeeOutflow(context.Background(), storage.FeeOutflow{
		ChainID: testChainID, TxHash: txHash, PayerAddress: payer,
		FeeBaseUnits: sdkmath.NewInt(fee), TxCode: txCode, BlockHeight: height,
		CreatedAt: testNow,
	})
	require.NoError(t, err)
}

// fakeChain implements ChainView with canned balances and transactions.
type fakeChain struct {
	balances map[string]sdkmath.Int
	txs      map[string]*client.TxInfo
	latest   int64
}

func newFakeChain() *fakeChain {
	return &fakeChain{balances: map[string]sdkmath.Int{}, txs: map[string]*client.TxInfo{}, latest: 100}
}

func (f *fakeChain) Balance(_ context.Context, addr, denom string) (sdkmath.Int, error) {
	if denom != storage.BaseDenom {
		return sdkmath.ZeroInt(), nil
	}
	if b, ok := f.balances[addr]; ok {
		return b, nil
	}
	return sdkmath.ZeroInt(), nil
}

func (f *fakeChain) Tx(_ context.Context, hash string) (*client.TxInfo, error) {
	if info, ok := f.txs[hash]; ok {
		return info, nil
	}
	return nil, client.ErrNotFound
}

func (f *fakeChain) LatestBlock(_ context.Context) (*client.Block, error) {
	return &client.Block{ChainID: testChainID, Height: f.latest}, nil
}

// signedSendTx builds a real single-MsgSend through the kit's tx package and
// registers it on the fake chain.
func signedSendTx(t *testing.T, f *fakeChain, from testAccount, to, amount string, seq uint64, height int64, events ...client.Event) string {
	t.Helper()
	u, err := tx.BuildMsgSend(from.Bech32, to, amount, "")
	require.NoError(t, err)
	signDoc, _, err := u.SignDoc(testChainID, 7, seq, tx.Fee{AmountBaseUnits: "500", GasLimit: 100000}, from.PubKey)
	require.NoError(t, err)
	sig, err := from.PrivKey.Sign(signDoc)
	require.NoError(t, err)
	signed, _, err := tx.Assemble(u, tx.SignatureResponse{Signature: sig, PubKeyCompressed: from.PubKey})
	require.NoError(t, err)
	hash := deposits.TxHashHex(signed)
	f.txs[hash] = &client.TxInfo{
		Hash: hash, Height: height, Code: 0, TxBytes: signed, Events: events,
	}
	return hash
}

// counterValue reads a counter without prometheus/testutil (kept out of the
// frozen go.mod).
func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	require.NoError(t, c.Write(&m))
	return m.GetCounter().GetValue()
}

// gaugeValue reads a gauge.
func gaugeValue(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	var m dto.Metric
	require.NoError(t, g.Write(&m))
	return m.GetGauge().GetValue()
}

func newTestReconciler(t *testing.T, s storage.Store, chain ChainView, opts ...Option) *Reconciler {
	t.Helper()
	opts = append(opts, WithNow(func() time.Time { return testNow }))
	r, err := New(s, chain, Config{ChainID: testChainID}, opts...)
	require.NoError(t, err)
	return r
}
