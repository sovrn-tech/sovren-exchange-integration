package main

// Typed review-queue resolution tests (FR-030/FR-035/FR-044): resolving a
// withdrawal review must drive the referenced withdrawal to its terminal state
// AND dispose the quarantined sequence + committed funds — the gap that let a
// CheckTx-rejected withdrawal pin its funds and sequence forever.

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	sdkmath "cosmossdk.io/math"
	"github.com/stretchr/testify/require"

	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

const resolveSource = "sovr1hotwalletresolve"

// seedReviewWithdrawal creates a withdrawal parked in REVIEW_REQUIRED with a
// bound sequence reservation left in restStatus, and opens its WITHDRAWAL
// review row. Returns the review item ID. restStatus may be SIGNED,
// BROADCAST, or RECONCILIATION_REQUIRED (the last mirrors the CheckTx-reject
// quarantine, the production resting state); each must reach CONSUMED directly
// on a signed FAILED/CONFIRMED disposition.
func seedReviewWithdrawal(t *testing.T, deps *Deps, id string, seq uint64, withBytes bool, restStatus storage.SequenceReservationStatus) int64 {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	acct := uint64(7)
	sequence := seq
	txHash := "TX" + id
	var signed []byte
	if withBytes {
		signed = []byte{0x0a, 0x01, 0x02}
	}
	_, err := deps.Store.Withdrawals().Create(ctx, storage.WithdrawalRecord{
		WithdrawalID: id, IdempotencyKey: "idem-" + id,
		ChainID: ctlChainID, SourceAddress: resolveSource,
		DestinationAddress: "sovr1destresolve", Denom: storage.BaseDenom,
		AmountBaseUnits: sdkmath.NewInt(1_000_000),
		AccountNumber:   &acct, Sequence: &sequence,
		SignedTxBytes: signed, TxHash: &txHash,
		Status:    storage.WithdrawalSigned,
		CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)

	res, err := deps.Store.Sequences().Reserve(ctx, storage.SequenceReservation{
		ChainID: ctlChainID, SourceAddress: resolveSource,
		AccountNumber: acct, Sequence: sequence,
		WorkRef: storage.WorkRef{Kind: storage.WorkWithdrawal, ID: id},
		Status:  storage.SequenceReserved,
	})
	require.NoError(t, err)
	require.NoError(t, deps.Store.Sequences().UpdateStatus(ctx, res.ID,
		storage.SequenceReserved, storage.SequenceSigned))
	switch restStatus {
	case storage.SequenceSigned:
		// leave at SIGNED
	case storage.SequenceBroadcast:
		require.NoError(t, deps.Store.Sequences().UpdateStatus(ctx, res.ID,
			storage.SequenceSigned, storage.SequenceBroadcast))
	case storage.SequenceReconciliationRequired:
		require.NoError(t, deps.Store.Sequences().UpdateStatus(ctx, res.ID,
			storage.SequenceSigned, storage.SequenceReconciliationRequired))
	default:
		t.Fatalf("seedReviewWithdrawal: unsupported restStatus %q", restStatus)
	}

	require.NoError(t, deps.Store.Withdrawals().UpdateStatus(ctx, id,
		storage.WithdrawalSigned, storage.WithdrawalReviewRequired, storage.WithdrawalUpdate{}))

	item, err := deps.Store.Review().Open(ctx, storage.ReviewItem{
		ChainID: ctlChainID, Kind: storage.ReviewKindWithdrawal, RefID: id,
		Reason: "checktx rejected", OpenedAt: now,
	})
	require.NoError(t, err)
	return item.ID
}

func committedCount(t *testing.T, deps *Deps) int64 {
	t.Helper()
	n, _, err := deps.Store.Withdrawals().SumCommittedBySource(context.Background(), ctlChainID, resolveSource)
	require.NoError(t, err)
	return n
}

func seqStatus(t *testing.T, deps *Deps, withdrawalID string) storage.SequenceReservationStatus {
	t.Helper()
	res, err := deps.Store.Sequences().GetByWorkRef(context.Background(),
		storage.WorkRef{Kind: storage.WorkWithdrawal, ID: withdrawalID})
	require.NoError(t, err)
	return res.Status
}

// TestReviewResolveWithdrawalConfirmed: operator-verified inclusion consumes the
// sequence and drives the withdrawal terminal; funds leave the committed set.
func TestReviewResolveWithdrawalConfirmed(t *testing.T) {
	deps := controlDeps(t)
	mux := adminMux(deps)
	ctx := context.Background()

	id := "wd-confirmed"
	itemID := seedReviewWithdrawal(t, deps, id, 100, true, storage.SequenceReconciliationRequired)
	require.Equal(t, int64(1), committedCount(t, deps), "signed REVIEW_REQUIRED withdrawal commits funds")

	rec := postJSON(t, mux, fmt.Sprintf("/v1/review-queue/%d/resolve", itemID),
		resolveRequest{Outcome: "WITHDRAWAL_CONFIRMED", Resolution: "tx TX in block 42"})
	require.Equal(t, http.StatusOK, rec.Code)

	w, err := deps.Store.Withdrawals().Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, storage.WithdrawalConfirmed, w.Status)
	require.Equal(t, storage.SequenceConsumed, seqStatus(t, deps, id))
	require.Equal(t, int64(0), committedCount(t, deps), "confirmed withdrawal no longer commits funds")
}

