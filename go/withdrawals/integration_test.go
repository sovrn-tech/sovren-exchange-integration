package withdrawals

// Live-chain withdrawal drills (T058). Gated on a local chain:
//
//	SOVREN_LOCAL_CHAIN_RPC   http://127.0.0.1:26657   (required)
//	SOVREN_LOCAL_CHAIN_REST  http://127.0.0.1:1317    (optional, unused here)
//	SOVREN_LOCAL_CHAIN_GRPC  127.0.0.1:9090           (optional, unused here)
//	SOVREN_DRILL_MNEMONIC    funded test mnemonic     (required; UNSAFE_TEST_ONLY)
//	SOVREN_DRILL_GAS_PRICE   usovr per gas            (default 0.025)
//
// Run via exchange-kit/go/withdrawals/drill/run.sh or
// exchange-kit/examples/withdrawal-demo.sh against a local dev chain.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sovrn-tech/sovren-exchange-integration/go/address"
	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
	"github.com/sovrn-tech/sovren-exchange-integration/go/sequences"
	"github.com/sovrn-tech/sovren-exchange-integration/go/signer/local"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage/sqlite"
)

type drillEnv struct {
	client  client.Client
	store   storage.Store
	wf      *Workflow
	chainID string
	source  string
	dest    string
}

