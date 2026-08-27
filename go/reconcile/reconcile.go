// Package reconcile is the kit's reconciliation engine (T063; FR-046–FR-048,
// data model §8/§8a):
//
//   - the ledger-based address formula — expected balance = Σ ChainTransferLedger
//     inflows − Σ outflows (including FEE_DEDUCTION fee outflows), computed from
//     the immutable ledger only and therefore independent of customer-credit
//     workflow status: below-minimum, review-parked, fee-funding and internal
//     movements are all in the ledger and can never produce false discrepancies;
//   - per-transaction reconciliation (chain truth re-derived and diffed against
//     the persisted ledger rows for that transaction);
//   - hot-wallet comparison including pending-signed / broadcast-unconfirmed
//     in-flight work (business.go);
//   - a separate business-layer section reconciling customer-credit and
//     sweep/withdrawal workflow totals against the ledger (business.go);
//   - schedule kinds — near-real-time / hourly wallet / daily full-address /
//     manual (schedule.go);
//   - the FR-044 two-node disagreement monitor (disagreement.go).
//
// Every non-zero difference produces a ReconciliationEntry carrying all FR-048
// fields (address, expected, observed, difference, earliest suspected height,
// related tx hashes, recommended rescan height), increments
// sovren_reconciliation_discrepancies_total, and logs the alert payload
// verbatim.
package reconcile

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	sdkmath "cosmossdk.io/math"

	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
	"github.com/sovrn-tech/sovren-exchange-integration/go/deposits"
	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/logging"
	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/metrics"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

// ChainView is the read-only chain subset the reconciler needs.
// client.Client implementations and the failover wrapper satisfy it.
type ChainView interface {
	Balance(ctx context.Context, addr, denom string) (sdkmath.Int, error)
	Tx(ctx context.Context, hash string) (*client.TxInfo, error)
	LatestBlock(ctx context.Context) (*client.Block, error)
}

const (
	// ledgerPageSize bounds each ledger pagination step.
	ledgerPageSize = 500
	// maxRelatedTxHashes caps the related_tx_hashes list in one entry.
	maxRelatedTxHashes = 50
	// workflowListLimit bounds the ListByStatus sweeps used by the
	// business-layer section (reference-adapter bound, documented in
	// docs/reconciliation.md).
	workflowListLimit = 10000
)

// Config configures a Reconciler.
type Config struct {
	ChainID string
}

// Reconciler computes reconciliation reports from the store's ledger and the
// live chain.
type Reconciler struct {
	store   storage.Store
	chain   ChainView
	cfg     Config
	log     *slog.Logger
	metrics *metrics.Set
	now     func() time.Time
	newID   func() string
}

// Option configures a Reconciler.
type Option func(*Reconciler)

// WithLogger replaces the default logger.
func WithLogger(l *slog.Logger) Option { return func(r *Reconciler) { r.log = l } }

// WithMetrics attaches the adapter metric set (discrepancy counter + gauges).
func WithMetrics(s *metrics.Set) Option { return func(r *Reconciler) { r.metrics = s } }

// WithNow injects the clock (tests).
func WithNow(now func() time.Time) Option { return func(r *Reconciler) { r.now = now } }

// New builds a Reconciler. chain may be nil only for ledger-only operations
// (ExpectedBalance); every comparison against live state requires it.
func New(store storage.Store, chain ChainView, cfg Config, opts ...Option) (*Reconciler, error) {
	if store == nil {
		return nil, fmt.Errorf("reconcile: store required")
	}
	if cfg.ChainID == "" {
		return nil, fmt.Errorf("reconcile: ChainID required")
	}
	r := &Reconciler{
		store: store, chain: chain, cfg: cfg,
		log:   logging.New("reconcile"),
		now:   time.Now,
		newID: newID,
	}
	for _, o := range opts {
		o(r)
	}
	r.log = r.log.With(logging.FieldChainID, cfg.ChainID)
	return r, nil
}

// LedgerPosition summarizes the ledger walk backing one expected-balance
// computation.
type LedgerPosition struct {
	// Expected is Σ inflows − Σ outflows − Σ fee deductions.
	Expected sdkmath.Int
	// EarliestHeight is the lowest block height among the address's
	// counted ledger rows (0 when the address has no ledger activity).
	EarliestHeight uint64
	// LatestHeight is the highest counted block height.
	LatestHeight uint64
	// RelatedTxHashes are the distinct tx hashes touched (newest last,
	// capped at maxRelatedTxHashes).
	RelatedTxHashes []string
	// Rows is the number of counted ledger rows (fee outflows excluded).
	Rows int
}