// TestReviewResolveWithdrawalFailed: a SIGNED withdrawal whose slot is spent
// (chain truth: sequence advanced) marks the sequence CONSUMED — never
// RELEASED — so the slot can NOT be re-issued while the old signed bytes could
// still redeem it. Funds are freed.
func TestReviewResolveWithdrawalFailed(t *testing.T) {
	deps := controlDeps(t)
	mux := adminMux(deps)
	ctx := context.Background()

	id := "wd-failed"
	itemID := seedReviewWithdrawal(t, deps, id, 200, true, storage.SequenceReconciliationRequired)
	require.Equal(t, int64(1), committedCount(t, deps))

	rec := postJSON(t, mux, fmt.Sprintf("/v1/review-queue/%d/resolve", itemID),
		resolveRequest{Outcome: "WITHDRAWAL_FAILED", Resolution: "chain sequence advanced past this tx"})
	require.Equal(t, http.StatusOK, rec.Code)

	w, err := deps.Store.Withdrawals().Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, storage.WithdrawalFailed, w.Status)
	require.Equal(t, storage.SequenceConsumed, seqStatus(t, deps, id),
		"a signed slot must be CONSUMED, never RELEASED")
	require.Equal(t, int64(0), committedCount(t, deps), "failed withdrawal frees committed funds")

	// The CONSUMED slot must NOT be reclaimable — re-reserving the same
	// sequence is a duplicate, so the old signed bytes can never be
	// double-obligated against a new payment.
	_, err = deps.Store.Sequences().Reserve(ctx, storage.SequenceReservation{
		ChainID: ctlChainID, SourceAddress: resolveSource,
		AccountNumber: 7, Sequence: 200,
		WorkRef: storage.WorkRef{Kind: storage.WorkWithdrawal, ID: "wd-reissue"},
		Status:  storage.SequenceReserved,
	})
	require.ErrorIs(t, err, storage.ErrDuplicate, "a consumed sequence slot must not be re-issuable")
}

// TestReviewResolveWithdrawalFailedFromLiveStatuses: a signed reservation that
// has NOT yet been quarantined (still SIGNED or BROADCAST) still reaches
// CONSUMED directly on FAILED — never RELEASED — closing the disposition-branch
// coverage beyond the RECONCILIATION_REQUIRED resting state.
func TestReviewResolveWithdrawalFailedFromLiveStatuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		seq  uint64
		rest storage.SequenceReservationStatus
	}{
		{"from_signed", 510, storage.SequenceSigned},
		{"from_broadcast", 520, storage.SequenceBroadcast},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps := controlDeps(t)
			mux := adminMux(deps)
			ctx := context.Background()

			id := "wd-" + tc.name
			itemID := seedReviewWithdrawal(t, deps, id, tc.seq, true, tc.rest)
			require.Equal(t, tc.rest, seqStatus(t, deps, id))

			rec := postJSON(t, mux, fmt.Sprintf("/v1/review-queue/%d/resolve", itemID),
				resolveRequest{Outcome: "WITHDRAWAL_FAILED", Resolution: "sequence advanced"})
			require.Equal(t, http.StatusOK, rec.Code)

			w, err := deps.Store.Withdrawals().Get(ctx, id)
			require.NoError(t, err)
			require.Equal(t, storage.WithdrawalFailed, w.Status)
			require.Equal(t, storage.SequenceConsumed, seqStatus(t, deps, id),
				"a signed reservation resting in %s must reach CONSUMED, never RELEASED", tc.rest)
		})
	}
}

