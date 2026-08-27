package withdrawals

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/logging"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

// Outcome is one of the eight FR-035 broadcaster status distinctions.
type Outcome string

const (
	// OutcomeEncodingFailed: the transaction could not be locally encoded.
	OutcomeEncodingFailed Outcome = "LOCAL_ENCODING_FAILED"
	// OutcomeSignatureFailed: the signature failed verification locally.
	OutcomeSignatureFailed Outcome = "SIGNATURE_FAILED"
	// OutcomeCheckTxRejected: node-side pre-inclusion rejection (CheckTx).
	OutcomeCheckTxRejected Outcome = "CHECKTX_REJECTED"
	// OutcomeMempoolAccepted: CheckTx passed; the tx is in the mempool.
	OutcomeMempoolAccepted Outcome = "MEMPOOL_ACCEPTED"
	// OutcomeIncluded: the tx is in a block; execution result not yet final
	// for the workflow (confirmation depth pending).
	OutcomeIncluded Outcome = "BLOCK_INCLUDED"
	// OutcomeExecSuccess: DeliverTx code 0 at the required depth.
	OutcomeExecSuccess Outcome = "EXECUTION_SUCCESS"
	// OutcomeExecFailed: included but DeliverTx code != 0 — reported
	// accurately, the transfer did NOT happen (fee was still deducted).
	OutcomeExecFailed Outcome = "EXECUTION_FAILED"
	// OutcomeUnknownTimeout: broadcast result unknown after timeout and the
	// original-tx search found nothing — quarantined, never re-signed.
	OutcomeUnknownTimeout Outcome = "UNKNOWN_AFTER_TIMEOUT"
)

// Broadcast pushes a SIGNED withdrawal's persisted bytes to the chain
// (FR-035). It never signs: only rec.SignedTxBytes ever reach the wire.
//
//   - CheckTx rejection ⇒ SIGNED → REVIEW_REQUIRED with the node's code and
//     log; the sequence reservation is quarantined. Not FAILED: the signed
//     bytes persist and could still be accepted by another node, so the funds
//     must stay committed (SumCommittedBySource counts a REVIEW_REQUIRED
//     record that still holds signed bytes) — never auto-released.
//   - Acceptance ⇒ BROADCAST (mempool accepted); inclusion and execution are
//     Confirm's job.
//   - Transport error/timeout ⇒ the unknown flow: search for the original tx
//     by hash; found ⇒ treat as accepted; not found ⇒ withdrawal
//     REVIEW_REQUIRED + reservation RECONCILIATION_REQUIRED. A second signed
//     transaction for this withdrawal is impossible from any path here.
func (w *Workflow) Broadcast(ctx context.Context, withdrawalID string) (Outcome, error) {
	rec, err := w.store.Withdrawals().Get(ctx, withdrawalID)
	if err != nil {
		return "", err
	}
	if rec.Status != storage.WithdrawalSigned {
		return "", fmt.Errorf("%w: withdrawal %s is %s", storage.ErrStatusConflict, withdrawalID, rec.Status)
	}
	if len(rec.SignedTxBytes) == 0 || rec.TxHash == nil {
		return "", fmt.Errorf("%w: withdrawal %s SIGNED without persisted bytes", storage.ErrInvalidRecord, withdrawalID)
	}
	controls, err := w.store.Controls().Get(ctx, rec.ChainID)
	if err != nil {
		return "", err
	}
	if controls.BroadcastPaused {
		return "", fmt.Errorf("%w: broadcast", ErrPaused)
	}

	res, err := w.chain.Broadcast(ctx, rec.SignedTxBytes, client.BroadcastSync)
	if err != nil {
		// Unknown outcome: the node may or may not have the tx. Search
		// first; never blind-retry, never re-sign (FR-035).
		return w.resolveUnknownAfterBroadcast(ctx, rec, err)
	}
	if !res.Accepted {
		// CheckTx pre-inclusion rejection. NOT terminal: the node said no, but
		// the persisted signed bytes could still be accepted by another node,
		// so the withdrawal goes to REVIEW_REQUIRED (not FAILED) and its
		// sequence is quarantined. FAILED means terminal + sequence CONSUMED +
		// funds released — correct only once the tx is included and DeliverTx
		// fails (the Confirm path below), where the sequence IS consumed.
		// REVIEW_REQUIRED means uncertain + may still land + funds committed:
		// SumCommittedBySource counts a REVIEW_REQUIRED record that still holds
		// signed bytes, so a later withdrawal cannot reserve the same balance
		// and take the next sequence, leaving two signed obligations exceeding
		// the wallet. The node's code and log stay on the record for operators.
		w.countFailed(rec.ChainID, "checktx")
		if err := w.store.WithTx(ctx, func(ctx context.Context, st storage.Store) error {
			if err := st.Withdrawals().UpdateStatus(ctx, withdrawalID,
				storage.WithdrawalSigned, storage.WithdrawalReviewRequired, storage.WithdrawalUpdate{
					TxCode: &res.Code, RawLog: &res.RawLog,
				}); err != nil {
				return err
			}
			if _, err := st.Review().Open(ctx, storage.ReviewItem{
				ChainID: rec.ChainID,
				Kind:    storage.ReviewKindWithdrawal,
				RefID:   withdrawalID,
				Reason:  fmt.Sprintf("checktx rejected: code %d: %s; signed bytes persist and may still be accepted elsewhere — reconcile before releasing funds or sequence", res.Code, res.RawLog),
			}); err != nil {
				return err
			}
			return w.quarantineReservation(ctx, st, withdrawalID, storage.SequenceSigned)
		}); err != nil {
			return OutcomeCheckTxRejected, err
		}
		return OutcomeCheckTxRejected, nil
	}

	if err := w.store.WithTx(ctx, func(ctx context.Context, st storage.Store) error {
		if err := st.Withdrawals().UpdateStatus(ctx, withdrawalID,
			storage.WithdrawalSigned, storage.WithdrawalBroadcast, storage.WithdrawalUpdate{}); err != nil {
			return err
		}
		return w.advanceReservation(ctx, st, withdrawalID, storage.SequenceSigned, storage.SequenceBroadcast)
	}); err != nil {
		return OutcomeMempoolAccepted, err
	}
	if w.metrics != nil {
		w.metrics.WithdrawalsBroadcast.WithLabelValues(rec.ChainID).Inc()
	}
	return OutcomeMempoolAccepted, nil
}

