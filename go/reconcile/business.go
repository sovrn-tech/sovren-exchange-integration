package reconcile

// Hot-wallet comparison and the business-layer reconciliation section
// (T063, data model §8): the ledger is chain truth; this file reconciles the
// *workflow* layers (deposit crediting, withdrawal and sweep state machines)
// against it, and explains hot-wallet balance drift with in-flight work.

import (
	"context"
	"fmt"
	"time"

	sdkmath "cosmossdk.io/math"

	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

// HotWalletReport is the hot-wallet comparison: on-chain balance vs the
// ledger-based expectation, with the in-flight (pending-signed /
// broadcast-unconfirmed) work that legitimately explains transient drift.
type HotWalletReport struct {
	ChainID string
	Address string

	// LedgerExpected is the §8 ledger formula result.
	LedgerExpected sdkmath.Int
	// Observed is the live on-chain balance.
	Observed sdkmath.Int
	// Difference is Observed − LedgerExpected.
	Difference sdkmath.Int

	// PendingSignedOutflow: withdrawals SIGNED but not yet broadcast from
	// this wallet, plus signed-not-broadcast sweeps sourced here
	// (amount + known fee). Not yet on chain — never explains drift, listed
	// for the operator's forward view.
	PendingSignedOutflow sdkmath.Int
	// BroadcastUnconfirmedOutflow: withdrawals BROADCAST/INCLUDED and sweeps
	// BROADCAST sourced from this wallet (amount + known fee). These may
	// already be on chain ahead of the scanner — negative drift up to this
	// value is explained.
	BroadcastUnconfirmedOutflow sdkmath.Int
	// BroadcastUnconfirmedInflow: sweeps BROADCAST targeting this wallet.
	// Positive drift up to this value is explained.
	BroadcastUnconfirmedInflow sdkmath.Int

	// ConfirmedWithdrawalOutflow / ConfirmedSweepInflow are the ledger's
	// settled WITHDRAWAL (OUT) and SWEEP (IN) totals for this wallet.
	ConfirmedWithdrawalOutflow sdkmath.Int
	ConfirmedSweepInflow       sdkmath.Int

	// Explained is true when Difference is zero or within the in-flight
	// window: [−BroadcastUnconfirmedOutflow, +BroadcastUnconfirmedInflow].
	Explained   bool
	GeneratedAt time.Time
}

// HotWallet builds the comparison for one wallet address.
func (r *Reconciler) HotWallet(ctx context.Context, address string) (HotWalletReport, error) {
	rep := HotWalletReport{
		ChainID: r.cfg.ChainID, Address: address,
		PendingSignedOutflow:        sdkmath.ZeroInt(),
		BroadcastUnconfirmedOutflow: sdkmath.ZeroInt(),
		BroadcastUnconfirmedInflow:  sdkmath.ZeroInt(),
		ConfirmedWithdrawalOutflow:  sdkmath.ZeroInt(),
		ConfirmedSweepInflow:        sdkmath.ZeroInt(),
		GeneratedAt:                 r.now().UTC(),
	}
	if r.chain == nil {
		return rep, fmt.Errorf("reconcile: chain client required for hot-wallet comparison")
	}
	pos, err := r.ExpectedBalance(ctx, address)
	if err != nil {
		return rep, err
	}
	rep.LedgerExpected = pos.Expected
	observed, err := r.chain.Balance(ctx, address, storage.BaseDenom)
	if err != nil {
		return rep, fmt.Errorf("reconcile: balance %s: %w", address, err)
	}
	rep.Observed = observed
	rep.Difference = observed.Sub(pos.Expected)

	// Settled ledger view for the wallet.
	if err := r.walkLedger(ctx, address, func(e storage.LedgerEntry) {
		if e.TxCode != 0 {
			return
		}
		switch {
		case e.Classification == storage.ClassWithdrawal && e.Direction == storage.DirectionOut:
			rep.ConfirmedWithdrawalOutflow = rep.ConfirmedWithdrawalOutflow.Add(e.AmountBaseUnits)
		case e.Classification == storage.ClassSweep && e.Direction == storage.DirectionIn:
			rep.ConfirmedSweepInflow = rep.ConfirmedSweepInflow.Add(e.AmountBaseUnits)
		}
	}); err != nil {
		return rep, err
	}

	// In-flight withdrawal work sourced from this wallet.
	for _, st := range []storage.WithdrawalStatus{storage.WithdrawalSigned, storage.WithdrawalBroadcast, storage.WithdrawalIncluded} {
		recs, err := r.store.Withdrawals().ListByStatus(ctx, r.cfg.ChainID, st, workflowListLimit)
		if err != nil {
			return rep, fmt.Errorf("reconcile: withdrawals %s: %w", st, err)
		}
		for _, w := range recs {
			if w.SourceAddress != address {
				continue
			}
			total := w.AmountBaseUnits
			if w.FeeAmountBaseUnits != nil {
				total = total.Add(*w.FeeAmountBaseUnits)
			}
			if st == storage.WithdrawalSigned {
				rep.PendingSignedOutflow = rep.PendingSignedOutflow.Add(total)
			} else {
				rep.BroadcastUnconfirmedOutflow = rep.BroadcastUnconfirmedOutflow.Add(total)
			}
		}
	}

	// In-flight sweeps: outflow when sourced here, inflow when targeting here.
	for _, st := range []storage.SweepStatus{storage.SweepSigned, storage.SweepBroadcast} {
		jobs, err := r.store.Sweeps().ListByStatus(ctx, r.cfg.ChainID, st, workflowListLimit)
		if err != nil {
			return rep, fmt.Errorf("reconcile: sweeps %s: %w", st, err)
		}
		for _, j := range jobs {
			switch {
			case j.SourceAddress == address && st == storage.SweepSigned:
				rep.PendingSignedOutflow = rep.PendingSignedOutflow.Add(j.AmountBaseUnits)
			case j.SourceAddress == address:
				rep.BroadcastUnconfirmedOutflow = rep.BroadcastUnconfirmedOutflow.Add(j.AmountBaseUnits)
			case j.HotWalletAddress == address && st == storage.SweepBroadcast:
				rep.BroadcastUnconfirmedInflow = rep.BroadcastUnconfirmedInflow.Add(j.AmountBaseUnits)
			}
		}
	}

	switch {
	case rep.Difference.IsZero():
		rep.Explained = true
	case rep.Difference.IsNegative():
		rep.Explained = rep.Difference.Neg().LTE(rep.BroadcastUnconfirmedOutflow)
	default:
		rep.Explained = rep.Difference.LTE(rep.BroadcastUnconfirmedInflow)
	}
	return rep, nil
}

// BusinessSection reconciles the customer-credit and sweep/withdrawal
// workflow layers against the ledger's EXTERNAL_DEPOSIT / SWEEP / WITHDRAWAL
// entries (data model §8 — "a separate business-layer section"). It is
// returned and logged alongside address reports, not persisted in the
// entry-based report record.
type BusinessSection struct {
	ChainID     string
	GeneratedAt time.Time

	// LedgerExternalDepositTotal: Σ successful EXTERNAL_DEPOSIT IN rows
	// across watched addresses — every real external inflow, credited or not.
	LedgerExternalDepositTotal sdkmath.Int
	// CreditedDepositTotal: Σ deposit records that reached CREDITED
	// (including SWEEP_PENDING / SWEPT afterwards).
	CreditedDepositTotal sdkmath.Int
	// UncreditedExternalTotal = ledger − credited. Below-minimum, review-
	// parked, awaiting-confirmation and suspended inflows live here; it is
	// never negative in a healthy system.
	UncreditedExternalTotal sdkmath.Int

	// LedgerWithdrawalOutflowTotal: Σ successful WITHDRAWAL OUT rows.
	LedgerWithdrawalOutflowTotal sdkmath.Int
	// WorkflowWithdrawalTotal: Σ CONFIRMED withdrawal record amounts.
	WorkflowWithdrawalTotal sdkmath.Int
	// InFlightWithdrawalTotal: Σ BROADCAST/INCLUDED amounts (may already be
	// in the ledger ahead of workflow confirmation, or vice versa).
	InFlightWithdrawalTotal sdkmath.Int

	// LedgerSweepInflowTotal: Σ successful SWEEP IN rows (counted on the
	// receiving side only, so one sweep is one amount).
	LedgerSweepInflowTotal sdkmath.Int
	// WorkflowSweepTotal: Σ CONFIRMED sweep job amounts.
	WorkflowSweepTotal sdkmath.Int

	// Findings are human-readable inconsistencies. An impossible state
	// (workflow total exceeding what the ledger can support) also increments
	// the discrepancy counter.
	Findings []string
}

// Business computes the business-layer section.
func (r *Reconciler) Business(ctx context.Context) (BusinessSection, error) {
	sec := BusinessSection{
		ChainID:     r.cfg.ChainID,
		GeneratedAt: r.now().UTC(),

		LedgerExternalDepositTotal:   sdkmath.ZeroInt(),
		CreditedDepositTotal:         sdkmath.ZeroInt(),
		UncreditedExternalTotal:      sdkmath.ZeroInt(),
		LedgerWithdrawalOutflowTotal: sdkmath.ZeroInt(),
		WorkflowWithdrawalTotal:      sdkmath.ZeroInt(),
		InFlightWithdrawalTotal:      sdkmath.ZeroInt(),
		LedgerSweepInflowTotal:       sdkmath.ZeroInt(),
		WorkflowSweepTotal:           sdkmath.ZeroInt(),
	}

	watched, err := r.store.Watch().ListActive(ctx, r.cfg.ChainID)
	if err != nil {
		return sec, fmt.Errorf("reconcile: watch set: %w", err)
	}
	for _, w := range watched {
		if err := r.walkLedger(ctx, w.Address, func(e storage.LedgerEntry) {
			if e.TxCode != 0 {
				return
			}
			switch {
			case e.Classification == storage.ClassExternalDeposit && e.Direction == storage.DirectionIn:
				sec.LedgerExternalDepositTotal = sec.LedgerExternalDepositTotal.Add(e.AmountBaseUnits)
			case e.Classification == storage.ClassWithdrawal && e.Direction == storage.DirectionOut:
				sec.LedgerWithdrawalOutflowTotal = sec.LedgerWithdrawalOutflowTotal.Add(e.AmountBaseUnits)
			case e.Classification == storage.ClassSweep && e.Direction == storage.DirectionIn:
				sec.LedgerSweepInflowTotal = sec.LedgerSweepInflowTotal.Add(e.AmountBaseUnits)
			}
		}); err != nil {
			return sec, err
		}
	}

	for _, st := range []storage.DepositStatus{storage.DepositCredited, storage.DepositSweepPending, storage.DepositSwept} {
		ds, err := r.store.Deposits().ListByStatus(ctx, r.cfg.ChainID, st, workflowListLimit)
		if err != nil {
			return sec, fmt.Errorf("reconcile: deposits %s: %w", st, err)
		}
		for _, d := range ds {
			sec.CreditedDepositTotal = sec.CreditedDepositTotal.Add(d.AmountBaseUnits)
		}
	}
	sec.UncreditedExternalTotal = sec.LedgerExternalDepositTotal.Sub(sec.CreditedDepositTotal)

	for _, st := range []storage.WithdrawalStatus{storage.WithdrawalConfirmed, storage.WithdrawalBroadcast, storage.WithdrawalIncluded} {
		ws, err := r.store.Withdrawals().ListByStatus(ctx, r.cfg.ChainID, st, workflowListLimit)
		if err != nil {
			return sec, fmt.Errorf("reconcile: withdrawals %s: %w", st, err)
		}
		for _, w := range ws {
			if st == storage.WithdrawalConfirmed {
				sec.WorkflowWithdrawalTotal = sec.WorkflowWithdrawalTotal.Add(w.AmountBaseUnits)
			} else {
				sec.InFlightWithdrawalTotal = sec.InFlightWithdrawalTotal.Add(w.AmountBaseUnits)
			}
		}
	}

	sweeps, err := r.store.Sweeps().ListByStatus(ctx, r.cfg.ChainID, storage.SweepConfirmed, workflowListLimit)
	if err != nil {
		return sec, fmt.Errorf("reconcile: sweeps: %w", err)
	}
	for _, j := range sweeps {
		sec.WorkflowSweepTotal = sec.WorkflowSweepTotal.Add(j.AmountBaseUnits)
	}

	// Consistency findings. Impossible states — the credit/confirm workflow
	// claiming more than the ledger ever saw — are discrepancies; workflow
	// lag (ledger ahead of confirmation) is a finding only.
	impossible := func(msg string) {
		sec.Findings = append(sec.Findings, msg)
		if r.metrics != nil {
			r.metrics.ReconciliationDiscrepancy.WithLabelValues(r.cfg.ChainID).Inc()
		}
		r.log.Error("business-layer reconciliation inconsistency",
			"error_code", "RECONCILIATION_DISCREPANCY", "finding", msg)
	}
	if sec.UncreditedExternalTotal.IsNegative() {
		impossible(fmt.Sprintf("credited deposit total %s exceeds ledger EXTERNAL_DEPOSIT total %s",
			sec.CreditedDepositTotal, sec.LedgerExternalDepositTotal))
	}
	if sec.WorkflowWithdrawalTotal.GT(sec.LedgerWithdrawalOutflowTotal) {
		impossible(fmt.Sprintf("confirmed withdrawal total %s exceeds ledger WITHDRAWAL outflow total %s",
			sec.WorkflowWithdrawalTotal, sec.LedgerWithdrawalOutflowTotal))
	}
	if sec.WorkflowSweepTotal.GT(sec.LedgerSweepInflowTotal) {
		impossible(fmt.Sprintf("confirmed sweep total %s exceeds ledger SWEEP inflow total %s",
			sec.WorkflowSweepTotal, sec.LedgerSweepInflowTotal))
	}
	if lag := sec.LedgerWithdrawalOutflowTotal.Sub(sec.WorkflowWithdrawalTotal); lag.IsPositive() {
		sec.Findings = append(sec.Findings, fmt.Sprintf(
			"ledger WITHDRAWAL outflows lead confirmed workflow totals by %s (in-flight: %s) — expected during confirmation lag",
			lag, sec.InFlightWithdrawalTotal))
	}
	return sec, nil
}

// walkLedger streams every ledger row for one address through fn.
func (r *Reconciler) walkLedger(ctx context.Context, address string, fn func(storage.LedgerEntry)) error {
	var afterID int64
	for {
		page, err := r.store.Ledger().List(ctx, storage.LedgerQuery{
			ChainID: r.cfg.ChainID, Address: address, AfterID: afterID, Limit: ledgerPageSize,
		})
		if err != nil {
			return fmt.Errorf("reconcile: ledger list %s: %w", address, err)
		}
		for _, e := range page {
			afterID = e.ID
			fn(e)
		}
		if len(page) < ledgerPageSize {
			return nil
		}
	}
}
