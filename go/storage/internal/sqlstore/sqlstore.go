// Package sqlstore is the shared database/sql implementation of the
// storage.Store contract. Both dialects persist the identical schema
// (storage/migrations, R7) — timestamps as RFC3339 UTC TEXT, amounts as
// integer-string TEXT, JSON for list-valued columns — so a single
// implementation serves both; the per-backend differences (placeholder
// style, unique-violation mapping, row locking, per-account serialization)
// are isolated behind the Dialect interface implemented by storage/sqlite
// and storage/postgres.
package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	sdkmath "cosmossdk.io/math"

	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

// Querier is the subset of *sql.DB / *sql.Tx the store runs on.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Dialect isolates the backend-specific behaviour.
type Dialect interface {
	// Rebind converts '?' placeholders to the backend's style.
	Rebind(query string) string
	// MapError translates driver constraint violations into the storage
	// package's typed errors (ErrDuplicate / ErrActiveSweepExists); any
	// other error is returned unchanged.
	MapError(err error) error
	// RowLock returns the row-locking suffix for read-modify-write SELECTs
	// (" FOR UPDATE" on Postgres, "" on SQLite where the single-writer
	// connection + BEGIN IMMEDIATE serializes globally).
	RowLock() string
	// AcquireAccountLock serializes sequence reservation for one
	// (chain_id, source_address) inside the current transaction (R7:
	// Postgres locks the chain_account_locks row; SQLite is a no-op).
	AcquireAccountLock(ctx context.Context, q Querier, chainID, sourceAddress string) error
}

// Store implements storage.Store over one Querier.
type Store struct {
	db *sql.DB // nil inside a transaction
	tx *sql.Tx // non-nil inside a transaction
	q  Querier
	d  Dialect
}

// New wraps an open, migrated *sql.DB.
func New(db *sql.DB, d Dialect) *Store {
	return &Store{db: db, q: db, d: d}
}

// WithTx implements storage.Tx. Nested calls run inside the already-open
// transaction.
func (s *Store) WithTx(ctx context.Context, fn func(ctx context.Context, st storage.Store) error) error {
	if s.tx != nil {
		return fn(ctx, s)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlstore: begin: %w", err)
	}
	ts := &Store{tx: tx, q: tx, d: s.d}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(ctx, ts); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlstore: commit: %w", err)
	}
	return nil
}

// withWrite runs fn inside a transaction, reusing the current one when the
// store is already transactional. Multi-statement read-modify-write paths
// (UpdateStatus, Reserve, Apply, SaveReport) route through it.
func (s *Store) withWrite(ctx context.Context, fn func(ctx context.Context, st *Store) error) error {
	if s.tx != nil {
		return fn(ctx, s)
	}
	return s.WithTx(ctx, func(ctx context.Context, st storage.Store) error {
		return fn(ctx, st.(*Store))
	})
}

const creditGateLockSource = "__sovren_credit_gate__"

// AcquireAccountLock serializes the caller against every other worker of
// one (chain_id, source_address) account for the remainder of the current
// transaction. Callers computing the next sequence with a read-modify-write
// (read reservations → Reserve) MUST take this lock before the read; it is
// only valid inside WithTx. On SQLite it is a no-op (the single-writer
// connection + BEGIN IMMEDIATE already serializes globally); on Postgres it
// locks the chain_account_locks row (R7).
func (s *Store) AcquireAccountLock(ctx context.Context, chainID, sourceAddress string) error {
	if s.tx == nil {
		return errors.New("sqlstore: AcquireAccountLock requires a transaction (use WithTx)")
	}
	return s.d.AcquireAccountLock(ctx, s.q, chainID, sourceAddress)
}

// AcquireCreditGateLock uses the existing per-chain account-lock table with a
// reserved source key. CreditDeposit, Controls.Apply, and ChainReview.Open all
// take this lock before reading or mutating the gate, giving PostgreSQL a
// linear transaction order; SQLite is already serialized by BEGIN IMMEDIATE.
func (s *Store) AcquireCreditGateLock(ctx context.Context, chainID string) error {
	return s.AcquireAccountLock(ctx, chainID, creditGateLockSource)
}

// Close closes the underlying database. Inside a transaction it is a no-op
// (the transaction owner closes the root store).
func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Repository accessors.

func (s *Store) Ledger() storage.LedgerRepo           { return ledgerRepo{s} }
func (s *Store) Deposits() storage.DepositRepo        { return depositRepo{s} }
func (s *Store) Checkpoints() storage.CheckpointRepo  { return checkpointRepo{s} }
func (s *Store) Withdrawals() storage.WithdrawalRepo  { return withdrawalRepo{s} }
func (s *Store) Sequences() storage.SequenceRepo      { return sequenceRepo{s} }
func (s *Store) Sweeps() storage.SweepRepo            { return sweepRepo{s} }
func (s *Store) Watch() storage.WatchRepo             { return watchRepo{s} }
func (s *Store) Controls() storage.ControlsRepo       { return controlsRepo{s} }
func (s *Store) Review() storage.ReviewRepo           { return reviewRepo{s} }
func (s *Store) ChainReview() storage.ChainReviewRepo { return chainReviewRepo{s} }
func (s *Store) Recon() storage.ReconRepo             { return reconRepo{s} }
func (s *Store) Outbox() storage.OutboxRepo           { return outboxRepo{s} }

