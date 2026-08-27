package reconcile

import (
	"context"
	"testing"
	"time"

	sdkmath "cosmossdk.io/math"
	"github.com/stretchr/testify/require"

	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/metrics"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

func createWithdrawal(t *testing.T, s storage.Store, id, source string, amount int64, fee int64, status storage.WithdrawalStatus) storage.WithdrawalRecord {
	t.Helper()
	feeInt := sdkmath.NewInt(fee)
	w := storage.WithdrawalRecord{
		WithdrawalID: id, IdempotencyKey: "idem-" + id,
		ChainID: testChainID, SourceAddress: source,
		DestinationAddress: "sovr1destination", Denom: storage.BaseDenom,
		AmountBaseUnits: sdkmath.NewInt(amount), Status: status,
		CreatedAt: testNow, UpdatedAt: testNow,
	}
	if fee > 0 {
		w.FeeAmountBaseUnits = &feeInt
	}
	rec, err := s.Withdrawals().Create(context.Background(), w)
	require.NoError(t, err)
	return rec
}

func createSweep(t *testing.T, s storage.Store, id, source, hot string, amount int64, to storage.SweepStatus) storage.SweepJob {
	t.Helper()
	ctx := context.Background()
	j, err := s.Sweeps().Create(ctx, storage.SweepJob{
		SweepID: id, IdempotencyKey: "idem-" + id,
		ChainID: testChainID, SourceAddress: source, HotWalletAddress: hot,
		Strategy:        storage.StrategyFeeReserve,
		AmountBaseUnits: sdkmath.NewInt(amount), FeeReserveBaseUnits: sdkmath.ZeroInt(),
		CreatedAt: testNow, UpdatedAt: testNow,
	})
	require.NoError(t, err)
	steps := map[storage.SweepStatus][]storage.SweepStatus{
		storage.SweepPending:   nil,
		storage.SweepBuilt:     {storage.SweepBuilt},
		storage.SweepSigned:    {storage.SweepBuilt, storage.SweepSigned},
		storage.SweepBroadcast: {storage.SweepBuilt, storage.SweepSigned, storage.SweepBroadcast},
		storage.SweepConfirmed: {storage.SweepBuilt, storage.SweepSigned, storage.SweepBroadcast, storage.SweepConfirmed},
	}[to]
	from := storage.SweepPending
	for _, next := range steps {
		require.NoError(t, s.Sweeps().UpdateStatus(ctx, j.SweepID, from, next, storage.SweepUpdate{}))
		from = next
	}
	j.Status = to
	return j
}

// TestFailedWithdrawalOutOfGasCleanReconciliation: a withdrawal that failed
// in DeliverTx (out of gas) paid its fee but moved no funds. The ledger holds
// the failed-tx OUT row (excluded — tx_code != 0) plus the FEE_DEDUCTION
// FeeOutflow, so the hot wallet reconciles clean against the on-chain balance
// that only lost the fee.
func TestFailedWithdrawalOutOfGasCleanReconciliation(t *testing.T) {
	s := openTestStore(t)
	_, hot := seedWatch(t, s)
	addr := hot.Bech32
	ctx := context.Background()

	// Funding sweep into the hot wallet: +10_000_000.
	appendLedger(t, s, addr, "DD01", 0, 0, 40, storage.DirectionIn, storage.ClassSweep, 10_000_000, 0)
	// Failed withdrawal (out of gas, code 11): OUT row recorded with the
	// failure code — excluded from the formula — and the fee (2_500) that the
	// ante handler still deducted, recorded as FEE_DEDUCTION (data model §8a).
	appendLedger(t, s, addr, "DD02", 0, 1, 45, storage.DirectionOut, storage.ClassWithdrawal, 1_000_000, 11)
	appendFee(t, s, addr, "DD02", 45, 2_500, 11)
	// The workflow accurately recorded FAILED (FR-031).
	createWithdrawal(t, s, "wd-failed", addr, 1_000_000, 2_500, storage.WithdrawalFailed)

	chain := newFakeChain()
	chain.balances[addr] = sdkmath.NewInt(10_000_000 - 2_500) // only the fee left the wallet

	m := metrics.NewSet()
	r := newTestReconciler(t, s, chain, WithMetrics(m))
	entry, err := r.ReconcileAddress(ctx, addr)
	require.NoError(t, err)
	require.True(t, entry.Difference.IsZero(), "difference: %s", entry.Difference)
	require.Equal(t, 0.0, counterValue(t, m.ReconciliationDiscrepancy.WithLabelValues(testChainID)))

	hw, err := r.HotWallet(ctx, addr)
	require.NoError(t, err)
	require.True(t, hw.Difference.IsZero())
	require.True(t, hw.Explained)
	require.Equal(t, sdkmath.NewInt(10_000_000), hw.ConfirmedSweepInflow)
	// The failed withdrawal's OUT row does not count as a settled outflow.
	require.True(t, hw.ConfirmedWithdrawalOutflow.IsZero())
}

// TestHotWalletBroadcastUnconfirmedExplained: a BROADCAST withdrawal already
// landed on chain but the scanner has not recorded it yet — negative drift
// within the in-flight window is explained, never a discrepancy claim.
func TestHotWalletBroadcastUnconfirmedExplained(t *testing.T) {
	s := openTestStore(t)
	_, hot := seedWatch(t, s)
	addr := hot.Bech32
	ctx := context.Background()

	appendLedger(t, s, addr, "EE01", 0, 0, 50, storage.DirectionIn, storage.ClassSweep, 5_000_000, 0)
	createWithdrawal(t, s, "wd-bcast", addr, 1_000_000, 2_500, storage.WithdrawalBroadcast)
	createWithdrawal(t, s, "wd-signed", addr, 300_000, 2_500, storage.WithdrawalSigned)

	chain := newFakeChain()
	// On-chain the broadcast withdrawal already executed: −1_000_000 − 2_500.
	chain.balances[addr] = sdkmath.NewInt(5_000_000 - 1_000_000 - 2_500)

	r := newTestReconciler(t, s, chain)
	hw, err := r.HotWallet(ctx, addr)
	require.NoError(t, err)
	require.Equal(t, sdkmath.NewInt(5_000_000), hw.LedgerExpected)
	require.Equal(t, sdkmath.NewInt(-1_002_500), hw.Difference)
	require.Equal(t, sdkmath.NewInt(1_002_500), hw.BroadcastUnconfirmedOutflow)
	require.Equal(t, sdkmath.NewInt(302_500), hw.PendingSignedOutflow)
	require.True(t, hw.Explained)

	// Drift beyond the in-flight window is NOT explained.
	chain.balances[addr] = sdkmath.NewInt(5_000_000 - 2_000_000)
	hw, err = r.HotWallet(ctx, addr)
	require.NoError(t, err)
	require.False(t, hw.Explained)
}

// TestBusinessSection reconciles workflow totals against ledger totals:
// uncredited external inflows are a lag bucket, never a discrepancy; a
// credited total exceeding the ledger is an impossible state and alerts.
func TestBusinessSection(t *testing.T) {
	s := openTestStore(t)
	customer, hot := seedWatch(t, s)
	ctx := context.Background()

	// Two external deposits (1_000_000 + 250_000); one credited, one parked.
	e1 := appendLedger(t, s, customer.Bech32, "FF01", 0, 0, 60, storage.DirectionIn, storage.ClassExternalDeposit, 1_000_000, 0)
	_ = e1
	appendLedger(t, s, customer.Bech32, "FF02", 0, 0, 61, storage.DirectionIn, storage.ClassExternalDeposit, 250_000, 0)
	insertDeposit(t, s, "FF01", customer.Bech32, 1_000_000, storage.DepositCredited)
	insertDeposit(t, s, "FF02", customer.Bech32, 250_000, storage.DepositBelowMinimum)

	// A confirmed sweep customer → hot (IN side counted once).
	appendLedger(t, s, customer.Bech32, "FF03", 0, 1, 62, storage.DirectionOut, storage.ClassSweep, 900_000, 0)
	appendLedger(t, s, hot.Bech32, "FF03", 0, 0, 62, storage.DirectionIn, storage.ClassSweep, 900_000, 0)
	createSweep(t, s, "sweep-1", customer.Bech32, hot.Bech32, 900_000, storage.SweepConfirmed)

	// A confirmed withdrawal from the hot wallet.
	appendLedger(t, s, hot.Bech32, "FF04", 0, 1, 63, storage.DirectionOut, storage.ClassWithdrawal, 400_000, 0)
	createWithdrawal(t, s, "wd-ok", hot.Bech32, 400_000, 2_500, storage.WithdrawalConfirmed)

	m := metrics.NewSet()
	r := newTestReconciler(t, s, newFakeChain(), WithMetrics(m))
	sec, err := r.Business(ctx)
	require.NoError(t, err)
	require.Equal(t, sdkmath.NewInt(1_250_000), sec.LedgerExternalDepositTotal)
	require.Equal(t, sdkmath.NewInt(1_000_000), sec.CreditedDepositTotal)
	require.Equal(t, sdkmath.NewInt(250_000), sec.UncreditedExternalTotal)
	require.Equal(t, sdkmath.NewInt(400_000), sec.LedgerWithdrawalOutflowTotal)
	require.Equal(t, sdkmath.NewInt(400_000), sec.WorkflowWithdrawalTotal)
	require.Equal(t, sdkmath.NewInt(900_000), sec.LedgerSweepInflowTotal)
	require.Equal(t, sdkmath.NewInt(900_000), sec.WorkflowSweepTotal)
	require.Empty(t, sec.Findings)
	require.Equal(t, 0.0, counterValue(t, m.ReconciliationDiscrepancy.WithLabelValues(testChainID)))

	// Impossible state: another credited deposit with no ledger backing.
	insertDeposit(t, s, "FF99", customer.Bech32, 500_000, storage.DepositCredited)
	sec, err = r.Business(ctx)
	require.NoError(t, err)
	require.True(t, sec.UncreditedExternalTotal.IsNegative())
	require.NotEmpty(t, sec.Findings)
	require.Equal(t, 1.0, counterValue(t, m.ReconciliationDiscrepancy.WithLabelValues(testChainID)))
}

// insertDeposit creates a deposit record and walks it to the target status
// through legal transitions.
func insertDeposit(t *testing.T, s storage.Store, txHash, recipient string, amount int64, to storage.DepositStatus) storage.DepositRecord {
	t.Helper()
	ctx := context.Background()
	d, err := s.Deposits().Insert(ctx, storage.DepositRecord{
		ChainID: testChainID, TxHash: txHash, MessageIndex: 0, CoinIndex: 0,
		BlockHeight: 60, BlockTimestamp: testNow,
		RecipientAddress: recipient, Denom: storage.BaseDenom,
		AmountBaseUnits: sdkmath.NewInt(amount),
		Status:          storage.DepositDiscovered,
		CreatedAt:       testNow, UpdatedAt: testNow,
	})
	require.NoError(t, err)
	paths := map[storage.DepositStatus][]storage.DepositStatus{
		storage.DepositBelowMinimum: {storage.DepositValidated, storage.DepositBelowMinimum},
		storage.DepositCredited: {storage.DepositValidated, storage.DepositAwaitingConfirmations,
			storage.DepositCreditable, storage.DepositCredited},
	}
	from := storage.DepositDiscovered
	for _, next := range paths[to] {
		set := storage.DepositUpdate{}
		if next == storage.DepositCredited {
			now := testNow.Add(time.Minute)
			set.CreditedAt = &now
		}
		require.NoError(t, s.Deposits().UpdateStatus(ctx, d.ID, from, next, set))
		from = next
	}
	d.Status = to
	return d
}
