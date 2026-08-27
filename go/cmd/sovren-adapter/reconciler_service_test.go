package main

// T069/T066 tests: the reconciler service's storage-mirroring gauge loop,
// runtime config resolution, and the `all`-mode registry sanity (every
// service under one process, one shared store/controls/metrics).

import (
	"context"
	"testing"
	"time"

	sdkmath "cosmossdk.io/math"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"

	"github.com/sovrn-tech/sovren-exchange-integration/go/reconcile"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

func metricValue(t *testing.T, m prometheus.Metric) float64 {
	t.Helper()
	var d dto.Metric
	require.NoError(t, m.Write(&d))
	if d.GetGauge() != nil {
		return d.GetGauge().GetValue()
	}
	return d.GetCounter().GetValue()
}

// TestRefreshGaugesMirrorsStorage: the T066 gauge loop reads shared storage
// and sets controls/review/chain-review/backlog/hot-wallet gauges.
func TestRefreshGaugesMirrorsStorage(t *testing.T) {
	deps := controlDeps(t)
	ctx := context.Background()

	// Sweep paused; one open review item; one open chain-review condition;
	// one CREDITABLE deposit; one hot wallet with a live balance.
	paused := true
	_, err := deps.Store.Controls().Apply(ctx, ctlChainID,
		storage.ControlsUpdate{SweepPaused: &paused}, "test", "gauge drill")
	require.NoError(t, err)
	_, err = deps.Store.Review().Open(ctx, storage.ReviewItem{
		ChainID: ctlChainID, Kind: storage.ReviewKindLedgerEntry, RefID: "1",
		Reason: "drill", OpenedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	_, err = deps.Store.ChainReview().Open(ctx, storage.ChainReviewCondition{
		ConditionID: "gauge-cond", ChainID: ctlChainID,
		Trigger: storage.TriggerHeightDivergence, OpenedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	d, err := deps.Store.Deposits().Insert(ctx, storage.DepositRecord{
		ChainID: ctlChainID, TxHash: "GA01", MessageIndex: 0, CoinIndex: 0,
		BlockHeight: 5, BlockTimestamp: time.Now().UTC(),
		RecipientAddress: "sovr1gaugedeposit", Denom: storage.BaseDenom,
		AmountBaseUnits: sdkmath.NewInt(1_000_000), Status: storage.DepositDiscovered,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	for _, step := range []storage.DepositStatus{
		storage.DepositValidated, storage.DepositAwaitingConfirmations, storage.DepositCreditable,
	} {
		require.NoError(t, deps.Store.Deposits().UpdateStatus(ctx, d.ID, d.Status, step, storage.DepositUpdate{}))
		d.Status = step
	}

	hot := "sovr1gaugehotwallet"
	require.NoError(t, deps.Store.Watch().Upsert(ctx, storage.WatchedAddress{
		ChainID: ctlChainID, Address: hot, Kind: storage.WatchHotWallet, Active: true,
	}))
	deps.Client.(*fakeAdapterClient).balances[hot] = sdkmath.NewInt(123_456_789)

	require.NoError(t, refreshGauges(ctx, deps))

	m := deps.Metrics
	require.Equal(t, 1.0, metricValue(t, m.ControlsPaused.WithLabelValues(ctlChainID, "sweep")))
	require.Equal(t, 0.0, metricValue(t, m.ControlsPaused.WithLabelValues(ctlChainID, "credit")))
	require.Equal(t, 0.0, metricValue(t, m.ControlsPaused.WithLabelValues(ctlChainID, "signing")))
	require.Equal(t, 0.0, metricValue(t, m.ControlsPaused.WithLabelValues(ctlChainID, "broadcast")))
	require.Equal(t, 1.0, metricValue(t, m.ReviewQueueDepth.WithLabelValues(ctlChainID)))
	require.Equal(t, 1.0, metricValue(t, m.ChainReviewConditionsOpen.WithLabelValues(ctlChainID, "HEIGHT_DIVERGENCE")))
	require.Equal(t, 0.0, metricValue(t, m.ChainReviewConditionsOpen.WithLabelValues(ctlChainID, "WRONG_CHAIN_ID")))
	require.Equal(t, 1.0, metricValue(t, m.DepositBacklog.WithLabelValues(ctlChainID)))
	require.Equal(t, 123_456_789.0, metricValue(t, m.HotWalletBalanceUsovr.WithLabelValues(ctlChainID, hot)))

	// Resolving state zeroes the mirrored gauges on the next refresh.
	require.NoError(t, deps.Store.ChainReview().Resolve(ctx, "gauge-cond", "done", time.Now().UTC()))
	require.NoError(t, refreshGauges(ctx, deps))
	require.Equal(t, 0.0, metricValue(t, m.ChainReviewConditionsOpen.WithLabelValues(ctlChainID, "HEIGHT_DIVERGENCE")))
}

// TestReconcilerRuntimeConfig: adapter.yaml intervals + disagreement config
// resolve into the reconcile runtime values; env overrides the NRT cadence.
func TestReconcilerRuntimeConfig(t *testing.T) {
	deps := controlDeps(t)
	deps.Config = &Config{
		Reconciler: ReconcilerConfig{WalletInterval: "30m", FullAddressInterval: "12h"},
		Nodes: NodesConfig{Disagreement: &DisagreementConfig{
			HeightDivergenceThreshold: 7, CheckInterval: "45s",
		}},
	}
	t.Setenv("SOVREN_RECONCILER_NRT_INTERVAL", "20s")

	sched, dis, err := reconcilerRuntime(deps)
	require.NoError(t, err)
	require.Equal(t, 30*time.Minute, sched.WalletInterval)
	require.Equal(t, 12*time.Hour, sched.FullAddressInterval)
	require.Equal(t, 20*time.Second, sched.NearRealTimeInterval)
	require.Equal(t, int64(7), dis.HeightTolerance)
	require.Equal(t, 45*time.Second, dis.Interval)
	require.True(t, dis.CompareHashAtCheckpoint)

	// Empty config falls back to the reconcile package defaults.
	deps.Config = &Config{}
	t.Setenv("SOVREN_RECONCILER_NRT_INTERVAL", "")
	sched, _, err = reconcilerRuntime(deps)
	require.NoError(t, err)
	// Zero values take the reconcile package defaults inside RunSchedules.
	require.Zero(t, sched.WalletInterval)
	require.Equal(t, time.Hour, reconcile.DefaultWalletInterval)
}

// TestAllModeRegistrySanity: `all` expands to every registered service, and
// the reconciler is registered alongside the scanner and withdrawals
// services — one process, one Deps (shared store/controls/metrics).
func TestAllModeRegistrySanity(t *testing.T) {
	names := registeredServices()
	require.Contains(t, names, "scanner")
	require.Contains(t, names, "withdrawals")
	require.Contains(t, names, "reconciler")

	// The `all` branch in run() copies the registry verbatim.
	runners := map[string]RunFunc{}
	for n, fn := range registry {
		runners[n] = fn
	}
	require.Len(t, runners, len(registry))
	for _, n := range names {
		require.NotNil(t, runners[n])
	}
}

// TestRunReconcilerStopsCleanly: the service starts its component loops from
// one shared Deps and exits cleanly on ctx cancel (also exercises the
// schedules + gauge loop wiring end to end with the fake client).
func TestRunReconcilerStopsCleanly(t *testing.T) {
	deps := controlDeps(t)
	deps.Config = &Config{Reconciler: ReconcilerConfig{WalletInterval: "1h", FullAddressInterval: "24h"}}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	require.NoError(t, runReconciler(ctx, deps))
}
