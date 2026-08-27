package sqlstore

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

// reconRepo persists reconciliation_reports + reconciliation_entries.
type reconRepo struct{ s *Store }

func (r reconRepo) SaveReport(ctx context.Context, rep storage.ReconciliationReport) error {
	if rep.ReportID == "" || rep.ChainID == "" {
		return invalidf("reconciliation report: empty report_id or chain_id")
	}
	if !rep.Kind.Valid() {
		return invalidf("reconciliation report: unknown kind %q", rep.Kind)
	}
	return r.s.withWrite(ctx, func(ctx context.Context, st *Store) error {
		_, err := st.exec(ctx, `
			INSERT INTO reconciliation_reports (report_id, chain_id, kind, period_start,
				period_end, generated_at, discrepancy_count)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			rep.ReportID, rep.ChainID, string(rep.Kind), fmtTime(rep.PeriodStart),
			fmtTime(rep.PeriodEnd), fmtTime(rep.GeneratedAt), rep.DiscrepancyCount)
		if err != nil {
			return err
		}
		for i, e := range rep.Entries {
			expected, err := amountText("expected_base_units", e.ExpectedBaseUnits, false)
			if err != nil {
				return err
			}
			observed, err := amountText("observed_base_units", e.ObservedBaseUnits, false)
			if err != nil {
				return err
			}
			// Difference may legitimately be negative; serialize directly.
			if e.Difference.IsNil() {
				return invalidf("reconciliation entry %d: difference is unset", i)
			}
			hashes, err := jsonText(nonNilStrings(e.RelatedTxHashes))
			if err != nil {
				return err
			}
			if _, err := st.exec(ctx, `
				INSERT INTO reconciliation_entries (report_id, entry_index, address,
					expected_base_units, observed_base_units, difference,
					earliest_suspected_height, related_tx_hashes, recommended_rescan_height)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				rep.ReportID, i, e.Address, expected, observed, e.Difference.String(),
				u64(e.EarliestSuspectedHeight), hashes, u64(e.RecommendedRescanHeight)); err != nil {
				return err
			}
		}
		return nil
	})
}

func (r reconRepo) GetReport(ctx context.Context, reportID string) (storage.ReconciliationReport, error) {
	rep, err := r.scanReportRow(r.s.queryRow(ctx, `SELECT report_id, chain_id, kind, period_start,
		period_end, generated_at, discrepancy_count
		FROM reconciliation_reports WHERE report_id = ?`, reportID))
	if err != nil {
		return rep, notFound(err, "reconciliation report "+reportID)
	}
	if rep.Entries, err = r.loadEntries(ctx, reportID); err != nil {
		return rep, err
	}
	return rep, nil
}

