package main

// Track A regression tests for PR #300 findings #2 and #4 on the withdrawals
// service. Shared adapter-service test helpers (fake stores, manifests, the
// unsafe-local signer harness) live here and are reused by the sweeper tests.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sovrn-tech/sovren-exchange-integration/go/address"
	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/logging"
	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/metrics"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage/sqlite"
)

// errAdapterProbe marks an injected store/chain failure so a test can assert
// the failure propagated all the way out of service startup.
var errAdapterProbe = errors.New("adapter startup probe failure")

// testAdapterMnemonic is the standard BIP39 zero vector (public test material,
// not a secret) used to wire the unsafe-local signer in service startup tests.
const testAdapterMnemonic = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"

// adapterTestManifest builds a testnet manifest with a populated fees section;
// recGasAdjustment sets recommended_gas_adjustment (empty models a manifest
// that omits it, exercising the require-explicit-value path).
func adapterTestManifest(recGasAdjustment string) *client.NetworkManifest {
	return &client.NetworkManifest{
		ChainID:     ctlChainID,
		NetworkType: "testnet",
		BaseDenom:   storage.BaseDenom,
		Fees: client.ManifestFees{
			RecommendedGasPrice:      "0.025" + storage.BaseDenom,
			RecommendedGasAdjustment: recGasAdjustment,
		},
		Versions: client.ManifestVersions{App: "v0.16.2", SDK: "v0.53.6"},
	}
}

