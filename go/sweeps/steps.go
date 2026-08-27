package sweeps

import (
	"context"
	"errors"
	"fmt"
	"time"

	sdkmath "cosmossdk.io/math"

	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/logging"
	"github.com/sovrn-tech/sovren-exchange-integration/go/sequences"
	"github.com/sovrn-tech/sovren-exchange-integration/go/signer"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
	"github.com/sovrn-tech/sovren-exchange-integration/go/tx"
	"github.com/sovrn-tech/sovren-exchange-integration/go/withdrawals"
)

// Prepare drives a PENDING (or crash-resumed BUILT) sweep to SIGNED:
// sequence reservation via work_ref{SWEEP}, final fee from simulation,
// strategy checks (defer on insufficiency — never a loop), the FEE_FUND
// funding gate, signing through the external signer boundary with
// adapter-side verification, and the atomic SIGNED persist (signed bytes +
// tx hash + reservation SIGNED in one transaction).
func (e *Engine) Prepare(ctx context.Context, sweepID string) error {
	j, err := e.store.Sweeps().Get(ctx, sweepID)
	if err != nil {
		return err
	}
	if j.Status != storage.SweepPending && j.Status != storage.SweepBuilt {
		return fmt.Errorf("%w: sweep %s is %s", storage.ErrStatusConflict, sweepID, j.Status)
	}
	controls, err := e.controls(ctx)
	if err != nil {
		return err
	}
	if controls.SweepPaused {
		return fmt.Errorf("%w: sweep", ErrPaused)
	}
	if e.signer == nil {
		return ErrNoSigner
	}

	// Reserve (idempotent per work_ref). A RELEASED slot that was consumed
	// on chain can never land this sweep deterministically: cancel the job;
	// its deposits re-attach to the next plan.
	res, err := e.seq.Reserve(ctx, j.ChainID, j.SourceAddress, storage.WorkRef{Kind: storage.WorkSweep, ID: j.SweepID})
	if err != nil {
		if errors.Is(err, sequences.ErrReleasedSlotUnusable) {
			return e.cancelJob(ctx, j, "sequence slot no longer reservable: "+err.Error())
		}
		return err
	}
	if res.Status != storage.SequenceReserved {
		// SIGNED/BROADCAST would mean a previous SIGNED persist half-applied,
		// which the atomic persist makes impossible; RECONCILIATION_REQUIRED
		// means an ambiguous earlier outcome. Hold for operator/reconciler.
		return fmt.Errorf("%w: reservation for sweep %s is %s", ErrQuarantined, sweepID, res.Status)
	}

	balance, err := e.chain.Balance(ctx, j.SourceAddress, storage.BaseDenom)
	if err != nil {
		return fmt.Errorf("sweeps: balance %s: %w", j.SourceAddress, err)
	}
	fee, gasLimit, err := e.estimateFee(ctx, j.SourceAddress, e.destination(j), j.AmountBaseUnits, res.AccountNumber, res.Sequence)
	if err != nil {
		if errors.Is(err, ErrSimulationUnavailable) {
			e.logger.Warn("sweep held: simulation unavailable",
				logging.FieldChainID, j.ChainID, logging.FieldSweepID, j.SweepID)
		}
		return err
	}

	// Economics first (they never depend on funding), so an uneconomical
	// FEE_FUND sweep defers BEFORE any fee-wallet money moves.
	if reason := e.economicsFailure(j, fee); reason != "" {
		return e.deferOrCancel(ctx, j, reason)
	}
	if hold := e.checkFunding(ctx, j, balance, fee); hold != nil {
		return hold
	}
	if reason := e.coverageFailure(j, balance, fee); reason != "" {
		return e.deferOrCancel(ctx, j, reason)
	}

	if j.Status == storage.SweepPending {
		if err := e.store.Sweeps().UpdateStatus(ctx, j.SweepID, storage.SweepPending, storage.SweepBuilt, storage.SweepUpdate{}); err != nil {
			return err
		}
		j.Status = storage.SweepBuilt
	}
	return e.sign(ctx, j, res, controls, fee, gasLimit)
}

