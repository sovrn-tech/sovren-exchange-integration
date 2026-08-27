package deposits

import (
	"context"
	"fmt"
	"sync"
	"testing"

	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/cosmos/gogoproto/proto"
	"github.com/stretchr/testify/require"

	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

// fakeChain serves LatestBlock/BlockByHeight/BlockResults from an in-memory
// chain; every other client.Client method panics (unused by the scanner).
type fakeChain struct {
	client.Client
	mu      sync.Mutex
	chainID string
	blocks  map[int64]*client.Block
	results map[int64]*client.BlockResults
	tip     int64
}

func newFakeChain() *fakeChain {
	return &fakeChain{
		chainID: testChainID,
		blocks:  map[int64]*client.Block{},
		results: map[int64]*client.BlockResults{},
	}
}

// addBlock appends the next block, hash-chained to its parent.
func (f *fakeChain) addBlock(txs [][]byte, results []client.TxExecResult) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	h := f.tip + 1
	var parentHash []byte
	if parent, ok := f.blocks[f.tip]; ok {
		parentHash = parent.Hash
	}
	f.blocks[h] = &client.Block{
		ChainID:       f.chainID,
		Height:        h,
		Hash:          []byte{byte(h), 0xEE},
		LastBlockHash: parentHash,
		Time:          testBlockTime,
		Txs:           txs,
	}
	f.results[h] = &client.BlockResults{Height: h, TxResults: results}
	f.tip = h
	return h
}

// rewriteBlockHash replaces one block's hash; successors keep chaining to
// the NEW hash (rollback simulation — the stored checkpoint goes stale).
func (f *fakeChain) rewriteBlockHash(height int64, hash []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blocks[height].Hash = hash
}

func (f *fakeChain) LatestBlock(context.Context) (*client.Block, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tip == 0 {
		return nil, fmt.Errorf("no blocks")
	}
	return f.blocks[f.tip], nil
}

func (f *fakeChain) BlockByHeight(_ context.Context, height int64) (*client.Block, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.blocks[height]
	if !ok {
		return nil, fmt.Errorf("no block at height %d", height)
	}
	return b, nil
}

func (f *fakeChain) BlockResults(_ context.Context, height int64) (*client.BlockResults, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.results[height]
	if !ok {
		return nil, fmt.Errorf("no results at height %d", height)
	}
	return r, nil
}

func seedWatch(t *testing.T, store storage.Store, accts []testAccount) {
	t.Helper()
	for _, w := range testWatchedAddresses(accts) {
		require.NoError(t, store.Watch().Upsert(context.Background(), w))
	}
}

func depositTx(t *testing.T, accts []testAccount, from, to int, amount string) [][]byte {
	t.Helper()
	return [][]byte{rawTx(t, []proto.Message{
		&banktypes.MsgSend{FromAddress: accts[from].Bech32, ToAddress: accts[to].Bech32, Amount: coins(amount + "usovr")},
	}, "", nil)}
}

func okResults(n int) []client.TxExecResult {
	out := make([]client.TxExecResult, n)
	return out
}

