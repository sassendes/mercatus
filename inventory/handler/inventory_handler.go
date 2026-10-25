package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"mercatus/inventory/middleware"
	"mercatus/inventory/model"
	"mercatus/inventory/service"

	"github.com/go-chi/chi/v5"
)

type InventoryHandler struct {
	inv       *service.InventoryService
	jwtSecret string
}

func NewInventoryHandler(inv *service.InventoryService, jwtSecret string) *InventoryHandler {
	return &InventoryHandler{inv: inv, jwtSecret: jwtSecret}
}

func (h *InventoryHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/{variantID}", h.GetStock)
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireAuth(h.jwtSecret))
		r.Put("/", h.SetStock)
	})
	return r
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (h *InventoryHandler) SetStock(w http.ResponseWriter, r *http.Request) {
	uid, _ := middleware.UserIDFromContext(r.Context())
	var req model.SetStockRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	err := h.inv.SetStockAsSeller(r.Context(), uid, req.VariantID, req.OnHand)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]string{"message": "stock set"})
	case errors.Is(err, service.ErrInvalidQuantity):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, service.ErrForbidden):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, service.ErrBelowReserved):
		writeError(w, http.StatusConflict, err.Error())
	default:
		slog.ErrorContext(r.Context(), "could not set stock", "error", err)
		writeError(w, http.StatusInternalServerError, "could not set stock")
	}
}

func (h *InventoryHandler) GetStock(w http.ResponseWriter, r *http.Request) {
	variantID, _ := strconv.ParseInt(chi.URLParam(r, "variantID"), 10, 64)
	st, err := h.inv.GetStock(r.Context(), variantID)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			writeError(w, http.StatusNotFound, "stock not found")
			return
		}
		slog.ErrorContext(r.Context(), "could not get stock", "error", err)
		writeError(w, http.StatusInternalServerError, "could not get stock")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"variant_id": st.VariantID, "available": max(st.Available(), 0)})
}
