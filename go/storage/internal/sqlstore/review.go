package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

// reviewRepo persists review_items.
type reviewRepo struct{ s *Store }

const reviewCols = `id, chain_id, kind, ref_id, reason, opened_at, resolved_at, resolution`

func (r reviewRepo) Open(ctx context.Context, item storage.ReviewItem) (storage.ReviewItem, error) {
	if item.ChainID == "" || item.RefID == "" {
		return item, invalidf("review item: empty chain_id or ref_id")
	}
	if !item.Kind.Valid() {
		return item, invalidf("review item: unknown kind %q", item.Kind)
	}
	openedAt := fmtTime(item.OpenedAt)
	err := r.s.queryRow(ctx, `
		INSERT INTO review_items (chain_id, kind, ref_id, reason, opened_at, resolved_at, resolution)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		RETURNING id`,
		item.ChainID, string(item.Kind), item.RefID, item.Reason, openedAt,
		fmtTimePtr(item.ResolvedAt), item.Resolution,
	).Scan(&item.ID)
	if err != nil {
		return item, r.s.d.MapError(err)
	}
	item.OpenedAt, err = parseTime(openedAt)
	return item, err
}

func (r reviewRepo) Get(ctx context.Context, id int64) (storage.ReviewItem, error) {
	row := r.s.queryRow(ctx, `SELECT `+reviewCols+` FROM review_items WHERE id = ?`, id)
	item, err := scanReview(row)
	if err != nil {
		return item, notFound(err, fmt.Sprintf("review item %d", id))
	}
	return item, nil
}

