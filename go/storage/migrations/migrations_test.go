package migrations_test

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage/migrations"
)

func allSQL(t *testing.T, d migrations.Dialect) string {
	t.Helper()
	names, err := migrations.Files(d)
	require.NoError(t, err)
	require.NotEmpty(t, names)
	var b strings.Builder
	for _, n := range names {
		text, err := migrations.Read(d, n)
		require.NoError(t, err)
		b.WriteString(text)
		b.WriteString("\n")
	}
	return b.String()
}

var dialects = []migrations.Dialect{migrations.DialectSQLite, migrations.DialectPostgres}

func TestFileSetsMatchAcrossDialects(t *testing.T) {
	sqliteFiles, err := migrations.Files(migrations.DialectSQLite)
	require.NoError(t, err)
	pgFiles, err := migrations.Files(migrations.DialectPostgres)
	require.NoError(t, err)
	require.Equal(t, sqliteFiles, pgFiles, "both dialects must ship the same migration file names")

	_, err = migrations.Files("mysql")
	require.Error(t, err)
}

func TestStatementsLossless(t *testing.T) {
	normalize := func(s string) string {
		var out []string
		for _, line := range strings.Split(s, "\n") {
			if i := strings.Index(line, "--"); i >= 0 {
				line = line[:i]
			}
			if f := strings.Fields(line); len(f) > 0 {
				out = append(out, strings.Join(f, " "))
			}
		}
		return strings.Join(out, " ")
	}
	for _, d := range dialects {
		names, err := migrations.Files(d)
		require.NoError(t, err)
		for _, n := range names {
			text, err := migrations.Read(d, n)
			require.NoError(t, err)
			stmts := migrations.Statements(text)
			require.NotEmpty(t, stmts, "%s/%s", d, n)
			for _, s := range stmts {
				require.True(t, strings.HasSuffix(s, ";"), "%s/%s: statement missing terminator: %q", d, n, s)
			}
			require.Equal(t, normalize(text), normalize(strings.Join(stmts, "\n")),
				"%s/%s: split must be lossless", d, n)
		}
	}
}

// Every CREATE TABLE / CREATE [UNIQUE] INDEX object must exist under the
// same name in both dialects (R7: identical schema, dialect adjusted).
func TestSchemaObjectParity(t *testing.T) {
	tableRe := regexp.MustCompile(`(?i)CREATE TABLE (?:IF NOT EXISTS )?(\w+)`)
	indexRe := regexp.MustCompile(`(?i)CREATE (UNIQUE )?INDEX (\w+)`)
	objects := func(d migrations.Dialect) (tables, indexes []string) {
		sqlText := allSQL(t, d)
		for _, m := range tableRe.FindAllStringSubmatch(sqlText, -1) {
			tables = append(tables, strings.ToLower(m[1]))
		}
		for _, m := range indexRe.FindAllStringSubmatch(sqlText, -1) {
			indexes = append(indexes, strings.ToLower(strings.TrimSpace(m[1]))+":"+strings.ToLower(m[2]))
		}
		return tables, indexes
	}
	sqliteTables, sqliteIndexes := objects(migrations.DialectSQLite)
	pgTables, pgIndexes := objects(migrations.DialectPostgres)
	require.Equal(t, sqliteTables, pgTables)
	require.Equal(t, sqliteIndexes, pgIndexes)

	require.Contains(t, sqliteTables, "ledger_entries")
	require.Contains(t, sqliteTables, "fee_outflows")
	require.Contains(t, sqliteTables, "deposits")
	require.Contains(t, sqliteTables, "scanner_checkpoints")
	require.Contains(t, sqliteTables, "withdrawals")
	require.Contains(t, sqliteTables, "sequence_reservations")
	require.Contains(t, sqliteTables, "sweep_jobs")
	require.Contains(t, sqliteTables, "watched_addresses")
	require.Contains(t, sqliteTables, "operational_controls")
	require.Contains(t, sqliteTables, "controls_audit")
	require.Contains(t, sqliteTables, "chain_review_conditions")
	require.Contains(t, sqliteTables, "review_items")
	require.Contains(t, sqliteTables, "reconciliation_reports")
	require.Contains(t, sqliteTables, "reconciliation_entries")
	require.Contains(t, sqliteTables, "outbox")
	require.Contains(t, sqliteTables, "chain_account_locks")
}

// The data model's uniqueness rules must appear verbatim as SQL constraints
// in both dialects.
func TestUniqueConstraintsPresent(t *testing.T) {
	required := []string{
		// deposits FR-024
		"UNIQUE (chain_id, tx_hash, message_index, coin_index, recipient_address)",
		// withdrawals + sweeps idempotency (FR-033 / FR-039)
		"UNIQUE (idempotency_key)",
		// sequence reservations §6
		"UNIQUE (chain_id, source_address, sequence)",
		"UNIQUE (work_kind, work_id)",
		// ledger identities §3
		"ON ledger_entries (chain_id, tx_hash, message_index, op_index)",
		"ON ledger_entries (chain_id, block_height, event_index)",
		// fee outflows §8a
		"UNIQUE (chain_id, tx_hash)",
	}
	for _, d := range dialects {
		sqlText := allSQL(t, d)
		for _, fragment := range required {
			require.Contains(t, sqlText, fragment, "dialect %s", d)
		}
		require.Equal(t, 2, strings.Count(sqlText, "UNIQUE (idempotency_key)"),
			"dialect %s: withdrawals and sweep_jobs each carry an idempotency-key constraint", d)
	}
}

