package repository

import (
	"context"
	"database/sql"
	"errors"

	"mercatus/admin/model"
)

var ErrTargetNotFound = errors.New("target not found")

type AdminRepository struct{ db *sql.DB }

func NewAdminRepository(db *sql.DB) *AdminRepository { return &AdminRepository{db: db} }

var effects = map[string]map[string]string{
	"product": {
		"remove":    `UPDATE products SET status = 'removed', updated_at = now() WHERE id = $1`,
		"reinstate": `UPDATE products SET status = 'active', updated_at = now() WHERE id = $1 AND status = 'removed'`,
	},
	"seller": {
		"suspend":   `UPDATE sellers SET status = 'suspended', updated_at = now() WHERE id = $1`,
		"reinstate": `UPDATE sellers SET status = CASE WHEN payout_enabled THEN 'active' ELSE 'pending' END, updated_at = now() WHERE id = $1 AND status = 'suspended'`,
	},
	"review": {
		"remove": `DELETE FROM reviews WHERE id = $1`,
	},
	"buyer": {
		"block": `INSERT INTO buyer_blocks (user_id, created_at) SELECT id, now() FROM users WHERE id = $1
			ON CONFLICT (user_id) DO UPDATE SET created_at = buyer_blocks.created_at`,
		"unblock": `DELETE FROM buyer_blocks WHERE user_id = $1`,
	},
}

func Supported(target, action string) bool {
	_, ok := effects[target][action]
	return ok
}

func (r *AdminRepository) Apply(ctx context.Context, a *model.ModerationAction) error {
	stmt, ok := effects[a.TargetType][a.Action]
	if !ok {
		return errors.New("unsupported moderation action")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, stmt, a.TargetID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrTargetNotFound
	}
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO moderation_actions (admin_id, target_type, target_id, action, reason, created_at)
		 VALUES ($1, $2, $3, $4, $5, now()) RETURNING id, created_at`,
		a.AdminID, a.TargetType, a.TargetID, a.Action, a.Reason,
	).Scan(&a.ID, &a.CreatedAt); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *AdminRepository) ListActions(ctx context.Context, limit int) ([]*model.ModerationAction, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, admin_id, target_type, target_id, action, reason, created_at
		 FROM moderation_actions ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.ModerationAction{}
	for rows.Next() {
		a := &model.ModerationAction{}
		if err := rows.Scan(&a.ID, &a.AdminID, &a.TargetType, &a.TargetID, &a.Action, &a.Reason, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (r *AdminRepository) Stats(ctx context.Context) (*model.PlatformStats, error) {
	s := &model.PlatformStats{}
	err := r.db.QueryRowContext(ctx, `
		SELECT
			(SELECT count(*) FROM users),
			(SELECT count(*) FROM sellers WHERE status = 'active'),
			(SELECT count(*) FROM sellers WHERE status = 'suspended'),
			(SELECT count(*) FROM products WHERE status = 'active'),
			(SELECT count(*) FROM products WHERE status = 'removed'),
			(SELECT count(*) FROM orders WHERE status = 'paid'),
			(SELECT COALESCE(sum(total_cents), 0) FROM orders WHERE status = 'paid'),
			(SELECT COALESCE(sum(so.commission_cents), 0) FROM sub_orders so JOIN orders o ON o.id = so.order_id WHERE o.status = 'paid'),
			(SELECT count(*) FROM moderation_actions)`,
	).Scan(&s.Users, &s.ActiveSellers, &s.SuspendedSellers, &s.ActiveProducts, &s.RemovedProducts,
		&s.PaidOrders, &s.GMVCents, &s.CommissionCents, &s.TotalActions)
	return s, err
}
