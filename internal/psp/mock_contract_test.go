package psp

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestStripeMock_Name(t *testing.T) {
	m := NewStripeMock(DefaultStripeMockConfig())
	if m.Name() != AdapterStripeMock {
		t.Fatalf("Name() = %q want %q", m.Name(), AdapterStripeMock)
	}
}

func TestStripeMock_Charge_successShape(t *testing.T) {
	cfg := DefaultStripeMockConfig()
	cfg.ChargeFailureRate = 0
	m := NewStripeMock(cfg)
	ctx := context.Background()
	pid := uuid.MustParse("11111111-1111-1111-1111-111111111111")

	resp, err := m.Charge(ctx, ChargeRequest{
		PaymentID:      pid,
		Amount:         999,
		Currency:       "usd",
		IdempotencyKey: "idem-1",
	})
	if err != nil {
		t.Fatalf("Charge: %v", err)
	}
	if resp.Status != ChargeStatusCaptured {
		t.Fatalf("Status = %s want %s", resp.Status, ChargeStatusCaptured)
	}
	if !strings.HasPrefix(resp.PSPReferenceID, "ch_") {
		t.Fatalf("PSPReferenceID should be Stripe-like, got %q", resp.PSPReferenceID)
	}
	if len(resp.RawResponse) == 0 {
		t.Fatal("expected RawResponse JSON")
	}
}

func TestStripeMock_Charge_unsupportedCurrency(t *testing.T) {
	m := NewStripeMock(DefaultStripeMockConfig())
	_, err := m.Charge(context.Background(), ChargeRequest{
		PaymentID: uuid.New(),
		Currency:  "INR",
	})
	var pe *Error
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.As(err, &pe) {
		t.Fatalf("expected *psp.Error, got %T: %v", err, err)
	}
	if pe.Code != CodeInvalidRequest {
		t.Fatalf("code = %q", pe.Code)
	}
	if pe.Retryable {
		t.Fatal("unsupported currency should not be retryable")
	}
}

func TestStripeMock_Refund_successShape(t *testing.T) {
	cfg := DefaultStripeMockConfig()
	cfg.RefundFailureRate = 0
	m := NewStripeMock(cfg)
	resp, err := m.Refund(context.Background(), RefundRequest{
		PaymentID:      uuid.New(),
		PSPReferenceID: "ch_test",
		Amount:         500,
		Currency:       "EUR",
		IdempotencyKey: "idem-r",
	})
	if err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if resp.Status != RefundStatusSucceeded {
		t.Fatalf("Status = %s", resp.Status)
	}
	if !strings.HasPrefix(resp.PSPRefundID, "re_") {
		t.Fatalf("PSPRefundID = %q", resp.PSPRefundID)
	}
}

func TestStripeMock_Refund_unsupportedCurrency(t *testing.T) {
	m := NewStripeMock(DefaultStripeMockConfig())
	_, err := m.Refund(context.Background(), RefundRequest{
		PaymentID:      uuid.New(),
		PSPReferenceID: "ch_x",
		Currency:       "INR",
	})
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("expected *psp.Error, got %v", err)
	}
	if pe.Code != CodeInvalidRequest {
		t.Fatalf("code = %q", pe.Code)
	}
}

func TestStripeMock_Config_clampsRatesAndLatency(t *testing.T) {
	cfg := StripeMockConfig{
		ChargeFailureRate:        2,
		RefundFailureRate:        -1,
		RetryableFailureFraction: math.NaN(),
		DeclinedFraction:         3,
		MinLatency:               -1 * time.Second,
		MaxLatency:               -5 * time.Second,
	}
	m := NewStripeMock(cfg).cfg
	if m.ChargeFailureRate != 1 {
		t.Fatalf("ChargeFailureRate clamp: %v", m.ChargeFailureRate)
	}
	if m.RefundFailureRate != 0 {
		t.Fatalf("RefundFailureRate clamp: %v", m.RefundFailureRate)
	}
	if m.RetryableFailureFraction != 0 {
		t.Fatalf("NaN fraction should become 0, got %v", m.RetryableFailureFraction)
	}
	if m.DeclinedFraction != 1 {
		t.Fatalf("DeclinedFraction clamp: %v", m.DeclinedFraction)
	}
	if m.MinLatency != 0 || m.MaxLatency != 0 {
		t.Fatalf("latency: min=%v max=%v", m.MinLatency, m.MaxLatency)
	}
}

func TestRazorpayMock_Name(t *testing.T) {
	m := NewRazorpayMock(DefaultRazorpayMockConfig())
	if m.Name() != AdapterRazorpayMock {
		t.Fatalf("Name() = %q", m.Name())
	}
}

func TestRazorpayMock_Charge_successShape(t *testing.T) {
	cfg := DefaultRazorpayMockConfig()
	cfg.ChargeFailureRate = 0
	m := NewRazorpayMock(cfg)
	resp, err := m.Charge(context.Background(), ChargeRequest{
		PaymentID: uuid.New(),
		Amount:    12_345,
		Currency:  "inr",
	})
	if err != nil {
		t.Fatalf("Charge: %v", err)
	}
	if resp.Status != ChargeStatusCaptured {
		t.Fatalf("Status = %s", resp.Status)
	}
	if !strings.HasPrefix(resp.PSPReferenceID, "pay_") {
		t.Fatalf("PSPReferenceID = %q", resp.PSPReferenceID)
	}
}

func TestRazorpayMock_Charge_unsupportedCurrency(t *testing.T) {
	m := NewRazorpayMock(DefaultRazorpayMockConfig())
	_, err := m.Charge(context.Background(), ChargeRequest{
		PaymentID: uuid.New(),
		Currency:  "USD",
	})
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("expected *psp.Error: %v", err)
	}
	if pe.Code != CodeInvalidRequest || pe.Retryable {
		t.Fatalf("unexpected error: %#v", pe)
	}
}

func TestRazorpayMock_Refund_successShape(t *testing.T) {
	cfg := DefaultRazorpayMockConfig()
	cfg.RefundFailureRate = 0
	m := NewRazorpayMock(cfg)
	resp, err := m.Refund(context.Background(), RefundRequest{
		PaymentID:      uuid.New(),
		PSPReferenceID: "pay_abc",
		Amount:         100,
		Currency:       "INR",
	})
	if err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if resp.Status != RefundStatusSucceeded {
		t.Fatalf("Status = %s", resp.Status)
	}
	if !strings.HasPrefix(resp.PSPRefundID, "rfnd_") {
		t.Fatalf("PSPRefundID = %q", resp.PSPRefundID)
	}
}