// TestReviewResolveSignedWithdrawalCannotCancel: a signed withdrawal's bytes
// remain redeemable, so CANCELLED (which would RELEASE the slot) is refused.
func TestReviewResolveSignedWithdrawalCannotCancel(t *testing.T) {
	deps := controlDeps(t)
	mux := adminMux(deps)
	ctx := context.Background()

	id := "wd-signed-cancel"
	itemID := seedReviewWithdrawal(t, deps, id, 250, true, storage.SequenceReconciliationRequired)

	rec := postJSON(t, mux, fmt.Sprintf("/v1/review-queue/%d/resolve", itemID),
		resolveRequest{Outcome: "WITHDRAWAL_CANCELLED", Resolution: "try to abandon a signed tx"})
	require.Equal(t, http.StatusConflict, rec.Code)

	// Nothing moved: record still REVIEW_REQUIRED, sequence still quarantined,
	// funds still committed, review row still open.
	w, err := deps.Store.Withdrawals().Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, storage.WithdrawalReviewRequired, w.Status)
	require.Equal(t, storage.SequenceReconciliationRequired, seqStatus(t, deps, id))
	require.Equal(t, int64(1), committedCount(t, deps))
	open, err := deps.Store.Review().ListOpen(ctx, ctlChainID, 0)
	require.NoError(t, err)
	require.Len(t, open, 1)
}

// seedUnsignedReviewWithdrawal creates a PRE-SIGN withdrawal (no signed bytes)
// parked in REVIEW_REQUIRED with a RESERVED sequence reservation, and opens its
// review row. Returns the review item ID.
func seedUnsignedReviewWithdrawal(t *testing.T, deps *Deps, id string, seq uint64) int64 {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	acct := uint64(7)
	sequence := seq
	_, err := deps.Store.Withdrawals().Create(ctx, storage.WithdrawalRecord{
		WithdrawalID: id, IdempotencyKey: "idem-" + id,
		ChainID: ctlChainID, SourceAddress: resolveSource,
		DestinationAddress: "sovr1destresolve", Denom: storage.BaseDenom,
		AmountBaseUnits: sdkmath.NewInt(1_000_000),
		AccountNumber:   &acct, Sequence: &sequence,
		Status:    storage.WithdrawalSequenceReserved,
		CreatedAt: now, UpdatedAt: now,
	})
	require.NoError(t, err)
	_, err = deps.Store.Sequences().Reserve(ctx, storage.SequenceReservation{
		ChainID: ctlChainID, SourceAddress: resolveSource,
		AccountNumber: acct, Sequence: sequence,
		WorkRef: storage.WorkRef{Kind: storage.WorkWithdrawal, ID: id},
		Status:  storage.SequenceReserved,
	})
	require.NoError(t, err)
	require.NoError(t, deps.Store.Withdrawals().UpdateStatus(ctx, id,
		storage.WithdrawalSequenceReserved, storage.WithdrawalReviewRequired, storage.WithdrawalUpdate{}))
	item, err := deps.Store.Review().Open(ctx, storage.ReviewItem{
		ChainID: ctlChainID, Kind: storage.ReviewKindWithdrawal, RefID: id,
		Reason: "pre-sign quarantine", OpenedAt: now,
	})
	require.NoError(t, err)
	return item.ID
}

