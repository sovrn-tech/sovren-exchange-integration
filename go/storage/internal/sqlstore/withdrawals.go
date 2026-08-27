package sqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	sdkmath "cosmossdk.io/math"

	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

// committedWithdrawalStatuses are the reserved-or-beyond, non-terminal states
// whose amount + max-fee cap is still committed against the source balance.
var committedWithdrawalStatuses = []storage.WithdrawalStatus{
	storage.WithdrawalFundsReserved,
	storage.WithdrawalSequenceReserved,
	storage.WithdrawalTransactionBuilt,
	storage.WithdrawalTransactionSimulated,
	storage.WithdrawalSigned,
	storage.WithdrawalBroadcast,
	storage.WithdrawalReviewRequired,
}

type withdrawalRepo struct{ s *Store }

const withdrawalCols = `withdrawal_id, idempotency_key, chain_id, source_address,
	destination_address, denom, amount_base_units, memo, account_number, sequence,
	gas_wanted, gas_limit, fee_amount_base_units, sign_mode, signed_tx_bytes,
	tx_hash, block_height, tx_code, raw_log, status, created_at, updated_at`

func (r withdrawalRepo) Create(ctx context.Context, w storage.WithdrawalRecord) (storage.WithdrawalRecord, error) {
	if w.WithdrawalID == "" || w.IdempotencyKey == "" {
		return w, invalidf("withdrawal: empty withdrawal_id or idempotency_key")
	}
	if w.ChainID == "" || w.SourceAddress == "" || w.DestinationAddress == "" {
		return w, invalidf("withdrawal: empty chain_id, source_address, or destination_address")
	}
	if w.Denom != storage.BaseDenom {
		return w, invalidf("withdrawal: denom %q is not %s", w.Denom, storage.BaseDenom)
	}
	if w.Status == "" {
		w.Status = storage.WithdrawalRequested
	}
	if !w.Status.Valid() {
		return w, invalidf("withdrawal: unknown status %q", w.Status)
	}
	if w.SignMode == "" {
		w.SignMode = storage.SignModeDirect
	}
	if w.SignMode != storage.SignModeDirect {
		return w, invalidf("withdrawal: sign_mode %q is not %s (R4)", w.SignMode, storage.SignModeDirect)
	}
	amount, err := amountText("amount_base_units", w.AmountBaseUnits, true)
	if err != nil {
		return w, err
	}
	fee, err := amountPtrText("fee_amount_base_units", w.FeeAmountBaseUnits)
	if err != nil {
		return w, err
	}
	createdAt, updatedAt := fmtTime(w.CreatedAt), fmtTime(w.UpdatedAt)
	_, err = r.s.exec(ctx, `
		INSERT INTO withdrawals (withdrawal_id, idempotency_key, chain_id, source_address,
			destination_address, denom, amount_base_units, memo, account_number, sequence,
			gas_wanted, gas_limit, fee_amount_base_units, sign_mode, signed_tx_bytes,
			tx_hash, block_height, tx_code, raw_log, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		w.WithdrawalID, w.IdempotencyKey, w.ChainID, w.SourceAddress, w.DestinationAddress,
		w.Denom, amount, w.Memo, u64Ptr(w.AccountNumber), u64Ptr(w.Sequence),
		u64Ptr(w.GasWanted), u64Ptr(w.GasLimit), fee, w.SignMode, w.SignedTxBytes,
		strPtr(w.TxHash), u64Ptr(w.BlockHeight), u32Ptr(w.TxCode), w.RawLog,
		string(w.Status), createdAt, updatedAt)
	if err != nil {
		return w, err
	}
	if w.CreatedAt, err = parseTime(createdAt); err != nil {
		return w, err
	}
	w.UpdatedAt, err = parseTime(updatedAt)
	return w, err
}

func (r withdrawalRepo) Get(ctx context.Context, withdrawalID string) (storage.WithdrawalRecord, error) {
	row := r.s.queryRow(ctx, `SELECT `+withdrawalCols+` FROM withdrawals WHERE withdrawal_id = ?`, withdrawalID)
	w, err := scanWithdrawal(row)
	if err != nil {
		return w, notFound(err, "withdrawal "+withdrawalID)
	}
	return w, nil
}

func (r withdrawalRepo) GetByIdempotencyKey(ctx context.Context, idempotencyKey string) (storage.WithdrawalRecord, error) {
	row := r.s.queryRow(ctx, `SELECT `+withdrawalCols+` FROM withdrawals WHERE idempotency_key = ?`, idempotencyKey)
	w, err := scanWithdrawal(row)
	if err != nil {
		return w, notFound(err, "withdrawal idempotency key "+idempotencyKey)
	}
	return w, nil
}

func (r withdrawalRepo) ListByStatus(ctx context.Context, chainID string, status storage.WithdrawalStatus, limit int) ([]storage.WithdrawalRecord, error) {
	query := `SELECT ` + withdrawalCols + ` FROM withdrawals WHERE chain_id = ? AND status = ? ORDER BY created_at, withdrawal_id`
	if limit > 0 {
		query += ` LIMIT ` + strconv.Itoa(limit)
	}
	rows, err := r.s.query(ctx, query, chainID, string(status))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storage.WithdrawalRecord
	for rows.Next() {
		w, err := scanWithdrawal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (r withdrawalRepo) UpdateStatus(ctx context.Context, withdrawalID string, from, to storage.WithdrawalStatus, set storage.WithdrawalUpdate) error {
	if err := storage.ValidateWithdrawalTransition(from, to); err != nil {
		return err
	}
	fee, err := amountPtrText("fee_amount_base_units", set.FeeAmountBaseUnits)
	if err != nil {
		return err
	}
	return r.s.withWrite(ctx, func(ctx context.Context, st *Store) error {
		var stored string
		err := st.queryRow(ctx,
			`SELECT status FROM withdrawals WHERE withdrawal_id = ?`+st.d.RowLock(), withdrawalID,
		).Scan(&stored)
		if err != nil {
			return notFound(err, "withdrawal "+withdrawalID)
		}
		if stored != string(from) {
			return fmt.Errorf("%w: withdrawal %s is %s, expected %s", storage.ErrStatusConflict, withdrawalID, stored, from)
		}
		query := `UPDATE withdrawals SET status = ?, updated_at = ?`
		args := []any{string(to), fmtTime(time.Time{})}
		if set.AccountNumber != nil {
			query += `, account_number = ?`
			args = append(args, u64Ptr(set.AccountNumber))
		}
		if set.Sequence != nil {
			query += `, sequence = ?`
			args = append(args, u64Ptr(set.Sequence))
		}
		if set.GasWanted != nil {
			query += `, gas_wanted = ?`
			args = append(args, u64Ptr(set.GasWanted))
		}
		if set.GasLimit != nil {
			query += `, gas_limit = ?`
			args = append(args, u64Ptr(set.GasLimit))
		}
		if set.FeeAmountBaseUnits != nil {
			query += `, fee_amount_base_units = ?`
			args = append(args, fee)
		}
		if set.SignedTxBytes != nil {
			query += `, signed_tx_bytes = ?`
			args = append(args, set.SignedTxBytes)
		}
		if set.TxHash != nil {
			query += `, tx_hash = ?`
			args = append(args, *set.TxHash)
		}
		if set.BlockHeight != nil {
			query += `, block_height = ?`
			args = append(args, u64Ptr(set.BlockHeight))
		}
		if set.TxCode != nil {
			query += `, tx_code = ?`
			args = append(args, u32Ptr(set.TxCode))
		}
		if set.RawLog != nil {
			query += `, raw_log = ?`
			args = append(args, *set.RawLog)
		}
		query += ` WHERE withdrawal_id = ? AND status = ?`
		args = append(args, withdrawalID, string(from))
		res, err := st.exec(ctx, query, args...)
		if err != nil {
			return err
		}
		return requireOneRow(res, fmt.Sprintf("withdrawal %s %s -> %s", withdrawalID, from, to))
	})
}

// SumCommittedBySource sums the committed reservations for one source. Amounts
// are folded in Go as integer math.Int (never a SQL SUM) so the total is
// exact and dialect-agnostic: SQL SUM over the integer-string TEXT column
// would coerce through int4/float in one dialect or the other.
func (r withdrawalRepo) SumCommittedBySource(ctx context.Context, chainID, sourceAddress string) (int64, sdkmath.Int, error) {
	sum := sdkmath.ZeroInt()
	// LEFT JOIN the sequence reservation so a REVIEW_REQUIRED row's commitment
	// keys on the RESERVATION STATE, not just persisted bytes. An ambiguous
	// signer quarantine has no persisted bytes yet a RECONCILIATION_REQUIRED
	// reservation whose signer may hold a redeemable signature — it MUST stay
	// committed, or sequences.Manager would hand a later withdrawal n+1 against
	// a balance that no longer reserves the sequence-n obligation, over-reserving
	// the wallet. Only a genuinely pre-sign REVIEW_REQUIRED row (no bytes AND
	// reservation absent / RESERVED / RELEASED) is excluded — the same
	// distinction review resolution uses (see the preSign predicate in
	// cmd/sovren-adapter/review_resolution.go; keep the two in sync).
	rows, err := r.s.query(ctx, `SELECT w.amount_base_units, w.status, w.signed_tx_bytes, sr.status
		FROM withdrawals w
		LEFT JOIN sequence_reservations sr
			ON sr.work_kind = ? AND sr.work_id = w.withdrawal_id
		WHERE w.chain_id = ? AND w.source_address = ?
		AND w.status IN (`+statusList(committedWithdrawalStatuses)+`)`,
		string(storage.WorkWithdrawal), chainID, sourceAddress)
	if err != nil {
		return 0, sum, err
	}
	defer rows.Close()
	var count int64
	for rows.Next() {
		var amount, status string
		var signedTx []byte
		var resStatus sql.NullString
		if err := rows.Scan(&amount, &status, &signedTx, &resStatus); err != nil {
			return 0, sum, err
		}
		if storage.WithdrawalStatus(status) == storage.WithdrawalReviewRequired &&
			len(signedTx) == 0 && preSignReservationStatus(resStatus) {
			continue
		}
		v, err := amountFromText(amount)
		if err != nil {
			return 0, sum, err
		}
		sum = sum.Add(v)
		count++
	}
	if err := rows.Err(); err != nil {
		return 0, sum, err
	}
	return count, sum, nil
}

// preSignReservationStatus reports whether a withdrawal's sequence reservation
// is in a genuinely pre-sign state — absent, RESERVED, or RELEASED — in which no
// signed transaction could redeem the slot. RELEASED is included because
// sequences.Manager.ReconcileAccount releases a no-signature RESERVED slot at
// startup without touching the withdrawal. Kept in sync with the preSign
// predicate in cmd/sovren-adapter/review_resolution.go.
func preSignReservationStatus(res sql.NullString) bool {
	if !res.Valid {
		return true
	}
	switch storage.SequenceReservationStatus(res.String) {
	case storage.SequenceReserved, storage.SequenceReleased:
		return true
	default:
		return false
	}
}

func scanWithdrawal(row rowScanner) (storage.WithdrawalRecord, error) {
	var (
		w                              storage.WithdrawalRecord
		amount, status                 string
		acctNum, seq, gasW, gasL       sql.NullInt64
		fee, txHash                    sql.NullString
		height, txCode                 sql.NullInt64
		signedTx                       []byte
		createdAt, updatedAt, signMode string
	)
	err := row.Scan(&w.WithdrawalID, &w.IdempotencyKey, &w.ChainID, &w.SourceAddress,
		&w.DestinationAddress, &w.Denom, &amount, &w.Memo, &acctNum, &seq,
		&gasW, &gasL, &fee, &signMode, &signedTx, &txHash, &height, &txCode,
		&w.RawLog, &status, &createdAt, &updatedAt)
	if err != nil {
		return w, err
	}
	w.Status = storage.WithdrawalStatus(status)
	w.SignMode = signMode
	w.SignedTxBytes = signedTx
	w.AccountNumber, w.Sequence = asU64Ptr(acctNum), asU64Ptr(seq)
	w.GasWanted, w.GasLimit = asU64Ptr(gasW), asU64Ptr(gasL)
	w.TxHash = asStrPtr(txHash)
	w.BlockHeight, w.TxCode = asU64Ptr(height), asU32Ptr(txCode)
	if w.AmountBaseUnits, err = amountFromText(amount); err != nil {
		return w, err
	}
	if w.FeeAmountBaseUnits, err = amountPtrFromText(fee); err != nil {
		return w, err
	}
	if w.CreatedAt, err = parseTime(createdAt); err != nil {
		return w, err
	}
	w.UpdatedAt, err = parseTime(updatedAt)
	return w, err
}
