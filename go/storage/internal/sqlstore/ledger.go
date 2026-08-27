package sqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"

	sdkmath "cosmossdk.io/math"

	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

type ledgerRepo struct{ s *Store }

const ledgerCols = `id, chain_id, kind, tx_hash, message_index, op_index, block_height,
	event_index, direction, address, counterparty_set, amount_base_units, denom,
	tx_code, classification, created_at`

func (r ledgerRepo) Append(ctx context.Context, e storage.LedgerEntry) (storage.LedgerEntry, error) {
	if e.ChainID == "" {
		return e, invalidf("ledger entry: empty chain_id")
	}
	if !e.Kind.Valid() {
		return e, invalidf("ledger entry: unknown kind %q", e.Kind)
	}
	if !e.Direction.Valid() {
		return e, invalidf("ledger entry: unknown direction %q", e.Direction)
	}
	if !e.Classification.Valid() {
		return e, invalidf("ledger entry: unknown classification %q", e.Classification)
	}
	if e.Address == "" {
		return e, invalidf("ledger entry: empty address")
	}
	if e.Denom == "" {
		return e, invalidf("ledger entry: empty denom")
	}
	switch e.Kind {
	case storage.LedgerKindTx:
		if e.TxHash == "" {
			return e, invalidf("ledger entry: TX kind requires tx_hash")
		}
	case storage.LedgerKindBlockEvent:
		if e.TxHash != "" {
			return e, invalidf("ledger entry: BLOCK_EVENT kind carries no tx_hash")
		}
	}
	amount, err := amountText("amount_base_units", e.AmountBaseUnits, false)
	if err != nil {
		return e, err
	}
	cps, err := jsonText(nonNilStrings(e.CounterpartySet))
	if err != nil {
		return e, err
	}
	createdAt := fmtTime(e.CreatedAt)

	// TX rows leave event_index NULL; BLOCK_EVENT rows leave the tx triple
	// NULL (schema CHECK).
	var txHash, msgIdx, opIdx, evIdx any
	if e.Kind == storage.LedgerKindTx {
		txHash, msgIdx, opIdx = e.TxHash, u32(e.MessageIndex), u32(e.OpIndex)
	} else {
		evIdx = u32(e.EventIndex)
	}

	err = r.s.queryRow(ctx, `
		INSERT INTO ledger_entries (chain_id, kind, tx_hash, message_index, op_index,
			block_height, event_index, direction, address, counterparty_set,
			amount_base_units, denom, tx_code, classification, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING id`,
		e.ChainID, string(e.Kind), txHash, msgIdx, opIdx, u64(e.BlockHeight), evIdx,
		string(e.Direction), e.Address, cps, amount, e.Denom, u32(e.TxCode),
		string(e.Classification), createdAt,
	).Scan(&e.ID)
	if err != nil {
		return e, r.s.d.MapError(err)
	}
	e.CreatedAt, err = parseTime(createdAt)
	return e, err
}

func (r ledgerRepo) GetTxEntry(ctx context.Context, chainID, txHash string, messageIndex, opIndex uint32) (storage.LedgerEntry, error) {
	row := r.s.queryRow(ctx, `SELECT `+ledgerCols+` FROM ledger_entries
		WHERE chain_id = ? AND kind = 'TX' AND tx_hash = ? AND message_index = ? AND op_index = ?`,
		chainID, txHash, u32(messageIndex), u32(opIndex))
	e, err := scanLedger(row)
	if err != nil {
		return e, notFound(err, fmt.Sprintf("ledger tx entry %s/%s/%d/%d", chainID, txHash, messageIndex, opIndex))
	}
	return e, nil
}

func (r ledgerRepo) GetBlockEventEntry(ctx context.Context, chainID string, blockHeight uint64, eventIndex uint32) (storage.LedgerEntry, error) {
	row := r.s.queryRow(ctx, `SELECT `+ledgerCols+` FROM ledger_entries
		WHERE chain_id = ? AND kind = 'BLOCK_EVENT' AND block_height = ? AND event_index = ?`,
		chainID, u64(blockHeight), u32(eventIndex))
	e, err := scanLedger(row)
	if err != nil {
		return e, notFound(err, fmt.Sprintf("ledger block-event entry %s/%d/%d", chainID, blockHeight, eventIndex))
	}
	return e, nil
}

