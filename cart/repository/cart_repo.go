package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"mercatus/cart/model"
)

var ErrNotFound = errors.New("not found")

type CartRepository struct {
	db *sql.DB
}

func NewCartRepository(db *sql.DB) *CartRepository {
	return &CartRepository{db: db}
}

func (r *CartRepository) GetOrCreateCart(ctx context.Context, userID int64) (*model.Cart, error) {
	c := &model.Cart{}
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO carts (user_id, created_at, updated_at) VALUES ($1, now(), now())
		 ON CONFLICT (user_id) DO UPDATE SET user_id = EXCLUDED.user_id
		 RETURNING id, user_id, created_at, updated_at`, userID,
	).Scan(&c.ID, &c.UserID, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (r *CartRepository) VariantInfo(ctx context.Context, variantID int64) (*model.VariantInfo, error) {
	v := &model.VariantInfo{VariantID: variantID}
	err := r.db.QueryRowContext(ctx,
		`SELECT p.seller_id, s.user_id, v.price, v.currency, COALESCE(GREATEST(st.on_hand - st.reserved, 0), 0)
		 FROM product_variants v
		 JOIN products p ON p.id = v.product_id AND p.status = 'active'
		 JOIN sellers s ON s.id = p.seller_id AND s.status = 'active'
		 LEFT JOIN stock st ON st.variant_id = v.id
		 WHERE v.id = $1 AND v.active`, variantID,
	).Scan(&v.SellerID, &v.SellerUserID, &v.Price, &v.Currency, &v.Available)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return v, nil
}

func (r *CartRepository) LoadItems(ctx context.Context, cartID int64) ([]*model.CartItem, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT ci.id, ci.cart_id, ci.variant_id, p.id, p.seller_id, s.store_name, p.title, p.slug, p.image_url, COALESCE(c.slug, ''),
		        v.sku, v.attributes, ci.quantity, v.price, v.currency,
		        COALESCE(GREATEST(st.on_hand - st.reserved, 0), 0),
		        (p.status = 'active' AND s.status = 'active' AND v.active), ci.created_at
		 FROM cart_items ci
		 JOIN product_variants v ON v.id = ci.variant_id
		 JOIN products p ON p.id = v.product_id
		 JOIN sellers s ON s.id = p.seller_id
		 LEFT JOIN categories c ON c.id = p.category_id
		 LEFT JOIN stock st ON st.variant_id = v.id
		 WHERE ci.cart_id = $1 ORDER BY ci.id`, cartID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []*model.CartItem{}
	for rows.Next() {
		it := &model.CartItem{}
		var attrs []byte
		if err := rows.Scan(&it.ID, &it.CartID, &it.VariantID, &it.ProductID, &it.SellerID, &it.StoreName, &it.Title,
			&it.Slug, &it.ImageURL, &it.Category, &it.SKU, &attrs, &it.Quantity, &it.UnitPrice, &it.Currency, &it.Available,
			&it.Purchasable, &it.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(attrs, &it.Attributes)
		items = append(items, it)
	}
	return items, rows.Err()
}

func (r *CartRepository) CurrentQuantity(ctx context.Context, cartID, variantID int64) (int, error) {
	var q int
	err := r.db.QueryRowContext(ctx,
		`SELECT quantity FROM cart_items WHERE cart_id = $1 AND variant_id = $2`, cartID, variantID).Scan(&q)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return q, err
}

func (r *CartRepository) SetItem(ctx context.Context, cartID int64, v *model.VariantInfo, qty int) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO cart_items (cart_id, variant_id, seller_id, quantity, unit_price, currency, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, now())
		 ON CONFLICT (cart_id, variant_id)
		 DO UPDATE SET quantity = EXCLUDED.quantity, unit_price = EXCLUDED.unit_price, seller_id = EXCLUDED.seller_id`,
		cartID, v.VariantID, v.SellerID, qty, v.Price, v.Currency)
	if err != nil {
		return err
	}
	_, err = r.db.ExecContext(ctx, `UPDATE carts SET updated_at = now() WHERE id = $1`, cartID)
	return err
}

func (r *CartRepository) RemoveItem(ctx context.Context, cartID, variantID int64) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM cart_items WHERE cart_id = $1 AND variant_id = $2`, cartID, variantID)
	return err
}

func (r *CartRepository) Clear(ctx context.Context, cartID int64) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM cart_items WHERE cart_id = $1`, cartID)
	return err
}
