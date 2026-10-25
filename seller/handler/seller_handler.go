package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"mercatus/seller/middleware"
	"mercatus/seller/service"

	"github.com/go-chi/chi/v5"
)

type SellerHandler struct {
	sellers   *service.SellerService
	jwtSecret string
}

func NewSellerHandler(sellers *service.SellerService, jwtSecret string) *SellerHandler {
	return &SellerHandler{sellers: sellers, jwtSecret: jwtSecret}
}

func (h *SellerHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireAuth(h.jwtSecret))
		r.Post("/onboard", h.Onboard)
		r.Get("/me", h.GetMe)
		r.Post("/connect-stripe", h.ConnectStripe)
	})
	r.Get("/{id}", h.GetSeller)
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

func fail(w http.ResponseWriter, r *http.Request, err error, fallback string) {
	var ve *service.ValidationError
	switch {
	case errors.As(err, &ve):
		writeError(w, http.StatusBadRequest, ve.Msg)
	case errors.Is(err, service.ErrAlreadySeller), errors.Is(err, service.ErrStripeTaken), errors.Is(err, service.ErrStripeConnected):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, service.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, service.ErrSuspended):
		writeError(w, http.StatusForbidden, err.Error())
	default:
		slog.ErrorContext(r.Context(), fallback, "error", err)
		writeError(w, http.StatusInternalServerError, fallback)
	}
}

func (h *SellerHandler) Onboard(w http.ResponseWriter, r *http.Request) {
	if middleware.RoleFromContext(r.Context()) == "admin" {
		writeError(w, http.StatusForbidden, "admins cannot become sellers")
		return
	}
	userID, _ := middleware.UserIDFromContext(r.Context())
	var req service.OnboardInput
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	seller, err := h.sellers.Onboard(r.Context(), userID, &req)
	if err != nil {
		fail(w, r, err, "could not onboard")
		return
	}
	writeJSON(w, http.StatusCreated, seller)
}

func (h *SellerHandler) GetMe(w http.ResponseWriter, r *http.Request) {
	userID, _ := middleware.UserIDFromContext(r.Context())
	seller, err := h.sellers.GetByUserID(r.Context(), userID)
	if err != nil {
		fail(w, r, err, "could not load seller")
		return
	}
	writeJSON(w, http.StatusOK, seller)
}

func (h *SellerHandler) GetSeller(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusNotFound, "seller not found")
		return
	}
	seller, err := h.sellers.GetPublic(r.Context(), id)
	if err != nil {
		fail(w, r, err, "could not load seller")
		return
	}
	writeJSON(w, http.StatusOK, seller)
}

func (h *SellerHandler) ConnectStripe(w http.ResponseWriter, r *http.Request) {
	userID, _ := middleware.UserIDFromContext(r.Context())
	var body struct {
		StripeAccountID string `json:"stripe_account_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	seller, err := h.sellers.ConnectStripe(r.Context(), userID, body.StripeAccountID)
	if err != nil {
		fail(w, r, err, "could not connect stripe")
		return
	}
	writeJSON(w, http.StatusOK, seller)
}