func newDrillEnv(t *testing.T) *drillEnv {
	t.Helper()
	rpc := os.Getenv("SOVREN_LOCAL_CHAIN_RPC")
	if rpc == "" {
		t.Skip("SOVREN_LOCAL_CHAIN_RPC not set; skipping live-chain withdrawal drills")
	}
	mnemonic := os.Getenv("SOVREN_DRILL_MNEMONIC")
	if mnemonic == "" {
		t.Skip("SOVREN_DRILL_MNEMONIC not set; skipping live-chain withdrawal drills (needs a funded key)")
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

	from, err := address.DeriveAddress(mnemonic, address.DefaultHDPath)
	require.NoError(t, err)
	dest, err := address.DeriveAddress(mnemonic, "m/44'/118'/0'/0/1")
	require.NoError(t, err)

	sg, err := local.New(local.Options{UnsafeTestOnly: true, NetworkType: "testnet"})
	require.NoError(t, err)
	require.NoError(t, sg.ImportKey(from.Bech32, from.PrivateKey))

	s, err := sqlite.Open(filepath.Join(t.TempDir(), "drill.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	mgr := sequences.NewManager(s, c)
	wf, err := New(s, c, mgr, sg, Config{
		ChainID:                status.ChainID,
		MinimumWithdrawalUsovr: "1",
		MaxFeeUsovr:            "500000",
		GasAdjustment:          "1.5",
		GasPriceUsovr:          gasPrice,
		SimulateUnavailable:    SimulateStatic,
		StaticGasLimit:         120000,
		BroadcastTimeout:       30 * time.Second,
		Confirmations:          2,
	})
	require.NoError(t, err)

	balance, err := c.Balance(context.Background(), from.Bech32, storage.BaseDenom)
	require.NoError(t, err)
	t.Logf("drill source %s balance %s usovr on %s", from.Bech32, balance, status.ChainID)

	return &drillEnv{client: c, store: s, wf: wf, chainID: status.ChainID, source: from.Bech32, dest: dest.Bech32}
}

// submitAndSign drives one withdrawal through SUBMIT → SIGNED and returns a
// plain error rather than failing a *testing.T directly: it is called from
// worker goroutines in TestDrillConcurrent20, and testing.T's FailNow-family
// methods (which require.NoError uses on failure) must only be called from
// the goroutine running the test function — calling them from another
// goroutine triggers runtime.Goexit() there, which a deferred recover()
// cannot catch, silently dropping the failure instead of surfacing it.
func (d *drillEnv) submitAndSign(id, idem, amount string) (storage.WithdrawalRecord, error) {
	ctx := context.Background()
	if _, err := d.wf.Submit(ctx, Request{
		WithdrawalID: id, IdempotencyKey: idem,
		SourceAddress: d.source, DestinationAddress: d.dest,
		AmountBaseUnits: amount, Memo: "drill " + id,
	}); err != nil {
		return storage.WithdrawalRecord{}, fmt.Errorf("submit %s: %w", id, err)
	}
	if err := d.wf.ValidateAddress(ctx, id); err != nil {
		return storage.WithdrawalRecord{}, fmt.Errorf("validate address %s: %w", id, err)
	}
	if err := d.wf.ApproveCompliance(ctx, id); err != nil {
		return storage.WithdrawalRecord{}, fmt.Errorf("approve compliance %s: %w", id, err)
	}
	if err := d.wf.ReserveFunds(ctx, id); err != nil {
		return storage.WithdrawalRecord{}, fmt.Errorf("reserve funds %s: %w", id, err)
	}
	if err := d.wf.ReserveSequence(ctx, id); err != nil {
		return storage.WithdrawalRecord{}, fmt.Errorf("reserve sequence %s: %w", id, err)
	}
	if err := d.wf.Build(ctx, id); err != nil {
		return storage.WithdrawalRecord{}, fmt.Errorf("build %s: %w", id, err)
	}
	if err := d.wf.Simulate(ctx, id); err != nil {
		return storage.WithdrawalRecord{}, fmt.Errorf("simulate %s: %w", id, err)
	}
	if err := d.wf.Sign(ctx, id); err != nil {
		return storage.WithdrawalRecord{}, fmt.Errorf("sign %s: %w", id, err)
	}
	rec, err := d.store.Withdrawals().Get(ctx, id)
	if err != nil {
		return storage.WithdrawalRecord{}, fmt.Errorf("get %s: %w", id, err)
	}
	return rec, nil
}

func (d *drillEnv) confirmWithin(t *testing.T, id string, deadline time.Duration) {
	t.Helper()
	ctx := context.Background()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		rec, err := d.store.Withdrawals().Get(ctx, id)
		require.NoError(t, err)
		switch rec.Status {
		case storage.WithdrawalConfirmed:
			return
		case storage.WithdrawalFailed, storage.WithdrawalReviewRequired:
			t.Fatalf("withdrawal %s ended %s: code=%v log=%s", id, rec.Status, rec.TxCode, rec.RawLog)
		case storage.WithdrawalBroadcast, storage.WithdrawalIncluded:
			if _, err := d.wf.Confirm(ctx, id); err != nil {
				t.Logf("confirm %s: %v", id, err)
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("withdrawal %s not CONFIRMED within %s", id, deadline)
}

// TestDrillWithdrawalLifecycle: build → sign (unsafe-local) → broadcast →
// confirm against the local chain, asserting the on-chain execution result.
func TestDrillWithdrawalLifecycle(t *testing.T) {
	d := newDrillEnv(t)
	ctx := context.Background()
	id := fmt.Sprintf("drill-%d", time.Now().UnixNano())

	rec, err := d.submitAndSign(id, "idem-"+id, "1000000")
	require.NoError(t, err)
	require.Equal(t, storage.WithdrawalSigned, rec.Status)

	outcome, err := d.wf.Broadcast(ctx, id)
	require.NoError(t, err)
	require.Equal(t, OutcomeMempoolAccepted, outcome)
	d.confirmWithin(t, id, 90*time.Second)

	final, err := d.store.Withdrawals().Get(ctx, id)
	require.NoError(t, err)
	info, err := d.client.Tx(ctx, *final.TxHash)
	require.NoError(t, err)
	require.Equal(t, uint32(0), info.Code, "on-chain execution failed: %s", info.RawLog)
}

// TestDrillDuplicateSubmit: the same idempotency key twice yields ONE
// on-chain transaction (FR-033 against a live chain).
func TestDrillDuplicateSubmit(t *testing.T) {
	d := newDrillEnv(t)
	ctx := context.Background()
	id := fmt.Sprintf("drill-dup-%d", time.Now().UnixNano())
	idem := "idem-" + id

	rec, err := d.submitAndSign(id, idem, "1000000")
	require.NoError(t, err)

	dup, err := d.wf.Submit(ctx, Request{
		WithdrawalID: id + "-second", IdempotencyKey: idem,
		SourceAddress: d.source, DestinationAddress: d.dest,
		AmountBaseUnits: "1000000", Memo: "duplicate attempt",
	})
	require.NoError(t, err)
	require.Equal(t, id, dup.WithdrawalID)
	_, err = d.store.Withdrawals().Get(ctx, id+"-second")
	require.ErrorIs(t, err, storage.ErrNotFound)

	_, err = d.wf.Broadcast(ctx, id)
	require.NoError(t, err)
	d.confirmWithin(t, id, 90*time.Second)
	require.NotNil(t, rec.TxHash)
}

// TestDrillConcurrent20: twenty withdrawals from one hot wallet — signed
// concurrently (sequence manager serializes), broadcast in sequence order,
// all confirmed with twenty distinct sequences.
func TestDrillConcurrent20(t *testing.T) {
	d := newDrillEnv(t)
	ctx := context.Background()
	const n = 20
	base := fmt.Sprintf("drill-c20-%d", time.Now().UnixNano())

	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					errCh <- fmt.Errorf("worker %d: %v", i, r)
				}
			}()
			id := fmt.Sprintf("%s-%02d", base, i)
			if _, err := d.submitAndSign(id, "idem-"+id, "1000000"); err != nil {
				errCh <- fmt.Errorf("worker %d: %w", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}

	// Broadcast in ascending sequence order (CheckTx enforces per-account
	// sequence contiguity in the mempool).
	type pair struct {
		id  string
		seq uint64
	}
	var pairs []pair
	seen := map[uint64]bool{}
	for i := range n {
		id := fmt.Sprintf("%s-%02d", base, i)
		rec, err := d.store.Withdrawals().Get(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, rec.Sequence)
		require.False(t, seen[*rec.Sequence], "sequence %d bound twice", *rec.Sequence)
		seen[*rec.Sequence] = true
		pairs = append(pairs, pair{id: id, seq: *rec.Sequence})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].seq < pairs[j].seq })
	for _, p := range pairs {
		_, err := d.wf.Broadcast(ctx, p.id)
		require.NoError(t, err, "broadcast %s (seq %d)", p.id, p.seq)
	}
	for _, p := range pairs {
		d.confirmWithin(t, p.id, 180*time.Second)
	}
}
