package sweeps

import (
	"context"
	"errors"
	"fmt"
	"time"

	sdkmath "cosmossdk.io/math"

	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/logging"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

// PlanReport is the outcome of one Plan pass.
type PlanReport struct {
	// JobsCreated lists sweep IDs created in status PENDING.
	JobsCreated []string
	// JobsDeferred lists sweep IDs created and immediately DEFERRED (the
	// snapshot cannot cover fee + reserve yet).
	JobsDeferred []string
	// CustodySwept counts deposits settled by CUSTODY_ABSTRACTED
	// bookkeeping (no transaction).
	CustodySwept int
	// Held maps source address → reason for sources skipped this pass
	// (active job, below minimum, simulation unavailable, ...).
	Held map[string]string
}

// depositListLimit bounds one planning pass; deeper backlogs drain across
// passes.
const depositListLimit = 500

// Plan discovers sweepable sources and creates at most one SweepJob per
// source, honoring the non-terminal partial-unique constraint: a source
// with ANY live job (including DEFERRED and quarantined SIGNED/BROADCAST)
// is never given a second one, regardless of how many new balance
// snapshots have appeared (data model §7 guarantee 1).
//
// Covered deposits (status CREDITED, plus SWEEP_PENDING orphans left by a
// terminal FAILED/CANCELLED sweep) flip CREDITED → SWEEP_PENDING in the
// same transaction that creates the job.
//
// Under CUSTODY_ABSTRACTED no transaction is emitted: deposits settle by
// bookkeeping (CREDITED → SWEEP_PENDING → SWEPT) and funds stay put under
// the unified custody boundary.
func (e *Engine) Plan(ctx context.Context) (PlanReport, error) {
	report := PlanReport{Held: map[string]string{}}

	controls, err := e.controls(ctx)
	if err != nil {
		return report, err
	}
	if controls.SweepPaused {
		return report, fmt.Errorf("%w: sweep", ErrPaused)
	}

	sources, err := e.sweepableSources(ctx)
	if err != nil {
		return report, err
	}
	credited, sweepPending, err := e.depositBacklog(ctx)
	if err != nil {
		return report, err
	}

	var height uint64
	if e.cfg.Strategy != storage.StrategyCustodyAbstract {
		status, err := e.chain.NodeStatus(ctx)
		if err != nil {
			return report, fmt.Errorf("sweeps: node status: %w", err)
		}
		height = uint64(status.LatestHeight)
	}

	for _, source := range sources {
		if ctx.Err() != nil {
			return report, ctx.Err()
		}
		coveredIDs := append(append([]int64{}, credited[source]...), sweepPending[source]...)

		if e.cfg.Strategy == storage.StrategyCustodyAbstract {
			n, err := e.custodySettle(ctx, source, credited[source], sweepPending[source])
			if err != nil {
				return report, err
			}
			report.CustodySwept += n
			continue
		}

		// Guarantee 1: one live sweep per source, ever.
		if _, err := e.store.Sweeps().GetActive(ctx, e.cfg.ChainID, source); err == nil {
			report.Held[source] = "active sweep exists"
			continue
		} else if !errors.Is(err, storage.ErrNotFound) {
			return report, err
		}

		balance, err := e.chain.Balance(ctx, source, storage.BaseDenom)
		if err != nil {
			return report, fmt.Errorf("sweeps: balance %s: %w", source, err)
		}
		if balance.LT(e.pcfg.minSweep) {
			report.Held[source] = "balance below minimum_sweep_amount_usovr"
			continue
		}

		sweepID, deferred, err := e.createJob(ctx, source, balance, height, coveredIDs)
		switch {
		case errors.Is(err, storage.ErrActiveSweepExists), errors.Is(err, storage.ErrDuplicate):
			// A concurrent planner (or a replayed snapshot) won; the
			// constraint is the guarantee — never a second live sweep.
			report.Held[source] = "duplicate or concurrent sweep creation"
			continue
		case errors.Is(err, ErrSimulationUnavailable):
			e.logger.Warn("sweep planning held: simulation unavailable",
				logging.FieldChainID, e.cfg.ChainID, logging.FieldAddress, source)
			report.Held[source] = "simulation unavailable"
			continue
		case err != nil:
			return report, err
		}
		if deferred {
			report.JobsDeferred = append(report.JobsDeferred, sweepID)
		} else {
			report.JobsCreated = append(report.JobsCreated, sweepID)
		}
	}
	return report, nil
}

// sweepableSources returns active CUSTOMER_DEPOSIT and OMNIBUS watched
// addresses (never the hot, cold, or fee wallets).
func (e *Engine) sweepableSources(ctx context.Context) ([]string, error) {
	watched, err := e.store.Watch().ListActive(ctx, e.cfg.ChainID)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, w := range watched {
		if w.Kind != storage.WatchCustomerDeposit && w.Kind != storage.WatchOmnibus {
			continue
		}
		if w.Address == e.cfg.HotWallet {
			continue
		}
		out = append(out, w.Address)
	}
	return out, nil
}

// depositBacklog groups deposit IDs by recipient: CREDITED deposits await
// their first sweep; SWEEP_PENDING deposits whose sweep ended terminal
// (FAILED/CANCELLED) are re-attachable — sources with a live sweep are
// filtered out by the GetActive gate before these are used.
func (e *Engine) depositBacklog(ctx context.Context) (credited, sweepPending map[string][]int64, err error) {
	credited, err = e.depositIDsByRecipient(ctx, storage.DepositCredited)
	if err != nil {
		return nil, nil, err
	}
	sweepPending, err = e.depositIDsByRecipient(ctx, storage.DepositSweepPending)
	if err != nil {
		return nil, nil, err
	}
	return credited, sweepPending, nil
}

func (e *Engine) depositIDsByRecipient(ctx context.Context, status storage.DepositStatus) (map[string][]int64, error) {
	recs, err := e.store.Deposits().ListByStatus(ctx, e.cfg.ChainID, status, depositListLimit)
	if err != nil {
		return nil, err
	}
	out := map[string][]int64{}
	for _, d := range recs {
		out[d.RecipientAddress] = append(out[d.RecipientAddress], d.ID)
	}
	return out, nil
}

// createJob snapshots one source into a SweepJob (PENDING), flipping the
// covered CREDITED deposits to SWEEP_PENDING atomically. When the snapshot
// already shows the strategy cannot fund the sweep, the job is created and
// immediately DEFERRED (surfaced, revisited later — deferred=true).
func (e *Engine) createJob(ctx context.Context, source string, balance sdkmath.Int, height uint64, coveredIDs []int64) (sweepID string, deferred bool, err error) {
	accountNumber, sequence, err := e.chain.Account(ctx, source)
	if err != nil {
		return "", false, fmt.Errorf("sweeps: account %s: %w", source, err)
	}
	amount, _, deferReason, err := e.planFeeAmount(ctx, source, e.cfg.HotWallet, balance, accountNumber, sequence)
	if err != nil {
		return "", false, err
	}
	if amount.IsNil() || !amount.IsPositive() {
		// Not even a placeholder amount can be stored; nothing to do until
		// the balance grows.
		amount = balance
	}

	idem := IdempotencyKey(e.cfg.ChainID, source, balance, height)
	sweepID = sweepIDFor(idem)
	now := time.Now().UTC()

	err = e.store.WithTx(ctx, func(ctx context.Context, st storage.Store) error {
		if _, err := st.Sweeps().Create(ctx, storage.SweepJob{
			SweepID:             sweepID,
			IdempotencyKey:      idem,
			ChainID:             e.cfg.ChainID,
			SourceAddress:       source,
			HotWalletAddress:    e.cfg.HotWallet,
			Strategy:            e.cfg.Strategy,
			AmountBaseUnits:     amount,
			FeeReserveBaseUnits: e.pcfg.feeReserve,
			DepositIDs:          coveredIDs,
			CreatedAt:           now,
			UpdatedAt:           now,
		}); err != nil {
			return err
		}
		for _, id := range coveredIDs {
			d, err := st.Deposits().GetByID(ctx, id)
			if err != nil {
				return err
			}
			if d.Status != storage.DepositCredited {
				continue // already SWEEP_PENDING from a terminal sweep
			}
			if err := st.Deposits().UpdateStatus(ctx, id, storage.DepositCredited, storage.DepositSweepPending, storage.DepositUpdate{}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return "", false, err
	}

	e.logger.Info("sweep planned",
		logging.FieldChainID, e.cfg.ChainID,
		logging.FieldSweepID, sweepID,
		logging.FieldAddress, source,
		logging.FieldHeight, height,
		"strategy", string(e.cfg.Strategy),
		"amount_base_units", amount.String(),
		"deposits", len(coveredIDs),
	)

	if deferReason != "" {
		if err := e.deferJob(ctx, storage.SweepJob{SweepID: sweepID, SourceAddress: source}, deferReason); err != nil && !errors.Is(err, ErrDeferred) {
			return sweepID, false, err
		}
		return sweepID, true, nil
	}
	return sweepID, false, nil
}

// planFeeMaxRounds bounds the plan-time fee fixed-point search. The fee
// depends on the simulated amount only through coarse effects (emptying the
// sender's balance record, digit widths), so the fixed point is normally
// found on the second round.
const planFeeMaxRounds = 8

// planFeeAmount finds the (amount, fee) pair the EXECUTION path will
// reproduce. Prepare simulates the transaction with the job's ACTUAL
// amount, and MsgSend gas depends on that amount — a full-balance send
// deletes the sender's coin record where a partial send rewrites it, and
// digit widths change the tx size — so a single full-balance probe
// systematically under-estimates the fee for the amount-from-fee
// strategies (FEE_RESERVE, THRESHOLD_ONLY) and plans sweeps that can
// never execute (balance < amount + real fee + reserve, forever, at a
// static balance).
//
// The search seeds with a full-balance simulation, applies the strategy
// rule, then re-simulates with the induced amount until the fee
// reproduces itself. Every simulation goes through the same construction
// as execution (BuildMsgSend + SignDoc with the sender pubkey + Simulate
// + gas adjustment + ceil fee maths), so at the fixed point — for a
// stable gas price — amount + execution fee + reserve == balance exactly.
// Strategies whose amount does not depend on the fee (FEE_FUND: full
// balance) terminate on the seed round, which already IS the execution
// simulation. A search that fails to converge defers rather than plan an
// amount that cannot execute.
func (e *Engine) planFeeAmount(ctx context.Context, source, dest string, balance sdkmath.Int, accountNumber, sequence uint64) (amount, fee sdkmath.Int, deferReason string, err error) {
	fee, _, err = e.estimateFee(ctx, source, dest, balance, accountNumber, sequence)
	if err != nil {
		return sdkmath.Int{}, sdkmath.Int{}, "", err
	}
	amount, deferReason = e.planAmount(balance, fee)
	for range planFeeMaxRounds {
		if deferReason != "" || amount.Equal(balance) {
			return amount, fee, deferReason, nil
		}
		refee, _, ferr := e.estimateFee(ctx, source, dest, amount, accountNumber, sequence)
		if ferr != nil {
			return sdkmath.Int{}, sdkmath.Int{}, "", ferr
		}
		if refee.Equal(fee) {
			// Fixed point: execution's simulation of this exact amount
			// yields this exact fee.
			return amount, fee, "", nil
		}
		fee = refee
		amount, deferReason = e.planAmount(balance, fee)
	}
	if deferReason == "" {
		deferReason = "fee estimate did not converge"
	}
	return amount, fee, deferReason, nil
}

// planAmount applies the strategy's amount rule to a balance snapshot with
// the estimated fee. An empty deferReason means the plan is executable.
func (e *Engine) planAmount(balance, feeEstimate sdkmath.Int) (amount sdkmath.Int, deferReason string) {
	switch e.cfg.Strategy {
	case storage.StrategyFeeFund:
		// Full balance; the fee arrives from the fee wallet (Prepare).
		amount = balance
	case storage.StrategyFeeReserve:
		amount = balance.Sub(feeEstimate).Sub(e.pcfg.feeReserve)
	case storage.StrategyThresholdOnly:
		amount = balance.Sub(feeEstimate)
	default:
		return sdkmath.Int{}, fmt.Sprintf("strategy %s plans no transaction", e.cfg.Strategy)
	}
	if !amount.IsPositive() {
		return balance, "balance cannot cover fee estimate and reserve"
	}
	if amount.LT(e.pcfg.minSweep) {
		return balance, "swept amount would fall below minimum_sweep_amount_usovr"
	}
	// The percentage rule applies to every transacting strategy — for
	// FEE_FUND it fires here, BEFORE any fee-wallet money moves.
	if !feeWithinPercentage(feeEstimate, amount, e.pcfg.maxFeePct) {
		return amount, "fee exceeds maximum_fee_percentage_for_sweep"
	}
	return amount, ""
}

// custodySettle performs the CUSTODY_ABSTRACTED bookkeeping settlement:
// CREDITED → SWEEP_PENDING → SWEPT with no transaction and no tx hash.
func (e *Engine) custodySettle(ctx context.Context, source string, creditedIDs, sweepPendingIDs []int64) (int, error) {
	if len(creditedIDs)+len(sweepPendingIDs) == 0 {
		return 0, nil
	}
	n := 0
	err := e.store.WithTx(ctx, func(ctx context.Context, st storage.Store) error {
		for _, id := range creditedIDs {
			if err := st.Deposits().UpdateStatus(ctx, id, storage.DepositCredited, storage.DepositSweepPending, storage.DepositUpdate{}); err != nil {
				return err
			}
			if err := st.Deposits().UpdateStatus(ctx, id, storage.DepositSweepPending, storage.DepositSwept, storage.DepositUpdate{}); err != nil {
				return err
			}
			n++
		}
		for _, id := range sweepPendingIDs {
			err := st.Deposits().UpdateStatus(ctx, id, storage.DepositSweepPending, storage.DepositSwept, storage.DepositUpdate{})
			if err != nil && !errors.Is(err, storage.ErrStatusConflict) {
				return err
			}
			n++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	e.logger.Info("custody-abstracted settlement",
		logging.FieldChainID, e.cfg.ChainID,
		logging.FieldAddress, source,
		"deposits", n,
	)
	return n, nil
}
