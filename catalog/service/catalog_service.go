package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"mercatus/catalog/model"
	"mercatus/catalog/repository"
)

var (
	ErrNoVariants      = errors.New("product must have at least one variant")
	ErrInvalidPrice    = errors.New("variant price must be between 1 and 100000000 cents")
	ErrNotFound        = errors.New("product not found")
	ErrNotSeller       = errors.New("an active seller account is required")
	ErrInvalidInput    = errors.New("invalid input")
	ErrDuplicate       = errors.New("already exists")
	ErrInvalidStatus   = errors.New("invalid status")
	ErrUnknownCategory = errors.New("category does not exist")
)

type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }
func (e *ValidationError) Unwrap() error { return ErrInvalidInput }

const (
	maxPrice    = 100_000_000
	maxVariants = 50
	maxStock    = 1_000_000
	searchLimit = 500
)

type CatalogService struct {
	products   *repository.ProductRepository
	categories *repository.CategoryRepository
	search     SearchIndex
}

func NewCatalogService(products *repository.ProductRepository, categories *repository.CategoryRepository, search SearchIndex) *CatalogService {
	return &CatalogService{products: products, categories: categories, search: search}
}

func (s *CatalogService) uniqueSlug(ctx context.Context, title string) (string, error) {
	base := Slugify(title)
	if base == "" {
		base = "product"
	}
	slug := base
	for i := 2; i < 1000; i++ {
		exists, err := s.products.SlugExists(ctx, slug)
		if err != nil {
			return "", err
		}
		if !exists {
			return slug, nil
		}
		slug = fmt.Sprintf("%s-%d", base, i)
	}
	return "", errors.New("could not allocate slug")
}

func validImageURL(raw string) bool {
	if raw == "" {
		return true
	}
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "https" || (u.Scheme == "" && u.Host == "" && strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "//"))) && len(raw) <= 2048
}

func (s *CatalogService) sellerFor(ctx context.Context, userID int64) (int64, error) {
	id, status, err := s.products.ActiveSellerIDForUser(ctx, userID)
	if err != nil {
		return 0, err
	}
	if id == 0 || status != "active" {
		return 0, ErrNotSeller
	}
	return id, nil
}

func (s *CatalogService) validateCategory(ctx context.Context, id *int64) error {
	if id == nil || *id == 0 {
		return nil
	}
	ok, err := s.categories.Exists(ctx, *id)
	if err != nil {
		return err
	}
	if !ok {
		return ErrUnknownCategory
	}
	return nil
}

