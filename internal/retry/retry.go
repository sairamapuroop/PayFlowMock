package retry

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"net"
	"time"

	"github.com/sairamapuroop/PayFlowMock/internal/psp"
)

// Config controls retry attempts and exponential backoff with full jitter between tries.
type Config struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	Multiplier  float64
}

// DefaultConfig returns the recommended defaults (3 attempts, 100ms base, 10s cap, 2x growth).
func DefaultConfig() Config {
	return Config{
		MaxAttempts: 3,
		BaseDelay:   100 * time.Millisecond,
		MaxDelay:    10 * time.Second,
		Multiplier:  2.0,
	}
}

func (c Config) withDefaults() Config {
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 3
	}
	if c.BaseDelay <= 0 {
		c.BaseDelay = 100 * time.Millisecond
	}
	if c.MaxDelay <= 0 {
		c.MaxDelay = 10 * time.Second
	}
	if c.Multiplier <= 0 {
		c.Multiplier = 2.0
	}
	return c
}

// Do runs op until it succeeds, ctx is cancelled, or non-retryable error / max attempts.
// Between failures it waits using exponential backoff with full jitter (uniform in [0, min(exp, MaxDelay)]).
func Do[T any](ctx context.Context, cfg Config, op func() (T, error)) (T, error) {
	cfg = cfg.withDefaults()
	var zero T
	var lastErr error

	for attempt := 0; attempt < cfg.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return zero, err
		}

		result, err := op()
		if err == nil {
			return result, nil
		}
		lastErr = err
		if !IsRetryable(err) {
			return zero, err
		}
		if attempt == cfg.MaxAttempts-1 {
			break
		}

		delay := calculateDelayWithJitter(cfg, attempt)
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-time.After(delay):
		}
	}

	return zero, fmt.Errorf("max retries exceeded: %w", lastErr)
}

func calculateDelayWithJitter(cfg Config, attempt int) time.Duration {
	exp := float64(cfg.BaseDelay) * math.Pow(cfg.Multiplier, float64(attempt))
	capped := math.Min(exp, float64(cfg.MaxDelay))
	if capped <= 0 {
		return 0
	}
	// Full jitter: uniform in [0, capped]
	return time.Duration(rand.Float64() * capped)
}

// IsRetryable classifies errors for retry: *psp.Error uses Retryable; otherwise deadline and network errors.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	var pe *psp.Error
	if errors.As(err, &pe) {
		return pe.Retryable
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return isNetworkError(err)
}

func isNetworkError(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	var op *net.OpError
	return errors.As(err, &op)
}
