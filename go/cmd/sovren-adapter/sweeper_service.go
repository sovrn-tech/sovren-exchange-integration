// sweeper service (T060 — owned by the sweeps track). Registered via
// init(); other services register from their own files, never this one.
//
// Drives the data-model §7 sweep engine (go/sweeps): plan → prepare →
// broadcast → confirm, with the FR-051 `sweep_paused` control honored on
// every step and deferrals surfaced through the
// sovren_sweeps_deferred_total counter. Sweep thresholds come exclusively
// from adapter.yaml `sweeps:` (FR-038/FR-040); gas parameters are shared
// with the withdrawals section; the signer is the same configured signer
// kind (secrets via environment, see withdrawals_service.go).
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/logging"
	"github.com/sovrn-tech/sovren-exchange-integration/go/sequences"
	"github.com/sovrn-tech/sovren-exchange-integration/go/signer"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
	"github.com/sovrn-tech/sovren-exchange-integration/go/sweeps"
)

func init() {
	register("sweeper", runSweeperService)
}

const sweeperDrivePeriod = 5 * time.Second

func runSweeperService(ctx context.Context, deps *Deps) error {
	log := deps.Logger.With(logging.FieldService, "sweeper")

	strategy := storage.SweepStrategy(deps.Config.Sweeps.Strategy)
	if strategy == "" {
		log.Info("sweeps.strategy not configured; sweeper idle")
		<-ctx.Done()
		return nil
	}

	// CUSTODY_ABSTRACTED emits no transactions and needs no signer.
	var sg signer.TransactionSigner
	if strategy != storage.StrategyCustodyAbstract {
		s, closeSigner, err := buildWithdrawalsSigner(deps)
		if err != nil {
			return fmt.Errorf("sweeper: signer: %w", err)
		}
		if closeSigner != nil {
			defer closeSigner()
		}
		sg = s
	}

	seqMgr := sequences.NewManager(deps.Store, deps.Client,
		sequences.WithLogger(log), sequences.WithMetrics(deps.Metrics))

	cfg, err := sweeperEngineConfig(deps, strategy)
	if err != nil {
		return fmt.Errorf("sweeper: config: %w", err)
	}
	engine, err := sweeps.New(deps.Store, deps.Client, seqMgr, sg, cfg,
		sweeps.WithLogger(log), sweeps.WithMetrics(deps.Metrics))
	if err != nil {
		return err
	}

	// Startup reconciliation (data model §6): every account with an
	// in-flight sweep is re-derived from chain truth before any new
	// sequence is handed out. Quarantined reservations stay quarantined;
	// nothing signed is ever released. Discovery must be COMPLETE and
	// reconciliation TOTAL — the sweeps engine's Pass loop has no per-account
	// filter this file could feed a blocked set into, so a discovery or
	// reconcile failure refuses to start the worker rather than fail-open.
	sources, err := sweeperSourceAddresses(ctx, deps)
	if err != nil {
		return fmt.Errorf("sweeper: startup source discovery: %w", err)
	}
	for _, source := range sources {
		report, err := seqMgr.ReconcileAccount(ctx, deps.Manifest.ChainID, source)
		if err != nil {
			return fmt.Errorf("sweeper: startup reconciliation for %s: %w", source, err)
		}
		if len(report.Actions) > 0 {
			log.Info("startup sequence reconciliation",
				logging.FieldAddress, source,
				"consumed", report.Consumed, "released", report.Released,
				"quarantined", report.Quarantined)
		}
	}

	ticker := time.NewTicker(sweeperDrivePeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			report := engine.Pass(ctx)
			for _, err := range report.Errors {
				log.Warn("sweep step failed", "error", err.Error())
			}
		}
	}
}

