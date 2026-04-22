package psp

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"
)

const razorpayMockName = "razorpay_mock"

// razorpaySupportedCurrencies: Razorpay mock only processes INR (amount in paise).
var razorpaySupportedCurrencies = map[string]struct{}{
	"INR": {},
}

// RazorpayMockConfig controls simulated latency and random failures for RazorpayMock.
// Zero values are safe: no artificial latency, no simulated failures.
type RazorpayMockConfig struct {
	MinLatency time.Duration
	MaxLatency time.Duration

	ChargeFailureRate        float64
	RefundFailureRate        float64
	RetryableFailureFraction float64
	DeclinedFraction         float64
}

// DefaultRazorpayMockConfig returns a config with no latency and no failures.
func DefaultRazorpayMockConfig() RazorpayMockConfig {
	return RazorpayMockConfig{
		RetryableFailureFraction: 0.4,
		DeclinedFraction:         0.7,
	}
}

// RazorpayMock is a Razorpay-like PSP mock for INR with configurable latency and failure rates.
type RazorpayMock struct {
	cfg RazorpayMockConfig
}

// NewRazorpayMock builds a RazorpayMock. Invalid rates are clamped to [0,1].
func NewRazorpayMock(cfg RazorpayMockConfig) *RazorpayMock {
	cfg.ChargeFailureRate = clamp01(cfg.ChargeFailureRate)
	cfg.RefundFailureRate = clamp01(cfg.RefundFailureRate)
	cfg.RetryableFailureFraction = clamp01(cfg.RetryableFailureFraction)
	cfg.DeclinedFraction = clamp01(cfg.DeclinedFraction)
	if cfg.MinLatency < 0 {
		cfg.MinLatency = 0
	}
	if cfg.MaxLatency < 0 {
		cfg.MaxLatency = 0
	}
	if cfg.MaxLatency < cfg.MinLatency {
		cfg.MaxLatency = cfg.MinLatency
	}
	return &RazorpayMock{cfg: cfg}
}

func (m *RazorpayMock) Name() string {
	return razorpayMockName
}

// Charge simulates a Razorpay payment capture for INR (paise).
func (m *RazorpayMock) Charge(ctx context.Context, req ChargeRequest) (*ChargeResponse, error) {
	if err := sleepStripeMock(ctx, m.cfg.MinLatency, m.cfg.MaxLatency); err != nil {
		return nil, err
	}
	cur := strings.ToUpper(strings.TrimSpace(req.Currency))
	if _, ok := razorpaySupportedCurrencies[cur]; !ok {
		return nil, NewError(CodeInvalidRequest, fmt.Sprintf("unsupported currency for razorpay mock (INR only): %q", req.Currency), false)
	}

	if rand.Float64() < m.cfg.ChargeFailureRate {
		return m.simulateChargeFailure()
	}

	payID := newRazorpayPaymentID()
	orderID := newRazorpayOrderID()
	raw, _ := json.Marshal(map[string]any{
		"entity":    "payment",
		"id":        payID,
		"order_id":  orderID,
		"amount":    req.Amount,
		"currency":  "INR",
		"status":    "captured",
		"method":    "card",
		"description": "mock razorpay capture",
		"captured":  true,
	})
	return &ChargeResponse{
		PSPReferenceID: payID,
		Status:         ChargeStatusCaptured,
		RawResponse:    raw,
	}, nil
}

func (m *RazorpayMock) simulateChargeFailure() (*ChargeResponse, error) {
	if rand.Float64() < m.cfg.RetryableFailureFraction {
		code := []string{CodeNetworkError, CodeRateLimited, CodeServiceUnavailable}[rand.IntN(3)]
		return nil, NewError(code, "simulated razorpay transient failure", true)
	}
	if rand.Float64() < m.cfg.DeclinedFraction {
		raw, _ := json.Marshal(map[string]any{
			"error": map[string]any{
				"code":        "BAD_REQUEST_ERROR",
				"description": "Payment failed",
				"source":      "customer",
				"step":        "payment_authorization",
				"reason":      "payment_failed",
			},
		})
		return &ChargeResponse{
			Status:      ChargeStatusDeclined,
			RawResponse: raw,
		}, nil
	}
	return nil, NewError(CodeInsufficientFunds, "simulated insufficient funds", false)
}

// Refund simulates a Razorpay refund for INR.
func (m *RazorpayMock) Refund(ctx context.Context, req RefundRequest) (*RefundResponse, error) {
	if err := sleepStripeMock(ctx, m.cfg.MinLatency, m.cfg.MaxLatency); err != nil {
		return nil, err
	}
	cur := strings.ToUpper(strings.TrimSpace(req.Currency))
	if _, ok := razorpaySupportedCurrencies[cur]; !ok {
		return nil, NewError(CodeInvalidRequest, fmt.Sprintf("unsupported currency for razorpay mock (INR only): %q", req.Currency), false)
	}

	if rand.Float64() < m.cfg.RefundFailureRate {
		if rand.Float64() < 0.5 {
			return nil, NewError(CodeNetworkError, "simulated razorpay refund transient failure", true)
		}
		return nil, NewError(CodeInvalidRequest, "simulated razorpay refund rejected", false)
	}

	refundID := newRazorpayRefundID()
	raw, _ := json.Marshal(map[string]any{
		"entity":   "refund",
		"id":       refundID,
		"payment_id": req.PSPReferenceID,
		"amount":   req.Amount,
		"currency": "INR",
		"status":   "processed",
		"notes":    map[string]string{},
	})
	return &RefundResponse{
		PSPRefundID: refundID,
		Status:      RefundStatusSucceeded,
		RawResponse: raw,
	}, nil
}

func newRazorpayPaymentID() string {
	return "pay_" + randomRazorpaySuffix(14)
}

func newRazorpayOrderID() string {
	return "order_" + randomRazorpaySuffix(14)
}

func newRazorpayRefundID() string {
	return "rfnd_" + randomRazorpaySuffix(14)
}

func randomRazorpaySuffix(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[rand.IntN(len(alphabet))]
	}
	return string(b)
}