// resolveUnknownAfterBroadcast handles a broadcast whose result is unknown:
// GetTx search first; found ⇒ the tx made it (proceed as accepted); not
// found ⇒ quarantine. The persisted bytes remain the only recovery path.
func (w *Workflow) resolveUnknownAfterBroadcast(ctx context.Context, rec storage.WithdrawalRecord, cause error) (Outcome, error) {
	info, err := w.chain.Tx(ctx, *rec.TxHash)
	if err == nil && info != nil {
		if err := w.store.WithTx(ctx, func(ctx context.Context, st storage.Store) error {
			if err := st.Withdrawals().UpdateStatus(ctx, rec.WithdrawalID,
				storage.WithdrawalSigned, storage.WithdrawalBroadcast, storage.WithdrawalUpdate{}); err != nil {
				return err
			}
			return w.advanceReservation(ctx, st, rec.WithdrawalID, storage.SequenceSigned, storage.SequenceBroadcast)
		}); err != nil {
			return OutcomeMempoolAccepted, err
		}
		return OutcomeMempoolAccepted, nil
	}

	w.logger.Warn("broadcast outcome unknown; quarantining after search",
		logging.FieldChainID, rec.ChainID,
		logging.FieldWithdrawalID, rec.WithdrawalID,
		logging.FieldTxHash, *rec.TxHash,
		logging.FieldErrorCode, string(OutcomeUnknownTimeout),
	)
	w.countFailed(rec.ChainID, "broadcast_unknown")
	if err := w.store.WithTx(ctx, func(ctx context.Context, st storage.Store) error {
		if err := st.Withdrawals().UpdateStatus(ctx, rec.WithdrawalID,
			storage.WithdrawalSigned, storage.WithdrawalReviewRequired, storage.WithdrawalUpdate{}); err != nil {
			return err
		}
		if _, err := st.Review().Open(ctx, storage.ReviewItem{
			ChainID: rec.ChainID,
			Kind:    storage.ReviewKindWithdrawal,
			RefID:   rec.WithdrawalID,
			Reason:  fmt.Sprintf("broadcast unknown: %v; tx %s not found; rebroadcast persisted bytes after review", cause, *rec.TxHash),
		}); err != nil {
			return err
		}
		return w.quarantineReservation(ctx, st, rec.WithdrawalID, storage.SequenceSigned)
	}); err != nil {
		return OutcomeUnknownTimeout, err
	}
	return OutcomeUnknownTimeout, fmt.Errorf("%w: broadcast unknown: %v", ErrQuarantined, cause)
}