// checkFunding applies the FEE_FUND funding gate for customer sweeps: when
// the source cannot cover amount + fee, exactly one fee-wallet funding
// transfer per sweep is emitted through this same engine and the sweep
// holds until it confirms. A confirmed funding that still leaves the
// balance short defers the sweep — never a second funding.
func (e *Engine) checkFunding(ctx context.Context, j storage.SweepJob, balance, fee sdkmath.Int) error {
	if j.Strategy != storage.StrategyFeeFund || IsFundingJob(j) {
		return nil
	}
	if balance.GTE(j.AmountBaseUnits.Add(fee)) {
		return nil // funded (or fee-free) — proceed
	}

	fundingID := fundingSweepID(j.SweepID)
	fj, err := e.store.Sweeps().Get(ctx, fundingID)
	switch {
	case errors.Is(err, storage.ErrNotFound):
		// Fee-wallet spend guardrail: never start a funding leg that would push
		// the fee wallet's windowed FEE_FUND spend over the cap — defer the
		// sweep (never loop) so a dust flood / derivation bug can't drain it.
		over, spent, err := e.feeFundingCapExceeded(ctx, fee)
		if err != nil {
			return err
		}
		if over {
			e.logger.Warn("fee-wallet spend cap reached; deferring sweep",
				logging.FieldChainID, j.ChainID, logging.FieldSweepID, j.SweepID,
				"window_spend_usovr", spent.String(), "this_fee_usovr", fee.String(),
				"cap_usovr", e.pcfg.feeWalletMaxSpend.String(),
				"window_blocks", e.cfg.FeeWalletSpendWindowBlocks)
			return e.deferOrCancel(ctx, j, "fee-wallet spend cap reached (windowed FEE_FUND spend + this fee exceeds fee_wallet_max_spend_usovr)")
		}
		return e.createFundingJob(ctx, j, fundingID, fee)
	case err != nil:
		return err
	}
	switch fj.Status {
	case storage.SweepConfirmed:
		// Funded once and still short (fee drift or an external spend):
		// surfaced deferral, never a funding retry loop.
		return e.deferOrCancel(ctx, j, "funding confirmed but balance still below amount + fee")
	case storage.SweepFailed, storage.SweepCancelled:
		return e.deferOrCancel(ctx, j, fmt.Sprintf("funding transfer ended %s; operator review", fj.Status))
	default:
		return fmt.Errorf("%w: funding %s is %s", ErrFundingPending, fundingID, fj.Status)
	}
}

// deferOrCancel defers a PENDING job; a crash-resumed BUILT job (which has
// no legal path back to DEFERRED) is cancelled so its deposits re-plan.
func (e *Engine) deferOrCancel(ctx context.Context, j storage.SweepJob, reason string) error {
	if j.Status == storage.SweepBuilt {
		return e.cancelJob(ctx, j, reason)
	}
	return e.deferJob(ctx, j, reason)
}

