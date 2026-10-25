package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"mercatus/catalog/model"
	"mercatus/platform/database"

	"github.com/lib/pq"
)

var (
	ErrDuplicateSlug = errors.New("slug already exists")
	ErrDuplicateSKU  = errors.New("duplicate sku")
	ErrNotFound      = errors.New("not found")
)

type ProductRepository struct {
	db *sql.DB
}

func NewProductRepository(db *sql.DB) *ProductRepository {
	return &ProductRepository{db: db}
}

func (r *ProductRepository) ActiveSellerIDForUser(ctx context.Context, userID int64) (int64, string, error) {
	var id int64
	var status string
	err := r.db.QueryRowContext(ctx, `SELECT id, status FROM sellers WHERE user_id = $1`, userID).Scan(&id, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", nil
	}
	return id, status, err
}

func (r *ProductRepository) CreateProductWithVariants(ctx context.Context, p *model.Product) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	err = tx.QueryRowContext(ctx,
		`INSERT INTO products (seller_id, title, slug, description, category_id, brand, image_url, status, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING id`,
		p.SellerID, p.Title, p.Slug, p.Description, p.CategoryID, p.Brand, p.ImageURL, p.Status, p.CreatedAt, p.UpdatedAt,
	).Scan(&p.ID)
	if err != nil {
		if database.IsUniqueViolation(err) {
			return ErrDuplicateSlug
		}
		return err
	}

	for _, v := range p.Variants {
		v.ProductID = p.ID
		if err := tx.QueryRowContext(ctx,
			`INSERT INTO product_variants (product_id, sku, price, currency, stock, attributes, active, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`,
			v.ProductID, v.SKU, v.Price, v.Currency, v.Stock, v.Attributes, v.Active, v.CreatedAt, v.UpdatedAt,
		).Scan(&v.ID); err != nil {
			if database.IsUniqueViolation(err) {
				return ErrDuplicateSKU
			}
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO stock (variant_id, on_hand, reserved, updated_at) VALUES ($1, $2, 0, now())
			 ON CONFLICT (variant_id) DO UPDATE SET on_hand = EXCLUDED.on_hand, updated_at = now()`,
			v.ID, v.Stock); err != nil {
			return err
		}
		v.Available = v.Stock
	}
	return tx.Commit()
}

const listColumns = `
	p.id, p.seller_id, s.store_name, p.title, p.slug, p.description, p.category_id, COALESCE(c.slug, ''),
	p.brand, p.image_url, p.status, COALESCE(pr.min_price, 0), COALESCE(pr.currency, 'USD'),
	COALESCE(rv.avg_rating, 0), COALESCE(rv.cnt, 0), COALESCE(av.available, 0), p.created_at, p.updated_at`

const listJoins = `
	FROM products p
	JOIN sellers s ON s.id = p.seller_id
	LEFT JOIN categories c ON c.id = p.category_id
	LEFT JOIN LATERAL (
		SELECT min(v.price) AS min_price, min(v.currency) AS currency
		FROM product_variants v WHERE v.product_id = p.id AND v.active
	) pr ON true
	LEFT JOIN LATERAL (
		SELECT avg(r.rating)::float8 AS avg_rating, count(*)::int AS cnt
		FROM reviews r WHERE r.product_id = p.id
	) rv ON true
	LEFT JOIN LATERAL (
		SELECT sum(GREATEST(st.on_hand - st.reserved, 0))::int AS available
		FROM product_variants v JOIN stock st ON st.variant_id = v.id
		WHERE v.product_id = p.id AND v.active
	) av ON true`

func scanProduct(row interface{ Scan(...any) error }) (*model.Product, error) {
	p := &model.Product{}
	err := row.Scan(&p.ID, &p.SellerID, &p.StoreName, &p.Title, &p.Slug, &p.Description, &p.CategoryID, &p.CategorySlug,
		&p.Brand, &p.ImageURL, &p.Status, &p.PriceFrom, &p.Currency, &p.RatingAvg, &p.RatingCount, &p.Available,
		&p.CreatedAt, &p.UpdatedAt)
	return p, err
}

func (r *ProductRepository) FindBySlug(ctx context.Context, slug string, publicOnly bool) (*model.Product, error) {
	q := `SELECT ` + listColumns + listJoins + ` WHERE p.slug = $1`
	if publicOnly {
		q += ` AND p.status = 'active' AND s.status = 'active'`
	}
	p, err := scanProduct(r.db.QueryRowContext(ctx, q, slug))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if p.Variants, err = r.findVariants(ctx, p.ID); err != nil {
		return nil, err
	}
	return p, nil
}

func (r *ProductRepository) Variants(ctx context.Context, productID int64) ([]*model.Variant, error) {
	return r.findVariants(ctx, productID)
}

func (r *ProductRepository) findVariants(ctx context.Context, productID int64) ([]*model.Variant, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT v.id, v.product_id, v.sku, v.price, v.currency, v.stock, COALESCE(GREATEST(st.on_hand - st.reserved, 0), 0),
		        v.attributes, v.active, v.created_at, v.updated_at
		 FROM product_variants v LEFT JOIN stock st ON st.variant_id = v.id
		 WHERE v.product_id = $1 AND v.active ORDER BY v.price, v.id`, productID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.Variant
	for rows.Next() {
		v := &model.Variant{}
		if err := rows.Scan(&v.ID, &v.ProductID, &v.SKU, &v.Price, &v.Currency, &v.Stock, &v.Available,
			&v.Attributes, &v.Active, &v.CreatedAt, &v.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func (r *ProductRepository) List(ctx context.Context, f *model.ProductFilter, publicOnly bool) ([]*model.Product, int, error) {
	var conds []string
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		conds = append(conds, fmt.Sprintf(cond, len(args)))
	}

	if publicOnly {
		conds = append(conds, "p.status = 'active'", "s.status = 'active'")
	} else {
		conds = append(conds, "p.status <> 'removed'")
	}
	if f.CategoryID > 0 {
		add("(p.category_id = $%[1]d OR c.parent_id = $%[1]d)", f.CategoryID)
	}
	if f.CategorySlug != "" {
		add("(c.slug = $%[1]d OR c.parent_id = (SELECT id FROM categories WHERE slug = $%[1]d))", f.CategorySlug)
	}
	if f.SellerID > 0 {
		add("p.seller_id = $%d", f.SellerID)
	}
	if f.RankedIDs != nil {
		add("p.id = ANY($%d)", pq.Array(f.RankedIDs))
	} else if f.Search != "" {
		add(`(p.title ILIKE $%[1]d OR p.brand ILIKE $%[1]d OR p.description ILIKE $%[1]d)`, "%"+escapeLike(f.Search)+"%")
	}
	if f.MinPrice > 0 {
		add("EXISTS (SELECT 1 FROM product_variants v WHERE v.product_id = p.id AND v.active AND v.price >= $%d)", f.MinPrice)
	}
	if f.MaxPrice > 0 {
		add("EXISTS (SELECT 1 FROM product_variants v WHERE v.product_id = p.id AND v.active AND v.price <= $%d)", f.MaxPrice)
	}
	where := " WHERE " + strings.Join(conds, " AND ")

	var total int
	countQ := `SELECT count(*) FROM products p JOIN sellers s ON s.id = p.seller_id LEFT JOIN categories c ON c.id = p.category_id` + where
	if err := r.db.QueryRowContext(ctx, countQ, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	order := "p.created_at DESC, p.id DESC"
	switch f.Sort {
	case model.SortPriceAsc:
		order = "pr.min_price ASC NULLS LAST, p.id DESC"
	case model.SortPriceDesc:
		order = "pr.min_price DESC NULLS LAST, p.id DESC"
	case model.SortRating:
		order = "rv.avg_rating DESC NULLS LAST, rv.cnt DESC, p.id DESC"
	case model.SortRelevance:
		if f.RankedIDs != nil {
			args = append(args, pq.Array(f.RankedIDs))
			order = fmt.Sprintf("array_position($%d::bigint[], p.id)", len(args))
		}
	}

	args = append(args, f.PageSize, (f.Page-1)*f.PageSize)
	q := `SELECT ` + listColumns + listJoins + where +
		fmt.Sprintf(" ORDER BY %s LIMIT $%d OFFSET $%d", order, len(args)-1, len(args))
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	products := []*model.Product{}
	for rows.Next() {
		p, err := scanProduct(rows)
		if err != nil {
			return nil, 0, err
		}
		products = append(products, p)
	}
	return products, total, rows.Err()
}

func (r *ProductRepository) SlugExists(ctx context.Context, slug string) (bool, error) {
	var exists bool
	err := r.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM products WHERE slug = $1)`, slug).Scan(&exists)
	return exists, err
}

func (r *ProductRepository) UpdateStatus(ctx context.Context, productID, sellerID int64, status model.ProductStatus) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE products SET status = $1, updated_at = now() WHERE id = $2 AND seller_id = $3 AND status <> 'removed'`,
		status, productID, sellerID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *ProductRepository) Update(ctx context.Context, productID, sellerID int64, req *model.UpdateProductRequest) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE products SET
			title = COALESCE($1, title),
			description = COALESCE($2, description),
			category_id = CASE WHEN $3::bigint = 0 THEN NULL ELSE COALESCE($3::bigint, category_id) END,
			brand = COALESCE($4, brand),
			image_url = COALESCE($5, image_url),
			updated_at = now()
		 WHERE id = $6 AND seller_id = $7 AND status <> 'removed'`,
		req.Title, req.Description, req.CategoryID, req.Brand, req.ImageURL, productID, sellerID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *ProductRepository) SlugForID(ctx context.Context, id int64) (string, error) {
	var slug string
	err := r.db.QueryRowContext(ctx, `SELECT slug FROM products WHERE id = $1`, id).Scan(&slug)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return slug, err
}

func (r *ProductRepository) SearchDocuments(ctx context.Context) ([]model.SearchDocument, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT p.id, p.title, concat_ws(' ', p.brand, c.name, p.description)
		 FROM products p
		 JOIN sellers s ON s.id = p.seller_id AND s.status = 'active'
		 LEFT JOIN categories c ON c.id = p.category_id
		 WHERE p.status = 'active'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var docs []model.SearchDocument
	for rows.Next() {
		var d model.SearchDocument
		if err := rows.Scan(&d.ID, &d.Title, &d.Body); err != nil {
			return nil, err
		}
		docs = append(docs, d)
	}
	return docs, rows.Err()
}
