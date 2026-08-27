package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage/sqlite"
)

func testDeps(t *testing.T) *Deps {
	t.Helper()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "admin-test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return &Deps{
		Config: &Config{},
		Store:  store,
		Manifest: &client.NetworkManifest{
			ChainID:     "sovr-local-dev",
			NetworkType: "testnet",
			Versions:    client.ManifestVersions{App: "v0.16.2", SDK: "v0.53.6"},
		},
	}
}

func getStatus(t *testing.T, mux http.Handler) statusResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/status", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp statusResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

func TestAdminStatusEndpoint(t *testing.T) {
	deps := testDeps(t)
	mux := adminMux(deps)
	ctx := context.Background()

	resp := getStatus(t, mux)
	require.Equal(t, "sovr-local-dev", resp.ChainID)
	require.Equal(t, "testnet", resp.NetworkType)
	require.False(t, resp.Controls.CreditPaused)
	require.Nil(t, resp.Checkpoint)
	require.False(t, resp.ChainReviewOpen)
	require.Equal(t, "v0.16.2", resp.Versions.AppVersion)

	// State changes are visible in GET /v1/status (contract assertion).
	paused := true
	_, err := deps.Store.Controls().Apply(ctx, "sovr-local-dev",
		storage.ControlsUpdate{CreditPaused: &paused}, "ops", "incident")
	require.NoError(t, err)
	require.NoError(t, deps.Store.Checkpoints().Set(ctx, storage.ScannerCheckpoint{
		ChainID:                  "sovr-local-dev",
		LastFullyProcessedHeight: 42,
		LastObservedBlockHash:    "AB12",
		UpdatedAt:                time.Now().UTC(),
	}))
	_, err = deps.Store.ChainReview().Open(ctx, storage.ChainReviewCondition{
		ConditionID: "cond-1",
		ChainID:     "sovr-local-dev",
		Trigger:     storage.TriggerBlockHashMismatch,
		OpenedAt:    time.Now().UTC(),
	})
	require.NoError(t, err)

	resp = getStatus(t, mux)
	require.True(t, resp.Controls.CreditPaused)
	require.NotNil(t, resp.Checkpoint)
	require.Equal(t, uint64(42), resp.Checkpoint.LastFullyProcessedHeight)
	require.True(t, resp.ChainReviewOpen)
	require.Len(t, resp.ChainReviewConditions, 1)
	require.Equal(t, "BLOCK_HASH_MISMATCH", resp.ChainReviewConditions[0].Trigger)
}

func TestAdminStatusMethodNotAllowed(t *testing.T) {
	mux := adminMux(testDeps(t))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/status", nil))
	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}
