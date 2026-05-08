package retry

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sairamapuroop/PayFlowMock/internal/psp"
)

// netTimeoutErr implements net.Error for classification tests (no real I/O).
type netTimeoutErr struct{}

func (netTimeoutErr) Error() string   { return "mock net timeout" }
func (netTimeoutErr) Timeout() bool   { return true }
func (netTimeoutErr) Temporary() bool { return true }

func TestIsRetryable(t *testing.T) {
	retryablePSP := psp.NewError(psp.CodeServiceUnavailable, "upstream", true)
	nonRetryablePSP := psp.NewError(psp.CodeCardDeclined, "declined", false)
	wrappedRetryable := fmt.Errorf("wrap: %w", retryablePSP)

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "plain_error", err: errors.New("oops"), want: false},
		{name: "psp_retryable", err: retryablePSP, want: true},
		{name: "psp_non_retryable", err: nonRetryablePSP, want: false},
		{name: "wrapped_psp_retryable", err: wrappedRetryable, want: true},
		{name: "deadline_exceeded", err: context.DeadlineExceeded, want: true},
		{name: "wrapped_deadline_exceeded", err: fmt.Errorf("outer: %w", context.DeadlineExceeded), want: true},
		{name: "net_error", err: netTimeoutErr{}, want: true},
		{name: "op_error", err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("refused")}, want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRetryable(tc.err); got != tc.want {
				t.Fatalf("IsRetryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestDo_MaxAttempts_AllRetryable(t *testing.T) {
	cfg := Config{
		MaxAttempts: 4,
		BaseDelay:   time.Nanosecond,
		MaxDelay:    time.Nanosecond,
		Multiplier:  2,
	}
	root := psp.NewError(psp.CodeRateLimited, "slow", true)
	var calls atomic.Int32

	_, err := Do(context.Background(), cfg, func() (int, error) {
		calls.Add(1)
		return 0, root
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "max retries exceeded") {
		t.Fatalf("error should mention max retries, got %v", err)
	}
	if !errors.Is(err, root) {
		t.Fatalf("expected wrapped root via errors.Is: %v", err)
	}
	if got := int(calls.Load()); got != cfg.MaxAttempts {
		t.Fatalf("op calls: got %d, want %d", got, cfg.MaxAttempts)
	}
}

func TestDo_SuccessAfterRetry(t *testing.T) {
	cfg := Config{
		MaxAttempts: 5,
		BaseDelay:   time.Nanosecond,
		MaxDelay:    time.Nanosecond,
		Multiplier:  2,
	}
	transient := psp.NewError(psp.CodeProcessingError, "try again", true)
	var calls atomic.Int32

	v, err := Do(context.Background(), cfg, func() (string, error) {
		n := calls.Add(1)
		if n < 3 {
			return "", transient
		}
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != "ok" {
		t.Fatalf("result: got %q, want ok", v)
	}
	if got := int(calls.Load()); got != 3 {
		t.Fatalf("op calls: got %d, want 3", got)
	}
}

func TestDo_NonRetryable_NoExtraAttempts(t *testing.T) {
	cfg := Config{
		MaxAttempts: 5,
		BaseDelay:   time.Nanosecond,
		MaxDelay:    time.Nanosecond,
		Multiplier:  2,
	}
	permanent := psp.NewError(psp.CodeInvalidRequest, "bad", false)
	var calls atomic.Int32

	_, err := Do(context.Background(), cfg, func() (struct{}, error) {
		calls.Add(1)
		return struct{}{}, permanent
	})
	if !errors.Is(err, permanent) {
		t.Fatalf("expected same error, got %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("op calls: got %d, want 1", calls.Load())
	}
}

func TestDo_ContextAlreadyCancelled(t *testing.T) {
	cfg := Config{
		MaxAttempts: 5,
		BaseDelay:   time.Nanosecond,
		MaxDelay:    time.Nanosecond,
		Multiplier:  2,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var calls atomic.Int32
	_, err := Do(ctx, cfg, func() (struct{}, error) {
		calls.Add(1)
		return struct{}{}, psp.NewError(psp.CodeServiceUnavailable, "x", true)
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("op calls: got %d, want 0", calls.Load())
	}
}

func TestDo_ContextCancelledDuringBackoff(t *testing.T) {
	cfg := Config{
		MaxAttempts: 5,
		BaseDelay:   time.Hour, // block until ctx is cancelled
		MaxDelay:    time.Hour,
		Multiplier:  2,
	}
	ctx, cancel := context.WithCancel(context.Background())
	retryErr := psp.NewError(psp.CodeRateLimited, "retry", true)

	firstFailed := make(chan struct{})
	var calls atomic.Int32

	errCh := make(chan error, 1)
	go func() {
		_, err := Do(ctx, cfg, func() (struct{}, error) {
			if calls.Add(1) == 1 {
				close(firstFailed)
				return struct{}{}, retryErr
			}
			t.Error("op should not run a second time")
			return struct{}{}, nil
		})
		errCh <- err
	}()

	<-firstFailed
	cancel()

	err := <-errCh
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled after cancel during backoff, got %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("op calls: got %d, want 1", calls.Load())
	}
}