// ExpectedBalance computes the ledger-based expected balance for one address
// (data model §8): every ledger row regardless of classification — including
// UNATTRIBUTED_REVIEW and block-scoped rows — counts, because the ledger
// records on-chain truth, not workflow opinion. Rows from failed transactions
// (tx_code != 0) moved no funds and are excluded; the fee those transactions
// still paid is captured by the FEE_DEDUCTION table and subtracted here.
func (r *Reconciler) ExpectedBalance(ctx context.Context, address string) (LedgerPosition, error) {
	pos := LedgerPosition{Expected: sdkmath.ZeroInt()}
	seenTx := map[string]bool{}
	var afterID int64
	for {
		page, err := r.store.Ledger().List(ctx, storage.LedgerQuery{
			ChainID: r.cfg.ChainID,
			Address: address,
			AfterID: afterID,
			Limit:   ledgerPageSize,
		})
		if err != nil {
			return pos, fmt.Errorf("reconcile: ledger list %s: %w", address, err)
		}
		for _, e := range page {
			afterID = e.ID
			if e.TxCode != 0 {
				continue // failed tx moved no funds (FR-029)
			}
			pos.Rows++
			if pos.EarliestHeight == 0 || e.BlockHeight < pos.EarliestHeight {
				pos.EarliestHeight = e.BlockHeight
			}
			if e.BlockHeight > pos.LatestHeight {
				pos.LatestHeight = e.BlockHeight
			}
			if e.TxHash != "" && !seenTx[e.TxHash] {
				seenTx[e.TxHash] = true
				if len(pos.RelatedTxHashes) < maxRelatedTxHashes {
					pos.RelatedTxHashes = append(pos.RelatedTxHashes, e.TxHash)
				}
			}
			switch e.Direction {
			case storage.DirectionIn:
				pos.Expected = pos.Expected.Add(e.AmountBaseUnits)
			case storage.DirectionOut:
				pos.Expected = pos.Expected.Sub(e.AmountBaseUnits)
			}
		}
		if len(page) < ledgerPageSize {
			break
		}
	}

	fees, err := r.store.Ledger().ListFeeOutflows(ctx, r.cfg.ChainID, address, 0, 0)
	if err != nil {
		return pos, fmt.Errorf("reconcile: fee outflows %s: %w", address, err)
	}
	for _, f := range fees {
		pos.Expected = pos.Expected.Sub(f.FeeBaseUnits)
		if pos.EarliestHeight == 0 || f.BlockHeight < pos.EarliestHeight {
			pos.EarliestHeight = f.BlockHeight
		}
		if f.BlockHeight > pos.LatestHeight {
			pos.LatestHeight = f.BlockHeight
		}
		if f.TxHash != "" && !seenTx[f.TxHash] {
			seenTx[f.TxHash] = true
			if len(pos.RelatedTxHashes) < maxRelatedTxHashes {
				pos.RelatedTxHashes = append(pos.RelatedTxHashes, f.TxHash)
			}
		}
	}
	return pos, nil
}

// ReconcileAddress compares the ledger-based expected balance against the
// live chain balance for one address and returns the FR-048 entry.
func (r *Reconciler) ReconcileAddress(ctx context.Context, address string) (storage.ReconciliationEntry, error) {
	if r.chain == nil {
		return storage.ReconciliationEntry{}, fmt.Errorf("reconcile: chain client required for address reconciliation")
	}
	pos, err := r.ExpectedBalance(ctx, address)
	if err != nil {
		return storage.ReconciliationEntry{}, err
	}
	observed, err := r.chain.Balance(ctx, address, storage.BaseDenom)
	if err != nil {
		return storage.ReconciliationEntry{}, fmt.Errorf("reconcile: balance %s: %w", address, err)
	}
	entry := storage.ReconciliationEntry{
		Address:           address,
		ExpectedBaseUnits: pos.Expected,
		ObservedBaseUnits: observed,
		Difference:        observed.Sub(pos.Expected),
	}
	if !entry.Difference.IsZero() {
		entry.EarliestSuspectedHeight = pos.EarliestHeight
		entry.RelatedTxHashes = pos.RelatedTxHashes
		entry.RecommendedRescanHeight = recommendedRescan(pos.EarliestHeight)
	}
	return entry, nil
}

// recommendedRescan maps an earliest-suspected height to the rescan base:
// re-scanning from the suspect height is idempotent (unique keys make replay
// safe — FR-024); height 0 (no ledger activity) rescans from genesis.
func recommendedRescan(earliest uint64) uint64 {
	if earliest == 0 {
		return 1
	}
	return earliest
}

