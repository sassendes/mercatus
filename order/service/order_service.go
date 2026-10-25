package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"mercatus/order/model"
	"mercatus/order/repository"
)

var (
	ErrEmptyOrder   = errors.New("order has no items")
	ErrInvalidItem  = errors.New("order item is invalid")
	ErrNotFound     = errors.New("order not found")
	ErrUnavailable  = errors.New("an item in your cart is no longer available")
	ErrNotSeller    = errors.New("seller account required")
	ErrOwnProduct   = errors.New("you cannot buy your own product")
	ErrInvalidState = repository.ErrInvalidTransition
)

type UnavailableError struct{ VariantID int64 }

func (e *UnavailableError) Error() string {
	return fmt.Sprintf("variant %d is no longer available", e.VariantID)
}
func (e *UnavailableError) Unwrap() error { return ErrUnavailable }

type OrderService struct {
	orders *repository.OrderRepository
}

func NewOrderService(o *repository.OrderRepository) *OrderService {
	return &OrderService{orders: o}
}

func (s *OrderService) Create(ctx context.Context, buyerID int64, lines []model.Line) (*model.Order, error) {
	if len(lines) == 0 {
		return nil, ErrEmptyOrder
	}
	ids := make([]int64, 0, len(lines))
	for _, l := range lines {
		if l.Quantity <= 0 || l.VariantID <= 0 {
			return nil, ErrInvalidItem
		}
		ids = append(ids, l.VariantID)
	}
	priced, err := s.orders.PriceLines(ctx, ids)
	if err != nil {
		return nil, err
	}

	bySeller := map[int64]*model.SubOrder{}
	for _, l := range lines {
		p, ok := priced[l.VariantID]
		if !ok || !p.Purchasable {
			return nil, &UnavailableError{VariantID: l.VariantID}
		}
		if p.Currency != "USD" {
			return nil, ErrInvalidItem
		}
		if p.SellerUserID == buyerID {
			return nil, ErrOwnProduct
		}
		so, ok := bySeller[p.SellerID]
		if !ok {
			so = &model.SubOrder{SellerID: p.SellerID, Status: "pending", Currency: "USD"}
			bySeller[p.SellerID] = so
		}
		lineTotal := p.UnitPriceCents * int64(l.Quantity)
		so.SubtotalCents += lineTotal
		so.CommissionCents += lineTotal * int64(p.CommissionBps) / 10000
		so.Items = append(so.Items, model.OrderItem{
			VariantID: l.VariantID, ProductID: p.ProductID, Title: p.Title, Quantity: l.Quantity,
			UnitPriceCents: p.UnitPriceCents, Currency: "USD",
		})
	}

	order := &model.Order{BuyerID: buyerID, Status: "pending", Currency: "USD", CreatedAt: time.Now()}
	sellers := make([]int64, 0, len(bySeller))
	for id := range bySeller {
		sellers = append(sellers, id)
	}
	sort.Slice(sellers, func(i, j int) bool { return sellers[i] < sellers[j] })
	for _, id := range sellers {
		so := bySeller[id]
		order.TotalCents += so.SubtotalCents
		order.ItemCount += len(so.Items)
		order.SubOrders = append(order.SubOrders, *so)
	}
	if err := s.orders.CreateOrder(ctx, order); err != nil {
		return nil, err
	}
	return order, nil
}

func (s *OrderService) Get(ctx context.Context, orderID, buyerID int64) (*model.Order, error) {
	o, err := s.orders.GetOrder(ctx, orderID, buyerID)
	if err != nil {
		return nil, err
	}
	if o == nil {
		return nil, ErrNotFound
	}
	return o, nil
}

func (s *OrderService) ListForBuyer(ctx context.Context, buyerID int64) ([]*model.Order, error) {
	return s.orders.ListOrdersForBuyer(ctx, buyerID)
}

func (s *OrderService) ListSales(ctx context.Context, userID int64) ([]*model.SubOrder, error) {
	sellerID, err := s.orders.SellerIDForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	if sellerID == 0 {
		return nil, ErrNotSeller
	}
	return s.orders.ListSales(ctx, sellerID)
}

func (s *OrderService) Splits(ctx context.Context, orderID int64) ([]model.SellerSplit, error) {
	return s.orders.Splits(ctx, orderID)
}

func (s *OrderService) MarkPaid(ctx context.Context, orderID int64) error {
	return s.orders.Transition(ctx, orderID, "pending", "paid")
}

func (s *OrderService) Cancel(ctx context.Context, orderID int64) error {
	return s.orders.Transition(ctx, orderID, "pending", "cancelled")
}

func (s *OrderService) Status(ctx context.Context, orderID int64) (string, error) {
	return s.orders.Status(ctx, orderID)
}

func (s *OrderService) PurchasedProduct(ctx context.Context, buyerID, productID int64) (bool, error) {
	return s.orders.PurchasedProduct(ctx, buyerID, productID)
}

func (s *OrderService) SellerUserIDs(ctx context.Context, orderID int64) ([]int64, error) {
	return s.orders.SellerUserIDs(ctx, orderID)
}