func (r reconRepo) ListReports(ctx context.Context, chainID string, kind storage.ReconciliationKind, limit int) ([]storage.ReconciliationReport, error) {
	query := `SELECT report_id, chain_id, kind, period_start, period_end, generated_at, discrepancy_count
		FROM reconciliation_reports WHERE chain_id = ? AND kind = ?
		ORDER BY generated_at DESC, report_id`
	if limit > 0 {
		query += ` LIMIT ` + strconv.Itoa(limit)
	}
	rows, err := r.s.query(ctx, query, chainID, string(kind))
	if err != nil {
		return nil, err
	}
	var out []storage.ReconciliationReport
	for rows.Next() {
		rep, err := r.scanReportRow(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, rep)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close() // release before per-report entry loads (single-conn SQLite)
	for i := range out {
		if out[i].Entries, err = r.loadEntries(ctx, out[i].ReportID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (r reconRepo) scanReportRow(row rowScanner) (storage.ReconciliationReport, error) {
	var (
		rep                  storage.ReconciliationReport
		kind                 string
		start, end, genAt    string
	)
	err := row.Scan(&rep.ReportID, &rep.ChainID, &kind, &start, &end, &genAt, &rep.DiscrepancyCount)
	if err != nil {
		return rep, err
	}
	rep.Kind = storage.ReconciliationKind(kind)
	if rep.PeriodStart, err = parseTime(start); err != nil {
		return rep, err
	}
	if rep.PeriodEnd, err = parseTime(end); err != nil {
		return rep, err
	}
	rep.GeneratedAt, err = parseTime(genAt)
	return rep, err
}

func (r reconRepo) loadEntries(ctx context.Context, reportID string) ([]storage.ReconciliationEntry, error) {
	rows, err := r.s.query(ctx, `SELECT address, expected_base_units, observed_base_units,
		difference, earliest_suspected_height, related_tx_hashes, recommended_rescan_height
		FROM reconciliation_entries WHERE report_id = ? ORDER BY entry_index`, reportID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storage.ReconciliationEntry
	for rows.Next() {
		var (
			e                        storage.ReconciliationEntry
			expected, observed, diff string
			suspected, rescan        int64
			hashes                   string
		)
		if err := rows.Scan(&e.Address, &expected, &observed, &diff, &suspected, &hashes, &rescan); err != nil {
			return nil, err
		}
		if e.ExpectedBaseUnits, err = amountFromText(expected); err != nil {
			return nil, err
		}
		if e.ObservedBaseUnits, err = amountFromText(observed); err != nil {
			return nil, err
		}
		if e.Difference, err = amountFromText(diff); err != nil {
			return nil, err
		}
		if e.RelatedTxHashes, err = jsonStrings(hashes); err != nil {
			return nil, err
		}
		e.EarliestSuspectedHeight, e.RecommendedRescanHeight = asU64(suspected), asU64(rescan)
		out = append(out, e)
	}
	return out, rows.Err()
}

// outboxRepo persists the transactional outbox.
type outboxRepo struct{ s *Store }

func (r outboxRepo) Enqueue(ctx context.Context, ev storage.OutboxEvent) (storage.OutboxEvent, error) {
	if ev.ChainID == "" || ev.Topic == "" {
		return ev, invalidf("outbox event: empty chain_id or topic")
	}
	payload := ev.Payload
	if payload == nil {
		payload = []byte{}
	}
	createdAt := fmtTime(ev.CreatedAt)
	err := r.s.queryRow(ctx, `
		INSERT INTO outbox (chain_id, topic, dedup_key, payload, created_at, dispatched_at)
		VALUES (?, ?, ?, ?, ?, ?)
		RETURNING id`,
		ev.ChainID, ev.Topic, nullIfEmpty(ev.DedupKey), payload, createdAt,
		fmtTimePtr(ev.DispatchedAt),
	).Scan(&ev.ID)
	if err != nil {
		return ev, r.s.d.MapError(err)
	}
	ev.CreatedAt, err = parseTime(createdAt)
	return ev, err
}

func (r outboxRepo) ListPending(ctx context.Context, limit int) ([]storage.OutboxEvent, error) {
	query := `SELECT id, chain_id, topic, dedup_key, payload, created_at, dispatched_at
		FROM outbox WHERE dispatched_at IS NULL ORDER BY id`
	if limit > 0 {
		query += ` LIMIT ` + strconv.Itoa(limit)
	}
	rows, err := r.s.query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storage.OutboxEvent
	for rows.Next() {
		var (
			ev                    storage.OutboxEvent
			dedup                 sql.NullString
			createdAt             string
			dispatchedAt          sql.NullString
		)
		if err := rows.Scan(&ev.ID, &ev.ChainID, &ev.Topic, &dedup, &ev.Payload, &createdAt, &dispatchedAt); err != nil {
			return nil, err
		}
		if dedup.Valid {
			ev.DedupKey = dedup.String
		}
		if ev.CreatedAt, err = parseTime(createdAt); err != nil {
			return nil, err
		}
		if ev.DispatchedAt, err = parseTimePtr(dispatchedAt); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

func (r outboxRepo) MarkDispatched(ctx context.Context, id int64, at time.Time) error {
	res, err := r.s.exec(ctx, `UPDATE outbox SET dispatched_at = ? WHERE id = ? AND dispatched_at IS NULL`,
		fmtTime(at), id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 1 {
		return nil
	}
	var exists int
	if err := r.s.queryRow(ctx, `SELECT COUNT(*) FROM outbox WHERE id = ?`, id).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return fmt.Errorf("%w: outbox event %d", storage.ErrNotFound, id)
	}
	return fmt.Errorf("%w: outbox event %d already dispatched", storage.ErrStatusConflict, id)
}
