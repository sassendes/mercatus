package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"mercatus/inventory/model"
)

var (
	ErrInsufficientStock = errors.New("insufficient stock")
	ErrReservationGone   = errors.New("reservation expired or released")
	ErrBelowReserved     = errors.New("on hand cannot be lower than reserved quantity")
)

type InsufficientStockError struct{ VariantID int64 }

func (e *InsufficientStockError) Error() string {
	return fmt.Sprintf("insufficient stock for variant %d", e.VariantID)
}
func (e *InsufficientStockError) Unwrap() error { return ErrInsufficientStock }

type InventoryRepository struct {
	db *sql.DB
}

func NewInventoryRepository(db *sql.DB) *InventoryRepository {
	return &InventoryRepository{db: db}
}

func (r *InventoryRepository) SellerOwnsVariant(ctx context.Context, userID, variantID int64) (bool, error) {
	var ok bool
	err := r.db.QueryRowContext(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM product_variants v
			JOIN products p ON p.id = v.product_id
			JOIN sellers s ON s.id = p.seller_id
			WHERE v.id = $1 AND s.user_id = $2)`, variantID, userID).Scan(&ok)
	return ok, err
}

func (r *InventoryRepository) SetStock(ctx context.Context, variantID int64, onHand int) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO stock (variant_id, on_hand, reserved, updated_at) VALUES ($1, 0, 0, now())
		 ON CONFLICT (variant_id) DO NOTHING`, variantID); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE stock SET on_hand = $2, updated_at = now() WHERE variant_id = $1 AND reserved <= $2`, variantID, onHand)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrBelowReserved
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE product_variants SET stock = $2, updated_at = now() WHERE id = $1`, variantID, onHand); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *InventoryRepository) GetStock(ctx context.Context, variantID int64) (*model.Stock, error) {
	s := &model.Stock{}
	err := r.db.QueryRowContext(ctx,
		`SELECT variant_id, on_hand, reserved, updated_at FROM stock WHERE variant_id = $1`, variantID,
	).Scan(&s.VariantID, &s.OnHand, &s.Reserved, &s.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s, nil
}

func tryReserve(ctx context.Context, ex interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, variantID int64, qty int) (bool, error) {
	res, err := ex.ExecContext(ctx,
		`UPDATE stock SET reserved = reserved + $1, updated_at = now()
		 WHERE variant_id = $2 AND on_hand - reserved >= $1`, qty, variantID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (r *InventoryRepository) ReserveOne(ctx context.Context, res *model.Reservation) error {
	return r.ReserveOrder(ctx, res.OrderID, []model.Line{{VariantID: res.VariantID, Quantity: res.Quantity}}, time.Until(res.ExpiresAt))
}

func (r *InventoryRepository) ReserveOrder(ctx context.Context, orderID int64, lines []model.Line, ttl time.Duration) error {
	merged := map[int64]int{}
	for _, l := range lines {
		merged[l.VariantID] += l.Quantity
	}
	ids := make([]int64, 0, len(merged))
	for id := range merged {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('reserve:' || $1::text, 0))`, orderID); err != nil {
		return err
	}
	var held int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM reservations WHERE order_id = $1 AND status = 'held'`, orderID).Scan(&held); err != nil {
		return err
	}
	if held > 0 {
		return nil
	}

	expires := time.Now().Add(ttl)
	for _, id := range ids {
		ok, err := tryReserve(ctx, tx, id, merged[id])
		if err != nil {
			return err
		}
		if !ok {
			return &InsufficientStockError{VariantID: id}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO reservations (variant_id, order_id, quantity, status, expires_at, created_at)
			 VALUES ($1, $2, $3, 'held', $4, now())`, id, orderID, merged[id], expires); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type heldRow struct {
	id        int64
	variantID int64
	qty       int
}

func lockByStatus(ctx context.Context, tx *sql.Tx, orderID int64, status string) ([]heldRow, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, variant_id, quantity FROM reservations
		 WHERE order_id = $1 AND status = $2 ORDER BY variant_id FOR UPDATE`, orderID, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []heldRow
	for rows.Next() {
		var h heldRow
		if err := rows.Scan(&h.id, &h.variantID, &h.qty); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func lockHeld(ctx context.Context, tx *sql.Tx, orderID int64) ([]heldRow, error) {
	return lockByStatus(ctx, tx, orderID, "held")
}

func (r *InventoryRepository) CommitOrder(ctx context.Context, orderID int64) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	held, err := lockHeld(ctx, tx, orderID)
	if err != nil {
		return err
	}
	for _, h := range held {
		var expired bool
		if err := tx.QueryRowContext(ctx,
			`SELECT expires_at <= now() FROM reservations WHERE id = $1`, h.id).Scan(&expired); err != nil {
			return err
		}
		if expired {
			return ErrReservationGone
		}
	}
	if len(held) == 0 {
		var committed int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM reservations WHERE order_id = $1 AND status = 'committed'`, orderID).Scan(&committed); err != nil {
			return err
		}
		if committed > 0 {
			return nil
		}
		return ErrReservationGone
	}
	for _, h := range held {
		if _, err := tx.ExecContext(ctx,
			`UPDATE stock SET on_hand = on_hand - $1, reserved = reserved - $1, updated_at = now() WHERE variant_id = $2`,
			h.qty, h.variantID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE product_variants SET stock = GREATEST(stock - $1, 0), updated_at = now() WHERE id = $2`,
			h.qty, h.variantID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE reservations SET status = 'committed' WHERE order_id = $1 AND status = 'held'`, orderID); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *InventoryRepository) ReleaseOrder(ctx context.Context, orderID int64) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	held, err := lockHeld(ctx, tx, orderID)
	if err != nil {
		return err
	}
	for _, h := range held {
		if _, err := tx.ExecContext(ctx,
			`UPDATE stock SET reserved = GREATEST(reserved - $1, 0), updated_at = now() WHERE variant_id = $2`,
			h.qty, h.variantID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE reservations SET status = 'released' WHERE order_id = $1 AND status = 'held'`, orderID); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *InventoryRepository) RestockOrder(ctx context.Context, orderID int64) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	committed, err := lockByStatus(ctx, tx, orderID, "committed")
	if err != nil {
		return err
	}
	for _, h := range committed {
		if _, err := tx.ExecContext(ctx,
			`UPDATE stock SET on_hand = on_hand + $1, updated_at = now() WHERE variant_id = $2`,
			h.qty, h.variantID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE product_variants SET stock = stock + $1, updated_at = now() WHERE id = $2`,
			h.qty, h.variantID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE reservations SET status = 'restocked' WHERE order_id = $1 AND status = 'committed'`, orderID); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *InventoryRepository) ExpireStaleReservations(ctx context.Context, now time.Time, limit int) (int, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx,
		`SELECT id, variant_id, quantity FROM reservations
		 WHERE status = 'held' AND expires_at < $1
		 ORDER BY id LIMIT $2 FOR UPDATE SKIP LOCKED`, now, limit)
	if err != nil {
		return 0, err
	}
	var stale []heldRow
	for rows.Next() {
		var h heldRow
		if err := rows.Scan(&h.id, &h.variantID, &h.qty); err != nil {
			rows.Close()
			return 0, err
		}
		stale = append(stale, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, h := range stale {
		if _, err := tx.ExecContext(ctx,
			`UPDATE stock SET reserved = GREATEST(reserved - $1, 0), updated_at = now() WHERE variant_id = $2`,
			h.qty, h.variantID); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE reservations SET status = 'released' WHERE id = $1`, h.id); err != nil {
			return 0, err
		}
	}
	return len(stale), tx.Commit()
}
