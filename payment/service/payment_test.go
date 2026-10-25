package service

import (
	"context"
	"errors"
	"sync"
	"testing"

	"mercatus/payment/model"
	"mercatus/payment/repository"
	"mercatus/platform/database/dbtest"
)

func charge() *model.ChargeRequest {
	return &model.ChargeRequest{
		IdempotencyKey: "order-1", OrderID: 1, AmountCents: 4500, Currency: "USD",
		Splits: []model.SellerSplit{
			{SellerID: 10, SubtotalCents: 4000, CommissionCents: 400},
			{SellerID: 20, SubtotalCents: 500, CommissionCents: 50},
		},
	}
}

func TestConcurrentChargesWithSameKeyChargeOnce(t *testing.T) {
	db := dbtest.New(t)
	svc := NewPaymentService(repository.NewPaymentRepository(db), NewTestStripeProvider())
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.Charge(ctx, 5, charge())
			if err != nil && !errors.Is(err, ErrInProgress) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	var payments, entries int
	db.QueryRow(`SELECT count(*) FROM payments`).Scan(&payments)
	db.QueryRow(`SELECT count(*) FROM ledger_entries`).Scan(&entries)
	if payments != 1 || entries != 4 {
		t.Fatalf("payments=%d entries=%d", payments, entries)
	}
	total, _ := svc.LedgerTotal(ctx)
	if total != 0 {
		t.Fatalf("ledger sum %d", total)
	}
	if bal, _ := svc.LedgerBalance(ctx, "seller:10"); bal != 3600 {
		t.Fatalf("seller balance %d", bal)
	}
}

func TestKeyReuseWithDifferentAmountIsRejected(t *testing.T) {
	db := dbtest.New(t)
	svc := NewPaymentService(repository.NewPaymentRepository(db), NewTestStripeProvider())
	ctx := context.Background()
	if _, err := svc.Charge(ctx, 5, charge()); err != nil {
		t.Fatal(err)
	}
	req := charge()
	req.AmountCents = 1
	req.Splits = []model.SellerSplit{{SellerID: 10, SubtotalCents: 1}}
	if _, err := svc.Charge(ctx, 5, req); !errors.Is(err, ErrKeyReuse) {
		t.Fatalf("want key reuse error, got %v", err)
	}
}

func TestRefundReversesLedgerAndIsIdempotent(t *testing.T) {
	db := dbtest.New(t)
	svc := NewPaymentService(repository.NewPaymentRepository(db), NewTestStripeProvider())
	ctx := context.Background()
	p, err := svc.Charge(ctx, 5, charge())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := svc.RefundOrder(ctx, p.OrderID); err != nil {
			t.Fatal(err)
		}
	}
	var entries int
	db.QueryRow(`SELECT count(*) FROM ledger_entries`).Scan(&entries)
	if entries != 8 {
		t.Fatalf("entries=%d want 8", entries)
	}
	if bal, _ := svc.LedgerBalance(ctx, "seller:10"); bal != 0 {
		t.Fatalf("seller balance after refund %d", bal)
	}
}

func TestDeclinedChargeFailsPayment(t *testing.T) {
	db := dbtest.New(t)
	prov := NewTestStripeProvider()
	prov.SetDecline(true)
	svc := NewPaymentService(repository.NewPaymentRepository(db), prov)
	p, err := svc.Charge(context.Background(), 5, charge())
	if !errors.Is(err, ErrPaymentFailed) || p.Status != model.StatusFailed {
		t.Fatalf("p=%+v err=%v", p, err)
	}
	var entries int
	db.QueryRow(`SELECT count(*) FROM ledger_entries`).Scan(&entries)
	if entries != 0 {
		t.Fatal("declined charge must not write ledger entries")
	}
}

type flakyProvider struct {
	onCharge func()
	failRef  bool
	refunded []string
}

func (f *flakyProvider) Charge(ctx context.Context, key string, amount int64, currency string) (string, error) {
	if f.onCharge != nil {
		f.onCharge()
	}
	return "pi_flaky_" + key, nil
}

func (f *flakyProvider) Lookup(ctx context.Context, key string) (string, error) {
	return "", nil
}

