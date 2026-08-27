// Package sequences is the kit's durable sequence manager (FR-034, data
// model §6). Manager.Reserve is the ONLY sequence source for transaction
// builders: it serializes per (chain_id, source_address) through the storage
// backend's account-lock primitive (Postgres SELECT … FOR UPDATE on the
// chain_account_locks row; SQLite single-writer + BEGIN IMMEDIATE), derives
// the next sequence from chain truth reconciled with open reservations, and
// binds exactly one reservation to one work item (withdrawal or sweep).
//
// Reconciliation is state-dependent and never blind (§6 reconciliation
// rule): only RESERVED reservations — nothing signed — are ever released; a
// SIGNED/BROADCAST/ambiguous reservation whose transaction cannot be found
// is quarantined as RECONCILIATION_REQUIRED, because valid signed bytes may
// still redeem the sequence from a mempool, another process, or the signer
// system. Recovery rebroadcasts the exact persisted signed bytes
// (RebroadcastPersisted) — never re-signs, never auto-releases.
package sequences

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/sovrn-tech/sovren-exchange-integration/go/client"
	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/logging"
	"github.com/sovrn-tech/sovren-exchange-integration/go/internal/metrics"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

var (
	// ErrReleasedSlotUnusable reports that a work item's reservation was
	// RELEASED during reconciliation and its sequence slot has since been
	// consumed on chain, so the same work_ref cannot be re-bound. The work
	// item must be quarantined for operator review — never silently rebound
	// to a different sequence.
	ErrReleasedSlotUnusable = errors.New("sequences: released reservation slot is no longer reservable")

	// ErrNoAccountLock reports a storage backend that does not expose the
	// per-account serialization primitive (R7).
	ErrNoAccountLock = errors.New("sequences: storage backend does not implement AcquireAccountLock")
)

// Chain is the subset of client.Client the manager needs. *client.Client
// implementations satisfy it.
type Chain interface {
	Account(ctx context.Context, addr string) (accountNumber, sequence uint64, err error)
	Tx(ctx context.Context, hash string) (*client.TxInfo, error)
	Broadcast(ctx context.Context, txBytes []byte, mode client.BroadcastMode) (*client.BroadcastResult, error)
}

// accountLocker is implemented by both shipped storage backends; the manager
// requires it for the read-modify-write reservation path (R7).
type accountLocker interface {
	AcquireAccountLock(ctx context.Context, chainID, sourceAddress string) error
}

// Manager hands out durable sequence reservations. Safe for concurrent use;
// concurrency is serialized per account by the storage backend, and the
// UNIQUE(chain_id, source_address, sequence) constraint is the last-line
// guarantee in all cases.
type Manager struct {
	store   storage.Store
	chain   Chain
	logger  *slog.Logger
	metrics *metrics.Set
}

// Option configures a Manager.
type Option func(*Manager)

// WithLogger replaces the default logger.
func WithLogger(l *slog.Logger) Option { return func(m *Manager) { m.logger = l } }

// WithMetrics attaches the adapter metric set (sequence-mismatch counter).
func WithMetrics(s *metrics.Set) Option { return func(m *Manager) { m.metrics = s } }

