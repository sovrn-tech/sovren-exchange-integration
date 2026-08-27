package main

// T064/T070 admin API tests: the full contract surface
// (contracts/adapter-config-and-ops.md), with the certification assertion
// that every state change is effective before the response returns and
// visible in GET /v1/status.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	bankv1beta1 "cosmossdk.io/api/cosmos/bank/v1beta1"
	sdkmath "cosmossdk.io/math"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/query"
	"github.com/stretchr/testify/require"

	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
	globalfeev1 "github.com/sovrn-tech/sovren-exchange-integration/go/gen/sovr/globalfee/v1"
	txqueryv1 "github.com/sovrn-tech/sovren-exchange-integration/go/gen/sovr/txquery/v1"
	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/logging"
	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/metrics"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage/sqlite"
)

const ctlChainID = "sovr-local-dev"

// fakeAdapterClient implements the full client.Client for admin/reconciler
// tests: canned balances and transactions, always-accepting broadcast.
type fakeAdapterClient struct {
	chainID  string
	latest   int64
	balances map[string]sdkmath.Int
	txs      map[string]*client.TxInfo
}

func newFakeAdapterClient(chainID string) *fakeAdapterClient {
	return &fakeAdapterClient{
		chainID: chainID, latest: 100,
		balances: map[string]sdkmath.Int{},
		txs:      map[string]*client.TxInfo{},
	}
}

var _ client.Client = (*fakeAdapterClient)(nil)

func (f *fakeAdapterClient) Account(context.Context, string) (uint64, uint64, error) {
	return 7, 0, nil
}

func (f *fakeAdapterClient) Balance(_ context.Context, addr, denom string) (sdkmath.Int, error) {
	if denom != storage.BaseDenom {
		return sdkmath.ZeroInt(), nil
	}
	if b, ok := f.balances[addr]; ok {
		return b, nil
	}
	return sdkmath.ZeroInt(), nil
}

func (f *fakeAdapterClient) AllBalances(context.Context, string) (sdk.Coins, error) { return nil, nil }

func (f *fakeAdapterClient) DenomMetadata(context.Context, string) (*bankv1beta1.Metadata, error) {
	return nil, client.ErrNotFound
}

func (f *fakeAdapterClient) Tx(_ context.Context, hash string) (*client.TxInfo, error) {
	if info, ok := f.txs[hash]; ok {
		return info, nil
	}
	return nil, client.ErrNotFound
}

func (f *fakeAdapterClient) BlockByHeight(_ context.Context, height int64) (*client.Block, error) {
	return &client.Block{ChainID: f.chainID, Height: height}, nil
}

func (f *fakeAdapterClient) LatestBlock(context.Context) (*client.Block, error) {
	return &client.Block{ChainID: f.chainID, Height: f.latest}, nil
}

func (f *fakeAdapterClient) BlockResults(_ context.Context, height int64) (*client.BlockResults, error) {
	return &client.BlockResults{Height: height}, nil
}

func (f *fakeAdapterClient) NodeStatus(context.Context) (*client.NodeStatus, error) {
	return &client.NodeStatus{ChainID: f.chainID, LatestHeight: f.latest}, nil
}

func (f *fakeAdapterClient) Simulate(context.Context, []byte) (*client.SimulateResult, error) {
	return &client.SimulateResult{GasWanted: 100000, GasUsed: 80000}, nil
}

func (f *fakeAdapterClient) Broadcast(_ context.Context, txBytes []byte, _ client.BroadcastMode) (*client.BroadcastResult, error) {
	return &client.BroadcastResult{TxHash: "FA" + fmt.Sprintf("%X", len(txBytes)), Accepted: true}, nil
}

func (f *fakeAdapterClient) GlobalFeeParams(context.Context) (*globalfeev1.Params, error) {
	return nil, client.ErrUnsupported
}

func (f *fakeAdapterClient) TxsByAddress(context.Context, string, *query.PageRequest, ...client.TxsByAddressOptions) (*txqueryv1.GetTxsByAddressResponse, error) {
	return nil, client.ErrUnsupported
}

func (f *fakeAdapterClient) Probe(context.Context) (client.ProbeResult, error) {
	return client.ProbeResult{NodeReachable: true, TxServiceRoutable: true}, nil
}

func (f *fakeAdapterClient) Close() error { return nil }

// controlDeps builds Deps with a real sqlite store, fake client, metrics and
// logger (the T064/T069/T070 test fixture).
func controlDeps(t *testing.T) *Deps {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "controls-test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return &Deps{
		Store:   store,
		Client:  newFakeAdapterClient(ctlChainID),
		Metrics: metrics.NewSet(),
		Logger:  logging.New("test"),
		Config:  &Config{},
		Manifest: &client.NetworkManifest{
			ChainID:     ctlChainID,
			NetworkType: "testnet",
			Versions:    client.ManifestVersions{App: "v0.16.2", SDK: "v0.53.6"},
		},
	}
}

