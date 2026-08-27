package sweeps

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmath "cosmossdk.io/math"
	"github.com/stretchr/testify/require"

	"github.com/sovrn-tech/sovren-exchange-integration/go/address"
	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
	"github.com/sovrn-tech/sovren-exchange-integration/go/deposits"
	"github.com/sovrn-tech/sovren-exchange-integration/go/sequences"
	"github.com/sovrn-tech/sovren-exchange-integration/go/signer/local"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage/sqlite"
)

const (
	testChainID  = "test-sovr-1"
	testMnemonic = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"
)

// fakeChain implements sweeps.Chain and sequences.Chain.
type fakeChain struct {
	mu            sync.Mutex
	accountNumber uint64
	sequences     map[string]uint64
	balances      map[string]sdkmath.Int
	simGasUsed    uint64
	simErr        error
	broadcastErr  error
	broadcastRej  *client.BroadcastResult
	broadcasts    [][]byte
	included      map[string]*client.TxInfo
	latestHeight  int64
}

func newFakeChain() *fakeChain {
	return &fakeChain{
		accountNumber: 42,
		sequences:     map[string]uint64{},
		balances:      map[string]sdkmath.Int{},
		simGasUsed:    100000,
		included:      map[string]*client.TxInfo{},
		latestHeight:  1000,
	}
}

func (f *fakeChain) Account(ctx context.Context, addr string) (uint64, uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.accountNumber, f.sequences[addr], nil
}

func (f *fakeChain) Balance(ctx context.Context, addr, denom string) (sdkmath.Int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b, ok := f.balances[addr]; ok {
		return b, nil
	}
	return sdkmath.ZeroInt(), nil
}

func (f *fakeChain) Simulate(ctx context.Context, txBytes []byte) (*client.SimulateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.simErr != nil {
		return nil, f.simErr
	}
	return &client.SimulateResult{GasWanted: f.simGasUsed, GasUsed: f.simGasUsed}, nil
}

func (f *fakeChain) Broadcast(ctx context.Context, txBytes []byte, mode client.BroadcastMode) (*client.BroadcastResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]byte, len(txBytes))
	copy(cp, txBytes)
	f.broadcasts = append(f.broadcasts, cp)
	if f.broadcastErr != nil {
		return nil, f.broadcastErr
	}
	if f.broadcastRej != nil {
		return f.broadcastRej, nil
	}
	digest := sha256.Sum256(txBytes)
	return &client.BroadcastResult{
		TxHash:   strings.ToUpper(hex.EncodeToString(digest[:])),
		Accepted: true,
	}, nil
}

func (f *fakeChain) Tx(ctx context.Context, hash string) (*client.TxInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if info, ok := f.included[strings.ToUpper(hash)]; ok {
		return info, nil
	}
	return nil, client.ErrNotFound
}

func (f *fakeChain) NodeStatus(ctx context.Context) (*client.NodeStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &client.NodeStatus{ChainID: testChainID, LatestHeight: f.latestHeight}, nil
}

func (f *fakeChain) setBalance(addr string, v int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.balances[addr] = sdkmath.NewInt(v)
}

func (f *fakeChain) addBalance(addr string, v int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.balances[addr] = f.balances[addr].Add(sdkmath.NewInt(v))
}

func (f *fakeChain) include(hash string, height int64, code uint32, log string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.included[strings.ToUpper(hash)] = &client.TxInfo{Hash: hash, Height: height, Code: code, RawLog: log}
}

func (f *fakeChain) broadcastCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.broadcasts)
}

func (f *fakeChain) broadcastAt(i int) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.broadcasts[i]
}

type harness struct {
	store     storage.Store
	chain     *fakeChain
	engine    *Engine
	seq       *sequences.Manager
	source    string
	hot       string
	feeWallet string
}

func defaultConfig(strategy storage.SweepStrategy, hot string) Config {
	return Config{
		ChainID:                      testChainID,
		Strategy:                     strategy,
		HotWallet:                    hot,
		MinimumSweepAmountUsovr:      "1000000",
		MaximumFeePercentageForSweep: "1.0",
		FeeReserveUsovr:              "50000",
		GasAdjustment:                "1.3",
		GasPriceUsovr:                "0.025",
		SimulateUnavailable:          SimulateQueue,
		BroadcastTimeout:             50 * time.Millisecond,
		Confirmations:                3,
	}
}