// TestReviewResolveAmbiguousSignerCannotCancel: a signer-verification / assembly
// failure quarantines the reservation to RECONCILIATION_REQUIRED with NO
// persisted bytes (withdrawals.Workflow.Sign). The signer may still hold a
// redeemable signature, so an empty signed_tx_bytes field must NOT make the
// slot cancellable/releasable — release-safety keys on the reservation state.
func TestReviewResolveAmbiguousSignerCannotCancel(t *testing.T) {
	deps := controlDeps(t)
	mux := adminMux(deps)
	ctx := context.Background()

	id := "wd-ambiguous"
	itemID := seedUnsignedReviewWithdrawal(t, deps, id, 600) // no bytes, reservation RESERVED
	// Simulate the Sign()-failure quarantine: RESERVED → RECONCILIATION_REQUIRED,
	// still with no persisted bytes.
	res, err := deps.Store.Sequences().GetByWorkRef(ctx, storage.WorkRef{Kind: storage.WorkWithdrawal, ID: id})
	require.NoError(t, err)
	require.NoError(t, deps.Store.Sequences().UpdateStatus(ctx, res.ID,
		storage.SequenceReserved, storage.SequenceReconciliationRequired))
	w, err := deps.Store.Withdrawals().Get(ctx, id)
	require.NoError(t, err)
	require.Empty(t, w.SignedTxBytes, "ambiguous signer state has no persisted bytes")

	// The ambiguous quarantine (no bytes, RECONCILIATION_REQUIRED) MUST keep its
	// funds committed — the signer may hold a redeemable signature, so a later
	// withdrawal must not reserve against this balance.
	require.Equal(t, int64(1), committedCount(t, deps),
		"a no-byte RECONCILIATION_REQUIRED quarantine must remain committed")

	// CANCELLED must be refused even though signed_tx_bytes is empty — the
	// quarantined slot may hold a redeemable signature.
	rec := postJSON(t, mux, fmt.Sprintf("/v1/review-queue/%d/resolve", itemID),
		resolveRequest{Outcome: "WITHDRAWAL_CANCELLED", Resolution: "try to release an ambiguous slot"})
	require.Equal(t, http.StatusConflict, rec.Code)

	w, err = deps.Store.Withdrawals().Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, storage.WithdrawalReviewRequired, w.Status)
	require.Equal(t, storage.SequenceReconciliationRequired, seqStatus(t, deps, id),
		"the quarantined slot must NOT be released")
	require.Equal(t, int64(1), committedCount(t, deps), "funds stay committed through the refused cancel")

	// FAILED is the correct resolution once chain truth proves the sequence
	// advanced — the slot is CONSUMED, never RELEASED, even with no bytes.
	rec = postJSON(t, mux, fmt.Sprintf("/v1/review-queue/%d/resolve", itemID),
		resolveRequest{Outcome: "WITHDRAWAL_FAILED", Resolution: "chain sequence advanced past it"})
	require.Equal(t, http.StatusOK, rec.Code)
	w, err = deps.Store.Withdrawals().Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, storage.WithdrawalFailed, w.Status)
	require.Equal(t, storage.SequenceConsumed, seqStatus(t, deps, id))
	require.Equal(t, int64(0), committedCount(t, deps), "FAILED frees the committed funds")
}

// TestReviewResolveReleasedPreSignCancellable: a startup ReconcileAccount can
// release a pre-sign (no-signature) RESERVED slot without touching the
// withdrawal or its review row. That RELEASED reservation must still be
// pre-sign — CANCELLED resolves the review (leaving the slot RELEASED), and
// CONFIRMED/FAILED are rejected rather than rolling back on an illegal
// RELEASED → CONSUMED. Regression for the "unresolvable review" gap.
func TestReviewResolveReleasedPreSignCancellable(t *testing.T) {
	deps := controlDeps(t)
	mux := adminMux(deps)
	ctx := context.Background()

	id := "wd-released-presign"
	itemID := seedUnsignedReviewWithdrawal(t, deps, id, 700) // no bytes, reservation RESERVED
	res, err := deps.Store.Sequences().GetByWorkRef(ctx, storage.WorkRef{Kind: storage.WorkWithdrawal, ID: id})
	require.NoError(t, err)
	// Simulate startup ReconcileAccount releasing a no-signature slot.
	require.NoError(t, deps.Store.Sequences().UpdateStatus(ctx, res.ID,
		storage.SequenceReserved, storage.SequenceReleased))
	require.Equal(t, storage.SequenceReleased, seqStatus(t, deps, id))

	// A released pre-sign slot holds no committed funds.
	require.Equal(t, int64(0), committedCount(t, deps))

	// CONFIRMED and FAILED are rejected (pre-sign — no signature could exist).
	path := fmt.Sprintf("/v1/review-queue/%d/resolve", itemID)
	rec := postJSON(t, mux, path, resolveRequest{Outcome: "WITHDRAWAL_CONFIRMED", Resolution: "no"})
	require.Equal(t, http.StatusConflict, rec.Code)
	rec = postJSON(t, mux, path, resolveRequest{Outcome: "WITHDRAWAL_FAILED", Resolution: "no"})
	require.Equal(t, http.StatusConflict, rec.Code)
	w, err := deps.Store.Withdrawals().Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, storage.WithdrawalReviewRequired, w.Status)
	require.Equal(t, storage.SequenceReleased, seqStatus(t, deps, id))

	// CANCELLED resolves the review — withdrawal CANCELLED, slot left RELEASED.
	rec = postJSON(t, mux, path, resolveRequest{Outcome: "WITHDRAWAL_CANCELLED", Resolution: "abandon released pre-sign"})
	require.Equal(t, http.StatusOK, rec.Code)
	w, err = deps.Store.Withdrawals().Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, storage.WithdrawalCancelled, w.Status)
	require.Equal(t, storage.SequenceReleased, seqStatus(t, deps, id))
	open, err := deps.Store.Review().ListOpen(ctx, ctlChainID, 0)
	require.NoError(t, err)
	require.Empty(t, open)
}