func TestScannerEndToEndCredit(t *testing.T) {
	store := openTestStore(t)
	_, accts := testWatchSet(t)
	seedWatch(t, store, accts)
	chain := newFakeChain()
	chain.addBlock(nil, nil) // genesis-ish block 1

	sc, err := NewScanner(chain, store, ScannerConfig{ChainID: testChainID, Confirmations: 2, StartHeight: 1})
	require.NoError(t, err)
	ctx := context.Background()

	txs := depositTx(t, accts, 0, 2, "1000000")
	chain.addBlock(txs, okResults(1)) // height 2
	require.NoError(t, sc.Cycle(ctx))

	// 1 confirmation so far — awaiting.
	d, err := store.Deposits().Get(ctx, testChainID, TxHashHex(txs[0]), 0, 0, accts[2].Bech32)
	require.NoError(t, err)
	require.Equal(t, storage.DepositAwaitingConfirmations, d.Status)

	chain.addBlock(nil, nil) // height 3 ⇒ 2 confirmations
	require.NoError(t, sc.Cycle(ctx))
	d, err = store.Deposits().GetByID(ctx, d.ID)
	require.NoError(t, err)
	require.Equal(t, storage.DepositCredited, d.Status)

	pending, err := store.Outbox().ListPending(ctx, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
}

func TestScannerRestartResumesFromCheckpointNoDupNoLoss(t *testing.T) {
	store := openTestStore(t)
	_, accts := testWatchSet(t)
	seedWatch(t, store, accts)
	chain := newFakeChain()
	chain.addBlock(nil, nil)
	tx1 := depositTx(t, accts, 0, 2, "111111")
	chain.addBlock(tx1, okResults(1))

	sc, err := NewScanner(chain, store, ScannerConfig{ChainID: testChainID, Confirmations: 1, StartHeight: 1})
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, sc.Cycle(ctx))

	cp, err := store.Checkpoints().Get(ctx, testChainID)
	require.NoError(t, err)
	require.Equal(t, uint64(2), cp.LastFullyProcessedHeight)

	// "Kill" the scanner; extend the chain; a NEW scanner over the same
	// store resumes from the checkpoint: the old deposit is not duplicated,
	// the new one is found.
	tx2 := depositTx(t, accts, 1, 2, "222222")
	chain.addBlock(tx2, okResults(1))
	sc2, err := NewScanner(chain, store, ScannerConfig{ChainID: testChainID, Confirmations: 1})
	require.NoError(t, err)
	require.NoError(t, sc2.Cycle(ctx))

	d1, err := store.Deposits().Get(ctx, testChainID, TxHashHex(tx1[0]), 0, 0, accts[2].Bech32)
	require.NoError(t, err)
	require.Equal(t, storage.DepositCredited, d1.Status)
	d2, err := store.Deposits().Get(ctx, testChainID, TxHashHex(tx2[0]), 0, 0, accts[2].Bech32)
	require.NoError(t, err)
	require.Equal(t, storage.DepositCredited, d2.Status)

	pending, err := store.Outbox().ListPending(ctx, 10)
	require.NoError(t, err)
	require.Len(t, pending, 2)
}

func TestScannerRangeReplayIdempotent(t *testing.T) {
	store := openTestStore(t)
	_, accts := testWatchSet(t)
	seedWatch(t, store, accts)
	chain := newFakeChain()
	chain.addBlock(nil, nil)
	txs := depositTx(t, accts, 0, 2, "999999")
	chain.addBlock(txs, okResults(1))
	chain.addBlock(nil, nil)

	sc, err := NewScanner(chain, store, ScannerConfig{ChainID: testChainID, Confirmations: 1, StartHeight: 1})
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, sc.Cycle(ctx))

	// Rescan the full range via the controls-backed resume-from path.
	require.NoError(t, sc.RescanFrom(ctx, 1, "test", "range replay"))
	require.NoError(t, sc.Cycle(ctx))

	// Still exactly one deposit, one credit event.
	d, err := store.Deposits().Get(ctx, testChainID, TxHashHex(txs[0]), 0, 0, accts[2].Bech32)
	require.NoError(t, err)
	require.Equal(t, storage.DepositCredited, d.Status)
	pending, err := store.Outbox().ListPending(ctx, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)

	// resume_from_height is cleared after application.
	controls, err := store.Controls().Get(ctx, testChainID)
	require.NoError(t, err)
	require.Nil(t, controls.ResumeFromHeight)
}

