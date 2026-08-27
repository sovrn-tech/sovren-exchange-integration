// Package postgres provides the kit's production storage backend over
// pgx/v5's database/sql driver (R7). Per-account write concurrency: sequence
// reservation serializes on a chain_account_locks row via
// INSERT … ON CONFLICT DO NOTHING + SELECT … FOR UPDATE
// (AcquireAccountLock), so different (chain_id, source_address) accounts
// reserve concurrently while one account's reservations are strictly
// ordered. The schema's UNIQUE constraints remain the last-line guarantee.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver

	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage/internal/sqlstore"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage/migrations"
)

// Open connects to PostgreSQL at dsn (any pgx-accepted DSN or URL), applies
// the embedded migrations, and returns the storage.Store aggregate.
func Open(dsn string) (storage.Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: open: %w", err)
	}
	if _, err := migrations.Apply(context.Background(), db, migrations.DialectPostgres); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("postgres: migrate: %w", err)
	}
	return sqlstore.New(db, dialect{}), nil
}

// AcquireAccountLock serializes work for one (chain_id, source_address)
// inside tx: it creates the chain_account_locks row on demand, then takes a
// row lock held until the transaction ends (data model §6 lock key
// CHAIN_ACCOUNT:{chain_id}:{source_address}).
func AcquireAccountLock(ctx context.Context, tx *sql.Tx, chainID, sourceAddress string) error {
	return acquireAccountLock(ctx, tx, chainID, sourceAddress)
}

func acquireAccountLock(ctx context.Context, q sqlstore.Querier, chainID, sourceAddress string) error {
	if _, err := q.ExecContext(ctx, `
		INSERT INTO chain_account_locks (chain_id, source_address)
		VALUES ($1, $2) ON CONFLICT DO NOTHING`, chainID, sourceAddress); err != nil {
		return fmt.Errorf("postgres: ensure account lock row: %w", err)
	}
	var one int
	if err := q.QueryRowContext(ctx, `
		SELECT 1 FROM chain_account_locks
		WHERE chain_id = $1 AND source_address = $2 FOR UPDATE`,
		chainID, sourceAddress).Scan(&one); err != nil {
		return fmt.Errorf("postgres: lock account %s/%s: %w", chainID, sourceAddress, err)
	}
	return nil
}

// dialect implements sqlstore.Dialect for PostgreSQL.
type dialect struct{}

// Rebind converts '?' placeholders to $1..$n. Kit queries never contain '?'
// inside literals.
func (dialect) Rebind(query string) string {
	var b []byte
	n := 0
	for i := 0; i < len(query); i++ {
		if query[i] == '?' {
			n++
			b = append(b, '$')
			b = appendInt(b, n)
			continue
		}
		b = append(b, query[i])
	}
	return string(b)
}

func appendInt(b []byte, n int) []byte {
	if n >= 10 {
		b = appendInt(b, n/10)
	}
	return append(b, byte('0'+n%10))
}

func (dialect) RowLock() string { return " FOR UPDATE" }

func (dialect) AcquireAccountLock(ctx context.Context, q sqlstore.Querier, chainID, sourceAddress string) error {
	return acquireAccountLock(ctx, q, chainID, sourceAddress)
}

// MapError translates unique-violation errors (SQLSTATE 23505), routing the
// sweep partial-unique index to ErrActiveSweepExists.
func (dialect) MapError(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		if pgErr.ConstraintName == "ux_sweep_nonterminal" {
			return fmt.Errorf("%w (%v)", storage.ErrActiveSweepExists, err)
		}
		return fmt.Errorf("%w (%v)", storage.ErrDuplicate, err)
	}
	return err
}
