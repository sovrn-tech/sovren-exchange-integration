package main

// Admin/ops API (contracts/adapter-config-and-ops.md): this file serves the
// read surface — GET /v1/status with controls state, checkpoint, open
// chain-review conditions, and versions — and mounts the mutation +
// review-queue surface from admin_controls.go (T064). Localhost scoping and
// authn are deployment configuration (admin.listen).

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

type statusControls struct {
	CreditPaused      bool    `json:"credit_paused"`
	SigningPaused     bool    `json:"signing_paused"`
	BroadcastPaused   bool    `json:"broadcast_paused"`
	SweepPaused       bool    `json:"sweep_paused"`
	ScanWithoutCredit bool    `json:"scan_without_credit"`
	ResumeFromHeight  *uint64 `json:"resume_from_height,omitempty"`
}

type statusCheckpoint struct {
	LastFullyProcessedHeight uint64 `json:"last_fully_processed_height"`
	LastObservedBlockHash    string `json:"last_observed_block_hash"`
	UpdatedAt                string `json:"updated_at"`
}

type statusChainReview struct {
	ConditionID string `json:"condition_id"`
	Trigger     string `json:"trigger"`
	OpenedAt    string `json:"opened_at"`
}

type statusVersions struct {
	Adapter    string `json:"adapter"`
	Commit     string `json:"commit"`
	AppVersion string `json:"app_version"`
	SDKVersion string `json:"sdk_version"`
}

type statusResponse struct {
	ChainID               string              `json:"chain_id"`
	NetworkType           string              `json:"network_type"`
	Controls              statusControls      `json:"controls"`
	Checkpoint            *statusCheckpoint   `json:"checkpoint"`
	ChainReviewOpen       bool                `json:"chain_review_open"`
	ChainReviewConditions []statusChainReview `json:"chain_review_conditions"`
	Versions              statusVersions      `json:"versions"`
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// adminAuth wraps the admin mux with bearer-token enforcement when a token is
// configured. Applied to every route (including GET /v1/status, which exposes
// operational posture useful to an attacker). With no token the caller is
// responsible for loopback binding, which config validation enforces.
func adminAuth(token string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	want := []byte(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			writeJSON(w, http.StatusUnauthorized, apiError{Code: "UNAUTHORIZED", Message: "valid bearer token required"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func adminMux(deps *Deps) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, apiError{Code: "METHOD_NOT_ALLOWED", Message: "GET only"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		resp, err := buildStatus(ctx, deps)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, apiError{Code: "STATUS_UNAVAILABLE", Message: err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, resp)
	})
	registerAdminControls(mux, deps)
	return adminAuth(deps.Config.Admin.AuthToken, mux)
}

func buildStatus(ctx context.Context, deps *Deps) (*statusResponse, error) {
	chainID := deps.Manifest.ChainID
	controls, err := deps.Store.Controls().Get(ctx, chainID)
	if err != nil {
		return nil, err
	}
	resp := &statusResponse{
		ChainID:     chainID,
		NetworkType: deps.Manifest.NetworkType,
		Controls: statusControls{
			CreditPaused:      controls.CreditPaused,
			SigningPaused:     controls.SigningPaused,
			BroadcastPaused:   controls.BroadcastPaused,
			SweepPaused:       controls.SweepPaused,
			ScanWithoutCredit: controls.ScanWithoutCredit,
			ResumeFromHeight:  controls.ResumeFromHeight,
		},
		Versions: statusVersions{
			Adapter:    Version,
			Commit:     Commit,
			AppVersion: deps.Manifest.Versions.App,
			SDKVersion: deps.Manifest.Versions.SDK,
		},
		ChainReviewConditions: []statusChainReview{},
	}

	cp, err := deps.Store.Checkpoints().Get(ctx, chainID)
	switch {
	case err == nil:
		resp.Checkpoint = &statusCheckpoint{
			LastFullyProcessedHeight: cp.LastFullyProcessedHeight,
			LastObservedBlockHash:    cp.LastObservedBlockHash,
			UpdatedAt:                cp.UpdatedAt.UTC().Format(time.RFC3339),
		}
	case errors.Is(err, storage.ErrNotFound):
	default:
		return nil, err
	}

	open, err := deps.Store.ChainReview().ListOpen(ctx, chainID)
	if err != nil {
		return nil, err
	}
	resp.ChainReviewOpen = len(open) > 0
	for _, c := range open {
		resp.ChainReviewConditions = append(resp.ChainReviewConditions, statusChainReview{
			ConditionID: c.ConditionID,
			Trigger:     string(c.Trigger),
			OpenedAt:    c.OpenedAt.UTC().Format(time.RFC3339),
		})
	}
	return resp, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
