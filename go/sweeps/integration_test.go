package sweeps

// Live-chain sweep drill (T062). Gated on a local chain:
//
//	SOVREN_LOCAL_CHAIN_RPC   http://127.0.0.1:26657   (required)
//	SOVREN_DRILL_MNEMONIC    funded test mnemonic     (required; UNSAFE_TEST_ONLY)
//	SOVREN_DRILL_GAS_PRICE   usovr per gas            (default 0.025)
//
// The drill funds a fresh customer-deposit address from the drill key,
// then drives a THRESHOLD_ONLY sweep of it back to the drill key (acting
// as the hot wallet) through the full engine: plan → prepare → broadcast →
// confirm, asserting the on-chain execution result and the deposit-record
// SWEEP_PENDING → SWEPT flip.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	sdkmath "cosmossdk.io/math"
	"github.com/stretchr/testify/require"

	"github.com/sovrn-tech/sovren-exchange-integration/go/address"
	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
	"github.com/sovrn-tech/sovren-exchange-integration/go/sequences"
	"github.com/sovrn-tech/sovren-exchange-integration/go/signer"
	"github.com/sovrn-tech/sovren-exchange-integration/go/signer/local"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage/sqlite"
	"github.com/sovrn-tech/sovren-exchange-integration/go/tx"
)

type drillEnv struct {
	client  client.Client
	store   storage.Store
	engine  *Engine
	sg      *local.Signer
	chainID string
	hot     string // drill key: funder + sweep destination
	source  string // fresh customer deposit address
}

