package service

import (
	"context"
	"errors"
	"strconv"

	"mercatus/payment/model"
	"mercatus/payment/repository"
)

var (
	ErrInvalidAmount  = errors.New("amount must be positive")
	ErrSplitMismatch  = errors.New("seller splits do not sum to amount")
	ErrMissingKey     = errors.New("idempotency key is required")
	ErrInProgress     = errors.New("a payment with this key is already in progress")
	ErrPaymentFailed  = errors.New("payment failed")
	ErrKeyReuse       = errors.New("idempotency key reused with different parameters")
	ErrNotRefundable  = errors.New("payment is not refundable")
	ErrInvalidAccount = errors.New("invalid ledger account")
)

type PaymentService struct {
	payments *repository.PaymentRepository
	provider Provider
}

func NewPaymentService(p *repository.PaymentRepository, provider Provider) *PaymentService {
	return &PaymentService{payments: p, provider: provider}
}

func (s *PaymentService) replay(existing *model.Payment, buyerID int64, req *model.ChargeRequest) (*model.Payment, error) {
	if existing.BuyerID != buyerID || existing.OrderID != req.OrderID || existing.AmountCents != req.AmountCents {
		return nil, ErrKeyReuse
	}
	switch existing.Status {
	case model.StatusCaptured:
		return existing, nil
	case model.StatusPending:
		return existing, ErrInProgress
	default:
		return existing, ErrPaymentFailed
	}
}

func (s *PaymentService) Charge(ctx context.Context, buyerID int64, req *model.ChargeRequest) (*model.Payment, error) {
	if req.IdempotencyKey == "" {
		return nil, ErrMissingKey
	}
	if req.AmountCents <= 0 {
		return nil, ErrInvalidAmount
	}
	currency := req.Currency
	if currency == "" {
		currency = "USD"
	}
	var splitSum int64
	for _, sp := range req.Splits {
		if sp.SubtotalCents < 0 || sp.CommissionCents < 0 || sp.CommissionCents > sp.SubtotalCents {
			return nil, ErrSplitMismatch
		}
		splitSum += sp.SubtotalCents
	}
	if splitSum != req.AmountCents {
		return nil, ErrSplitMismatch
	}

	if existing, err := s.payments.FindByIdempotencyKey(ctx, req.IdempotencyKey); err != nil {
		return nil, err
	} else if existing != nil {
		return s.replay(existing, buyerID, req)
	}

	p := &model.Payment{
		IdempotencyKey: req.IdempotencyKey, OrderID: req.OrderID, BuyerID: buyerID,
		AmountCents: req.AmountCents, Currency: currency,
	}
	if err := s.payments.InsertPending(ctx, p); err != nil {
		if errors.Is(err, repository.ErrDuplicateKey) {
			existing, ferr := s.payments.FindByIdempotencyKey(ctx, req.IdempotencyKey)
			if ferr != nil {
				return nil, ferr
			}
			if existing == nil {
				return nil, err
			}
			return s.replay(existing, buyerID, req)
		}
		return nil, err
	}

	providerRef, err := s.provider.Charge(ctx, req.IdempotencyKey, req.AmountCents, currency)
	if err != nil {
		if errors.Is(err, ErrDeclined) {
			_ = s.payments.Fail(context.WithoutCancel(ctx), p.ID)
			p.Status = model.StatusFailed
		}
		return p, errors.Join(ErrPaymentFailed, err)
	}

	bg := context.WithoutCancel(ctx)
	p.ProviderRef = providerRef
	if err := s.payments.SetProviderRef(bg, p.ID, providerRef); err != nil {
		return p, s.undoCharge(bg, p, err)
	}

	entries := buildLedger(p.ID, buyerID, currency, req)
	if err := s.payments.Capture(bg, p.ID, providerRef, entries); err != nil {
		return p, s.undoCharge(bg, p, err)
	}
	p.Status = model.StatusCaptured
	p.ProviderRef = providerRef
	return p, nil
}

