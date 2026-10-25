package repository

import (
	"context"
	"database/sql"
	"errors"

	"mercatus/platform/database"
	"mercatus/reviews/model"
)

var (
	ErrDuplicate = errors.New("duplicate")
	ErrNotFound  = errors.New("not found")
)

type ReviewRepository struct {
	db *sql.DB
}

func NewReviewRepository(db *sql.DB) *ReviewRepository {
	return &ReviewRepository{db: db}
}

func (r *ReviewRepository) ProductReviewable(ctx context.Context, productID int64) (bool, error) {
	var ok bool
	err := r.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM products WHERE id = $1 AND status = 'active')`, productID).Scan(&ok)
	return ok, err
}

func (r *ReviewRepository) OwnsProduct(ctx context.Context, userID, productID int64) (bool, error) {
	var ok bool
	err := r.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM products p JOIN sellers s ON s.id = p.seller_id WHERE p.id = $1 AND s.user_id = $2)`,
		productID, userID).Scan(&ok)
	return ok, err
}

func (r *ReviewRepository) PurchasedProduct(ctx context.Context, userID, productID int64) (bool, error) {
	var ok bool
	err := r.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM orders o JOIN sub_orders so ON so.order_id = o.id
		 JOIN order_items oi ON oi.sub_order_id = so.id
		 WHERE o.buyer_id = $1 AND oi.product_id = $2 AND o.status = 'paid')`, userID, productID).Scan(&ok)
	return ok, err
}

func (r *ReviewRepository) Create(ctx context.Context, rv *model.Review) error {
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO reviews (product_id, user_id, rating, title, body, verified_purchase, helpful, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, 0, $7) RETURNING id`,
		rv.ProductID, rv.UserID, rv.Rating, rv.Title, rv.Body, rv.Verified, rv.CreatedAt,
	).Scan(&rv.ID)
	if database.IsUniqueViolation(err) {
		return ErrDuplicate
	}
	return err
}

func (r *ReviewRepository) ListForProduct(ctx context.Context, productID int64) ([]*model.Review, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT rv.id, rv.product_id, rv.user_id, COALESCE(u.username, 'Customer'), rv.rating, rv.title, rv.body,
		        rv.verified_purchase, rv.helpful, rv.created_at
		 FROM reviews rv LEFT JOIN users u ON u.id = rv.user_id
		 WHERE rv.product_id = $1 ORDER BY rv.helpful DESC, rv.created_at DESC LIMIT 100`, productID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.Review{}
	for rows.Next() {
		rv := &model.Review{}
		if err := rows.Scan(&rv.ID, &rv.ProductID, &rv.UserID, &rv.Author, &rv.Rating, &rv.Title,
			&rv.Body, &rv.Verified, &rv.Helpful, &rv.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, rv)
	}
	return out, rows.Err()
}

func (r *ReviewRepository) Summary(ctx context.Context, productID int64) (*model.RatingSummary, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT rating, COUNT(*) FROM reviews WHERE product_id = $1 GROUP BY rating`, productID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sum := &model.RatingSummary{ProductID: productID, Breakdown: map[int]int{1: 0, 2: 0, 3: 0, 4: 0, 5: 0}}
	var weighted, total int
	for rows.Next() {
		var rating, count int
		if err := rows.Scan(&rating, &count); err != nil {
			return nil, err
		}
		sum.Breakdown[rating] = count
		weighted += rating * count
		total += count
	}
	sum.Count = total
	if total > 0 {
		sum.Average = float64(weighted) / float64(total)
	}
	return sum, rows.Err()
}

func (r *ReviewRepository) Vote(ctx context.Context, reviewID, userID int64) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var author int64
	err = tx.QueryRowContext(ctx, `SELECT user_id FROM reviews WHERE id = $1`, reviewID).Scan(&author)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if author == userID {
		return ErrDuplicate
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO review_votes (review_id, user_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, reviewID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrDuplicate
	}
	if _, err := tx.ExecContext(ctx, `UPDATE reviews SET helpful = helpful + 1 WHERE id = $1`, reviewID); err != nil {
		return err
	}
	return tx.Commit()
}
