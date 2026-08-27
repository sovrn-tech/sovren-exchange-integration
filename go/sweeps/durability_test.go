package sweeps

// Durability drills (T062): fee-insufficient deferral without a retry loop,
// duplicate re-runs, crash between sign and broadcast (quarantine + the
// identical-bytes rebroadcast), the unresolved-sweep + new-balance-snapshot
// race (partial-unique constraint blocks a second live sweep), and the
// FR-051 sweep pause.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	sdkmath "cosmossdk.io/math"
	"github.com/stretchr/testify/require"

	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/metrics"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

// scrapeMetrics returns the Prometheus text exposition of a Set.
func scrapeMetrics(t *testing.T, m *metrics.Set) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

// TestFeeInsufficientDefers: a balance at the minimum cannot cover fee +
// reserve on top of a minimum-sized sweep — the job is created, surfaced as
// DEFERRED (counter incremented), and NOT retried: nothing is ever
// broadcast until conditions change and Revisit revives it.
func TestFeeInsufficientDefers(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, nil)
	m := metrics.NewSet()
	h.engine.metrics = m

	// 1_000_000 ≥ minimum, but 1_000_000 − 3250 − 50000 < minimum.
	h.chain.setBalance(h.source, 1_000_000)
	depID := h.creditDeposit(t, "DEP1", 1_000_000)

	report, err := h.engine.Plan(ctx)
	require.NoError(t, err)
	require.Empty(t, report.JobsCreated)
	require.Len(t, report.JobsDeferred, 1)
	sweepID := report.JobsDeferred[0]

	j := h.job(t, sweepID)
	require.Equal(t, storage.SweepDeferred, j.Status)
	require.Contains(t, scrapeMetrics(t, m),
		`sovren_sweeps_deferred_total{chain_id="test-sovr-1"} 1`,
		"deferral surfaced via sovren_sweeps_deferred_total")
	require.Equal(t, storage.DepositSweepPending, h.deposit(t, depID).Status,
		"covered deposits stay earmarked while deferred")

	// No retry loop: revisiting under unchanged conditions does nothing.
	for range 3 {
		revived, err := h.engine.Revisit(ctx, sweepID)
		require.NoError(t, err)
		require.False(t, revived)
	}
	require.Equal(t, storage.SweepDeferred, h.job(t, sweepID).Status)
	require.Equal(t, 0, h.chain.broadcastCount(), "a deferred sweep never broadcasts")
	_, err = h.store.Sequences().GetByWorkRef(ctx, storage.WorkRef{Kind: storage.WorkSweep, ID: sweepID})
	require.ErrorIs(t, err, storage.ErrNotFound, "no sequence bound while deferred from planning")

	// Conditions change (new customer deposit): Revisit revives, and the
	// sweep completes with the ORIGINAL snapshot amount.
	h.chain.setBalance(h.source, 3_000_000)
	revived, err := h.engine.Revisit(ctx, sweepID)
	require.NoError(t, err)
	require.True(t, revived)
	j = h.driveToConfirmed(t, sweepID)
	require.Equal(t, "1000000", j.AmountBaseUnits.String())
	require.Equal(t, storage.DepositSwept, h.deposit(t, depID).Status)
}

// TestDuplicateRerunIdempotent: re-planning the same snapshot while a job
// is live hits the active-sweep gate; re-planning it after the job is
// terminal hits the idempotency key. Either way: one job, one broadcast.
func TestDuplicateRerunIdempotent(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, nil)
	h.chain.setBalance(h.source, 10_000_000_000)
	h.creditDeposit(t, "DEP1", 10_000_000_000)

	sweepID := h.planOne(t)

	// Same snapshot, second run: no second job.
	report, err := h.engine.Plan(ctx)
	require.NoError(t, err)
	require.Empty(t, report.JobsCreated)
	require.Contains(t, report.Held[h.source], "active sweep")

	h.driveToConfirmed(t, sweepID)
	require.Equal(t, 1, h.chain.broadcastCount())

	// Terminal job + unchanged (balance, height) snapshot: the FR-039
	// idempotency key blocks re-execution.
	h.chain.setBalance(h.source, 10_000_000_000)
	report, err = h.engine.Plan(ctx)
	require.NoError(t, err)
	require.Empty(t, report.JobsCreated)
	require.Contains(t, report.Held[h.source], "duplicate")
	require.Equal(t, 1, h.chain.broadcastCount(), "still exactly one broadcast")

	// Prepare/Broadcast/Confirm re-runs on the finished job all refuse.
	require.ErrorIs(t, h.engine.Prepare(ctx, sweepID), storage.ErrStatusConflict)
	require.ErrorIs(t, h.engine.Broadcast(ctx, sweepID), storage.ErrStatusConflict)
	require.ErrorIs(t, h.engine.Confirm(ctx, sweepID), storage.ErrStatusConflict)
}

