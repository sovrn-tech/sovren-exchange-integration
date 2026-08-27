package reconcile

import (
	"context"
	"testing"

	sdkmath "cosmossdk.io/math"
	"github.com/stretchr/testify/require"

	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/metrics"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

// TestExpectedBalanceIndependentOfCreditStatus pins the §8 formula: every
// successful ledger row counts regardless of classification (uncredited
// inflows — review-parked, fee-funding, internal — never produce false
// discrepancies), failed-tx rows are excluded, FEE_DEDUCTION subtracts.
func TestExpectedBalanceIndependentOfCreditStatus(t *testing.T) {
	s := openTestStore(t)
	customer, _ := seedWatch(t, s)
	addr := customer.Bech32

	appendLedger(t, s, addr, "AA01", 0, 0, 10, storage.DirectionIn, storage.ClassExternalDeposit, 1_000_000, 0)
	appendLedger(t, s, addr, "AA02", 0, 0, 11, storage.DirectionIn, storage.ClassUnattributedReview, 50_000, 0) // review-parked
	appendLedger(t, s, addr, "AA03", 0, 0, 12, storage.DirectionIn, storage.ClassFeeFunding, 20_000, 0)         // internal fee top-up
	appendLedger(t, s, addr, "AA04", 0, 0, 13, storage.DirectionIn, storage.ClassInternalTransfer, 5_000, 0)
	appendLedger(t, s, addr, "AA05", 0, 0, 14, storage.DirectionOut, storage.ClassSweep, 400_000, 0)
	appendLedger(t, s, addr, "AA06", 0, 0, 15, storage.DirectionIn, storage.ClassExternalDeposit, 999_999, 5) // failed tx: excluded
	appendFee(t, s, addr, "AA07", 16, 700, 0)                                                                 // FEE_DEDUCTION

	r := newTestReconciler(t, s, nil)
	pos, err := r.ExpectedBalance(context.Background(), addr)
	require.NoError(t, err)
	// 1_000_000 + 50_000 + 20_000 + 5_000 − 400_000 − 700
	require.Equal(t, sdkmath.NewInt(674_300), pos.Expected)
	require.Equal(t, uint64(10), pos.EarliestHeight)
	require.Equal(t, uint64(16), pos.LatestHeight)
	require.Contains(t, pos.RelatedTxHashes, "AA01")
	require.Contains(t, pos.RelatedTxHashes, "AA07")
	require.NotContains(t, pos.RelatedTxHashes, "AA06") // failed tx not counted
}

// TestReconcileAddressClean: chain balance equal to the ledger expectation —
// no discrepancy, no FR-048 fields.
func TestReconcileAddressClean(t *testing.T) {
	s := openTestStore(t)
	customer, _ := seedWatch(t, s)
	addr := customer.Bech32
	appendLedger(t, s, addr, "BB01", 0, 0, 20, storage.DirectionIn, storage.ClassExternalDeposit, 750_000, 0)

	chain := newFakeChain()
	chain.balances[addr] = sdkmath.NewInt(750_000)
	r := newTestReconciler(t, s, chain)

	entry, err := r.ReconcileAddress(context.Background(), addr)
	require.NoError(t, err)
	require.True(t, entry.Difference.IsZero())
	require.Zero(t, entry.EarliestSuspectedHeight)
	require.Empty(t, entry.RelatedTxHashes)
}

// TestDiscrepancyInjectionFieldCompleteReport injects a balance discrepancy
// and asserts the persisted report carries every FR-048 field, and that the
// zero-tolerance alert counter fires.
func TestDiscrepancyInjectionFieldCompleteReport(t *testing.T) {
	s := openTestStore(t)
	customer, hot := seedWatch(t, s)
	addr := customer.Bech32
	appendLedger(t, s, addr, "CC01", 0, 0, 30, storage.DirectionIn, storage.ClassExternalDeposit, 1_000_000, 0)
	appendLedger(t, s, addr, "CC02", 0, 0, 35, storage.DirectionIn, storage.ClassExternalDeposit, 250_000, 0)

	chain := newFakeChain()
	chain.balances[addr] = sdkmath.NewInt(1_100_000) // 150_000 missing on chain
	chain.balances[hot.Bech32] = sdkmath.ZeroInt()
	m := metrics.NewSet()
	r := newTestReconciler(t, s, chain, WithMetrics(m))

	rep, err := r.Run(context.Background(), storage.ReconManual)
	require.NoError(t, err)
	require.Equal(t, 1, rep.DiscrepancyCount)

	var e *storage.ReconciliationEntry
	for i := range rep.Entries {
		if rep.Entries[i].Address == addr {
			e = &rep.Entries[i]
		}
	}
	require.NotNil(t, e)
	// Every FR-048 field.
	require.Equal(t, addr, e.Address)
	require.Equal(t, sdkmath.NewInt(1_250_000), e.ExpectedBaseUnits)
	require.Equal(t, sdkmath.NewInt(1_100_000), e.ObservedBaseUnits)
	require.Equal(t, sdkmath.NewInt(-150_000), e.Difference)
	require.Equal(t, uint64(30), e.EarliestSuspectedHeight)
	require.ElementsMatch(t, []string{"CC01", "CC02"}, e.RelatedTxHashes)
	require.Equal(t, uint64(30), e.RecommendedRescanHeight)

	// Alert counter fired exactly once.
	require.Equal(t, 1.0, counterValue(t, m.ReconciliationDiscrepancy.WithLabelValues(testChainID)))

	// Report persisted with the entries intact.
	stored, err := s.Recon().GetReport(context.Background(), rep.ReportID)
	require.NoError(t, err)
	require.Equal(t, storage.ReconManual, stored.Kind)
	require.Equal(t, 1, stored.DiscrepancyCount)
	require.Len(t, stored.Entries, len(rep.Entries))
}

// TestWalletHourlyScopesToOperationalWallets: WALLET_HOURLY reconciles the
// hot wallet but not customer deposit addresses.
func TestWalletHourlyScopesToOperationalWallets(t *testing.T) {
	s := openTestStore(t)
	customer, hot := seedWatch(t, s)
	chain := newFakeChain()
	chain.balances[hot.Bech32] = sdkmath.ZeroInt()
	chain.balances[customer.Bech32] = sdkmath.NewInt(999) // would be a discrepancy if scanned
	r := newTestReconciler(t, s, chain)

	rep, err := r.Run(context.Background(), storage.ReconWalletHourly)
	require.NoError(t, err)
	require.Len(t, rep.Entries, 1)
	require.Equal(t, hot.Bech32, rep.Entries[0].Address)
	require.Equal(t, 0, rep.DiscrepancyCount)
}

// TestReconcileTx re-derives a real signed MsgSend from chain truth: a
// matching ledger row reconciles clean; a mismatched amount produces a
// field-complete per-tx discrepancy entry.
func TestReconcileTx(t *testing.T) {
	s := openTestStore(t)
	customer, _ := seedWatch(t, s)
	external := deriveAccount(t, 0)
	chain := newFakeChain()
	r := newTestReconciler(t, s, chain)
	ctx := context.Background()

	// Clean: ledger row matches the on-chain transfer.
	okHash := signedSendTx(t, chain, external, customer.Bech32, "100000", 0, 50)
	appendLedger(t, s, customer.Bech32, okHash, 0, 0, 50, storage.DirectionIn, storage.ClassExternalDeposit, 100_000, 0)
	rep, err := r.ReconcileTx(ctx, okHash, storage.ReconManual)
	require.NoError(t, err)
	require.Equal(t, 0, rep.DiscrepancyCount)
	require.Empty(t, rep.Entries)

	// Mismatch: ledger recorded 99_999 for a 100_000 transfer.
	badHash := signedSendTx(t, chain, external, customer.Bech32, "100000", 1, 60)
	appendLedger(t, s, customer.Bech32, badHash, 0, 0, 60, storage.DirectionIn, storage.ClassExternalDeposit, 99_999, 0)
	rep, err = r.ReconcileTx(ctx, badHash, storage.ReconManual)
	require.NoError(t, err)
	require.Equal(t, 1, rep.DiscrepancyCount)
	require.Len(t, rep.Entries, 1)
	e := rep.Entries[0]
	require.Equal(t, customer.Bech32, e.Address)
	require.Equal(t, sdkmath.NewInt(99_999), e.ExpectedBaseUnits)
	require.Equal(t, sdkmath.NewInt(100_000), e.ObservedBaseUnits)
	require.Equal(t, sdkmath.NewInt(1), e.Difference)
	require.Equal(t, uint64(60), e.EarliestSuspectedHeight)
	require.Equal(t, []string{badHash}, e.RelatedTxHashes)
	require.Equal(t, uint64(60), e.RecommendedRescanHeight)

	// Unknown hash: no chain truth to reconcile against — clean, logged.
	rep, err = r.ReconcileTx(ctx, "DEADBEEF", storage.ReconManual)
	require.NoError(t, err)
	require.Equal(t, 0, rep.DiscrepancyCount)
}

// TestNearRealTimePass walks new ledger entries and persists reports only on
// discrepancy.
func TestNearRealTimePass(t *testing.T) {
	s := openTestStore(t)
	customer, _ := seedWatch(t, s)
	external := deriveAccount(t, 0)
	chain := newFakeChain()
	r := newTestReconciler(t, s, chain)
	ctx := context.Background()

	okHash := signedSendTx(t, chain, external, customer.Bech32, "500000", 0, 70)
	appendLedger(t, s, customer.Bech32, okHash, 0, 0, 70, storage.DirectionIn, storage.ClassExternalDeposit, 500_000, 0)

	cursors := map[string]int64{}
	require.NoError(t, r.NearRealTimePass(ctx, cursors))
	reports, err := s.Recon().ListReports(ctx, testChainID, storage.ReconTxNearRealTime, 10)
	require.NoError(t, err)
	require.Empty(t, reports) // clean pass persists nothing

	// A second pass sees no new entries (cursor advanced).
	require.NoError(t, r.NearRealTimePass(ctx, cursors))

	badHash := signedSendTx(t, chain, external, customer.Bech32, "500000", 1, 71)
	appendLedger(t, s, customer.Bech32, badHash, 0, 0, 71, storage.DirectionIn, storage.ClassExternalDeposit, 400_000, 0)
	require.NoError(t, r.NearRealTimePass(ctx, cursors))
	reports, err = s.Recon().ListReports(ctx, testChainID, storage.ReconTxNearRealTime, 10)
	require.NoError(t, err)
	require.Len(t, reports, 1)
	require.Equal(t, 1, reports[0].DiscrepancyCount)
}

// TestScheduleDefaults pins the contract default cadences.
func TestScheduleDefaults(t *testing.T) {
	s := Schedule{}.withDefaults()
	require.Equal(t, DefaultNearRealTimeInterval, s.NearRealTimeInterval)
	require.Equal(t, DefaultWalletInterval, s.WalletInterval)
	require.Equal(t, DefaultFullAddressInterval, s.FullAddressInterval)
}

// TestRunRejectsNearRealTimeKind: periodic Run serves only the address-scan
// kinds.
func TestRunRejectsNearRealTimeKind(t *testing.T) {
	s := openTestStore(t)
	seedWatch(t, s)
	r := newTestReconciler(t, s, newFakeChain())
	_, err := r.Run(context.Background(), storage.ReconTxNearRealTime)
	require.Error(t, err)
}
