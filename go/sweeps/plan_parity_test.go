package sweeps

// Plan-fee / execution-fee parity tests (live-drill regression). The sweep
// drill on a real chain caught the planner estimating its fee from a
// FULL-BALANCE probe simulation while execution simulates the ACTUAL
// amount (balance − fee): MsgSend gas depends on the amount (emptying the
// sender's coin record is a store delete; a partial send rewrites it), so
// the planner under-estimated (2280 vs 2421 at the drill) and every
// THRESHOLD_ONLY / zero-reserve FEE_RESERVE plan was permanently
// unexecutable — DEFERRED forever at a static balance.
//
// These tests pin, against a fake chain whose simulated gas depends on the
// simulated transaction's content exactly the way the live chain's does:
//
//	(a) plan-time and execution-time fee estimation build the identical
//	    transaction (same amount, pubkey embedded in AuthInfo on BOTH
//	    paths) and produce the exact same fee;
//	(b) a full-balance plan executes with zero drift and
//	    amount + fee + reserve == balance exactly;
//	(c) genuine execution-time drift defers safely, and the deferred job
//	    is re-planned: Revisit terminal-CANCELs the stale job and the next
//	    Plan pass creates an executable job with a recomputed amount.

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
	txtypes "github.com/cosmos/cosmos-sdk/types/tx"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
	"github.com/cosmos/gogoproto/proto"
	"github.com/stretchr/testify/require"

	"github.com/sovrn-tech/sovren-exchange-integration/go/address"
	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
	"github.com/sovrn-tech/sovren-exchange-integration/go/sequences"
	"github.com/sovrn-tech/sovren-exchange-integration/go/signer/local"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage/sqlite"
)

// Drill-shaped gas model: a balance-emptying send simulates at 60800 gas;
// a residual-leaving send costs 3760 more (store rewrite vs delete). With
// the drill's gas_adjustment 1.5 and gas price 0.025 that reproduces the
// observed fees exactly: full-balance probe → 2280, actual amount → 2421.
const (
	parityBaseGas     = 60800
	parityResidualGas = 3760
)

// paritySim is one recorded Simulate call, decoded from the exact bytes
// the engine submitted.
type paritySim struct {
	sender    string
	amount    sdkmath.Int
	hasPubKey bool
	gasUsed   uint64
}

// parityChain implements Chain (and the sequences manager's chain view)
// with content-dependent simulated gas.
type parityChain struct {
	mu            sync.Mutex
	t             *testing.T
	accountNumber uint64
	sequences     map[string]uint64
	balances      map[string]sdkmath.Int
	driftGas      uint64
	sims          []paritySim
	broadcasts    int
	included      map[string]*client.TxInfo
	latestHeight  int64
}

func newParityChain(t *testing.T) *parityChain {
	return &parityChain{
		t:             t,
		accountNumber: 42,
		sequences:     map[string]uint64{},
		balances:      map[string]sdkmath.Int{},
		included:      map[string]*client.TxInfo{},
		latestHeight:  1000,
	}
}

func (f *parityChain) Account(ctx context.Context, addr string) (uint64, uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.accountNumber, f.sequences[addr], nil
}

func (f *parityChain) Balance(ctx context.Context, addr, denom string) (sdkmath.Int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b, ok := f.balances[addr]; ok {
		return b, nil
	}
	return sdkmath.ZeroInt(), nil
}

func (f *parityChain) Simulate(ctx context.Context, txBytes []byte) (*client.SimulateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	send, auth := decodeTxParts(f.t, txBytes)
	amount := send.Amount.AmountOf(storage.BaseDenom)
	gas := uint64(parityBaseGas) + f.driftGas
	if bal, ok := f.balances[send.FromAddress]; ok && amount.LT(bal) {
		gas += parityResidualGas
	}
	f.sims = append(f.sims, paritySim{
		sender:    send.FromAddress,
		amount:    amount,
		hasPubKey: len(auth.SignerInfos) == 1 && auth.SignerInfos[0].PublicKey != nil,
		gasUsed:   gas,
	})
	return &client.SimulateResult{GasWanted: gas, GasUsed: gas}, nil
}

func (f *parityChain) Broadcast(ctx context.Context, txBytes []byte, mode client.BroadcastMode) (*client.BroadcastResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.broadcasts++
	digest := sha256.Sum256(txBytes)
	return &client.BroadcastResult{
		TxHash:   strings.ToUpper(hex.EncodeToString(digest[:])),
		Accepted: true,
	}, nil
}

