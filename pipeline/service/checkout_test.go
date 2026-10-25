package service

import (
	"context"
	"errors"
	"testing"

	"mercatus/pipeline/repository"
	"mercatus/platform/database/dbtest"
)

func fraudDeps(decision FraudDecision, charged, cancelled *int) Deps {
	return Deps{
		CartLines: func(context.Context, int64) ([]Line, error) { return []Line{{VariantID: 1, Quantity: 1}}, nil },
		ClearCart: func(context.Context, int64) error { return nil },
		CreateOrder: func(context.Context, int64, []Line) (*OrderSummary, error) {
			return &OrderSummary{ID: 7, TotalCents: 100, Currency: "USD"}, nil
		},
		CancelOrder: func(context.Context, int64) error { *cancelled++; return nil },
		MarkPaid:    func(context.Context, int64) error { return nil },
		Reserve:     func(context.Context, int64, []Line) error { return nil },
		Release:     func(context.Context, int64) error { return nil },
		Commit:      func(context.Context, int64) error { return nil },
		Charge: func(context.Context, string, int64, *OrderSummary) (int64, error) {
			*charged++
			return 1, nil
		},
		RefundOrder: func(context.Context, int64) error { return nil },
		Fraud:       func(context.Context, int64, int64) (FraudDecision, error) { return decision, nil },
	}
}

func TestFraudReviewHoldsOrderBeforeCharge(t *testing.T) {
	db := dbtest.New(t)
	sagas := repository.NewSagaRepository(db)
	var charged, cancelled int
	svc := NewCheckoutService(NewSagaService(sagas), sagas, fraudDeps(FraudReview, &charged, &cancelled))
	res, err := svc.Checkout(context.Background(), 1, "review-key-1")
	if !errors.Is(err, ErrFraudReview) {
		t.Fatalf("want review error, got %v", err)
	}
	if charged != 0 || cancelled != 1 {
		t.Fatalf("charged=%d cancelled=%d", charged, cancelled)
	}
	if res.Error != "this order needs a manual check before it can go through" {
		t.Fatalf("public error %q", res.Error)
	}
}

func TestFraudApproveCharges(t *testing.T) {
	db := dbtest.New(t)
	sagas := repository.NewSagaRepository(db)
	var charged, cancelled int
	svc := NewCheckoutService(NewSagaService(sagas), sagas, fraudDeps(FraudApprove, &charged, &cancelled))
	if _, err := svc.Checkout(context.Background(), 1, "approve-key-1"); err != nil {
		t.Fatal(err)
	}
	if charged != 1 || cancelled != 0 {
		t.Fatalf("charged=%d cancelled=%d", charged, cancelled)
	}
}

func TestMarkPaidFailureRestocksCommittedStock(t *testing.T) {
	db := dbtest.New(t)
	sagas := repository.NewSagaRepository(db)
	var charged, cancelled, restocked, refunded int
	d := fraudDeps(FraudApprove, &charged, &cancelled)
	d.MarkPaid = func(context.Context, int64) error { return errors.New("db down") }
	d.Restock = func(context.Context, int64) error { restocked++; return nil }
	d.RefundOrder = func(context.Context, int64) error { refunded++; return nil }
	svc := NewCheckoutService(NewSagaService(sagas), sagas, d)
	if _, err := svc.Checkout(context.Background(), 1, "markpaid-key-1"); !errors.Is(err, ErrCheckoutFailed) {
		t.Fatalf("want checkout failure, got %v", err)
	}
	if restocked != 1 || refunded == 0 || cancelled != 1 {
		t.Fatalf("restocked=%d refunded=%d cancelled=%d", restocked, refunded, cancelled)
	}
}