// Run generates one reconciliation report for the given kind:
//
//	WALLET_HOURLY  — operational wallets only (hot / cold / fee)
//	ADDRESS_DAILY  — every active watched address
//	MANUAL         — every active watched address, operator-triggered
//
// (TX_NEAR_REAL_TIME reports come from ReconcileTx.) The report is persisted;
// every non-zero difference emits the FR-048 alert.
func (r *Reconciler) Run(ctx context.Context, kind storage.ReconciliationKind) (storage.ReconciliationReport, error) {
	if !kind.Valid() || kind == storage.ReconTxNearRealTime {
		return storage.ReconciliationReport{}, fmt.Errorf("reconcile: unsupported run kind %q", kind)
	}
	watched, err := r.store.Watch().ListActive(ctx, r.cfg.ChainID)
	if err != nil {
		return storage.ReconciliationReport{}, fmt.Errorf("reconcile: watch set: %w", err)
	}
	var entries []storage.ReconciliationEntry
	for _, w := range watched {
		if kind == storage.ReconWalletHourly && !isOperationalWallet(w.Kind) {
			continue
		}
		entry, err := r.ReconcileAddress(ctx, w.Address)
		if err != nil {
			return storage.ReconciliationReport{}, err
		}
		entries = append(entries, entry)
	}
	return r.finishReport(ctx, kind, entries)
}

func isOperationalWallet(k storage.WatchedAddressKind) bool {
	return k == storage.WatchHotWallet || k == storage.WatchColdWallet || k == storage.WatchFeeWallet
}

// ReconcileAddressReport reconciles one address now and persists a MANUAL
// report (admin POST /v1/reconcile/address).
func (r *Reconciler) ReconcileAddressReport(ctx context.Context, address string) (storage.ReconciliationReport, error) {
	entry, err := r.ReconcileAddress(ctx, address)
	if err != nil {
		return storage.ReconciliationReport{}, err
	}
	return r.finishReport(ctx, storage.ReconManual, []storage.ReconciliationEntry{entry})
}

// ReconcileTx re-derives one transaction's watched-address movements from
// chain truth and diffs them against the persisted ledger rows (the
// TX_NEAR_REAL_TIME / manual per-tx check). kind must be TX_NEAR_REAL_TIME or
// MANUAL. A transaction the chain does not know is reported without entries
// (nothing on-chain to reconcile against; the ledger cannot be queried by
// bare hash) and logged.
func (r *Reconciler) ReconcileTx(ctx context.Context, txHash string, kind storage.ReconciliationKind) (storage.ReconciliationReport, error) {
	if kind != storage.ReconTxNearRealTime && kind != storage.ReconManual {
		return storage.ReconciliationReport{}, fmt.Errorf("reconcile: unsupported tx-reconcile kind %q", kind)
	}
	if r.chain == nil {
		return storage.ReconciliationReport{}, fmt.Errorf("reconcile: chain client required for tx reconciliation")
	}
	info, err := r.chain.Tx(ctx, txHash)
	if errors.Is(err, client.ErrNotFound) {
		r.log.Warn("reconcile tx: transaction not found on chain",
			logging.FieldTxHash, txHash)
		return r.finishReport(ctx, kind, nil)
	}
	if err != nil {
		return storage.ReconciliationReport{}, fmt.Errorf("reconcile: tx %s: %w", txHash, err)
	}

	watched, err := r.store.Watch().ListActive(ctx, r.cfg.ChainID)
	if err != nil {
		return storage.ReconciliationReport{}, fmt.Errorf("reconcile: watch set: %w", err)
	}
	watch := deposits.NewWatchSet(watched)

	// Synthesize a single-tx block parse: the same tolerant decode +
	// classification the scanner used, re-run against chain truth.
	bp, err := deposits.ParseBlockTransfers(
		&client.Block{ChainID: r.cfg.ChainID, Height: info.Height, Txs: [][]byte{info.TxBytes}},
		&client.BlockResults{Height: info.Height, TxResults: []client.TxExecResult{{
			Code: info.Code, Log: info.RawLog, Events: info.Events,
		}}},
		watch,
	)
	if err != nil {
		return storage.ReconciliationReport{}, fmt.Errorf("reconcile: parse tx %s: %w", txHash, err)
	}

	// Convention (data model §8): expected = what the persisted ledger holds,
	// observed = what the chain reports now; difference = observed − expected.
	height := uint64(info.Height)
	var entries []storage.ReconciliationEntry
	for _, c := range bp.Transfers {
		if c.TxCode != 0 {
			continue // failed tx moved no funds; fee handled below
		}
		observed := signedAmount(c.Direction, c.AmountBaseUnits) // chain truth
		expected := sdkmath.ZeroInt()                            // ledger row
		stored, err := r.store.Ledger().GetTxEntry(ctx, r.cfg.ChainID, c.TxHash, c.MessageIndex, c.OpIndex)
		switch {
		case err == nil:
			expected = signedAmount(stored.Direction, stored.AmountBaseUnits)
		case errors.Is(err, storage.ErrNotFound):
			// missing ledger row — expected stays zero
		default:
			return storage.ReconciliationReport{}, fmt.Errorf("reconcile: ledger lookup %s/%d/%d: %w", c.TxHash, c.MessageIndex, c.OpIndex, err)
		}
		if !expected.Equal(observed) {
			entries = append(entries, storage.ReconciliationEntry{
				Address:                 c.Address,
				ExpectedBaseUnits:       expected,
				ObservedBaseUnits:       observed,
				Difference:              observed.Sub(expected),
				EarliestSuspectedHeight: height,
				RelatedTxHashes:         []string{c.TxHash},
				RecommendedRescanHeight: recommendedRescan(height),
			})
		}
	}
	for _, f := range bp.FeeDeductions {
		rows, err := r.store.Ledger().ListFeeOutflows(ctx, r.cfg.ChainID, f.PayerAddress, height, height)
		if err != nil {
			return storage.ReconciliationReport{}, fmt.Errorf("reconcile: fee outflows %s: %w", f.PayerAddress, err)
		}
		found := false
		for _, row := range rows {
			if row.TxHash == txHash && row.FeeBaseUnits.Equal(f.FeeBaseUnits) {
				found = true
				break
			}
		}
		if !found {
			entries = append(entries, storage.ReconciliationEntry{
				Address:                 f.PayerAddress,
				ExpectedBaseUnits:       sdkmath.ZeroInt(),
				ObservedBaseUnits:       f.FeeBaseUnits.Neg(),
				Difference:              f.FeeBaseUnits.Neg(),
				EarliestSuspectedHeight: height,
				RelatedTxHashes:         []string{txHash},
				RecommendedRescanHeight: recommendedRescan(height),
			})
		}
	}
	return r.finishReport(ctx, kind, entries)
}