func (r ledgerRepo) List(ctx context.Context, q storage.LedgerQuery) ([]storage.LedgerEntry, error) {
	query := `SELECT ` + ledgerCols + ` FROM ledger_entries
		WHERE chain_id = ? AND address = ? AND block_height >= ? AND id > ?`
	args := []any{q.ChainID, q.Address, u64(q.FromHeight), q.AfterID}
	if q.ToHeight != 0 {
		query += ` AND block_height <= ?`
		args = append(args, u64(q.ToHeight))
	}
	query += ` ORDER BY id`
	if q.Limit > 0 {
		query += ` LIMIT ` + strconv.Itoa(q.Limit)
	}
	rows, err := r.s.query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storage.LedgerEntry
	for rows.Next() {
		e, err := scanLedger(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (r ledgerRepo) AppendFeeOutflow(ctx context.Context, f storage.FeeOutflow) (storage.FeeOutflow, error) {
	if f.ChainID == "" || f.TxHash == "" {
		return f, invalidf("fee outflow: empty chain_id or tx_hash")
	}
	if f.PayerAddress == "" {
		return f, invalidf("fee outflow: empty payer_address")
	}
	fee, err := amountText("fee_base_units", f.FeeBaseUnits, false)
	if err != nil {
		return f, err
	}
	createdAt := fmtTime(f.CreatedAt)
	err = r.s.queryRow(ctx, `
		INSERT INTO fee_outflows (chain_id, tx_hash, payer_address, fee_base_units, tx_code, block_height, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		RETURNING id`,
		f.ChainID, f.TxHash, f.PayerAddress, fee, u32(f.TxCode), u64(f.BlockHeight), createdAt,
	).Scan(&f.ID)
	if err != nil {
		return f, r.s.d.MapError(err)
	}
	f.CreatedAt, err = parseTime(createdAt)
	return f, err
}

func (r ledgerRepo) ListFeeOutflows(ctx context.Context, chainID, payerAddress string, fromHeight, toHeight uint64) ([]storage.FeeOutflow, error) {
	query := `SELECT id, chain_id, tx_hash, payer_address, fee_base_units, tx_code, block_height, created_at
		FROM fee_outflows WHERE chain_id = ? AND payer_address = ? AND block_height >= ?`
	args := []any{chainID, payerAddress, u64(fromHeight)}
	if toHeight != 0 {
		query += ` AND block_height <= ?`
		args = append(args, u64(toHeight))
	}
	query += ` ORDER BY block_height, id`
	rows, err := r.s.query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storage.FeeOutflow
	for rows.Next() {
		var (
			f         storage.FeeOutflow
			fee       string
			txCode    int64
			height    int64
			createdAt string
		)
		if err := rows.Scan(&f.ID, &f.ChainID, &f.TxHash, &f.PayerAddress, &fee, &txCode, &height, &createdAt); err != nil {
			return nil, err
		}
		if f.FeeBaseUnits, err = amountFromText(fee); err != nil {
			return nil, err
		}
		f.TxCode, f.BlockHeight = asU32(txCode), asU64(height)
		if f.CreatedAt, err = parseTime(createdAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (r ledgerRepo) AppendFeeFundingSpend(ctx context.Context, s storage.FeeFundingSpend) (storage.FeeFundingSpend, error) {
	if s.ChainID == "" || s.TxHash == "" {
		return s, invalidf("fee funding spend: empty chain_id or tx_hash")
	}
	if s.FeeWalletAddress == "" {
		return s, invalidf("fee funding spend: empty fee_wallet_address")
	}
	amount, err := amountText("amount_base_units", s.AmountBaseUnits, false)
	if err != nil {
		return s, err
	}
	createdAt := fmtTime(s.CreatedAt)
	err = r.s.queryRow(ctx, `
		INSERT INTO fee_funding_spends (chain_id, tx_hash, fee_wallet_address, amount_base_units, block_height, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		RETURNING id`,
		s.ChainID, s.TxHash, s.FeeWalletAddress, amount, u64(s.BlockHeight), createdAt,
	).Scan(&s.ID)
	if err != nil {
		return s, r.s.d.MapError(err)
	}
	s.CreatedAt, err = parseTime(createdAt)
	return s, err
}

// SumFeeFundingSpend folds the windowed amounts in Go as integer math.Int (never
// a SQL SUM) so the total is exact and dialect-agnostic — the same reasoning as
// withdrawalRepo.SumCommittedBySource: a SQL SUM over the integer-string TEXT
// column would coerce through int4/float in one dialect or the other.
func (r ledgerRepo) SumFeeFundingSpend(ctx context.Context, chainID, feeWalletAddress string, fromHeight, toHeight uint64) (sdkmath.Int, error) {
	sum := sdkmath.ZeroInt()
	query := `SELECT amount_base_units FROM fee_funding_spends
		WHERE chain_id = ? AND fee_wallet_address = ? AND block_height >= ?`
	args := []any{chainID, feeWalletAddress, u64(fromHeight)}
	if toHeight != 0 {
		query += ` AND block_height <= ?`
		args = append(args, u64(toHeight))
	}
	rows, err := r.s.query(ctx, query, args...)
	if err != nil {
		return sum, err
	}
	defer rows.Close()
	for rows.Next() {
		var amount string
		if err := rows.Scan(&amount); err != nil {
			return sum, err
		}
		v, err := amountFromText(amount)
		if err != nil {
			return sum, err
		}
		sum = sum.Add(v)
	}
	return sum, rows.Err()
}

type rowScanner interface{ Scan(dest ...any) error }

func scanLedger(row rowScanner) (storage.LedgerEntry, error) {
	var (
		e                      storage.LedgerEntry
		kind, dir, class       string
		txHash                 sql.NullString
		msgIdx, opIdx, evIdx   sql.NullInt64
		height, txCode         int64
		cps, amount, createdAt string
	)
	err := row.Scan(&e.ID, &e.ChainID, &kind, &txHash, &msgIdx, &opIdx, &height,
		&evIdx, &dir, &e.Address, &cps, &amount, &e.Denom, &txCode, &class, &createdAt)
	if err != nil {
		return e, err
	}
	e.Kind = storage.LedgerEntryKind(kind)
	e.Direction = storage.LedgerDirection(dir)
	e.Classification = storage.Classification(class)
	if txHash.Valid {
		e.TxHash = txHash.String
	}
	if msgIdx.Valid {
		e.MessageIndex = asU32(msgIdx.Int64)
	}
	if opIdx.Valid {
		e.OpIndex = asU32(opIdx.Int64)
	}
	if evIdx.Valid {
		e.EventIndex = asU32(evIdx.Int64)
	}
	e.BlockHeight, e.TxCode = asU64(height), asU32(txCode)
	if e.CounterpartySet, err = jsonStrings(cps); err != nil {
		return e, err
	}
	if e.AmountBaseUnits, err = amountFromText(amount); err != nil {
		return e, err
	}
	e.CreatedAt, err = parseTime(createdAt)
	return e, err
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
