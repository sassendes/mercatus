package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"mercatus/media/middleware"
	"mercatus/media/model"
	"mercatus/media/service"

	"github.com/go-chi/chi/v5"
)

type MediaHandler struct {
	media     *service.MediaService
	jwtSecret string
}

func NewMediaHandler(m *service.MediaService, jwtSecret string) *MediaHandler {
	return &MediaHandler{media: m, jwtSecret: jwtSecret}
}

func (h *MediaHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Group(func(r chi.Router) {
		r.Use(middleware.RequireAuth(h.jwtSecret))
		r.Post("/presign", h.Presign)
		r.Post("/{id}/confirm", h.Confirm)
	})
	r.Get("/{id}", h.Get)
	return r
}

func writeJSON(w http.ResponseWriter, s int, d any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(s)
	_ = json.NewEncoder(w).Encode(d)
}

func writeErr(w http.ResponseWriter, s int, m string) { writeJSON(w, s, map[string]string{"error": m}) }

func fail(w http.ResponseWriter, r *http.Request, err error, fallback string) {
	switch {
	case errors.Is(err, service.ErrDisabled):
		writeErr(w, http.StatusServiceUnavailable, err.Error())
	case errors.Is(err, service.ErrBadContentType), errors.Is(err, service.ErrBadKind), errors.Is(err, service.ErrBadSize), errors.Is(err, service.ErrNotUploaded):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, service.ErrNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	default:
		slog.ErrorContext(r.Context(), fallback, "error", err)
		writeErr(w, http.StatusInternalServerError, fallback)
	}
}

func (h *MediaHandler) Presign(w http.ResponseWriter, r *http.Request) {
	uid, _ := middleware.UserIDFromContext(r.Context())
	var req model.PresignRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	res, err := h.media.Presign(r.Context(), uid, &req)
	if err != nil {
		fail(w, r, err, "could not presign")
		return
	}
	writeJSON(w, http.StatusCreated, res)
}

func (h *MediaHandler) Confirm(w http.ResponseWriter, r *http.Request) {
	uid, _ := middleware.UserIDFromContext(r.Context())
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	m, err := h.media.Confirm(r.Context(), id, uid)
	if err != nil {
		fail(w, r, err, "could not confirm upload")
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (h *MediaHandler) Get(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	m, err := h.media.Get(r.Context(), id)
	if err != nil {
		fail(w, r, err, "could not load media")
		return
	}
	writeJSON(w, http.StatusOK, m)
}