func (r reviewRepo) ListOpen(ctx context.Context, chainID string, limit int) ([]storage.ReviewItem, error) {
	query := `SELECT ` + reviewCols + ` FROM review_items WHERE chain_id = ? AND resolved_at IS NULL ORDER BY id`
	if limit > 0 {
		query += ` LIMIT ` + strconv.Itoa(limit)
	}
	rows, err := r.s.query(ctx, query, chainID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storage.ReviewItem
	for rows.Next() {
		item, err := scanReview(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (r reviewRepo) Resolve(ctx context.Context, id int64, resolution string, at time.Time) error {
	res, err := r.s.exec(ctx, `UPDATE review_items SET resolved_at = ?, resolution = ?
		WHERE id = ? AND resolved_at IS NULL`,
		fmtTime(at), resolution, id)
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
	// Distinguish already-resolved from missing.
	if _, err := r.s.Review().Get(ctx, id); err != nil {
		return err
	}
	return fmt.Errorf("%w: review item %d already resolved", storage.ErrStatusConflict, id)
}

func scanReview(row rowScanner) (storage.ReviewItem, error) {
	var (
		item       storage.ReviewItem
		kind       string
		openedAt   string
		resolvedAt sql.NullString
	)
	err := row.Scan(&item.ID, &item.ChainID, &kind, &item.RefID, &item.Reason,
		&openedAt, &resolvedAt, &item.Resolution)
	if err != nil {
		return item, err
	}
	item.Kind = storage.ReviewItemKind(kind)
	if item.OpenedAt, err = parseTime(openedAt); err != nil {
		return item, err
	}
	item.ResolvedAt, err = parseTimePtr(resolvedAt)
	return item, err
}

// chainReviewRepo persists chain_review_conditions (FR-044).
type chainReviewRepo struct{ s *Store }

const chainReviewCols = `condition_id, chain_id, trigger_kind, node_a_observation,
	node_b_observation, opened_at, resolved_at, resolution`

func (r chainReviewRepo) Open(ctx context.Context, c storage.ChainReviewCondition) (storage.ChainReviewCondition, error) {
	if c.ConditionID == "" || c.ChainID == "" {
		return c, invalidf("chain review condition: empty condition_id or chain_id")
	}
	if !c.Trigger.Valid() {
		return c, invalidf("chain review condition: unknown trigger %q", c.Trigger)
	}
	nodeA, err := jsonText(c.NodeA)
	if err != nil {
		return c, err
	}
	nodeB, err := jsonText(c.NodeB)
	if err != nil {
		return c, err
	}
	openedAt := fmtTime(c.OpenedAt)
	err = r.s.withWrite(ctx, func(ctx context.Context, st *Store) error {
		if err := st.AcquireCreditGateLock(ctx, c.ChainID); err != nil {
			return err
		}
		_, err := st.exec(ctx, `
			INSERT INTO chain_review_conditions (condition_id, chain_id, trigger_kind,
				node_a_observation, node_b_observation, opened_at, resolved_at, resolution)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			c.ConditionID, c.ChainID, string(c.Trigger), nodeA, nodeB, openedAt,
			fmtTimePtr(c.ResolvedAt), c.Resolution)
		return err
	})
	if err != nil {
		return c, err
	}
	c.OpenedAt, err = parseTime(openedAt)
	return c, err
}

func (r chainReviewRepo) Get(ctx context.Context, conditionID string) (storage.ChainReviewCondition, error) {
	row := r.s.queryRow(ctx, `SELECT `+chainReviewCols+` FROM chain_review_conditions WHERE condition_id = ?`, conditionID)
	c, err := scanChainReview(row)
	if err != nil {
		return c, notFound(err, "chain review condition "+conditionID)
	}
	return c, nil
}

func (r chainReviewRepo) HasOpen(ctx context.Context, chainID string) (bool, error) {
	var n int
	err := r.s.queryRow(ctx, `SELECT COUNT(*) FROM chain_review_conditions
		WHERE chain_id = ? AND resolved_at IS NULL`, chainID).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (r chainReviewRepo) ListOpen(ctx context.Context, chainID string) ([]storage.ChainReviewCondition, error) {
	rows, err := r.s.query(ctx, `SELECT `+chainReviewCols+` FROM chain_review_conditions
		WHERE chain_id = ? AND resolved_at IS NULL ORDER BY opened_at, condition_id`, chainID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []storage.ChainReviewCondition
	for rows.Next() {
		c, err := scanChainReview(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r chainReviewRepo) Resolve(ctx context.Context, conditionID, resolution string, at time.Time) error {
	res, err := r.s.exec(ctx, `UPDATE chain_review_conditions SET resolved_at = ?, resolution = ?
		WHERE condition_id = ? AND resolved_at IS NULL`,
		fmtTime(at), resolution, conditionID)
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
	if _, err := r.Get(ctx, conditionID); err != nil {
		return err
	}
	return fmt.Errorf("%w: chain review condition %s already resolved", storage.ErrStatusConflict, conditionID)
}

func scanChainReview(row rowScanner) (storage.ChainReviewCondition, error) {
	var (
		c            storage.ChainReviewCondition
		trigger      string
		nodeA, nodeB string
		openedAt     string
		resolvedAt   sql.NullString
	)
	err := row.Scan(&c.ConditionID, &c.ChainID, &trigger, &nodeA, &nodeB,
		&openedAt, &resolvedAt, &c.Resolution)
	if err != nil {
		return c, err
	}
	c.Trigger = storage.ChainReviewTrigger(trigger)
	if err := unmarshalJSON(nodeA, &c.NodeA); err != nil {
		return c, err
	}
	if err := unmarshalJSON(nodeB, &c.NodeB); err != nil {
		return c, err
	}
	if c.OpenedAt, err = parseTime(openedAt); err != nil {
		return c, err
	}
	c.ResolvedAt, err = parseTimePtr(resolvedAt)
	return c, err
}

// unmarshalJSON decodes a JSON TEXT column.
func unmarshalJSON(s string, v any) error {
	if err := json.Unmarshal([]byte(s), v); err != nil {
		return fmt.Errorf("sqlstore: unmarshal %q: %w", s, err)
	}
	return nil
}
