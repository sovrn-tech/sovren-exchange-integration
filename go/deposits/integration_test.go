package deposits

// Live-chain integration drills (T049). Gated behind SOVREN_LOCAL_CHAIN_RPC:
// a local chain may not be running while unit suites execute. Orchestrated
// end-to-end by deposits/drill/run.sh against the compose chain.
//
// Environment:
//   SOVREN_LOCAL_CHAIN_RPC       CometBFT RPC URL (e.g. http://localhost:26657) — required
//   SOVREN_LOCAL_CHAIN_MNEMONIC  funded account mnemonic (m/44'/118'/0'/0/0)   — required
//   SOVREN_LOCAL_CHAIN_REST     \ accepted for parity with other kit drills;
//   SOVREN_LOCAL_CHAIN_GRPC    /  unused here (the scanner is RPC-only)

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sovrn-tech/sovren-exchange-integration/go/address"
	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage/sqlite"
	"github.com/sovrn-tech/sovren-exchange-integration/go/tx"
)

type liveEnv struct {
	client  client.Client
	chainID string
	funded  testAccount
}

func liveChain(t *testing.T) *liveEnv {
	t.Helper()
	rpcURL := os.Getenv("SOVREN_LOCAL_CHAIN_RPC")
	if rpcURL == "" {
		t.Skip("SOVREN_LOCAL_CHAIN_RPC not set; skipping live-chain integration drills")
	}
	mnemonic := os.Getenv("SOVREN_LOCAL_CHAIN_MNEMONIC")
	if mnemonic == "" {
		t.Skip("SOVREN_LOCAL_CHAIN_MNEMONIC not set; skipping live-chain integration drills")
	}
	c, err := client.NewCometRPC(rpcURL, client.WithTimeout(10*time.Second))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	status, err := c.NodeStatus(ctx)
	require.NoError(t, err, "local chain unreachable at %s", rpcURL)

	derived, err := address.DeriveAddress(mnemonic, "m/44'/118'/0'/0/0")
	require.NoError(t, err)
	funded := testAccount{Bech32: derived.Bech32, PubKey: derived.PublicKeyCompressed}
	funded.PrivKey = privKeyFromBytes(derived.PrivateKey)
	return &liveEnv{client: c, chainID: status.ChainID, funded: funded}
}

