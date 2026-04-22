package psp

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"strings"
	"time"
)

const stripeMockName = "stripe_mock"

// stripeSupportedCurrencies are currencies this mock accepts (Stripe-like routing).
var stripeSupportedCurrencies = map[string]struct{}{
	"USD": {},
	"EUR": {},
	"GBP": {},
}

// StripeMockConfig controls simulated latency and random failures for StripeMock.
// Zero values are safe: no artificial latency, no simulated failures.
type StripeMockConfig struct {
	// MinLatency and MaxLatency define inclusive bounds for a per-request sleep.
	// If both are zero, no latency is injected. If only MaxLatency is set, it is used as a fixed delay.
	MinLatency time.Duration
	MaxLatency time.Duration

	// ChargeFailureRate is the probability [0,1] that Charge returns a simulated failure
	// (retryable error or declined capture). Success probability is 1 - ChargeFailureRate.
	ChargeFailureRate float64

	// RefundFailureRate is the probability [0,1] that Refund returns a simulated failure.
	RefundFailureRate float64

	// RetryableFailureFraction is the probability [0,1] that a simulated charge failure
	// is a retryable *Error (network/rate limit/service unavailable). The remainder are
	// non-retryable outcomes: *ChargeResponse with StatusDeclined or a non-retryable *Error.
	RetryableFailureFraction float64

	// DeclinedFraction is the probability [0,1] that a non-retryable charge failure is
	// returned as *ChargeResponse with StatusDeclined (vs a non-retryable *Error such as
	// insufficient_funds). Only used when the failure is classified as non-retryable.
	DeclinedFraction float64
}

// DefaultStripeMockConfig returns a config with no latency and no failures.
func DefaultStripeMockConfig() StripeMockConfig {
	return StripeMockConfig{
		RetryableFailureFraction: 0.4,
		DeclinedFraction:         0.7,
	}
}

// StripeMock is a Stripe-like PSP mock with configurable latency and failure rates.
type StripeMock struct {
	cfg StripeMockConfig
}

// NewStripeMock builds a StripeMock. Invalid rates are clamped to [0,1].
func NewStripeMock(cfg StripeMockConfig) *StripeMock {
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
	return &StripeMock{cfg: cfg}
}

func (m *StripeMock) Name() string {
	return stripeMockName
}

// Charge simulates a Stripe charge with optional latency and random failures.
func (m *StripeMock) Charge(ctx context.Context, req ChargeRequest) (*ChargeResponse, error) {
	if err := sleepStripeMock(ctx, m.cfg.MinLatency, m.cfg.MaxLatency); err != nil {
		return nil, err
	}
	cur := strings.ToUpper(strings.TrimSpace(req.Currency))
	if _, ok := stripeSupportedCurrencies[cur]; !ok {
		return nil, NewError(CodeInvalidRequest, fmt.Sprintf("unsupported currency for stripe mock: %q", req.Currency), false)
	}

	if rand.Float64() < m.cfg.ChargeFailureRate {
		return m.simulateChargeFailure()
	}

	refID := newStripeChargeID()
	raw, _ := json.Marshal(map[string]any{
		"object":   "charge",
		"id":       refID,
		"amount":   req.Amount,
		"currency": strings.ToLower(cur),
		"status":   "succeeded",
		"captured": true,
	})
	return &ChargeResponse{
		PSPReferenceID: refID,
		Status:         ChargeStatusCaptured,
		RawResponse:    raw,
	}, nil
}

func (m *StripeMock) simulateChargeFailure() (*ChargeResponse, error) {
	if rand.Float64() < m.cfg.RetryableFailureFraction {
		code := []string{CodeNetworkError, CodeRateLimited, CodeServiceUnavailable}[rand.IntN(3)]
		return nil, NewError(code, "simulated stripe transient failure", true)
	}
	if rand.Float64() < m.cfg.DeclinedFraction {
		raw, _ := json.Marshal(map[string]any{
			"object":              "charge",
			"status":              "failed",
			"failure_code":        CodeCardDeclined,
			"failure_message":     "Your card was declined.",
			"outcome_network_status": "declined_by_network",
		})
		return &ChargeResponse{
			Status:      ChargeStatusDeclined,
			RawResponse: raw,
		}, nil
	}
	return nil, NewError(CodeInsufficientFunds, "simulated insufficient funds", false)
}

// Refund simulates a Stripe refund with optional latency and random failures.
func (m *StripeMock) Refund(ctx context.Context, req RefundRequest) (*RefundResponse, error) {
	if err := sleepStripeMock(ctx, m.cfg.MinLatency, m.cfg.MaxLatency); err != nil {
		return nil, err
	}
	cur := strings.ToUpper(strings.TrimSpace(req.Currency))
	if _, ok := stripeSupportedCurrencies[cur]; !ok {
		return nil, NewError(CodeInvalidRequest, fmt.Sprintf("unsupported currency for stripe mock: %q", req.Currency), false)
	}

	if rand.Float64() < m.cfg.RefundFailureRate {
		if rand.Float64() < 0.5 {
			return nil, NewError(CodeNetworkError, "simulated stripe refund transient failure", true)
		}
		return nil, NewError(CodeInvalidRequest, "simulated stripe refund rejected", false)
	}

	refundID := newStripeRefundID()
	raw, _ := json.Marshal(map[string]any{
		"object":   "refund",
		"id":       refundID,
		"amount":   req.Amount,
		"currency": strings.ToLower(cur),
		"status":   "succeeded",
		"charge":   req.PSPReferenceID,
	})
	return &RefundResponse{
		PSPRefundID: refundID,
		Status:      RefundStatusSucceeded,
		RawResponse: raw,
	}, nil
}

func sleepStripeMock(ctx context.Context, minD, maxD time.Duration) error {
	d := stripeMockLatency(minD, maxD)
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func stripeMockLatency(minD, maxD time.Duration) time.Duration {
	if minD == 0 && maxD == 0 {
		return 0
	}
	if maxD == 0 {
		maxD = minD
	}
	if minD == maxD {
		return minD
	}
	delta := maxD - minD
	ns := rand.Int64N(int64(delta))
	return minD + time.Duration(ns)
}

func clamp01(x float64) float64 {
	if math.IsNaN(x) {
		return 0
	}
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}

func newStripeChargeID() string {
	return "ch_" + randomStripeSuffix(24)
}

func newStripeRefundID() string {
	return "re_" + randomStripeSuffix(24)
}

func randomStripeSuffix(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[rand.IntN(len(alphabet))]
	}
	return string(b)
}
