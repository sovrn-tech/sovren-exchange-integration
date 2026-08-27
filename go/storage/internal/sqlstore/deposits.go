package sqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

type depositRepo struct{ s *Store }

const depositCols = `id, chain_id, tx_hash, message_index, coin_index, recipient_address,
	block_height, block_timestamp, sender_address, denom, amount_base_units, memo,
	tx_code, tx_log, status, prior_status, credited_at, sweep_tx_hash, created_at, updated_at`

func (r depositRepo) Insert(ctx context.Context, d storage.DepositRecord) (storage.DepositRecord, error) {
	if d.ChainID == "" || d.TxHash == "" || d.RecipientAddress == "" {
		return d, invalidf("deposit: empty chain_id, tx_hash, or recipient_address")
	}
	if d.Denom != storage.BaseDenom {
		return d, invalidf("deposit: denom %q is not %s", d.Denom, storage.BaseDenom)
	}
	if !d.Status.Valid() {
		return d, invalidf("deposit: unknown status %q", d.Status)
	}
	if (d.Status == storage.DepositSuspended) != (d.PriorStatus != nil) {
		return d, invalidf("deposit: prior_status must be set iff status is SUSPENDED")
	}
	if d.PriorStatus != nil && !d.PriorStatus.Valid() {
		return d, invalidf("deposit: unknown prior_status %q", *d.PriorStatus)
	}
	amount, err := amountText("amount_base_units", d.AmountBaseUnits, true)
	if err != nil {
		return d, err
	}
	createdAt, updatedAt := fmtTime(d.CreatedAt), fmtTime(d.UpdatedAt)
	var prior any
	if d.PriorStatus != nil {
		prior = string(*d.PriorStatus)
	}
	err = r.s.queryRow(ctx, `
		INSERT INTO deposits (chain_id, tx_hash, message_index, coin_index, recipient_address,
			block_height, block_timestamp, sender_address, denom, amount_base_units, memo,
			tx_code, tx_log, status, prior_status, credited_at, sweep_tx_hash, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING id`,
		d.ChainID, d.TxHash, u32(d.MessageIndex), u32(d.CoinIndex), d.RecipientAddress,
		u64(d.BlockHeight), fmtTime(d.BlockTimestamp), strPtr(d.SenderAddress), d.Denom,
		amount, d.Memo, u32(d.TxCode), d.TxLog, string(d.Status), prior,
		fmtTimePtr(d.CreditedAt), strPtr(d.SweepTxHash), createdAt, updatedAt,
	).Scan(&d.ID)
	if err != nil {
		return d, r.s.d.MapError(err)
	}
	if d.CreatedAt, err = parseTime(createdAt); err != nil {
		return d, err
	}
	d.UpdatedAt, err = parseTime(updatedAt)
	return d, err
}

func (r depositRepo) Get(ctx context.Context, chainID, txHash string, messageIndex, coinIndex uint32, recipientAddress string) (storage.DepositRecord, error) {
	row := r.s.queryRow(ctx, `SELECT `+depositCols+` FROM deposits
		WHERE chain_id = ? AND tx_hash = ? AND message_index = ? AND coin_index = ? AND recipient_address = ?`,
		chainID, txHash, u32(messageIndex), u32(coinIndex), recipientAddress)
	d, err := scanDeposit(row)
	if err != nil {
		return d, notFound(err, fmt.Sprintf("deposit %s/%s/%d/%d/%s", chainID, txHash, messageIndex, coinIndex, recipientAddress))
	}
	return d, nil
}

func (r depositRepo) GetByID(ctx context.Context, id int64) (storage.DepositRecord, error) {
	row := r.s.queryRow(ctx, `SELECT `+depositCols+` FROM deposits WHERE id = ?`, id)
	d, err := scanDeposit(row)
	if err != nil {
		return d, notFound(err, fmt.Sprintf("deposit id %d", id))
	}
	return d, nil
}

