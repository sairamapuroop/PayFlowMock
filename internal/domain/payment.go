package domain

import (
	"math/big"
	"time"

	"github.com/google/uuid"
)

type Payment struct {
	ID               uuid.UUID `json:"id"`
	MerchantID       uuid.UUID `json:"merchant_id"`
	Amount           big.Int   `json:"amount"`
	Currency         string    `json:"currency"`
	Status           Status    `json:"status"`
	IdempotencyKey   string    `json:"idempotency_key"`
	PSP              string    `json:"psp,omitempty"`
	PSPReferenceID   string    `json:"psp_reference_id,omitempty"`
	FailureReason    string    `json:"failure_reason,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type Refund struct {
	ID        uuid.UUID `json:"id"`
	Amount    big.Int   `json:"amount"`
	Currency  string    `json:"currency"`
	Status    Status    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CreatePaymentRequest is the request body for POST /v1/payments.
type CreatePaymentRequest struct {
	MerchantID       uuid.UUID `json:"merchant_id" validate:"required"`
	Amount           big.Int   `json:"amount" validate:"required,gt=0"`
	Currency         string    `json:"currency" validate:"required,oneof=USD EUR GBP INR"`
	IdempotencyKey   string    `json:"idempotency_key" validate:"required"`
}

// CreatePaymentResponse is the response body for POST /v1/payments.
type CreatePaymentResponse struct {
	PaymentID uuid.UUID `json:"payment_id"`
	Status    Status    `json:"status"`
}

// RefundRequest is the request body for POST /v1/payments/{id}/refund.
type RefundRequest struct {
	PaymentID      uuid.UUID `json:"payment_id"`
	Amount         big.Int   `json:"amount" validate:"required,gt=0"`
	Currency       string    `json:"currency" validate:"required,oneof=USD EUR GBP INR"`
	IdempotencyKey string    `json:"idempotency_key" validate:"required"`
}

type RefundResponse struct {
	RefundID uuid.UUID `json:"refund_id"`
	Status    Status    `json:"status"`
}


type Status string


const (
	StatusInitiated  Status = "initiated"
	StatusProcessing Status = "processing"
	StatusSuccess    Status = "success"
	StatusFailed     Status = "failed"
	StatusRefunded   Status = "refunded"
)

var allowedCurrencies = map[string]struct{}{
	"USD": {},
	"EUR": {},
	"GBP": {},
	"INR": {},
}

// ValidCurrency returns true if c is a supported ISO currency code for payments.
func ValidCurrency(c string) bool {
	_, ok := allowedCurrencies[c]
	return ok
}

// IsKnownStatus reports whether s is one of the defined payment statuses.
func IsKnownStatus(s Status) bool {
	switch s {
	case StatusInitiated, StatusProcessing, StatusSuccess, StatusFailed, StatusRefunded:
		return true
	default:
		return false
	}
}

func ValidTransition(from, to Status) bool {
	switch from {
	case StatusInitiated:
		return to == StatusProcessing
	case StatusProcessing:
		return to == StatusSuccess || to == StatusFailed
	case StatusSuccess:
		return to == StatusRefunded
	case StatusFailed:
		return false
	case StatusRefunded:
		return false
	}
	return false
}

// NewID returns a time-ordered UUID v7 (RFC 9562) for primary keys and indexing.
func NewID() (uuid.UUID, error) {
	return uuid.NewV7()
}