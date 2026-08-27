// reconciler service (T069 — owned by the reconciliation track). Registered
// via init(); other services register from their own files, never this one.
//
// One service, three responsibilities:
//
//  1. the data-model §8 reconciliation schedules — near-real-time per-tx,
//     hourly wallet, daily full-address (+ business-layer section), with
//     MANUAL runs served by the admin API (admin_controls.go);
//  2. the FR-044 node-disagreement monitor, when a secondary node is
//     configured (nodes.secondary) — single-node deployments log the
//     degradation and run without it;
//  3. the storage-mirroring metric gauge loop (T066): review-queue depth,
//     open chain-review conditions, per-flow controls, deposit backlog and
//     hot-wallet balances are read from the shared store here so no other
//     service needs cross-file emission hooks.
//
// `sovren-adapter all` runs scanner + withdrawals + sweeper + reconciler in
// one process over one shared store, one controls switchboard, and one
// metric registry (main.go builds a single Deps for every registered
// service).
package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/logging"
	"github.com/sovrn-tech/sovren-exchange-integration/go/reconcile"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

func init() {
	register("reconciler", runReconciler)
}

// gaugeRefreshInterval matches the monitoring contract's 15s scrape cadence.
const gaugeRefreshInterval = 15 * time.Second

func runReconciler(ctx context.Context, deps *Deps) error {
	log := deps.Logger.With(logging.FieldService, "reconciler")
	sched, disagreement, err := reconcilerRuntime(deps)
	if err != nil {
		return fmt.Errorf("reconciler: config: %w", err)
	}

	var wg sync.WaitGroup
	start := func(name string, fn func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(ctx); err != nil && ctx.Err() == nil {
				log.Error("reconciler component failed", "component", name, "error", err.Error())
			}
		}()
	}

	// Gauge loop always runs — it needs only the store (+ client for
	// hot-wallet balances, skipped when unavailable).
	start("gauges", func(ctx context.Context) error {
		return runGaugeLoop(ctx, deps)
	})

	if deps.Client != nil {
		rec, err := reconcile.New(deps.Store, deps.Client,
			reconcile.Config{ChainID: deps.Manifest.ChainID},
			reconcile.WithLogger(log), reconcile.WithMetrics(deps.Metrics))
		if err != nil {
			return err
		}
		start("schedules", func(ctx context.Context) error {
			return rec.RunSchedules(ctx, sched)
		})
	} else {
		log.Warn("chain client unavailable; reconciliation schedules disabled (gauge loop only)")
	}

	if fo, ok := deps.Client.(*client.Failover); ok {
		disagreement.ChainID = deps.Manifest.ChainID
		if hw := firstHotWallet(ctx, deps); hw != "" {
			disagreement.SequenceAddress = hw
			disagreement.BalanceAddress = hw
		}
		mon, err := reconcile.NewMonitor(fo, deps.Store, disagreement,
			reconcile.WithMonitorLogger(log), reconcile.WithMonitorMetrics(deps.Metrics))
		if err != nil {
			return err
		}
		start("disagreement", mon.Run)
	} else {
		log.Info("no secondary node configured; FR-044 disagreement monitoring disabled")
	}

	wg.Wait()
	return nil
}

// reconcilerRuntime resolves adapter.yaml (+ env overrides) into the runtime
// schedule and disagreement config. SOVREN_RECONCILER_NRT_INTERVAL overrides
// the near-real-time cadence (default 1m), which has no adapter.yaml field.
func reconcilerRuntime(deps *Deps) (reconcile.Schedule, reconcile.DisagreementConfig, error) {
	var sched reconcile.Schedule
	if v := deps.Config.Reconciler.WalletInterval; v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return sched, reconcile.DisagreementConfig{}, fmt.Errorf("reconciler.wallet_interval: %w", err)
		}
		sched.WalletInterval = d
	}
	if v := deps.Config.Reconciler.FullAddressInterval; v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return sched, reconcile.DisagreementConfig{}, fmt.Errorf("reconciler.full_address_interval: %w", err)
		}
		sched.FullAddressInterval = d
	}
	if v := os.Getenv("SOVREN_RECONCILER_NRT_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return sched, reconcile.DisagreementConfig{}, fmt.Errorf("SOVREN_RECONCILER_NRT_INTERVAL: %w", err)
		}
		sched.NearRealTimeInterval = d
	}

	dis := reconcile.DisagreementConfig{CompareHashAtCheckpoint: true}
	if dc := deps.Config.Nodes.Disagreement; dc != nil {
		dis.HeightTolerance = int64(dc.HeightDivergenceThreshold)
		if dc.CheckInterval != "" {
			d, err := time.ParseDuration(dc.CheckInterval)
			if err != nil {
				return sched, dis, fmt.Errorf("nodes.disagreement.check_interval: %w", err)
			}
			dis.Interval = d
		}
	}
	return sched, dis, nil
}

