package psp

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/sony/gobreaker"
)

type failCounterAdapter struct {
	name  string
	calls atomic.Int32
}

func (a *failCounterAdapter) Name() string { return a.name }

func (a *failCounterAdapter) Charge(context.Context, ChargeRequest) (*ChargeResponse, error) {
	a.calls.Add(1)
	return nil, NewError(CodeNetworkError, "always fail", true)
}

func (a *failCounterAdapter) Refund(context.Context, RefundRequest) (*RefundResponse, error) {
	a.calls.Add(1)
	return nil, NewError(CodeNetworkError, "always fail", true)
}

func TestCircuitBreakerAdapter_OpensAfterFiveFailures_FastFailsWithoutCallingAdapter(t *testing.T) {
	inner := &failCounterAdapter{name: "test_psp"}
	cb := NewCircuitBreakerAdapter(inner)
	ctx := context.Background()
	req := ChargeRequest{PaymentID: uuid.New(), Amount: 100, Currency: "USD"}

	for i := 0; i < 5; i++ {
		_, err := cb.Charge(ctx, req)
		if err == nil {
			t.Fatalf("attempt %d: expected error", i+1)
		}
	}

	if inner.calls.Load() != 5 {
		t.Fatalf("inner Charge calls = %d want 5", inner.calls.Load())
	}

	_, err := cb.Charge(ctx, req)
	if !errors.Is(err, gobreaker.ErrOpenState) {
		t.Fatalf("expected ErrOpenState, got %v", err)
	}
	if inner.calls.Load() != 5 {
		t.Fatalf("open state should not call inner adapter; calls = %d", inner.calls.Load())
	}
}

func TestCircuitBreakerAdapter_NameDelegates(t *testing.T) {
	inner := &failCounterAdapter{name: "delegated"}
	cb := NewCircuitBreakerAdapter(inner)
	if cb.Name() != "delegated" {
		t.Fatalf("Name() = %q", cb.Name())
	}
}