func (f *parityChain) Tx(ctx context.Context, hash string) (*client.TxInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if info, ok := f.included[strings.ToUpper(hash)]; ok {
		return info, nil
	}
	return nil, client.ErrNotFound
}

func (f *parityChain) NodeStatus(ctx context.Context) (*client.NodeStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &client.NodeStatus{ChainID: testChainID, LatestHeight: f.latestHeight}, nil
}

func (f *parityChain) setBalance(addr string, v int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.balances[addr] = sdkmath.NewInt(v)
}

func (f *parityChain) setDrift(gas uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.driftGas = gas
}

func (f *parityChain) bumpHeight(by int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.latestHeight += by
}

func (f *parityChain) include(hash string, height int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.included[strings.ToUpper(hash)] = &client.TxInfo{Hash: hash, Height: height, Code: 0}
}

func (f *parityChain) simsSnapshot() []paritySim {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]paritySim(nil), f.sims...)
}

func (f *parityChain) broadcastCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.broadcasts
}

// decodeTxParts decodes TxRaw bytes into the single MsgSend and AuthInfo.
func decodeTxParts(t *testing.T, txBytes []byte) (*banktypes.MsgSend, *txtypes.AuthInfo) {
	t.Helper()
	var raw txtypes.TxRaw
	require.NoError(t, proto.Unmarshal(txBytes, &raw))
	var body txtypes.TxBody
	require.NoError(t, proto.Unmarshal(raw.BodyBytes, &body))
	require.Len(t, body.Messages, 1)
	var send banktypes.MsgSend
	require.NoError(t, proto.Unmarshal(body.Messages[0].Value, &send))
	var auth txtypes.AuthInfo
	require.NoError(t, proto.Unmarshal(raw.AuthInfoBytes, &auth))
	return &send, &auth
}

type parityHarness struct {
	store  storage.Store
	chain  *parityChain
	engine *Engine
	source string
	hot    string
}

