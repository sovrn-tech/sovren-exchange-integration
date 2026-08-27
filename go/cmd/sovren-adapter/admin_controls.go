package main

// Admin/ops API mutations (T064 — contracts/adapter-config-and-ops.md):
// pause/resume per flow, scan-without-credit, scanner resume-from,
// reconcile-now (tx / address), and the review queue. Registered onto the
// admin mux from admin.go's adminMux.
//
// Every mutation is audit-logged: operational-control flips persist audit
// rows via ControlsRepo.Apply (who/when/why — the controls_audit table), and
// every handler additionally emits a structured "admin audit" log line.
// Reconcile-now and review-resolve mutations have no dedicated audit table;
// their durable trace is the persisted reconciliation report / resolved
// review row plus the structured log (documented in docs/reconciliation.md).
//
// All state changes take effect before the response returns and are visible
// in GET /v1/status (certification asserts this): every path below writes
// through the store synchronously.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/sovrn-tech/sovren-exchange-integration/go/reconcile"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

const adminRequestTimeout = 30 * time.Second

// registerAdminControls mounts the mutation + review-queue surface.
func registerAdminControls(mux *http.ServeMux, deps *Deps) {
	mux.HandleFunc("POST /v1/controls/pause", adminHandler(deps, handleControlsPause(true)))
	mux.HandleFunc("POST /v1/controls/resume", adminHandler(deps, handleControlsPause(false)))
	mux.HandleFunc("POST /v1/controls/scan-without-credit", adminHandler(deps, handleScanWithoutCredit))
	mux.HandleFunc("POST /v1/scanner/resume-from", adminHandler(deps, handleScannerResumeFrom))
	mux.HandleFunc("POST /v1/reconcile/tx", adminHandler(deps, handleReconcileTx))
	mux.HandleFunc("POST /v1/reconcile/address", adminHandler(deps, handleReconcileAddress))
	mux.HandleFunc("GET /v1/review-queue", adminHandler(deps, handleReviewQueueList))
	mux.HandleFunc("POST /v1/review-queue/{id}/resolve", adminHandler(deps, handleReviewQueueResolve))
	mux.HandleFunc("POST /v1/chain-review/{id}/resolve", adminHandler(deps, handleChainReviewResolve))
}

// adminHandler wraps a handler with the shared timeout and error envelope.
func adminHandler(deps *Deps, fn func(ctx context.Context, deps *Deps, r *http.Request) (int, any)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), adminRequestTimeout)
		defer cancel()
		status, body := fn(ctx, deps, r)
		writeJSON(w, status, body)
	}
}

// actorOf resolves the audit actor: the X-Actor header when present
// (deployment authn — mTLS identity or bearer principal — is expected to set
// it), else the generic API principal.
func actorOf(r *http.Request) string {
	if a := r.Header.Get("X-Actor"); a != "" {
		return a
	}
	return "admin-api"
}

