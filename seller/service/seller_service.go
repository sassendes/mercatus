package service

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"mercatus/seller/model"
	"mercatus/seller/repository"
)

var (
	ErrAlreadySeller   = errors.New("user is already a seller")
	ErrNotFound        = errors.New("seller not found")
	ErrInvalidInput    = errors.New("invalid input")
	ErrSuspended       = errors.New("seller account is suspended")
	ErrStripeTaken     = errors.New("this stripe account is already linked to another store")
	ErrStripeConnected = errors.New("a stripe account is already connected, contact support to change it")
	stripeAccountRegex = regexp.MustCompile(`^acct_[A-Za-z0-9]{8,32}$`)
)

type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }
func (e *ValidationError) Unwrap() error { return ErrInvalidInput }

const defaultCommissionRate = 1000

type SellerService struct {
	sellers *repository.SellerRepository
}

func NewSellerService(sellers *repository.SellerRepository) *SellerService {
	return &SellerService{sellers: sellers}
}

func (s *SellerService) uniqueSlug(ctx context.Context, name string) (string, error) {
	base := Slugify(name)
	if base == "" {
		base = "store"
	}
	slug := base
	for i := 2; i < 1000; i++ {
		exists, err := s.sellers.SlugExists(ctx, slug)
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

func (s *SellerService) Onboard(ctx context.Context, userID int64, req *OnboardInput) (*model.Seller, error) {
	name := strings.TrimSpace(req.StoreName)
	if n := utf8.RuneCountInString(name); n < 2 || n > 80 {
		return nil, &ValidationError{"store name must be between 2 and 80 characters"}
	}
	if utf8.RuneCountInString(req.Description) > 2000 {
		return nil, &ValidationError{"description is too long"}
	}
	var seller *model.Seller
	for attempt := 0; attempt < 3; attempt++ {
		slug, err := s.uniqueSlug(ctx, name)
		if err != nil {
			return nil, err
		}
		now := time.Now()
		seller = &model.Seller{
			UserID: userID, StoreName: name, Slug: slug, Description: strings.TrimSpace(req.Description),
			Status: model.StatusPending, CommissionRate: defaultCommissionRate, CreatedAt: now, UpdatedAt: now,
		}
		err = s.sellers.CreateAndPromote(ctx, seller)
		if errors.Is(err, repository.ErrAlreadySeller) {
			return nil, ErrAlreadySeller
		}
		if errors.Is(err, repository.ErrDuplicateSlug) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return seller, nil
	}
	return nil, errors.New("could not allocate store slug")
}

type OnboardInput struct {
	StoreName   string `json:"store_name"`
	Description string `json:"description"`
}

func (s *SellerService) GetByUserID(ctx context.Context, userID int64) (*model.Seller, error) {
	seller, err := s.sellers.FindByUserID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if seller == nil {
		return nil, ErrNotFound
	}
	return seller, nil
}

func (s *SellerService) GetPublic(ctx context.Context, id int64) (*model.PublicSeller, error) {
	seller, err := s.sellers.FindByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if seller == nil || seller.Status != model.StatusActive {
		return nil, ErrNotFound
	}
	return seller.Public(), nil
}

func (s *SellerService) ConnectStripe(ctx context.Context, userID int64, stripeAccountID string) (*model.Seller, error) {
	stripeAccountID = strings.TrimSpace(stripeAccountID)
	if !stripeAccountRegex.MatchString(stripeAccountID) {
		return nil, &ValidationError{"stripe_account_id must look like acct_XXXXXXXX"}
	}
	seller, err := s.GetByUserID(ctx, userID)
	if err != nil {
		return nil, err
	}
	if seller.Status == model.StatusSuspended {
		return nil, ErrSuspended
	}
	if seller.StripeAccountID != "" && seller.StripeAccountID != stripeAccountID {
		return nil, ErrStripeConnected
	}
	if err := s.sellers.ActivateWithStripe(ctx, seller.ID, stripeAccountID); err != nil {
		if errors.Is(err, repository.ErrStripeTaken) {
			return nil, ErrStripeTaken
		}
		return nil, err
	}
	return s.GetByUserID(ctx, userID)
}
