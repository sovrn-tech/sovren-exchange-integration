package main

// Typed review-queue resolution (FR-030/FR-035/FR-044).
//
// Resolving a review-queue row must do more than stamp resolved_at: the row
// only *points at* a deposit or withdrawal that is itself parked in
// REVIEW_REQUIRED, holding funds and (for withdrawals) a quarantined sequence
// reservation. If resolution left those domain records untouched they would
// stay REVIEW_REQUIRED forever — a CheckTx-rejected withdrawal would pin its
// funds and sequence indefinitely once identical-byte recovery stops making
// progress. So the operator supplies a typed OUTCOME describing the
// chain-truth they verified out of band, and this orchestrator applies the
// referenced record's terminal transition, the sequence/funds disposition,
// and the review-row resolve atomically in one transaction.
//
// Chain-truth verification is the operator's responsibility: this endpoint is
// the operator's instrument to *record* a verified determination (mirroring
// handleChainReviewResolve), not to re-derive it. It is reached only behind
// the admin mux (mTLS + admin authn).

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

// reviewOutcome is the typed disposition an operator applies to a review-queue
// item. Each outcome is legal only for a specific ReviewItemKind.
type reviewOutcome string

const (
	// outcomeWithdrawalConfirmed: operator verified the transaction landed on
	// chain. Withdrawal → CONFIRMED; sequence → CONSUMED (the slot is spent).
	// Requires a live signed reservation (a slot that may hold a signature),
	// not a genuinely pre-sign one.
	outcomeWithdrawalConfirmed reviewOutcome = "WITHDRAWAL_CONFIRMED"
	// outcomeWithdrawalFailed: for a withdrawal whose signing was reached (its
	// reservation is SIGNED / BROADCAST / RECONCILIATION_REQUIRED — a signature
	// may exist) but whose original did not land, and whose slot is now spent —
	// operator verified (chain truth) that the account sequence has advanced
	// past it, so any signature can never be redeemed. Withdrawal → FAILED;
	// sequence → CONSUMED (never RELEASED — releasing would re-issue a slot a
	// lingering signature could still claim, recreating the double-obligation
	// the quarantine prevents; see the sequence-management invariant in
	// data-model.md §6). Funds leave the committed set. A genuinely pre-sign
	// withdrawal is abandoned via WITHDRAWAL_CANCELLED instead.
	outcomeWithdrawalFailed reviewOutcome = "WITHDRAWAL_FAILED"
	// outcomeWithdrawalCancelled: operator abandons a genuinely PRE-SIGN
	// withdrawal — one with no persisted bytes AND a reservation that is absent,
	// still RESERVED, or already RELEASED (a startup-reconciled pre-sign slot),
	// so nothing could ever redeem the sequence. Withdrawal → CANCELLED;
	// sequence → RELEASED (re-issuable, or left RELEASED) and funds freed. A
	// withdrawal whose reservation is SIGNED / BROADCAST / RECONCILIATION_REQUIRED
	// (including a signer-failure quarantine that persisted no bytes) can NOT be
	// cancelled: a signature may exist, so it resolves via WITHDRAWAL_CONFIRMED
	// (included) or WITHDRAWAL_FAILED (sequence advanced) once chain truth is
	// established.
	outcomeWithdrawalCancelled reviewOutcome = "WITHDRAWAL_CANCELLED"
	// outcomeDepositApproved: operator accepts the deposit. It re-enters the
	// credit pipeline (REVIEW_REQUIRED → VALIDATED → AWAITING_CONFIRMATIONS);
	// the scanner's confirmation + credit passes then credit it under the
	// credit gate. Resolution never force-credits.
	outcomeDepositApproved reviewOutcome = "DEPOSIT_APPROVED"
	// outcomeDepositRejected: operator rejects the deposit. → REJECTED
	// (terminal, never credited).
	outcomeDepositRejected reviewOutcome = "DEPOSIT_REJECTED"
	// outcomeLedgerAcknowledged: operator has investigated an unattributed
	// ledger movement. The chain-transfer ledger is immutable, so there is no
	// domain record to transition and no funds/sequence to free; the review
	// row is simply resolved.
	outcomeLedgerAcknowledged reviewOutcome = "LEDGER_ACKNOWLEDGED"
)

// outcomesByKind lists the legal outcomes for each review-item kind.
var outcomesByKind = map[storage.ReviewItemKind][]reviewOutcome{
	storage.ReviewKindWithdrawal:  {outcomeWithdrawalConfirmed, outcomeWithdrawalFailed, outcomeWithdrawalCancelled},
	storage.ReviewKindDeposit:     {outcomeDepositApproved, outcomeDepositRejected},
	storage.ReviewKindLedgerEntry: {outcomeLedgerAcknowledged},
}

func outcomeLegalForKind(kind storage.ReviewItemKind, o reviewOutcome) bool {
	for _, allowed := range outcomesByKind[kind] {
		if allowed == o {
			return true
		}
	}
	return false
}