// TestCrashBetweenSignAndBroadcast: the process dies after SIGNED persists;
// a fresh engine (restart) broadcasts into a transport error, searches,
// quarantines the reservation — and recovery rebroadcasts the EXACT
// persisted bytes. Never a re-sign: the wire sees one byte string only.
func TestCrashBetweenSignAndBroadcast(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, nil)
	h.chain.setBalance(h.source, 10_000_000_000)
	h.creditDeposit(t, "DEP1", 10_000_000_000)

	sweepID := h.planOne(t)
	require.NoError(t, h.engine.Prepare(ctx, sweepID))
	signed := h.job(t, sweepID)
	require.Equal(t, storage.SweepSigned, signed.Status)

	// "Crash": a brand-new engine over the same durable store.
	engine2, err := New(h.store, h.chain, h.seq, nil /* signer never needed again */, defaultConfig(storage.StrategyFeeReserve, h.hot))
	require.NoError(t, err)

	// Broadcast outcome unknown (transport error), tx not findable.
	h.chain.mu.Lock()
	h.chain.broadcastErr = errors.New("connection reset")
	h.chain.mu.Unlock()
	err = engine2.Broadcast(ctx, sweepID)
	require.ErrorIs(t, err, ErrQuarantined)
	require.Equal(t, storage.SweepSigned, h.job(t, sweepID).Status,
		"sweep holds SIGNED — still the single live sweep for the account")
	require.Equal(t, storage.SequenceReconciliationRequired, h.reservation(t, sweepID).Status)

	// Sequence reconciliation must NOT release a signed reservation.
	rep, err := h.seq.ReconcileAccount(ctx, testChainID, h.source)
	require.NoError(t, err)
	require.Zero(t, rep.Released)
	require.Equal(t, storage.SequenceReconciliationRequired, h.reservation(t, sweepID).Status)

	// Recovery: rebroadcast the identical persisted bytes.
	h.chain.mu.Lock()
	h.chain.broadcastErr = nil
	h.chain.mu.Unlock()
	require.NoError(t, engine2.Recover(ctx, sweepID))
	require.Equal(t, storage.SweepBroadcast, h.job(t, sweepID).Status)
	require.Equal(t, 2, h.chain.broadcastCount())
	require.Equal(t, h.chain.broadcastAt(0), h.chain.broadcastAt(1),
		"recovery sends byte-identical transactions — re-signing is impossible")
	require.Equal(t, signed.SignedTxBytes, h.chain.broadcastAt(1))

	h.chain.include(*signed.TxHash, 990, 0, "")
	require.NoError(t, engine2.Confirm(ctx, sweepID))
	require.Equal(t, storage.SweepConfirmed, h.job(t, sweepID).Status)
	require.Equal(t, storage.SequenceConsumed, h.reservation(t, sweepID).Status,
		"quarantined reservation resolves only through observed inclusion")
}

