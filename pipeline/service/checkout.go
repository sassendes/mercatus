package service

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"mercatus/pipeline/model"
	"mercatus/pipeline/repository"
)

var (
	ErrEmptyCart       = errors.New("your cart is empty")
	ErrFraudDeclined   = errors.New("this order could not be processed")
	ErrFraudReview     = errors.New("this order needs a manual review")
	ErrCheckoutFailed  = errors.New("checkout failed")
	ErrInvalidKey      = errors.New("idempotency key must be 8-100 characters")
	ErrSagaNotFound    = errors.New("checkout not found")
	ErrCheckoutRunning = errors.New("checkout already in progress")
)

type Downstream struct {
	Reserve func(ctx context.Context) error
	Release func(ctx context.Context) error
	Charge  func(ctx context.Context) error
	Refund  func(ctx context.Context) error
	Confirm func(ctx context.Context) error
}

func BuildCheckoutSteps(d Downstream, failAt string) []Step {
	wrap := func(name string, real func(ctx context.Context) error) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			if failAt == name {
				return errors.New("forced failure at " + name)
			}
			if real != nil {
				return real(ctx)
			}
			return nil
		}
	}
	return []Step{
		{Name: "reserve", Do: wrap("reserve", d.Reserve), Undo: d.Release},
		{Name: "charge", Do: wrap("charge", d.Charge), Undo: d.Refund},
		{Name: "confirm", Do: wrap("confirm", d.Confirm)},
	}
}

type Line struct {
	VariantID int64
	Quantity  int
}

type Split struct {
	SellerID        int64
	SubtotalCents   int64
	CommissionCents int64
}

type OrderSummary struct {
	ID         int64
	TotalCents int64
	Currency   string
	Splits     []Split
}

type FraudDecision string

const (
	FraudApprove FraudDecision = "approve"
	FraudReview  FraudDecision = "review"
	FraudDecline FraudDecision = "decline"
)

type Deps struct {
	CartLines   func(ctx context.Context, buyerID int64) ([]Line, error)
	ClearCart   func(ctx context.Context, buyerID int64) error
	CreateOrder func(ctx context.Context, buyerID int64, lines []Line) (*OrderSummary, error)
	OrderStatus func(ctx context.Context, orderID int64) (string, error)
	CancelOrder func(ctx context.Context, orderID int64) error
	MarkPaid    func(ctx context.Context, orderID int64) error
	Reserve     func(ctx context.Context, orderID int64, lines []Line) error
	Release     func(ctx context.Context, orderID int64) error
	Commit      func(ctx context.Context, orderID int64) error
	Restock     func(ctx context.Context, orderID int64) error
	Charge      func(ctx context.Context, key string, buyerID int64, order *OrderSummary) (int64, error)
	RefundOrder func(ctx context.Context, orderID int64) error
	Fraud       func(ctx context.Context, buyerID int64, amountCents int64) (FraudDecision, error)
	Blocked     func(ctx context.Context, buyerID int64) (bool, error)
	Notify      func(ctx context.Context, buyerID int64, subject, body string)
}

type CheckoutService struct {
	engine  *SagaService
	sagas   *repository.SagaRepository
	deps    Deps
	timeout time.Duration
}

func NewCheckoutService(engine *SagaService, sagas *repository.SagaRepository, deps Deps) *CheckoutService {
	return &CheckoutService{engine: engine, sagas: sagas, deps: deps, timeout: 30 * time.Second}
}

func resultOf(s *model.Saga) *model.CheckoutResult {
	return &model.CheckoutResult{SagaID: s.ID, OrderID: s.OrderID, Status: s.Status, Error: publicError(s.Error)}
}

func publicError(msg string) string {
	switch {
	case msg == "":
		return ""
	case strings.Contains(msg, "insufficient stock"):
		return "one or more items are out of stock"
	case strings.Contains(msg, "declined"):
		return "your payment was declined"
	case strings.Contains(msg, ErrFraudDeclined.Error()):
		return ErrFraudDeclined.Error()
	case strings.Contains(msg, ErrFraudReview.Error()):
		return "this order needs a manual check before it can go through"
	case strings.Contains(msg, "your own product"):
		return "you cannot buy your own product"
	case strings.Contains(msg, "no longer available"):
		return "an item in your cart is no longer available"
	default:
		return "checkout could not be completed"
	}
}