func TestScannerHashChainBreakOrphansAndOpensChainReview(t *testing.T) {
	store := openTestStore(t)
	_, accts := testWatchSet(t)
	seedWatch(t, store, accts)
	chain := newFakeChain()
	chain.addBlock(nil, nil)
	txs := depositTx(t, accts, 0, 2, "555555")
	chain.addBlock(txs, okResults(1)) // height 2

	sc, err := NewScanner(chain, store, ScannerConfig{ChainID: testChainID, Confirmations: 10, StartHeight: 1})
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, sc.Cycle(ctx))

	d, err := store.Deposits().Get(ctx, testChainID, TxHashHex(txs[0]), 0, 0, accts[2].Bech32)
	require.NoError(t, err)
	require.Equal(t, storage.DepositAwaitingConfirmations, d.Status)

	// Simulate node rollback/resync: block 2 is replaced, so block 3's
	// parent hash no longer matches the stored checkpoint hash.
	chain.rewriteBlockHash(2, []byte{0xFF, 0xFF})
	chain.addBlock(nil, nil) // height 3, chains to the REPLACED block 2

	require.NoError(t, sc.Cycle(ctx))

	// Deposit from the replaced block is ORPHANED; chain review is open;
	// checkpoint rolled back one block.
	d, err = store.Deposits().GetByID(ctx, d.ID)
	require.NoError(t, err)
	require.Equal(t, storage.DepositOrphaned, d.Status)
	open, err := store.ChainReview().ListOpen(ctx, testChainID)
	require.NoError(t, err)
	require.Len(t, open, 1)
	require.Equal(t, storage.TriggerBlockHashMismatch, open[0].Trigger)
	cp, err := store.Checkpoints().Get(ctx, testChainID)
	require.NoError(t, err)
	require.Equal(t, uint64(1), cp.LastFullyProcessedHeight)

	// Next cycle re-scans the replaced block: the ORPHANED deposit is
	// re-evaluated (revived), but crediting stays gated by the open
	// chain-review condition (FR-023 / FR-044).
	require.NoError(t, sc.Cycle(ctx))
	d, err = store.Deposits().GetByID(ctx, d.ID)
	require.NoError(t, err)
	require.Equal(t, storage.DepositAwaitingConfirmations, d.Status)
}

func TestScannerScanWithoutCreditSuspendsAndResumes(t *testing.T) {
	store := openTestStore(t)
	_, accts := testWatchSet(t)
	seedWatch(t, store, accts)
	ctx := context.Background()
	on := true
	_, err := store.Controls().Apply(ctx, testChainID,
		storage.ControlsUpdate{ScanWithoutCredit: &on}, "test", "drill")
	require.NoError(t, err)

	chain := newFakeChain()
	chain.addBlock(nil, nil)
	txs := depositTx(t, accts, 0, 2, "777777")
	chain.addBlock(txs, okResults(1))

	sc, err := NewScanner(chain, store, ScannerConfig{ChainID: testChainID, Confirmations: 1, StartHeight: 1})
	require.NoError(t, err)
	require.NoError(t, sc.Cycle(ctx))
	// Scanning continued; crediting parked as SUSPENDED.
	d, err := store.Deposits().Get(ctx, testChainID, TxHashHex(txs[0]), 0, 0, accts[2].Bech32)
	require.NoError(t, err)
	require.Equal(t, storage.DepositSuspended, d.Status)
	require.NotNil(t, d.PriorStatus)
	require.Equal(t, storage.DepositCreditable, *d.PriorStatus)

	// Control released ⇒ resumes to prior status and credits.
	off := false
	_, err = store.Controls().Apply(ctx, testChainID,
		storage.ControlsUpdate{ScanWithoutCredit: &off}, "test", "drill over")
	require.NoError(t, err)
	require.NoError(t, sc.Cycle(ctx))
	d, err = store.Deposits().GetByID(ctx, d.ID)
	require.NoError(t, err)
	require.Equal(t, storage.DepositCredited, d.Status)
}

// gateFlipStore flips the credit gate exactly once, right after the first
// credit commits — deterministically reproducing an admin pause that lands
// mid-batch (after processPending's single gate load, between two per-record
// credit txs). Only CreditDeposit routes through this WithTx; processPending's
// status-advance writes run on the embedded store's own WithTx, so a first
// commit here is a credit commit.
type gateFlipStore struct {
	storage.Store
	flip    func()
	commits int
	fired   bool
}