func adapterFreshStore(t *testing.T) storage.Store {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "adapter-src.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// setUnsafeLocalSigner wires the environment the unsafe-local signer needs so
// runWithdrawalsService can build its signer and reach startup reconciliation.
func setUnsafeLocalSigner(t *testing.T) {
	t.Helper()
	t.Setenv("SOVREN_SIGNER_UNSAFE", "UNSAFE_TEST_ONLY")
	t.Setenv("SOVREN_SIGNER_MNEMONIC", testAdapterMnemonic)
}

// stubSourceStore embeds a real store and swaps in per-repo test doubles for
// the three repositories the source-discovery scans touch. A nil field
// delegates to the embedded store.
type stubSourceStore struct {
	storage.Store
	withdrawals storage.WithdrawalRepo
	sweeps      storage.SweepRepo
	watch       storage.WatchRepo
}

func (s stubSourceStore) Withdrawals() storage.WithdrawalRepo {
	if s.withdrawals != nil {
		return s.withdrawals
	}
	return s.Store.Withdrawals()
}

func (s stubSourceStore) Sweeps() storage.SweepRepo {
	if s.sweeps != nil {
		return s.sweeps
	}
	return s.Store.Sweeps()
}

func (s stubSourceStore) Watch() storage.WatchRepo {
	if s.watch != nil {
		return s.watch
	}
	return s.Store.Watch()
}

// errWithdrawalRepo fails ListByStatus with a fixed error.
type errWithdrawalRepo struct {
	storage.WithdrawalRepo
	err error
}

func (r errWithdrawalRepo) ListByStatus(context.Context, string, storage.WithdrawalStatus, int) ([]storage.WithdrawalRecord, error) {
	return nil, r.err
}

// errWatchRepo fails ListActive with a fixed error (shared with sweeper tests).
type errWatchRepo struct {
	storage.WatchRepo
	err error
}

func (r errWatchRepo) ListActive(context.Context, string) ([]storage.WatchedAddress, error) {
	return nil, r.err
}

// pagedWithdrawalRepo returns synthetic sources for one status, honoring the
// limit so a caller that stops at the first page is caught dropping records.
type pagedWithdrawalRepo struct {
	storage.WithdrawalRepo
	onStatus storage.WithdrawalStatus
	sources  []string
}

func (r pagedWithdrawalRepo) ListByStatus(_ context.Context, _ string, status storage.WithdrawalStatus, limit int) ([]storage.WithdrawalRecord, error) {
	if status != r.onStatus {
		return nil, nil
	}
	recs := make([]storage.WithdrawalRecord, 0, len(r.sources))
	for _, s := range r.sources {
		recs = append(recs, storage.WithdrawalRecord{SourceAddress: s})
	}
	if limit > 0 && len(recs) > limit {
		recs = recs[:limit]
	}
	return recs, nil
}

// accountFailClient wraps the shared fake client and fails Account for a chosen
// address (or every address when failFor is empty), driving ReconcileAccount to
// error for that source.
type accountFailClient struct {
	*fakeAdapterClient
	failFor string
}

var _ client.Client = (*accountFailClient)(nil)

func (c *accountFailClient) Account(ctx context.Context, addr string) (uint64, uint64, error) {
	if c.failFor == "" || addr == c.failFor {
		return 0, 0, errAdapterProbe
	}
	return c.fakeAdapterClient.Account(ctx, addr)
}

// adapterRunDeps builds Deps wired for a real runWithdrawalsService startup:
// unsafe-local signer config, a valid withdrawals section, and the fees
// manifest. gasAdj sets withdrawals.gas_adjustment.
func adapterRunDeps(store storage.Store, cl client.Client, gasAdj string) *Deps {
	return &Deps{
		Store:   store,
		Client:  cl,
		Metrics: metrics.NewSet(),
		Logger:  logging.New("test"),
		Config: &Config{
			Signer: SignerConfig{Kind: "unsafe-local"},
			Withdrawals: WithdrawalsConfig{
				MinimumWithdrawalUsovr: "1000",
				MaxFeeUsovr:            "500000",
				GasAdjustment:          gasAdj,
			},
		},
		Manifest: adapterTestManifest("1.5"),
	}
}

// --- Finding #4: gas adjustment fallback ------------------------------------

// TestWithdrawalsGasAdjustmentFallsBackToManifest: an empty adapter
// gas_adjustment resolves to the manifest's recommended_gas_adjustment (1.5),
// never the retired 1.3 constant.
func TestWithdrawalsGasAdjustmentFallsBackToManifest(t *testing.T) {
	deps := &Deps{
		Config: &Config{Withdrawals: WithdrawalsConfig{
			MinimumWithdrawalUsovr: "1000", MaxFeeUsovr: "500000",
			// gas_adjustment intentionally empty
		}},
		Manifest: adapterTestManifest("1.5"),
	}
	cfg, err := withdrawalsWorkflowConfig(deps)
	require.NoError(t, err)
	require.Equal(t, "1.5", cfg.GasAdjustment)
	require.NotEqual(t, "1.3", cfg.GasAdjustment)
}

// TestWithdrawalsGasAdjustmentExplicitWins: a configured value overrides the
// manifest recommendation.
func TestWithdrawalsGasAdjustmentExplicitWins(t *testing.T) {
	deps := &Deps{
		Config: &Config{Withdrawals: WithdrawalsConfig{
			MinimumWithdrawalUsovr: "1000", MaxFeeUsovr: "500000",
			GasAdjustment: "1.7",
		}},
		Manifest: adapterTestManifest("1.5"),
	}
	cfg, err := withdrawalsWorkflowConfig(deps)
	require.NoError(t, err)
	require.Equal(t, "1.7", cfg.GasAdjustment)
}

// TestWithdrawalsGasAdjustmentEmptyEverywhereErrors: empty config AND empty
// manifest recommendation is a configuration error — never a silent default.
func TestWithdrawalsGasAdjustmentEmptyEverywhereErrors(t *testing.T) {
	deps := &Deps{
		Config: &Config{Withdrawals: WithdrawalsConfig{
			MinimumWithdrawalUsovr: "1000", MaxFeeUsovr: "500000",
		}},
		Manifest: adapterTestManifest(""),
	}
	_, err := withdrawalsWorkflowConfig(deps)
	require.Error(t, err)
	require.Contains(t, err.Error(), "gas_adjustment")
}

// --- Finding #2: complete discovery + fail-closed startup -------------------

// TestWithdrawalsSourceDiscoveryPropagatesListError: a ListByStatus error is
// surfaced, never swallowed into an empty "already reconciled" set.
func TestWithdrawalsSourceDiscoveryPropagatesListError(t *testing.T) {
	deps := &Deps{
		Store:    stubSourceStore{Store: adapterFreshStore(t), withdrawals: errWithdrawalRepo{err: errAdapterProbe}},
		Manifest: adapterTestManifest("1.5"),
	}
	_, err := withdrawalsSourceAddresses(context.Background(), deps)
	require.ErrorIs(t, err, errAdapterProbe)
}

// TestWithdrawalsSourceDiscoveryPropagatesWatchError: a Watch().ListActive
// error is surfaced.
func TestWithdrawalsSourceDiscoveryPropagatesWatchError(t *testing.T) {
	deps := &Deps{
		Store:    stubSourceStore{Store: adapterFreshStore(t), watch: errWatchRepo{err: errAdapterProbe}},
		Manifest: adapterTestManifest("1.5"),
	}
	_, err := withdrawalsSourceAddresses(context.Background(), deps)
	require.ErrorIs(t, err, errAdapterProbe)
}

// TestWithdrawalsSourceDiscoveryPaginatesBeyondPageSize: more than one page of
// in-flight sources is discovered completely (the pre-fix single 500-cap query
// would have dropped the tail).
func TestWithdrawalsSourceDiscoveryPaginatesBeyondPageSize(t *testing.T) {
	const n = sourceReconcilePageSize + 37
	sources := make([]string, n)
	for i := range sources {
		sources[i] = fmt.Sprintf("sovr1src%06d", i)
	}
	deps := &Deps{
		Store: stubSourceStore{
			Store:       adapterFreshStore(t),
			withdrawals: pagedWithdrawalRepo{onStatus: storage.WithdrawalSequenceReserved, sources: sources},
		},
		Manifest: adapterTestManifest("1.5"),
	}
	out, err := withdrawalsSourceAddresses(context.Background(), deps)
	require.NoError(t, err)
	require.Len(t, out, n)
}

// TestRunWithdrawalsFailsStartupOnDiscoveryError: a store error during source
// discovery fails startup (the RunFunc returns) rather than proceeding to hand
// out sequences.
func TestRunWithdrawalsFailsStartupOnDiscoveryError(t *testing.T) {
	setUnsafeLocalSigner(t)
	store := stubSourceStore{Store: adapterFreshStore(t), withdrawals: errWithdrawalRepo{err: errAdapterProbe}}
	deps := adapterRunDeps(store, newFakeAdapterClient(ctlChainID), "1.5")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := runWithdrawalsService(ctx, deps)
	require.ErrorIs(t, err, errAdapterProbe)
}

// TestRunWithdrawalsFailsStartupOnReconcileError: a ReconcileAccount failure
// for an in-flight source fails startup — no worker runs, so no new sequence
// issues for an unverified account (FR-034).
func TestRunWithdrawalsFailsStartupOnReconcileError(t *testing.T) {
	setUnsafeLocalSigner(t)
	store := adapterFreshStore(t)
	ctx := context.Background()
	hot := "sovr1hotwalletreconcileprobe"
	require.NoError(t, store.Watch().Upsert(ctx, storage.WatchedAddress{
		ChainID: ctlChainID, Address: hot, Kind: storage.WatchHotWallet, Active: true,
	}))
	cl := &accountFailClient{fakeAdapterClient: newFakeAdapterClient(ctlChainID), failFor: hot}
	deps := adapterRunDeps(store, cl, "1.5")
	runCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	err := runWithdrawalsService(runCtx, deps)
	require.ErrorIs(t, err, errAdapterProbe)
}

// --- Withdrawal-destination blocklist (PR #300 review) ----------------------

// The config builder must seed the default core-module-account set so a
// module account can never be a withdrawal destination even with an empty
// exchange blocklist.
func TestWithdrawalsProhibitedSeedsModuleAccounts(t *testing.T) {
	deps := &Deps{
		Config:   &Config{Withdrawals: WithdrawalsConfig{MinimumWithdrawalUsovr: "1000", MaxFeeUsovr: "500000", GasAdjustment: "1.5"}},
		Manifest: adapterTestManifest("1.5"),
	}
	cfg, err := withdrawalsWorkflowConfig(deps)
	require.NoError(t, err)
	def := address.DefaultProhibitedModuleAccounts()
	require.NotEmpty(t, def)
	got := map[string]struct{}{}
	for _, d := range cfg.ProhibitedDestinations {
		got[d] = struct{}{}
	}
	for a := range def {
		require.Contains(t, got, a, "default module account %s must be in the prohibited set", a)
	}
}

// A configured exchange blocklist entry is validated, normalized, and merged
// with the defaults; an invalid entry is a configuration error.
func TestWithdrawalsProhibitedMergesAndValidatesConfig(t *testing.T) {
	// A valid, non-module sovr account address stands in for an
	// exchange-specific entry: a plain derived account (index 9), which is
	// genuinely outside the default 32-account module set — unlike a module
	// account such as "transfer", which is now in the defaults.
	derived, err := address.DeriveAddress(testAdapterMnemonic, "m/44'/118'/0'/0/9")
	require.NoError(t, err)
	blocked := derived.Bech32
	deps := &Deps{
		Config: &Config{Withdrawals: WithdrawalsConfig{
			MinimumWithdrawalUsovr: "1000", MaxFeeUsovr: "500000", GasAdjustment: "1.5",
			ProhibitedDestinations: []string{blocked},
		}},
		Manifest: adapterTestManifest("1.5"),
	}
	cfg, err := withdrawalsWorkflowConfig(deps)
	require.NoError(t, err)
	require.Contains(t, cfg.ProhibitedDestinations, blocked)
	require.Greater(t, len(cfg.ProhibitedDestinations), len(address.DefaultProhibitedModuleAccounts()))

	// invalid entry → error naming the field
	bad := &Deps{
		Config: &Config{Withdrawals: WithdrawalsConfig{
			MinimumWithdrawalUsovr: "1000", MaxFeeUsovr: "500000", GasAdjustment: "1.5",
			ProhibitedDestinations: []string{"cosmos1notsovr"},
		}},
		Manifest: adapterTestManifest("1.5"),
	}
	_, err = withdrawalsWorkflowConfig(bad)
	require.Error(t, err)
	require.Contains(t, err.Error(), "prohibited_destinations")
}
