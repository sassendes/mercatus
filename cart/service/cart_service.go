package service

import (
	"context"
	"errors"

	"mercatus/cart/model"
	"mercatus/cart/repository"
)

var (
	ErrInvalidQuantity    = errors.New("quantity must be between 1 and 99")
	ErrItemNotFound       = errors.New("item not in cart")
	ErrVariantUnavailable = errors.New("this item is not available")
	ErrInsufficientStock  = errors.New("not enough stock for the requested quantity")
	ErrTooManyItems       = errors.New("cart is full")
	ErrOwnProduct         = errors.New("you cannot buy your own product")
)

const (
	maxLineQuantity = 99
	maxLines        = 100
)

type CartService struct {
	carts *repository.CartRepository
}

func NewCartService(carts *repository.CartRepository) *CartService {
	return &CartService{carts: carts}
}

func (s *CartService) Get(ctx context.Context, userID int64) (*model.Cart, error) {
	cart, err := s.carts.GetOrCreateCart(ctx, userID)
	if err != nil {
		return nil, err
	}
	items, err := s.carts.LoadItems(ctx, cart.ID)
	if err != nil {
		return nil, err
	}
	cart.Items = items
	cart.Currency = "USD"
	for _, it := range items {
		if !it.Purchasable {
			continue
		}
		cart.Subtotal += it.UnitPrice * int64(it.Quantity)
		cart.ItemCount += it.Quantity
	}
	return cart, nil
}

func (s *CartService) setQuantity(ctx context.Context, userID, variantID int64, qty int, additive bool) (*model.Cart, error) {
	if qty <= 0 || qty > maxLineQuantity {
		return nil, ErrInvalidQuantity
	}
	cart, err := s.carts.GetOrCreateCart(ctx, userID)
	if err != nil {
		return nil, err
	}
	info, err := s.carts.VariantInfo(ctx, variantID)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, ErrVariantUnavailable
	}
	if err != nil {
		return nil, err
	}
	if info.SellerUserID == userID {
		return nil, ErrOwnProduct
	}
	current, err := s.carts.CurrentQuantity(ctx, cart.ID, variantID)
	if err != nil {
		return nil, err
	}
	if !additive && current == 0 {
		return nil, ErrItemNotFound
	}
	if additive && current == 0 {
		items, err := s.carts.LoadItems(ctx, cart.ID)
		if err != nil {
			return nil, err
		}
		if len(items) >= maxLines {
			return nil, ErrTooManyItems
		}
	}
	target := qty
	if additive {
		target = current + qty
	}
	if target > maxLineQuantity {
		return nil, ErrInvalidQuantity
	}
	if target > info.Available {
		return nil, ErrInsufficientStock
	}
	if err := s.carts.SetItem(ctx, cart.ID, info, target); err != nil {
		return nil, err
	}
	return s.Get(ctx, userID)
}

func (s *CartService) AddItem(ctx context.Context, userID int64, req *model.AddItemRequest) (*model.Cart, error) {
	return s.setQuantity(ctx, userID, req.VariantID, req.Quantity, true)
}

func (s *CartService) UpdateQuantity(ctx context.Context, userID, variantID int64, qty int) (*model.Cart, error) {
	return s.setQuantity(ctx, userID, variantID, qty, false)
}

func (s *CartService) RemoveItem(ctx context.Context, userID, variantID int64) (*model.Cart, error) {
	cart, err := s.carts.GetOrCreateCart(ctx, userID)
	if err != nil {
		return nil, err
	}
	if err := s.carts.RemoveItem(ctx, cart.ID, variantID); err != nil {
		return nil, err
	}
	return s.Get(ctx, userID)
}

func (s *CartService) Clear(ctx context.Context, userID int64) error {
	cart, err := s.carts.GetOrCreateCart(ctx, userID)
	if err != nil {
		return err
	}
	return s.carts.Clear(ctx, cart.ID)
}