func (s *PaymentService) undoCharge(ctx context.Context, p *model.Payment, cause error) error {
	if err := s.provider.Refund(ctx, refundKey(p.ID), p.ProviderRef, p.AmountCents); err != nil {
		return errors.Join(ErrPaymentFailed, cause, err)
	}
	if err := s.payments.Fail(ctx, p.ID); err != nil {
		return errors.Join(ErrPaymentFailed, cause, err)
	}
	p.Status = model.StatusFailed
	return errors.Join(ErrPaymentFailed, cause)
}

func buildLedger(paymentID, buyerID int64, currency string, req *model.ChargeRequest) []model.LedgerEntry {
	entries := []model.LedgerEntry{
		{PaymentID: paymentID, Account: AccountBuyer(buyerID), AmountCents: -req.AmountCents, Currency: currency},
	}
	var totalCommission int64
	for _, sp := range req.Splits {
		entries = append(entries, model.LedgerEntry{
			PaymentID: paymentID, Account: AccountSeller(sp.SellerID), AmountCents: sp.SubtotalCents - sp.CommissionCents, Currency: currency,
		})
		totalCommission += sp.CommissionCents
	}
	return append(entries, model.LedgerEntry{
		PaymentID: paymentID, Account: AccountPlatform, AmountCents: totalCommission, Currency: currency,
	})
}

func refundKey(paymentID int64) string { return "refund-" + strconv.FormatInt(paymentID, 10) }

func (s *PaymentService) Refund(ctx context.Context, paymentID int64) error {
	err := s.payments.WithLocked(ctx, paymentID, func(l *repository.LockedPayment) error {
		p := l.P
		switch p.Status {
		case model.StatusRefunded, model.StatusFailed:
			return nil
		case model.StatusPending:
			ref := p.ProviderRef
			if ref == "" {
				found, err := s.provider.Lookup(ctx, p.IdempotencyKey)
				if err != nil {
					return err
				}
				ref = found
			}
			if ref != "" {
				if err := s.provider.Refund(ctx, refundKey(p.ID), ref, p.AmountCents); err != nil {
					return err
				}
			}
			return l.Fail(ctx)
		}
		if err := s.provider.Refund(ctx, refundKey(p.ID), p.ProviderRef, p.AmountCents); err != nil {
			return err
		}
		original, err := l.Entries(ctx)
		if err != nil {
			return err
		}
		reversal := make([]model.LedgerEntry, 0, len(original))
		for _, e := range original {
			reversal = append(reversal, model.LedgerEntry{PaymentID: p.ID, Account: e.Account, AmountCents: -e.AmountCents, Currency: e.Currency})
		}
		return l.Refund(ctx, reversal)
	})
	if errors.Is(err, repository.ErrNotTransitable) {
		return ErrNotRefundable
	}
	return err
}

func (s *PaymentService) RefundOrder(ctx context.Context, orderID int64) error {
	payments, err := s.payments.FindCapturedForOrder(ctx, orderID)
	if err != nil {
		return err
	}
	var errs []error
	for _, p := range payments {
		errs = append(errs, s.Refund(ctx, p.ID))
	}
	return errors.Join(errs...)
}

func (s *PaymentService) LedgerBalance(ctx context.Context, account string) (int64, error) {
	if !validAccount(account) {
		return 0, ErrInvalidAccount
	}
	return s.payments.LedgerBalance(ctx, account)
}

func (s *PaymentService) LedgerSumForPayment(ctx context.Context, paymentID int64) (int64, error) {
	return s.payments.LedgerSumForPayment(ctx, paymentID)
}

func (s *PaymentService) LedgerTotal(ctx context.Context) (int64, error) {
	return s.payments.LedgerTotal(ctx)
}

const AccountPlatform = "platform:commission"

func AccountBuyer(id int64) string  { return "buyer:" + strconv.FormatInt(id, 10) }
func AccountSeller(id int64) string { return "seller:" + strconv.FormatInt(id, 10) }

func validAccount(a string) bool {
	if a == AccountPlatform {
		return true
	}
	for _, prefix := range []string{"buyer:", "seller:"} {
		if len(a) > len(prefix) && a[:len(prefix)] == prefix {
			_, err := strconv.ParseInt(a[len(prefix):], 10, 64)
			return err == nil
		}
	}
	return false
}