// firstHotWallet returns the first active hot wallet, or "".
func firstHotWallet(ctx context.Context, deps *Deps) string {
	watched, err := deps.Store.Watch().ListActive(ctx, deps.Manifest.ChainID)
	if err != nil {
		return ""
	}
	for _, w := range watched {
		if w.Kind == storage.WatchHotWallet {
			return w.Address
		}
	}
	return ""
}

// runGaugeLoop refreshes the storage-mirroring gauges every
// gaugeRefreshInterval until ctx is done.
func runGaugeLoop(ctx context.Context, deps *Deps) error {
	ticker := time.NewTicker(gaugeRefreshInterval)
	defer ticker.Stop()
	for {
		if err := refreshGauges(ctx, deps); err != nil && ctx.Err() == nil {
			deps.Logger.Warn("gauge refresh failed", logging.FieldService, "reconciler", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

const gaugeListLimit = 10000

// refreshGauges reads shared storage once and sets every mirrored gauge.
// Exported-shape helper (called from tests) — it must never mutate state.
func refreshGauges(ctx context.Context, deps *Deps) error {
	if deps.Metrics == nil {
		return nil
	}
	chainID := deps.Manifest.ChainID
	m := deps.Metrics

	controls, err := deps.Store.Controls().Get(ctx, chainID)
	if err != nil {
		return err
	}
	pausedVal := func(paused bool) float64 {
		if paused {
			return 1
		}
		return 0
	}
	m.ControlsPaused.WithLabelValues(chainID, string(storage.FlowCredit)).Set(pausedVal(controls.CreditPaused))
	m.ControlsPaused.WithLabelValues(chainID, string(storage.FlowSigning)).Set(pausedVal(controls.SigningPaused))
	m.ControlsPaused.WithLabelValues(chainID, string(storage.FlowBroadcast)).Set(pausedVal(controls.BroadcastPaused))
	m.ControlsPaused.WithLabelValues(chainID, string(storage.FlowSweep)).Set(pausedVal(controls.SweepPaused))

	review, err := deps.Store.Review().ListOpen(ctx, chainID, gaugeListLimit)
	if err != nil {
		return err
	}
	m.ReviewQueueDepth.WithLabelValues(chainID).Set(float64(len(review)))

	open, err := deps.Store.ChainReview().ListOpen(ctx, chainID)
	if err != nil {
		return err
	}
	counts := map[storage.ChainReviewTrigger]int{}
	for _, c := range open {
		counts[c.Trigger]++
	}
	for _, t := range storage.AllChainReviewTriggers {
		m.ChainReviewConditionsOpen.WithLabelValues(chainID, string(t)).Set(float64(counts[t]))
	}

	backlog, err := deps.Store.Deposits().ListByStatus(ctx, chainID, storage.DepositCreditable, gaugeListLimit)
	if err != nil {
		return err
	}
	m.DepositBacklog.WithLabelValues(chainID).Set(float64(len(backlog)))

	if deps.Client != nil {
		watched, err := deps.Store.Watch().ListActive(ctx, chainID)
		if err != nil {
			return err
		}
		for _, w := range watched {
			if w.Kind != storage.WatchHotWallet && w.Kind != storage.WatchFeeWallet {
				continue
			}
			bal, err := deps.Client.Balance(ctx, w.Address, storage.BaseDenom)
			if err != nil {
				continue // transient node error; next tick retries
			}
			f, _ := bal.BigInt().Float64()
			m.HotWalletBalanceUsovr.WithLabelValues(chainID, w.Address).Set(f)
		}
	}
	return nil
}
