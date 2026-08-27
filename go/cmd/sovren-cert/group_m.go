package main

// Group M — monitoring (T075): contract metric presence + movement under a
// real component workload, and the alert-rule packs under promtool.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	sdkmath "cosmossdk.io/math"

	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
	"github.com/sovrn-tech/sovren-exchange-integration/go/deposits"
	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/metrics"
	"github.com/sovrn-tech/sovren-exchange-integration/go/reconcile"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

func init() {
	register("M1", scenarioM1Metrics)
	register("M2", scenarioM2AlertRules)
}

// scenarioM1Metrics drives the scanner and reconciler against the stub chain
// with one metric set attached, scrapes the Prometheus handler, and asserts
// the PRD §31.1 contract names are present and moved.
func scenarioM1Metrics(ctx context.Context, rc *RunContext) Result {
	st, cleanup, err := tempStore("m1")
	if err != nil {
		return fail("temp store: "+err.Error(), nil)
	}
	defer cleanup()

	sender, err := certKey(1)
	if err != nil {
		return fail("derive: "+err.Error(), nil)
	}
	recipient, err := certKey(3)
	if err != nil {
		return fail("derive: "+err.Error(), nil)
	}
	if err := watchAddr(ctx, st, certChainID, recipient.Bech32, storage.WatchCustomerDeposit); err != nil {
		return fail("watch: "+err.Error(), nil)
	}

	chain := newStubChain(certChainID, 6)
	txBytes, txHash, err := buildSignedSend(sender, recipient.Bech32, "1500000", "", certChainID, 0)
	if err != nil {
		return fail("build synthetic tx: "+err.Error(), nil)
	}
	chain.addTx(3, txBytes, client.TxExecResult{Code: 0})

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
		return fail("scan: "+err.Error(), nil)
	}
	// Replay to move the duplicate counter.
	if err := sc.RescanFrom(ctx, 1, "sovren-cert", "M1 metric movement"); err != nil {
		return fail("rescan: "+err.Error(), nil)
	}
	if err := sc.Cycle(ctx); err != nil {
		return fail("replay: "+err.Error(), nil)
	}

	// One injected discrepancy moves the reconciliation counter.
	chain.balances[recipient.Bech32] = sdkmath.NewInt(1) // ledger expects 1500000
	rec, err := reconcile.New(st, chain, reconcile.Config{ChainID: certChainID}, reconcile.WithMetrics(m))
	if err != nil {
		return fail("reconciler: "+err.Error(), nil)
	}
	if _, err := rec.ReconcileAddressReport(ctx, recipient.Bech32); err != nil {
		return fail("reconcile: "+err.Error(), nil)
	}

	// Scrape through the real handler (what Prometheus would see).
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		return fail("scrape: "+err.Error(), nil)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fail("scrape read: "+err.Error(), nil)
	}
	exposition := string(body)

	// Presence: every family this workload must have materialized.
	present := []string{
		"sovren_scanner_latest_chain_height",
		"sovren_scanner_last_processed_height",
		"sovren_scanner_height_lag",
		"sovren_scanner_blocks_processed_total",
		"sovren_deposits_discovered_total",
		"sovren_deposits_credited_total",
		"sovren_duplicate_deposits_total",
		"sovren_reconciliation_discrepancies_total",
	}
	var missing []string
	for _, name := range present {
		if !strings.Contains(exposition, name+"{") && !strings.Contains(exposition, name+" ") {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fail(fmt.Sprintf("contract metrics missing from the exposition: %v", missing), nil)
	}

	// Movement.
	moved := map[string]float64{
		"blocks_processed": counterValue(m.ScannerBlocksProcessed.WithLabelValues(certChainID)),
		"discovered":       counterValue(m.DepositsDiscovered.WithLabelValues(certChainID)),
		"credited":         counterValue(m.DepositsCredited.WithLabelValues(certChainID)),
		"duplicates":       counterValue(m.DuplicateDeposits.WithLabelValues(certChainID)),
		"discrepancies":    counterValue(m.ReconciliationDiscrepancy.WithLabelValues(certChainID)),
		"latest_height":    counterValue(m.ScannerLatestChainHeight.WithLabelValues(certChainID)),
	}
	for k, v := range moved {
		if v < 1 {
			return fail(fmt.Sprintf("metric %s did not move under load (value %v)", k, v), map[string]any{"tx": txHash})
		}
	}
	return pass(map[string]any{
		"families_checked":  len(present),
		"blocks_processed":  moved["blocks_processed"],
		"deposits_credited": moved["credited"],
		"duplicates":        moved["duplicates"],
		"discrepancies":     moved["discrepancies"],
		"latest_height":     moved["latest_height"],
	})
}

// scenarioM2AlertRules validates both shipped alert packs and runs their
// promtool unit tests (simulated firing conditions: scanner lag,
// reconciliation discrepancy, node health).
func scenarioM2AlertRules(ctx context.Context, rc *RunContext) Result {
	alertsDir := filepath.Join(rc.KitRoot, "monitoring", "alerts")
	testsDir := filepath.Join(alertsDir, "tests")

	checkOut, err := runCmd(ctx, alertsDir, 2*time.Minute, "promtool", "check", "rules",
		filepath.Join(alertsDir, "sovren-adapter-alerts.yml"),
		filepath.Join(alertsDir, "node-alerts.yml"))
	if err != nil {
		return fail("promtool check rules failed: "+err.Error(), map[string]any{"output": tailOf(checkOut, 2000)})
	}

	testFiles := []string{"adapter-alerts.test.yml", "node-alerts.test.yml"}
	outputs := map[string]any{"check": "ok"}
	for _, tf := range testFiles {
		out, err := runCmd(ctx, testsDir, 3*time.Minute, "promtool", "test", "rules", tf)
		outputs[tf] = tailOf(out, 1200)
		if err != nil {
			return fail(fmt.Sprintf("promtool test rules %s failed: %v", tf, err), outputs)
		}
	}
	return pass(outputs)
}

func runCmd(ctx context.Context, dir string, timeout time.Duration, name string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}
