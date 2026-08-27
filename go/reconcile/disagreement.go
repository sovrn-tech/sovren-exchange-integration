package reconcile

// Node-disagreement monitor (T065, FR-044): a periodic two-node Compare loop
// that opens ChainReviewCondition records on mismatch. An open condition
// closes the FR-023 crediting gate automatically — deposits.LoadCreditGate /
// EvaluateCreditConditions consult ChainReviewRepo.HasOpen on every credit
// decision — and raises the NodesDisagree alert through the
// sovren_chain_review_conditions_open gauge.

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/logging"
	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/metrics"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

// Comparer runs the FR-044 two-node checks. *client.Failover satisfies it.
type Comparer interface {
	Compare(ctx context.Context, req client.CompareRequest) (*client.CompareResult, error)
}

// DefaultDisagreementInterval matches the adapter.yaml
// nodes.disagreement.check_interval default.
const DefaultDisagreementInterval = 30 * time.Second

// DefaultHeightTolerance matches the adapter.yaml
// nodes.disagreement.height_divergence_threshold default.
const DefaultHeightTolerance = 5

// autoResolveCleanChecks is the number of consecutive fully-matching checks
// after which the monitor auto-resolves conditions it opened for *transient*
// triggers (height divergence, query mismatch). BLOCK_HASH_MISMATCH and
// WRONG_CHAIN_ID never auto-resolve — those demand operator judgment.
const autoResolveCleanChecks = 3

// DisagreementConfig configures a Monitor.
type DisagreementConfig struct {
	ChainID string
	// Interval between checks; DefaultDisagreementInterval when zero.
	Interval time.Duration
	// HeightTolerance is the allowed latest-height gap (blocks);
	// DefaultHeightTolerance when zero.
	HeightTolerance int64
	// SequenceAddress optionally adds an account-sequence check (hot wallet).
	SequenceAddress string
	// BalanceAddress optionally adds a balance check (hot wallet).
	BalanceAddress string
	// CompareHashAtCheckpoint adds a block-hash check at the scanner
	// checkpoint height (both nodes must agree on scanned history).
	CompareHashAtCheckpoint bool
}

// Monitor is the periodic disagreement checker.
type Monitor struct {
	cmp     Comparer
	store   storage.Store
	cfg     DisagreementConfig
	log     *slog.Logger
	metrics *metrics.Set
	now     func() time.Time
	newID   func() string

	cleanStreak int
}

// MonitorOption configures a Monitor.
type MonitorOption func(*Monitor)

// WithMonitorLogger replaces the default logger.
func WithMonitorLogger(l *slog.Logger) MonitorOption { return func(m *Monitor) { m.log = l } }

// WithMonitorMetrics attaches the adapter metric set.
func WithMonitorMetrics(s *metrics.Set) MonitorOption { return func(m *Monitor) { m.metrics = s } }

// WithMonitorNow injects the clock (tests).
func WithMonitorNow(now func() time.Time) MonitorOption { return func(m *Monitor) { m.now = now } }

// NewMonitor builds a Monitor.
func NewMonitor(cmp Comparer, store storage.Store, cfg DisagreementConfig, opts ...MonitorOption) (*Monitor, error) {
	if cmp == nil || store == nil {
		return nil, fmt.Errorf("reconcile: disagreement monitor requires a comparer and a store")
	}
	if cfg.ChainID == "" {
		return nil, fmt.Errorf("reconcile: ChainID required")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultDisagreementInterval
	}
	if cfg.HeightTolerance <= 0 {
		cfg.HeightTolerance = DefaultHeightTolerance
	}
	m := &Monitor{
		cmp: cmp, store: store, cfg: cfg,
		log:   logging.New("reconcile.disagreement"),
		now:   time.Now,
		newID: newID,
	}
	for _, o := range opts {
		o(m)
	}
	m.log = m.log.With(logging.FieldChainID, cfg.ChainID)
	return m, nil
}

