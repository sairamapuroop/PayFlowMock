package psp

import (
	"context"
	"errors"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/sairamapuroop/PayFlowMock/pkg/metrics"
	"github.com/sony/gobreaker"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
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
	pspName := adapter.Name()
	settings := gobreaker.Settings{
		Name:        pspName,
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
			metrics.C().PSPCircuitState.WithLabelValues(name).Set(circuitStateValue(to))
		},
	}
	metrics.C().PSPCircuitState.WithLabelValues(pspName).Set(circuitStateValue(gobreaker.StateClosed))
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
	start := time.Now()
	tracer := otel.Tracer("payflow/psp")
	ctx, span := tracer.Start(ctx, "psp.charge")
	span.SetAttributes(
		attribute.String("psp.name", c.adapter.Name()),
		attribute.String("payment.id", req.PaymentID.String()),
		attribute.Int64("amount", req.Amount),
		attribute.String("currency", req.Currency),
	)
	defer span.End()

	result, err := c.breaker.Execute(func() (interface{}, error) {
		return c.adapter.Charge(ctx, req)
	})
	outcome := classifyOutcome(err)
	metrics.C().PSPAttemptsTotal.WithLabelValues(c.adapter.Name(), "charge", outcome).Inc()
	metrics.C().PaymentLatencySeconds.WithLabelValues(c.adapter.Name()).Observe(time.Since(start).Seconds())
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.SetAttributes(attribute.String("payflow.outcome", outcome))
		return nil, err
	}
	span.SetAttributes(attribute.String("payflow.outcome", "success"))
	return result.(*ChargeResponse), nil
}

// Refund runs the refund through the circuit breaker.
func (c *CircuitBreakerAdapter) Refund(ctx context.Context, req RefundRequest) (*RefundResponse, error) {
	start := time.Now()
	tracer := otel.Tracer("payflow/psp")
	ctx, span := tracer.Start(ctx, "psp.refund")
	span.SetAttributes(
		attribute.String("psp.name", c.adapter.Name()),
		attribute.String("payment.id", req.PaymentID.String()),
		attribute.Int64("amount", req.Amount),
		attribute.String("currency", req.Currency),
	)
	defer span.End()

	result, err := c.breaker.Execute(func() (interface{}, error) {
		return c.adapter.Refund(ctx, req)
	})
	outcome := classifyOutcome(err)
	metrics.C().PSPAttemptsTotal.WithLabelValues(c.adapter.Name(), "refund", outcome).Inc()
	metrics.C().PaymentLatencySeconds.WithLabelValues(c.adapter.Name()).Observe(time.Since(start).Seconds())
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		span.SetAttributes(attribute.String("payflow.outcome", outcome))
		return nil, err
	}
	span.SetAttributes(attribute.String("payflow.outcome", "success"))
	return result.(*RefundResponse), nil
}

func classifyOutcome(err error) string {
	if err == nil {
		return "success"
	}
	if IsRetryable(err) || errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests) {
		return "retryable_error"
	}
	return "nonretryable_error"
}

func circuitStateValue(state gobreaker.State) float64 {
	switch state {
	case gobreaker.StateClosed:
		return 0
	case gobreaker.StateHalfOpen:
		return 1
	case gobreaker.StateOpen:
		return 2
	default:
		return -1
	}
}