// sendLive signs and broadcasts one MsgSend from the funded account and
// waits for inclusion, returning the tx hash and inclusion height.
func (e *liveEnv) sendLive(t *testing.T, to, amount, memo string) (string, uint64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	accNum, seq, err := e.client.Account(ctx, e.funded.Bech32)
	require.NoError(t, err, "funded account %s not found on chain", e.funded.Bech32)

	u, err := tx.BuildMsgSend(e.funded.Bech32, to, amount, memo)
	require.NoError(t, err)
	signDoc, _, err := u.SignDoc(e.chainID, accNum, seq, tx.Fee{AmountBaseUnits: "5000", GasLimit: 200000}, e.funded.PubKey)
	require.NoError(t, err)
	sig, err := e.funded.PrivKey.Sign(signDoc)
	require.NoError(t, err)
	signed, hash, err := tx.Assemble(u, tx.SignatureResponse{Signature: sig, PubKeyCompressed: e.funded.PubKey})
	require.NoError(t, err)

	res, err := e.client.Broadcast(ctx, signed, client.BroadcastSync)
	require.NoError(t, err)
	require.True(t, res.Accepted, "CheckTx rejected: %s", res.RawLog)

	for {
		info, err := e.client.Tx(ctx, hash)
		if err == nil {
			require.Equal(t, uint32(0), info.Code, "tx failed on chain: %s", info.RawLog)
			return hash, uint64(info.Height)
		}
		if !errors.Is(err, client.ErrNotFound) {
			require.NoError(t, err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("tx %s not included before timeout", hash)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (e *liveEnv) newStore(t *testing.T, path string) storage.Store {
	t.Helper()
	s, err := sqlite.Open(path)
	require.NoError(t, err)
	return s
}

func (e *liveEnv) seedWatch(t *testing.T, s storage.Store, addr string) {
	t.Helper()
	require.NoError(t, s.Watch().Upsert(context.Background(), storage.WatchedAddress{
		ChainID: e.chainID,
		Address: addr,
		Kind:    storage.WatchCustomerDeposit,
		Active:  true,
	}))
}

func (e *liveEnv) scannerAt(t *testing.T, s storage.Store, start uint64) *Scanner {
	t.Helper()
	sc, err := NewScanner(e.client, s, ScannerConfig{
		ChainID:       e.chainID,
		Confirmations: 1,
		StartHeight:   start,
		PollInterval:  300 * time.Millisecond,
	})
	require.NoError(t, err)
	return sc
}

func waitCredited(t *testing.T, s storage.Store, chainID, txHash, recipient string, cycle func() error) storage.DepositRecord {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		require.NoError(t, cycle())
		d, err := s.Deposits().Get(context.Background(), chainID, txHash, 0, 0, recipient)
		if err == nil && d.Status == storage.DepositCredited {
			return d
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("deposit %s never credited", txHash)
	return storage.DepositRecord{}
}

// TestIntegrationDepositEndToEnd funds a fresh watched address on the live
// chain and drives the scanner to CREDITED.
func TestIntegrationDepositEndToEnd(t *testing.T) {
	env := liveChain(t)
	ctx := context.Background()

	depositAddr, err := address.DeriveAddress(testMnemonic, "m/44'/118'/0'/0/90")
	require.NoError(t, err)

	latest, err := env.client.LatestBlock(ctx)
	require.NoError(t, err)
	startHeight := uint64(latest.Height)

	txHash, height := env.sendLive(t, depositAddr.Bech32, "1234567", "drill-e2e")

	store := env.newStore(t, filepath.Join(t.TempDir(), "drill-e2e.db"))
	defer store.Close()
	env.seedWatch(t, store, depositAddr.Bech32)
	sc := env.scannerAt(t, store, startHeight)

	d := waitCredited(t, store, env.chainID, txHash, depositAddr.Bech32, func() error { return sc.Cycle(ctx) })
	require.Equal(t, "1234567", d.AmountBaseUnits.String())
	require.Equal(t, height, d.BlockHeight)
	require.Equal(t, "drill-e2e", d.Memo)

	// Ledger row exists and classifies external.
	entry, err := store.Ledger().GetTxEntry(ctx, env.chainID, txHash, 0, 0)
	require.NoError(t, err)
	require.Equal(t, storage.ClassExternalDeposit, entry.Classification)

	// Exactly one outbox event.
	pending, err := store.Outbox().ListPending(ctx, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
}

// TestIntegrationScannerKillRestartMidRange stops the scanner mid-range and
// recreates it over the same store: no loss, no duplication (SC-004).
func TestIntegrationScannerKillRestartMidRange(t *testing.T) {
	env := liveChain(t)
	ctx := context.Background()

	depositAddr, err := address.DeriveAddress(testMnemonic, "m/44'/118'/0'/0/91")
	require.NoError(t, err)

	latest, err := env.client.LatestBlock(ctx)
	require.NoError(t, err)
	startHeight := uint64(latest.Height)

	hash1, _ := env.sendLive(t, depositAddr.Bech32, "111111", "drill-restart-1")

	dbPath := filepath.Join(t.TempDir(), "drill-restart.db")
	store := env.newStore(t, dbPath)
	env.seedWatch(t, store, depositAddr.Bech32)

	// First scanner: one cycle only, then "killed" (dropped).
	sc1 := env.scannerAt(t, store, startHeight)
	require.NoError(t, sc1.Cycle(ctx))
	cp, err := store.Checkpoints().Get(ctx, env.chainID)
	require.NoError(t, err)
	require.GreaterOrEqual(t, cp.LastFullyProcessedHeight, startHeight)

	// Second deposit lands while no scanner is running.
	hash2, _ := env.sendLive(t, depositAddr.Bech32, "222222", "drill-restart-2")

	// New scanner over the SAME store resumes from the checkpoint.
	sc2 := env.scannerAt(t, store, 0)
	d1 := waitCredited(t, store, env.chainID, hash1, depositAddr.Bech32, func() error { return sc2.Cycle(ctx) })
	d2 := waitCredited(t, store, env.chainID, hash2, depositAddr.Bech32, func() error { return sc2.Cycle(ctx) })
	require.Equal(t, "111111", d1.AmountBaseUnits.String())
	require.Equal(t, "222222", d2.AmountBaseUnits.String())

	pending, err := store.Outbox().ListPending(ctx, 10)
	require.NoError(t, err)
	require.Len(t, pending, 2, "exactly one credit event per deposit")
	store.Close()
}

// TestIntegrationRangeReplayIdempotent re-scans an already-processed range;
// unique keys make the replay a no-op (FR-024/FR-026).
func TestIntegrationRangeReplayIdempotent(t *testing.T) {
	env := liveChain(t)
	ctx := context.Background()

	depositAddr, err := address.DeriveAddress(testMnemonic, "m/44'/118'/0'/0/92")
	require.NoError(t, err)

	latest, err := env.client.LatestBlock(ctx)
	require.NoError(t, err)
	startHeight := uint64(latest.Height)

	txHash, height := env.sendLive(t, depositAddr.Bech32, "333333", "drill-replay")

	store := env.newStore(t, filepath.Join(t.TempDir(), "drill-replay.db"))
	defer store.Close()
	env.seedWatch(t, store, depositAddr.Bech32)
	sc := env.scannerAt(t, store, startHeight)
	waitCredited(t, store, env.chainID, txHash, depositAddr.Bech32, func() error { return sc.Cycle(ctx) })

	// Replay the full range twice via the controls-backed rescan path.
	for i := 0; i < 2; i++ {
		require.NoError(t, sc.RescanFrom(ctx, height, "drill", fmt.Sprintf("replay %d", i)))
		require.NoError(t, sc.Cycle(ctx))
	}

	d, err := store.Deposits().Get(ctx, env.chainID, txHash, 0, 0, depositAddr.Bech32)
	require.NoError(t, err)
	require.Equal(t, storage.DepositCredited, d.Status)
	pending, err := store.Outbox().ListPending(ctx, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1, "replay must not re-credit")
}

// TestIntegrationDBOutageRecovery closes the store under the scanner
// (cycle errors, no crash) and recreates scanner+store over the same file:
// durable state carries the pipeline to CREDITED.
func TestIntegrationDBOutageRecovery(t *testing.T) {
	env := liveChain(t)
	ctx := context.Background()

	depositAddr, err := address.DeriveAddress(testMnemonic, "m/44'/118'/0'/0/93")
	require.NoError(t, err)

	latest, err := env.client.LatestBlock(ctx)
	require.NoError(t, err)
	startHeight := uint64(latest.Height)

	txHash, _ := env.sendLive(t, depositAddr.Bech32, "444444", "drill-db-outage")

	dbPath := filepath.Join(t.TempDir(), "drill-db.db")
	store := env.newStore(t, dbPath)
	env.seedWatch(t, store, depositAddr.Bech32)
	sc := env.scannerAt(t, store, startHeight)
	require.NoError(t, sc.Cycle(ctx))

	// Outage: close the store; the next cycle errors but must not panic.
	require.NoError(t, store.Close())
	require.Error(t, sc.Cycle(ctx))

	// Recovery: reopen the same database file with a fresh scanner.
	store2 := env.newStore(t, dbPath)
	defer store2.Close()
	sc2 := env.scannerAt(t, store2, 0)
	d := waitCredited(t, store2, env.chainID, txHash, depositAddr.Bech32, func() error { return sc2.Cycle(ctx) })
	require.Equal(t, "444444", d.AmountBaseUnits.String())

	pending, err := store2.Outbox().ListPending(ctx, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
}