// The sweep partial-unique index must scope to exactly the non-terminal
// statuses (data model §7: terminal = CONFIRMED/FAILED/CANCELLED).
func TestSweepPartialUniqueMatchesTerminalSet(t *testing.T) {
	quoted := make([]string, len(storage.TerminalSweepStatuses))
	for i, s := range storage.TerminalSweepStatuses {
		quoted[i] = "'" + string(s) + "'"
	}
	want := fmt.Sprintf("WHERE status NOT IN (%s)", strings.Join(quoted, ","))
	for _, d := range dialects {
		sqlText := allSQL(t, d)
		idx := strings.Index(sqlText, "CREATE UNIQUE INDEX ux_sweep_nonterminal")
		require.GreaterOrEqual(t, idx, 0, "dialect %s", d)
		require.Contains(t, sqlText[idx:idx+300], "ON sweep_jobs (chain_id, source_address)", "dialect %s", d)
		require.Contains(t, sqlText[idx:idx+300], want, "dialect %s", d)
	}
}

// Every status/enum CHECK list must equal the Go enum set, in order — the
// schema and the storage package can never drift apart silently.
func TestCheckConstraintsMatchGoEnums(t *testing.T) {
	inList := func(vals []string) string { return "('" + strings.Join(vals, "','") + "')" }
	toStrings := func(n int, get func(i int) string) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = get(i)
		}
		return out
	}
	cases := []struct {
		column string
		want   string
	}{
		{"kind", "kind IN " + inList(toStrings(len(storage.AllLedgerEntryKinds), func(i int) string { return string(storage.AllLedgerEntryKinds[i]) }))},
		{"direction", "direction IN " + inList(toStrings(len(storage.AllLedgerDirections), func(i int) string { return string(storage.AllLedgerDirections[i]) }))},
		{"classification", "classification IN " + inList(toStrings(len(storage.AllClassifications), func(i int) string { return string(storage.AllClassifications[i]) }))},
		{"deposit status", "status IN " + inList(toStrings(len(storage.AllDepositStatuses), func(i int) string { return string(storage.AllDepositStatuses[i]) }))},
		{"withdrawal status", "status IN " + inList(toStrings(len(storage.AllWithdrawalStatuses), func(i int) string { return string(storage.AllWithdrawalStatuses[i]) }))},
		{"sweep status", "status IN " + inList(toStrings(len(storage.AllSweepStatuses), func(i int) string { return string(storage.AllSweepStatuses[i]) }))},
		{"sequence status", "status IN " + inList(toStrings(len(storage.AllSequenceReservationStatuses), func(i int) string { return string(storage.AllSequenceReservationStatuses[i]) }))},
		{"work_kind", "work_kind IN " + inList(toStrings(len(storage.AllWorkKinds), func(i int) string { return string(storage.AllWorkKinds[i]) }))},
		{"strategy", "strategy IN " + inList(toStrings(len(storage.AllSweepStrategies), func(i int) string { return string(storage.AllSweepStrategies[i]) }))},
		{"watched kind", "kind IN " + inList(toStrings(len(storage.AllWatchedAddressKinds), func(i int) string { return string(storage.AllWatchedAddressKinds[i]) }))},
		{"trigger", "trigger_kind IN " + inList(toStrings(len(storage.AllChainReviewTriggers), func(i int) string { return string(storage.AllChainReviewTriggers[i]) }))},
		{"review kind", "kind IN " + inList(toStrings(len(storage.AllReviewItemKinds), func(i int) string { return string(storage.AllReviewItemKinds[i]) }))},
		{"recon kind", "kind IN " + inList(toStrings(len(storage.AllReconciliationKinds), func(i int) string { return string(storage.AllReconciliationKinds[i]) }))},
	}
	for _, d := range dialects {
		sqlText := allSQL(t, d)
		for _, c := range cases {
			require.Contains(t, sqlText, c.want, "dialect %s: %s CHECK must match the Go enum set", d, c.column)
		}
	}
}

// signed_tx_bytes must be a real byte-array column in each dialect
// (data model §5: persisted at SIGNED for byte-identical rebroadcast).
func TestSignedTxBytesColumnType(t *testing.T) {
	sqliteSQL := allSQL(t, migrations.DialectSQLite)
	pgSQL := allSQL(t, migrations.DialectPostgres)
	require.Equal(t, 2, strings.Count(sqliteSQL, "signed_tx_bytes BLOB"), "withdrawals + sweep_jobs")
	require.Equal(t, 2, strings.Count(pgSQL, "signed_tx_bytes BYTEA"), "withdrawals + sweep_jobs")
	require.Contains(t, sqliteSQL, "payload BLOB NOT NULL")
	require.Contains(t, pgSQL, "payload BYTEA NOT NULL")
}

func TestDenomChecksPinUsovr(t *testing.T) {
	for _, d := range dialects {
		sqlText := allSQL(t, d)
		require.Equal(t, 2, strings.Count(sqlText, "CHECK (denom = 'usovr')"),
			"dialect %s: deposits and withdrawals pin denom", d)
		require.Contains(t, sqlText, "CHECK (sign_mode = 'SIGN_MODE_DIRECT')", "dialect %s", d)
	}
}