var _ storage.Store = (*Store)(nil)

// exec runs a rebound statement, mapping constraint violations.
func (s *Store) exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	res, err := s.q.ExecContext(ctx, s.d.Rebind(query), args...)
	if err != nil {
		return nil, s.d.MapError(err)
	}
	return res, nil
}

func (s *Store) query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	rows, err := s.q.QueryContext(ctx, s.d.Rebind(query), args...)
	if err != nil {
		return nil, s.d.MapError(err)
	}
	return rows, nil
}

func (s *Store) queryRow(ctx context.Context, query string, args ...any) *sql.Row {
	return s.q.QueryRowContext(ctx, s.d.Rebind(query), args...)
}

// ---------------------------------------------------------------------------
// Serialization helpers (R7 conventions: RFC3339 UTC TEXT timestamps,
// integer-string TEXT amounts, JSON TEXT lists).
// ---------------------------------------------------------------------------

// timeLayout is fixed-width RFC3339 UTC with nanoseconds so stored strings
// sort lexicographically in chronological order.
const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

func fmtTime(t time.Time) string {
	if t.IsZero() {
		t = time.Now()
	}
	return t.UTC().Format(timeLayout)
}

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("sqlstore: bad timestamp %q: %w", s, err)
	}
	return t.UTC(), nil
}

func fmtTimePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(timeLayout)
}

func parseTimePtr(s sql.NullString) (*time.Time, error) {
	if !s.Valid {
		return nil, nil
	}
	t, err := parseTime(s.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// amountText serializes a required amount column; nil Ints and negatives
// (where disallowed by the schema CHECK) fail pre-insert with
// ErrInvalidRecord.
func amountText(field string, v sdkmath.Int, requirePositive bool) (string, error) {
	if v.IsNil() {
		return "", fmt.Errorf("%w: %s is unset", storage.ErrInvalidRecord, field)
	}
	if v.IsNegative() {
		return "", fmt.Errorf("%w: %s is negative", storage.ErrInvalidRecord, field)
	}
	if requirePositive && v.IsZero() {
		return "", fmt.Errorf("%w: %s must be positive", storage.ErrInvalidRecord, field)
	}
	return v.String(), nil
}

func amountFromText(s string) (sdkmath.Int, error) {
	v, ok := sdkmath.NewIntFromString(s)
	if !ok {
		return sdkmath.Int{}, fmt.Errorf("sqlstore: bad integer amount %q", s)
	}
	return v, nil
}

func amountPtrText(field string, v *sdkmath.Int) (any, error) {
	if v == nil {
		return nil, nil
	}
	s, err := amountText(field, *v, false)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func amountPtrFromText(s sql.NullString) (*sdkmath.Int, error) {
	if !s.Valid {
		return nil, nil
	}
	v, err := amountFromText(s.String)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func jsonText(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("sqlstore: marshal: %w", err)
	}
	return string(b), nil
}

func jsonStrings(s string) ([]string, error) {
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("sqlstore: unmarshal string list %q: %w", s, err)
	}
	return out, nil
}

func jsonInt64s(s string) ([]int64, error) {
	var out []int64
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("sqlstore: unmarshal int list %q: %w", s, err)
	}
	return out, nil
}

// Unsigned column helpers: heights and indexes are stored in signed integer
// columns; values are bounded far below 2^63 in practice.

func u64(v uint64) int64   { return int64(v) }
func u32(v uint32) int64   { return int64(v) }
func asU64(v int64) uint64 { return uint64(v) }
func asU32(v int64) uint32 { return uint32(v) }

func u64Ptr(v *uint64) any {
	if v == nil {
		return nil
	}
	return int64(*v)
}

func u32Ptr(v *uint32) any {
	if v == nil {
		return nil
	}
	return int64(*v)
}

func asU64Ptr(v sql.NullInt64) *uint64 {
	if !v.Valid {
		return nil
	}
	u := uint64(v.Int64)
	return &u
}

func asU32Ptr(v sql.NullInt64) *uint32 {
	if !v.Valid {
		return nil
	}
	u := uint32(v.Int64)
	return &u
}

func strPtr(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}

func asStrPtr(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	s := v.String
	return &s
}

// nullIfEmpty stores "" as NULL (used for the outbox dedup key, where empty
// means "no dedup" and must not collide in the UNIQUE index).
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func notFound(err error, what string) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", storage.ErrNotFound, what)
	}
	return err
}

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{storage.ErrInvalidRecord}, args...)...)
}

// statusList renders an enum slice as a quoted SQL IN list. Values come from
// the package's own enum constants, never from user input.
func statusList[T ~string](vals []T) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = "'" + string(v) + "'"
	}
	return strings.Join(parts, ",")
}
