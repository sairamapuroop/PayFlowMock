package domain

import (
	"time"

	"github.com/google/uuid"
)

// OutboxEvent is a durable webhook delivery record (transactional outbox).
type OutboxEvent struct {
	ID           uuid.UUID
	PaymentID    uuid.UUID
	MerchantID   uuid.UUID
	EventType    string
	WebhookURL   string
	Payload      []byte
	Status       string
	LastError    string
	AttemptCount int
	NextRetryAt  time.Time
	DeliveredAt  time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

const (
	OutboxStatusPending    = "PENDING"
	OutboxStatusProcessing = "PROCESSING"
	OutboxStatusDelivered  = "DELIVERED"
	OutboxStatusDead       = "DEAD"
)

const (
	EventPaymentSuccess  = "payment.success"
	EventPaymentFailed   = "payment.failed"
	EventPaymentRefunded = "payment.refunded"
)
