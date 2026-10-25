package repository

import (
	"context"
	"database/sql"
	"errors"

	"mercatus/order/model"

	"github.com/lib/pq"
)

var ErrInvalidTransition = errors.New("invalid order status transition")

type OrderRepository struct {
	db *sql.DB
}

func NewOrderRepository(db *sql.DB) *OrderRepository {
	return &OrderRepository{db: db}
}

func (r *OrderRepository) PriceLines(ctx context.Context, variantIDs []int64) (map[int64]*model.PricedLine, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT v.id, p.id, p.seller_id, s.user_id, p.title, v.price, v.currency, s.commission_rate,
		        (v.active AND p.status = 'active' AND s.status = 'active')
		 FROM product_variants v
		 JOIN products p ON p.id = v.product_id
		 JOIN sellers s ON s.id = p.seller_id
		 WHERE v.id = ANY($1)`, pq.Array(variantIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]*model.PricedLine{}
	for rows.Next() {
		l := &model.PricedLine{}
		if err := rows.Scan(&l.VariantID, &l.ProductID, &l.SellerID, &l.SellerUserID, &l.Title, &l.UnitPriceCents, &l.Currency,
			&l.CommissionBps, &l.Purchasable); err != nil {
			return nil, err
		}
		out[l.VariantID] = l
	}
	return out, rows.Err()
}

func (r *OrderRepository) CreateOrder(ctx context.Context, o *model.Order) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err = tx.QueryRowContext(ctx,
		`INSERT INTO orders (buyer_id, status, total_cents, currency, created_at)
		 VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		o.BuyerID, o.Status, o.TotalCents, o.Currency, o.CreatedAt,
	).Scan(&o.ID); err != nil {
		return err
	}
	for i := range o.SubOrders {
		so := &o.SubOrders[i]
		so.OrderID = o.ID
		if err = tx.QueryRowContext(ctx,
			`INSERT INTO sub_orders (order_id, seller_id, status, subtotal_cents, commission_cents, currency)
			 VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
			so.OrderID, so.SellerID, so.Status, so.SubtotalCents, so.CommissionCents, so.Currency,
		).Scan(&so.ID); err != nil {
			return err
		}
		for j := range so.Items {
			it := &so.Items[j]
			it.SubOrderID = so.ID
			if err = tx.QueryRowContext(ctx,
				`INSERT INTO order_items (sub_order_id, variant_id, product_id, title, quantity, unit_price_cents, currency)
				 VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
				it.SubOrderID, it.VariantID, it.ProductID, it.Title, it.Quantity, it.UnitPriceCents, it.Currency,
			).Scan(&it.ID); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (r *OrderRepository) GetOrder(ctx context.Context, orderID, buyerID int64) (*model.Order, error) {
	o := &model.Order{}
	err := r.db.QueryRowContext(ctx,
		`SELECT id, buyer_id, status, total_cents, currency, created_at
		 FROM orders WHERE id = $1 AND buyer_id = $2`, orderID, buyerID,
	).Scan(&o.ID, &o.BuyerID, &o.Status, &o.TotalCents, &o.Currency, &o.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT so.id, so.order_id, so.seller_id, COALESCE(s.store_name, ''), so.status, so.subtotal_cents,
		        so.commission_cents, so.currency,
		        oi.id, oi.variant_id, oi.product_id, COALESCE(NULLIF(oi.title, ''), p.title, ''), COALESCE(p.slug, ''),
		        COALESCE(p.image_url, ''), COALESCE(c.slug, ''), oi.quantity, oi.unit_price_cents, oi.currency
		 FROM sub_orders so
		 LEFT JOIN sellers s ON s.id = so.seller_id
		 JOIN order_items oi ON oi.sub_order_id = so.id
		 LEFT JOIN products p ON p.id = oi.product_id
		 LEFT JOIN categories c ON c.id = p.category_id
		 WHERE so.order_id = $1 ORDER BY so.id, oi.id`, o.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	index := map[int64]int{}
	for rows.Next() {
		var so model.SubOrder
		var it model.OrderItem
		if err := rows.Scan(&so.ID, &so.OrderID, &so.SellerID, &so.StoreName, &so.Status, &so.SubtotalCents,
			&so.CommissionCents, &so.Currency, &it.ID, &it.VariantID, &it.ProductID, &it.Title, &it.Slug,
			&it.ImageURL, &it.Category, &it.Quantity, &it.UnitPriceCents, &it.Currency); err != nil {
			return nil, err
		}
		it.SubOrderID = so.ID
		pos, ok := index[so.ID]
		if !ok {
			o.SubOrders = append(o.SubOrders, so)
			pos = len(o.SubOrders) - 1
			index[so.ID] = pos
		}
		o.SubOrders[pos].Items = append(o.SubOrders[pos].Items, it)
		o.ItemCount += it.Quantity
	}
	return o, rows.Err()
}

func (r *OrderRepository) ListOrdersForBuyer(ctx context.Context, buyerID int64) ([]*model.Order, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT o.id, o.buyer_id, o.status, o.total_cents, o.currency, o.created_at,
		        COALESCE((SELECT sum(oi.quantity) FROM sub_orders so JOIN order_items oi ON oi.sub_order_id = so.id WHERE so.order_id = o.id), 0)::int
		 FROM orders o WHERE o.buyer_id = $1 ORDER BY o.created_at DESC LIMIT 100`, buyerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.Order{}
	ids := []int64{}
	byID := map[int64]*model.Order{}
	for rows.Next() {
		o := &model.Order{}
		if err := rows.Scan(&o.ID, &o.BuyerID, &o.Status, &o.TotalCents, &o.Currency, &o.CreatedAt, &o.ItemCount); err != nil {
			return nil, err
		}
		out = append(out, o)
		ids = append(ids, o.ID)
		byID[o.ID] = o
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return out, nil
	}
	prev, err := r.db.QueryContext(ctx,
		`SELECT so.order_id, COALESCE(NULLIF(oi.title, ''), p.title, ''), COALESCE(p.image_url, ''), COALESCE(p.slug, ''), COALESCE(c.slug, '')
		 FROM sub_orders so JOIN order_items oi ON oi.sub_order_id = so.id
		 LEFT JOIN products p ON p.id = oi.product_id
		 LEFT JOIN categories c ON c.id = p.category_id
		 WHERE so.order_id = ANY($1) ORDER BY so.order_id, oi.id`, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	defer prev.Close()
	for prev.Next() {
		var id int64
		var p model.Preview
		if err := prev.Scan(&id, &p.Title, &p.ImageURL, &p.Slug, &p.Category); err != nil {
			return nil, err
		}
		if o := byID[id]; o != nil && len(o.Preview) < 4 {
			o.Preview = append(o.Preview, p)
		}
	}
	return out, prev.Err()
}

func (r *OrderRepository) SellerIDForUser(ctx context.Context, userID int64) (int64, error) {
	var id int64
	err := r.db.QueryRowContext(ctx, `SELECT id FROM sellers WHERE user_id = $1`, userID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

func (r *OrderRepository) ListSales(ctx context.Context, sellerID int64) ([]*model.SubOrder, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT so.id, so.order_id, so.seller_id, so.status, so.subtotal_cents, so.commission_cents, so.currency, o.created_at,
		        COALESCE(u.email, ''),
		        COALESCE((SELECT string_agg(oi.quantity || ' x ' || oi.title, ', ' ORDER BY oi.id) FROM order_items oi WHERE oi.sub_order_id = so.id), '')
		 FROM sub_orders so JOIN orders o ON o.id = so.order_id
		 LEFT JOIN users u ON u.id = o.buyer_id
		 WHERE so.seller_id = $1 AND so.status <> 'pending'
		 ORDER BY o.created_at DESC LIMIT 200`, sellerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.SubOrder{}
	for rows.Next() {
		so := &model.SubOrder{}
		if err := rows.Scan(&so.ID, &so.OrderID, &so.SellerID, &so.Status, &so.SubtotalCents, &so.CommissionCents,
			&so.Currency, &so.CreatedAt, &so.BuyerEmail, &so.Summary); err != nil {
			return nil, err
		}
		out = append(out, so)
	}
	return out, rows.Err()
}

func (r *OrderRepository) Transition(ctx context.Context, orderID int64, from, to string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE orders SET status = $1 WHERE id = $2 AND status = $3`, to, orderID, from)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var current string
		err := tx.QueryRowContext(ctx, `SELECT status FROM orders WHERE id = $1`, orderID).Scan(&current)
		if err == nil && current == to {
			return nil
		}
		return ErrInvalidTransition
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sub_orders SET status = $1 WHERE order_id = $2`, to, orderID); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *OrderRepository) Status(ctx context.Context, orderID int64) (string, error) {
	var s string
	err := r.db.QueryRowContext(ctx, `SELECT status FROM orders WHERE id = $1`, orderID).Scan(&s)
	return s, err
}

func (r *OrderRepository) Splits(ctx context.Context, orderID int64) ([]model.SellerSplit, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT seller_id, subtotal_cents, commission_cents FROM sub_orders WHERE order_id = $1 ORDER BY seller_id`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.SellerSplit
	for rows.Next() {
		var s model.SellerSplit
		if err := rows.Scan(&s.SellerID, &s.SubtotalCents, &s.CommissionCents); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *OrderRepository) PurchasedProduct(ctx context.Context, buyerID, productID int64) (bool, error) {
	var ok bool
	err := r.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM orders o JOIN sub_orders so ON so.order_id = o.id
		 JOIN order_items oi ON oi.sub_order_id = so.id
		 WHERE o.buyer_id = $1 AND oi.product_id = $2 AND o.status = 'paid')`, buyerID, productID).Scan(&ok)
	return ok, err
}

func (r *OrderRepository) SellerUserIDs(ctx context.Context, orderID int64) ([]int64, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT DISTINCT s.user_id FROM sub_orders so JOIN sellers s ON s.id = so.seller_id WHERE so.order_id = $1`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