func newDrillEnv(t *testing.T) *drillEnv {
	t.Helper()
	rpc := os.Getenv("SOVREN_LOCAL_CHAIN_RPC")
	if rpc == "" {
		t.Skip("SOVREN_LOCAL_CHAIN_RPC not set; skipping live-chain sweep drill")
	}
	mnemonic := os.Getenv("SOVREN_DRILL_MNEMONIC")
	if mnemonic == "" {
		t.Skip("SOVREN_DRILL_MNEMONIC not set; skipping live-chain sweep drill (needs a funded key)")
	}
	gasPrice := os.Getenv("SOVREN_DRILL_GAS_PRICE")
	if gasPrice == "" {
		gasPrice = "0.025"
	}

	c, err := client.NewCometRPC(rpc, client.WithTimeout(15*time.Second))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	status, err := c.NodeStatus(context.Background())
	require.NoError(t, err, "chain unreachable at %s", rpc)

	hot, err := address.DeriveAddress(mnemonic, address.DefaultHDPath)
	require.NoError(t, err)
	source, err := address.DeriveAddress(mnemonic, "m/44'/118'/0'/0/7")
	require.NoError(t, err)

	sg, err := local.New(local.Options{UnsafeTestOnly: true, NetworkType: "testnet"})
	require.NoError(t, err)
	require.NoError(t, sg.ImportKey(hot.Bech32, hot.PrivateKey))
	require.NoError(t, sg.ImportKey(source.Bech32, source.PrivateKey))

	s, err := sqlite.Open(filepath.Join(t.TempDir(), "sweep-drill.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	for _, w := range []storage.WatchedAddress{
		{ChainID: status.ChainID, Address: source.Bech32, Kind: storage.WatchCustomerDeposit, Active: true},
		{ChainID: status.ChainID, Address: hot.Bech32, Kind: storage.WatchHotWallet, Active: true},
	} {
		require.NoError(t, s.Watch().Upsert(ctx, w))
	}

	mgr := sequences.NewManager(s, c)
	engine, err := New(s, c, mgr, sg, Config{
		ChainID:                      status.ChainID,
		Strategy:                     storage.StrategyThresholdOnly,
		HotWallet:                    hot.Bech32,
		MinimumSweepAmountUsovr:      "1000",
		MaximumFeePercentageForSweep: "100",
		FeeReserveUsovr:              "0",
		GasAdjustment:                "1.5",
		GasPriceUsovr:                gasPrice,
		SimulateUnavailable:          SimulateStatic,
		StaticGasLimit:               120000,
		BroadcastTimeout:             30 * time.Second,
		Confirmations:                2,
	})
	require.NoError(t, err)

	return &drillEnv{client: c, store: s, engine: engine, sg: sg,
		chainID: status.ChainID, hot: hot.Bech32, source: source.Bech32}
}

// fundSource sends amount usovr from the drill key to the deposit address
// and waits for inclusion, returning the funding tx hash.
func (d *drillEnv) fundSource(t *testing.T, amount string) string {
	t.Helper()
	ctx := context.Background()
	accountNumber, sequence, err := d.client.Account(ctx, d.hot)
	require.NoError(t, err)

	unsigned, err := tx.BuildMsgSend(d.hot, d.source, amount, "sweep drill funding")
	require.NoError(t, err)
	pubResp, err := d.sg.GetPublicKey(ctx, signer.PublicKeyRequest{KeyRef: d.hot})
	require.NoError(t, err)
	signDocBytes, summary, err := unsigned.SignDoc(d.chainID, accountNumber, sequence,
		tx.Fee{AmountBaseUnits: "12000", GasLimit: 120000}, pubResp.PublicKeyCompressed)
	require.NoError(t, err)
	resp, err := d.sg.Sign(ctx, signer.SigningRequest{
		KeyRef:       d.hot,
		SignMode:     signer.SignModeDirect,
		SignDocBytes: signDocBytes,
		Summary:      summary,
	})
	require.NoError(t, err)
	txBytes, txHash, err := tx.Assemble(unsigned, tx.SignatureResponse{
		Signature:        resp.Signature,
		PubKeyCompressed: resp.PubKeyCompressed,
	})
	require.NoError(t, err)
	res, err := d.client.Broadcast(ctx, txBytes, client.BroadcastSync)
	require.NoError(t, err)
	require.True(t, res.Accepted, "funding rejected: code %d: %s", res.Code, res.RawLog)

	end := time.Now().Add(60 * time.Second)
	for time.Now().Before(end) {
		if info, err := d.client.Tx(ctx, txHash); err == nil && info != nil && info.Height > 0 {
			require.Equal(t, uint32(0), info.Code, "funding failed on chain: %s", info.RawLog)
			return txHash
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("funding tx %s not included within 60s", txHash)
	return ""
}

// creditDrillDeposit records the funding transfer as a CREDITED deposit so
// the drill exercises the SWEEP_PENDING → SWEPT flip.
func (d *drillEnv) creditDrillDeposit(t *testing.T, txHash, amount string) int64 {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	amt, ok := sdkmath.NewIntFromString(amount)
	require.True(t, ok)
	rec, err := d.store.Deposits().Insert(ctx, storage.DepositRecord{
		ChainID:          d.chainID,
		TxHash:           txHash,
		BlockHeight:      1,
		BlockTimestamp:   now,
		RecipientAddress: d.source,
		Denom:            storage.BaseDenom,
		AmountBaseUnits:  amt,
		Status:           storage.DepositDiscovered,
		CreatedAt:        now,
		UpdatedAt:        now,
	})
	require.NoError(t, err)
	for _, step := range []struct{ from, to storage.DepositStatus }{
		{storage.DepositDiscovered, storage.DepositValidated},
		{storage.DepositValidated, storage.DepositAwaitingConfirmations},
		{storage.DepositAwaitingConfirmations, storage.DepositCreditable},
	} {
		require.NoError(t, d.store.Deposits().UpdateStatus(ctx, rec.ID, step.from, step.to, storage.DepositUpdate{}))
	}
	require.NoError(t, d.store.Deposits().UpdateStatus(ctx, rec.ID,
		storage.DepositCreditable, storage.DepositCredited, storage.DepositUpdate{CreditedAt: &now}))
	return rec.ID
}

// TestDrillSweepLifecycle: fund → plan → prepare → broadcast → confirm
// against the local chain, asserting the on-chain execution result, the
// hot-wallet credit, and the deposit-record flip.
func TestDrillSweepLifecycle(t *testing.T) {
	d := newDrillEnv(t)
	ctx := context.Background()

	const fundAmount = "2000000"
	fundHash := d.fundSource(t, fundAmount)
	depID := d.creditDrillDeposit(t, fundHash, fundAmount)

	sourceBefore, err := d.client.Balance(ctx, d.source, storage.BaseDenom)
	require.NoError(t, err)
	require.True(t, sourceBefore.GTE(sdkmath.NewInt(2_000_000)))
	t.Logf("drill source %s balance %s usovr on %s", d.source, sourceBefore, d.chainID)

	var sweepID string
	end := time.Now().Add(180 * time.Second)
	for time.Now().Before(end) {
		report := d.engine.Pass(ctx)
		for _, err := range report.Errors {
			t.Logf("pass error: %v", err)
		}
		if sweepID == "" && len(report.Plan.JobsCreated) == 1 {
			sweepID = report.Plan.JobsCreated[0]
		}
		if sweepID != "" {
			j, err := d.store.Sweeps().Get(ctx, sweepID)
			require.NoError(t, err)
			switch j.Status {
			case storage.SweepConfirmed:
				assertDrillConfirmed(t, d, j, depID)
				return
			case storage.SweepFailed, storage.SweepCancelled, storage.SweepDeferred:
				t.Fatalf("sweep %s ended %s (code %v)", sweepID, j.Status, j.TxCode)
			}
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("sweep not CONFIRMED within 180s (job %q)", sweepID)
}

func assertDrillConfirmed(t *testing.T, d *drillEnv, j storage.SweepJob, depID int64) {
	t.Helper()
	ctx := context.Background()
	require.NotNil(t, j.TxHash)
	info, err := d.client.Tx(ctx, *j.TxHash)
	require.NoError(t, err)
	require.Equal(t, uint32(0), info.Code, "on-chain execution failed: %s", info.RawLog)

	dep, err := d.store.Deposits().GetByID(ctx, depID)
	require.NoError(t, err)
	require.Equal(t, storage.DepositSwept, dep.Status)
	require.NotNil(t, dep.SweepTxHash)
	require.Equal(t, *j.TxHash, *dep.SweepTxHash)

	sourceAfter, err := d.client.Balance(ctx, d.source, storage.BaseDenom)
	require.NoError(t, err)
	require.True(t, sourceAfter.LT(j.AmountBaseUnits),
		"source retained %s after sweeping %s", sourceAfter, j.AmountBaseUnits)
	t.Logf("sweep %s confirmed: tx %s amount %s, source residual %s",
		j.SweepID, *j.TxHash, j.AmountBaseUnits, sourceAfter)
}