func decodeBody(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// auditLog emits the structured mutation trace (FR-051).
func auditLog(deps *Deps, action, actor string, fields ...any) {
	if deps.Logger == nil {
		return
	}
	args := append([]any{"event", "admin_audit", "action", action, "actor", actor}, fields...)
	deps.Logger.Info("admin mutation", args...)
}

func storageErrStatus(err error) (int, apiError) {
	switch {
	case errors.Is(err, storage.ErrNotFound):
		return http.StatusNotFound, apiError{Code: "NOT_FOUND", Message: err.Error()}
	case errors.Is(err, storage.ErrStatusConflict):
		return http.StatusConflict, apiError{Code: "STATUS_CONFLICT", Message: err.Error()}
	default:
		return http.StatusInternalServerError, apiError{Code: "INTERNAL", Message: err.Error()}
	}
}

// --- controls -------------------------------------------------------------

type controlsRequest struct {
	Flow   string `json:"flow"`
	Reason string `json:"reason,omitempty"`
}

type controlsResponse struct {
	Controls statusControls `json:"controls"`
}

func toStatusControls(c storage.OperationalControls) statusControls {
	return statusControls{
		CreditPaused:      c.CreditPaused,
		SigningPaused:     c.SigningPaused,
		BroadcastPaused:   c.BroadcastPaused,
		SweepPaused:       c.SweepPaused,
		ScanWithoutCredit: c.ScanWithoutCredit,
		ResumeFromHeight:  c.ResumeFromHeight,
	}
}

// handleControlsPause serves POST /v1/controls/pause and /resume: each flow
// pauses/resumes independently (FR-051) — the update touches exactly one
// field.
func handleControlsPause(pause bool) func(ctx context.Context, deps *Deps, r *http.Request) (int, any) {
	return func(ctx context.Context, deps *Deps, r *http.Request) (int, any) {
		var req controlsRequest
		if err := decodeBody(r, &req); err != nil {
			return http.StatusBadRequest, apiError{Code: "INVALID_REQUEST", Message: err.Error()}
		}
		flow := storage.ControlFlow(req.Flow)
		if !flow.Valid() {
			return http.StatusBadRequest, apiError{Code: "INVALID_FLOW",
				Message: fmt.Sprintf("flow %q is not one of credit|signing|broadcast|sweep", req.Flow)}
		}
		v := pause
		var u storage.ControlsUpdate
		switch flow {
		case storage.FlowCredit:
			u.CreditPaused = &v
		case storage.FlowSigning:
			u.SigningPaused = &v
		case storage.FlowBroadcast:
			u.BroadcastPaused = &v
		case storage.FlowSweep:
			u.SweepPaused = &v
		}
		actor := actorOf(r)
		reason := req.Reason
		if reason == "" {
			reason = "admin API request"
		}
		controls, err := deps.Store.Controls().Apply(ctx, deps.Manifest.ChainID, u, actor, reason)
		if err != nil {
			return storageErrStatus(err)
		}
		action := "controls.pause"
		if !pause {
			action = "controls.resume"
		}
		auditLog(deps, action, actor, "flow", string(flow), "reason", reason)
		return http.StatusOK, controlsResponse{Controls: toStatusControls(controls)}
	}
}

type scanWithoutCreditRequest struct {
	Enabled bool   `json:"enabled"`
	Reason  string `json:"reason,omitempty"`
}

func handleScanWithoutCredit(ctx context.Context, deps *Deps, r *http.Request) (int, any) {
	var req scanWithoutCreditRequest
	if err := decodeBody(r, &req); err != nil {
		return http.StatusBadRequest, apiError{Code: "INVALID_REQUEST", Message: err.Error()}
	}
	actor := actorOf(r)
	reason := req.Reason
	if reason == "" {
		reason = "admin API request"
	}
	controls, err := deps.Store.Controls().Apply(ctx, deps.Manifest.ChainID,
		storage.ControlsUpdate{ScanWithoutCredit: &req.Enabled}, actor, reason)
	if err != nil {
		return storageErrStatus(err)
	}
	auditLog(deps, "controls.scan-without-credit", actor, "enabled", req.Enabled, "reason", reason)
	return http.StatusOK, controlsResponse{Controls: toStatusControls(controls)}
}

// --- scanner --------------------------------------------------------------

type resumeFromRequest struct {
	Height uint64 `json:"height"`
	Reason string `json:"reason,omitempty"`
}

// handleScannerResumeFrom records the rescan base in the operational
// controls; the scanner consumes it on its next cycle. Replay is idempotent
// — unique keys make re-processing safe (FR-024).
func handleScannerResumeFrom(ctx context.Context, deps *Deps, r *http.Request) (int, any) {
	var req resumeFromRequest
	if err := decodeBody(r, &req); err != nil {
		return http.StatusBadRequest, apiError{Code: "INVALID_REQUEST", Message: err.Error()}
	}
	if req.Height == 0 {
		return http.StatusBadRequest, apiError{Code: "INVALID_HEIGHT", Message: "height must be >= 1"}
	}
	actor := actorOf(r)
	reason := req.Reason
	if reason == "" {
		reason = "admin API rescan request"
	}
	h := req.Height
	controls, err := deps.Store.Controls().Apply(ctx, deps.Manifest.ChainID,
		storage.ControlsUpdate{ResumeFromHeight: &h}, actor, reason)
	if err != nil {
		return storageErrStatus(err)
	}
	auditLog(deps, "scanner.resume-from", actor, "height", req.Height, "reason", reason)
	return http.StatusOK, controlsResponse{Controls: toStatusControls(controls)}
}

// --- reconcile-now --------------------------------------------------------

// buildReconciler constructs the on-demand reconciler for admin requests.
func buildReconciler(deps *Deps) (*reconcile.Reconciler, error) {
	if deps.Client == nil {
		return nil, fmt.Errorf("chain client unavailable")
	}
	opts := []reconcile.Option{}
	if deps.Metrics != nil {
		opts = append(opts, reconcile.WithMetrics(deps.Metrics))
	}
	if deps.Logger != nil {
		opts = append(opts, reconcile.WithLogger(deps.Logger))
	}
	return reconcile.New(deps.Store, deps.Client, reconcile.Config{ChainID: deps.Manifest.ChainID}, opts...)
}

type reconcileTxRequest struct {
	TxHash string `json:"tx_hash"`
}

func handleReconcileTx(ctx context.Context, deps *Deps, r *http.Request) (int, any) {
	var req reconcileTxRequest
	if err := decodeBody(r, &req); err != nil {
		return http.StatusBadRequest, apiError{Code: "INVALID_REQUEST", Message: err.Error()}
	}
	if req.TxHash == "" {
		return http.StatusBadRequest, apiError{Code: "INVALID_REQUEST", Message: "tx_hash required"}
	}
	rec, err := buildReconciler(deps)
	if err != nil {
		return http.StatusServiceUnavailable, apiError{Code: "RECONCILE_UNAVAILABLE", Message: err.Error()}
	}
	report, err := rec.ReconcileTx(ctx, req.TxHash, storage.ReconManual)
	if err != nil {
		return http.StatusInternalServerError, apiError{Code: "RECONCILE_FAILED", Message: err.Error()}
	}
	auditLog(deps, "reconcile.tx", actorOf(r), "tx_hash", req.TxHash,
		"report_id", report.ReportID, "discrepancies", report.DiscrepancyCount)
	return http.StatusOK, report
}

type reconcileAddressRequest struct {
	Address string `json:"address"`
}

func handleReconcileAddress(ctx context.Context, deps *Deps, r *http.Request) (int, any) {
	var req reconcileAddressRequest
	if err := decodeBody(r, &req); err != nil {
		return http.StatusBadRequest, apiError{Code: "INVALID_REQUEST", Message: err.Error()}
	}
	if req.Address == "" {
		return http.StatusBadRequest, apiError{Code: "INVALID_REQUEST", Message: "address required"}
	}
	rec, err := buildReconciler(deps)
	if err != nil {
		return http.StatusServiceUnavailable, apiError{Code: "RECONCILE_UNAVAILABLE", Message: err.Error()}
	}
	report, err := rec.ReconcileAddressReport(ctx, req.Address)
	if err != nil {
		return http.StatusInternalServerError, apiError{Code: "RECONCILE_FAILED", Message: err.Error()}
	}
	auditLog(deps, "reconcile.address", actorOf(r), "address", req.Address,
		"report_id", report.ReportID, "discrepancies", report.DiscrepancyCount)
	return http.StatusOK, report
}

// --- review queue ---------------------------------------------------------

type reviewItemView struct {
	ID       int64  `json:"id"`
	ChainID  string `json:"chain_id"`
	Kind     string `json:"kind"`
	RefID    string `json:"ref_id"`
	Reason   string `json:"reason"`
	OpenedAt string `json:"opened_at"`
}

type reviewQueueResponse struct {
	Items []reviewItemView `json:"items"`
}

const reviewQueueListLimit = 500

func handleReviewQueueList(ctx context.Context, deps *Deps, r *http.Request) (int, any) {
	items, err := deps.Store.Review().ListOpen(ctx, deps.Manifest.ChainID, reviewQueueListLimit)
	if err != nil {
		return storageErrStatus(err)
	}
	resp := reviewQueueResponse{Items: []reviewItemView{}}
	for _, it := range items {
		resp.Items = append(resp.Items, reviewItemView{
			ID:       it.ID,
			ChainID:  it.ChainID,
			Kind:     string(it.Kind),
			RefID:    it.RefID,
			Reason:   it.Reason,
			OpenedAt: it.OpenedAt.UTC().Format(time.RFC3339),
		})
	}
	return http.StatusOK, resp
}

type resolveRequest struct {
	// Outcome is the typed disposition for a review-queue resolve (required
	// there): WITHDRAWAL_CONFIRMED / WITHDRAWAL_FAILED / WITHDRAWAL_CANCELLED /
	// DEPOSIT_APPROVED / DEPOSIT_REJECTED / LEDGER_ACKNOWLEDGED. Unused by the
	// chain-review resolve, which needs only a free-text resolution.
	Outcome    string `json:"outcome,omitempty"`
	Resolution string `json:"resolution"`
}

func handleReviewQueueResolve(ctx context.Context, deps *Deps, r *http.Request) (int, any) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return http.StatusBadRequest, apiError{Code: "INVALID_REQUEST", Message: "id must be an integer"}
	}
	var req resolveRequest
	if err := decodeBody(r, &req); err != nil {
		return http.StatusBadRequest, apiError{Code: "INVALID_REQUEST", Message: err.Error()}
	}
	if req.Resolution == "" {
		return http.StatusBadRequest, apiError{Code: "INVALID_REQUEST", Message: "resolution required"}
	}
	if req.Outcome == "" {
		return http.StatusBadRequest, apiError{Code: "INVALID_REQUEST", Message: "outcome required"}
	}
	status, body := resolveReviewItem(ctx, deps, id, reviewOutcome(req.Outcome), req.Resolution, time.Now().UTC())
	if status == http.StatusOK {
		auditLog(deps, "review-queue.resolve", actorOf(r), "id", id, "outcome", req.Outcome, "resolution", req.Resolution)
	}
	return status, body
}

// handleChainReviewResolve resolves an FR-044 ChainReviewCondition (adapter
// extension beyond the contract table: hash-mismatch and wrong-chain-ID
// conditions never auto-resolve, and a stuck-open condition keeps the
// crediting gate closed — operators need a first-class path out after
// investigation; documented in docs/reconciliation.md).
func handleChainReviewResolve(ctx context.Context, deps *Deps, r *http.Request) (int, any) {
	id := r.PathValue("id")
	var req resolveRequest
	if err := decodeBody(r, &req); err != nil {
		return http.StatusBadRequest, apiError{Code: "INVALID_REQUEST", Message: err.Error()}
	}
	if req.Resolution == "" {
		return http.StatusBadRequest, apiError{Code: "INVALID_REQUEST", Message: "resolution required"}
	}
	if err := deps.Store.ChainReview().Resolve(ctx, id, req.Resolution, time.Now().UTC()); err != nil {
		return storageErrStatus(err)
	}
	auditLog(deps, "chain-review.resolve", actorOf(r), "condition_id", id, "resolution", req.Resolution)
	return http.StatusOK, map[string]any{"condition_id": id, "resolved": true}
}
