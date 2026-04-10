package repository

import "errors"

var (
	ErrRepositoryNotConfigured = errors.New("payment repository is not configured")
	ErrInvalidPayment          = errors.New("invalid payment")
	ErrInvalidAmount           = errors.New("amount must be positive and fit in int64")
	ErrInvalidIdempotencyKey   = errors.New("idempotency key is required")
	ErrNotFound                = errors.New("payment not found")
	ErrConflict                = errors.New("payment with this idempotency key already exists")
	ErrStatusMismatch          = errors.New("payment status does not match the expected value")
	ErrInvalidStatusTransition = errors.New("invalid status transition")
)
