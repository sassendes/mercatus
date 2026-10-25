package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"mercatus/reviews/middleware"
	"mercatus/reviews/model"
	"mercatus/reviews/service"

	"github.com/go-chi/chi/v5"
)

type ReviewHandler struct {
	reviews   *service.ReviewService
	jwtSecret string
}

func NewReviewHandler(r *service.ReviewService, jwtSecret string) *ReviewHandler {
	return &ReviewHandler{reviews: r, jwtSecret: jwtSecret}
}

func (h *ReviewHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/product/{productID}", h.List)
	r.Get("/product/{productID}/summary", h.Summary)
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireAuth(h.jwtSecret))
		r.Post("/", h.Create)
		r.Post("/{id}/helpful", h.MarkHelpful)
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

func internal(w http.ResponseWriter, r *http.Request, err error, msg string) {
	slog.ErrorContext(r.Context(), msg, "error", err)
	writeError(w, http.StatusInternalServerError, msg)
}

func idParam(r *http.Request, name string) int64 {
	id, _ := strconv.ParseInt(chi.URLParam(r, name), 10, 64)
	return id
}

func (h *ReviewHandler) Create(w http.ResponseWriter, r *http.Request) {
	uid, _ := middleware.UserIDFromContext(r.Context())
	var req model.CreateReviewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	rv, err := h.reviews.Create(r.Context(), uid, &req)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrInvalidRating), errors.Is(err, service.ErrInvalidText), errors.Is(err, service.ErrOwnProduct):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, service.ErrProductNotFound):
			writeError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, service.ErrAlreadyReviewed):
			writeError(w, http.StatusConflict, err.Error())
		default:
			internal(w, r, err, "could not create review")
		}
		return
	}
	writeJSON(w, http.StatusCreated, rv)
}

func (h *ReviewHandler) List(w http.ResponseWriter, r *http.Request) {
	items, err := h.reviews.ListForProduct(r.Context(), idParam(r, "productID"))
	if err != nil {
		internal(w, r, err, "could not list reviews")
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (h *ReviewHandler) Summary(w http.ResponseWriter, r *http.Request) {
	sum, err := h.reviews.Summary(r.Context(), idParam(r, "productID"))
	if err != nil {
		internal(w, r, err, "could not compute summary")
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

func (h *ReviewHandler) MarkHelpful(w http.ResponseWriter, r *http.Request) {
	uid, _ := middleware.UserIDFromContext(r.Context())
	err := h.reviews.MarkHelpful(r.Context(), idParam(r, "id"), uid)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]string{"message": "marked helpful"})
	case errors.Is(err, service.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, service.ErrAlreadyVoted):
		writeError(w, http.StatusConflict, err.Error())
	default:
		internal(w, r, err, "could not record vote")
	}
}
