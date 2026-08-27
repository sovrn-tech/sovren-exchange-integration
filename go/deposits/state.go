package deposits

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	sdkmath "cosmossdk.io/math"

	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

// OutboxTopicDepositCredited is the transactional-outbox topic emitted
// exactly once per credited deposit (data model §3b invariant).
const OutboxTopicDepositCredited = "deposit.credited"

// RecordPolicy configures the block write path.
type RecordPolicy struct {
	ChainID string
	// MinimumDepositUsovr parks smaller validated deposits as BELOW_MINIMUM;
	// nil or zero disables parking (threshold is exchange configuration,
	// never a code constant — FR-040).
	MinimumDepositUsovr sdkmath.Int
}

func (p RecordPolicy) minimumSet() bool {
	return !p.MinimumDepositUsovr.IsNil() && p.MinimumDepositUsovr.IsPositive()
}

// RecordOutcome reports what one block's transactional write did.
type RecordOutcome struct {
	LedgerAppends     int
	FeeOutflows       int
	DepositsInserted  int
	DepositsRevived   int
	Duplicates        int
	ReviewItemsOpened int
}

// RecordBlock persists one parsed block atomically: ledger appends, fee
// outflows, deposit derivation (exclusively from EXTERNAL_DEPOSIT entries),
// review-queue items, and the checkpoint advance — all in one Store.WithTx
// transaction (FR-026). Replays are idempotent: every unique-key hit is
// tolerated and counted, never re-credited (FR-024).
func RecordBlock(ctx context.Context, store storage.Store, bp *BlockParse, pol RecordPolicy, now time.Time) (RecordOutcome, error) {
	var out RecordOutcome
	if pol.ChainID == "" {
		return out, fmt.Errorf("deposits: RecordPolicy.ChainID must be set")
	}
	err := store.WithTx(ctx, func(ctx context.Context, s storage.Store) error {
		for _, c := range bp.Transfers {
			entry, err := s.Ledger().Append(ctx, c.LedgerEntry(pol.ChainID, bp.Height, now))
			fresh := err == nil
			if err != nil && !errors.Is(err, storage.ErrDuplicate) {
				return fmt.Errorf("ledger append %s/%d/%d: %w", c.TxHash, c.MessageIndex, c.OpIndex, err)
			}
			if fresh {
				out.LedgerAppends++
			}
			// Mixed-input rows are review-only ledger records; deposit
			// records derive only from EXTERNAL_DEPOSIT (data model §3b).
			if c.Classification == storage.ClassUnattributedReview && fresh {
				if err := openLedgerReview(ctx, s, pol.ChainID, entry.ID, c.ReviewReason, now); err != nil {
					return err
				}
				out.ReviewItemsOpened++
			}
			if c.Direction == storage.DirectionIn && c.Classification == storage.ClassExternalDeposit {
				n, err := recordDeposit(ctx, s, bp, c, pol, now)
				if err != nil {
					return err
				}
				out.DepositsInserted += n.inserted
				out.DepositsRevived += n.revived
				out.Duplicates += n.duplicates
				out.ReviewItemsOpened += n.reviews
			}
		}

		for _, f := range bp.FeeDeductions {
			_, err := s.Ledger().AppendFeeOutflow(ctx, storage.FeeOutflow{
				ChainID:      pol.ChainID,
				TxHash:       f.TxHash,
				PayerAddress: f.PayerAddress,
				FeeBaseUnits: f.FeeBaseUnits,
				TxCode:       f.TxCode,
				BlockHeight:  bp.Height,
				CreatedAt:    now,
			})
			if err != nil {
				if errors.Is(err, storage.ErrDuplicate) {
					continue
				}
				return fmt.Errorf("fee outflow %s: %w", f.TxHash, err)
			}
			out.FeeOutflows++
		}

		for _, rc := range bp.ReviewCandidates {
			entry, err := s.Ledger().Append(ctx, rc.LedgerEntry(pol.ChainID, bp.Height, now))
			if err != nil {
				if errors.Is(err, storage.ErrDuplicate) {
					continue
				}
				return fmt.Errorf("review ledger append %s/%d: %w", rc.TxHash, rc.OpIndex, err)
			}
			out.LedgerAppends++
			if err := openLedgerReview(ctx, s, pol.ChainID, entry.ID, rc.Reason, now); err != nil {
				return err
			}
			out.ReviewItemsOpened++
		}

		for _, be := range bp.BlockEvents {
			entry, err := s.Ledger().Append(ctx, be.LedgerEntry(pol.ChainID, bp.Height, now))
			if err != nil {
				if errors.Is(err, storage.ErrDuplicate) {
					continue
				}
				return fmt.Errorf("block event append %d/%d: %w", bp.Height, be.EventIndex, err)
			}
			out.LedgerAppends++
			if err := openLedgerReview(ctx, s, pol.ChainID, entry.ID, be.Reason, now); err != nil {
				return err
			}
			out.ReviewItemsOpened++
		}

		// Checkpoint advance in the same transaction as the block's record
		// writes (FR-026).
		return s.Checkpoints().Set(ctx, storage.ScannerCheckpoint{
			ChainID:                  pol.ChainID,
			LastFullyProcessedHeight: bp.Height,
			LastObservedBlockHash:    bp.BlockHash,
			UpdatedAt:                now,
		})
	})
	return out, err
}

