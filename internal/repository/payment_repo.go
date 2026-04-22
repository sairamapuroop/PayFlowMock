package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sairamapuroop/PayFlowMock/internal/domain"
	"github.com/sairamapuroop/PayFlowMock/internal/merchant"
)

// NewPaymentRepo returns a repository backed by the given pool.
// outbox and registry are used to enqueue webhook rows in the same transaction as payment updates;
// outbox must be non-nil for status/refund mutators that emit events.
func NewPaymentRepo(db *pgxpool.Pool, outbox *OutboxRepo, registry merchant.Registry) *PaymentRepo {
	return &PaymentRepo{db: db, outbox: outbox, registry: registry}
}

type PaymentRepo struct {
	db       *pgxpool.Pool
	outbox   *OutboxRepo
	registry merchant.Registry
}

type outboxWebhookEnvelope struct {
	ID        string            `json:"id"`
	Type      string            `json:"type"`
	CreatedAt string            `json:"created_at"`
	Data      outboxWebhookData `json:"data"`
}

type outboxWebhookData struct {
	PaymentID      string `json:"payment_id"`
	MerchantID     string `json:"merchant_id"`
	Amount         int64  `json:"amount"`
	Currency       string `json:"currency"`
	Status         string `json:"status"`
	PSP            string `json:"psp,omitempty"`
	PSPReferenceID string `json:"psp_reference_id,omitempty"`
	FailureReason  string `json:"failure_reason,omitempty"`
	RefundID       string `json:"refund_id,omitempty"`
	RefundAmount   int64  `json:"refund_amount,omitempty"`
}

func (r *PaymentRepo) webhookURLForMerchant(merchantID uuid.UUID) string {
	if r == nil || r.registry == nil {
		return ""
	}
	u, ok := r.registry.WebhookURL(merchantID)
	if !ok {
		return ""
	}
	return u
}

func marshalOutboxPayload(eventID uuid.UUID, eventType string, createdAt time.Time, data outboxWebhookData) ([]byte, error) {
	env := outboxWebhookEnvelope{
		ID:        eventID.String(),
		Type:      eventType,
		CreatedAt: createdAt.UTC().Format(time.RFC3339),
		Data:      data,
	}
	return json.Marshal(env)
}

