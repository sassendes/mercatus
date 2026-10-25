package repository

import (
	"context"
	"database/sql"
	"errors"

	"mercatus/platform/database"
	"mercatus/seller/model"
)

var (
	ErrAlreadySeller = errors.New("user is already a seller")
	ErrDuplicateSlug = errors.New("store slug taken")
	ErrStripeTaken   = errors.New("stripe account already linked")
)

type SellerRepository struct {
	db *sql.DB
}

func NewSellerRepository(db *sql.DB) *SellerRepository {
	return &SellerRepository{db: db}
}

func (r *SellerRepository) CreateAndPromote(ctx context.Context, s *model.Seller) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx,
		`INSERT INTO sellers (user_id, store_name, slug, description, status, commission_rate, stripe_account_id, payout_enabled, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, '', false, $7, $8) RETURNING id`,
		s.UserID, s.StoreName, s.Slug, s.Description, s.Status, s.CommissionRate, s.CreatedAt, s.UpdatedAt,
	).Scan(&s.ID)
	if database.IsUniqueViolation(err) {
		var exists bool
		_ = r.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sellers WHERE user_id = $1)`, s.UserID).Scan(&exists)
		if exists {
			return ErrAlreadySeller
		}
		return ErrDuplicateSlug
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET role = 'seller', updated_at = now() WHERE id = $1 AND role = 'buyer'`, s.UserID); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *SellerRepository) FindByUserID(ctx context.Context, userID int64) (*model.Seller, error) {
	return r.findBy(ctx, `user_id`, userID)
}

func (r *SellerRepository) FindByID(ctx context.Context, id int64) (*model.Seller, error) {
	return r.findBy(ctx, `id`, id)
}

func (r *SellerRepository) findBy(ctx context.Context, col string, val int64) (*model.Seller, error) {
	query := `SELECT id, user_id, store_name, slug, description, status, commission_rate, stripe_account_id, payout_enabled, created_at, updated_at
		FROM sellers WHERE ` + col + ` = $1`
	s := &model.Seller{}
	err := r.db.QueryRowContext(ctx, query, val).Scan(
		&s.ID, &s.UserID, &s.StoreName, &s.Slug, &s.Description, &s.Status,
		&s.CommissionRate, &s.StripeAccountID, &s.PayoutEnabled, &s.CreatedAt, &s.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (r *SellerRepository) SlugExists(ctx context.Context, slug string) (bool, error) {
	var exists bool
	err := r.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sellers WHERE slug = $1)`, slug).Scan(&exists)
	return exists, err
}

func (r *SellerRepository) ActivateWithStripe(ctx context.Context, id int64, acct string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE sellers SET stripe_account_id = $1, payout_enabled = true,
		        status = CASE WHEN status = 'suspended' THEN status ELSE 'active' END, updated_at = now()
		 WHERE id = $2`, acct, id)
	if database.IsUniqueViolation(err) {
		return ErrStripeTaken
	}
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
