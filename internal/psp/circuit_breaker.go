package psp

import (
	"context"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/sony/gobreaker"
)

// CircuitBreakerAdapter wraps a PSPAdapter with a per-PSP circuit breaker so a
// degraded provider fails fast instead of being hammered on every request.
type CircuitBreakerAdapter struct {
	adapter PSPAdapter
	breaker *gobreaker.CircuitBreaker
}

// NewCircuitBreakerAdapter returns a PSPAdapter that trips after repeated
// failures and blocks calls while open, then probes in half-open state.
func NewCircuitBreakerAdapter(adapter PSPAdapter) *CircuitBreakerAdapter {
	settings := gobreaker.Settings{
		Name:        adapter.Name(),
		MaxRequests: 3,
		Interval:    60 * time.Second,
		Timeout:     30 * time.Second,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			return counts.ConsecutiveFailures >= 5
		},
		OnStateChange: func(name string, from, to gobreaker.State) {
			log.Info().
				Str("psp", name).
				Str("from", from.String()).
				Str("to", to.String()).
				Msg("circuit breaker state change")
		},
	}
	return &CircuitBreakerAdapter{
		adapter: adapter,
		breaker: gobreaker.NewCircuitBreaker(settings),
	}
}

// Name delegates to the wrapped adapter.
func (c *CircuitBreakerAdapter) Name() string {
	return c.adapter.Name()
}

// Charge runs the charge through the circuit breaker.
func (c *CircuitBreakerAdapter) Charge(ctx context.Context, req ChargeRequest) (*ChargeResponse, error) {
	result, err := c.breaker.Execute(func() (interface{}, error) {
		return c.adapter.Charge(ctx, req)
	})
	if err != nil {
		return nil, err
	}
	return result.(*ChargeResponse), nil
}

// Refund runs the refund through the circuit breaker.
func (c *CircuitBreakerAdapter) Refund(ctx context.Context, req RefundRequest) (*RefundResponse, error) {
	result, err := c.breaker.Execute(func() (interface{}, error) {
		return c.adapter.Refund(ctx, req)
	})
	if err != nil {
		return nil, err
	}
	return result.(*RefundResponse), nil
}