// sweeperEngineConfig maps adapter.yaml + manifest values into the engine
// config; every economic value stays configuration (FR-038/FR-040).
func sweeperEngineConfig(deps *Deps, strategy storage.SweepStrategy) (sweeps.Config, error) {
	confirmations, _, _, err := deps.Config.ScannerRuntime()
	if err != nil {
		return sweeps.Config{}, err
	}
	broadcastTimeout := 15 * time.Second
	if v := deps.Config.Withdrawals.BroadcastTimeout; v != "" {
		if broadcastTimeout, err = time.ParseDuration(v); err != nil {
			return sweeps.Config{}, fmt.Errorf("withdrawals.broadcast_timeout: %w", err)
		}
	}
	cfg := sweeps.Config{
		ChainID:                      deps.Manifest.ChainID,
		Strategy:                     strategy,
		HotWallet:                    deps.Config.Sweeps.HotWallet,
		MinimumSweepAmountUsovr:      deps.Config.Sweeps.MinimumSweepAmountUsovr,
		MaximumFeePercentageForSweep: deps.Config.Sweeps.MaximumFeePercentageForSweep,
		FeeReserveUsovr:              deps.Config.Sweeps.FeeReserveUsovr,
		FeeWalletMaxSpendUsovr:       deps.Config.Sweeps.FeeWalletMaxSpendUsovr,
		FeeWalletSpendWindowBlocks:   deps.Config.Sweeps.FeeWalletSpendWindowBlocks,
		BroadcastTimeout:             broadcastTimeout,
		Confirmations:                confirmations,
	}
	if strategy == storage.StrategyCustodyAbstract {
		return cfg, nil
	}

	// Gas parameters are shared with the withdrawals section; the fee-safety
	// default (queue on simulate-unavailable) matches withdrawals. An empty
	// gas_adjustment falls back to the manifest's recommended_gas_adjustment,
	// never a hardcoded constant (which had lagged the manifest at a
	// too-low value and starved live sweeps of gas).
	gasAdjustment, err := resolveGasAdjustment(deps.Config.Withdrawals.GasAdjustment, deps.Manifest.Fees.RecommendedGasAdjustment)
	if err != nil {
		return sweeps.Config{}, err
	}
	cfg.GasAdjustment = gasAdjustment
	gasPrice, err := withdrawalsGasPrice(deps.Manifest.Fees.RecommendedGasPrice, deps.Manifest.BaseDenom)
	if err != nil {
		return sweeps.Config{}, err
	}
	cfg.GasPriceUsovr = gasPrice
	cfg.SimulateUnavailable = sweeps.SimulateQueue
	if deps.Config.Withdrawals.SimulateUnavailable == "static" {
		cfg.SimulateUnavailable = sweeps.SimulateStatic
	}
	// Shared static MsgSend gas override (same knob as the withdrawals
	// service — one MsgSend shape, one static gas).
	cfg.StaticGasLimit = 120000
	if v := os.Getenv("SOVREN_WITHDRAWALS_STATIC_GAS"); v != "" {
		if cfg.StaticGasLimit, err = strconv.ParseUint(v, 10, 64); err != nil {
			return sweeps.Config{}, fmt.Errorf("SOVREN_WITHDRAWALS_STATIC_GAS: %w", err)
		}
	}
	return cfg, nil
}

// sweeperSourceAddresses collects every account with an in-flight sweep
// (customer sources and, for FEE_FUND, the fee wallet) for startup
// reconciliation. Store errors PROPAGATE and every status queue is paged to
// completion so no in-flight source is silently dropped past the first page.
func sweeperSourceAddresses(ctx context.Context, deps *Deps) ([]string, error) {
	chainID := deps.Manifest.ChainID
	seen := map[string]bool{}
	var out []string
	add := func(addr string) {
		if addr != "" && !seen[addr] {
			seen[addr] = true
			out = append(out, addr)
		}
	}
	for _, status := range []storage.SweepStatus{
		storage.SweepPending, storage.SweepBuilt,
		storage.SweepSigned, storage.SweepBroadcast, storage.SweepDeferred,
	} {
		status := status
		err := paginateAll(func(limit int) (int, error) {
			jobs, err := deps.Store.Sweeps().ListByStatus(ctx, chainID, status, limit)
			if err != nil {
				return 0, fmt.Errorf("list sweeps in status %s: %w", status, err)
			}
			for _, j := range jobs {
				add(j.SourceAddress)
			}
			return len(jobs), nil
		})
		if err != nil {
			return nil, err
		}
	}
	watched, err := deps.Store.Watch().ListActive(ctx, chainID)
	if err != nil {
		return nil, fmt.Errorf("list active watched addresses: %w", err)
	}
	for _, w := range watched {
		if w.Kind == storage.WatchFeeWallet {
			add(w.Address)
		}
	}
	return out, nil
}
