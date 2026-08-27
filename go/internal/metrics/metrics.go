// Package metrics is the kit's Prometheus instrument set: the full
// contracts/metrics.md contract (T007 skeleton + T066 completion) — the PRD
// §31.1 names verbatim plus the operational extras, all carrying chain_id.
//
// Metric names are a published contract (PRD §31.1); renaming is breaking.
// Gauges that mirror storage state (review-queue depth, open chain-review
// conditions, controls, deposit backlog, hot-wallet balances) are refreshed
// by the reconciler service's gauge loop — other services never need to
// emit them.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Set holds the kit's registered instruments. One Set per adapter process.
type Set struct {
	Registry *prometheus.Registry

	ScannerLatestChainHeight   *prometheus.GaugeVec   // chain_id
	ScannerLastProcessedHeight *prometheus.GaugeVec   // chain_id
	ScannerHeightLag           *prometheus.GaugeVec   // chain_id
	ScannerBlocksProcessed     *prometheus.CounterVec // chain_id
	DepositsDiscovered         *prometheus.CounterVec // chain_id
	DepositsCredited           *prometheus.CounterVec // chain_id
	DuplicateDeposits          *prometheus.CounterVec // chain_id
	WithdrawalsRequested       *prometheus.CounterVec // chain_id
	WithdrawalsBroadcast       *prometheus.CounterVec // chain_id
	WithdrawalsConfirmed       *prometheus.CounterVec // chain_id
	WithdrawalsFailed          *prometheus.CounterVec // chain_id, stage
	SequenceMismatch           *prometheus.CounterVec // chain_id
	RPCErrors                  *prometheus.CounterVec // chain_id, endpoint
	GRPCErrors                 *prometheus.CounterVec // chain_id, endpoint
	ReconciliationDiscrepancy  *prometheus.CounterVec // chain_id
	HotWalletBalanceUsovr      *prometheus.GaugeVec   // chain_id, address

	// Operational extras (contracts/metrics.md — same stability guarantee).
	ReviewQueueDepth          *prometheus.GaugeVec   // chain_id
	ChainReviewConditionsOpen *prometheus.GaugeVec   // chain_id, trigger
	ControlsPaused            *prometheus.GaugeVec   // chain_id, flow (1 = paused)
	SweepsDeferred            *prometheus.CounterVec // chain_id
	FeeFundingUsovr           *prometheus.CounterVec // chain_id (FEE_FUND funding legs confirmed)
	DepositBacklog            *prometheus.GaugeVec   // chain_id (CREDITABLE not yet CREDITED)
	BuildInfo                 *prometheus.GaugeVec   // version, commit

	// Alert-pack inputs not yet wired to a live source: UpgradePlanHeight is
	// set by the adapter's upgrade-plan poll once the client exposes the
	// x/upgrade query; UnsupportedBinaryVersion flips to 1 on a
	// manifest/app-version mismatch. Both stay absent (never scraped as 0)
	// until their producers emit them, so their alerts cannot false-fire.
	UpgradePlanHeight        *prometheus.GaugeVec // chain_id
	UnsupportedBinaryVersion *prometheus.GaugeVec // chain_id
}

// NewSet builds a fresh registry with the core instrument set.
func NewSet() *Set {
	reg := prometheus.NewRegistry()
	f := promauto.With(reg)
	byChain := []string{"chain_id"}
	s := &Set{
		Registry:                   reg,
		ScannerLatestChainHeight:   f.NewGaugeVec(prometheus.GaugeOpts{Name: "sovren_scanner_latest_chain_height"}, byChain),
		ScannerLastProcessedHeight: f.NewGaugeVec(prometheus.GaugeOpts{Name: "sovren_scanner_last_processed_height"}, byChain),
		ScannerHeightLag:           f.NewGaugeVec(prometheus.GaugeOpts{Name: "sovren_scanner_height_lag"}, byChain),
		ScannerBlocksProcessed:     f.NewCounterVec(prometheus.CounterOpts{Name: "sovren_scanner_blocks_processed_total"}, byChain),
		DepositsDiscovered:         f.NewCounterVec(prometheus.CounterOpts{Name: "sovren_deposits_discovered_total"}, byChain),
		DepositsCredited:           f.NewCounterVec(prometheus.CounterOpts{Name: "sovren_deposits_credited_total"}, byChain),
		DuplicateDeposits:          f.NewCounterVec(prometheus.CounterOpts{Name: "sovren_duplicate_deposits_total"}, byChain),
		WithdrawalsRequested:       f.NewCounterVec(prometheus.CounterOpts{Name: "sovren_withdrawals_requested_total"}, byChain),
		WithdrawalsBroadcast:       f.NewCounterVec(prometheus.CounterOpts{Name: "sovren_withdrawals_broadcast_total"}, byChain),
		WithdrawalsConfirmed:       f.NewCounterVec(prometheus.CounterOpts{Name: "sovren_withdrawals_confirmed_total"}, byChain),
		WithdrawalsFailed:          f.NewCounterVec(prometheus.CounterOpts{Name: "sovren_withdrawals_failed_total"}, []string{"chain_id", "stage"}),
		SequenceMismatch:           f.NewCounterVec(prometheus.CounterOpts{Name: "sovren_sequence_mismatch_total"}, byChain),
		RPCErrors:                  f.NewCounterVec(prometheus.CounterOpts{Name: "sovren_rpc_errors_total"}, []string{"chain_id", "endpoint"}),
		GRPCErrors:                 f.NewCounterVec(prometheus.CounterOpts{Name: "sovren_grpc_errors_total"}, []string{"chain_id", "endpoint"}),
		ReconciliationDiscrepancy:  f.NewCounterVec(prometheus.CounterOpts{Name: "sovren_reconciliation_discrepancies_total"}, byChain),
		HotWalletBalanceUsovr:      f.NewGaugeVec(prometheus.GaugeOpts{Name: "sovren_hot_wallet_balance_usovr"}, []string{"chain_id", "address"}),

		ReviewQueueDepth:          f.NewGaugeVec(prometheus.GaugeOpts{Name: "sovren_review_queue_depth"}, byChain),
		ChainReviewConditionsOpen: f.NewGaugeVec(prometheus.GaugeOpts{Name: "sovren_chain_review_conditions_open"}, []string{"chain_id", "trigger"}),
		ControlsPaused:            f.NewGaugeVec(prometheus.GaugeOpts{Name: "sovren_controls_paused"}, []string{"chain_id", "flow"}),
		SweepsDeferred:            f.NewCounterVec(prometheus.CounterOpts{Name: "sovren_sweeps_deferred_total"}, byChain),
		FeeFundingUsovr:           f.NewCounterVec(prometheus.CounterOpts{Name: "sovren_fee_funding_usovr_total"}, byChain),
		DepositBacklog:            f.NewGaugeVec(prometheus.GaugeOpts{Name: "sovren_deposit_backlog"}, byChain),
		BuildInfo:                 f.NewGaugeVec(prometheus.GaugeOpts{Name: "sovren_adapter_build_info"}, []string{"version", "commit"}),

		UpgradePlanHeight:        f.NewGaugeVec(prometheus.GaugeOpts{Name: "sovren_upgrade_plan_height"}, byChain),
		UnsupportedBinaryVersion: f.NewGaugeVec(prometheus.GaugeOpts{Name: "sovren_unsupported_binary_version"}, byChain),
	}
	return s
}

// Handler returns the Prometheus exposition handler for the adapter's
// :9464/metrics listener (contracts/metrics.md).
func (s *Set) Handler() http.Handler {
	return promhttp.HandlerFor(s.Registry, promhttp.HandlerOpts{})
}