// TestUnresolvedSweepBlocksNewSnapshot pins §7 guarantee 1: while a signed
// sweep is unresolved (quarantined broadcast, unknown outcome), later
// balance snapshots mint fresh idempotency keys — and the non-terminal
// partial-unique constraint still refuses a second live sweep.
func TestUnresolvedSweepBlocksNewSnapshot(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, nil)
	h.chain.setBalance(h.source, 10_000_000_000)
	h.creditDeposit(t, "DEP1", 10_000_000_000)

	sweepID := h.planOne(t)
	require.NoError(t, h.engine.Prepare(ctx, sweepID))
	h.chain.mu.Lock()
	h.chain.broadcastErr = errors.New("timeout")
	h.chain.mu.Unlock()
	require.ErrorIs(t, h.engine.Broadcast(ctx, sweepID), ErrQuarantined)

	// A NEW snapshot arrives: more deposits, a later height.
	h.chain.mu.Lock()
	h.chain.broadcastErr = nil
	h.chain.balances[h.source] = sdkmath.NewInt(25_000_000_000)
	h.chain.latestHeight = 2000
	h.chain.mu.Unlock()

	report, err := h.engine.Plan(ctx)
	require.NoError(t, err)
	require.Empty(t, report.JobsCreated, "no second live sweep while one is unresolved")
	require.Contains(t, report.Held[h.source], "active sweep")

	// Even bypassing the planner, the DB constraint is the guarantee.
	_, err = h.store.Sweeps().Create(ctx, storage.SweepJob{
		SweepID:             "RACER",
		IdempotencyKey:      IdempotencyKey(testChainID, h.source, sdkmath.NewInt(25_000_000_000), 2000),
		ChainID:             testChainID,
		SourceAddress:       h.source,
		HotWalletAddress:    h.hot,
		Strategy:            storage.StrategyFeeReserve,
		AmountBaseUnits:     sdkmath.NewInt(1),
		FeeReserveBaseUnits: sdkmath.ZeroInt(),
	})
	require.ErrorIs(t, err, storage.ErrActiveSweepExists)

	active, err := h.store.Sweeps().GetActive(ctx, testChainID, h.source)
	require.NoError(t, err)
	require.Equal(t, sweepID, active.SweepID)
}

// TestSweepPausedHonored: the FR-051 sweep_paused control stops planning,
// preparation, and broadcast independently; resume restores them.
func TestSweepPausedHonored(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, nil)
	h.chain.setBalance(h.source, 10_000_000_000)

	sweepID := h.planOne(t)

	paused := true
	_, err := h.store.Controls().Apply(ctx, testChainID, storage.ControlsUpdate{SweepPaused: &paused}, "test", "unit")
	require.NoError(t, err)

	_, err = h.engine.Plan(ctx)
	require.ErrorIs(t, err, ErrPaused)
	require.ErrorIs(t, h.engine.Prepare(ctx, sweepID), ErrPaused)
	require.Equal(t, storage.SweepPending, h.job(t, sweepID).Status)
	require.Equal(t, 0, h.chain.broadcastCount())

	unpaused := false
	_, err = h.store.Controls().Apply(ctx, testChainID, storage.ControlsUpdate{SweepPaused: &unpaused}, "test", "unit")
	require.NoError(t, err)
	require.NoError(t, h.engine.Prepare(ctx, sweepID))

	// Pause again between sign and broadcast: broadcast refuses.
	_, err = h.store.Controls().Apply(ctx, testChainID, storage.ControlsUpdate{SweepPaused: &paused}, "test", "unit")
	require.NoError(t, err)
	require.ErrorIs(t, h.engine.Broadcast(ctx, sweepID), ErrPaused)
	require.Equal(t, 0, h.chain.broadcastCount())
}

// TestPassDrivesLifecycle: the service-facing Pass loop advances a sweep
// end to end across iterations without external orchestration.
func TestPassDrivesLifecycle(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, nil)
	h.chain.setBalance(h.source, 10_000_000_000)
	depID := h.creditDeposit(t, "DEP1", 10_000_000_000)

	report := h.engine.Pass(ctx) // plan + prepare
	require.Empty(t, report.Errors)
	require.Len(t, report.Plan.JobsCreated, 1)
	sweepID := report.Plan.JobsCreated[0]

	report = h.engine.Pass(ctx) // broadcast
	require.Empty(t, report.Errors)
	j := h.job(t, sweepID)
	require.Equal(t, storage.SweepBroadcast, j.Status)

	h.chain.include(*j.TxHash, 990, 0, "")
	report = h.engine.Pass(ctx) // confirm
	require.Empty(t, report.Errors)
	require.Equal(t, storage.SweepConfirmed, h.job(t, sweepID).Status)
	require.Equal(t, storage.DepositSwept, h.deposit(t, depID).Status)
}