// finishReport counts discrepancies, emits alerts, persists the report
// (near-real-time reports are persisted only when they carry discrepancies —
// a clean per-tx check every few seconds is metric noise, not audit
// material), and returns it.
func (r *Reconciler) finishReport(ctx context.Context, kind storage.ReconciliationKind, entries []storage.ReconciliationEntry) (storage.ReconciliationReport, error) {
	now := r.now().UTC()
	rep := storage.ReconciliationReport{
		ReportID:    r.newID(),
		ChainID:     r.cfg.ChainID,
		Kind:        kind,
		PeriodStart: r.periodStart(ctx, kind, now),
		PeriodEnd:   now,
		GeneratedAt: now,
		Entries:     entries,
	}
	for _, e := range entries {
		if e.Difference.IsZero() {
			continue
		}
		rep.DiscrepancyCount++
		r.emitDiscrepancy(rep, e)
	}
	if kind == storage.ReconTxNearRealTime && rep.DiscrepancyCount == 0 {
		return rep, nil
	}
	if err := r.store.Recon().SaveReport(ctx, rep); err != nil {
		return rep, fmt.Errorf("reconcile: save report: %w", err)
	}
	return rep, nil
}

// periodStart is the previous same-kind report's generation time (a
// contiguous period chain), or the zero time for the first report.
func (r *Reconciler) periodStart(ctx context.Context, kind storage.ReconciliationKind, now time.Time) time.Time {
	prev, err := r.store.Recon().ListReports(ctx, r.cfg.ChainID, kind, 1)
	if err != nil || len(prev) == 0 {
		return time.Time{}
	}
	if prev[0].GeneratedAt.After(now) {
		return now
	}
	return prev[0].GeneratedAt
}

// emitDiscrepancy increments the zero-tolerance counter and logs the FR-048
// alert payload verbatim (every field of the entry).
func (r *Reconciler) emitDiscrepancy(rep storage.ReconciliationReport, e storage.ReconciliationEntry) {
	if r.metrics != nil {
		r.metrics.ReconciliationDiscrepancy.WithLabelValues(r.cfg.ChainID).Inc()
	}
	r.log.Error("reconciliation discrepancy",
		logging.FieldErrorCode, "RECONCILIATION_DISCREPANCY",
		"report_id", rep.ReportID,
		"kind", string(rep.Kind),
		logging.FieldAddress, e.Address,
		"expected_base_units", e.ExpectedBaseUnits.String(),
		"observed_base_units", e.ObservedBaseUnits.String(),
		"difference", e.Difference.String(),
		"earliest_suspected_height", e.EarliestSuspectedHeight,
		"related_tx_hashes", e.RelatedTxHashes,
		"recommended_rescan_height", e.RecommendedRescanHeight,
	)
}

// signedAmount folds direction into a signed balance effect.
func signedAmount(d storage.LedgerDirection, amt sdkmath.Int) sdkmath.Int {
	if d == storage.DirectionOut {
		return amt.Neg()
	}
	return amt
}

// newID returns a 16-byte random hex identifier.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("r%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