func (g *gateFlipStore) WithTx(ctx context.Context, fn func(ctx context.Context, s storage.Store) error) error {
	err := g.Store.WithTx(ctx, fn)
	if err == nil {
		g.commits++
		if !g.fired {
			g.fired = true
			g.flip()
		}
	}
	return err
}

// A credit pause (or an opened chain-review) that lands mid-batch — after the
// cycle's gate load but before a later record's credit tx — must not let the
// rest of the batch credit. CreditDeposit re-checks the gate inside its tx and
// aborts; the scanner stops the credit pass, leaving the not-yet-credited
// records CREDITABLE for a later cycle (fund-safety TOCTOU, PR #300 review).
func TestScannerCreditGatePausedMidBatchLeavesRestCreditable(t *testing.T) {
	underlying := openTestStore(t)
	ws, accts := testWatchSet(t)
	ctx := context.Background()

	// Three CREDITABLE deposits (external sender → customer address), inserted
	// in ascending id so the credit loop processes them 100000 → 200000 →
	// 300000.
	pol := RecordPolicy{ChainID: testChainID}
	var ids []int64
	for i, amt := range []string{"100000", "200000", "300000"} {
		bp, txHash := externalDepositBlock(t, ws, accts, int64(80+i), amt)
		out, err := RecordBlock(ctx, underlying, bp, pol, testBlockTime)
		require.NoError(t, err)
		require.Equal(t, 1, out.DepositsInserted)
		d, err := underlying.Deposits().Get(ctx, testChainID, txHash, 0, 0, accts[2].Bech32)
		require.NoError(t, err)
		require.NoError(t, underlying.Deposits().UpdateStatus(ctx, d.ID,
			storage.DepositAwaitingConfirmations, storage.DepositCreditable, storage.DepositUpdate{}))
		ids = append(ids, d.ID)
	}

	wrapped := &gateFlipStore{Store: underlying, flip: func() {
		on := true
		_, err := underlying.Controls().Apply(ctx, testChainID,
			storage.ControlsUpdate{CreditPaused: &on}, "admin", "mid-batch pause")
		require.NoError(t, err)
	}}

	sc, err := NewScanner(newFakeChain(), wrapped, ScannerConfig{ChainID: testChainID, Confirmations: 1})
	require.NoError(t, err)

	// latestHeight far past every deposit so confirmations never gate.
	require.NoError(t, sc.processPending(ctx, 1000))

	// Exactly one credit committed before the flip closed the gate.
	require.Equal(t, 1, wrapped.commits)
	require.True(t, wrapped.fired)

	// The pre-flip record credited; the rest stay CREDITABLE (never credited),
	// available to a later cycle once the pause lifts.
	d0, err := underlying.Deposits().GetByID(ctx, ids[0])
	require.NoError(t, err)
	require.Equal(t, storage.DepositCredited, d0.Status)
	require.NotNil(t, d0.CreditedAt)
	for _, id := range ids[1:] {
		d, err := underlying.Deposits().GetByID(ctx, id)
		require.NoError(t, err)
		require.Equal(t, storage.DepositCreditable, d.Status)
		require.Nil(t, d.CreditedAt)
	}

	// Exactly one credited-deposit outbox event: the batch stopped at the flip.
	pending, err := underlying.Outbox().ListPending(ctx, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, OutboxTopicDepositCredited, pending[0].Topic)
}

func TestScannerWrongChainIDOpensReview(t *testing.T) {
	store := openTestStore(t)
	chain := newFakeChain()
	chain.chainID = "sovr-other-1"
	chain.addBlock(nil, nil)
	sc, err := NewScanner(chain, store, ScannerConfig{ChainID: testChainID, Confirmations: 1})
	require.NoError(t, err)
	require.Error(t, sc.Cycle(context.Background()))
	open, err := store.ChainReview().ListOpen(context.Background(), testChainID)
	require.NoError(t, err)
	require.Len(t, open, 1)
	require.Equal(t, storage.TriggerWrongChainID, open[0].Trigger)
}
