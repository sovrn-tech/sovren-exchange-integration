package deposits

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
	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/logging"
	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/metrics"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

// DefaultConfirmations is the published recommended launch value (FR-028):
// CometBFT is single-block-final (1 committed block = protocol finality), so
// the recommended launch default of 2 is a node-operations sanity buffer for
// node comparison, monitoring, and incident detection — not a reorg defence.
// The supported range is 1..12 and the final threshold is an exchange risk
// decision.
const DefaultConfirmations = 2

// DefaultPollInterval is the block poll cadence. The kit's client transports
// are unary-only, so a short poll interval is the accelerator-equivalent of
// the optional WebSocket NewBlock subscription (documented deviation — FR-027
// polling remains the primary mechanism either way).
const DefaultPollInterval = 2 * time.Second

const defaultPendingBatchLimit = 500

// ScannerConfig configures one Scanner (contract: go-client-api.md
// §deposits). Every economic value comes from configuration (FR-040).
type ScannerConfig struct {
	ChainID string
	// Confirmations defaults to DefaultConfirmations when zero.
	Confirmations uint64
	// PollInterval defaults to DefaultPollInterval when zero.
	PollInterval time.Duration
	// StartHeight, when non-zero, is the explicit rescan base applied on the
	// first cycle; zero resumes from the durable checkpoint (FR-026).
	StartHeight uint64
	// MinimumDepositUsovr parks smaller deposits BELOW_MINIMUM; nil/zero
	// disables parking.
	MinimumDepositUsovr sdkmath.Int
	// DisableHashChainVerification turns off the R6 last_block_id.hash chain
	// check (on by default; disable only in tests).
	DisableHashChainVerification bool
	// MemoRecognizer recognizes omnibus memos (FR-016); nil ⇒ any non-empty
	// memo is recognized.
	MemoRecognizer func(memo string) bool
	// PendingBatchLimit bounds each per-status pending sweep; default 500.
	PendingBatchLimit int

	Logger  *slog.Logger
	Metrics *metrics.Set
	// Now is the clock; nil ⇒ time.Now (tests inject).
	Now func() time.Time
}

// Scanner is the checkpointed ascending block walker (FR-026): it advances
// the durable checkpoint only after a block is fully persisted (same
// transaction), verifies the block-hash chain (R6), and evaluates the FR-023
// crediting conditions for pending deposits each cycle.
type Scanner struct {
	client client.Client
	store  storage.Store
	cfg    ScannerConfig
	log    *slog.Logger

	// forcedBase is the pending explicit rescan base (StartHeight or a
	// controls resume_from_height), consumed by the next cycle.
	forcedBase uint64
}