// Confirm advances a BROADCAST or INCLUDED withdrawal from chain truth:
// inclusion ⇒ INCLUDED (height, code, log persisted; reservation CONSUMED);
// DeliverTx failure ⇒ FAILED with the accurate execution report; success at
// the configured depth ⇒ CONFIRMED. A BROADCAST withdrawal whose tx cannot
// be found after BroadcastTimeout enters the unknown flow (REVIEW_REQUIRED +
// reservation quarantine) — never a re-sign.
func (w *Workflow) Confirm(ctx context.Context, withdrawalID string) (Outcome, error) {
	rec, err := w.store.Withdrawals().Get(ctx, withdrawalID)
	if err != nil {
		return "", err
	}
	switch rec.Status {
	case storage.WithdrawalBroadcast, storage.WithdrawalIncluded:
	default:
		return "", fmt.Errorf("%w: withdrawal %s is %s", storage.ErrStatusConflict, withdrawalID, rec.Status)
	}
	if rec.TxHash == nil {
		return "", fmt.Errorf("%w: withdrawal %s has no tx hash", storage.ErrInvalidRecord, withdrawalID)
	}

	info, err := w.chain.Tx(ctx, *rec.TxHash)
	if err != nil && !errors.Is(err, client.ErrNotFound) {
		return "", fmt.Errorf("withdrawals: tx lookup: %w", err)
	}

	if info == nil || info.Height == 0 {
		if rec.Status == storage.WithdrawalIncluded {
			// Previously seen included but now unfindable: node disagreement
			// or reorg — operator review.
			return OutcomeUnknownTimeout, w.quarantine(ctx, withdrawalID, rec.Status,
				fmt.Sprintf("tx %s no longer found after inclusion", *rec.TxHash))
		}
		if time.Since(rec.UpdatedAt) < w.cfg.BroadcastTimeout {
			return OutcomeMempoolAccepted, nil
		}
		w.countFailed(rec.ChainID, "confirm_unknown")
		if err := w.store.WithTx(ctx, func(ctx context.Context, st storage.Store) error {
			if err := st.Withdrawals().UpdateStatus(ctx, withdrawalID,
				storage.WithdrawalBroadcast, storage.WithdrawalReviewRequired, storage.WithdrawalUpdate{}); err != nil {
				return err
			}
			if _, err := st.Review().Open(ctx, storage.ReviewItem{
				ChainID: rec.ChainID,
				Kind:    storage.ReviewKindWithdrawal,
				RefID:   withdrawalID,
				Reason:  fmt.Sprintf("tx %s unknown after broadcast timeout; rebroadcast persisted bytes after review", *rec.TxHash),
			}); err != nil {
				return err
			}
			return w.quarantineReservation(ctx, st, withdrawalID, storage.SequenceBroadcast)
		}); err != nil {
			return OutcomeUnknownTimeout, err
		}
		return OutcomeUnknownTimeout, fmt.Errorf("%w: unknown after timeout", ErrQuarantined)
	}

	if rec.Status == storage.WithdrawalBroadcast {
		height := uint64(info.Height)
		if err := w.store.WithTx(ctx, func(ctx context.Context, st storage.Store) error {
			if err := st.Withdrawals().UpdateStatus(ctx, withdrawalID,
				storage.WithdrawalBroadcast, storage.WithdrawalIncluded, storage.WithdrawalUpdate{
					BlockHeight: &height,
					TxCode:      &info.Code,
					RawLog:      &info.RawLog,
				}); err != nil {
				return err
			}
			return w.consumeReservation(ctx, st, withdrawalID)
		}); err != nil {
			return OutcomeIncluded, err
		}
		rec.Status = storage.WithdrawalIncluded
	}

	if info.Code != 0 {
		// Execution failed: the transfer did not happen. Report accurately —
		// never CONFIRMED, never retried implicitly (the sequence WAS
		// consumed and the fee WAS deducted).
		w.countFailed(rec.ChainID, "delivertx")
		if err := w.store.Withdrawals().UpdateStatus(ctx, withdrawalID,
			storage.WithdrawalIncluded, storage.WithdrawalFailed, storage.WithdrawalUpdate{
				TxCode: &info.Code, RawLog: &info.RawLog,
			}); err != nil {
			return OutcomeExecFailed, err
		}
		return OutcomeExecFailed, nil
	}

	status, err := w.chain.NodeStatus(ctx)
	if err != nil {
		return OutcomeIncluded, fmt.Errorf("withdrawals: node status: %w", err)
	}
	if uint64(status.LatestHeight-info.Height)+1 < w.cfg.Confirmations {
		return OutcomeIncluded, nil
	}
	if err := w.store.Withdrawals().UpdateStatus(ctx, withdrawalID,
		storage.WithdrawalIncluded, storage.WithdrawalConfirmed, storage.WithdrawalUpdate{}); err != nil {
		return OutcomeExecSuccess, err
	}
	if w.metrics != nil {
		w.metrics.WithdrawalsConfirmed.WithLabelValues(rec.ChainID).Inc()
	}
	return OutcomeExecSuccess, nil
}

