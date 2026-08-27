package reconcile

import (
	"context"
	"testing"

	sdkmath "cosmossdk.io/math"
	"github.com/stretchr/testify/require"

	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
	"github.com/sovrn-tech/sovren-exchange-integration/go/deposits"
	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/metrics"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

// fakeComparer returns a scripted sequence of compare results.
type fakeComparer struct {
	results []*client.CompareResult
	calls   int
}

func (f *fakeComparer) Compare(_ context.Context, _ client.CompareRequest) (*client.CompareResult, error) {
	i := f.calls
	if i >= len(f.results) {
		i = len(f.results) - 1
	}
	f.calls++
	return f.results[i], nil
}

func matchResult() *client.CompareResult {
	return &client.CompareResult{Items: []client.CompareItem{
		{Kind: client.CompareHeight, Match: true, Primary: "100", Secondary: "100"},
	}}
}

func mismatchResult(kind client.CompareKind) *client.CompareResult {
	return &client.CompareResult{Items: []client.CompareItem{
		{Kind: kind, Match: false, Primary: "100", Secondary: "42"},
	}}
}

func newTestMonitor(t *testing.T, s storage.Store, cmp Comparer, m *metrics.Set) *Monitor {
	t.Helper()
	mon, err := NewMonitor(cmp, s, DisagreementConfig{ChainID: testChainID}, WithMonitorMetrics(m))
	require.NoError(t, err)
	return mon
}

// TestDisagreementOpensConditionAndGatesCrediting simulates a two-node
// disagreement: the Compare mismatch opens a ChainReviewCondition, the
// FR-023 crediting gate closes (EvaluateCreditConditions holds), and the
// open-conditions gauge fires the NodesDisagree alert input.
func TestDisagreementOpensConditionAndGatesCrediting(t *testing.T) {
	s := openTestStore(t)
	seedWatch(t, s)
	ctx := context.Background()
	m := metrics.NewSet()
	cmp := &fakeComparer{results: []*client.CompareResult{mismatchResult(client.CompareHeight)}}
	mon := newTestMonitor(t, s, cmp, m)

	res, err := mon.Check(ctx)
	require.NoError(t, err)
	require.False(t, res.AllMatch())

	open, err := s.ChainReview().ListOpen(ctx, testChainID)
	require.NoError(t, err)
	require.Len(t, open, 1)
	require.Equal(t, storage.TriggerHeightDivergence, open[0].Trigger)
	require.Equal(t, "primary", open[0].NodeA.Endpoint)
	require.Equal(t, "100", open[0].NodeA.Value)
	require.Equal(t, "42", open[0].NodeB.Value)

	// Gauge input for the NodesDisagree / WrongChainID alerts.
	require.Equal(t, 1.0, gaugeValue(t,
		m.ChainReviewConditionsOpen.WithLabelValues(testChainID, string(storage.TriggerHeightDivergence))))

	// Crediting gate is closed: a CREDITABLE deposit holds, never credits.
	gate, err := deposits.LoadCreditGate(ctx, s, testChainID)
	require.NoError(t, err)
	require.True(t, gate.ChainReviewOpen)
	d := storage.DepositRecord{
		ChainID: testChainID, TxHash: "GG01", Denom: storage.BaseDenom,
		AmountBaseUnits: sdkmath.NewInt(1_000_000), BlockHeight: 10,
		Status: storage.DepositCreditable,
	}
	decision, reason := deposits.EvaluateCreditConditions(d, 100, 3, gate)
	require.Equal(t, deposits.DecisionHold, decision)
	require.Contains(t, reason, "chain-review")

	// A second mismatched check does not open a duplicate condition.
	_, err = mon.Check(ctx)
	require.NoError(t, err)
	open, err = s.ChainReview().ListOpen(ctx, testChainID)
	require.NoError(t, err)
	require.Len(t, open, 1)
}

// TestDisagreementAutoResolvesTransientTriggers: sustained agreement
// auto-resolves height-divergence conditions and reopens the crediting gate;
// BLOCK_HASH_MISMATCH stays open for the operator.
func TestDisagreementAutoResolvesTransientTriggers(t *testing.T) {
	s := openTestStore(t)
	seedWatch(t, s)
	ctx := context.Background()
	m := metrics.NewSet()
	cmp := &fakeComparer{results: []*client.CompareResult{
		mismatchResult(client.CompareHeight),
		matchResult(), matchResult(), matchResult(), matchResult(),
	}}
	mon := newTestMonitor(t, s, cmp, m)

	for i := 0; i < 5; i++ {
		_, err := mon.Check(ctx)
		require.NoError(t, err)
	}
	open, err := s.ChainReview().ListOpen(ctx, testChainID)
	require.NoError(t, err)
	require.Empty(t, open, "transient height divergence should auto-resolve")
	require.Equal(t, 0.0, gaugeValue(t,
		m.ChainReviewConditionsOpen.WithLabelValues(testChainID, string(storage.TriggerHeightDivergence))))
	gate, err := deposits.LoadCreditGate(ctx, s, testChainID)
	require.NoError(t, err)
	require.False(t, gate.ChainReviewOpen)

	// Hash mismatch never auto-resolves.
	cmp2 := &fakeComparer{results: []*client.CompareResult{
		mismatchResult(client.CompareBlockHash),
		matchResult(), matchResult(), matchResult(), matchResult(),
	}}
	mon2 := newTestMonitor(t, s, cmp2, m)
	for i := 0; i < 5; i++ {
		_, err := mon2.Check(ctx)
		require.NoError(t, err)
	}
	open, err = s.ChainReview().ListOpen(ctx, testChainID)
	require.NoError(t, err)
	require.Len(t, open, 1)
	require.Equal(t, storage.TriggerBlockHashMismatch, open[0].Trigger)
}

// TestTriggerMapping pins the CompareKind → FR-044 trigger taxonomy.
func TestTriggerMapping(t *testing.T) {
	require.Equal(t, storage.TriggerHeightDivergence, triggerFor(client.CompareHeight))
	require.Equal(t, storage.TriggerBlockHashMismatch, triggerFor(client.CompareBlockHash))
	require.Equal(t, storage.TriggerQueryResultMismatch, triggerFor(client.CompareTxResult))
	require.Equal(t, storage.TriggerQueryResultMismatch, triggerFor(client.CompareAccountSequence))
	require.Equal(t, storage.TriggerQueryResultMismatch, triggerFor(client.CompareBalance))
}