func (s *CatalogService) CreateProduct(ctx context.Context, userID int64, req *model.CreateProductRequest) (*model.Product, error) {
	sellerID, err := s.sellerFor(ctx, userID)
	if err != nil {
		return nil, err
	}
	req.Title = strings.TrimSpace(req.Title)
	if n := utf8.RuneCountInString(req.Title); n < 3 || n > 200 {
		return nil, &ValidationError{"title must be between 3 and 200 characters"}
	}
	if utf8.RuneCountInString(req.Description) > 10000 || utf8.RuneCountInString(req.Brand) > 100 {
		return nil, &ValidationError{"description or brand too long"}
	}
	if !validImageURL(req.ImageURL) {
		return nil, &ValidationError{"image_url must be an https URL"}
	}
	if len(req.Variants) == 0 {
		return nil, ErrNoVariants
	}
	if len(req.Variants) > maxVariants {
		return nil, &ValidationError{"too many variants"}
	}
	if req.CategoryID != nil && *req.CategoryID == 0 {
		req.CategoryID = nil
	}
	if err := s.validateCategory(ctx, req.CategoryID); err != nil {
		return nil, err
	}

	now := time.Now()
	product := &model.Product{
		SellerID:    sellerID,
		Title:       req.Title,
		Description: strings.TrimSpace(req.Description),
		CategoryID:  req.CategoryID,
		Brand:       strings.TrimSpace(req.Brand),
		ImageURL:    req.ImageURL,
		Status:      model.StatusActive,
		Currency:    "USD",
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	seen := map[string]bool{}
	for i, vr := range req.Variants {
		if vr.Price <= 0 || vr.Price > maxPrice {
			return nil, ErrInvalidPrice
		}
		if vr.Stock < 0 || vr.Stock > maxStock {
			return nil, &ValidationError{"stock must be between 0 and 1000000"}
		}
		currency := strings.ToUpper(strings.TrimSpace(vr.Currency))
		if currency == "" {
			currency = "USD"
		}
		if currency != "USD" {
			return nil, &ValidationError{"only USD is supported"}
		}
		sku := strings.TrimSpace(vr.SKU)
		if sku == "" {
			sku = fmt.Sprintf("SKU-%d", i+1)
		}
		if len(sku) > 64 || seen[sku] {
			return nil, &ValidationError{"variant skus must be unique and at most 64 characters"}
		}
		seen[sku] = true
		attrs := vr.Attributes
		if attrs == nil {
			attrs = model.Attributes{}
		}
		product.Variants = append(product.Variants, &model.Variant{
			SKU: sku, Price: vr.Price, Currency: currency, Stock: vr.Stock,
			Attributes: attrs, Active: true, CreatedAt: now, UpdatedAt: now,
		})
	}

	for attempt := 0; attempt < 3; attempt++ {
		if product.Slug, err = s.uniqueSlug(ctx, req.Title); err != nil {
			return nil, err
		}
		err = s.products.CreateProductWithVariants(ctx, product)
		if !errors.Is(err, repository.ErrDuplicateSlug) {
			break
		}
	}
	if err != nil {
		if errors.Is(err, repository.ErrDuplicateSKU) {
			return nil, &ValidationError{"duplicate sku"}
		}
		return nil, err
	}
	s.indexAsync(product.Slug)
	return s.products.FindBySlug(ctx, product.Slug, false)
}

func (s *CatalogService) indexAsync(slug string) {
	if s.search == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		p, err := s.products.FindBySlug(ctx, slug, true)
		if err != nil || p == nil {
			return
		}
		doc := model.SearchDocument{ID: p.ID, Title: p.Title, Body: strings.Join([]string{p.Brand, p.Description}, " ")}
		if err := s.search.Upsert(ctx, doc); err != nil {
			slog.Warn("search index upsert failed", "product_id", p.ID, "error", err)
		}
	}()
}

func (s *CatalogService) GetBySlug(ctx context.Context, slug string) (*model.Product, error) {
	p, err := s.products.FindBySlug(ctx, slug, true)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, ErrNotFound
	}
	return p, nil
}

func normalizeFilter(f *model.ProductFilter) {
	if f.Page < 1 {
		f.Page = 1
	}
	if f.Page > 500 {
		f.Page = 500
	}
	if f.PageSize < 1 || f.PageSize > 100 {
		f.PageSize = 24
	}
	f.Search = strings.TrimSpace(f.Search)
	if len(f.Search) > 200 {
		f.Search = f.Search[:200]
	}
	switch f.Sort {
	case model.SortNewest, model.SortPriceAsc, model.SortPriceDesc, model.SortRating, model.SortRelevance:
	default:
		f.Sort = ""
	}
}

func (s *CatalogService) List(ctx context.Context, f *model.ProductFilter) (*model.ProductListResponse, error) {
	normalizeFilter(f)
	if f.Search != "" && s.search != nil {
		ids, err := s.search.Search(ctx, f.Search, searchLimit)
		if err != nil {
			slog.WarnContext(ctx, "search engine unavailable, falling back to database search", "error", err)
		} else {
			f.RankedIDs = ids
			if f.Sort == "" {
				f.Sort = model.SortRelevance
			}
		}
	}
	if f.Sort == "" || (f.Sort == model.SortRelevance && f.RankedIDs == nil) {
		f.Sort = model.SortNewest
	}
	products, total, err := s.products.List(ctx, f, true)
	if err != nil {
		return nil, err
	}
	return &model.ProductListResponse{Products: products, Total: total, Page: f.Page, PageSize: f.PageSize}, nil
}

