package psp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// PSPAdapter is the uniform interface for payment service providers (charge and refund).
type PSPAdapter interface {
	Name() string
	Charge(ctx context.Context, req ChargeRequest) (*ChargeResponse, error)
	Refund(ctx context.Context, req RefundRequest) (*RefundResponse, error)
}

// ChargeRequest is a PSP charge in the smallest currency unit (e.g. cents, paise).
type ChargeRequest struct {
	PaymentID      uuid.UUID
	Amount         int64
	Currency       string
	IdempotencyKey string
}

// ChargeStatus is the PSP-reported outcome of a charge attempt.
type ChargeStatus string

const (
	ChargeStatusAuthorized ChargeStatus = "authorized"
	ChargeStatusCaptured   ChargeStatus = "captured"
	ChargeStatusDeclined   ChargeStatus = "declined"
	ChargeStatusError      ChargeStatus = "error"
)

// ChargeResponse is the normalized result of a successful Charge call (HTTP-level success).
// A declined card still returns *ChargeResponse with Status Declined and may omit PSPReferenceID.
type ChargeResponse struct {
	PSPReferenceID string
	Status         ChargeStatus
	RawResponse    json.RawMessage
}

// RefundRequest asks the PSP to refund a prior capture.
type RefundRequest struct {
	PaymentID      uuid.UUID
	PSPReferenceID string // charge/capture id at the PSP
	Amount         int64  // smallest currency unit
	Currency       string
	IdempotencyKey string
}

// RefundStatus is the PSP-reported refund outcome.
type RefundStatus string

const (
	RefundStatusPending   RefundStatus = "pending"
	RefundStatusSucceeded RefundStatus = "succeeded"
	RefundStatusFailed    RefundStatus = "failed"
)

// RefundResponse is the normalized result of a Refund call.
type RefundResponse struct {
	PSPRefundID string
	Status      RefundStatus
	RawResponse json.RawMessage
}

// Error is a PSP-level failure with a stable code and retry hint.
// Use errors.As with *Error to classify errors in retry and circuit breaker logic.
type Error struct {
	Code      string
	Message   string
	Retryable bool
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Message != "" {
		return fmt.Sprintf("psp: %s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("psp: %s", e.Code)
}

// Well-known error codes (taxonomy for retry vs non-retryable behavior).
const (
	CodeCardDeclined        = "card_declined"
	CodeInsufficientFunds   = "insufficient_funds"
	CodeInvalidRequest      = "invalid_request"
	CodeAuthenticationError = "authentication_error"
	CodeNetworkError        = "network_error"
	CodeRateLimited         = "rate_limited"
	CodeServiceUnavailable  = "service_unavailable"
	CodeProcessingError     = "processing_error"
	CodeUnknown             = "unknown"
)

// NewError builds a *Error with the given code and retry flag.
func NewError(code, message string, retryable bool) *Error {
	return &Error{Code: code, Message: message, Retryable: retryable}
}

// IsRetryable reports whether err is a *Error marked retryable, or wraps one that is.
// Non-PSP errors return false.
func IsRetryable(err error) bool {
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Retryable
	}
	return false
}
