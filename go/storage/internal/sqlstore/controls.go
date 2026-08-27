package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"

	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

// watchRepo persists watched_addresses.
type watchRepo struct{ s *Store }

func (r watchRepo) Upsert(ctx context.Context, w storage.WatchedAddress) error {
	if w.ChainID == "" || w.Address == "" {
		return invalidf("watched address: empty chain_id or address")
	}
	if !w.Kind.Valid() {
		return invalidf("watched address: unknown kind %q", w.Kind)
	}
	_, err := r.s.exec(ctx, `
		INSERT INTO watched_addresses (chain_id, address, kind, customer_ref, memo_required, active)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (chain_id, address) DO UPDATE SET
			kind = excluded.kind,
			customer_ref = excluded.customer_ref,
			memo_required = excluded.memo_required,
			active = excluded.active`,
		w.ChainID, w.Address, string(w.Kind), w.CustomerRef, w.MemoRequired, w.Active)
	return err
}

func (r watchRepo) Get(ctx context.Context, chainID, address string) (storage.WatchedAddress, error) {
	var (
		w    storage.WatchedAddress
		kind string
	)
	err := r.s.queryRow(ctx, `SELECT chain_id, address, kind, customer_ref, memo_required, active
		FROM watched_addresses WHERE chain_id = ? AND address = ?`, chainID, address,
	).Scan(&w.ChainID, &w.Address, &kind, &w.CustomerRef, &w.MemoRequired, &w.Active)
	if err != nil {
		return w, notFound(err, "watched address "+chainID+"/"+address)
	}
	w.Kind = storage.WatchedAddressKind(kind)
	return w, nil
}

func (r watchRepo) ListActive(ctx context.Context, chainID string) ([]storage.WatchedAddress, error) {
	rows, err := r.s.query(ctx, `SELECT chain_id, address, kind, customer_ref, memo_required, active
		FROM watched_addresses WHERE chain_id = ? AND active ORDER BY address`, chainID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storage.WatchedAddress
	for rows.Next() {
		var (
			w    storage.WatchedAddress
			kind string
		)
		if err := rows.Scan(&w.ChainID, &w.Address, &kind, &w.CustomerRef, &w.MemoRequired, &w.Active); err != nil {
			return nil, err
		}
		w.Kind = storage.WatchedAddressKind(kind)
		out = append(out, w)
	}
	return out, rows.Err()
}

func (r watchRepo) SetActive(ctx context.Context, chainID, address string, active bool) error {
	res, err := r.s.exec(ctx, `UPDATE watched_addresses SET active = ? WHERE chain_id = ? AND address = ?`,
		active, chainID, address)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return notFound(sql.ErrNoRows, "watched address "+chainID+"/"+address)
	}
	return nil
}

// controlsRepo persists operational_controls + controls_audit.
type controlsRepo struct{ s *Store }

func (r controlsRepo) Get(ctx context.Context, chainID string) (storage.OperationalControls, error) {
	c, err := getControls(ctx, r.s, chainID, false)
	if err != nil && errors.Is(err, storage.ErrNotFound) {
		// Controls default to running (all-false zero value).
		return storage.OperationalControls{ChainID: chainID}, nil
	}
	return c, err
}

func getControls(ctx context.Context, st *Store, chainID string, lock bool) (storage.OperationalControls, error) {
	var (
		c         storage.OperationalControls
		resume    sql.NullInt64
		updatedAt string
	)
	query := `SELECT chain_id, credit_paused, signing_paused, broadcast_paused, sweep_paused,
		scan_without_credit, resume_from_height, updated_at
		FROM operational_controls WHERE chain_id = ?`
	if lock {
		query += st.d.RowLock()
	}
	err := st.queryRow(ctx, query, chainID).Scan(&c.ChainID, &c.CreditPaused, &c.SigningPaused,
		&c.BroadcastPaused, &c.SweepPaused, &c.ScanWithoutCredit, &resume, &updatedAt)
	if err != nil {
		return c, notFound(err, "operational controls "+chainID)
	}
	c.ResumeFromHeight = asU64Ptr(resume)
	c.UpdatedAt, err = parseTime(updatedAt)
	return c, err
}

// controlChange is one audited field flip.
type controlChange struct {
	field    string
	old, new string
}

