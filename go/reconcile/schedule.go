package reconcile

// Reconciliation schedules (T063, data model §8 kinds):
//
//	TX_NEAR_REAL_TIME — every NearRealTimeInterval, each watched address's new
//	                    ledger transactions are re-derived from chain truth
//	                    and diffed (ReconcileTx); reports persist only on
//	                    discrepancy.
//	WALLET_HOURLY     — every WalletInterval, operational wallets (hot / cold
//	                    / fee) balance-reconciled; report always persisted.
//	ADDRESS_DAILY     — every FullAddressInterval, every active watched
//	                    address; business-layer section computed alongside.
//	MANUAL            — operator-triggered via the admin API
//	                    (POST /v1/reconcile/tx, /v1/reconcile/address); no
//	                    schedule.

import (
	"context"
	"time"

	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

// Schedule carries the three periodic cadences. Zero values take the
// adapter.yaml contract defaults.
type Schedule struct {
	NearRealTimeInterval time.Duration // default 1m
	WalletInterval       time.Duration // default 1h (adapter.yaml reconciler.wallet_interval)
	FullAddressInterval  time.Duration // default 24h (reconciler.full_address_interval)
}

// Defaults for the Schedule zero values.
const (
	DefaultNearRealTimeInterval = time.Minute
	DefaultWalletInterval       = time.Hour
	DefaultFullAddressInterval  = 24 * time.Hour
)

func (s Schedule) withDefaults() Schedule {
	if s.NearRealTimeInterval <= 0 {
		s.NearRealTimeInterval = DefaultNearRealTimeInterval
	}
	if s.WalletInterval <= 0 {
		s.WalletInterval = DefaultWalletInterval
	}
	if s.FullAddressInterval <= 0 {
		s.FullAddressInterval = DefaultFullAddressInterval
	}
	return s
}

// RunSchedules drives the three periodic kinds until ctx is done. Pass
// errors are logged and retried on the next tick; nothing here is fatal —
// reconciliation is an observer, never a mutator of chain-facing state.
func (r *Reconciler) RunSchedules(ctx context.Context, sched Schedule) error {
	sched = sched.withDefaults()
	nrt := time.NewTicker(sched.NearRealTimeInterval)
	wallet := time.NewTicker(sched.WalletInterval)
	full := time.NewTicker(sched.FullAddressInterval)
	defer nrt.Stop()
	defer wallet.Stop()
	defer full.Stop()

	cursors := map[string]int64{}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-nrt.C:
			if err := r.NearRealTimePass(ctx, cursors); err != nil && ctx.Err() == nil {
				r.log.Warn("near-real-time reconciliation pass failed", "error", err.Error())
			}
		case <-wallet.C:
			if _, err := r.Run(ctx, storage.ReconWalletHourly); err != nil && ctx.Err() == nil {
				r.log.Warn("wallet reconciliation failed", "error", err.Error())
			}
		case <-full.C:
			if _, err := r.Run(ctx, storage.ReconAddressDaily); err != nil && ctx.Err() == nil {
				r.log.Warn("full address reconciliation failed", "error", err.Error())
			}
			if _, err := r.Business(ctx); err != nil && ctx.Err() == nil {
				r.log.Warn("business-layer reconciliation failed", "error", err.Error())
			}
		}
	}
}

// NearRealTimePass reconciles every transaction newly appended to the ledger
// since the previous pass (per-address AfterID cursors, in-memory — a
// restart simply restarts the window; the hourly/daily balance formulas
// cover anything a restart skipped). Exported for the admin service and
// tests.
func (r *Reconciler) NearRealTimePass(ctx context.Context, cursors map[string]int64) error {
	watched, err := r.store.Watch().ListActive(ctx, r.cfg.ChainID)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, w := range watched {
		afterID := cursors[w.Address]
		for {
			page, err := r.store.Ledger().List(ctx, storage.LedgerQuery{
				ChainID: r.cfg.ChainID, Address: w.Address, AfterID: afterID, Limit: ledgerPageSize,
			})
			if err != nil {
				return err
			}
			for _, e := range page {
				afterID = e.ID
				if e.Kind != storage.LedgerKindTx || e.TxHash == "" || seen[e.TxHash] {
					continue
				}
				seen[e.TxHash] = true
				if _, err := r.ReconcileTx(ctx, e.TxHash, storage.ReconTxNearRealTime); err != nil {
					return err
				}
			}
			if len(page) < ledgerPageSize {
				break
			}
		}
		cursors[w.Address] = afterID
	}
	return nil
}
