package main

// Track A regression tests for PR #300 findings #2 and #4 on the sweeper
// service. Shared helpers (stubSourceStore, adapterTestManifest,
// accountFailClient, errAdapterProbe, ...) come from withdrawals_service_test.go.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/logging"
	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/metrics"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

// errSweepRepo fails ListByStatus with a fixed error.
type errSweepRepo struct {
	storage.SweepRepo
	err error
}

func (r errSweepRepo) ListByStatus(context.Context, string, storage.SweepStatus, int) ([]storage.SweepJob, error) {
	return nil, r.err
}

// pagedSweepRepo returns synthetic sources for one status, honoring the limit.
type pagedSweepRepo struct {
	storage.SweepRepo
	onStatus storage.SweepStatus
	sources  []string
}

func (r pagedSweepRepo) ListByStatus(_ context.Context, _ string, status storage.SweepStatus, limit int) ([]storage.SweepJob, error) {
	if status != r.onStatus {
		return nil, nil
	}
	jobs := make([]storage.SweepJob, 0, len(r.sources))
	for _, s := range r.sources {
		jobs = append(jobs, storage.SweepJob{SourceAddress: s})
	}
	if limit > 0 && len(jobs) > limit {
		jobs = jobs[:limit]
	}
	return jobs, nil
}

// sweeperRunDeps builds Deps for a real runSweeperService startup under the
// CUSTODY_ABSTRACTED strategy (no signer, no gas resolution needed) so the
// startup discovery/reconcile paths can be exercised in isolation.
func sweeperRunDeps(store storage.Store, cl client.Client) *Deps {
	return &Deps{
		Store:   store,
		Client:  cl,
		Metrics: metrics.NewSet(),
		Logger:  logging.New("test"),
		Config: &Config{
			Sweeps: SweepsConfig{
				Strategy:                     string(storage.StrategyCustodyAbstract),
				MinimumSweepAmountUsovr:      "1000",
				MaximumFeePercentageForSweep: "5",
				FeeReserveUsovr:              "0",
			},
		},
		Manifest: adapterTestManifest("1.5"),
	}
}

// --- Finding #4: gas adjustment fallback ------------------------------------

// TestSweeperGasAdjustmentFallsBackToManifest: an empty gas_adjustment resolves
// to the manifest's recommended_gas_adjustment (1.5), never the retired 1.3
// constant that had starved live sweeps of gas.
func TestSweeperGasAdjustmentFallsBackToManifest(t *testing.T) {
	deps := &Deps{
		Config: &Config{Sweeps: SweepsConfig{
			Strategy:                     string(storage.StrategyFeeReserve),
			MinimumSweepAmountUsovr:      "1000",
			MaximumFeePercentageForSweep: "5",
			FeeReserveUsovr:              "0",
			HotWallet:                    "sovr1hotwalletsweeps",
		}},
		Manifest: adapterTestManifest("1.5"),
	}
	cfg, err := sweeperEngineConfig(deps, storage.StrategyFeeReserve)
	require.NoError(t, err)
	require.Equal(t, "1.5", cfg.GasAdjustment)
	require.NotEqual(t, "1.3", cfg.GasAdjustment)
}

// TestSweeperGasAdjustmentExplicitWins: a configured value (shared withdrawals
// section) overrides the manifest recommendation.
func TestSweeperGasAdjustmentExplicitWins(t *testing.T) {
	deps := &Deps{
		Config: &Config{
			Withdrawals: WithdrawalsConfig{GasAdjustment: "1.8"},
			Sweeps: SweepsConfig{
				Strategy:                     string(storage.StrategyFeeReserve),
				MinimumSweepAmountUsovr:      "1000",
				MaximumFeePercentageForSweep: "5",
				FeeReserveUsovr:              "0",
				HotWallet:                    "sovr1hotwalletsweeps",
			},
		},
		Manifest: adapterTestManifest("1.5"),
	}
	cfg, err := sweeperEngineConfig(deps, storage.StrategyFeeReserve)
	require.NoError(t, err)
	require.Equal(t, "1.8", cfg.GasAdjustment)
}