// TestReviewResolveWithdrawalCancelledPreSign: a pre-sign withdrawal (no signed
// bytes) is safely abandoned — sequence RELEASED and re-issuable, funds freed.
func TestReviewResolveWithdrawalCancelledPreSign(t *testing.T) {
	deps := controlDeps(t)
	mux := adminMux(deps)
	ctx := context.Background()

	id := "wd-cancelled"
	itemID := seedUnsignedReviewWithdrawal(t, deps, id, 300)

	rec := postJSON(t, mux, fmt.Sprintf("/v1/review-queue/%d/resolve", itemID),
		resolveRequest{Outcome: "WITHDRAWAL_CANCELLED", Resolution: "operator abandoned pre-sign"})
	require.Equal(t, http.StatusOK, rec.Code)

	w, err := deps.Store.Withdrawals().Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, storage.WithdrawalCancelled, w.Status)
	require.Equal(t, storage.SequenceReleased, seqStatus(t, deps, id),
		"an unsigned slot is safely RELEASED")

	// No bytes ever existed, so the released slot is re-issuable.
	reissued, err := deps.Store.Sequences().Reserve(ctx, storage.SequenceReservation{
		ChainID: ctlChainID, SourceAddress: resolveSource,
		AccountNumber: 7, Sequence: 300,
		WorkRef: storage.WorkRef{Kind: storage.WorkWithdrawal, ID: "wd-reissue-presign"},
		Status:  storage.SequenceReserved,
	})
	require.NoError(t, err, "an unsigned released slot must be reclaimable")
	require.Equal(t, storage.SequenceReserved, reissued.Status)
}

// TestReviewResolveUnsignedWithdrawalOutcomes: a pre-sign review (no signed
// bytes) refuses CONFIRMED and FAILED — both assert a slot was spent, which
// can't be true of a tx that was never broadcast — and is abandoned only via
// CANCELLED.
func TestReviewResolveUnsignedWithdrawalOutcomes(t *testing.T) {
	deps := controlDeps(t)
	mux := adminMux(deps)
	ctx := context.Background()

	id := "wd-nobytes"
	itemID := seedUnsignedReviewWithdrawal(t, deps, id, 350)
	path := fmt.Sprintf("/v1/review-queue/%d/resolve", itemID)

	// CONFIRMED and FAILED both require signed bytes.
	rec := postJSON(t, mux, path, resolveRequest{Outcome: "WITHDRAWAL_CONFIRMED", Resolution: "claims landed"})
	require.Equal(t, http.StatusConflict, rec.Code)
	rec = postJSON(t, mux, path, resolveRequest{Outcome: "WITHDRAWAL_FAILED", Resolution: "claims spent"})
	require.Equal(t, http.StatusConflict, rec.Code)

	// The record and review row are untouched by the refused resolutions.
	w, err := deps.Store.Withdrawals().Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, storage.WithdrawalReviewRequired, w.Status)
	open, err := deps.Store.Review().ListOpen(ctx, ctlChainID, 0)
	require.NoError(t, err)
	require.Len(t, open, 1)

	// CANCELLED is the correct pre-sign abandonment.
	rec = postJSON(t, mux, path, resolveRequest{Outcome: "WITHDRAWAL_CANCELLED", Resolution: "abandon pre-sign"})
	require.Equal(t, http.StatusOK, rec.Code)
	w, err = deps.Store.Withdrawals().Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, storage.WithdrawalCancelled, w.Status)
	require.Equal(t, storage.SequenceReleased, seqStatus(t, deps, id))
}