// feeFundingCapExceeded reports whether funding this sweep's fee would push the
// fee wallet's FEE_FUND spend within the configured rolling block window over
// fee_wallet_max_spend_usovr. The sum has two parts, and their read order is
// load-bearing:
//
//  1. The single in-flight (non-terminal) funding leg, if any — read FIRST via
//     GetActive. At most one exists (the fee wallet's one-non-terminal-sweep
//     slot), and it WILL record its spend when it confirms.
//  2. Confirmed spend from fee_funding_spends — records the sweeper writes
//     ATOMICALLY with each funding leg's confirmation (broadcast.Confirm), NOT
//     the deposit scanner's asynchronous FEE_FUNDING ledger rows (which lag the
//     tip and would let a just-confirmed spend go uncounted).
//
// Because Confirm flips the leg terminal AND inserts its spend row in one
// transaction, reading the in-flight leg BEFORE the recorded sum guarantees a
// leg that confirms between the two reads is counted at least once (as in-flight
// then, or as recorded now — never neither). Worst case it is briefly counted
// twice, which only defers a sweep early: the error is always toward the safe
// side, so the check can never under-count and let a leg overshoot the cap —
// closing the check-vs-create TOCTOU under concurrent drivers without a lock.
// A disabled cap (zero) always returns false.
func (e *Engine) feeFundingCapExceeded(ctx context.Context, fee sdkmath.Int) (bool, sdkmath.Int, error) {
	maxSpend := e.pcfg.feeWalletMaxSpend
	if !maxSpend.IsPositive() {
		return false, sdkmath.ZeroInt(), nil // cap disabled
	}
	feeWallet, err := e.feeWallet(ctx)
	if err != nil {
		return false, sdkmath.ZeroInt(), err
	}
	status, err := e.chain.NodeStatus(ctx)
	if err != nil {
		return false, sdkmath.ZeroInt(), err
	}
	height := uint64(status.LatestHeight)
	var from uint64
	if height >= e.cfg.FeeWalletSpendWindowBlocks {
		from = height - e.cfg.FeeWalletSpendWindowBlocks + 1
	}
	spent := sdkmath.ZeroInt()
	// (1) In-flight leg first — see the ordering argument above.
	if inflight, err := e.store.Sweeps().GetActive(ctx, e.cfg.ChainID, feeWallet); err == nil {
		if IsFundingJob(inflight) {
			spent = spent.Add(inflight.AmountBaseUnits)
		}
	} else if !errors.Is(err, storage.ErrNotFound) {
		return false, sdkmath.ZeroInt(), err
	}
	// (2) Confirmed spend recorded within the window.
	recorded, err := e.store.Ledger().SumFeeFundingSpend(ctx, e.cfg.ChainID, feeWallet, from, height)
	if err != nil {
		return false, sdkmath.ZeroInt(), err
	}
	spent = spent.Add(recorded)
	return spent.Add(fee).GT(maxSpend), spent, nil
}

// createFundingJob emits the FEE_FUND funding MsgSend (fee wallet → sweep
// source, amount = the sweep's final fee) as its own SweepJob, giving the
// funding leg the same durability: reservation via work_ref{SWEEP},
// persisted signed bytes, search-first broadcast. The scanner classifies
// the resulting on-chain transfer FEE_FUNDING (watched fee-wallet input) —
// internal by construction, never a customer credit (FR-023).
func (e *Engine) createFundingJob(ctx context.Context, parent storage.SweepJob, fundingID string, fee sdkmath.Int) error {
	feeWallet, err := e.feeWallet(ctx)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	_, err = e.store.Sweeps().Create(ctx, storage.SweepJob{
		SweepID:             fundingID,
		IdempotencyKey:      FundingIdempotencyKey(parent.ChainID, feeWallet, parent.SweepID),
		ChainID:             parent.ChainID,
		SourceAddress:       feeWallet,
		HotWalletAddress:    parent.SourceAddress, // funding destination
		Strategy:            storage.StrategyFeeFund,
		AmountBaseUnits:     fee,
		FeeReserveBaseUnits: sdkmath.ZeroInt(),
		DepositIDs:          nil, // a funding leg covers no deposits
		CreatedAt:           now,
		UpdatedAt:           now,
	})
	switch {
	case err == nil:
		e.logger.Info("fee funding transfer planned",
			logging.FieldChainID, parent.ChainID,
			logging.FieldSweepID, fundingID,
			logging.FieldAddress, parent.SourceAddress,
			"fee_wallet", feeWallet,
			"amount_base_units", fee.String(),
		)
	case errors.Is(err, storage.ErrActiveSweepExists):
		// Another sweep's funding leg holds the fee wallet's live slot;
		// this sweep waits its turn.
	case errors.Is(err, storage.ErrDuplicate):
		// The funding job exists under this key already (raced ourselves).
	default:
		return err
	}
	return fmt.Errorf("%w: funding for sweep %s", ErrFundingPending, parent.SweepID)
}

// feeWallet returns the active FEE_WALLET watched address.
func (e *Engine) feeWallet(ctx context.Context) (string, error) {
	watched, err := e.store.Watch().ListActive(ctx, e.cfg.ChainID)
	if err != nil {
		return "", err
	}
	for _, w := range watched {
		if w.Kind == storage.WatchFeeWallet {
			return w.Address, nil
		}
	}
	return "", ErrNoFeeWallet
}