// TestSweeperGasAdjustmentEmptyEverywhereErrors: empty config AND empty manifest
// recommendation is a configuration error.
func TestSweeperGasAdjustmentEmptyEverywhereErrors(t *testing.T) {
	deps := &Deps{
		Config: &Config{Sweeps: SweepsConfig{
			Strategy:                     string(storage.StrategyFeeReserve),
			MinimumSweepAmountUsovr:      "1000",
			MaximumFeePercentageForSweep: "5",
			FeeReserveUsovr:              "0",
			HotWallet:                    "sovr1hotwalletsweeps",
		}},
		Manifest: adapterTestManifest(""),
	}
	_, err := sweeperEngineConfig(deps, storage.StrategyFeeReserve)
	require.Error(t, err)
	require.Contains(t, err.Error(), "gas_adjustment")
}

// --- Finding #2: complete discovery + fail-closed startup -------------------

// TestSweeperSourceDiscoveryPropagatesListError: a Sweeps().ListByStatus error
// is surfaced, never swallowed.
func TestSweeperSourceDiscoveryPropagatesListError(t *testing.T) {
	deps := &Deps{
		Store:    stubSourceStore{Store: adapterFreshStore(t), sweeps: errSweepRepo{err: errAdapterProbe}},
		Manifest: adapterTestManifest("1.5"),
	}
	_, err := sweeperSourceAddresses(context.Background(), deps)
	require.ErrorIs(t, err, errAdapterProbe)
}

// TestSweeperSourceDiscoveryPropagatesWatchError: a Watch().ListActive error is
// surfaced.
func TestSweeperSourceDiscoveryPropagatesWatchError(t *testing.T) {
	deps := &Deps{
		Store:    stubSourceStore{Store: adapterFreshStore(t), watch: errWatchRepo{err: errAdapterProbe}},
		Manifest: adapterTestManifest("1.5"),
	}
	_, err := sweeperSourceAddresses(context.Background(), deps)
	require.ErrorIs(t, err, errAdapterProbe)
}

// TestSweeperSourceDiscoveryPaginatesBeyondPageSize: more than one page of
// in-flight sweep sources is discovered completely.
func TestSweeperSourceDiscoveryPaginatesBeyondPageSize(t *testing.T) {
	const n = sourceReconcilePageSize + 23
	sources := make([]string, n)
	for i := range sources {
		sources[i] = fmt.Sprintf("sovr1swp%06d", i)
	}
	deps := &Deps{
		Store: stubSourceStore{
			Store:  adapterFreshStore(t),
			sweeps: pagedSweepRepo{onStatus: storage.SweepPending, sources: sources},
		},
		Manifest: adapterTestManifest("1.5"),
	}
	out, err := sweeperSourceAddresses(context.Background(), deps)
	require.NoError(t, err)
	require.Len(t, out, n)
}

// TestRunSweeperFailsStartupOnDiscoveryError: a store error during source
// discovery fails startup rather than proceeding to the sweep loop.
func TestRunSweeperFailsStartupOnDiscoveryError(t *testing.T) {
	store := stubSourceStore{Store: adapterFreshStore(t), sweeps: errSweepRepo{err: errAdapterProbe}}
	deps := sweeperRunDeps(store, newFakeAdapterClient(ctlChainID))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := runSweeperService(ctx, deps)
	require.ErrorIs(t, err, errAdapterProbe)
}

// TestRunSweeperFailsStartupOnReconcileError: a ReconcileAccount failure for an
// in-flight source (here the fee wallet) fails startup — the sweeps engine's
// Pass loop has no per-account filter, so the safe response is to refuse to
// start the worker (FR-034 / data model §6).
func TestRunSweeperFailsStartupOnReconcileError(t *testing.T) {
	store := adapterFreshStore(t)
	ctx := context.Background()
	feeWallet := "sovr1feewalletreconcileprobe"
	require.NoError(t, store.Watch().Upsert(ctx, storage.WatchedAddress{
		ChainID: ctlChainID, Address: feeWallet, Kind: storage.WatchFeeWallet, Active: true,
	}))
	cl := &accountFailClient{fakeAdapterClient: newFakeAdapterClient(ctlChainID), failFor: feeWallet}
	deps := sweeperRunDeps(store, cl)
	runCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	err := runSweeperService(runCtx, deps)
	require.ErrorIs(t, err, errAdapterProbe)
}
