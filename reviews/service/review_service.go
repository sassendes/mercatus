package service

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"mercatus/reviews/model"
	"mercatus/reviews/repository"
)

var (
	ErrInvalidRating   = errors.New("rating must be between 1 and 5")
	ErrInvalidText     = errors.New("title must be at most 150 characters and body at most 5000")
	ErrAlreadyReviewed = errors.New("you already reviewed this product")
	ErrProductNotFound = errors.New("product not found")
	ErrNotFound        = errors.New("review not found")
	ErrAlreadyVoted    = errors.New("you cannot vote on this review again")
	ErrOwnProduct      = errors.New("you cannot review your own product")
)

type ReviewService struct {
	reviews *repository.ReviewRepository
}

func NewReviewService(r *repository.ReviewRepository) *ReviewService {
	return &ReviewService{reviews: r}
}

func (s *ReviewService) Create(ctx context.Context, userID int64, req *model.CreateReviewRequest) (*model.Review, error) {
	if req.Rating < 1 || req.Rating > 5 {
		return nil, ErrInvalidRating
	}
	title, body := strings.TrimSpace(req.Title), strings.TrimSpace(req.Body)
	if utf8.RuneCountInString(title) > 150 || utf8.RuneCountInString(body) > 5000 {
		return nil, ErrInvalidText
	}
	ok, err := s.reviews.ProductReviewable(ctx, req.ProductID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrProductNotFound
	}
	own, err := s.reviews.OwnsProduct(ctx, userID, req.ProductID)
	if err != nil {
		return nil, err
	}
	if own {
		return nil, ErrOwnProduct
	}
	verified, err := s.reviews.PurchasedProduct(ctx, userID, req.ProductID)
	if err != nil {
		return nil, err
	}
	rv := &model.Review{
		ProductID: req.ProductID, UserID: userID, Rating: req.Rating, Title: title, Body: body,
		Verified: verified, CreatedAt: time.Now(),
	}
	if err := s.reviews.Create(ctx, rv); err != nil {
		if errors.Is(err, repository.ErrDuplicate) {
			return nil, ErrAlreadyReviewed
		}
		return nil, err
	}
	return rv, nil
}

func (s *ReviewService) ListForProduct(ctx context.Context, productID int64) ([]*model.Review, error) {
	return s.reviews.ListForProduct(ctx, productID)
}

func (s *ReviewService) Summary(ctx context.Context, productID int64) (*model.RatingSummary, error) {
	return s.reviews.Summary(ctx, productID)
}

func (s *ReviewService) MarkHelpful(ctx context.Context, reviewID, userID int64) error {
	err := s.reviews.Vote(ctx, reviewID, userID)
	switch {
	case errors.Is(err, repository.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, repository.ErrDuplicate):
		return ErrAlreadyVoted
	}
	return err
}