// newHarness builds a sqlite store, a local UNSAFE_TEST_ONLY signer holding
// the source and fee-wallet keys, the watch set (source CUSTOMER_DEPOSIT,
// hot HOT_WALLET, fee wallet FEE_WALLET), and an Engine.
func newHarness(t *testing.T, mut func(*Config)) *harness {
	t.Helper()
	s, err := sqlite.Open(filepath.Join(t.TempDir(), "kit.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	hot, err := address.DeriveAddress(testMnemonic, "m/44'/118'/0'/0/0")
	require.NoError(t, err)
	source, err := address.DeriveAddress(testMnemonic, "m/44'/118'/0'/0/2")
	require.NoError(t, err)
	feeWallet, err := address.DeriveAddress(testMnemonic, "m/44'/118'/0'/0/3")
	require.NoError(t, err)

	loc, err := local.New(local.Options{UnsafeTestOnly: true, NetworkType: "testnet"})
	require.NoError(t, err)
	require.NoError(t, loc.ImportKey(source.Bech32, source.PrivateKey))
	require.NoError(t, loc.ImportKey(feeWallet.Bech32, feeWallet.PrivateKey))

	ctx := context.Background()
	for _, w := range []storage.WatchedAddress{
		{ChainID: testChainID, Address: source.Bech32, Kind: storage.WatchCustomerDeposit, Active: true},
		{ChainID: testChainID, Address: hot.Bech32, Kind: storage.WatchHotWallet, Active: true},
		{ChainID: testChainID, Address: feeWallet.Bech32, Kind: storage.WatchFeeWallet, Active: true},
	} {
		require.NoError(t, s.Watch().Upsert(ctx, w))
	}

	cfg := defaultConfig(storage.StrategyFeeReserve, hot.Bech32)
	if mut != nil {
		mut(&cfg)
	}

	chain := newFakeChain()
	mgr := sequences.NewManager(s, chain)
	engine, err := New(s, chain, mgr, loc, cfg)
	require.NoError(t, err)
	return &harness{
		store: s, chain: chain, engine: engine, seq: mgr,
		source: source.Bech32, hot: hot.Bech32, feeWallet: feeWallet.Bech32,
	}
}

// creditDeposit inserts a deposit for the source address and walks it to
// CREDITED through the legal §3b transitions.
func (h *harness) creditDeposit(t *testing.T, txHash string, amount int64) int64 {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	d, err := h.store.Deposits().Insert(ctx, storage.DepositRecord{
		ChainID:          testChainID,
		TxHash:           txHash,
		MessageIndex:     0,
		CoinIndex:        0,
		BlockHeight:      900,
		BlockTimestamp:   now,
		RecipientAddress: h.source,
		Denom:            storage.BaseDenom,
		AmountBaseUnits:  sdkmath.NewInt(amount),
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
		require.NoError(t, h.store.Deposits().UpdateStatus(ctx, d.ID, step.from, step.to, storage.DepositUpdate{}))
	}
	require.NoError(t, h.store.Deposits().UpdateStatus(ctx, d.ID,
		storage.DepositCreditable, storage.DepositCredited, storage.DepositUpdate{CreditedAt: &now}))
	return d.ID
}

func (h *harness) deposit(t *testing.T, id int64) storage.DepositRecord {
	t.Helper()
	d, err := h.store.Deposits().GetByID(context.Background(), id)
	require.NoError(t, err)
	return d
}

func (h *harness) job(t *testing.T, sweepID string) storage.SweepJob {
	t.Helper()
	j, err := h.store.Sweeps().Get(context.Background(), sweepID)
	require.NoError(t, err)
	return j
}

func (h *harness) reservation(t *testing.T, sweepID string) storage.SequenceReservation {
	t.Helper()
	res, err := h.store.Sequences().GetByWorkRef(context.Background(),
		storage.WorkRef{Kind: storage.WorkSweep, ID: sweepID})
	require.NoError(t, err)
	return res
}

// driveToConfirmed advances one job Prepare → Broadcast → include → Confirm.
func (h *harness) driveToConfirmed(t *testing.T, sweepID string) storage.SweepJob {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, h.engine.Prepare(ctx, sweepID))
	j := h.job(t, sweepID)
	require.Equal(t, storage.SweepSigned, j.Status)
	require.NotEmpty(t, j.SignedTxBytes)
	require.NotNil(t, j.TxHash)
	require.NoError(t, h.engine.Broadcast(ctx, sweepID))
	h.chain.include(*j.TxHash, 990, 0, "")
	require.NoError(t, h.engine.Confirm(ctx, sweepID))
	j = h.job(t, sweepID)
	require.Equal(t, storage.SweepConfirmed, j.Status)
	return j
}

// planOne runs Plan and requires exactly one job created.
func (h *harness) planOne(t *testing.T) string {
	t.Helper()
	report, err := h.engine.Plan(context.Background())
	require.NoError(t, err)
	require.Len(t, report.JobsCreated, 1, "held: %v deferred: %v", report.Held, report.JobsDeferred)
	return report.JobsCreated[0]
}

// ---------------------------------------------------------------------------
// Per-strategy happy paths (T062)
// ---------------------------------------------------------------------------

// TestFeeReserveHappyPath: amount = balance − fee − fee_reserve, the source
// pays its own fee, deposits flip SWEEP_PENDING → SWEPT on confirm, and the
// reservation is CONSUMED. Fee maths pinned: gas 100000 × 1.3 = 130000,
// fee = ceil(130000 × 0.025) = 3250.
func TestFeeReserveHappyPath(t *testing.T) {
	h := newHarness(t, nil)
	h.chain.setBalance(h.source, 10_000_000_000)
	depID := h.creditDeposit(t, "DEP1", 10_000_000_000)

	sweepID := h.planOne(t)
	j := h.job(t, sweepID)
	require.Equal(t, storage.SweepPending, j.Status)
	require.Equal(t, "9999946750", j.AmountBaseUnits.String(), "balance − 3250 fee − 50000 reserve")
	require.Equal(t, "50000", j.FeeReserveBaseUnits.String())
	require.Equal(t, []int64{depID}, j.DepositIDs)
	require.Equal(t, storage.DepositSweepPending, h.deposit(t, depID).Status,
		"CREDITED → SWEEP_PENDING in the creating transaction")
	require.Equal(t, IdempotencyKey(testChainID, h.source, sdkmath.NewInt(10_000_000_000), 1000), j.IdempotencyKey)

	j = h.driveToConfirmed(t, sweepID)
	d := h.deposit(t, depID)
	require.Equal(t, storage.DepositSwept, d.Status, "SWEEP_PENDING → SWEPT on confirm")
	require.NotNil(t, d.SweepTxHash)
	require.Equal(t, *j.TxHash, *d.SweepTxHash)
	require.Equal(t, storage.SequenceConsumed, h.reservation(t, sweepID).Status)
	require.Equal(t, 1, h.chain.broadcastCount())
}

// TestThresholdOnlyHappyPath: below the configured minimum nothing happens;
// at or above it the full balance minus fee sweeps (no reserve kept).
func TestThresholdOnlyHappyPath(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Strategy = storage.StrategyThresholdOnly })
	h.chain.setBalance(h.source, 999_999) // below minimum_sweep_amount_usovr

	report, err := h.engine.Plan(context.Background())
	require.NoError(t, err)
	require.Empty(t, report.JobsCreated)
	require.Contains(t, report.Held[h.source], "below minimum")

	h.chain.setBalance(h.source, 5_000_000)
	sweepID := h.planOne(t)
	j := h.job(t, sweepID)
	require.Equal(t, "4996750", j.AmountBaseUnits.String(), "balance − 3250 fee, no reserve")
	h.driveToConfirmed(t, sweepID)
}