func (s *CatalogService) ListMine(ctx context.Context, userID int64, page int) (*model.ProductListResponse, error) {
	sellerID, err := s.sellerFor(ctx, userID)
	if err != nil {
		return nil, err
	}
	f := &model.ProductFilter{SellerID: sellerID, Page: page, PageSize: 100, Sort: model.SortNewest}
	normalizeFilter(f)
	products, total, err := s.products.List(ctx, f, false)
	if err != nil {
		return nil, err
	}
	for _, p := range products {
		if p.Variants, err = s.products.Variants(ctx, p.ID); err != nil {
			return nil, err
		}
	}
	return &model.ProductListResponse{Products: products, Total: total, Page: f.Page, PageSize: f.PageSize}, nil
}

func (s *CatalogService) SetStatus(ctx context.Context, userID, productID int64, status model.ProductStatus) error {
	if !status.SellerSettable() {
		return ErrInvalidStatus
	}
	sellerID, err := s.sellerFor(ctx, userID)
	if err != nil {
		return err
	}
	if err := s.products.UpdateStatus(ctx, productID, sellerID, status); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	s.refreshIndex(productID, status == model.StatusActive)
	return nil
}

func (s *CatalogService) Update(ctx context.Context, userID, productID int64, req *model.UpdateProductRequest) (*model.Product, error) {
	sellerID, err := s.sellerFor(ctx, userID)
	if err != nil {
		return nil, err
	}
	if req.Title != nil {
		t := strings.TrimSpace(*req.Title)
		if n := utf8.RuneCountInString(t); n < 3 || n > 200 {
			return nil, &ValidationError{"title must be between 3 and 200 characters"}
		}
		req.Title = &t
	}
	if req.ImageURL != nil && !validImageURL(*req.ImageURL) {
		return nil, &ValidationError{"image_url must be an https URL"}
	}
	if req.Description != nil {
		d := strings.TrimSpace(*req.Description)
		if utf8.RuneCountInString(d) > 10000 {
			return nil, &ValidationError{"description or brand too long"}
		}
		req.Description = &d
	}
	if req.Brand != nil {
		b := strings.TrimSpace(*req.Brand)
		if utf8.RuneCountInString(b) > 100 {
			return nil, &ValidationError{"description or brand too long"}
		}
		req.Brand = &b
	}
	if err := s.validateCategory(ctx, req.CategoryID); err != nil {
		return nil, err
	}
	if err := s.products.Update(ctx, productID, sellerID, req); err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	slug, err := s.products.SlugForID(ctx, productID)
	if err != nil {
		return nil, err
	}
	s.indexAsync(slug)
	return s.products.FindBySlug(ctx, slug, false)
}

func (s *CatalogService) refreshIndex(productID int64, active bool) {
	if s.search == nil {
		return
	}
	if !active {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := s.search.Delete(ctx, productID); err != nil {
				slog.Warn("search index delete failed", "product_id", productID, "error", err)
			}
		}()
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if slug, err := s.products.SlugForID(ctx, productID); err == nil {
			s.indexAsync(slug)
		}
	}()
}

func (s *CatalogService) Reindex(ctx context.Context) (int, error) {
	if s.search == nil {
		return 0, nil
	}
	docs, err := s.products.SearchDocuments(ctx)
	if err != nil {
		return 0, err
	}
	if err := s.search.Replace(ctx, docs); err != nil {
		return 0, err
	}
	return len(docs), nil
}

func (s *CatalogService) CreateCategory(ctx context.Context, req *model.CreateCategoryRequest) (*model.Category, error) {
	name := strings.TrimSpace(req.Name)
	if n := utf8.RuneCountInString(name); n < 2 || n > 80 {
		return nil, &ValidationError{"category name must be between 2 and 80 characters"}
	}
	if err := s.validateCategory(ctx, req.ParentID); err != nil {
		return nil, err
	}
	c := &model.Category{ParentID: req.ParentID, Name: name, Slug: Slugify(name), CreatedAt: time.Now()}
	if err := s.categories.Create(ctx, c); err != nil {
		if errors.Is(err, repository.ErrDuplicateCategory) {
			return nil, ErrDuplicate
		}
		return nil, err
	}
	return c, nil
}

func (s *CatalogService) ListCategories(ctx context.Context) ([]*model.Category, error) {
	return s.categories.List(ctx)
}