// newParityHarness mirrors the live drill's engine configuration
// (THRESHOLD_ONLY-style economics: tiny minimum, no reserve, adjustment
// 1.5, gas price 0.025).
func newParityHarness(t *testing.T, strategy storage.SweepStrategy) *parityHarness {
	t.Helper()
	s, err := sqlite.Open(filepath.Join(t.TempDir(), "parity.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	hot, err := address.DeriveAddress(testMnemonic, "m/44'/118'/0'/0/0")
	require.NoError(t, err)
	source, err := address.DeriveAddress(testMnemonic, "m/44'/118'/0'/0/2")
	require.NoError(t, err)

	loc, err := local.New(local.Options{UnsafeTestOnly: true, NetworkType: "testnet"})
	require.NoError(t, err)
	require.NoError(t, loc.ImportKey(source.Bech32, source.PrivateKey))

	ctx := context.Background()
	for _, w := range []storage.WatchedAddress{
		{ChainID: testChainID, Address: source.Bech32, Kind: storage.WatchCustomerDeposit, Active: true},
		{ChainID: testChainID, Address: hot.Bech32, Kind: storage.WatchHotWallet, Active: true},
	} {
		require.NoError(t, s.Watch().Upsert(ctx, w))
	}

	chain := newParityChain(t)
	mgr := sequences.NewManager(s, chain)
	engine, err := New(s, chain, mgr, loc, Config{
		ChainID:                      testChainID,
		Strategy:                     strategy,
		HotWallet:                    hot.Bech32,
		MinimumSweepAmountUsovr:      "1000",
		MaximumFeePercentageForSweep: "100",
		FeeReserveUsovr:              "0",
		GasAdjustment:                "1.5",
		GasPriceUsovr:                "0.025",
		SimulateUnavailable:          SimulateQueue,
		BroadcastTimeout:             50 * time.Millisecond,
		Confirmations:                3,
	})
	require.NoError(t, err)
	return &parityHarness{store: s, chain: chain, engine: engine, source: source.Bech32, hot: hot.Bech32}
}

func (h *parityHarness) job(t *testing.T, sweepID string) storage.SweepJob {
	t.Helper()
	j, err := h.store.Sweeps().Get(context.Background(), sweepID)
	require.NoError(t, err)
	return j
}

// creditParityDeposit walks a deposit for the source to CREDITED.
func (h *parityHarness) creditDeposit(t *testing.T, txHash string, amount int64) int64 {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	d, err := h.store.Deposits().Insert(ctx, storage.DepositRecord{
		ChainID:          testChainID,
		TxHash:           txHash,
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

func (h *parityHarness) depositStatus(t *testing.T, id int64) storage.DepositStatus {
	t.Helper()
	d, err := h.store.Deposits().GetByID(context.Background(), id)
	require.NoError(t, err)
	return d.Status
}

// TestPlanFeeMatchesExecutionFee pins (a) and (b): the planner's final
// simulation is the execution simulation — same amount, pubkey-bearing
// AuthInfo on both paths, exactly equal fee — so a full-balance plan
// executes with zero drift and amount + fee + reserve == balance exactly.
func TestPlanFeeMatchesExecutionFee(t *testing.T) {
	for _, strategy := range []storage.SweepStrategy{
		storage.StrategyThresholdOnly,
		storage.StrategyFeeReserve, // zero-reserve edge: full-balance maths
	} {
		t.Run(string(strategy), func(t *testing.T) {
			ctx := context.Background()
			h := newParityHarness(t, strategy)
			h.chain.setBalance(h.source, 2_000_000)
			depID := h.creditDeposit(t, "DEP1", 2_000_000)

			report, err := h.engine.Plan(ctx)
			require.NoError(t, err)
			require.Empty(t, report.JobsDeferred, "a coverable full-balance plan must not defer")
			require.Len(t, report.JobsCreated, 1, "held: %v", report.Held)
			sweepID := report.JobsCreated[0]
			j := h.job(t, sweepID)

			// Fixed point at the drill numbers: the full-balance probe
			// alone would say fee 2280 / amount 1997720 (the live failure);
			// re-simulating the induced amount converges on fee 2421.
			require.Equal(t, "1997579", j.AmountBaseUnits.String(),
				"amount = balance − the fee of the ACTUAL transaction, not of a full-balance probe")

			planSims := h.chain.simsSnapshot()
			require.GreaterOrEqual(t, len(planSims), 2, "the planner must re-simulate the induced amount")
			for i, s := range planSims {
				require.True(t, s.hasPubKey, "plan simulation %d must embed the sender pubkey in AuthInfo (KF-1 parity)", i)
				require.Equal(t, h.source, s.sender)
			}
			planFinal := planSims[len(planSims)-1]
			require.Equal(t, j.AmountBaseUnits.String(), planFinal.amount.String(),
				"the planner's final simulation carries the planned amount itself")

			// Execution with zero drift: Prepare simulates the identical
			// transaction and computes the identical fee.
			require.NoError(t, h.engine.Prepare(ctx, sweepID))
			j = h.job(t, sweepID)
			require.Equal(t, storage.SweepSigned, j.Status)

			execSims := h.chain.simsSnapshot()[len(planSims):]
			require.Len(t, execSims, 1)
			require.True(t, execSims[0].hasPubKey, "execution simulation embeds the sender pubkey")
			require.Equal(t, planFinal.amount.String(), execSims[0].amount.String(),
				"plan and execution simulate the same amount")
			require.Equal(t, planFinal.gasUsed, execSims[0].gasUsed,
				"identical construction ⇒ identical simulated gas")

			send, auth := decodeTxParts(t, j.SignedTxBytes)
			require.Equal(t, j.AmountBaseUnits.String(), send.Amount.AmountOf(storage.BaseDenom).String())
			execFee := auth.Fee.Amount.AmountOf(storage.BaseDenom)
			require.Equal(t, "2421", execFee.String(), "execution fee == plan fee exactly")
			require.Equal(t, uint64(96840), auth.Fee.GasLimit)
			require.Equal(t, "2000000",
				j.AmountBaseUnits.Add(execFee).Add(j.FeeReserveBaseUnits).String(),
				"amount + fee + reserve == balance exactly (no dust, no shortfall)")

			require.NoError(t, h.engine.Broadcast(ctx, sweepID))
			h.chain.include(*j.TxHash, 990)
			require.NoError(t, h.engine.Confirm(ctx, sweepID))
			require.Equal(t, storage.SweepConfirmed, h.job(t, sweepID).Status)
			require.Equal(t, storage.DepositSwept, h.depositStatus(t, depID))
		})
	}
}

// TestFeeDriftDefersThenReplans pins (c): a gas change between plan and
// execution defers safely; a still-unfundable balance stays DEFERRED (no
// cancel, no loop); once a fresh plan passes, Revisit terminal-CANCELs the
// stale job (freeing the non-terminal-unique slot) and the next Plan pass
// creates an executable job with the recomputed amount, to which the
// orphaned SWEEP_PENDING deposits re-attach.
func TestFeeDriftDefersThenReplans(t *testing.T) {
	ctx := context.Background()
	h := newParityHarness(t, storage.StrategyThresholdOnly)
	h.chain.setBalance(h.source, 2_000_000)
	depID := h.creditDeposit(t, "DEP1", 2_000_000)

	report, err := h.engine.Plan(ctx)
	require.NoError(t, err)
	require.Len(t, report.JobsCreated, 1)
	staleID := report.JobsCreated[0]
	require.Equal(t, "1997579", h.job(t, staleID).AmountBaseUnits.String())

	// Genuine drift: gas rises before execution (fee 2421 → 2721).
	h.chain.setDrift(8000)
	err = h.engine.Prepare(ctx, staleID)
	require.ErrorIs(t, err, ErrDeferred)
	require.Equal(t, storage.SweepDeferred, h.job(t, staleID).Status)
	require.Equal(t, storage.DepositSweepPending, h.depositStatus(t, depID))
	require.Equal(t, 0, h.chain.broadcastCount(), "a deferred sweep never broadcasts")

	// While the balance genuinely funds no sweep, Revisit neither revives
	// nor cancels: DEFERRED is stable — no retry loop.
	h.chain.setBalance(h.source, 900)
	for range 3 {
		revived, err := h.engine.Revisit(ctx, staleID)
		require.NoError(t, err)
		require.False(t, revived)
		require.Equal(t, storage.SweepDeferred, h.job(t, staleID).Status)
	}

	// Balance restored: the stored amount is still unexecutable at the new
	// fee, but a FRESH plan passes — Revisit terminal-CANCELs the stale job.
	h.chain.setBalance(h.source, 2_000_000)
	revived, err := h.engine.Revisit(ctx, staleID)
	require.NoError(t, err)
	require.False(t, revived, "the stale job is not revived; it is replaced")
	require.Equal(t, storage.SweepCancelled, h.job(t, staleID).Status,
		"stale full-balance job ends terminal, freeing the non-terminal-unique slot")
	require.Equal(t, storage.DepositSweepPending, h.depositStatus(t, depID),
		"covered deposits stay earmarked for the next plan")

	// Next pass re-plans with the recomputed amount (new height ⇒ new
	// idempotency key, exactly as on a live chain).
	h.chain.bumpHeight(1)
	report, err = h.engine.Plan(ctx)
	require.NoError(t, err)
	require.Len(t, report.JobsCreated, 1, "held: %v deferred: %v", report.Held, report.JobsDeferred)
	freshID := report.JobsCreated[0]
	require.NotEqual(t, staleID, freshID)
	fresh := h.job(t, freshID)
	require.Equal(t, "1997279", fresh.AmountBaseUnits.String(), "balance − the drifted fee 2721")
	require.Equal(t, []int64{depID}, fresh.DepositIDs, "orphaned SWEEP_PENDING deposits re-attach")

	// The re-planned job executes to CONFIRMED at the new gas level.
	require.NoError(t, h.engine.Prepare(ctx, freshID))
	fresh = h.job(t, freshID)
	require.Equal(t, storage.SweepSigned, fresh.Status)
	_, auth := decodeTxParts(t, fresh.SignedTxBytes)
	execFee := auth.Fee.Amount.AmountOf(storage.BaseDenom)
	require.Equal(t, "2721", execFee.String())
	require.Equal(t, "2000000", fresh.AmountBaseUnits.Add(execFee).String(),
		"recomputed amount + drifted fee == balance exactly")

	require.NoError(t, h.engine.Broadcast(ctx, freshID))
	h.chain.include(*fresh.TxHash, 995)
	require.NoError(t, h.engine.Confirm(ctx, freshID))
	require.Equal(t, storage.SweepConfirmed, h.job(t, freshID).Status)
	require.Equal(t, storage.DepositSwept, h.depositStatus(t, depID))
	require.Equal(t, storage.SweepCancelled, h.job(t, staleID).Status, "the old job stays terminal")
	require.Equal(t, 1, h.chain.broadcastCount(), "exactly one transaction ever reaches the wire")
}