// TestCustodyAbstractedHappyPath: no transaction, no job — deposits settle
// by bookkeeping under the unified custody boundary.
func TestCustodyAbstractedHappyPath(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.Strategy = storage.StrategyCustodyAbstract })
	// Rebuild the engine without a signer: CUSTODY_ABSTRACTED needs none.
	engine, err := New(h.store, h.chain, h.seq, nil, defaultConfig(storage.StrategyCustodyAbstract, h.hot))
	require.NoError(t, err)
	h.engine = engine

	h.chain.setBalance(h.source, 10_000_000_000)
	d1 := h.creditDeposit(t, "DEP1", 4_000_000)
	d2 := h.creditDeposit(t, "DEP2", 6_000_000)

	report, err := h.engine.Plan(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, report.CustodySwept)
	require.Empty(t, report.JobsCreated)
	require.Equal(t, storage.DepositSwept, h.deposit(t, d1).Status)
	require.Equal(t, storage.DepositSwept, h.deposit(t, d2).Status)
	require.Nil(t, h.deposit(t, d1).SweepTxHash, "no transaction backs a custody-abstracted settlement")
	require.Equal(t, 0, h.chain.broadcastCount())
	for _, status := range []storage.SweepStatus{storage.SweepPending, storage.SweepConfirmed} {
		jobs, err := h.store.Sweeps().ListByStatus(context.Background(), testChainID, status, 10)
		require.NoError(t, err)
		require.Empty(t, jobs)
	}
}