func postJSON(t *testing.T, mux http.Handler, path string, body any, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func adminStatus(t *testing.T, mux http.Handler) statusResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/status", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp statusResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

// TestAdminPauseResumePerFlow drives every flow through pause and resume:
// each POST pauses exactly its flow, the change is visible in GET /v1/status
// after the response returns, and every flip lands in the audit table with
// the caller's actor.
func TestAdminPauseResumePerFlow(t *testing.T) {
	deps := controlDeps(t)
	mux := adminMux(deps)
	ctx := context.Background()

	flowPaused := func(s statusResponse, flow storage.ControlFlow) bool {
		switch flow {
		case storage.FlowCredit:
			return s.Controls.CreditPaused
		case storage.FlowSigning:
			return s.Controls.SigningPaused
		case storage.FlowBroadcast:
			return s.Controls.BroadcastPaused
		case storage.FlowSweep:
			return s.Controls.SweepPaused
		}
		return false
	}

	for _, flow := range storage.AllControlFlows {
		rec := postJSON(t, mux, "/v1/controls/pause",
			controlsRequest{Flow: string(flow), Reason: "drill"}, "X-Actor", "ops-alice")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		st := adminStatus(t, mux)
		require.True(t, flowPaused(st, flow), "flow %s must be paused", flow)
		for _, other := range storage.AllControlFlows {
			if other != flow {
				require.False(t, flowPaused(st, other), "pausing %s must not pause %s", flow, other)
			}
		}

		rec = postJSON(t, mux, "/v1/controls/resume",
			controlsRequest{Flow: string(flow)}, "X-Actor", "ops-alice")
		require.Equal(t, http.StatusOK, rec.Code)
		require.False(t, flowPaused(adminStatus(t, mux), flow))
	}

	audit, err := deps.Store.Controls().ListAudit(ctx, ctlChainID, 100)
	require.NoError(t, err)
	require.Len(t, audit, 8) // 4 pauses + 4 resumes
	for _, a := range audit {
		require.Equal(t, "ops-alice", a.Actor)
	}

	// Unknown flow is rejected.
	rec := postJSON(t, mux, "/v1/controls/pause", controlsRequest{Flow: "everything"})
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAdminScanWithoutCredit(t *testing.T) {
	deps := controlDeps(t)
	mux := adminMux(deps)

	rec := postJSON(t, mux, "/v1/controls/scan-without-credit",
		scanWithoutCreditRequest{Enabled: true, Reason: "incident 42"})
	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, adminStatus(t, mux).Controls.ScanWithoutCredit)

	rec = postJSON(t, mux, "/v1/controls/scan-without-credit",
		scanWithoutCreditRequest{Enabled: false})
	require.Equal(t, http.StatusOK, rec.Code)
	require.False(t, adminStatus(t, mux).Controls.ScanWithoutCredit)
}

func TestAdminScannerResumeFrom(t *testing.T) {
	deps := controlDeps(t)
	mux := adminMux(deps)

	rec := postJSON(t, mux, "/v1/scanner/resume-from", resumeFromRequest{Height: 12345})
	require.Equal(t, http.StatusOK, rec.Code)
	st := adminStatus(t, mux)
	require.NotNil(t, st.Controls.ResumeFromHeight)
	require.Equal(t, uint64(12345), *st.Controls.ResumeFromHeight)

	rec = postJSON(t, mux, "/v1/scanner/resume-from", resumeFromRequest{Height: 0})
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAdminReviewQueue(t *testing.T) {
	deps := controlDeps(t)
	mux := adminMux(deps)
	ctx := context.Background()

	// A DEPOSIT review row points at a real deposit parked in REVIEW_REQUIRED.
	dep, err := deps.Store.Deposits().Insert(ctx, storage.DepositRecord{
		ChainID: ctlChainID, TxHash: "RQ01", MessageIndex: 0, CoinIndex: 0,
		BlockHeight: 9, BlockTimestamp: time.Now().UTC(),
		RecipientAddress: "sovr1reviewdeposit", Denom: storage.BaseDenom,
		AmountBaseUnits: sdkmath.NewInt(2_000_000), Status: storage.DepositDiscovered,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	require.NoError(t, deps.Store.Deposits().UpdateStatus(ctx, dep.ID,
		storage.DepositDiscovered, storage.DepositReviewRequired, storage.DepositUpdate{}))

	item, err := deps.Store.Review().Open(ctx, storage.ReviewItem{
		ChainID: ctlChainID, Kind: storage.ReviewKindDeposit,
		RefID:  strconv.FormatInt(dep.ID, 10),
		Reason: "omnibus memo missing", OpenedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/review-queue", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var list reviewQueueResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list.Items, 1)
	require.Equal(t, item.ID, list.Items[0].ID)
	require.Equal(t, "DEPOSIT", list.Items[0].Kind)

	// A DEPOSIT item rejects a withdrawal outcome (wrong kind).
	resolvePath := fmt.Sprintf("/v1/review-queue/%d/resolve", item.ID)
	rec = postJSON(t, mux, resolvePath, resolveRequest{Outcome: "WITHDRAWAL_FAILED", Resolution: "wrong kind"})
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// Resolve DEPOSIT_APPROVED: effective immediately — the queue empties, and
	// the backing deposit re-enters the credit pipeline (not force-credited).
	rec = postJSON(t, mux, resolvePath, resolveRequest{Outcome: "DEPOSIT_APPROVED", Resolution: "manually approved"})
	require.Equal(t, http.StatusOK, rec.Code)

	got, err := deps.Store.Deposits().GetByID(ctx, dep.ID)
	require.NoError(t, err)
	require.Equal(t, storage.DepositAwaitingConfirmations, got.Status,
		"approved review deposit must re-enter the pipeline, never jump straight to CREDITED")

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/review-queue", nil))
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Empty(t, list.Items)

	// Double resolve conflicts; unknown id 404s; junk id 400s.
	rec = postJSON(t, mux, resolvePath, resolveRequest{Outcome: "DEPOSIT_APPROVED", Resolution: "again"})
	require.Equal(t, http.StatusConflict, rec.Code)
	rec = postJSON(t, mux, "/v1/review-queue/99999/resolve", resolveRequest{Outcome: "DEPOSIT_APPROVED", Resolution: "x"})
	require.Equal(t, http.StatusNotFound, rec.Code)
	rec = postJSON(t, mux, "/v1/review-queue/junk/resolve", resolveRequest{Outcome: "DEPOSIT_APPROVED", Resolution: "x"})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	// Missing resolution is invalid; missing outcome is invalid.
	rec = postJSON(t, mux, "/v1/review-queue/1/resolve", map[string]string{})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	rec = postJSON(t, mux, "/v1/review-queue/1/resolve", resolveRequest{Resolution: "no outcome"})
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAdminReconcileAddress(t *testing.T) {
	deps := controlDeps(t)
	mux := adminMux(deps)
	ctx := context.Background()

	addr := "sovr1exampleaddress"
	require.NoError(t, deps.Store.Watch().Upsert(ctx, storage.WatchedAddress{
		ChainID: ctlChainID, Address: addr, Kind: storage.WatchCustomerDeposit, Active: true,
	}))
	_, err := deps.Store.Ledger().Append(ctx, storage.LedgerEntry{
		ChainID: ctlChainID, Kind: storage.LedgerKindTx, TxHash: "AB01",
		BlockHeight: 5, Direction: storage.DirectionIn, Address: addr,
		AmountBaseUnits: sdkmath.NewInt(500_000), Denom: storage.BaseDenom,
		Classification: storage.ClassExternalDeposit, CreatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	deps.Client.(*fakeAdapterClient).balances[addr] = sdkmath.NewInt(400_000) // inject 100_000 discrepancy

	rec := postJSON(t, mux, "/v1/reconcile/address", reconcileAddressRequest{Address: addr})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var rep storage.ReconciliationReport
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rep))
	require.Equal(t, 1, rep.DiscrepancyCount)
	require.Equal(t, storage.ReconManual, rep.Kind)
	require.Len(t, rep.Entries, 1)
	require.Equal(t, addr, rep.Entries[0].Address)

	// Persisted before the response returned.
	stored, err := deps.Store.Recon().GetReport(ctx, rep.ReportID)
	require.NoError(t, err)
	require.Equal(t, 1, stored.DiscrepancyCount)

	// Missing address is invalid.
	rec = postJSON(t, mux, "/v1/reconcile/address", reconcileAddressRequest{})
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAdminReconcileTx(t *testing.T) {
	deps := controlDeps(t)
	mux := adminMux(deps)

	// Unknown tx: nothing on chain to reconcile against — clean MANUAL report.
	rec := postJSON(t, mux, "/v1/reconcile/tx", reconcileTxRequest{TxHash: "DEADBEEF"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var rep storage.ReconciliationReport
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rep))
	require.Equal(t, 0, rep.DiscrepancyCount)

	rec = postJSON(t, mux, "/v1/reconcile/tx", reconcileTxRequest{})
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAdminReconcileUnavailableWithoutClient(t *testing.T) {
	deps := controlDeps(t)
	deps.Client = nil
	mux := adminMux(deps)
	rec := postJSON(t, mux, "/v1/reconcile/tx", reconcileTxRequest{TxHash: "AB"})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestAdminChainReviewResolve(t *testing.T) {
	deps := controlDeps(t)
	mux := adminMux(deps)
	ctx := context.Background()

	_, err := deps.Store.ChainReview().Open(ctx, storage.ChainReviewCondition{
		ConditionID: "cond-9", ChainID: ctlChainID,
		Trigger: storage.TriggerBlockHashMismatch, OpenedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	require.True(t, adminStatus(t, mux).ChainReviewOpen)

	rec := postJSON(t, mux, "/v1/chain-review/cond-9/resolve",
		resolveRequest{Resolution: "node resynced from snapshot; hashes verified"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.False(t, adminStatus(t, mux).ChainReviewOpen)

	rec = postJSON(t, mux, "/v1/chain-review/cond-9/resolve", resolveRequest{Resolution: "again"})
	require.Equal(t, http.StatusConflict, rec.Code)
}
