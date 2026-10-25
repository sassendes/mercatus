package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"mercatus/order/middleware"
	"mercatus/order/service"

	"github.com/go-chi/chi/v5"
)

type OrderHandler struct {
	orders    *service.OrderService
	jwtSecret string
}

func NewOrderHandler(o *service.OrderService, jwtSecret string) *OrderHandler {
	return &OrderHandler{orders: o, jwtSecret: jwtSecret}
}

func (h *OrderHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Use(middleware.RequireAuth(h.jwtSecret))
	r.Get("/", h.List)
	r.Get("/sales", h.Sales)
	r.Get("/{id}", h.Get)
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

func internal(w http.ResponseWriter, r *http.Request, err error, msg string) {
	slog.ErrorContext(r.Context(), msg, "error", err)
	writeError(w, http.StatusInternalServerError, msg)
}

func (h *OrderHandler) Get(w http.ResponseWriter, r *http.Request) {
	uid, _ := middleware.UserIDFromContext(r.Context())
	orderID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "order not found")
		return
	}
	o, err := h.orders.Get(r.Context(), orderID, uid)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			writeError(w, http.StatusNotFound, "order not found")
			return
		}
		internal(w, r, err, "could not get order")
		return
	}
	writeJSON(w, http.StatusOK, o)
}

func (h *OrderHandler) List(w http.ResponseWriter, r *http.Request) {
	uid, _ := middleware.UserIDFromContext(r.Context())
	items, err := h.orders.ListForBuyer(r.Context(), uid)
	if err != nil {
		internal(w, r, err, "could not list orders")
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (h *OrderHandler) Sales(w http.ResponseWriter, r *http.Request) {
	uid, _ := middleware.UserIDFromContext(r.Context())
	items, err := h.orders.ListSales(r.Context(), uid)
	if err != nil {
		if errors.Is(err, service.ErrNotSeller) {
			writeError(w, http.StatusForbidden, err.Error())
			return
		}
		internal(w, r, err, "could not list sales")
		return
	}
	writeJSON(w, http.StatusOK, items)
}