// TestFeeFundHappyPath: the sweep takes the FULL balance; the fee arrives
// from the fee wallet as a separate funding SweepJob driven through the
// same engine with its own reservation and persisted bytes. The funding
// transfer is ledger-classified FEE_FUNDING and NEVER becomes a deposit —
// asserted by composing the actual funding bytes with
// deposits.ParseBlockTransfers + RecordBlock.
func TestFeeFundHappyPath(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, func(c *Config) { c.Strategy = storage.StrategyFeeFund })
	h.chain.setBalance(h.source, 2_000_000_000)
	h.chain.setBalance(h.feeWallet, 1_000_000_000)
	depID := h.creditDeposit(t, "DEP1", 2_000_000_000)

	sweepID := h.planOne(t)
	j := h.job(t, sweepID)
	require.Equal(t, "2000000000", j.AmountBaseUnits.String(), "FEE_FUND sweeps the full balance")

	// Prepare holds on the funding leg it just planned.
	err := h.engine.Prepare(ctx, sweepID)
	require.ErrorIs(t, err, ErrFundingPending)
	fundingID := fundingSweepID(sweepID)
	fj := h.job(t, fundingID)
	require.True(t, IsFundingJob(fj))
	require.Equal(t, h.feeWallet, fj.SourceAddress)
	require.Equal(t, h.source, fj.HotWalletAddress, "funding destination is the sweep source")
	require.Equal(t, "3250", fj.AmountBaseUnits.String(), "funding amount = the sweep's fee")
	require.Empty(t, fj.DepositIDs, "a funding leg covers no deposits")

	// A second Prepare emits NO second funding job (idempotent, no loop).
	err = h.engine.Prepare(ctx, sweepID)
	require.ErrorIs(t, err, ErrFundingPending)
	require.Equal(t, 0, h.chain.broadcastCount())

	// Drive the funding leg to CONFIRMED via the same engine steps, then
	// mirror its effect on the fake chain.
	fj = h.driveToConfirmed(t, fundingID)
	h.chain.addBalance(h.source, 3250)
	h.chain.addBalance(h.feeWallet, -(3250 + 3250)) // amount + its own fee
	require.Equal(t, storage.SequenceConsumed, h.reservation(t, fundingID).Status)

	// Funded: the customer sweep now signs and confirms.
	j = h.driveToConfirmed(t, sweepID)
	require.Equal(t, storage.DepositSwept, h.deposit(t, depID).Status)
	require.Equal(t, 2, h.chain.broadcastCount(), "exactly one funding + one sweep broadcast")

	// The funding transfer must never be credited: run the REAL funding
	// bytes through the deposit parser + recorder.
	assertFeeFundingNeverCredited(t, h, fj.SignedTxBytes)
}

