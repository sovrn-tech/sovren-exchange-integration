package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

type sequenceRepo struct{ s *Store }

const sequenceCols = `id, chain_id, source_address, account_number, sequence,
	work_kind, work_id, status, created_at, updated_at`

func (r sequenceRepo) Reserve(ctx context.Context, res storage.SequenceReservation) (storage.SequenceReservation, error) {
	if res.ChainID == "" || res.SourceAddress == "" {
		return res, invalidf("sequence reservation: empty chain_id or source_address")
	}
	if !res.WorkRef.Kind.Valid() || res.WorkRef.ID == "" {
		return res, invalidf("sequence reservation: invalid work_ref %+v", res.WorkRef)
	}
	res.Status = storage.SequenceReserved

	err := r.s.withWrite(ctx, func(ctx context.Context, st *Store) error {
		// Per-account serialization (R7): Postgres locks the
		// chain_account_locks row; SQLite's single-writer connection +
		// BEGIN IMMEDIATE already serializes globally.
		if err := st.d.AcquireAccountLock(ctx, st.q, res.ChainID, res.SourceAddress); err != nil {
			return err
		}

		var (
			existingID     int64
			existingStatus string
			existingCA     string
		)
		err := st.queryRow(ctx, `SELECT id, status, created_at FROM sequence_reservations
			WHERE chain_id = ? AND source_address = ? AND sequence = ?`+st.d.RowLock(),
			res.ChainID, res.SourceAddress, u64(res.Sequence),
		).Scan(&existingID, &existingStatus, &existingCA)
		switch {
		case err != nil && !errors.Is(err, sql.ErrNoRows):
			return err
		case err == nil:
			// A RELEASED slot is reclaimable (new work ref, back to
			// RESERVED); any live slot is a duplicate.
			if existingStatus != string(storage.SequenceReleased) {
				return fmt.Errorf("%w: sequence %s/%s/%d already reserved (status %s)",
					storage.ErrDuplicate, res.ChainID, res.SourceAddress, res.Sequence, existingStatus)
			}
			updatedAt := fmtTime(time.Time{})
			upd, execErr := st.exec(ctx, `UPDATE sequence_reservations
				SET account_number = ?, work_kind = ?, work_id = ?, status = ?, updated_at = ?
				WHERE id = ? AND status = ?`,
				u64(res.AccountNumber), string(res.WorkRef.Kind), res.WorkRef.ID,
				string(storage.SequenceReserved), updatedAt, existingID, string(storage.SequenceReleased))
			if execErr != nil {
				return execErr
			}
			if reqErr := requireOneRow(upd, "sequence reservation reclaim"); reqErr != nil {
				return reqErr
			}
			res.ID = existingID
			if res.CreatedAt, err = parseTime(existingCA); err != nil {
				return err
			}
			res.UpdatedAt, err = parseTime(updatedAt)
			return err
		}

		createdAt := fmtTime(res.CreatedAt)
		updatedAt := fmtTime(res.UpdatedAt)
		err = st.queryRow(ctx, `
			INSERT INTO sequence_reservations (chain_id, source_address, account_number,
				sequence, work_kind, work_id, status, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			RETURNING id`,
			res.ChainID, res.SourceAddress, u64(res.AccountNumber), u64(res.Sequence),
			string(res.WorkRef.Kind), res.WorkRef.ID, string(storage.SequenceReserved),
			createdAt, updatedAt,
		).Scan(&res.ID)
		if err != nil {
			return st.d.MapError(err)
		}
		if res.CreatedAt, err = parseTime(createdAt); err != nil {
			return err
		}
		res.UpdatedAt, err = parseTime(updatedAt)
		return err
	})
	return res, err
}

func (r sequenceRepo) GetByWorkRef(ctx context.Context, ref storage.WorkRef) (storage.SequenceReservation, error) {
	row := r.s.queryRow(ctx, `SELECT `+sequenceCols+` FROM sequence_reservations
		WHERE work_kind = ? AND work_id = ?`, string(ref.Kind), ref.ID)
	res, err := scanSequence(row)
	if err != nil {
		return res, notFound(err, fmt.Sprintf("sequence reservation for %s/%s", ref.Kind, ref.ID))
	}
	return res, nil
}

func (r sequenceRepo) ListUnconsumed(ctx context.Context, chainID, sourceAddress string) ([]storage.SequenceReservation, error) {
	rows, err := r.s.query(ctx, `SELECT `+sequenceCols+` FROM sequence_reservations
		WHERE chain_id = ? AND source_address = ?
		AND status NOT IN (`+statusList([]storage.SequenceReservationStatus{
		storage.SequenceConsumed, storage.SequenceReleased,
	})+`) ORDER BY sequence`, chainID, sourceAddress)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storage.SequenceReservation
	for rows.Next() {
		res, err := scanSequence(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, res)
	}
	return out, rows.Err()
}

func (r sequenceRepo) UpdateStatus(ctx context.Context, id int64, from, to storage.SequenceReservationStatus) error {
	if err := storage.ValidateSequenceTransition(from, to); err != nil {
		return err
	}
	return r.s.withWrite(ctx, func(ctx context.Context, st *Store) error {
		var stored string
		err := st.queryRow(ctx,
			`SELECT status FROM sequence_reservations WHERE id = ?`+st.d.RowLock(), id,
		).Scan(&stored)
		if err != nil {
			return notFound(err, fmt.Sprintf("sequence reservation id %d", id))
		}
		if stored != string(from) {
			return fmt.Errorf("%w: sequence reservation %d is %s, expected %s", storage.ErrStatusConflict, id, stored, from)
		}
		res, err := st.exec(ctx, `UPDATE sequence_reservations SET status = ?, updated_at = ?
			WHERE id = ? AND status = ?`,
			string(to), fmtTime(time.Time{}), id, string(from))
		if err != nil {
			return err
		}
		return requireOneRow(res, fmt.Sprintf("sequence reservation %d %s -> %s", id, from, to))
	})
}

func scanSequence(row rowScanner) (storage.SequenceReservation, error) {
	var (
		res                        storage.SequenceReservation
		acctNum, seq               int64
		kind, status               string
		createdAt, updatedAt       string
	)
	err := row.Scan(&res.ID, &res.ChainID, &res.SourceAddress, &acctNum, &seq,
		&kind, &res.WorkRef.ID, &status, &createdAt, &updatedAt)
	if err != nil {
		return res, err
	}
	res.AccountNumber, res.Sequence = asU64(acctNum), asU64(seq)
	res.WorkRef.Kind = storage.WorkKind(kind)
	res.Status = storage.SequenceReservationStatus(status)
	if res.CreatedAt, err = parseTime(createdAt); err != nil {
		return res, err
	}
	res.UpdatedAt, err = parseTime(updatedAt)
	return res, err
}