func (c *CheckoutService) Checkout(ctx context.Context, buyerID int64, key string) (*model.CheckoutResult, error) {
	key = strings.TrimSpace(key)
	if key != "" && (len(key) < 8 || len(key) > 100) {
		return nil, ErrInvalidKey
	}
	if key != "" {
		if existing, err := c.sagas.FindByKey(ctx, buyerID, key); err != nil {
			return nil, err
		} else if existing != nil {
			return resultOf(existing), nil
		}
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.timeout)
	defer cancel()

	lines, err := c.deps.CartLines(ctx, buyerID)
	if err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return nil, ErrEmptyCart
	}

	saga := &model.Saga{BuyerID: buyerID, IdempotencyKey: key}
	if err := c.sagas.Create(ctx, saga); err != nil {
		if errors.Is(err, repository.ErrDuplicateKey) {
			existing, ferr := c.sagas.FindByKey(ctx, buyerID, key)
			if ferr != nil || existing == nil {
				return nil, ErrCheckoutRunning
			}
			return resultOf(existing), nil
		}
		return nil, err
	}

	fail := func(err error) (*model.CheckoutResult, error) {
		_ = c.sagas.SetError(ctx, saga.ID, err.Error())
		_ = c.sagas.SetStatus(ctx, saga.ID, model.SagaFailed)
		saga.Status, saga.Error = model.SagaFailed, err.Error()
		return resultOf(saga), err
	}

	order, err := c.deps.CreateOrder(ctx, buyerID, lines)
	if err != nil {
		return fail(err)
	}
	saga.OrderID = order.ID
	if err := c.sagas.SetOrder(ctx, saga.ID, order.ID); err != nil {
		return fail(err)
	}

	if c.deps.Blocked != nil {
		blocked, err := c.deps.Blocked(ctx, buyerID)
		if err != nil {
			_ = c.deps.CancelOrder(ctx, order.ID)
			return fail(err)
		}
		if blocked {
			slog.WarnContext(ctx, "blocked buyer attempted checkout", "order_id", order.ID, "buyer_id", buyerID)
			_ = c.deps.CancelOrder(ctx, order.ID)
			return fail(ErrFraudDeclined)
		}
	}

	if c.deps.Fraud != nil {
		decision, err := c.deps.Fraud(ctx, buyerID, order.TotalCents)
		if err != nil {
			slog.WarnContext(ctx, "fraud check unavailable", "order_id", order.ID, "error", err)
		}
		if decision == FraudDecline {
			_ = c.deps.CancelOrder(ctx, order.ID)
			return fail(ErrFraudDeclined)
		}
		if decision == FraudReview {
			slog.WarnContext(ctx, "order held for fraud review", "order_id", order.ID, "buyer_id", buyerID)
			_ = c.deps.CancelOrder(ctx, order.ID)
			return fail(ErrFraudReview)
		}
	}

	paymentKey := "order-" + strconv.FormatInt(order.ID, 10)
	steps := []Step{
		{
			Name: "reserve",
			Do:   func(ctx context.Context) error { return c.deps.Reserve(ctx, order.ID, lines) },
			Undo: func(ctx context.Context) error { return c.deps.Release(ctx, order.ID) },
		},
		{
			Name: "charge",
			Do: func(ctx context.Context) error {
				pid, err := c.deps.Charge(ctx, paymentKey, buyerID, order)
				if pid > 0 {
					_ = c.sagas.SetPayment(ctx, saga.ID, pid)
				}
				return err
			},
			Undo: func(ctx context.Context) error { return c.deps.RefundOrder(ctx, order.ID) },
		},
		{
			Name: "commit",
			Do:   func(ctx context.Context) error { return c.deps.Commit(ctx, order.ID) },
			Undo: func(ctx context.Context) error { return c.restock(ctx, order.ID) },
		},
		{
			Name: "confirm",
			Do: func(ctx context.Context) error {
				return retry(ctx, 3, func(ctx context.Context) error { return c.deps.MarkPaid(ctx, order.ID) })
			},
		},
	}

	if err := c.engine.Run(ctx, saga, steps); err != nil {
		if rerr := c.deps.RefundOrder(ctx, order.ID); rerr != nil {
			slog.ErrorContext(ctx, "could not refund order after failed checkout", "order_id", order.ID, "error", rerr)
		}
		if cerr := c.deps.CancelOrder(ctx, order.ID); cerr != nil {
			slog.ErrorContext(ctx, "could not cancel order after failed checkout", "order_id", order.ID, "error", cerr)
		}
		saga.Error = err.Error()
		return resultOf(saga), errors.Join(ErrCheckoutFailed, err)
	}

	c.finish(ctx, buyerID, order.ID)
	return resultOf(saga), nil
}