func (r depositRepo) ListByStatus(ctx context.Context, chainID string, status storage.DepositStatus, limit int) ([]storage.DepositRecord, error) {
	query := `SELECT ` + depositCols + ` FROM deposits WHERE chain_id = ? AND status = ? ORDER BY id`
	if limit > 0 {
		query += ` LIMIT ` + strconv.Itoa(limit)
	}
	rows, err := r.s.query(ctx, query, chainID, string(status))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storage.DepositRecord
	for rows.Next() {
		d, err := scanDeposit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (r depositRepo) UpdateStatus(ctx context.Context, id int64, from, to storage.DepositStatus, set storage.DepositUpdate) error {
	if err := storage.ValidateDepositTransition(from, to); err != nil {
		return err
	}
	return r.s.withWrite(ctx, func(ctx context.Context, st *Store) error {
		var (
			stored string
			prior  sql.NullString
		)
		err := st.queryRow(ctx,
			`SELECT status, prior_status FROM deposits WHERE id = ?`+st.d.RowLock(), id,
		).Scan(&stored, &prior)
		if err != nil {
			return notFound(err, fmt.Sprintf("deposit id %d", id))
		}
		if stored != string(from) {
			return fmt.Errorf("%w: deposit %d is %s, expected %s", storage.ErrStatusConflict, id, stored, from)
		}
		// Resuming SUSPENDED is pinned to the recorded prior status.
		if from == storage.DepositSuspended {
			if !prior.Valid {
				return fmt.Errorf("%w: deposit %d is SUSPENDED with no prior status", storage.ErrStatusConflict, id)
			}
			if prior.String != string(to) {
				return fmt.Errorf("%w: deposit %d SUSPENDED resumes only to %s, not %s",
					storage.ErrIllegalTransition, id, prior.String, to)
			}
		}
		var newPrior any
		if to == storage.DepositSuspended {
			newPrior = string(from)
		}

		query := `UPDATE deposits SET status = ?, prior_status = ?, updated_at = ?`
		args := []any{string(to), newPrior, fmtTime(time.Time{})}
		if set.CreditedAt != nil {
			query += `, credited_at = ?`
			args = append(args, fmtTimePtr(set.CreditedAt))
		}
		if set.SweepTxHash != nil {
			query += `, sweep_tx_hash = ?`
			args = append(args, *set.SweepTxHash)
		}
		if set.TxLog != nil {
			query += `, tx_log = ?`
			args = append(args, *set.TxLog)
		}
		query += ` WHERE id = ? AND status = ?`
		args = append(args, id, string(from))
		res, err := st.exec(ctx, query, args...)
		if err != nil {
			return err
		}
		return requireOneRow(res, fmt.Sprintf("deposit %d %s -> %s", id, from, to))
	})
}

func scanDeposit(row rowScanner) (storage.DepositRecord, error) {
	var (
		d                              storage.DepositRecord
		msgIdx, coinIdx, height, code  int64
		blockTS, createdAt, updatedAt  string
		sender, prior, creditedAt, swp sql.NullString
		amount, status                 string
	)
	err := row.Scan(&d.ID, &d.ChainID, &d.TxHash, &msgIdx, &coinIdx, &d.RecipientAddress,
		&height, &blockTS, &sender, &d.Denom, &amount, &d.Memo, &code, &d.TxLog,
		&status, &prior, &creditedAt, &swp, &createdAt, &updatedAt)
	if err != nil {
		return d, err
	}
	d.MessageIndex, d.CoinIndex = asU32(msgIdx), asU32(coinIdx)
	d.BlockHeight, d.TxCode = asU64(height), asU32(code)
	d.SenderAddress = asStrPtr(sender)
	d.SweepTxHash = asStrPtr(swp)
	d.Status = storage.DepositStatus(status)
	if prior.Valid {
		p := storage.DepositStatus(prior.String)
		d.PriorStatus = &p
	}
	if d.AmountBaseUnits, err = amountFromText(amount); err != nil {
		return d, err
	}
	if d.BlockTimestamp, err = parseTime(blockTS); err != nil {
		return d, err
	}
	if d.CreditedAt, err = parseTimePtr(creditedAt); err != nil {
		return d, err
	}
	if d.CreatedAt, err = parseTime(createdAt); err != nil {
		return d, err
	}
	d.UpdatedAt, err = parseTime(updatedAt)
	return d, err
}

// checkpointRepo persists scanner_checkpoints.
type checkpointRepo struct{ s *Store }

func (r checkpointRepo) Get(ctx context.Context, chainID string) (storage.ScannerCheckpoint, error) {
	var (
		cp        storage.ScannerCheckpoint
		height    int64
		updatedAt string
	)
	err := r.s.queryRow(ctx, `SELECT chain_id, last_fully_processed_height, last_observed_block_hash, updated_at
		FROM scanner_checkpoints WHERE chain_id = ?`, chainID,
	).Scan(&cp.ChainID, &height, &cp.LastObservedBlockHash, &updatedAt)
	if err != nil {
		return cp, notFound(err, "checkpoint "+chainID)
	}
	cp.LastFullyProcessedHeight = asU64(height)
	cp.UpdatedAt, err = parseTime(updatedAt)
	return cp, err
}

func (r checkpointRepo) Set(ctx context.Context, cp storage.ScannerCheckpoint) error {
	if cp.ChainID == "" {
		return invalidf("checkpoint: empty chain_id")
	}
	_, err := r.s.exec(ctx, `
		INSERT INTO scanner_checkpoints (chain_id, last_fully_processed_height, last_observed_block_hash, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (chain_id) DO UPDATE SET
			last_fully_processed_height = excluded.last_fully_processed_height,
			last_observed_block_hash = excluded.last_observed_block_hash,
			updated_at = excluded.updated_at`,
		cp.ChainID, u64(cp.LastFullyProcessedHeight), cp.LastObservedBlockHash, fmtTime(cp.UpdatedAt))
	return err
}

func requireOneRow(res sql.Result, what string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: %s: row changed concurrently", storage.ErrStatusConflict, what)
	}
	return nil
}