// assertFeeFundingNeverCredited composes the funding transaction with
// deposits.ParseBlockTransfers and RecordBlock: classification FEE_FUNDING
// on both rows, zero EXTERNAL_DEPOSIT rows, zero deposit records (FR-023).
func assertFeeFundingNeverCredited(t *testing.T, h *harness, fundingTxBytes []byte) {
	t.Helper()
	ctx := context.Background()
	watch := deposits.NewWatchSet([]storage.WatchedAddress{
		{ChainID: testChainID, Address: h.source, Kind: storage.WatchCustomerDeposit, Active: true},
		{ChainID: testChainID, Address: h.hot, Kind: storage.WatchHotWallet, Active: true},
		{ChainID: testChainID, Address: h.feeWallet, Kind: storage.WatchFeeWallet, Active: true},
	})
	block := &client.Block{
		ChainID:       testChainID,
		Height:        995,
		Hash:          []byte{0xAA, 0x01},
		Time:          time.Now().UTC(),
		LastBlockHash: []byte{0xAA, 0x00},
		Txs:           [][]byte{fundingTxBytes},
	}
	results := &client.BlockResults{
		Height:    995,
		TxResults: []client.TxExecResult{{Code: 0}},
	}
	bp, err := deposits.ParseBlockTransfers(block, results, watch)
	require.NoError(t, err)
	require.NotEmpty(t, bp.Transfers, "the funding MsgSend must decode")
	for _, tr := range bp.Transfers {
		require.Equal(t, storage.ClassFeeFunding, tr.Classification,
			"fee wallet → customer address is FEE_FUNDING, never EXTERNAL_DEPOSIT")
	}

	outcome, err := deposits.RecordBlock(ctx, h.store, bp, deposits.RecordPolicy{ChainID: testChainID}, time.Now().UTC())
	require.NoError(t, err)
	require.Zero(t, outcome.DepositsInserted, "FEE_FUNDING transfers never create deposit records")
	require.Positive(t, outcome.LedgerAppends, "the movement IS ledgered (reconciliation truth)")

	entries, err := h.store.Ledger().List(ctx, storage.LedgerQuery{
		ChainID: testChainID, Address: h.source, FromHeight: 995, ToHeight: 995,
	})
	require.NoError(t, err)
	require.NotEmpty(t, entries)
	for _, e := range entries {
		require.Equal(t, storage.ClassFeeFunding, e.Classification)
	}
}

// TestConfigRejectsBadValues pins that thresholds are validated config,
// never silently defaulted (FR-038/FR-040).
func TestConfigRejectsBadValues(t *testing.T) {
	hot, err := address.DeriveAddress(testMnemonic, "m/44'/118'/0'/0/0")
	require.NoError(t, err)

	bad := defaultConfig(storage.StrategyFeeReserve, hot.Bech32)
	bad.MaximumFeePercentageForSweep = "1,0"
	_, err = bad.parse()
	require.Error(t, err)

	bad = defaultConfig(storage.StrategyFeeReserve, hot.Bech32)
	bad.MinimumSweepAmountUsovr = "10.5"
	_, err = bad.parse()
	require.Error(t, err)

	bad = defaultConfig(storage.StrategyFeeReserve, hot.Bech32)
	bad.FeeReserveUsovr = ""
	_, err = bad.parse()
	require.Error(t, err)

	bad = defaultConfig(storage.StrategyFeeReserve, "")
	_, err = bad.parse()
	require.Error(t, err, "hot wallet required for transacting strategies")

	bad = defaultConfig("NOT_A_STRATEGY", hot.Bech32)
	_, err = bad.parse()
	require.Error(t, err)

	bad = defaultConfig(storage.StrategyFeeReserve, hot.Bech32)
	bad.SimulateUnavailable = "yolo"
	_, err = bad.parse()
	require.Error(t, err)

	ok := defaultConfig(storage.StrategyCustodyAbstract, "")
	_, err = ok.parse()
	require.NoError(t, err, "custody-abstracted needs no hot wallet or gas parameters")
}