func (r *PaymentRepo) enqueueOutboxPaymentEventTx(
	ctx context.Context,
	tx pgx.Tx,
	eventType string,
	paymentID, merchantID uuid.UUID,
	amount int64,
	currency string,
	status domain.Status,
	pspName, pspReferenceID, failureReason string,
	refundID uuid.UUID,
	refundAmount int64,
	withRefund bool,
	createdAt time.Time,
) error {
	if r == nil || r.outbox == nil {
		return ErrRepositoryNotConfigured
	}
	eventID, err := domain.NewID()
	if err != nil {
		return fmt.Errorf("new outbox event id: %w", err)
	}
	data := outboxWebhookData{
		PaymentID:      paymentID.String(),
		MerchantID:     merchantID.String(),
		Amount:         amount,
		Currency:       currency,
		Status:         string(status),
		PSP:            pspName,
		PSPReferenceID: pspReferenceID,
		FailureReason:  failureReason,
	}
	if withRefund {
		data.RefundID = refundID.String()
		data.RefundAmount = refundAmount
	}
	payload, err := marshalOutboxPayload(eventID, eventType, createdAt, data)
	if err != nil {
		return fmt.Errorf("marshal outbox payload: %w", err)
	}
	ev := &domain.OutboxEvent{
		ID:         eventID,
		PaymentID:  paymentID,
		MerchantID: merchantID,
		EventType:  eventType,
		WebhookURL: r.webhookURLForMerchant(merchantID),
		Payload:    payload,
	}
	return r.outbox.EnqueueTx(ctx, tx, ev)
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func strOrNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func validatePaymentForCreate(p *domain.Payment) error {
	if p == nil {
		return fmt.Errorf("%w: payment is nil", ErrInvalidPayment)
	}
	if p.ID == uuid.Nil {
		return fmt.Errorf("%w: id is required", ErrInvalidPayment)
	}
	if p.MerchantID == uuid.Nil {
		return fmt.Errorf("%w: merchant_id is required", ErrInvalidPayment)
	}
	key := strings.TrimSpace(p.IdempotencyKey)
	if key == "" {
		return ErrInvalidIdempotencyKey
	}
	if len(key) > 255 {
		return fmt.Errorf("%w: idempotency key too long", ErrInvalidIdempotencyKey)
	}
	p.IdempotencyKey = key

	if p.Amount.Sign() <= 0 {
		return fmt.Errorf("%w: amount must be positive", ErrInvalidAmount)
	}
	if !p.Amount.IsInt64() {
		return ErrInvalidAmount
	}

	curr := strings.TrimSpace(strings.ToUpper(p.Currency))
	if len(curr) != 3 {
		return fmt.Errorf("%w: currency must be a 3-letter code", ErrInvalidPayment)
	}
	if !domain.ValidCurrency(curr) {
		return fmt.Errorf("%w: unsupported currency %q", ErrInvalidPayment, curr)
	}
	p.Currency = curr

	switch {
	case p.Status == "":
		p.Status = domain.StatusInitiated
	case p.Status == domain.StatusInitiated:
		// ok
	default:
		return fmt.Errorf("%w: new payments must have status %q or empty", ErrInvalidPayment, domain.StatusInitiated)
	}

	if !domain.IsKnownStatus(p.Status) {
		return fmt.Errorf("%w: unknown status %q", ErrInvalidPayment, p.Status)
	}

	return nil
}

func scanPayment(row pgx.Row) (*domain.Payment, error) {
	var (
		p              domain.Payment
		amount         big.Int
		psp            sql.NullString
		pspReferenceID sql.NullString
		failureReason  sql.NullString
	)
	err := row.Scan(
		&p.ID,
		&p.MerchantID,
		&amount,
		&p.Currency,
		&p.Status,
		&p.IdempotencyKey,
		&psp,
		&pspReferenceID,
		&failureReason,
		&p.CreatedAt,
		&p.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	p.Amount.SetInt64(amount.Int64())
	if psp.Valid {
		p.PSP = psp.String
	}
	if pspReferenceID.Valid {
		p.PSPReferenceID = pspReferenceID.String
	}
	if failureReason.Valid {
		p.FailureReason = failureReason.String
	}
	return &p, nil
}

const paymentSelectColumns = `
	id, merchant_id, amount, currency, status, idempotency_key, psp, psp_reference_id, failure_reason, created_at, updated_at
`

// CreatePayment inserts a payment row. Duplicate idempotency keys return ErrConflict.
func (r *PaymentRepo) CreatePayment(ctx context.Context, payment *domain.Payment) error {
	if r == nil || r.db == nil {
		return ErrRepositoryNotConfigured
	}
	if err := validatePaymentForCreate(payment); err != nil {
		return err
	}

	createdAt := payment.CreatedAt
	updatedAt := payment.UpdatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	if updatedAt.IsZero() {
		updatedAt = createdAt
	}

	_, err := r.db.Exec(ctx, `
		INSERT INTO payments (
			id, merchant_id, amount, currency, status,
			idempotency_key, psp, psp_reference_id, failure_reason,
			created_at, updated_at
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, $8, $9,
			$10, $11
		)`,
		payment.ID,
		payment.MerchantID,
		payment.Amount.Int64(),
		payment.Currency,
		payment.Status,
		payment.IdempotencyKey,
		strOrNil(payment.PSP),
		strOrNil(payment.PSPReferenceID),
		strOrNil(payment.FailureReason),
		createdAt,
		updatedAt,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("insert payment: %w", err)
	}

	payment.CreatedAt = createdAt
	payment.UpdatedAt = updatedAt
	return nil
}

// GetPaymentByID loads a payment by primary key.
func (r *PaymentRepo) GetPaymentByID(ctx context.Context, id uuid.UUID) (*domain.Payment, error) {
	if r == nil || r.db == nil {
		return nil, ErrRepositoryNotConfigured
	}
	if id == uuid.Nil {
		return nil, fmt.Errorf("%w: id is required", ErrInvalidPayment)
	}

	row := r.db.QueryRow(ctx, `
		SELECT `+paymentSelectColumns+`
		FROM payments
		WHERE id = $1`, id)

	p, err := scanPayment(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get payment by id: %w", err)
	}
	return p, nil
}

// UpdatePaymentStatus transitions status from fromStatus to toStatus for optimistic concurrency.
func (r *PaymentRepo) UpdatePaymentStatus(ctx context.Context, id uuid.UUID, fromStatus, toStatus domain.Status) error {
	if r == nil || r.db == nil {
		return ErrRepositoryNotConfigured
	}
	if id == uuid.Nil {
		return fmt.Errorf("%w: id is required", ErrInvalidPayment)
	}
	if !domain.IsKnownStatus(fromStatus) || !domain.IsKnownStatus(toStatus) {
		return fmt.Errorf("%w: unknown status", ErrInvalidStatusTransition)
	}
	if fromStatus == toStatus {
		return nil
	}
	if !domain.ValidTransition(fromStatus, toStatus) {
		return fmt.Errorf("%w: cannot go from %q to %q", ErrInvalidStatusTransition, fromStatus, toStatus)
	}

	tag, err := r.db.Exec(ctx, `
		UPDATE payments
		SET status = $1, updated_at = NOW()
		WHERE id = $2 AND status = $3`,
		toStatus, id, fromStatus,
	)
	if err != nil {
		return fmt.Errorf("update payment status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		row := r.db.QueryRow(ctx, `SELECT status FROM payments WHERE id = $1`, id)
		var current domain.Status
		if err := row.Scan(&current); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return fmt.Errorf("update payment status: resolve row: %w", err)
		}
		return fmt.Errorf("%w (current %q, expected %q)", ErrStatusMismatch, current, fromStatus)
	}
	return nil
}

// GetPaymentStatusByIdempotencyKey returns the payment for the given idempotency key.
func (r *PaymentRepo) GetPaymentStatusByIdempotencyKey(ctx context.Context, key string) (*domain.Payment, error) {
	if r == nil || r.db == nil {
		return nil, ErrRepositoryNotConfigured
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, ErrInvalidIdempotencyKey
	}

	row := r.db.QueryRow(ctx, `
		SELECT `+paymentSelectColumns+`
		FROM payments
		WHERE idempotency_key = $1`, key)

	p, err := scanPayment(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get payment by idempotency key: %w", err)
	}
	return p, nil
}

// UpdatePaymentStatusWithPSP transitions status and persists PSP metadata with optimistic locking on fromStatus.
func (r *PaymentRepo) UpdatePaymentStatusWithPSP(ctx context.Context, id uuid.UUID, fromStatus, toStatus domain.Status, pspName, pspReferenceID string) error {
	if r == nil || r.db == nil {
		return ErrRepositoryNotConfigured
	}
	if id == uuid.Nil {
		return fmt.Errorf("%w: id is required", ErrInvalidPayment)
	}
	if !domain.IsKnownStatus(fromStatus) || !domain.IsKnownStatus(toStatus) {
		return fmt.Errorf("%w: unknown status", ErrInvalidStatusTransition)
	}
	if fromStatus == toStatus {
		return nil
	}
	if !domain.ValidTransition(fromStatus, toStatus) {
		return fmt.Errorf("%w: cannot go from %q to %q", ErrInvalidStatusTransition, fromStatus, toStatus)
	}

	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("update payment status with psp: begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()

	row := tx.QueryRow(ctx, `
		UPDATE payments
		SET status = $1, psp = $2, psp_reference_id = $3, updated_at = NOW()
		WHERE id = $4 AND status = $5
		RETURNING merchant_id, amount, currency`,
		toStatus, strOrNil(pspName), strOrNil(pspReferenceID), id, fromStatus,
	)
	var merchantID uuid.UUID
	var amount int64
	var currency string
	scanErr := row.Scan(&merchantID, &amount, &currency)
	if scanErr != nil {
		if errors.Is(scanErr, pgx.ErrNoRows) {
			var current domain.Status
			qErr := tx.QueryRow(ctx, `SELECT status FROM payments WHERE id = $1`, id).Scan(&current)
			if errors.Is(qErr, pgx.ErrNoRows) {
				err = ErrNotFound
				return err
			}
			if qErr != nil {
				err = fmt.Errorf("update payment status with psp: resolve row: %w", qErr)
				return err
			}
			err = fmt.Errorf("%w (current %q, expected %q)", ErrStatusMismatch, current, fromStatus)
			return err
		}
		err = fmt.Errorf("update payment status with psp: %w", scanErr)
		return err
	}

	createdAt := time.Now().UTC()
	if err = r.enqueueOutboxPaymentEventTx(ctx, tx, domain.EventPaymentSuccess, id, merchantID, amount, currency, toStatus, pspName, pspReferenceID, "", uuid.Nil, 0, false, createdAt); err != nil {
		return err
	}

	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("update payment status with psp: commit: %w", err)
	}
	return nil
}

// UpdatePaymentFailed transitions to failed and stores PSP fields (optional) and failure_reason.
func (r *PaymentRepo) UpdatePaymentFailed(ctx context.Context, id uuid.UUID, fromStatus domain.Status, pspName, pspReferenceID, failureReason string) error {
	if r == nil || r.db == nil {
		return ErrRepositoryNotConfigured
	}
	if id == uuid.Nil {
		return fmt.Errorf("%w: id is required", ErrInvalidPayment)
	}
	toStatus := domain.StatusFailed
	if !domain.IsKnownStatus(fromStatus) || !domain.IsKnownStatus(toStatus) {
		return fmt.Errorf("%w: unknown status", ErrInvalidStatusTransition)
	}
	if fromStatus == toStatus {
		return nil
	}
	if !domain.ValidTransition(fromStatus, toStatus) {
		return fmt.Errorf("%w: cannot go from %q to %q", ErrInvalidStatusTransition, fromStatus, toStatus)
	}

	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("update payment failed: begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()

	row := tx.QueryRow(ctx, `
		UPDATE payments
		SET status = $1, psp = $2, psp_reference_id = $3, failure_reason = $4, updated_at = NOW()
		WHERE id = $5 AND status = $6
		RETURNING merchant_id, amount, currency`,
		toStatus, strOrNil(pspName), strOrNil(pspReferenceID), strOrNil(failureReason), id, fromStatus,
	)
	var merchantID uuid.UUID
	var amount int64
	var currency string
	scanErr := row.Scan(&merchantID, &amount, &currency)
	if scanErr != nil {
		if errors.Is(scanErr, pgx.ErrNoRows) {
			var current domain.Status
			qErr := tx.QueryRow(ctx, `SELECT status FROM payments WHERE id = $1`, id).Scan(&current)
			if errors.Is(qErr, pgx.ErrNoRows) {
				err = ErrNotFound
				return err
			}
			if qErr != nil {
				err = fmt.Errorf("update payment failed: resolve row: %w", qErr)
				return err
			}
			err = fmt.Errorf("%w (current %q, expected %q)", ErrStatusMismatch, current, fromStatus)
			return err
		}
		err = fmt.Errorf("update payment failed: %w", scanErr)
		return err
	}

	createdAt := time.Now().UTC()
	if err = r.enqueueOutboxPaymentEventTx(ctx, tx, domain.EventPaymentFailed, id, merchantID, amount, currency, toStatus, pspName, pspReferenceID, failureReason, uuid.Nil, 0, false, createdAt); err != nil {
		return err
	}

	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("update payment failed: commit: %w", err)
	}
	return nil
}

// RefundPaymentAtomic inserts a refund and sets the payment to REFUNDED in a single transaction.
func (r *PaymentRepo) RefundPaymentAtomic(ctx context.Context, paymentID uuid.UUID, refundAmount int64, idempotencyKey string) (refundID uuid.UUID, err error) {
	if r == nil || r.db == nil {
		return uuid.Nil, ErrRepositoryNotConfigured
	}
	if paymentID == uuid.Nil {
		return uuid.Nil, fmt.Errorf("%w: id is required", ErrInvalidPayment)
	}
	key := strings.TrimSpace(idempotencyKey)
	if key == "" {
		return uuid.Nil, ErrInvalidIdempotencyKey
	}
	if len(key) > 255 {
		return uuid.Nil, fmt.Errorf("%w: idempotency key too long", ErrInvalidIdempotencyKey)
	}
	if refundAmount <= 0 {
		return uuid.Nil, ErrInvalidAmount
	}

	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return uuid.Nil, fmt.Errorf("refund tx begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()

	err = tx.QueryRow(ctx, `
		INSERT INTO refunds (payment_id, amount, status, idempotency_key)
		VALUES ($1, $2, $3, $4)
		RETURNING id`,
		paymentID, refundAmount, domain.StatusSuccess, key,
	).Scan(&refundID)
	if err != nil {
		if isUniqueViolation(err) {
			return uuid.Nil, ErrConflict
		}
		return uuid.Nil, fmt.Errorf("insert refund: %w", err)
	}

	row := tx.QueryRow(ctx, `
		UPDATE payments
		SET status = $1, updated_at = NOW()
		WHERE id = $2 AND status = $3
		RETURNING merchant_id, amount, currency, psp, psp_reference_id`,
		domain.StatusRefunded, paymentID, domain.StatusSuccess,
	)
	var merchantID uuid.UUID
	var amount int64
	var currency string
	var pspName, pspReferenceID sql.NullString
	scanErr := row.Scan(&merchantID, &amount, &currency, &pspName, &pspReferenceID)
	if scanErr != nil {
		if errors.Is(scanErr, pgx.ErrNoRows) {
			err = ErrStatusMismatch
			return uuid.Nil, err
		}
		return uuid.Nil, fmt.Errorf("update payment to refunded: %w", scanErr)
	}

	pspN := ""
	pspRef := ""
	if pspName.Valid {
		pspN = pspName.String
	}
	if pspReferenceID.Valid {
		pspRef = pspReferenceID.String
	}

	createdAt := time.Now().UTC()
	if err = r.enqueueOutboxPaymentEventTx(ctx, tx, domain.EventPaymentRefunded, paymentID, merchantID, amount, currency, domain.StatusRefunded, pspN, pspRef, "", refundID, refundAmount, true, createdAt); err != nil {
		return uuid.Nil, err
	}

	if err = tx.Commit(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("refund tx commit: %w", err)
	}
	return refundID, nil
}