func (f *flakyProvider) Refund(ctx context.Context, key, ref string, amount int64) error {
	if f.failRef {
		return errors.New("provider refund unavailable")
	}
	f.refunded = append(f.refunded, ref)
	return nil
}

func TestCaptureFailureRefundsAtProvider(t *testing.T) {
	db := dbtest.New(t)
	prov := &flakyProvider{}
	prov.onCharge = func() { db.Exec(`ALTER TABLE ledger_entries RENAME TO ledger_entries_gone`) }
	svc := NewPaymentService(repository.NewPaymentRepository(db), prov)
	p, err := svc.Charge(context.Background(), 5, charge())
	if !errors.Is(err, ErrPaymentFailed) {
		t.Fatalf("want payment failed, got %v", err)
	}
	if p == nil || p.ID == 0 {
		t.Fatal("payment must be returned so the saga can record it")
	}
	if len(prov.refunded) != 1 || prov.refunded[0] != "pi_flaky_order-1" {
		t.Fatalf("provider refunds %v", prov.refunded)
	}
	var st string
	db.QueryRow(`SELECT status FROM payments WHERE id = $1`, p.ID).Scan(&st)
	if st != "failed" {
		t.Fatalf("status %s want failed", st)
	}
}

func TestPendingChargeWithProviderRefIsRefundedLater(t *testing.T) {
	db := dbtest.New(t)
	prov := &flakyProvider{failRef: true}
	prov.onCharge = func() { db.Exec(`ALTER TABLE ledger_entries RENAME TO ledger_entries_gone`) }
	svc := NewPaymentService(repository.NewPaymentRepository(db), prov)
	p, err := svc.Charge(context.Background(), 5, charge())
	if err == nil {
		t.Fatal("expected failure")
	}
	var ref, pending string
	db.QueryRow(`SELECT status FROM payments WHERE id = $1`, p.ID).Scan(&pending)
	if pending != "pending" {
		t.Fatalf("unrefunded charge must stay pending, got %s", pending)
	}
	db.QueryRow(`SELECT provider_ref FROM payments WHERE id = $1`, p.ID).Scan(&ref)
	if ref != "pi_flaky_order-1" {
		t.Fatalf("provider ref not stored, got %q", ref)
	}
	prov.failRef = false
	if err := svc.RefundOrder(context.Background(), p.OrderID); err != nil {
		t.Fatal(err)
	}
	var st string
	db.QueryRow(`SELECT status FROM payments WHERE id = $1`, p.ID).Scan(&st)
	if len(prov.refunded) != 1 || st != "failed" {
		t.Fatalf("refunds=%v status=%s", prov.refunded, st)
	}
}

func TestPendingChargeWithLostRefIsFoundAndRefunded(t *testing.T) {
	db := dbtest.New(t)
	prov := NewTestStripeProvider()
	repo := repository.NewPaymentRepository(db)
	svc := NewPaymentService(repo, prov)
	ctx := context.Background()
	if _, err := prov.Charge(ctx, "order-9", 4500, "USD"); err != nil {
		t.Fatal(err)
	}
	p := &model.Payment{IdempotencyKey: "order-9", OrderID: 9, BuyerID: 5, AmountCents: 4500, Currency: "USD"}
	if err := repo.InsertPending(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := svc.RefundOrder(ctx, 9); err != nil {
		t.Fatal(err)
	}
	var st string
	db.QueryRow(`SELECT status FROM payments WHERE id = $1`, p.ID).Scan(&st)
	if st != "failed" || !prov.refunded[refundKey(p.ID)] {
		t.Fatalf("status=%s refunded=%v", st, prov.refunded)
	}
}

func TestUnknownChargeOutcomeStaysPending(t *testing.T) {
	db := dbtest.New(t)
	svc := NewPaymentService(repository.NewPaymentRepository(db), NewTestStripeProvider())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p, err := svc.Charge(ctx, 5, charge())
	if !errors.Is(err, ErrPaymentFailed) || p == nil || p.Status != model.StatusPending {
		t.Fatalf("p=%+v err=%v", p, err)
	}
}