// economicsFailure applies the funding-independent rules (minimum amount,
// fee percentage) against the FINAL fee. Empty means economical. Funding
// legs move a fee-sized amount, so both rules would reject them by
// construction and do not apply.
func (e *Engine) economicsFailure(j storage.SweepJob, fee sdkmath.Int) string {
	if IsFundingJob(j) {
		return ""
	}
	if j.AmountBaseUnits.LT(e.pcfg.minSweep) {
		return "amount below minimum_sweep_amount_usovr"
	}
	if !feeWithinPercentage(fee, j.AmountBaseUnits, e.pcfg.maxFeePct) {
		return "fee exceeds maximum_fee_percentage_for_sweep"
	}
	return ""
}

// coverageFailure checks the live balance against amount + fee + reserve.
// Empty means covered.
func (e *Engine) coverageFailure(j storage.SweepJob, balance, fee sdkmath.Int) string {
	required := j.AmountBaseUnits.Add(fee).Add(e.reserveFor(j))
	if balance.LT(required) {
		return fmt.Sprintf("balance %s below amount %s + fee %s + reserve %s",
			balance, j.AmountBaseUnits, fee, e.reserveFor(j))
	}
	return ""
}

// sign runs the signer boundary for a BUILT sweep and atomically persists
// SIGNED (bytes + hash) with the reservation flip. Mirrors the withdrawal
// signing trust boundary: the summary is derived from the exact sign-doc
// bytes and cross-checked; the signer response is verified before anything
// is persisted; an ambiguous outcome quarantines the reservation.
func (e *Engine) sign(ctx context.Context, j storage.SweepJob, res storage.SequenceReservation, controls storage.OperationalControls, fee sdkmath.Int, gasLimit uint64) error {
	if controls.SigningPaused {
		return fmt.Errorf("%w: signing", ErrPaused)
	}
	unsigned, err := tx.BuildMsgSend(j.SourceAddress, e.destination(j), j.AmountBaseUnits.String(), "")
	if err != nil {
		return e.cancelJob(ctx, j, "build rejected: "+err.Error())
	}
	// The source public key is required BEFORE sign-doc production: it is
	// embedded in AuthInfo.SignerInfos[0].PublicKey inside the signed bytes.
	pubKey, err := e.senderPubKey(ctx, j.SourceAddress)
	if err != nil {
		if errors.Is(err, ErrSignerUnavailable) {
			e.logger.Warn("signer unavailable; sweep queued",
				logging.FieldChainID, j.ChainID, logging.FieldSweepID, j.SweepID)
			return err
		}
		_ = e.store.Sequences().UpdateStatus(ctx, res.ID, storage.SequenceReserved, storage.SequenceReconciliationRequired)
		return fmt.Errorf("%w: source public key fetch failed: %v", ErrQuarantined, err)
	}
	signDocBytes, summary, err := unsigned.SignDoc(j.ChainID, res.AccountNumber, res.Sequence, tx.Fee{
		AmountBaseUnits: fee.String(),
		GasLimit:        gasLimit,
	}, pubKey)
	if err != nil {
		return e.cancelJob(ctx, j, "sign doc encoding failed: "+err.Error())
	}
	if mismatch := e.summaryMismatch(j, res, fee, summary); mismatch != "" {
		return e.cancelJob(ctx, j, "sign doc does not match sweep job: "+mismatch)
	}

	sigRes, err := e.signer.Sign(ctx, signer.SigningRequest{
		KeyRef:       e.keyRef(j.SourceAddress),
		SignMode:     signer.SignModeDirect,
		SignDocBytes: signDocBytes,
		Summary:      summary,
	})
	if err != nil {
		if errors.Is(err, signer.ErrSignerUnavailable) {
			e.logger.Warn("signer unavailable; sweep queued",
				logging.FieldChainID, j.ChainID, logging.FieldSweepID, j.SweepID)
			return fmt.Errorf("%w: %v", ErrSignerUnavailable, err)
		}
		// Refusal is definitive (nothing was signed): quarantine the
		// reservation for reconciliation and surface the reason.
		_ = e.store.Sequences().UpdateStatus(ctx, res.ID, storage.SequenceReserved, storage.SequenceReconciliationRequired)
		return fmt.Errorf("%w: signer refused (%s)", ErrQuarantined, signer.CodeOf(err))
	}
	// Adapter-side verification at the trust boundary; failure is an
	// ambiguous signer outcome — quarantine, never broadcast.
	if err := withdrawals.VerifySignedResponse(signDocBytes, sigRes, j.SourceAddress); err != nil {
		_ = e.store.Sequences().UpdateStatus(ctx, res.ID, storage.SequenceReserved, storage.SequenceReconciliationRequired)
		return fmt.Errorf("%w: signed response verification failed: %v", ErrQuarantined, err)
	}
	signedTxBytes, txHash, err := tx.Assemble(unsigned, tx.SignatureResponse{
		Signature:        sigRes.Signature,
		PubKeyCompressed: sigRes.PubKeyCompressed,
	})
	if err != nil {
		_ = e.store.Sequences().UpdateStatus(ctx, res.ID, storage.SequenceReserved, storage.SequenceReconciliationRequired)
		return fmt.Errorf("%w: assembly failed: %v", ErrQuarantined, err)
	}

	if err := e.store.WithTx(ctx, func(ctx context.Context, st storage.Store) error {
		if err := st.Sweeps().UpdateStatus(ctx, j.SweepID, storage.SweepBuilt, storage.SweepSigned, storage.SweepUpdate{
			SignedTxBytes: signedTxBytes,
			TxHash:        &txHash,
		}); err != nil {
			return err
		}
		return st.Sequences().UpdateStatus(ctx, res.ID, storage.SequenceReserved, storage.SequenceSigned)
	}); err != nil {
		return err
	}
	e.logger.Info("sweep signed",
		logging.FieldChainID, j.ChainID,
		logging.FieldSweepID, j.SweepID,
		logging.FieldTxHash, txHash,
		logging.FieldSequence, res.Sequence,
	)
	return nil
}