// Run checks until ctx is done. Check errors are logged and retried on the
// next tick.
func (m *Monitor) Run(ctx context.Context) error {
	ticker := time.NewTicker(m.cfg.Interval)
	defer ticker.Stop()
	for {
		if _, err := m.Check(ctx); err != nil && ctx.Err() == nil {
			m.log.Warn("disagreement check failed", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Check runs one comparison pass: mismatches open ChainReviewConditions
// (deduplicated per trigger), full agreement advances the auto-resolve
// streak, and the open-conditions gauge is refreshed either way.
func (m *Monitor) Check(ctx context.Context) (*client.CompareResult, error) {
	req := client.CompareRequest{
		Height:          true,
		HeightTolerance: m.cfg.HeightTolerance,
		SequenceAddress: m.cfg.SequenceAddress,
		BalanceAddress:  m.cfg.BalanceAddress,
	}
	if req.BalanceAddress != "" {
		req.BalanceDenom = storage.BaseDenom
	}
	if m.cfg.CompareHashAtCheckpoint {
		cp, err := m.store.Checkpoints().Get(ctx, m.cfg.ChainID)
		if err == nil && cp.LastFullyProcessedHeight > 0 {
			req.BlockHashAtHeight = int64(cp.LastFullyProcessedHeight)
		}
	}

	res, err := m.cmp.Compare(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("reconcile: compare: %w", err)
	}

	if res.AllMatch() {
		m.cleanStreak++
		if m.cleanStreak >= autoResolveCleanChecks {
			if err := m.autoResolveTransient(ctx); err != nil {
				return res, err
			}
		}
	} else {
		m.cleanStreak = 0
		for _, it := range res.Mismatches() {
			if err := m.openCondition(ctx, it); err != nil {
				return res, err
			}
		}
	}
	return res, m.refreshGauge(ctx)
}

// triggerFor maps a compare item to its FR-044 trigger.
func triggerFor(kind client.CompareKind) storage.ChainReviewTrigger {
	switch kind {
	case client.CompareHeight:
		return storage.TriggerHeightDivergence
	case client.CompareBlockHash:
		return storage.TriggerBlockHashMismatch
	default:
		return storage.TriggerQueryResultMismatch
	}
}

// openCondition opens one condition unless the same trigger is already open
// (no per-tick spam), and emits the FR-044 alert log.
func (m *Monitor) openCondition(ctx context.Context, it client.CompareItem) error {
	trigger := triggerFor(it.Kind)
	open, err := m.store.ChainReview().ListOpen(ctx, m.cfg.ChainID)
	if err != nil {
		return err
	}
	for _, c := range open {
		if c.Trigger == trigger {
			return nil
		}
	}
	cond := storage.ChainReviewCondition{
		ConditionID: m.newID(),
		ChainID:     m.cfg.ChainID,
		Trigger:     trigger,
		NodeA:       storage.NodeObservation{Endpoint: "primary", Value: firstNonEmpty(it.Primary, it.PrimaryErr)},
		NodeB:       storage.NodeObservation{Endpoint: "secondary", Value: firstNonEmpty(it.Secondary, it.SecondaryErr)},
		OpenedAt:    m.now().UTC(),
	}
	if _, err := m.store.ChainReview().Open(ctx, cond); err != nil {
		return fmt.Errorf("reconcile: open condition: %w", err)
	}
	m.log.Error("node disagreement detected — crediting gate closed",
		logging.FieldErrorCode, "NODES_DISAGREE",
		"trigger", string(trigger),
		"condition_id", cond.ConditionID,
		"check", string(it.Kind),
		"primary", it.Primary, "primary_err", it.PrimaryErr,
		"secondary", it.Secondary, "secondary_err", it.SecondaryErr,
	)
	return nil
}

// autoResolveTransient resolves open HEIGHT_DIVERGENCE /
// QUERY_RESULT_MISMATCH conditions after sustained agreement (a lagging node
// that caught up). Hash and chain-ID conditions stay open for the operator.
func (m *Monitor) autoResolveTransient(ctx context.Context) error {
	open, err := m.store.ChainReview().ListOpen(ctx, m.cfg.ChainID)
	if err != nil {
		return err
	}
	for _, c := range open {
		if c.Trigger != storage.TriggerHeightDivergence && c.Trigger != storage.TriggerQueryResultMismatch {
			continue
		}
		resolution := fmt.Sprintf("auto-resolved: nodes agreed on %d consecutive checks", autoResolveCleanChecks)
		if err := m.store.ChainReview().Resolve(ctx, c.ConditionID, resolution, m.now().UTC()); err != nil {
			return fmt.Errorf("reconcile: resolve condition %s: %w", c.ConditionID, err)
		}
		m.log.Info("chain-review condition auto-resolved",
			"condition_id", c.ConditionID, "trigger", string(c.Trigger))
	}
	return nil
}

// refreshGauge sets sovren_chain_review_conditions_open per trigger,
// zeroing triggers with no open condition so stale series never linger.
func (m *Monitor) refreshGauge(ctx context.Context) error {
	if m.metrics == nil {
		return nil
	}
	open, err := m.store.ChainReview().ListOpen(ctx, m.cfg.ChainID)
	if err != nil {
		return err
	}
	counts := map[storage.ChainReviewTrigger]int{}
	for _, c := range open {
		counts[c.Trigger]++
	}
	for _, t := range storage.AllChainReviewTriggers {
		m.metrics.ChainReviewConditionsOpen.
			WithLabelValues(m.cfg.ChainID, string(t)).
			Set(float64(counts[t]))
	}
	return nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
