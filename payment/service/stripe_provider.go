package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"sync/atomic"
)

var ErrDeclined = errors.New("payment declined")

type Provider interface {
	Charge(ctx context.Context, idempotencyKey string, amountCents int64, currency string) (providerRef string, err error)
	Lookup(ctx context.Context, idempotencyKey string) (providerRef string, err error)
	Refund(ctx context.Context, idempotencyKey, providerRef string, amountCents int64) error
}

type TestStripeProvider struct {
	decline  atomic.Bool
	mu       sync.Mutex
	charges  map[string]string
	refunded map[string]bool
}

func NewTestStripeProvider() *TestStripeProvider {
	return &TestStripeProvider{charges: map[string]string{}, refunded: map[string]bool{}}
}

func (p *TestStripeProvider) SetDecline(v bool) { p.decline.Store(v) }

func (p *TestStripeProvider) Charge(ctx context.Context, idempotencyKey string, amountCents int64, currency string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if p.decline.Load() {
		return "", ErrDeclined
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if ref, ok := p.charges[idempotencyKey]; ok {
		return ref, nil
	}
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	ref := "pi_test_" + hex.EncodeToString(b)
	p.charges[idempotencyKey] = ref
	return ref, nil
}

func (p *TestStripeProvider) Lookup(ctx context.Context, idempotencyKey string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.charges[idempotencyKey], nil
}

func (p *TestStripeProvider) Refund(ctx context.Context, idempotencyKey, providerRef string, amountCents int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refunded[idempotencyKey] = true
	return nil
}