// Recover drives a REVIEW_REQUIRED withdrawal that holds persisted signed
// bytes: search-first, then rebroadcast the identical bytes via the
// sequence manager (never re-sign). On success the withdrawal returns to
// the chain-truth status; the reservation leaves quarantine only when the
// transaction is observed on chain.
func (w *Workflow) Recover(ctx context.Context, withdrawalID string) (Outcome, error) {
	rec, err := w.store.Withdrawals().Get(ctx, withdrawalID)
	if err != nil {
		return "", err
	}
	if rec.Status != storage.WithdrawalReviewRequired {
		return "", fmt.Errorf("%w: withdrawal %s is %s", storage.ErrStatusConflict, withdrawalID, rec.Status)
	}
	if len(rec.SignedTxBytes) == 0 {
		return "", fmt.Errorf("%w: withdrawal %s has no persisted signed bytes; operator must resolve", storage.ErrInvalidRecord, withdrawalID)
	}
	res, err := w.seq.RebroadcastPersisted(ctx, rec.SignedTxBytes)
	if err != nil {
		return "", err
	}
	switch {
	case res.AlreadyIncluded:
		if err := w.store.Withdrawals().UpdateStatus(ctx, withdrawalID,
			storage.WithdrawalReviewRequired, storage.WithdrawalBroadcast, storage.WithdrawalUpdate{}); err != nil {
			return OutcomeIncluded, err
		}
		return OutcomeIncluded, nil
	case res.Accepted:
		if err := w.store.Withdrawals().UpdateStatus(ctx, withdrawalID,
			storage.WithdrawalReviewRequired, storage.WithdrawalBroadcast, storage.WithdrawalUpdate{}); err != nil {
			return OutcomeMempoolAccepted, err
		}
		return OutcomeMempoolAccepted, nil
	default:
		return OutcomeCheckTxRejected, fmt.Errorf("withdrawals: rebroadcast rejected: code %d: %s", res.Code, res.RawLog)
	}
}

// advanceReservation moves the withdrawal's reservation from → to, treating
// a concurrent identical move as success.
func (w *Workflow) advanceReservation(ctx context.Context, st storage.Store, withdrawalID string, from, to storage.SequenceReservationStatus) error {
	res, err := st.Sequences().GetByWorkRef(ctx, storage.WorkRef{Kind: storage.WorkWithdrawal, ID: withdrawalID})
	if err != nil {
		return err
	}
	if res.Status == to {
		return nil
	}
	err = st.Sequences().UpdateStatus(ctx, res.ID, from, to)
	if errors.Is(err, storage.ErrStatusConflict) {
		return nil
	}
	return err
}

// consumeReservation marks the reservation CONSUMED from whichever live
// status it holds (inclusion on chain is definitive).
func (w *Workflow) consumeReservation(ctx context.Context, st storage.Store, withdrawalID string) error {
	res, err := st.Sequences().GetByWorkRef(ctx, storage.WorkRef{Kind: storage.WorkWithdrawal, ID: withdrawalID})
	if err != nil {
		return err
	}
	switch res.Status {
	case storage.SequenceConsumed:
		return nil
	case storage.SequenceSigned, storage.SequenceBroadcast, storage.SequenceReconciliationRequired:
		err := st.Sequences().UpdateStatus(ctx, res.ID, res.Status, storage.SequenceConsumed)
		if errors.Is(err, storage.ErrStatusConflict) {
			return nil
		}
		return err
	default:
		return fmt.Errorf("%w: reservation for %s is %s at inclusion", storage.ErrStatusConflict, withdrawalID, res.Status)
	}
}

// quarantineReservation moves the reservation to RECONCILIATION_REQUIRED,
// tolerating races with reconciliation.
func (w *Workflow) quarantineReservation(ctx context.Context, st storage.Store, withdrawalID string, from storage.SequenceReservationStatus) error {
	res, err := st.Sequences().GetByWorkRef(ctx, storage.WorkRef{Kind: storage.WorkWithdrawal, ID: withdrawalID})
	if err != nil {
		return err
	}
	if res.Status == storage.SequenceReconciliationRequired || res.Status == storage.SequenceConsumed {
		return nil
	}
	err = st.Sequences().UpdateStatus(ctx, res.ID, from, storage.SequenceReconciliationRequired)
	if errors.Is(err, storage.ErrStatusConflict) {
		return nil
	}
	return err
}
