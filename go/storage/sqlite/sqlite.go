// Package sqlite provides the kit's default, CGO-free storage backend over
// modernc.org/sqlite (R7). Concurrency model: a single-writer pool
// (SetMaxOpenConns(1)) + WAL + busy_timeout, with every transaction opened
// BEGIN IMMEDIATE (_txlock=immediate) so writers take the write lock up
// front instead of failing mid-transaction. Writes are globally serialized —
// acceptable for certification and small deployments; per-account write
// concurrency is a documented Postgres property (storage/postgres).
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"

	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" driver

	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage/internal/sqlstore"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage/migrations"
)

// Open opens (creating if needed) the SQLite database at dsn — a plain file
// path or a file: URI — applies the embedded migrations, and returns the
// storage.Store aggregate.
func Open(dsn string) (storage.Store, error) {
	db, err := sql.Open("sqlite", buildDSN(dsn))
	if err != nil {
		return nil, fmt.Errorf("sqlite: open: %w", err)
	}
	// Single-writer pool: all access serializes through one connection, so
	// concurrent callers queue in the pool instead of hitting SQLITE_BUSY.
	db.SetMaxOpenConns(1)
	if _, err := migrations.Apply(context.Background(), db, migrations.DialectSQLite); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite: migrate: %w", err)
	}
	return sqlstore.New(db, dialect{}), nil
}

// buildDSN layers the kit's required connection options onto the caller's
// DSN: BEGIN IMMEDIATE transactions, 5s busy timeout, WAL, foreign keys ON.
func buildDSN(dsn string) string {
	base := dsn
	var existing string
	if i := strings.IndexByte(dsn, '?'); i >= 0 && strings.HasPrefix(dsn, "file:") {
		base, existing = dsn[:i], dsn[i+1:]
	}
	if !strings.HasPrefix(base, "file:") {
		// Escape URI-significant bytes, then restore path separators.
		base = "file:" + strings.ReplaceAll(url.PathEscape(base), "%2F", "/")
	}
	params := []string{
		"_txlock=immediate",
		"_pragma=busy_timeout(5000)",
		"_pragma=journal_mode(WAL)",
		"_pragma=foreign_keys(1)",
	}
	if existing != "" {
		params = append([]string{existing}, params...)
	}
	return base + "?" + strings.Join(params, "&")
}

// dialect implements sqlstore.Dialect for SQLite.
type dialect struct{}

// Rebind is the identity: SQLite uses '?' placeholders natively.
func (dialect) Rebind(query string) string { return query }

// RowLock returns "": SQLite has no FOR UPDATE; the single-writer connection
// plus BEGIN IMMEDIATE serializes read-modify-write transactions globally.
func (dialect) RowLock() string { return "" }

// AcquireAccountLock is a no-op: the single-writer pool already serializes
// per-account reservation (R7).
func (dialect) AcquireAccountLock(context.Context, sqlstore.Querier, string, string) error {
	return nil
}

// MapError translates SQLite constraint violations. modernc.org/sqlite
// surfaces them as SQLITE_CONSTRAINT_* errors whose message names the
// violated columns: "UNIQUE constraint failed: table.col, ...". The only
// unique over sweep_jobs (chain_id, source_address) is the non-terminal
// partial index ux_sweep_nonterminal, so that column pair identifies the
// active-sweep guard.
func (dialect) MapError(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "sweep_jobs.chain_id, sweep_jobs.source_address"):
		return fmt.Errorf("%w (%v)", storage.ErrActiveSweepExists, err)
	case strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "PRIMARY KEY constraint"):
		return fmt.Errorf("%w (%v)", storage.ErrDuplicate, err)
	}
	return err
}