// summaryMismatch compares the summary derived from the sign-doc bytes to
// the sweep job field-for-field.
func (e *Engine) summaryMismatch(j storage.SweepJob, res storage.SequenceReservation, fee sdkmath.Int, s signer.SigningSummary) string {
	checks := []struct{ field, doc, approved string }{
		{"chain_id", s.ChainID, j.ChainID},
		{"sender", s.SenderAddress, j.SourceAddress},
		{"recipient", s.RecipientAddress, e.destination(j)},
		{"amount", s.AmountBaseUnits, j.AmountBaseUnits.String()},
		{"denom", s.Denom, storage.BaseDenom},
		{"memo", s.Memo, ""},
		{"fee", s.FeeBaseUnits, fee.String()},
		{"sequence", s.Sequence, fmt.Sprintf("%d", res.Sequence)},
		{"account_number", s.AccountNumber, fmt.Sprintf("%d", res.AccountNumber)},
		{"message_type", s.MessageType, signer.MsgTypeBankSend},
	}
	for _, c := range checks {
		if c.doc != c.approved {
			return c.field
		}
	}
	return ""
}

// cancelJob terminates a pre-SIGNED job (PENDING/BUILT → CANCELLED) and
// reports the reason as an error for step callers.
func (e *Engine) cancelJob(ctx context.Context, j storage.SweepJob, reason string) error {
	if err := e.terminateJob(ctx, j, reason); err != nil {
		return err
	}
	return fmt.Errorf("sweeps: cancelled: %s", reason)
}

// terminateJob flips a pre-SIGNED job (PENDING/BUILT/DEFERRED → CANCELLED),
// releasing a still-unsigned reservation. Covered deposits keep their
// SWEEP_PENDING status and re-attach to the next planned sweep. Returns nil
// on success.
func (e *Engine) terminateJob(ctx context.Context, j storage.SweepJob, reason string) error {
	err := e.store.WithTx(ctx, func(ctx context.Context, st storage.Store) error {
		if err := st.Sweeps().UpdateStatus(ctx, j.SweepID, j.Status, storage.SweepCancelled, storage.SweepUpdate{}); err != nil {
			return err
		}
		return e.releaseUnsignedReservation(ctx, st, j.SweepID)
	})
	if err != nil {
		return err
	}
	e.logger.Warn("sweep cancelled",
		logging.FieldChainID, j.ChainID,
		logging.FieldSweepID, j.SweepID,
		"reason", reason,
	)
	return nil
}

