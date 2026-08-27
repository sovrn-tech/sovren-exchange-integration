package reconcile

// Live-chain integration drills (T070 env-gated variants). Gated behind
// SOVREN_LOCAL_CHAIN_RPC — a local chain may not be running while unit
// suites execute.
//
// Environment:
//
//	SOVREN_LOCAL_CHAIN_RPC  CometBFT RPC URL (e.g. http://localhost:26657)

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

func liveClient(t *testing.T) (client.Client, string) {
	t.Helper()
	rpc := os.Getenv("SOVREN_LOCAL_CHAIN_RPC")
	if rpc == "" {
		t.Skip("SOVREN_LOCAL_CHAIN_RPC not set; skipping live-chain reconcile drill")
	}
	c, err := client.NewCometRPC(rpc, client.WithTimeout(10*time.Second))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	status, err := c.NodeStatus(context.Background())
	require.NoError(t, err)
	return c, status.ChainID
}

// TestIntegrationFreshAddressReconcilesClean: an address with no history has
// an empty ledger and a zero balance — the formula agrees with the chain.
func TestIntegrationFreshAddressReconcilesClean(t *testing.T) {
	c, chainID := liveClient(t)
	s := openTestStore(t)
	ctx := context.Background()

	fresh := deriveAccount(t, 42)
	require.NoError(t, s.Watch().Upsert(ctx, storage.WatchedAddress{
		ChainID: chainID, Address: fresh.Bech32,
		Kind: storage.WatchCustomerDeposit, Active: true,
	}))

	r, err := New(s, c, Config{ChainID: chainID})
	require.NoError(t, err)
	entry, err := r.ReconcileAddress(ctx, fresh.Bech32)
	require.NoError(t, err)
	require.True(t, entry.Difference.IsZero(),
		"fresh address must reconcile clean, got difference %s", entry.Difference)

	rep, err := r.Run(ctx, storage.ReconManual)
	require.NoError(t, err)
	require.Equal(t, 0, rep.DiscrepancyCount)
}

// TestIntegrationSameNodeNeverDisagrees: a failover pair of the same
// endpoint must always agree — no chain-review condition may open.
func TestIntegrationSameNodeNeverDisagrees(t *testing.T) {
	rpc := os.Getenv("SOVREN_LOCAL_CHAIN_RPC")
	if rpc == "" {
		t.Skip("SOVREN_LOCAL_CHAIN_RPC not set; skipping live-chain reconcile drill")
	}
	a, err := client.NewCometRPC(rpc, client.WithTimeout(10*time.Second))
	require.NoError(t, err)
	defer a.Close()
	b, err := client.NewCometRPC(rpc, client.WithTimeout(10*time.Second))
	require.NoError(t, err)
	defer b.Close()
	fo := client.NewFailover(a, b, client.FailoverPolicy{})

	ctx := context.Background()
	status, err := a.NodeStatus(ctx)
	require.NoError(t, err)

	s := openTestStore(t)
	mon, err := NewMonitor(fo, s, DisagreementConfig{
		ChainID: status.ChainID, HeightTolerance: 2,
	})
	require.NoError(t, err)
	res, err := mon.Check(ctx)
	require.NoError(t, err)
	require.True(t, res.AllMatch())
	open, err := s.ChainReview().ListOpen(ctx, status.ChainID)
	require.NoError(t, err)
	require.Empty(t, open)
}