// NewScanner builds a Scanner; cfg zero-values take documented defaults.
func NewScanner(c client.Client, store storage.Store, cfg ScannerConfig) (*Scanner, error) {
	if c == nil || store == nil {
		return nil, fmt.Errorf("deposits: scanner requires a client and a store")
	}
	if cfg.ChainID == "" {
		return nil, fmt.Errorf("deposits: ScannerConfig.ChainID must be set")
	}
	if cfg.Confirmations == 0 {
		cfg.Confirmations = DefaultConfirmations
	}
	if cfg.Confirmations > 12 {
		return nil, fmt.Errorf("deposits: ScannerConfig.Confirmations must be between 1 and 12")
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultPollInterval
	}
	if cfg.PendingBatchLimit <= 0 {
		cfg.PendingBatchLimit = defaultPendingBatchLimit
	}
	if cfg.Logger == nil {
		cfg.Logger = logging.New("deposits.scanner")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Scanner{
		client:     c,
		store:      store,
		cfg:        cfg,
		log:        cfg.Logger.With(logging.FieldChainID, cfg.ChainID),
		forcedBase: cfg.StartHeight,
	}, nil
}

// Run polls until ctx is done. Cycle errors (node outage, storage outage)
// are logged and retried on the next tick — restart-safety comes from the
// durable checkpoint, not from in-memory state.
func (s *Scanner) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()
	for {
		cctx, log := logging.WithCorrelation(ctx, s.log, newCorrelationID())
		if err := s.Cycle(cctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Error("scan cycle failed", logging.FieldErrorCode, "SCAN_CYCLE_FAILED", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// RescanFrom records an explicit rescan base in the operational controls
// (admin POST /v1/scanner/resume-from writes the same field); the scanner
// picks it up on its next cycle. Replay is idempotent — unique keys make
// re-processing safe (FR-024).
func (s *Scanner) RescanFrom(ctx context.Context, height uint64, actor, reason string) error {
	h := height
	_, err := s.store.Controls().Apply(ctx, s.cfg.ChainID,
		storage.ControlsUpdate{ResumeFromHeight: &h}, actor, reason)
	return err
}

// Cycle performs one scan pass: walk new blocks, then evaluate pending
// deposits. Exported for tests and drills; Run calls it on every tick.
func (s *Scanner) Cycle(ctx context.Context) error {
	latest, err := s.client.LatestBlock(ctx)
	if err != nil {
		return fmt.Errorf("latest block: %w", err)
	}
	if latest.ChainID != "" && latest.ChainID != s.cfg.ChainID {
		return s.wrongChain(ctx, latest)
	}
	latestHeight := uint64(latest.Height)

	next, expectedHash, err := s.resolveCursor(ctx, latestHeight)
	if err != nil {
		return err
	}

	for h := next; h <= latestHeight; h++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		block, err := s.client.BlockByHeight(ctx, int64(h))
		if err != nil {
			return fmt.Errorf("block %d: %w", h, err)
		}
		results, err := s.client.BlockResults(ctx, int64(h))
		if err != nil {
			return fmt.Errorf("block results %d: %w", h, err)
		}
		watch, err := s.loadWatch(ctx)
		if err != nil {
			return err
		}
		bp, err := ParseBlockTransfers(block, results, watch)
		if err != nil {
			return err
		}
		if !s.cfg.DisableHashChainVerification && expectedHash != "" && bp.LastBlockHash != expectedHash {
			return s.orphanFlow(ctx, h, expectedHash, bp.LastBlockHash)
		}
		outcome, err := RecordBlock(ctx, s.store, bp, s.recordPolicy(), s.cfg.Now().UTC())
		if err != nil {
			return fmt.Errorf("record block %d: %w", h, err)
		}
		expectedHash = bp.BlockHash
		s.observeBlock(h, latestHeight, outcome)
	}

	return s.processPending(ctx, latestHeight)
}

func (s *Scanner) recordPolicy() RecordPolicy {
	return RecordPolicy{ChainID: s.cfg.ChainID, MinimumDepositUsovr: s.cfg.MinimumDepositUsovr}
}

func (s *Scanner) loadWatch(ctx context.Context) (WatchSet, error) {
	addrs, err := s.store.Watch().ListActive(ctx, s.cfg.ChainID)
	if err != nil {
		return WatchSet{}, fmt.Errorf("load watch set: %w", err)
	}
	return NewWatchSet(addrs).WithMemoRecognizer(s.cfg.MemoRecognizer), nil
}

// resolveCursor determines the next height to scan and the expected parent
// hash: an explicit rescan base (StartHeight / controls resume_from_height)
// wins; otherwise the durable checkpoint; otherwise the chain tip.
func (s *Scanner) resolveCursor(ctx context.Context, latestHeight uint64) (next uint64, expectedHash string, err error) {
	controls, err := s.store.Controls().Get(ctx, s.cfg.ChainID)
	if err != nil {
		return 0, "", fmt.Errorf("load controls: %w", err)
	}
	base := s.forcedBase
	if controls.ResumeFromHeight != nil {
		base = *controls.ResumeFromHeight
		if _, err := s.store.Controls().Apply(ctx, s.cfg.ChainID,
			storage.ControlsUpdate{ClearResumeFromHeight: true},
			"scanner", fmt.Sprintf("rescan base %d applied", base)); err != nil {
			return 0, "", fmt.Errorf("clear resume_from_height: %w", err)
		}
	}
	if base > 0 {
		s.forcedBase = 0
		hash, err := s.parentHash(ctx, base)
		if err != nil {
			return 0, "", err
		}
		return base, hash, nil
	}

	cp, err := s.store.Checkpoints().Get(ctx, s.cfg.ChainID)
	switch {
	case err == nil:
		return cp.LastFullyProcessedHeight + 1, cp.LastObservedBlockHash, nil
	case errors.Is(err, storage.ErrNotFound):
		// First run without an explicit base: start at the tip; history is
		// an explicit rescan decision, never an implicit backfill.
		return latestHeight, "", nil
	default:
		return 0, "", fmt.Errorf("load checkpoint: %w", err)
	}
}

func (s *Scanner) parentHash(ctx context.Context, base uint64) (string, error) {
	if base <= 1 {
		return "", nil
	}
	parent, err := s.client.BlockByHeight(ctx, int64(base-1))
	if err != nil {
		return "", fmt.Errorf("rescan parent block %d: %w", base-1, err)
	}
	return hexUpper(parent.Hash), nil
}

// orphanFlow handles a broken block-hash chain (R6): open a chain-review
// condition (closing the FR-023 crediting gate), mark deposits from the
// replaced block ORPHANED, and roll the checkpoint back one block so the
// next cycle re-scans it (repeated mismatches walk further back).
func (s *Scanner) orphanFlow(ctx context.Context, height uint64, expected, got string) error {
	s.log.Warn("block hash chain broken",
		logging.FieldHeight, height, "expected_parent_hash", expected, "got_parent_hash", got)
	if err := s.openChainReviewOnce(ctx, storage.TriggerBlockHashMismatch,
		storage.NodeObservation{Endpoint: "scanner-checkpoint", Height: height - 1, Value: expected},
		storage.NodeObservation{Endpoint: "node", Height: height - 1, Value: got},
	); err != nil {
		return err
	}

	replacedHeight := height - 1
	pending := []storage.DepositStatus{
		storage.DepositDiscovered, storage.DepositValidated,
		storage.DepositAwaitingConfirmations, storage.DepositCreditable,
		storage.DepositBelowMinimum,
	}
	for _, st := range pending {
		ds, err := s.store.Deposits().ListByStatus(ctx, s.cfg.ChainID, st, 0)
		if err != nil {
			return err
		}
		for _, d := range ds {
			if d.BlockHeight < replacedHeight {
				continue
			}
			if err := s.store.Deposits().UpdateStatus(ctx, d.ID, st, storage.DepositOrphaned, storage.DepositUpdate{}); err != nil {
				return err
			}
		}
	}

	newHeight := uint64(0)
	newHash := ""
	if replacedHeight > 1 {
		newHeight = replacedHeight - 1
		parent, err := s.client.BlockByHeight(ctx, int64(newHeight))
		if err != nil {
			return fmt.Errorf("rollback parent block %d: %w", newHeight, err)
		}
		newHash = hexUpper(parent.Hash)
	}
	return s.store.Checkpoints().Set(ctx, storage.ScannerCheckpoint{
		ChainID:                  s.cfg.ChainID,
		LastFullyProcessedHeight: newHeight,
		LastObservedBlockHash:    newHash,
		UpdatedAt:                s.cfg.Now().UTC(),
	})
}

func (s *Scanner) wrongChain(ctx context.Context, latest *client.Block) error {
	if err := s.openChainReviewOnce(ctx, storage.TriggerWrongChainID,
		storage.NodeObservation{Endpoint: "config", Value: s.cfg.ChainID},
		storage.NodeObservation{Endpoint: "node", Height: uint64(latest.Height), Value: latest.ChainID},
	); err != nil {
		return err
	}
	return fmt.Errorf("node reports chain_id %q, configured %q", latest.ChainID, s.cfg.ChainID)
}

// openChainReviewOnce opens an FR-044 condition unless one with the same
// trigger is already open (no per-cycle spam).
func (s *Scanner) openChainReviewOnce(ctx context.Context, trigger storage.ChainReviewTrigger, a, b storage.NodeObservation) error {
	open, err := s.store.ChainReview().ListOpen(ctx, s.cfg.ChainID)
	if err != nil {
		return err
	}
	for _, c := range open {
		if c.Trigger == trigger {
			return nil
		}
	}
	_, err = s.store.ChainReview().Open(ctx, storage.ChainReviewCondition{
		ConditionID: newCorrelationID(),
		ChainID:     s.cfg.ChainID,
		Trigger:     trigger,
		NodeA:       a,
		NodeB:       b,
		OpenedAt:    s.cfg.Now().UTC(),
	})
	return err
}

// processPending advances the deposit state machine for pending records:
// SUSPENDED resume, confirmation counting, BELOW_MINIMUM revival, and the
// FR-023 credit evaluation.
func (s *Scanner) processPending(ctx context.Context, latestHeight uint64) error {
	gate, err := LoadCreditGate(ctx, s.store, s.cfg.ChainID)
	if err != nil {
		return err
	}
	deposits := s.store.Deposits()
	limit := s.cfg.PendingBatchLimit

	if !gate.ScanWithoutCredit && !gate.CreditPaused {
		suspended, err := deposits.ListByStatus(ctx, s.cfg.ChainID, storage.DepositSuspended, limit)
		if err != nil {
			return err
		}
		for _, d := range suspended {
			if d.PriorStatus == nil {
				continue
			}
			if err := deposits.UpdateStatus(ctx, d.ID, storage.DepositSuspended, *d.PriorStatus, storage.DepositUpdate{}); err != nil {
				return err
			}
		}
	}

	parked, err := deposits.ListByStatus(ctx, s.cfg.ChainID, storage.DepositBelowMinimum, limit)
	if err != nil {
		return err
	}
	pol := s.recordPolicy()
	for _, d := range parked {
		if pol.minimumSet() && d.AmountBaseUnits.LT(pol.MinimumDepositUsovr) {
			continue
		}
		if err := deposits.UpdateStatus(ctx, d.ID, storage.DepositBelowMinimum, storage.DepositAwaitingConfirmations, storage.DepositUpdate{}); err != nil {
			return err
		}
	}

	awaiting, err := deposits.ListByStatus(ctx, s.cfg.ChainID, storage.DepositAwaitingConfirmations, limit)
	if err != nil {
		return err
	}
	for _, d := range awaiting {
		if ConfirmationCount(latestHeight, d.BlockHeight) < s.cfg.Confirmations {
			continue
		}
		if err := deposits.UpdateStatus(ctx, d.ID, storage.DepositAwaitingConfirmations, storage.DepositCreditable, storage.DepositUpdate{}); err != nil {
			return err
		}
	}

	creditable, err := deposits.ListByStatus(ctx, s.cfg.ChainID, storage.DepositCreditable, limit)
	if err != nil {
		return err
	}
	for _, d := range creditable {
		decision, reason := EvaluateCreditConditions(d, latestHeight, s.cfg.Confirmations, gate)
		switch decision {
		case DecisionCredit:
			if err := CreditDeposit(ctx, s.store, d, s.cfg.Now().UTC()); err != nil {
				// The gate closed between this cycle's gate load and the credit
				// tx (admin pause / chain-review opened mid-batch): stop the
				// credit pass — the gate is now closed for every remaining
				// record. The record stays CREDITABLE for a later cycle.
				if errors.Is(err, ErrCreditGateClosed) {
					s.log.Info("credit gate closed mid-batch; halting credit pass",
						logging.FieldDepositID, d.ID)
					return nil
				}
				return err
			}
			if s.cfg.Metrics != nil {
				s.cfg.Metrics.DepositsCredited.WithLabelValues(s.cfg.ChainID).Inc()
			}
		case DecisionSuspend:
			if err := deposits.UpdateStatus(ctx, d.ID, storage.DepositCreditable, storage.DepositSuspended, storage.DepositUpdate{}); err != nil {
				return err
			}
		case DecisionHold:
			s.log.Debug("credit held", logging.FieldDepositID, d.ID, "reason", reason)
		case DecisionNever:
			s.log.Error("creditable deposit failed permanent FR-023 condition",
				logging.FieldDepositID, d.ID, "reason", reason)
		}
	}
	return nil
}

func (s *Scanner) observeBlock(height, latestHeight uint64, outcome RecordOutcome) {
	if outcome.DepositsInserted > 0 || outcome.Duplicates > 0 {
		s.log.Info("block recorded", logging.FieldHeight, height,
			"deposits", outcome.DepositsInserted, "duplicates", outcome.Duplicates,
			"ledger_appends", outcome.LedgerAppends, "reviews", outcome.ReviewItemsOpened)
	}
	m := s.cfg.Metrics
	if m == nil {
		return
	}
	m.ScannerBlocksProcessed.WithLabelValues(s.cfg.ChainID).Inc()
	m.ScannerLastProcessedHeight.WithLabelValues(s.cfg.ChainID).Set(float64(height))
	m.ScannerLatestChainHeight.WithLabelValues(s.cfg.ChainID).Set(float64(latestHeight))
	m.ScannerHeightLag.WithLabelValues(s.cfg.ChainID).Set(float64(latestHeight - height))
	if outcome.DepositsInserted > 0 {
		m.DepositsDiscovered.WithLabelValues(s.cfg.ChainID).Add(float64(outcome.DepositsInserted))
	}
	if outcome.Duplicates > 0 {
		m.DuplicateDeposits.WithLabelValues(s.cfg.ChainID).Add(float64(outcome.Duplicates))
	}
}

func hexUpper(b []byte) string {
	return fmt.Sprintf("%X", b)
}

// newCorrelationID returns a 16-byte random hex correlation ID.
func newCorrelationID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
