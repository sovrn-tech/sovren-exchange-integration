package sqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

type sweepRepo struct{ s *Store }

const sweepCols = `sweep_id, idempotency_key, chain_id, source_address, hot_wallet_address,
	strategy, amount_base_units, fee_reserve_base_units, deposit_ids, signed_tx_bytes,
	tx_hash, tx_code, status, created_at, updated_at`

func (r sweepRepo) Create(ctx context.Context, j storage.SweepJob) (storage.SweepJob, error) {
	if j.SweepID == "" || j.IdempotencyKey == "" {
		return j, invalidf("sweep: empty sweep_id or idempotency_key")
	}
	if j.ChainID == "" || j.SourceAddress == "" || j.HotWalletAddress == "" {
		return j, invalidf("sweep: empty chain_id, source_address, or hot_wallet_address")
	}
	if !j.Strategy.Valid() {
		return j, invalidf("sweep: unknown strategy %q", j.Strategy)
	}
	j.Status = storage.SweepPending
	amount, err := amountText("amount_base_units", j.AmountBaseUnits, false)
	if err != nil {
		return j, err
	}
	feeReserve, err := amountText("fee_reserve_base_units", j.FeeReserveBaseUnits, false)
	if err != nil {
		return j, err
	}
	depositIDs, err := jsonText(nonNilInt64s(j.DepositIDs))
	if err != nil {
		return j, err
	}
	createdAt, updatedAt := fmtTime(j.CreatedAt), fmtTime(j.UpdatedAt)
	_, err = r.s.exec(ctx, `
		INSERT INTO sweep_jobs (sweep_id, idempotency_key, chain_id, source_address,
			hot_wallet_address, strategy, amount_base_units, fee_reserve_base_units,
			deposit_ids, signed_tx_bytes, tx_hash, tx_code, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		j.SweepID, j.IdempotencyKey, j.ChainID, j.SourceAddress, j.HotWalletAddress,
		string(j.Strategy), amount, feeReserve, depositIDs, j.SignedTxBytes,
		strPtr(j.TxHash), u32Ptr(j.TxCode), string(j.Status), createdAt, updatedAt)
	if err != nil {
		return j, err
	}
	if j.CreatedAt, err = parseTime(createdAt); err != nil {
		return j, err
	}
	j.UpdatedAt, err = parseTime(updatedAt)
	return j, err
}

func (r sweepRepo) Get(ctx context.Context, sweepID string) (storage.SweepJob, error) {
	row := r.s.queryRow(ctx, `SELECT `+sweepCols+` FROM sweep_jobs WHERE sweep_id = ?`, sweepID)
	j, err := scanSweep(row)
	if err != nil {
		return j, notFound(err, "sweep "+sweepID)
	}
	return j, nil
}

func (r sweepRepo) GetByIdempotencyKey(ctx context.Context, idempotencyKey string) (storage.SweepJob, error) {
	row := r.s.queryRow(ctx, `SELECT `+sweepCols+` FROM sweep_jobs WHERE idempotency_key = ?`, idempotencyKey)
	j, err := scanSweep(row)
	if err != nil {
		return j, notFound(err, "sweep idempotency key "+idempotencyKey)
	}
	return j, nil
}

func (r sweepRepo) GetActive(ctx context.Context, chainID, sourceAddress string) (storage.SweepJob, error) {
	row := r.s.queryRow(ctx, `SELECT `+sweepCols+` FROM sweep_jobs
		WHERE chain_id = ? AND source_address = ?
		AND status NOT IN (`+statusList(storage.TerminalSweepStatuses)+`)`,
		chainID, sourceAddress)
	j, err := scanSweep(row)
	if err != nil {
		return j, notFound(err, fmt.Sprintf("active sweep for %s/%s", chainID, sourceAddress))
	}
	return j, nil
}

func (r sweepRepo) ListByStatus(ctx context.Context, chainID string, status storage.SweepStatus, limit int) ([]storage.SweepJob, error) {
	query := `SELECT ` + sweepCols + ` FROM sweep_jobs WHERE chain_id = ? AND status = ? ORDER BY created_at, sweep_id`
	if limit > 0 {
		query += ` LIMIT ` + strconv.Itoa(limit)
	}
	rows, err := r.s.query(ctx, query, chainID, string(status))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storage.SweepJob
	for rows.Next() {
		j, err := scanSweep(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (r sweepRepo) UpdateStatus(ctx context.Context, sweepID string, from, to storage.SweepStatus, set storage.SweepUpdate) error {
	if err := storage.ValidateSweepTransition(from, to); err != nil {
		return err
	}
	var depositIDs string
	if set.DepositIDs != nil {
		var err error
		if depositIDs, err = jsonText(set.DepositIDs); err != nil {
			return err
		}
	}
	return r.s.withWrite(ctx, func(ctx context.Context, st *Store) error {
		var stored string
		err := st.queryRow(ctx,
			`SELECT status FROM sweep_jobs WHERE sweep_id = ?`+st.d.RowLock(), sweepID,
		).Scan(&stored)
		if err != nil {
			return notFound(err, "sweep "+sweepID)
		}
		if stored != string(from) {
			return fmt.Errorf("%w: sweep %s is %s, expected %s", storage.ErrStatusConflict, sweepID, stored, from)
		}
		query := `UPDATE sweep_jobs SET status = ?, updated_at = ?`
		args := []any{string(to), fmtTime(time.Time{})}
		if set.SignedTxBytes != nil {
			query += `, signed_tx_bytes = ?`
			args = append(args, set.SignedTxBytes)
		}
		if set.TxHash != nil {
			query += `, tx_hash = ?`
			args = append(args, *set.TxHash)
		}
		if set.TxCode != nil {
			query += `, tx_code = ?`
			args = append(args, u32Ptr(set.TxCode))
		}
		if set.DepositIDs != nil {
			query += `, deposit_ids = ?`
			args = append(args, depositIDs)
		}
		query += ` WHERE sweep_id = ? AND status = ?`
		args = append(args, sweepID, string(from))
		res, err := st.exec(ctx, query, args...)
		if err != nil {
			return err
		}
		return requireOneRow(res, fmt.Sprintf("sweep %s %s -> %s", sweepID, from, to))
	})
}

func scanSweep(row rowScanner) (storage.SweepJob, error) {
	var (
		j                    storage.SweepJob
		strategy, status     string
		amount, feeReserve   string
		depositIDs           string
		signedTx             []byte
		txHash               sql.NullString
		txCode               sql.NullInt64
		createdAt, updatedAt string
	)
	err := row.Scan(&j.SweepID, &j.IdempotencyKey, &j.ChainID, &j.SourceAddress,
		&j.HotWalletAddress, &strategy, &amount, &feeReserve, &depositIDs, &signedTx,
		&txHash, &txCode, &status, &createdAt, &updatedAt)
	if err != nil {
		return j, err
	}
	j.Strategy = storage.SweepStrategy(strategy)
	j.Status = storage.SweepStatus(status)
	j.SignedTxBytes = signedTx
	j.TxHash = asStrPtr(txHash)
	j.TxCode = asU32Ptr(txCode)
	if j.AmountBaseUnits, err = amountFromText(amount); err != nil {
		return j, err
	}
	if j.FeeReserveBaseUnits, err = amountFromText(feeReserve); err != nil {
		return j, err
	}
	if j.DepositIDs, err = jsonInt64s(depositIDs); err != nil {
		return j, err
	}
	if j.CreatedAt, err = parseTime(createdAt); err != nil {
		return j, err
	}
	j.UpdatedAt, err = parseTime(updatedAt)
	return j, err
}

func nonNilInt64s(s []int64) []int64 {
	if s == nil {
		return []int64{}
	}
	return s
}
