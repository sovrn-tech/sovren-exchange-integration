// Package migrations embeds the kit's SQL schema, one file set per dialect.
// Both dialects define the identical table/constraint set (R7);
// migrations_test cross-checks them against each other and against the
// storage package's Go enum sets.
package migrations

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

// FS holds the embedded migration files, laid out as <dialect>/<NNNN>_<name>.sql.
//
//go:embed sqlite/*.sql postgres/*.sql
var FS embed.FS

// Dialect selects a migration file set.
type Dialect string

const (
	DialectSQLite   Dialect = "sqlite"
	DialectPostgres Dialect = "postgres"
)

// Valid reports whether d is a known dialect.
func (d Dialect) Valid() bool { return d == DialectSQLite || d == DialectPostgres }

// Files returns the dialect's migration file names in apply order
// (lexicographic — files are NNNN-prefixed).
func Files(d Dialect) ([]string, error) {
	if !d.Valid() {
		return nil, fmt.Errorf("migrations: unknown dialect %q", d)
	}
	entries, err := fs.ReadDir(FS, string(d))
	if err != nil {
		return nil, fmt.Errorf("migrations: read dir %s: %w", d, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// Read returns one migration file's SQL text.
func Read(d Dialect, name string) (string, error) {
	b, err := fs.ReadFile(FS, string(d)+"/"+name)
	if err != nil {
		return "", fmt.Errorf("migrations: read %s/%s: %w", d, name, err)
	}
	return string(b), nil
}

// Statements splits migration SQL into single executable statements so both
// drivers apply them one Exec per statement (pgx extended protocol rejects
// multi-statement strings). Migration files therefore must not contain ';'
// inside string literals or comments; TestStatementsRoundTrip enforces the
// split is lossless for the embedded files.
func Statements(sqlText string) []string {
	var stmts []string
	var b strings.Builder
	for _, rawLine := range strings.Split(sqlText, "\n") {
		line := rawLine
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
		if strings.HasSuffix(strings.TrimSpace(line), ";") {
			stmts = append(stmts, strings.TrimSpace(b.String()))
			b.Reset()
		}
	}
	if rest := strings.TrimSpace(b.String()); rest != "" {
		stmts = append(stmts, rest)
	}
	return stmts
}

// Apply runs every pending migration for the dialect inside one transaction
// per file, tracking applied files in schema_migrations. It returns the
// names it applied. Callers pass an already-open *sql.DB (the driver
// registration lives in storage/sqlite and storage/postgres).
func Apply(ctx context.Context, db *sql.DB, d Dialect) ([]string, error) {
	if _, err := db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return nil, fmt.Errorf("migrations: init schema_migrations: %w", err)
	}
	names, err := Files(d)
	if err != nil {
		return nil, err
	}
	var applied []string
	for _, name := range names {
		done, err := isApplied(ctx, db, d, name)
		if err != nil {
			return applied, err
		}
		if done {
			continue
		}
		if err := applyOne(ctx, db, d, name); err != nil {
			return applied, err
		}
		applied = append(applied, name)
	}
	return applied, nil
}

func placeholder(d Dialect) string {
	if d == DialectPostgres {
		return "$1"
	}
	return "?"
}

func isApplied(ctx context.Context, db *sql.DB, d Dialect, name string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations WHERE name = `+placeholder(d), name).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("migrations: check %s: %w", name, err)
	}
	return n > 0, nil
}

func applyOne(ctx context.Context, db *sql.DB, d Dialect, name string) (err error) {
	text, err := Read(d, name)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migrations: begin %s: %w", name, err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	for _, stmt := range Statements(text) {
		if _, execErr := tx.ExecContext(ctx, stmt); execErr != nil {
			err = fmt.Errorf("migrations: %s: %w\nstatement: %s", name, execErr, stmt)
			return err
		}
	}
	insert := `INSERT INTO schema_migrations (name, applied_at) VALUES (` + placeholder(d) + `, CURRENT_TIMESTAMP)`
	if _, err = tx.ExecContext(ctx, insert, name); err != nil {
		err = fmt.Errorf("migrations: record %s: %w", name, err)
		return err
	}
	if err = tx.Commit(); err != nil {
		err = fmt.Errorf("migrations: commit %s: %w", name, err)
	}
	return err
}