func (c *CheckoutService) finish(ctx context.Context, buyerID, orderID int64) {
	if err := c.deps.ClearCart(ctx, buyerID); err != nil {
		slog.WarnContext(ctx, "could not clear cart after checkout", "buyer_id", buyerID, "error", err)
	}
	if c.deps.Notify != nil {
		c.deps.Notify(ctx, buyerID, "Order confirmed",
			"Your order #"+strconv.FormatInt(orderID, 10)+" is paid. Each seller delivers your software directly, check your email and this inbox.")
	}
}

func (c *CheckoutService) restock(ctx context.Context, orderID int64) error {
	if c.deps.Restock == nil {
		return nil
	}
	return c.deps.Restock(ctx, orderID)
}

func (c *CheckoutService) Get(ctx context.Context, buyerID, sagaID int64) (*model.Saga, []*model.SagaStep, error) {
	saga, steps, err := c.engine.Get(ctx, sagaID)
	if err != nil {
		return nil, nil, err
	}
	if saga == nil || saga.BuyerID != buyerID {
		return nil, nil, ErrSagaNotFound
	}
	saga.Error = publicError(saga.Error)
	return saga, steps, nil
}

func (c *CheckoutService) RecoverStale(ctx context.Context, olderThan time.Duration) (int, error) {
	stale, err := c.sagas.ClaimStale(ctx, olderThan, 50)
	if err != nil {
		return 0, err
	}
	for _, s := range stale {
		c.recover(ctx, s)
	}
	return len(stale), nil
}

func (c *CheckoutService) recover(ctx context.Context, s *model.Saga) {
	log := slog.With("saga_id", s.ID, "order_id", s.OrderID)
	if s.OrderID == 0 {
		_ = c.sagas.SetStatus(ctx, s.ID, model.SagaFailed)
		return
	}
	status, err := c.deps.OrderStatus(ctx, s.OrderID)
	if err != nil {
		log.ErrorContext(ctx, "saga recovery: could not read order", "error", err)
		_ = c.sagas.SetStatus(ctx, s.ID, model.SagaRunning)
		return
	}
	if status == "paid" {
		_ = c.sagas.SetStatus(ctx, s.ID, model.SagaCompleted)
		c.finish(ctx, s.BuyerID, s.OrderID)
		log.InfoContext(ctx, "saga recovery: order already paid, marked completed")
		return
	}
	var errs []error
	errs = append(errs, c.deps.RefundOrder(ctx, s.OrderID), c.deps.Release(ctx, s.OrderID), c.restock(ctx, s.OrderID))
	if status == "pending" {
		errs = append(errs, c.deps.CancelOrder(ctx, s.OrderID))
	}
	if err := errors.Join(errs...); err != nil {
		log.ErrorContext(ctx, "saga recovery: compensation incomplete, will retry", "error", err)
		return
	}
	_ = c.sagas.SetError(ctx, s.ID, "recovered after interruption")
	_ = c.sagas.SetStatus(ctx, s.ID, model.SagaFailed)
	log.WarnContext(ctx, "saga recovery: compensated interrupted checkout")
}

type countryKey struct{}

func WithCountry(ctx context.Context, country string) context.Context {
	return context.WithValue(ctx, countryKey{}, country)
}

func CountryFrom(ctx context.Context) string {
	c, _ := ctx.Value(countryKey{}).(string)
	return c
}

func retry(ctx context.Context, attempts int, fn func(ctx context.Context) error) error {
	var err error
	for i := 0; i < attempts; i++ {
		if err = fn(ctx); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(time.Duration(100*(i+1)) * time.Millisecond):
		}
	}
	return err
}