type depositOutcome struct {
	inserted, revived, duplicates, reviews int
}

// recordDeposit inserts and routes one deposit record. Insert conflicts are
// the DUPLICATE observation (counted, never re-credited); an existing
// ORPHANED record is re-evaluated (ORPHANED → DISCOVERED, R6).
func recordDeposit(ctx context.Context, s storage.Store, bp *BlockParse, c TransferCandidate, pol RecordPolicy, now time.Time) (depositOutcome, error) {
	var out depositOutcome
	d := storage.DepositRecord{
		ChainID:          pol.ChainID,
		TxHash:           c.TxHash,
		MessageIndex:     c.MessageIndex,
		CoinIndex:        c.CoinIndex,
		BlockHeight:      bp.Height,
		BlockTimestamp:   bp.Time,
		SenderAddress:    c.SenderAddress,
		RecipientAddress: c.Address,
		Denom:            c.Denom,
		AmountBaseUnits:  c.AmountBaseUnits,
		Memo:             c.Memo,
		TxCode:           c.TxCode,
		TxLog:            c.TxLog,
		Status:           storage.DepositDiscovered,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	ins, err := s.Deposits().Insert(ctx, d)
	switch {
	case err == nil:
		out.inserted = 1
	case errors.Is(err, storage.ErrDuplicate):
		existing, gerr := s.Deposits().Get(ctx, d.ChainID, d.TxHash, d.MessageIndex, d.CoinIndex, d.RecipientAddress)
		if gerr != nil {
			return out, fmt.Errorf("deposit conflict lookup: %w", gerr)
		}
		if existing.Status != storage.DepositOrphaned {
			out.duplicates = 1
			return out, nil
		}
		if uerr := s.Deposits().UpdateStatus(ctx, existing.ID, storage.DepositOrphaned, storage.DepositDiscovered, storage.DepositUpdate{}); uerr != nil {
			return out, uerr
		}
		ins = existing
		ins.Status = storage.DepositDiscovered
		out.revived = 1
	default:
		return out, fmt.Errorf("deposit insert %s/%d/%d: %w", d.TxHash, d.MessageIndex, d.CoinIndex, err)
	}

	move := func(from, to storage.DepositStatus, set storage.DepositUpdate) error {
		return s.Deposits().UpdateStatus(ctx, ins.ID, from, to, set)
	}
	switch {
	case c.TxCode != 0:
		// Failed execution is never credited (FR-029) — terminal REJECTED.
		return out, move(storage.DepositDiscovered, storage.DepositRejected, storage.DepositUpdate{})
	case c.ReviewReason != "":
		if err := move(storage.DepositDiscovered, storage.DepositReviewRequired, storage.DepositUpdate{}); err != nil {
			return out, err
		}
		if _, err := s.Review().Open(ctx, storage.ReviewItem{
			ChainID:  pol.ChainID,
			Kind:     storage.ReviewKindDeposit,
			RefID:    strconv.FormatInt(ins.ID, 10),
			Reason:   c.ReviewReason,
			OpenedAt: now,
		}); err != nil {
			return out, err
		}
		out.reviews = 1
		return out, nil
	default:
		if err := move(storage.DepositDiscovered, storage.DepositValidated, storage.DepositUpdate{}); err != nil {
			return out, err
		}
		if pol.minimumSet() && c.AmountBaseUnits.LT(pol.MinimumDepositUsovr) {
			return out, move(storage.DepositValidated, storage.DepositBelowMinimum, storage.DepositUpdate{})
		}
		return out, move(storage.DepositValidated, storage.DepositAwaitingConfirmations, storage.DepositUpdate{})
	}
}

func openLedgerReview(ctx context.Context, s storage.Store, chainID string, ledgerID int64, reason string, now time.Time) error {
	if reason == "" {
		reason = "unattributed ledger activity"
	}
	_, err := s.Review().Open(ctx, storage.ReviewItem{
		ChainID:  chainID,
		Kind:     storage.ReviewKindLedgerEntry,
		RefID:    strconv.FormatInt(ledgerID, 10),
		Reason:   reason,
		OpenedAt: now,
	})
	return err
}

// ---------------------------------------------------------------------------
// FR-023 credit-condition evaluation
// ---------------------------------------------------------------------------

// CreditGate carries the runtime suspension state consulted on every credit
// decision (FR-023 tail conditions).
type CreditGate struct {
	CreditPaused      bool // FR-051 deposit-crediting pause
	ScanWithoutCredit bool // FR-051 scan-without-credit — parks as SUSPENDED
	ChainReviewOpen   bool // FR-044 open condition closes the crediting gate
}

// LoadCreditGate reads the gate from controls + open chain-review conditions.
func LoadCreditGate(ctx context.Context, store storage.Store, chainID string) (CreditGate, error) {
	controls, err := store.Controls().Get(ctx, chainID)
	if err != nil {
		return CreditGate{}, err
	}
	open, err := store.ChainReview().HasOpen(ctx, chainID)
	if err != nil {
		return CreditGate{}, err
	}
	return CreditGate{
		CreditPaused:      controls.CreditPaused,
		ScanWithoutCredit: controls.ScanWithoutCredit,
		ChainReviewOpen:   open,
	}, nil
}

// CreditDecision is the outcome of evaluating the FR-023 condition list.
type CreditDecision string

const (
	// DecisionCredit — every FR-023 condition holds; credit now.
	DecisionCredit CreditDecision = "CREDIT"
	// DecisionHold — a condition does not hold yet (or a pause/chain-review
	// gate is closed); re-evaluate later, no state change.
	DecisionHold CreditDecision = "HOLD"
	// DecisionSuspend — scan-without-credit is engaged; park as SUSPENDED.
	DecisionSuspend CreditDecision = "SUSPEND"
	// DecisionNever — a permanent condition fails (failed tx, wrong denom,
	// non-positive amount, already credited); the deposit must not credit.
	DecisionNever CreditDecision = "NEVER"
)

// ConfirmationCount is latest − block_height + 1 — recomputed, never stored
// authority (data model §3b).
func ConfirmationCount(latestHeight, blockHeight uint64) uint64 {
	if latestHeight < blockHeight {
		return 0
	}
	return latestHeight - blockHeight + 1
}

// EvaluateCreditConditions applies the full FR-023 list to one deposit.
// Structural conditions (committed block, recognized transfer shape,
// exchange-controlled recipient, external classification) are established
// at derivation time — a DepositRecord only exists when they held; this
// function re-checks the record-level and runtime conditions.
func EvaluateCreditConditions(d storage.DepositRecord, latestHeight, confirmations uint64, gate CreditGate) (CreditDecision, string) {
	if d.TxCode != 0 {
		return DecisionNever, "execution result indicates failure (FR-029)"
	}
	if d.Denom != storage.BaseDenom {
		return DecisionNever, "denomination is not the base denomination"
	}
	if d.AmountBaseUnits.IsNil() || !d.AmountBaseUnits.IsPositive() {
		return DecisionNever, "amount is not positive"
	}
	if d.Status == storage.DepositCredited || d.Status == storage.DepositSweepPending || d.Status == storage.DepositSwept || d.CreditedAt != nil {
		return DecisionNever, "deposit already credited"
	}
	if d.Status != storage.DepositCreditable {
		return DecisionHold, fmt.Sprintf("status %s is not CREDITABLE", d.Status)
	}
	if ConfirmationCount(latestHeight, d.BlockHeight) < confirmations {
		return DecisionHold, "confirmation threshold not reached (FR-028)"
	}
	if gate.ScanWithoutCredit {
		return DecisionSuspend, "scan-without-credit engaged (FR-051)"
	}
	if gate.CreditPaused {
		return DecisionHold, "deposit crediting paused (FR-051)"
	}
	if gate.ChainReviewOpen {
		return DecisionHold, "chain-review condition open (FR-044)"
	}
	return DecisionCredit, ""
}

// DepositDedupKey is the outbox dedup key making the credited event
// exactly-once per FR-024 unique deposit key.
func DepositDedupKey(d storage.DepositRecord) string {
	return fmt.Sprintf("%s:%s:%s:%d:%d:%s",
		OutboxTopicDepositCredited, d.ChainID, d.TxHash, d.MessageIndex, d.CoinIndex, d.RecipientAddress)
}

// creditedEvent is the outbox payload for a credited deposit.
type creditedEvent struct {
	ChainID          string `json:"chain_id"`
	TxHash           string `json:"tx_hash"`
	MessageIndex     uint32 `json:"message_index"`
	CoinIndex        uint32 `json:"coin_index"`
	RecipientAddress string `json:"recipient_address"`
	SenderAddress    string `json:"sender_address,omitempty"`
	Denom            string `json:"denom"`
	AmountBaseUnits  string `json:"amount_base_units"`
	Memo             string `json:"memo,omitempty"`
	BlockHeight      string `json:"block_height"`
	CreditedAt       string `json:"credited_at"`
}

// ErrCreditGateClosed aborts an in-flight credit: the gate re-check inside the
// crediting transaction found crediting paused, scan-without-credit engaged, or
// a chain-review condition open. The CREDITABLE→CREDITED flip and outbox
// enqueue never happen, so the record stays CREDITABLE for a later pass. It is a
// benign TOCTOU guard (a batch's gate load precedes each credit tx), never a
// failure — callers must stop the batch on it, not surface it as an error.
var ErrCreditGateClosed = errors.New("deposits: credit gate closed inside credit transaction")

// CreditDeposit flips CREDITABLE → CREDITED and enqueues the credited event
// in one transaction (transactional outbox — the §3b exactly-once
// invariant). Callers must have evaluated DecisionCredit first. The credit gate
// is re-validated transactionally with the flip: a pause / chain-review opened
// after the caller's gate load must not let a batch keep crediting
// (ErrCreditGateClosed aborts, leaving the record CREDITABLE).
func CreditDeposit(ctx context.Context, store storage.Store, d storage.DepositRecord, now time.Time) error {
	ev := creditedEvent{
		ChainID:          d.ChainID,
		TxHash:           d.TxHash,
		MessageIndex:     d.MessageIndex,
		CoinIndex:        d.CoinIndex,
		RecipientAddress: d.RecipientAddress,
		Denom:            d.Denom,
		AmountBaseUnits:  d.AmountBaseUnits.String(),
		Memo:             d.Memo,
		BlockHeight:      strconv.FormatUint(d.BlockHeight, 10),
		CreditedAt:       now.UTC().Format(time.RFC3339),
	}
	if d.SenderAddress != nil {
		ev.SenderAddress = *d.SenderAddress
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	return store.WithTx(ctx, func(ctx context.Context, s storage.Store) error {
		locker, ok := s.(interface {
			AcquireCreditGateLock(context.Context, string) error
		})
		if !ok {
			return errors.New("deposits: storage backend does not implement credit-gate serialization")
		}
		if err := locker.AcquireCreditGateLock(ctx, d.ChainID); err != nil {
			return err
		}
		// Re-read the gate on the tx-scoped store, atomically with the flip
		// below: a CreditPaused / ChainReviewOpen / ScanWithoutCredit change
		// committed after the caller's batch-level gate load must abort here so
		// no more than the pre-flip records credit (fund-safety TOCTOU).
		gate, err := LoadCreditGate(ctx, s, d.ChainID)
		if err != nil {
			return err
		}
		if gate.CreditPaused || gate.ScanWithoutCredit || gate.ChainReviewOpen {
			return ErrCreditGateClosed
		}
		if err := s.Deposits().UpdateStatus(ctx, d.ID, storage.DepositCreditable, storage.DepositCredited,
			storage.DepositUpdate{CreditedAt: &now}); err != nil {
			return err
		}
		if _, err := s.Outbox().Enqueue(ctx, storage.OutboxEvent{
			ChainID:   d.ChainID,
			Topic:     OutboxTopicDepositCredited,
			DedupKey:  DepositDedupKey(d),
			Payload:   payload,
			CreatedAt: now,
		}); err != nil {
			return err
		}
		return nil
	})
}
