package sweeps

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/logging"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

// Broadcast pushes a SIGNED sweep's persisted bytes to the chain. It never
// signs: only j.SignedTxBytes ever reach the wire.
//
//   - CheckTx rejection ⇒ BROADCAST (code recorded) → FAILED; the
//     reservation is quarantined (signed bytes exist and could still be
//     accepted elsewhere — never auto-released). Covered deposits stay
//     SWEEP_PENDING and re-attach to the next planned sweep.
//   - Acceptance ⇒ BROADCAST; inclusion and depth are Confirm's job.
//   - Transport error / timeout ⇒ the unknown flow: search for the
//     original tx by hash first; found ⇒ treat as accepted; not found ⇒
//     the sweep HOLDS its SIGNED status (still the account's single live
//     sweep) and the reservation is quarantined. Recovery is Recover —
//     rebroadcasting the identical persisted bytes.
func (e *Engine) Broadcast(ctx context.Context, sweepID string) error {
	j, err := e.store.Sweeps().Get(ctx, sweepID)
	if err != nil {
		return err
	}
	if j.Status != storage.SweepSigned {
		return fmt.Errorf("%w: sweep %s is %s", storage.ErrStatusConflict, sweepID, j.Status)
	}
	if len(j.SignedTxBytes) == 0 || j.TxHash == nil {
		return fmt.Errorf("%w: sweep %s SIGNED without persisted bytes", storage.ErrInvalidRecord, sweepID)
	}
	controls, err := e.controls(ctx)
	if err != nil {
		return err
	}
	if controls.SweepPaused {
		return fmt.Errorf("%w: sweep", ErrPaused)
	}
	if controls.BroadcastPaused {
		return fmt.Errorf("%w: broadcast", ErrPaused)
	}

	res, err := e.chain.Broadcast(ctx, j.SignedTxBytes, client.BroadcastSync)
	if err != nil {
		// Unknown outcome: the node may or may not hold the tx. Search
		// first; never blind-retry, never re-sign.
		return e.resolveUnknownAfterBroadcast(ctx, j, err)
	}
	if !res.Accepted {
		e.logger.Warn("sweep rejected by CheckTx",
			logging.FieldChainID, j.ChainID,
			logging.FieldSweepID, j.SweepID,
			logging.FieldTxHash, *j.TxHash,
			"code", res.Code, "raw_log", res.RawLog,
		)
		return e.store.WithTx(ctx, func(ctx context.Context, st storage.Store) error {
			if err := st.Sweeps().UpdateStatus(ctx, sweepID, storage.SweepSigned, storage.SweepBroadcast, storage.SweepUpdate{
				TxCode: &res.Code,
			}); err != nil {
				return err
			}
			if err := st.Sweeps().UpdateStatus(ctx, sweepID, storage.SweepBroadcast, storage.SweepFailed, storage.SweepUpdate{}); err != nil {
				return err
			}
			return e.quarantineReservation(ctx, st, sweepID, storage.SequenceSigned)
		})
	}

	if err := e.store.WithTx(ctx, func(ctx context.Context, st storage.Store) error {
		if err := st.Sweeps().UpdateStatus(ctx, sweepID, storage.SweepSigned, storage.SweepBroadcast, storage.SweepUpdate{}); err != nil {
			return err
		}
		return e.advanceReservation(ctx, st, sweepID, storage.SequenceSigned, storage.SequenceBroadcast)
	}); err != nil {
		return err
	}
	e.logger.Info("sweep broadcast",
		logging.FieldChainID, j.ChainID,
		logging.FieldSweepID, j.SweepID,
		logging.FieldTxHash, *j.TxHash,
	)
	return nil
}

// resolveUnknownAfterBroadcast handles a broadcast whose result is unknown:
// GetTx search first; found ⇒ the tx made it (proceed as accepted); not
// found ⇒ quarantine the reservation. The persisted bytes remain the only
// recovery path.
func (e *Engine) resolveUnknownAfterBroadcast(ctx context.Context, j storage.SweepJob, cause error) error {
	if info, err := e.chain.Tx(ctx, *j.TxHash); err == nil && info != nil {
		return e.store.WithTx(ctx, func(ctx context.Context, st storage.Store) error {
			if err := st.Sweeps().UpdateStatus(ctx, j.SweepID, storage.SweepSigned, storage.SweepBroadcast, storage.SweepUpdate{}); err != nil {
				return err
			}
			return e.advanceReservation(ctx, st, j.SweepID, storage.SequenceSigned, storage.SequenceBroadcast)
		})
	}

	e.logger.Warn("sweep broadcast outcome unknown; quarantining after search",
		logging.FieldChainID, j.ChainID,
		logging.FieldSweepID, j.SweepID,
		logging.FieldTxHash, *j.TxHash,
		logging.FieldErrorCode, "UNKNOWN_AFTER_TIMEOUT",
	)
	if err := e.store.WithTx(ctx, func(ctx context.Context, st storage.Store) error {
		return e.quarantineReservation(ctx, st, j.SweepID, storage.SequenceSigned)
	}); err != nil {
		return err
	}
	return fmt.Errorf("%w: broadcast unknown: %v; rebroadcast persisted bytes via Recover", ErrQuarantined, cause)
}

