package main

// Group C — reconciliation & operational controls (T075). Storage-level:
// these run against a throwaway SQLite store and the stub chain view, so
// they are always runnable with no live chain.

import (
	"context"
	"fmt"
	"time"

	sdkmath "cosmossdk.io/math"

	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
	"github.com/sovrn-tech/sovren-exchange-integration/go/deposits"
	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/metrics"
	"github.com/sovrn-tech/sovren-exchange-integration/go/reconcile"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

func init() {
	register("C1", scenarioC1CleanRun)
	register("C2", scenarioC2DiscrepancyFields)
	register("C3", scenarioC3FeeOutflow)
	register("C4", scenarioC4PauseIndependence)
	register("C5", scenarioC5ResumeFromHeight)
}

const (
	certLedgerTxHash  = "AAAA1111BBBB2222CCCC3333DDDD4444EEEE5555FFFF6666AAAA7777BBBB8888"
	certFailedTxHash  = "1111AAAA2222BBBB3333CCCC4444DDDD5555EEEE6666FFFF7777AAAA8888BBBB"
)

// seedExternalDeposit appends one successful EXTERNAL_DEPOSIT ledger row.
func seedExternalDeposit(ctx context.Context, st storage.Store, addr, txHash string, height uint64, amount int64) error {
	_, err := st.Ledger().Append(ctx, storage.LedgerEntry{
		ChainID:         certChainID,
		Kind:            storage.LedgerKindTx,
		TxHash:          txHash,
		MessageIndex:    0,
		OpIndex:         0,
		BlockHeight:     height,
		Direction:       storage.DirectionIn,
		Address:         addr,
		AmountBaseUnits: sdkmath.NewInt(amount),
		Denom:           storage.BaseDenom,
		TxCode:          0,
		Classification:  storage.ClassExternalDeposit,
		CreatedAt:       time.Now().UTC(),
	})
	return err
}

// scenarioC1CleanRun: the §8 address formula against a matching on-chain
// balance yields zero discrepancies.
func scenarioC1CleanRun(ctx context.Context, rc *RunContext) Result {
	st, cleanup, err := tempStore("c1")
	if err != nil {
		return fail("temp store: "+err.Error(), nil)
	}
	defer cleanup()

	key, err := certKey(101)
	if err != nil {
		return fail("derive: "+err.Error(), nil)
	}
	addr := key.Bech32
	if err := watchAddr(ctx, st, certChainID, addr, storage.WatchCustomerDeposit); err != nil {
		return fail("watch: "+err.Error(), nil)
	}
	if err := seedExternalDeposit(ctx, st, addr, certLedgerTxHash, 10, 5_000_000); err != nil {
		return fail("ledger seed: "+err.Error(), nil)
	}

	chain := newStubChain(certChainID, 12)
	chain.balances[addr] = sdkmath.NewInt(5_000_000)

	rec, err := reconcile.New(st, chain, reconcile.Config{ChainID: certChainID})
	if err != nil {
		return fail("reconciler: "+err.Error(), nil)
	}
	report, err := rec.ReconcileAddressReport(ctx, addr)
	if err != nil {
		return fail("reconcile: "+err.Error(), nil)
	}
	if report.DiscrepancyCount != 0 {
		return fail(fmt.Sprintf("clean ledger produced %d discrepancies", report.DiscrepancyCount),
			map[string]any{"report_id": report.ReportID})
	}
	// The report must be persisted and retrievable.
	if _, err := st.Recon().GetReport(ctx, report.ReportID); err != nil {
		return fail("report not persisted: "+err.Error(), nil)
	}
	return pass(map[string]any{
		"report_id":     report.ReportID,
		"address":       addr,
		"expected":      "5000000",
		"observed":      "5000000",
		"discrepancies": 0,
	})
}

// scenarioC2DiscrepancyFields: an injected on-chain shortfall must produce a
// discrepancy entry carrying every FR-048 field, and the discrepancy metric
// must move.
func scenarioC2DiscrepancyFields(ctx context.Context, rc *RunContext) Result {
	st, cleanup, err := tempStore("c2")
	if err != nil {
		return fail("temp store: "+err.Error(), nil)
	}
	defer cleanup()

	key, err := certKey(102)
	if err != nil {
		return fail("derive: "+err.Error(), nil)
	}
	addr := key.Bech32
	if err := watchAddr(ctx, st, certChainID, addr, storage.WatchCustomerDeposit); err != nil {
		return fail("watch: "+err.Error(), nil)
	}
	if err := seedExternalDeposit(ctx, st, addr, certLedgerTxHash, 10, 5_000_000); err != nil {
		return fail("ledger seed: "+err.Error(), nil)
	}

	chain := newStubChain(certChainID, 12)
	chain.balances[addr] = sdkmath.NewInt(4_600_000) // injected 0.4 SOVR shortfall

	m := metrics.NewSet()
	rec, err := reconcile.New(st, chain, reconcile.Config{ChainID: certChainID}, reconcile.WithMetrics(m))
	if err != nil {
		return fail("reconciler: "+err.Error(), nil)
	}
	report, err := rec.ReconcileAddressReport(ctx, addr)
	if err != nil {
		return fail("reconcile: "+err.Error(), nil)
	}
	if report.DiscrepancyCount != 1 || len(report.Entries) == 0 {
		return fail(fmt.Sprintf("expected exactly 1 discrepancy, got %d", report.DiscrepancyCount), nil)
	}
	var entry *storage.ReconciliationEntry
	for i := range report.Entries {
		if !report.Entries[i].Difference.IsZero() {
			entry = &report.Entries[i]
		}
	}
	if entry == nil {
		return fail("no non-zero discrepancy entry in the report", nil)
	}

	// FR-048 field completeness.
	var missing []string
	if entry.Address == "" {
		missing = append(missing, "address")
	}
	if entry.ExpectedBaseUnits.IsNil() {
		missing = append(missing, "expected")
	}
	if entry.ObservedBaseUnits.IsNil() {
		missing = append(missing, "observed")
	}
	if entry.Difference.IsNil() || entry.Difference.IsZero() {
		missing = append(missing, "difference")
	}
	if entry.EarliestSuspectedHeight == 0 {
		missing = append(missing, "earliest_suspected_height")
	}
	if len(entry.RelatedTxHashes) == 0 {
		missing = append(missing, "related_tx_hashes")
	}
	if entry.RecommendedRescanHeight == 0 {
		missing = append(missing, "recommended_rescan_height")
	}
	if len(missing) > 0 {
		return fail(fmt.Sprintf("FR-048 fields missing from the discrepancy entry: %v", missing), nil)
	}

	metric := counterValue(m.ReconciliationDiscrepancy.WithLabelValues(certChainID))
	if metric < 1 {
		return fail("sovren_reconciliation_discrepancies_total did not move", nil)
	}
	return pass(map[string]any{
		"report_id":                 report.ReportID,
		"expected":                  entry.ExpectedBaseUnits.String(),
		"observed":                  entry.ObservedBaseUnits.String(),
		"difference":                entry.Difference.String(),
		"earliest_suspected_height": entry.EarliestSuspectedHeight,
		"related_tx_hashes":         len(entry.RelatedTxHashes),
		"recommended_rescan_height": entry.RecommendedRescanHeight,
		"discrepancy_metric":        metric,
	})
}

// scenarioC3FeeOutflow: a failed withdrawal still pays its fee; the recorded
// FeeOutflow must make the follow-up hot-wallet reconciliation clean.
func scenarioC3FeeOutflow(ctx context.Context, rc *RunContext) Result {
	st, cleanup, err := tempStore("c3")
	if err != nil {
		return fail("temp store: "+err.Error(), nil)
	}
	defer cleanup()

	key, err := certKey(103)
	if err != nil {
		return fail("derive: "+err.Error(), nil)
	}
	hot := key.Bech32
	if err := watchAddr(ctx, st, certChainID, hot, storage.WatchHotWallet); err != nil {
		return fail("watch: "+err.Error(), nil)
	}
	if err := seedExternalDeposit(ctx, st, hot, certLedgerTxHash, 20, 10_000_000); err != nil {
		return fail("ledger seed: "+err.Error(), nil)
	}
	// Failed withdrawal (e.g. out of gas, code 11): the transfer moved
	// nothing, the ante-deducted fee did leave the wallet (data model §8a).
	if _, err := st.Ledger().AppendFeeOutflow(ctx, storage.FeeOutflow{
		ChainID:      certChainID,
		TxHash:       certFailedTxHash,
		PayerAddress: hot,
		FeeBaseUnits: sdkmath.NewInt(5_000),
		TxCode:       11,
		BlockHeight:  25,
		CreatedAt:    time.Now().UTC(),
	}); err != nil {
		return fail("fee outflow seed: "+err.Error(), nil)
	}

	chain := newStubChain(certChainID, 30)
	chain.balances[hot] = sdkmath.NewInt(10_000_000 - 5_000)

	rec, err := reconcile.New(st, chain, reconcile.Config{ChainID: certChainID})
	if err != nil {
		return fail("reconciler: "+err.Error(), nil)
	}

	hw, err := rec.HotWallet(ctx, hot)
	if err != nil {
		return fail("hot wallet reconcile: "+err.Error(), nil)
	}
	if !hw.Difference.IsZero() || !hw.Explained {
		return fail(fmt.Sprintf("hot wallet drifted despite FeeOutflow capture: difference=%s explained=%v",
			hw.Difference, hw.Explained), nil)
	}
	report, err := rec.ReconcileAddressReport(ctx, hot)
	if err != nil {
		return fail("address reconcile: "+err.Error(), nil)
	}
	if report.DiscrepancyCount != 0 {
		return fail(fmt.Sprintf("address formula missed the fee outflow: %d discrepancies", report.DiscrepancyCount), nil)
	}
	return pass(map[string]any{
		"hot_wallet":       hot,
		"failed_tx":        certFailedTxHash,
		"failed_tx_code":   11,
		"fee_captured":     "5000",
		"ledger_expected":  hw.LedgerExpected.String(),
		"observed":         hw.Observed.String(),
		"difference":       hw.Difference.String(),
		"address_report":   report.ReportID,
		"discrepancies":    report.DiscrepancyCount,
	})
}

// scenarioC4PauseIndependence: each FR-051 pause control flips exactly its
// own flow, every flip is audit-logged, and the credit gate follows the
// credit pause.
func scenarioC4PauseIndependence(ctx context.Context, rc *RunContext) Result {
	st, cleanup, err := tempStore("c4")
	if err != nil {
		return fail("temp store: "+err.Error(), nil)
	}
	defer cleanup()

	type flowCheck struct {
		name  string
		set   func(v bool) storage.ControlsUpdate
		state func(c storage.OperationalControls) bool
	}
	flows := []flowCheck{
		{"credit", func(v bool) storage.ControlsUpdate { return storage.ControlsUpdate{CreditPaused: &v} },
			func(c storage.OperationalControls) bool { return c.CreditPaused }},
		{"signing", func(v bool) storage.ControlsUpdate { return storage.ControlsUpdate{SigningPaused: &v} },
			func(c storage.OperationalControls) bool { return c.SigningPaused }},
		{"broadcast", func(v bool) storage.ControlsUpdate { return storage.ControlsUpdate{BroadcastPaused: &v} },
			func(c storage.OperationalControls) bool { return c.BroadcastPaused }},
		{"sweep", func(v bool) storage.ControlsUpdate { return storage.ControlsUpdate{SweepPaused: &v} },
			func(c storage.OperationalControls) bool { return c.SweepPaused }},
	}

	for i, f := range flows {
		if _, err := st.Controls().Apply(ctx, certChainID, f.set(true), "sovren-cert", "C4 pause "+f.name); err != nil {
			return fail("pause "+f.name+": "+err.Error(), nil)
		}
		c, err := st.Controls().Get(ctx, certChainID)
		if err != nil {
			return fail("controls read: "+err.Error(), nil)
		}
		for j, g := range flows {
			want := j == i
			if g.state(c) != want {
				return fail(fmt.Sprintf("pausing %s flipped %s (independence violated)", f.name, g.name), nil)
			}
		}
		if f.name == "credit" {
			gate, err := deposits.LoadCreditGate(ctx, st, certChainID)
			if err != nil {
				return fail("credit gate: "+err.Error(), nil)
			}
			if !gate.CreditPaused {
				return fail("credit gate did not observe the credit pause", nil)
			}
			d := storage.DepositRecord{
				Status: storage.DepositCreditable, Denom: storage.BaseDenom,
				AmountBaseUnits: sdkmath.NewInt(1), BlockHeight: 1,
			}
			decision, reason := deposits.EvaluateCreditConditions(d, 10, 1, gate)
			if decision != deposits.DecisionHold {
				return fail(fmt.Sprintf("credit decision under pause was %s (want HOLD): %s", decision, reason), nil)
			}
		}
		if _, err := st.Controls().Apply(ctx, certChainID, f.set(false), "sovren-cert", "C4 resume "+f.name); err != nil {
			return fail("resume "+f.name+": "+err.Error(), nil)
		}
	}

	final, err := st.Controls().Get(ctx, certChainID)
	if err != nil {
		return fail("controls read: "+err.Error(), nil)
	}
	if final.CreditPaused || final.SigningPaused || final.BroadcastPaused || final.SweepPaused {
		return fail("controls did not return to all-running after resumes", nil)
	}
	audit, err := st.Controls().ListAudit(ctx, certChainID, 100)
	if err != nil {
		return fail("audit read: "+err.Error(), nil)
	}
	if len(audit) < 8 {
		return fail(fmt.Sprintf("expected >= 8 audit rows (4 pauses + 4 resumes), got %d", len(audit)), nil)
	}
	return pass(map[string]any{
		"flows_checked": 4,
		"audit_rows":    len(audit),
		"final_state":   "all running",
	})
}

// scenarioC5ResumeFromHeight: a synthetic chain is scanned to the tip, then
// resume-from-height forces a full replay — the replay must hit only
// DUPLICATE identities and credit nothing twice.
func scenarioC5ResumeFromHeight(ctx context.Context, rc *RunContext) Result {
	st, cleanup, err := tempStore("c5")
	if err != nil {
		return fail("temp store: "+err.Error(), nil)
	}
	defer cleanup()

	sender, err := certKey(1)
	if err != nil {
		return fail("derive: "+err.Error(), nil)
	}
	recipient, err := certKey(2)
	if err != nil {
		return fail("derive: "+err.Error(), nil)
	}
	if err := watchAddr(ctx, st, certChainID, recipient.Bech32, storage.WatchCustomerDeposit); err != nil {
		return fail("watch: "+err.Error(), nil)
	}

	chain := newStubChain(certChainID, 8)
	txBytes, txHash, err := buildSignedSend(sender, recipient.Bech32, "2500000", "", certChainID, 0)
	if err != nil {
		return fail("build synthetic tx: "+err.Error(), nil)
	}
	chain.addTx(4, txBytes, client.TxExecResult{Code: 0})

	m := metrics.NewSet()
	sc, err := deposits.NewScanner(chain, st, deposits.ScannerConfig{
		ChainID:       certChainID,
		Confirmations: 1,
		StartHeight:   1,
		Metrics:       m,
	})
	if err != nil {
		return fail("scanner: "+err.Error(), nil)
	}
	if err := sc.Cycle(ctx); err != nil {
		return fail("initial scan: "+err.Error(), nil)
	}

	d, err := st.Deposits().Get(ctx, certChainID, txHash, 0, 0, recipient.Bech32)
	if err != nil {
		return fail("deposit not recorded after initial scan: "+err.Error(), nil)
	}
	if d.Status != storage.DepositCredited {
		return fail(fmt.Sprintf("deposit not credited after initial scan (status %s)", d.Status), nil)
	}
	cpBefore, err := st.Checkpoints().Get(ctx, certChainID)
	if err != nil {
		return fail("checkpoint read: "+err.Error(), nil)
	}
	creditedAtFirst := d.CreditedAt

	// Operator-driven rescan: resume_from_height goes through the controls
	// row (the admin API path) and the scanner consumes it next cycle.
	if err := sc.RescanFrom(ctx, 1, "sovren-cert", "C5 resume-from-height drill"); err != nil {
		return fail("rescan request: "+err.Error(), nil)
	}
	if err := sc.Cycle(ctx); err != nil {
		return fail("replay scan: "+err.Error(), nil)
	}

	d2, err := st.Deposits().Get(ctx, certChainID, txHash, 0, 0, recipient.Bech32)
	if err != nil {
		return fail("deposit lost during replay: "+err.Error(), nil)
	}
	if d2.Status != storage.DepositCredited || d2.CreditedAt == nil || creditedAtFirst == nil ||
		!d2.CreditedAt.Equal(*creditedAtFirst) {
		return fail("replay changed the credit (credited_at moved or status changed)", map[string]any{
			"status": string(d2.Status),
		})
	}
	cpAfter, err := st.Checkpoints().Get(ctx, certChainID)
	if err != nil {
		return fail("checkpoint read: "+err.Error(), nil)
	}
	if cpAfter.LastFullyProcessedHeight != cpBefore.LastFullyProcessedHeight {
		return fail(fmt.Sprintf("replay did not return the checkpoint to the tip (%d != %d)",
			cpAfter.LastFullyProcessedHeight, cpBefore.LastFullyProcessedHeight), nil)
	}
	dups := counterValue(m.DuplicateDeposits.WithLabelValues(certChainID))
	if dups < 1 {
		return fail("replay recorded no DUPLICATE observations (expected at least one)", nil)
	}
	credited := counterValue(m.DepositsCredited.WithLabelValues(certChainID))
	if credited != 1 {
		return fail(fmt.Sprintf("credited counter is %v after replay (want exactly 1)", credited), nil)
	}
	return pass(map[string]any{
		"tx_hash":            txHash,
		"deposit_height":     4,
		"checkpoint":         cpAfter.LastFullyProcessedHeight,
		"duplicates_on_replay": dups,
		"credited_total":     credited,
	})
}