// TestReviewResolveDepositRejected drives the deposit terminal.
func TestReviewResolveDepositRejected(t *testing.T) {
	deps := controlDeps(t)
	mux := adminMux(deps)
	ctx := context.Background()

	dep, err := deps.Store.Deposits().Insert(ctx, storage.DepositRecord{
		ChainID: ctlChainID, TxHash: "RJ01", MessageIndex: 0, CoinIndex: 0,
		BlockHeight: 3, BlockTimestamp: time.Now().UTC(),
		RecipientAddress: "sovr1rejectdeposit", Denom: storage.BaseDenom,
		AmountBaseUnits: sdkmath.NewInt(500_000), Status: storage.DepositDiscovered,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	require.NoError(t, deps.Store.Deposits().UpdateStatus(ctx, dep.ID,
		storage.DepositDiscovered, storage.DepositReviewRequired, storage.DepositUpdate{}))
	item, err := deps.Store.Review().Open(ctx, storage.ReviewItem{
		ChainID: ctlChainID, Kind: storage.ReviewKindDeposit,
		RefID: strconv.FormatInt(dep.ID, 10), Reason: "unsupported shape", OpenedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	rec := postJSON(t, mux, fmt.Sprintf("/v1/review-queue/%d/resolve", item.ID),
		resolveRequest{Outcome: "DEPOSIT_REJECTED", Resolution: "not a customer deposit"})
	require.Equal(t, http.StatusOK, rec.Code)

	got, err := deps.Store.Deposits().GetByID(ctx, dep.ID)
	require.NoError(t, err)
	require.Equal(t, storage.DepositRejected, got.Status)
}

// TestReviewResolveLedgerAcknowledged: an immutable ledger-entry review resolves
// with no domain transition.
func TestReviewResolveLedgerAcknowledged(t *testing.T) {
	deps := controlDeps(t)
	mux := adminMux(deps)
	ctx := context.Background()

	item, err := deps.Store.Review().Open(ctx, storage.ReviewItem{
		ChainID: ctlChainID, Kind: storage.ReviewKindLedgerEntry, RefID: "88",
		Reason: "unattributed movement", OpenedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	// A ledger item rejects a deposit/withdrawal outcome.
	rec := postJSON(t, mux, fmt.Sprintf("/v1/review-queue/%d/resolve", item.ID),
		resolveRequest{Outcome: "DEPOSIT_APPROVED", Resolution: "wrong kind"})
	require.Equal(t, http.StatusBadRequest, rec.Code)

	rec = postJSON(t, mux, fmt.Sprintf("/v1/review-queue/%d/resolve", item.ID),
		resolveRequest{Outcome: "LEDGER_ACKNOWLEDGED", Resolution: "investigated, benign"})
	require.Equal(t, http.StatusOK, rec.Code)

	open, err := deps.Store.Review().ListOpen(ctx, ctlChainID, 0)
	require.NoError(t, err)
	require.Empty(t, open)
}

// TestReviewResolveWithdrawalRecordConflict: if the referenced withdrawal has
// already left REVIEW_REQUIRED, resolution conflicts rather than corrupting it.
func TestReviewResolveWithdrawalRecordConflict(t *testing.T) {
	deps := controlDeps(t)
	mux := adminMux(deps)
	ctx := context.Background()

	id := "wd-conflict"
	itemID := seedReviewWithdrawal(t, deps, id, 400, true, storage.SequenceReconciliationRequired)
	// Independently drive the withdrawal out of REVIEW_REQUIRED.
	require.NoError(t, deps.Store.Withdrawals().UpdateStatus(ctx, id,
		storage.WithdrawalReviewRequired, storage.WithdrawalConfirmed, storage.WithdrawalUpdate{}))

	rec := postJSON(t, mux, fmt.Sprintf("/v1/review-queue/%d/resolve", itemID),
		resolveRequest{Outcome: "WITHDRAWAL_FAILED", Resolution: "stale"})
	require.Equal(t, http.StatusConflict, rec.Code)

	// The review row stays open — nothing was silently resolved.
	open, err := deps.Store.Review().ListOpen(ctx, ctlChainID, 0)
	require.NoError(t, err)
	require.Len(t, open, 1)
}