// Confirm advances a BROADCAST sweep from chain truth: DeliverTx failure ⇒
// FAILED with the accurate code (the sequence WAS consumed and deposits
// stay SWEEP_PENDING for the next sweep); success at the configured depth
// ⇒ CONFIRMED, the reservation CONSUMED, and every covered deposit flipped
// SWEEP_PENDING → SWEPT in the same transaction. A sweep whose tx cannot
// be found after BroadcastTimeout has its reservation quarantined — never
// a re-sign.
func (e *Engine) Confirm(ctx context.Context, sweepID string) error {
	j, err := e.store.Sweeps().Get(ctx, sweepID)
	if err != nil {
		return err
	}
	if j.Status != storage.SweepBroadcast {
		return fmt.Errorf("%w: sweep %s is %s", storage.ErrStatusConflict, sweepID, j.Status)
	}
	if j.TxHash == nil {
		return fmt.Errorf("%w: sweep %s has no tx hash", storage.ErrInvalidRecord, sweepID)
	}

	info, err := e.chain.Tx(ctx, *j.TxHash)
	if err != nil && !errors.Is(err, client.ErrNotFound) {
		return fmt.Errorf("sweeps: tx lookup: %w", err)
	}

	if info == nil || info.Height == 0 {
		if time.Since(j.UpdatedAt) < e.cfg.BroadcastTimeout {
			return nil // still propagating
		}
		e.logger.Warn("sweep unknown after broadcast timeout; quarantining",
			logging.FieldChainID, j.ChainID,
			logging.FieldSweepID, j.SweepID,
			logging.FieldTxHash, *j.TxHash,
		)
		if err := e.store.WithTx(ctx, func(ctx context.Context, st storage.Store) error {
			return e.quarantineReservation(ctx, st, sweepID, storage.SequenceBroadcast)
		}); err != nil {
			return err
		}
		return fmt.Errorf("%w: unknown after timeout; rebroadcast persisted bytes via Recover", ErrQuarantined)
	}

	if info.Code != 0 {
		// Execution failed: the transfer did not happen. Report accurately;
		// the fee was still deducted and the sequence consumed.
		e.logger.Warn("sweep execution failed",
			logging.FieldChainID, j.ChainID,
			logging.FieldSweepID, j.SweepID,
			logging.FieldTxHash, *j.TxHash,
			"code", info.Code, "raw_log", info.RawLog,
		)
		return e.store.WithTx(ctx, func(ctx context.Context, st storage.Store) error {
			if err := st.Sweeps().UpdateStatus(ctx, sweepID, storage.SweepBroadcast, storage.SweepFailed, storage.SweepUpdate{
				TxCode: &info.Code,
			}); err != nil {
				return err
			}
			return e.consumeReservation(ctx, st, sweepID)
		})
	}

	status, err := e.chain.NodeStatus(ctx)
	if err != nil {
		return fmt.Errorf("sweeps: node status: %w", err)
	}
	if uint64(status.LatestHeight-info.Height)+1 < e.cfg.Confirmations {
		return nil // depth pending
	}

	if err := e.store.WithTx(ctx, func(ctx context.Context, st storage.Store) error {
		if err := st.Sweeps().UpdateStatus(ctx, sweepID, storage.SweepBroadcast, storage.SweepConfirmed, storage.SweepUpdate{
			TxCode: &info.Code,
		}); err != nil {
			return err
		}
		if err := e.consumeReservation(ctx, st, sweepID); err != nil {
			return err
		}
		if err := e.markDepositsSwept(ctx, st, j, *j.TxHash); err != nil {
			return err
		}
		// Record fee-wallet spend ATOMICALLY with the confirm that frees the
		// fee wallet's reservation slot, so the spend cap sees every confirmed
		// leg before the next one can start (broadcast/steps: the cap reads
		// fee_funding_spends, never the async scanner ledger).
		return e.recordFeeFundingSpend(ctx, st, j, uint64(info.Height))
	}); err != nil {
		return err
	}
	if IsFundingJob(j) {
		// A confirmed FEE_FUND funding leg is real fee-wallet spend; also bump
		// the real-time counter behind the abnormal-fee-funding-volume alert.
		e.countFeeFunding(j.AmountBaseUnits)
	}
	e.logger.Info("sweep confirmed",
		logging.FieldChainID, j.ChainID,
		logging.FieldSweepID, j.SweepID,
		logging.FieldTxHash, *j.TxHash,
		logging.FieldHeight, info.Height,
		"deposits_swept", len(j.DepositIDs),
	)
	return nil
}