// allowedOutcomesFor renders the legal outcomes for a kind, for error text.
func allowedOutcomesFor(kind storage.ReviewItemKind) string {
	allowed := outcomesByKind[kind]
	strs := make([]string, len(allowed))
	for i, o := range allowed {
		strs[i] = string(o)
	}
	if len(strs) == 0 {
		return "(none)"
	}
	out := strs[0]
	for _, s := range strs[1:] {
		out += ", " + s
	}
	return out
}

// resolveReviewItem applies a typed resolution to review-queue item id: it
// transitions the referenced deposit/withdrawal, disposes the sequence/funds,
// and resolves the review row atomically. Returns the HTTP status + body.
func resolveReviewItem(ctx context.Context, deps *Deps, id int64, outcome reviewOutcome, note string, now time.Time) (int, any) {
	item, err := deps.Store.Review().Get(ctx, id)
	if err != nil {
		s, e := storageErrStatus(err)
		return s, e
	}
	if item.ResolvedAt != nil {
		return http.StatusConflict, apiError{Code: "STATUS_CONFLICT", Message: fmt.Sprintf("review item %d already resolved", id)}
	}
	if !outcomeLegalForKind(item.Kind, outcome) {
		return http.StatusBadRequest, apiError{
			Code:    "INVALID_REQUEST",
			Message: fmt.Sprintf("outcome %q is not valid for a %s review item; allowed: %s", outcome, item.Kind, allowedOutcomesFor(item.Kind)),
		}
	}

	// The typed outcome is persisted alongside the operator's free-text note so
	// the resolved row records exactly what was done.
	resolution := string(outcome) + ": " + note

	err = deps.Store.WithTx(ctx, func(ctx context.Context, st storage.Store) error {
		switch item.Kind {
		case storage.ReviewKindWithdrawal:
			if err := resolveWithdrawalReview(ctx, st, item.RefID, outcome); err != nil {
				return err
			}
		case storage.ReviewKindDeposit:
			if err := resolveDepositReview(ctx, st, item.RefID, outcome); err != nil {
				return err
			}
		case storage.ReviewKindLedgerEntry:
			// Immutable ledger entry: nothing to transition.
		default:
			return fmt.Errorf("%w: unknown review kind %q", storage.ErrInvalidRecord, item.Kind)
		}
		return st.Review().Resolve(ctx, id, resolution, now)
	})
	if err != nil {
		return reviewResolveErrStatus(err)
	}
	return http.StatusOK, map[string]any{"id": id, "resolved": true, "outcome": string(outcome)}
}

// resolveWithdrawalReview transitions a REVIEW_REQUIRED withdrawal to its
// operator-determined terminal state and disposes the bound sequence.
func resolveWithdrawalReview(ctx context.Context, st storage.Store, withdrawalID string, o reviewOutcome) error {
	w, err := st.Withdrawals().Get(ctx, withdrawalID)
	if err != nil {
		return err
	}
	if w.Status != storage.WithdrawalReviewRequired {
		return fmt.Errorf("%w: withdrawal %s is %s, not REVIEW_REQUIRED", storage.ErrStatusConflict, withdrawalID, w.Status)
	}

	// Release-safety keys on the RESERVATION STATE, not len(SignedTxBytes).
	// withdrawals.Workflow.Sign quarantines the reservation to
	// RECONCILIATION_REQUIRED on signer-verification / assembly failure WITHOUT
	// persisting bytes (workflow.go), yet the signer may hold a redeemable
	// signature. So a slot is genuinely pre-sign — and safe to CANCEL/RELEASE —
	// only when it has no persisted bytes AND the reservation is absent, still
	// RESERVED, or already RELEASED (startup sequences.Manager.ReconcileAccount
	// releases a no-signature RESERVED slot without touching this withdrawal or
	// its review row). Every other reservation state (SIGNED / BROADCAST /
	// RECONCILIATION_REQUIRED) may correspond to a live signature and must
	// resolve to CONSUMED, never RELEASED (data-model.md §6; mirrors the
	// reconciler and SumCommittedBySource's preSignReservationStatus).
	res, seqErr := st.Sequences().GetByWorkRef(ctx, storage.WorkRef{Kind: storage.WorkWithdrawal, ID: withdrawalID})
	hasReservation := true
	if errors.Is(seqErr, storage.ErrNotFound) {
		hasReservation = false
	} else if seqErr != nil {
		return seqErr
	}
	preSign := len(w.SignedTxBytes) == 0 &&
		(!hasReservation ||
			res.Status == storage.SequenceReserved ||
			res.Status == storage.SequenceReleased)

	var (
		target storage.WithdrawalStatus
		seqTo  storage.SequenceReservationStatus
	)
	switch o {
	case outcomeWithdrawalConfirmed:
		// Nothing could have landed if the reservation never left RESERVED.
		if preSign {
			return fmt.Errorf("%w: withdrawal %s has no live signed reservation; a pre-sign withdrawal cannot be CONFIRMED — abandon it via WITHDRAWAL_CANCELLED", storage.ErrStatusConflict, withdrawalID)
		}
		target, seqTo = storage.WithdrawalConfirmed, storage.SequenceConsumed
	case outcomeWithdrawalFailed:
		// FAILED means the slot is spent (chain truth: sequence advanced past
		// this tx, or the tx executed and failed) — CONSUMED, never RELEASED.
		// Valid even when the adapter never persisted bytes (a signer-failure
		// quarantine), because the reservation left RESERVED.
		if preSign {
			return fmt.Errorf("%w: withdrawal %s has no live signed reservation; a pre-sign withdrawal is abandoned via WITHDRAWAL_CANCELLED, not WITHDRAWAL_FAILED", storage.ErrStatusConflict, withdrawalID)
		}
		target, seqTo = storage.WithdrawalFailed, storage.SequenceConsumed
	case outcomeWithdrawalCancelled:
		// Release is only safe when nothing could ever redeem the slot: no
		// persisted bytes AND the reservation is absent or still RESERVED.
		if !preSign {
			return fmt.Errorf("%w: withdrawal %s has a live or quarantined signed reservation; its slot may hold a redeemable signature and cannot be released — use WITHDRAWAL_CONFIRMED (included) or WITHDRAWAL_FAILED (sequence advanced) after verifying chain truth", storage.ErrStatusConflict, withdrawalID)
		}
		target, seqTo = storage.WithdrawalCancelled, storage.SequenceReleased
	default:
		return fmt.Errorf("%w: outcome %q not applicable to a withdrawal", storage.ErrInvalidRecord, o)
	}

	if err := st.Withdrawals().UpdateStatus(ctx, withdrawalID,
		storage.WithdrawalReviewRequired, target, storage.WithdrawalUpdate{}); err != nil {
		return err
	}
	if !hasReservation {
		return nil // genuinely pre-sign with no reservation: nothing to dispose
	}
	return disposeReservation(ctx, st, withdrawalID, seqTo)
}

