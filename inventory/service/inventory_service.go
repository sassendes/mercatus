package service

import (
	"context"
	"errors"
	"time"

	"mercatus/inventory/model"
	"mercatus/inventory/repository"
)

var (
	ErrInsufficientStock = repository.ErrInsufficientStock
	ErrReservationGone   = repository.ErrReservationGone
	ErrNotFound          = errors.New("stock not found")
	ErrInvalidQuantity   = errors.New("quantity must be greater than zero")
	ErrForbidden         = errors.New("you do not own this variant")
	ErrBelowReserved     = repository.ErrBelowReserved
)

const ReservationTTL = 15 * time.Minute

type InventoryService struct {
	inv *repository.InventoryRepository
}

func NewInventoryService(inv *repository.InventoryRepository) *InventoryService {
	return &InventoryService{inv: inv}
}

func (s *InventoryService) SetStockAsSeller(ctx context.Context, userID, variantID int64, onHand int) error {
	if onHand < 0 || onHand > 1_000_000 {
		return ErrInvalidQuantity
	}
	ok, err := s.inv.SellerOwnsVariant(ctx, userID, variantID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrForbidden
	}
	return s.inv.SetStock(ctx, variantID, onHand)
}

func (s *InventoryService) SetStock(ctx context.Context, variantID int64, onHand int) error {
	if onHand < 0 {
		return ErrInvalidQuantity
	}
	return s.inv.SetStock(ctx, variantID, onHand)
}

func (s *InventoryService) GetStock(ctx context.Context, variantID int64) (*model.Stock, error) {
	st, err := s.inv.GetStock(ctx, variantID)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, ErrNotFound
	}
	return st, nil
}

func (s *InventoryService) Reserve(ctx context.Context, req *model.ReserveRequest) (*model.Reservation, error) {
	if req.Quantity <= 0 {
		return nil, ErrInvalidQuantity
	}
	res := &model.Reservation{
		VariantID: req.VariantID, OrderID: req.OrderID, Quantity: req.Quantity,
		Status: "held", ExpiresAt: time.Now().Add(ReservationTTL), CreatedAt: time.Now(),
	}
	if err := s.inv.ReserveOne(ctx, res); err != nil {
		return nil, err
	}
	return res, nil
}

func (s *InventoryService) ReserveOrder(ctx context.Context, orderID int64, lines []model.Line) error {
	if len(lines) == 0 {
		return ErrInvalidQuantity
	}
	for _, l := range lines {
		if l.Quantity <= 0 {
			return ErrInvalidQuantity
		}
	}
	return s.inv.ReserveOrder(ctx, orderID, lines, ReservationTTL)
}

func (s *InventoryService) Commit(ctx context.Context, orderID int64) error {
	return s.inv.CommitOrder(ctx, orderID)
}

func (s *InventoryService) ReleaseOrder(ctx context.Context, orderID int64) error {
	return s.inv.ReleaseOrder(ctx, orderID)
}

func (s *InventoryService) RestockOrder(ctx context.Context, orderID int64) error {
	return s.inv.RestockOrder(ctx, orderID)
}

func (s *InventoryService) ExpireStale(ctx context.Context) (int, error) {
	total := 0
	for {
		n, err := s.inv.ExpireStaleReservations(ctx, time.Now(), 500)
		total += n
		if err != nil || n < 500 {
			return total, err
		}
	}
}
