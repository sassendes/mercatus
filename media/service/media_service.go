package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"mercatus/media/model"
	"mercatus/media/repository"
)

var (
	ErrBadContentType = errors.New("unsupported content type")
	ErrBadKind        = errors.New("unsupported media kind")
	ErrNotFound       = errors.New("media not found")
	ErrNotUploaded    = errors.New("upload not found in storage or failed validation")
	ErrDisabled       = errors.New("media storage is not configured")
	ErrBadSize        = errors.New("size must be between 1 byte and 10 MiB")
)

const (
	maxUploadBytes = 10 << 20
	presignTTL     = 10 * time.Minute
)

var allowedTypes = map[string]string{
	"image/jpeg": "jpg",
	"image/png":  "png",
	"image/webp": "webp",
}

var allowedKinds = map[string]bool{"product_image": true, "avatar": true}

type MediaService struct {
	media *repository.MediaRepository
	store *S3
}

func NewMediaService(m *repository.MediaRepository, store *S3) *MediaService {
	return &MediaService{media: m, store: store}
}

type PresignResult struct {
	Asset     *model.MediaAsset `json:"asset"`
	UploadURL string            `json:"upload_url"`
	Method    string            `json:"method"`
	PublicURL string            `json:"public_url"`
	Headers   map[string]string `json:"headers"`
	ExpiresIn int               `json:"expires_in"`
	MaxBytes  int               `json:"max_bytes"`
}

func (s *MediaService) Enabled() bool { return s.store != nil }

func (s *MediaService) Presign(ctx context.Context, ownerID int64, req *model.PresignRequest) (*PresignResult, error) {
	if s.store == nil {
		return nil, ErrDisabled
	}
	ext, ok := allowedTypes[req.ContentType]
	if !ok {
		return nil, ErrBadContentType
	}
	if !allowedKinds[req.Kind] {
		return nil, ErrBadKind
	}
	if req.Size <= 0 || req.Size > maxUploadBytes {
		return nil, ErrBadSize
	}
	key := req.Kind + "/" + randHex(16) + "." + ext
	m := &model.MediaAsset{OwnerID: ownerID, Kind: req.Kind, ObjectKey: key, ContentType: req.ContentType}
	if err := s.media.Create(ctx, m); err != nil {
		return nil, err
	}
	headers := map[string]string{
		"Content-Type":   req.ContentType,
		"Content-Length": strconv.FormatInt(req.Size, 10),
	}
	return &PresignResult{
		Asset:     m,
		UploadURL: s.store.PresignWithHeaders(http.MethodPut, key, presignTTL, time.Now(), headers),
		Method:    http.MethodPut,
		Headers:   headers,
		PublicURL: s.store.PublicURL(key),
		ExpiresIn: int(presignTTL.Seconds()),
		MaxBytes:  maxUploadBytes,
	}, nil
}

func (s *MediaService) Confirm(ctx context.Context, id, ownerID int64) (*model.MediaAsset, error) {
	if s.store == nil {
		return nil, ErrDisabled
	}
	m, err := s.media.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if m == nil || m.OwnerID != ownerID {
		return nil, ErrNotFound
	}
	size, ct, err := s.store.Head(ctx, m.ObjectKey)
	if err != nil {
		return nil, ErrNotUploaded
	}
	if size <= 0 || size > maxUploadBytes || strings.TrimSpace(strings.Split(ct, ";")[0]) != m.ContentType {
		if derr := s.store.Delete(ctx, m.ObjectKey); derr != nil {
			slog.WarnContext(ctx, "could not delete rejected upload", "key", m.ObjectKey, "error", derr)
		}
		return nil, ErrNotUploaded
	}
	if err := s.media.MarkReady(ctx, id, ownerID); err != nil {
		return nil, ErrNotFound
	}
	m.Status = "ready"
	m.URL = s.store.PublicURL(m.ObjectKey)
	return m, nil
}

func (s *MediaService) Get(ctx context.Context, id int64) (*model.MediaAsset, error) {
	m, err := s.media.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if m == nil || m.Status != "ready" {
		return nil, ErrNotFound
	}
	if s.store != nil {
		m.URL = s.store.PublicURL(m.ObjectKey)
	}
	return m, nil
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