// NewManager builds a Manager over one storage backend and one chain client.
func NewManager(store storage.Store, chain Chain, opts ...Option) *Manager {
	m := &Manager{store: store, chain: chain, logger: logging.New("sequences")}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Reserve returns the reservation bound to ref, creating it if absent.
//
// Idempotent per work_ref: a second call for the same work item returns the
// existing reservation in whatever status it has reached (the caller
// inspects Status). A reservation RELEASED by reconciliation is re-bound to
// the same slot when that slot is still ahead of chain truth; a burned slot
// returns ErrReleasedSlotUnusable so the caller quarantines the work item.
//
// The next sequence is chain truth reconciled with open reservations:
// max(account.sequence, highest unconsumed reservation + 1). An open
// reservation strictly below account.sequence is a mismatch — counted,
// logged, and left for ReconcileAccount; Reserve never auto-resolves it.
func (m *Manager) Reserve(ctx context.Context, chainID, source string, ref storage.WorkRef) (storage.SequenceReservation, error) {
	if !ref.Kind.Valid() || ref.ID == "" {
		return storage.SequenceReservation{}, fmt.Errorf("%w: invalid work_ref %+v", storage.ErrInvalidRecord, ref)
	}

	existing, err := m.store.Sequences().GetByWorkRef(ctx, ref)
	switch {
	case err == nil && existing.Status != storage.SequenceReleased:
		return existing, nil
	case err != nil && !errors.Is(err, storage.ErrNotFound):
		return storage.SequenceReservation{}, err
	}

	accountNumber, chainSeq, err := m.chain.Account(ctx, source)
	if err != nil {
		return storage.SequenceReservation{}, fmt.Errorf("sequences: account query for %s: %w", source, err)
	}

	var out storage.SequenceReservation
	err = m.store.WithTx(ctx, func(ctx context.Context, st storage.Store) error {
		locker, ok := st.(accountLocker)
		if !ok {
			return ErrNoAccountLock
		}
		if err := locker.AcquireAccountLock(ctx, chainID, source); err != nil {
			return err
		}

		open, err := st.Sequences().ListUnconsumed(ctx, chainID, source)
		if err != nil {
			return err
		}
		next := chainSeq
		for _, r := range open {
			if r.Sequence < chainSeq {
				m.countMismatch(chainID)
				m.logger.Warn("open reservation below chain sequence; run ReconcileAccount",
					logging.FieldChainID, chainID,
					logging.FieldAddress, source,
					logging.FieldSequence, r.Sequence,
					"chain_sequence", chainSeq,
				)
			}
			if r.Sequence >= next {
				next = r.Sequence + 1
			}
		}

		if existing.Status == storage.SequenceReleased {
			// Re-bind the same work item to its original slot only while the
			// slot is still ahead of chain truth; a consumed slot can never
			// land this work item's payment deterministically.
			if existing.Sequence < chainSeq {
				return fmt.Errorf("%w: %s/%s sequence %d is below chain sequence %d",
					ErrReleasedSlotUnusable, chainID, source, existing.Sequence, chainSeq)
			}
			next = existing.Sequence
		}

		res, err := st.Sequences().Reserve(ctx, storage.SequenceReservation{
			ChainID:       chainID,
			SourceAddress: source,
			AccountNumber: accountNumber,
			Sequence:      next,
			WorkRef:       ref,
		})
		if err != nil {
			return err
		}
		out = res
		return nil
	})
	if err != nil {
		// A concurrent caller may have bound the same work_ref first; the
		// binding is what matters, not who inserted it.
		if errors.Is(err, storage.ErrDuplicate) {
			if raced, getErr := m.store.Sequences().GetByWorkRef(ctx, ref); getErr == nil {
				return raced, nil
			}
		}
		return storage.SequenceReservation{}, err
	}
	return out, nil
}

// Action records one reconciliation decision for the report/audit trail.
type Action struct {
	ReservationID int64
	WorkRef       storage.WorkRef
	Sequence      uint64
	From          storage.SequenceReservationStatus
	To            storage.SequenceReservationStatus
	Reason        string
}

// Report is the outcome of one ReconcileAccount pass.
type Report struct {
	ChainID       string
	SourceAddress string
	ChainSequence uint64
	Actions       []Action
	Quarantined   int
	Released      int
	Consumed      int
}

// ReconcileAccount re-derives every unconsumed reservation for one account
// from chain truth. Run it at startup and after any detected mismatch.
//
// Resolution is state-dependent (§6): a transaction found on chain by hash
// consumes its reservation; a RESERVED reservation with no signed bytes is
// released; SIGNED, BROADCAST, and every ambiguous case (signed bytes exist,
// work item unreadable, tx not found) is quarantined as
// RECONCILIATION_REQUIRED. A GetTx not-found is never sufficient to release
// a sequence that signed bytes may still redeem.
func (m *Manager) ReconcileAccount(ctx context.Context, chainID, source string) (Report, error) {
	_, chainSeq, err := m.chain.Account(ctx, source)
	if err != nil {
		return Report{}, fmt.Errorf("sequences: account query for %s: %w", source, err)
	}
	report := Report{ChainID: chainID, SourceAddress: source, ChainSequence: chainSeq}

	open, err := m.store.Sequences().ListUnconsumed(ctx, chainID, source)
	if err != nil {
		return report, err
	}
	for _, r := range open {
		to, reason := m.resolve(ctx, r, chainSeq)
		if to == r.Status {
			continue
		}
		if err := m.store.Sequences().UpdateStatus(ctx, r.ID, r.Status, to); err != nil {
			// A concurrent resolver won the race; skip rather than fail the
			// whole pass.
			if errors.Is(err, storage.ErrStatusConflict) {
				continue
			}
			return report, err
		}
		report.Actions = append(report.Actions, Action{
			ReservationID: r.ID, WorkRef: r.WorkRef, Sequence: r.Sequence,
			From: r.Status, To: to, Reason: reason,
		})
		switch to {
		case storage.SequenceConsumed:
			report.Consumed++
		case storage.SequenceReleased:
			report.Released++
		case storage.SequenceReconciliationRequired:
			report.Quarantined++
			m.countMismatch(chainID)
		}
		m.logger.Info("sequence reservation reconciled",
			logging.FieldChainID, chainID,
			logging.FieldAddress, source,
			logging.FieldSequence, r.Sequence,
			"from", string(r.Status), "to", string(to), "reason", reason,
		)
	}
	return report, nil
}

// resolve decides one reservation's target status from chain truth.
func (m *Manager) resolve(ctx context.Context, r storage.SequenceReservation, chainSeq uint64) (storage.SequenceReservationStatus, string) {
	txHash, hasSigned, factsErr := m.workFacts(ctx, r.WorkRef)
	if factsErr != nil {
		if r.Status == storage.SequenceReconciliationRequired {
			return r.Status, ""
		}
		return storage.SequenceReconciliationRequired, "work item unreadable: " + factsErr.Error()
	}

	if txHash != "" {
		if info, err := m.chain.Tx(ctx, txHash); err == nil && info != nil && info.Height > 0 {
			return storage.SequenceConsumed, fmt.Sprintf("tx %s included at height %d", txHash, info.Height)
		} else if err != nil && !errors.Is(err, client.ErrNotFound) {
			if r.Status == storage.SequenceReconciliationRequired {
				return r.Status, ""
			}
			return storage.SequenceReconciliationRequired, "tx lookup failed: " + err.Error()
		}
	}

	switch r.Status {
	case storage.SequenceReserved:
		if hasSigned {
			// Signed bytes exist for a reservation the state machine thinks
			// is unsigned: ambiguous — quarantine, never release.
			return storage.SequenceReconciliationRequired, "signed bytes exist for RESERVED reservation"
		}
		if r.Sequence < chainSeq {
			return storage.SequenceReleased, "unsigned; slot consumed on chain by another transaction"
		}
		return storage.SequenceReleased, "unsigned; released for re-issue"
	case storage.SequenceSigned, storage.SequenceBroadcast:
		return storage.SequenceReconciliationRequired, "signed transaction not found on chain; bytes may still redeem the sequence"
	case storage.SequenceReconciliationRequired:
		return r.Status, ""
	default:
		return r.Status, ""
	}
}

// workFacts returns the bound work item's known tx hash and whether signed
// bytes were persisted for it.
func (m *Manager) workFacts(ctx context.Context, ref storage.WorkRef) (txHash string, hasSigned bool, err error) {
	switch ref.Kind {
	case storage.WorkWithdrawal:
		w, err := m.store.Withdrawals().Get(ctx, ref.ID)
		if err != nil {
			return "", false, err
		}
		if w.TxHash != nil {
			txHash = *w.TxHash
		}
		return txHash, len(w.SignedTxBytes) > 0, nil
	case storage.WorkSweep:
		j, err := m.store.Sweeps().Get(ctx, ref.ID)
		if err != nil {
			return "", false, err
		}
		if j.TxHash != nil {
			txHash = *j.TxHash
		}
		return txHash, len(j.SignedTxBytes) > 0, nil
	default:
		return "", false, fmt.Errorf("%w: unknown work kind %q", storage.ErrInvalidRecord, ref.Kind)
	}
}

// RebroadcastResult reports what RebroadcastPersisted found or did.
type RebroadcastResult struct {
	TxHash string
	// AlreadyIncluded is true when the transaction was found on chain and
	// nothing was broadcast.
	AlreadyIncluded bool
	Height          int64
	Code            uint32
	RawLog          string
	// Accepted is true when a broadcast was performed and CheckTx passed.
	Accepted bool
}

// RebroadcastPersisted is the ONLY recovery path for a quarantined
// signed transaction: it searches for the exact persisted bytes by hash
// first and, only when absent, rebroadcasts those identical bytes. It takes
// bytes, not a work item — re-signing is structurally impossible here.
func (m *Manager) RebroadcastPersisted(ctx context.Context, signedTxBytes []byte) (RebroadcastResult, error) {
	if len(signedTxBytes) == 0 {
		return RebroadcastResult{}, fmt.Errorf("%w: empty signed tx bytes", storage.ErrInvalidRecord)
	}
	digest := sha256.Sum256(signedTxBytes)
	hash := strings.ToUpper(hex.EncodeToString(digest[:]))

	info, err := m.chain.Tx(ctx, hash)
	switch {
	case err == nil && info != nil && info.Height > 0:
		return RebroadcastResult{
			TxHash: hash, AlreadyIncluded: true,
			Height: info.Height, Code: info.Code, RawLog: info.RawLog,
		}, nil
	case err != nil && !errors.Is(err, client.ErrNotFound):
		return RebroadcastResult{TxHash: hash}, fmt.Errorf("sequences: tx search before rebroadcast: %w", err)
	}

	res, err := m.chain.Broadcast(ctx, signedTxBytes, client.BroadcastSync)
	if err != nil {
		return RebroadcastResult{TxHash: hash}, fmt.Errorf("sequences: rebroadcast: %w", err)
	}
	return RebroadcastResult{
		TxHash: res.TxHash, Code: res.Code, RawLog: res.RawLog, Accepted: res.Accepted,
	}, nil
}

func (m *Manager) countMismatch(chainID string) {
	if m.metrics != nil {
		m.metrics.SequenceMismatch.WithLabelValues(chainID).Inc()
	}
}
