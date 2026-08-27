package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage/internal/storetest"
)

// dsnEnv names the opt-in DSN for the live-database tests. Point it at a
// throwaway database — every test drops and recreates the kit's tables.
const dsnEnv = "SOVREN_TEST_POSTGRES_DSN"

// kitTables lists every table the migrations create (drop order irrelevant
// with CASCADE), plus the migration ledger itself.
var kitTables = []string{
	"reconciliation_entries", "reconciliation_reports",
	"ledger_entries", "fee_outflows", "fee_funding_spends", "deposits",
	"scanner_checkpoints", "withdrawals", "sequence_reservations", "sweep_jobs",
	"watched_addresses", "operational_controls", "controls_audit",
	"chain_review_conditions", "review_items", "outbox",
	"chain_account_locks", "schema_migrations",
}

func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv(dsnEnv)
	if dsn == "" {
		t.Skipf("%s not set; skipping PostgreSQL driver tests", dsnEnv)
	}
	return dsn
}

func resetDatabase(t *testing.T, dsn string) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	defer db.Close()
	for _, table := range kitTables {
		_, err := db.ExecContext(context.Background(), fmt.Sprintf(`DROP TABLE IF EXISTS %s CASCADE`, table))
		require.NoError(t, err)
	}
}

// TestPostgresSuite runs the full backend conformance suite (identical to
// the SQLite run) against the database named by SOVREN_TEST_POSTGRES_DSN.
func TestPostgresSuite(t *testing.T) {
	dsn := testDSN(t)
	storetest.RunSuite(t, func(t *testing.T) storage.Store {
		resetDatabase(t, dsn)
		s, err := Open(dsn)
		require.NoError(t, err)
		return s
	})
}

// TestAcquireAccountLockExported exercises the exported *sql.Tx helper
// directly (the sequences path uses the internal dialect hook).
func TestAcquireAccountLockExported(t *testing.T) {
	dsn := testDSN(t)
	resetDatabase(t, dsn)
	s, err := Open(dsn)
	require.NoError(t, err)
	defer func() { require.NoError(t, s.Close()) }()

	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	// First acquisition creates the lock row on demand; a second in the
	// same transaction re-locks it without blocking.
	require.NoError(t, AcquireAccountLock(ctx, tx, "sovr-1", "sovr1hot"))
	require.NoError(t, AcquireAccountLock(ctx, tx, "sovr-1", "sovr1hot"))

	var n int
	require.NoError(t, tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM chain_account_locks WHERE chain_id = $1 AND source_address = $2`,
		"sovr-1", "sovr1hot").Scan(&n))
	require.Equal(t, 1, n)
}

// The unit tests below need no database.

func TestRebind(t *testing.T) {
	d := dialect{}
	require.Equal(t, `SELECT 1`, d.Rebind(`SELECT 1`))
	require.Equal(t, `SELECT $1, $2`, d.Rebind(`SELECT ?, ?`))
	got := d.Rebind(`INSERT INTO t VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	require.Equal(t, `INSERT INTO t VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`, got)
}

func TestMapError(t *testing.T) {
	d := dialect{}
	require.NoError(t, d.MapError(nil))

	plain := errors.New("connection refused")
	require.Equal(t, plain, d.MapError(plain))

	unique := &pgconn.PgError{Code: "23505", ConstraintName: "withdrawals_idempotency_key_key"}
	require.ErrorIs(t, d.MapError(unique), storage.ErrDuplicate)

	sweepGuard := &pgconn.PgError{Code: "23505", ConstraintName: "ux_sweep_nonterminal"}
	require.ErrorIs(t, d.MapError(sweepGuard), storage.ErrActiveSweepExists)
	require.NotErrorIs(t, d.MapError(sweepGuard), storage.ErrDuplicate)

	check := &pgconn.PgError{Code: "23514", ConstraintName: "deposits_denom_check"}
	require.Equal(t, error(check), d.MapError(check))
}
