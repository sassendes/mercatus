package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"mercatus/catalog/middleware"
	"mercatus/catalog/model"
	"mercatus/catalog/service"

	"github.com/go-chi/chi/v5"
)

type CatalogHandler struct {
	catalog   *service.CatalogService
	jwtSecret string
}

func NewCatalogHandler(catalog *service.CatalogService, jwtSecret string) *CatalogHandler {
	return &CatalogHandler{catalog: catalog, jwtSecret: jwtSecret}
}

func (h *CatalogHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/products", h.ListProducts)
	r.Get("/products/{slug}", h.GetProduct)
	r.Get("/categories", h.ListCategories)

	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireAuth(h.jwtSecret))
		r.Get("/mine", h.ListMine)
		r.Post("/products", h.CreateProduct)
		r.Patch("/products/{id}", h.UpdateProduct)
		r.Patch("/products/{id}/status", h.SetStatus)
		r.Group(func(r chi.Router) {
			r.Use(middleware.RequireRole("admin"))
			r.Post("/categories", h.CreateCategory)
		})
	})
	return r
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
}

func (h *CatalogHandler) fail(w http.ResponseWriter, r *http.Request, err error, fallback string) {
	var ve *service.ValidationError
	switch {
	case errors.As(err, &ve):
		writeError(w, http.StatusBadRequest, ve.Msg)
	case errors.Is(err, service.ErrNoVariants), errors.Is(err, service.ErrInvalidPrice),
		errors.Is(err, service.ErrInvalidStatus), errors.Is(err, service.ErrUnknownCategory):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, service.ErrNotSeller):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, service.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, service.ErrDuplicate):
		writeError(w, http.StatusConflict, err.Error())
	default:
		slog.ErrorContext(r.Context(), fallback, "error", err)
		writeError(w, http.StatusInternalServerError, fallback)
	}
}

func (h *CatalogHandler) CreateProduct(w http.ResponseWriter, r *http.Request) {
	uid, _ := middleware.UserIDFromContext(r.Context())
	var req model.CreateProductRequest
	if !decode(w, r, &req) {
		return
	}
	product, err := h.catalog.CreateProduct(r.Context(), uid, &req)
	if err != nil {
		h.fail(w, r, err, "could not create product")
		return
	}
	writeJSON(w, http.StatusCreated, product)
}

func (h *CatalogHandler) UpdateProduct(w http.ResponseWriter, r *http.Request) {
	uid, _ := middleware.UserIDFromContext(r.Context())
	id := parseInt64(chi.URLParam(r, "id"))
	var req model.UpdateProductRequest
	if !decode(w, r, &req) {
		return
	}
	product, err := h.catalog.Update(r.Context(), uid, id, &req)
	if err != nil {
		h.fail(w, r, err, "could not update product")
		return
	}
	writeJSON(w, http.StatusOK, product)
}

func (h *CatalogHandler) GetProduct(w http.ResponseWriter, r *http.Request) {
	product, err := h.catalog.GetBySlug(r.Context(), chi.URLParam(r, "slug"))
	if err != nil {
		h.fail(w, r, err, "could not fetch product")
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=15")
	writeJSON(w, http.StatusOK, product)
}

func (h *CatalogHandler) ListProducts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := &model.ProductFilter{
		CategoryID:   parseInt64(q.Get("category_id")),
		CategorySlug: q.Get("category"),
		MinPrice:     parseInt64(q.Get("min_price")),
		MaxPrice:     parseInt64(q.Get("max_price")),
		SellerID:     parseInt64(q.Get("seller_id")),
		Search:       q.Get("search"),
		Sort:         model.ProductSort(q.Get("sort")),
		Page:         int(parseInt64(q.Get("page"))),
		PageSize:     int(parseInt64(q.Get("page_size"))),
	}
	resp, err := h.catalog.List(r.Context(), filter)
	if err != nil {
		h.fail(w, r, err, "could not list products")
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=15")
	writeJSON(w, http.StatusOK, resp)
}

func (h *CatalogHandler) ListMine(w http.ResponseWriter, r *http.Request) {
	uid, _ := middleware.UserIDFromContext(r.Context())
	resp, err := h.catalog.ListMine(r.Context(), uid, int(parseInt64(r.URL.Query().Get("page"))))
	if err != nil {
		h.fail(w, r, err, "could not list products")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *CatalogHandler) SetStatus(w http.ResponseWriter, r *http.Request) {
	uid, _ := middleware.UserIDFromContext(r.Context())
	productID := parseInt64(chi.URLParam(r, "id"))
	var body struct {
		Status model.ProductStatus `json:"status"`
	}
	if !decode(w, r, &body) {
		return
	}
	if err := h.catalog.SetStatus(r.Context(), uid, productID, body.Status); err != nil {
		h.fail(w, r, err, "could not update status")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "status updated"})
}

func (h *CatalogHandler) CreateCategory(w http.ResponseWriter, r *http.Request) {
	var req model.CreateCategoryRequest
	if !decode(w, r, &req) {
		return
	}
	c, err := h.catalog.CreateCategory(r.Context(), &req)
	if err != nil {
		h.fail(w, r, err, "could not create category")
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func (h *CatalogHandler) ListCategories(w http.ResponseWriter, r *http.Request) {
	cats, err := h.catalog.ListCategories(r.Context())
	if err != nil {
		h.fail(w, r, err, "could not list categories")
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(w, http.StatusOK, cats)
}

func parseInt64(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}