// Revisit re-evaluates a DEFERRED sweep against current conditions and
// returns it to PENDING only when the strategy maths now pass. It performs
// no signing and no broadcast — deferral is never a retry loop.
func (e *Engine) Revisit(ctx context.Context, sweepID string) (revived bool, err error) {
	j, err := e.store.Sweeps().Get(ctx, sweepID)
	if err != nil {
		return false, err
	}
	if j.Status != storage.SweepDeferred {
		return false, fmt.Errorf("%w: sweep %s is %s", storage.ErrStatusConflict, sweepID, j.Status)
	}
	controls, err := e.controls(ctx)
	if err != nil {
		return false, err
	}
	if controls.SweepPaused {
		return false, fmt.Errorf("%w: sweep", ErrPaused)
	}

	balance, err := e.chain.Balance(ctx, j.SourceAddress, storage.BaseDenom)
	if err != nil {
		return false, err
	}
	accountNumber, sequence, err := e.chain.Account(ctx, j.SourceAddress)
	if err != nil {
		return false, err
	}
	fee, _, err := e.estimateFee(ctx, j.SourceAddress, e.destination(j), j.AmountBaseUnits, accountNumber, sequence)
	if err != nil {
		return false, err
	}
	// Revival requires the full maths to pass — including, for a FEE_FUND
	// sweep whose one funding attempt is spent, complete fee coverage from
	// the live balance.
	if e.economicsFailure(j, fee) == "" && e.coverageFailure(j, balance, fee) == "" {
		if err := e.store.Sweeps().UpdateStatus(ctx, j.SweepID, storage.SweepDeferred, storage.SweepPending, storage.SweepUpdate{}); err != nil {
			return false, err
		}
		e.logger.Info("deferred sweep revived",
			logging.FieldChainID, j.ChainID,
			logging.FieldSweepID, j.SweepID,
		)
		return true, nil
	}
	// Still failing with the STORED amount. For the amount-from-fee
	// strategies the stored amount itself may be the problem (planned
	// against a fee that has since changed): try a fresh plan and, when it
	// passes, cancel this job so the recomputed amount takes over.
	return false, e.replanStaleAmount(ctx, j, balance, accountNumber, sequence)
}

// replanStaleAmount closes the fee-drift trap for the amount-from-fee
// strategies (FEE_RESERVE, THRESHOLD_ONLY): their amount is
// balance − fee [− reserve] at PLAN-time fees, so a fee that changed
// between plan and execution leaves the job unexecutable forever at a
// static balance — the AMOUNT is stale, not the conditions. When a fresh
// plan at current conditions passes in full, the stale job is
// terminal-CANCELLED (freeing the non-terminal-unique slot; covered
// deposits stay SWEEP_PENDING and re-attach), so the next Plan pass
// creates a job with the recomputed amount. The cancel fires ONLY when
// the fresh maths pass — a balance that genuinely funds no sweep stays
// DEFERRED (no retry loop), and FEE_FUND never re-plans from here (a
// fresh job would mean a second funding attempt).
func (e *Engine) replanStaleAmount(ctx context.Context, j storage.SweepJob, balance sdkmath.Int, accountNumber, sequence uint64) error {
	if IsFundingJob(j) ||
		(j.Strategy != storage.StrategyFeeReserve && j.Strategy != storage.StrategyThresholdOnly) {
		return nil // stays deferred silently
	}
	freshAmount, _, freshReason, err := e.planFeeAmount(ctx, j.SourceAddress, e.destination(j), balance, accountNumber, sequence)
	if err != nil {
		return err
	}
	if freshReason != "" || freshAmount.Equal(j.AmountBaseUnits) {
		return nil // a fresh plan would not execute either: stay deferred
	}
	return e.terminateJob(ctx, j, fmt.Sprintf(
		"deferred amount %s is stale at current fees; cancelled for re-plan (fresh amount %s)",
		j.AmountBaseUnits, freshAmount))
}