// disposeReservation drives the withdrawal's sequence reservation to its
// terminal disposition. A withdrawal may have no reservation (a review opened
// before ReserveSequence) — that is not an error. The (signed → CONSUMED,
// unsigned → RELEASED) rule in resolveWithdrawalReview always yields a
// transition that is directly legal from the reservation's resting status
// (§6): CONSUMED from SIGNED / BROADCAST / RECONCILIATION_REQUIRED, RELEASED
// from RESERVED. Any other pair is an unexpected state and is surfaced as an
// ErrIllegalTransition (→ 409) by UpdateStatus rather than silently coerced.
func disposeReservation(ctx context.Context, st storage.Store, withdrawalID string, to storage.SequenceReservationStatus) error {
	res, err := st.Sequences().GetByWorkRef(ctx, storage.WorkRef{Kind: storage.WorkWithdrawal, ID: withdrawalID})
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if res.Status == to {
		return nil
	}
	return st.Sequences().UpdateStatus(ctx, res.ID, res.Status, to)
}

// resolveDepositReview transitions a REVIEW_REQUIRED deposit. APPROVED
// re-enters the credit pipeline (never force-credits); REJECTED is terminal.
func resolveDepositReview(ctx context.Context, st storage.Store, refID string, o reviewOutcome) error {
	id, err := strconv.ParseInt(refID, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: deposit review ref %q is not an integer id", storage.ErrInvalidRecord, refID)
	}
	d, err := st.Deposits().GetByID(ctx, id)
	if err != nil {
		return err
	}
	if d.Status != storage.DepositReviewRequired {
		return fmt.Errorf("%w: deposit %d is %s, not REVIEW_REQUIRED", storage.ErrStatusConflict, id, d.Status)
	}
	switch o {
	case outcomeDepositRejected:
		return st.Deposits().UpdateStatus(ctx, id,
			storage.DepositReviewRequired, storage.DepositRejected, storage.DepositUpdate{})
	case outcomeDepositApproved:
		// REVIEW_REQUIRED → VALIDATED → AWAITING_CONFIRMATIONS. Operator
		// approval overrides the minimum-deposit park (they have explicitly
		// accepted this deposit); the scanner then promotes it to CREDITABLE
		// and credits it under the credit gate.
		if err := st.Deposits().UpdateStatus(ctx, id,
			storage.DepositReviewRequired, storage.DepositValidated, storage.DepositUpdate{}); err != nil {
			return err
		}
		return st.Deposits().UpdateStatus(ctx, id,
			storage.DepositValidated, storage.DepositAwaitingConfirmations, storage.DepositUpdate{})
	default:
		return fmt.Errorf("%w: outcome %q not applicable to a deposit", storage.ErrInvalidRecord, o)
	}
}

// reviewResolveErrStatus maps orchestration errors to HTTP status. Beyond the
// shared mapping it treats an illegal transition (the referenced record moved
// out of REVIEW_REQUIRED concurrently) as a conflict, and an invalid-record
// error (bad ref, wrong outcome for state) as a 400.
func reviewResolveErrStatus(err error) (int, any) {
	switch {
	case errors.Is(err, storage.ErrIllegalTransition):
		return http.StatusConflict, apiError{Code: "STATUS_CONFLICT", Message: err.Error()}
	case errors.Is(err, storage.ErrInvalidRecord):
		return http.StatusBadRequest, apiError{Code: "INVALID_REQUEST", Message: err.Error()}
	default:
		s, e := storageErrStatus(err)
		return s, e
	}
}
