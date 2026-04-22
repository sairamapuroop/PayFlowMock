package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sairamapuroop/PayFlowMock/internal/domain"
)

var errOutboxEventNotFound = errors.New("outbox event not found")

// OutboxRepo persists transactional outbox rows and supports the webhook worker.
type OutboxRepo struct {
	db *pgxpool.Pool
}

// NewOutboxRepo returns a repository backed by the given pool.
func NewOutboxRepo(db *pgxpool.Pool) *OutboxRepo {
	return &OutboxRepo{db: db}
}

// EnqueueTx inserts an outbox row inside the caller's transaction (same tx as payment updates).
func (r *OutboxRepo) EnqueueTx(ctx context.Context, tx pgx.Tx, e *domain.OutboxEvent) error {
	if e == nil {
		return fmt.Errorf("outbox event is nil")
	}
	if e.ID == uuid.Nil {
		return fmt.Errorf("outbox event id is required")
	}

	now := time.Now().UTC()
	if e.CreatedAt.IsZero() {
		e.CreatedAt = now
	}
	if e.UpdatedAt.IsZero() {
		e.UpdatedAt = now
	}
	if e.NextRetryAt.IsZero() {
		e.NextRetryAt = now
	}
	status := e.Status
	if status == "" {
		status = domain.OutboxStatusPending
	}

	const q = `
INSERT INTO outbox_events (
	id, payment_id, merchant_id, event_type, webhook_url, payload,
	status, attempt_count, next_retry_at, last_error, delivered_at, created_at, updated_at
) VALUES (
	$1, $2, $3, $4, $5, $6,
	$7, $8, $9, $10, $11, $12, $13
)`
	var lastErr any
	if e.LastError != "" {
		lastErr = e.LastError
	}
	var deliveredAt any
	if !e.DeliveredAt.IsZero() {
		deliveredAt = e.DeliveredAt
	}

	_, err := tx.Exec(ctx, q,
		e.ID,
		e.PaymentID,
		e.MerchantID,
		e.EventType,
		e.WebhookURL,
		e.Payload,
		status,
		e.AttemptCount,
		e.NextRetryAt,
		lastErr,
		deliveredAt,
		e.CreatedAt,
		e.UpdatedAt,
	)
	return err
}

// ClaimBatch locks up to limit due PENDING rows, marks them PROCESSING, and returns them.
// Uses FOR UPDATE SKIP LOCKED so concurrent workers do not claim the same rows.
func (r *OutboxRepo) ClaimBatch(ctx context.Context, limit int) ([]*domain.OutboxEvent, error) {
	if limit <= 0 {
		limit = 1
	}

	const q = `
WITH due AS (
	SELECT id FROM outbox_events
	WHERE status = 'PENDING' AND next_retry_at <= NOW()
	ORDER BY next_retry_at
	FOR UPDATE SKIP LOCKED
	LIMIT $1
)
UPDATE outbox_events o
SET status = 'PROCESSING', updated_at = NOW()
FROM due
WHERE o.id = due.id
RETURNING
	o.id, o.payment_id, o.merchant_id, o.event_type, o.webhook_url, o.payload,
	o.status, o.attempt_count, o.next_retry_at, o.last_error, o.created_at, o.updated_at, o.delivered_at`

	rows, err := r.db.Query(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*domain.OutboxEvent
	for rows.Next() {
		e, err := scanOutboxEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ReclaimStaleProcessing resets PROCESSING rows whose updated_at is older than staleAfter
// back to PENDING so they can be claimed again after a worker crash or stalled delivery.
func (r *OutboxRepo) ReclaimStaleProcessing(ctx context.Context, staleAfter time.Duration) (int64, error) {
	if staleAfter <= 0 {
		return 0, fmt.Errorf("staleAfter must be positive")
	}
	micros := staleAfter.Microseconds()
	const q = `
UPDATE outbox_events
SET status = 'PENDING', next_retry_at = NOW(), updated_at = NOW()
WHERE status = 'PROCESSING'
  AND updated_at < NOW() - ($1::bigint * INTERVAL '1 microsecond')`

	tag, err := r.db.Exec(ctx, q, micros)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// MarkDelivered marks an event as successfully delivered.
func (r *OutboxRepo) MarkDelivered(ctx context.Context, id uuid.UUID) error {
	const q = `
UPDATE outbox_events
SET status = $2, delivered_at = NOW(), last_error = NULL, updated_at = NOW()
WHERE id = $1 AND status = $3`
	tag, err := r.db.Exec(ctx, q, id, domain.OutboxStatusDelivered, domain.OutboxStatusProcessing)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errOutboxEventNotFound
	}
	return nil
}

// MarkRetry increments attempt_count, sets status back to PENDING, and schedules the next retry.
func (r *OutboxRepo) MarkRetry(ctx context.Context, id uuid.UUID, nextAt time.Time, lastErr string) error {
	const q = `
UPDATE outbox_events
SET status = $2,
    attempt_count = attempt_count + 1,
    next_retry_at = $3,
    last_error = $4,
    updated_at = NOW()
WHERE id = $1 AND status = $5`
	tag, err := r.db.Exec(ctx, q,
		id,
		domain.OutboxStatusPending,
		nextAt,
		lastErr,
		domain.OutboxStatusProcessing,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errOutboxEventNotFound
	}
	return nil
}

// MarkDead marks an event as permanently failed.
func (r *OutboxRepo) MarkDead(ctx context.Context, id uuid.UUID, lastErr string) error {
	const q = `
UPDATE outbox_events
SET status = $2, last_error = $3, updated_at = NOW()
WHERE id = $1 AND status = $4`
	tag, err := r.db.Exec(ctx, q, id, domain.OutboxStatusDead, lastErr, domain.OutboxStatusProcessing)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errOutboxEventNotFound
	}
	return nil
}

func scanOutboxEvent(row pgx.Row) (*domain.OutboxEvent, error) {
	var (
		e           domain.OutboxEvent
		lastErr     sql.NullString
		deliveredAt sql.NullTime
	)
	err := row.Scan(
		&e.ID,
		&e.PaymentID,
		&e.MerchantID,
		&e.EventType,
		&e.WebhookURL,
		&e.Payload,
		&e.Status,
		&e.AttemptCount,
		&e.NextRetryAt,
		&lastErr,
		&e.CreatedAt,
		&e.UpdatedAt,
		&deliveredAt,
	)
	if err != nil {
		return nil, err
	}
	if lastErr.Valid {
		e.LastError = lastErr.String
	}
	if deliveredAt.Valid {
		e.DeliveredAt = deliveredAt.Time
	}
	return &e, nil
}