func (r controlsRepo) Apply(ctx context.Context, chainID string, u storage.ControlsUpdate, actor, reason string) (storage.OperationalControls, error) {
	if chainID == "" {
		return storage.OperationalControls{}, invalidf("operational controls: empty chain_id")
	}
	var out storage.OperationalControls
	err := r.s.withWrite(ctx, func(ctx context.Context, st *Store) error {
		if err := st.AcquireCreditGateLock(ctx, chainID); err != nil {
			return err
		}
		cur, err := getControls(ctx, st, chainID, true)
		if err != nil {
			if !errors.Is(err, storage.ErrNotFound) {
				return err
			}
			cur = storage.OperationalControls{ChainID: chainID}
		}

		var changes []controlChange
		applyBool := func(field string, dst *bool, src *bool) {
			if src != nil && *src != *dst {
				changes = append(changes, controlChange{field, strconv.FormatBool(*dst), strconv.FormatBool(*src)})
				*dst = *src
			}
		}
		applyBool("credit_paused", &cur.CreditPaused, u.CreditPaused)
		applyBool("signing_paused", &cur.SigningPaused, u.SigningPaused)
		applyBool("broadcast_paused", &cur.BroadcastPaused, u.BroadcastPaused)
		applyBool("sweep_paused", &cur.SweepPaused, u.SweepPaused)
		applyBool("scan_without_credit", &cur.ScanWithoutCredit, u.ScanWithoutCredit)

		// ClearResumeFromHeight wins over ResumeFromHeight.
		heightStr := func(v *uint64) string {
			if v == nil {
				return ""
			}
			return strconv.FormatUint(*v, 10)
		}
		var target *uint64
		hasTarget := false
		switch {
		case u.ClearResumeFromHeight:
			target, hasTarget = nil, true
		case u.ResumeFromHeight != nil:
			target, hasTarget = u.ResumeFromHeight, true
		}
		if hasTarget && heightStr(target) != heightStr(cur.ResumeFromHeight) {
			changes = append(changes, controlChange{"resume_from_height", heightStr(cur.ResumeFromHeight), heightStr(target)})
			cur.ResumeFromHeight = target
		}

		if len(changes) == 0 {
			out = cur
			return nil
		}

		now := fmtTime(time.Time{})
		if _, err := st.exec(ctx, `
			INSERT INTO operational_controls (chain_id, credit_paused, signing_paused,
				broadcast_paused, sweep_paused, scan_without_credit, resume_from_height, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (chain_id) DO UPDATE SET
				credit_paused = excluded.credit_paused,
				signing_paused = excluded.signing_paused,
				broadcast_paused = excluded.broadcast_paused,
				sweep_paused = excluded.sweep_paused,
				scan_without_credit = excluded.scan_without_credit,
				resume_from_height = excluded.resume_from_height,
				updated_at = excluded.updated_at`,
			chainID, cur.CreditPaused, cur.SigningPaused, cur.BroadcastPaused,
			cur.SweepPaused, cur.ScanWithoutCredit, u64Ptr(cur.ResumeFromHeight), now); err != nil {
			return err
		}
		for _, ch := range changes {
			if _, err := st.exec(ctx, `
				INSERT INTO controls_audit (chain_id, field, old_value, new_value, actor, reason, occurred_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)`,
				chainID, ch.field, ch.old, ch.new, actor, reason, now); err != nil {
				return err
			}
		}
		if cur.UpdatedAt, err = parseTime(now); err != nil {
			return err
		}
		out = cur
		return nil
	})
	return out, err
}

func (r controlsRepo) ListAudit(ctx context.Context, chainID string, limit int) ([]storage.ControlsAuditEntry, error) {
	query := `SELECT id, chain_id, field, old_value, new_value, actor, reason, occurred_at
		FROM controls_audit WHERE chain_id = ? ORDER BY id DESC`
	if limit > 0 {
		query += ` LIMIT ` + strconv.Itoa(limit)
	}
	rows, err := r.s.query(ctx, query, chainID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storage.ControlsAuditEntry
	for rows.Next() {
		var (
			e  storage.ControlsAuditEntry
			at string
		)
		if err := rows.Scan(&e.ID, &e.ChainID, &e.Field, &e.OldValue, &e.NewValue, &e.Actor, &e.Reason, &at); err != nil {
			return nil, err
		}
		if e.At, err = parseTime(at); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
