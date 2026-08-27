package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestBuildDSNConnectionSettings proves the DSN Open builds actually yields
// connections with the R7-required settings: WAL journal, 5s busy timeout,
// foreign keys ON.
func TestBuildDSNConnectionSettings(t *testing.T) {
	dsn := buildDSN(filepath.Join(t.TempDir(), "settings.db"))
	require.Contains(t, dsn, "_txlock=immediate")

	db, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	defer db.Close()

	var journalMode string
	require.NoError(t, db.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode))
	require.Equal(t, "wal", journalMode)

	var busyTimeout int
	require.NoError(t, db.QueryRow(`PRAGMA busy_timeout`).Scan(&busyTimeout))
	require.Equal(t, 5000, busyTimeout)

	var foreignKeys int
	require.NoError(t, db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys))
	require.Equal(t, 1, foreignKeys)
}

func TestBuildDSNPreservesCallerParams(t *testing.T) {
	dsn := buildDSN("file:/tmp/x.db?cache=private")
	require.Contains(t, dsn, "cache=private")
	require.Contains(t, dsn, "_pragma=busy_timeout(5000)")
	require.Contains(t, dsn, "_pragma=journal_mode(WAL)")
	require.Contains(t, dsn, "_pragma=foreign_keys(1)")
}