// Recover drives a quarantined sweep (SIGNED or BROADCAST with a
// RECONCILIATION_REQUIRED reservation) by searching for and, only when
// absent, rebroadcasting the EXACT persisted bytes via the sequence
// manager. Re-signing is structurally impossible here. The reservation
// leaves quarantine only when Confirm observes the transaction on chain.
func (e *Engine) Recover(ctx context.Context, sweepID string) error {
	j, err := e.store.Sweeps().Get(ctx, sweepID)
	if err != nil {
		return err
	}
	if j.Status != storage.SweepSigned && j.Status != storage.SweepBroadcast {
		return fmt.Errorf("%w: sweep %s is %s", storage.ErrStatusConflict, sweepID, j.Status)
	}
	if len(j.SignedTxBytes) == 0 {
		return fmt.Errorf("%w: sweep %s has no persisted signed bytes; operator must resolve", storage.ErrInvalidRecord, sweepID)
	}
	res, err := e.seq.RebroadcastPersisted(ctx, j.SignedTxBytes)
	if err != nil {
		return err
	}
	if !res.AlreadyIncluded && !res.Accepted {
		return fmt.Errorf("sweeps: rebroadcast rejected: code %d: %s", res.Code, res.RawLog)
	}
	if j.Status == storage.SweepSigned {
		if err := e.store.Sweeps().UpdateStatus(ctx, sweepID, storage.SweepSigned, storage.SweepBroadcast, storage.SweepUpdate{}); err != nil {
			return err
		}
	}
	e.logger.Info("sweep recovered by rebroadcasting persisted bytes",
		logging.FieldChainID, j.ChainID,
		logging.FieldSweepID, j.SweepID,
		logging.FieldTxHash, res.TxHash,
		"already_included", res.AlreadyIncluded,
	)
	return nil
}

// PassReport summarizes one Pass.
type PassReport struct {
	Plan   PlanReport
	Errors []error
}

// Pass runs one full sweeper iteration: plan new jobs, then advance every
// live job one step (PENDING/BUILT → Prepare, SIGNED → Broadcast,
// BROADCAST → Confirm, DEFERRED → Revisit). Hold-type conditions (pauses,
// funding in flight, signer/simulation outages, deferrals, lost optimistic
// races) are by-design waits, not errors.
func (e *Engine) Pass(ctx context.Context) PassReport {
	var report PassReport
	plan, err := e.Plan(ctx)
	report.Plan = plan
	if err != nil {
		if !IsHold(err) {
			report.Errors = append(report.Errors, fmt.Errorf("plan: %w", err))
		}
		return report
	}

	steps := []struct {
		status storage.SweepStatus
		step   func(context.Context, string) error
	}{
		{storage.SweepPending, e.Prepare},
		{storage.SweepBuilt, e.Prepare},
		{storage.SweepSigned, e.Broadcast},
		{storage.SweepBroadcast, e.Confirm},
		{storage.SweepDeferred, func(ctx context.Context, id string) error {
			_, err := e.Revisit(ctx, id)
			return err
		}},
	}
	for _, s := range steps {
		if ctx.Err() != nil {
			return report
		}
		jobs, err := e.store.Sweeps().ListByStatus(ctx, e.cfg.ChainID, s.status, 50)
		if err != nil {
			report.Errors = append(report.Errors, fmt.Errorf("list %s: %w", s.status, err))
			continue
		}
		for _, j := range jobs {
			if err := s.step(ctx, j.SweepID); err != nil && !IsHold(err) {
				report.Errors = append(report.Errors, fmt.Errorf("sweep %s (%s): %w", j.SweepID, s.status, err))
			}
		}
	}
	return report
}

// IsHold reports errors that mean "try again later by design".
func IsHold(err error) bool {
	return errors.Is(err, ErrPaused) ||
		errors.Is(err, ErrDeferred) ||
		errors.Is(err, ErrFundingPending) ||
		errors.Is(err, ErrSimulationUnavailable) ||
		errors.Is(err, ErrSignerUnavailable) ||
		errors.Is(err, ErrQuarantined) ||
		errors.Is(err, storage.ErrStatusConflict)
}
