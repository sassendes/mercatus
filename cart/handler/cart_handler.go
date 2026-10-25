package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"mercatus/cart/middleware"
	"mercatus/cart/model"
	"mercatus/cart/service"

	"github.com/go-chi/chi/v5"
)

type CartHandler struct {
	cart      *service.CartService
	jwtSecret string
}

func NewCartHandler(cart *service.CartService, jwtSecret string) *CartHandler {
	return &CartHandler{cart: cart, jwtSecret: jwtSecret}
}

func (h *CartHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Use(middleware.RequireAuth(h.jwtSecret))
	r.Get("/", h.GetCart)
	r.Delete("/", h.Clear)
	r.Post("/items", h.AddItem)
	r.Patch("/items/{variantID}", h.UpdateQuantity)
	r.Delete("/items/{variantID}", h.RemoveItem)
	return r
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func fail(w http.ResponseWriter, r *http.Request, err error, fallback string) {
	switch {
	case errors.Is(err, service.ErrInvalidQuantity), errors.Is(err, service.ErrTooManyItems), errors.Is(err, service.ErrOwnProduct):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, service.ErrItemNotFound), errors.Is(err, service.ErrVariantUnavailable):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, service.ErrInsufficientStock):
		writeError(w, http.StatusConflict, err.Error())
	default:
		slog.ErrorContext(r.Context(), fallback, "error", err)
		writeError(w, http.StatusInternalServerError, fallback)
	}
}

func variantParam(r *http.Request) int64 {
	id, _ := strconv.ParseInt(chi.URLParam(r, "variantID"), 10, 64)
	return id
}

func (h *CartHandler) GetCart(w http.ResponseWriter, r *http.Request) {
	uid, _ := middleware.UserIDFromContext(r.Context())
	cart, err := h.cart.Get(r.Context(), uid)
	if err != nil {
		fail(w, r, err, "could not load cart")
		return
	}
	writeJSON(w, http.StatusOK, cart)
}

func (h *CartHandler) AddItem(w http.ResponseWriter, r *http.Request) {
	uid, _ := middleware.UserIDFromContext(r.Context())
	var req model.AddItemRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	cart, err := h.cart.AddItem(r.Context(), uid, &req)
	if err != nil {
		fail(w, r, err, "could not add item")
		return
	}
	writeJSON(w, http.StatusOK, cart)
}

func (h *CartHandler) UpdateQuantity(w http.ResponseWriter, r *http.Request) {
	uid, _ := middleware.UserIDFromContext(r.Context())
	var req model.UpdateQuantityRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	cart, err := h.cart.UpdateQuantity(r.Context(), uid, variantParam(r), req.Quantity)
	if err != nil {
		fail(w, r, err, "could not update item")
		return
	}
	writeJSON(w, http.StatusOK, cart)
}

func (h *CartHandler) RemoveItem(w http.ResponseWriter, r *http.Request) {
	uid, _ := middleware.UserIDFromContext(r.Context())
	cart, err := h.cart.RemoveItem(r.Context(), uid, variantParam(r))
	if err != nil {
		fail(w, r, err, "could not remove item")
		return
	}
	writeJSON(w, http.StatusOK, cart)
}

func (h *CartHandler) Clear(w http.ResponseWriter, r *http.Request) {
	uid, _ := middleware.UserIDFromContext(r.Context())
	if err := h.cart.Clear(r.Context(), uid); err != nil {
		fail(w, r, err, "could not clear cart")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "cart cleared"})
}
