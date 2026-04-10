package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/sairamapuroop/PayFlowMock/internal/domain"
	"github.com/sairamapuroop/PayFlowMock/internal/repository"
)

const stubPSPName = "stub"

// PaymentRepository is the persistence contract for payment flows (implemented by *repository.PaymentRepo).
type PaymentRepository interface {
	CreatePayment(ctx context.Context, payment *domain.Payment) error
	GetPaymentByID(ctx context.Context, id uuid.UUID) (*domain.Payment, error)
	UpdatePaymentStatus(ctx context.Context, id uuid.UUID, fromStatus, toStatus domain.Status) error
	UpdatePaymentStatusWithPSP(ctx context.Context, id uuid.UUID, fromStatus, toStatus domain.Status, pspName, pspReferenceID string) error
	RefundPaymentAtomic(ctx context.Context, paymentID uuid.UUID, refundAmount int64, idempotencyKey string) (refundID uuid.UUID, err error)
}

// PaymentServiceAPI is the surface exposed to HTTP handlers.
type PaymentServiceAPI interface {
	CreatePayment(ctx context.Context, req domain.CreatePaymentRequest) (*domain.CreatePaymentResponse, error)
	GetPayment(ctx context.Context, id uuid.UUID) (*domain.Payment, error)
	RefundPayment(ctx context.Context, paymentID uuid.UUID, req domain.RefundRequest) (*domain.RefundResponse, error)
}

// PaymentService handles payment use cases.
type PaymentService struct {
	repo PaymentRepository
}

// NewPaymentService returns a service backed by repo.
func NewPaymentService(repo PaymentRepository) *PaymentService {
	if repo == nil {
		return nil
	}
	return &PaymentService{repo: repo}
}

var (
	// ErrValidation indicates the client sent invalid input.
	ErrValidation = errors.New("validation error")
)

// CreatePayment validates input, persists a new payment, and runs the Week-1 stub PSP flow to SUCCESS.
func (s *PaymentService) CreatePayment(ctx context.Context, req domain.CreatePaymentRequest) (*domain.CreatePaymentResponse, error) {
	if s == nil || s.repo == nil {
		return nil, errors.New("payment service is not configured")
	}
	if err := validateCreatePaymentRequest(&req); err != nil {
		return nil, err
	}

	id, err := domain.NewID()
	if err != nil {
		return nil, fmt.Errorf("generate payment id: %w", err)
	}

	payment := &domain.Payment{
		ID:             id,
		MerchantID:     req.MerchantID,
		Amount:         req.Amount,
		Currency:       strings.ToUpper(strings.TrimSpace(req.Currency)),
		Status:         domain.StatusInitiated,
		IdempotencyKey: strings.TrimSpace(req.IdempotencyKey),
	}

	if err := s.repo.CreatePayment(ctx, payment); err != nil {
		return nil, err
	}

	if err := s.repo.UpdatePaymentStatus(ctx, id, domain.StatusInitiated, domain.StatusProcessing); err != nil {
		return nil, err
	}

	stubRef := fmt.Sprintf("%s-%s", stubPSPName, id.String())
	if err := s.repo.UpdatePaymentStatusWithPSP(ctx, id, domain.StatusProcessing, domain.StatusSuccess, stubPSPName, stubRef); err != nil {
		return nil, err
	}

	return &domain.CreatePaymentResponse{
		PaymentID: id,
		Status:    domain.StatusSuccess,
	}, nil
}

// GetPayment returns the persisted payment or ErrNotFound.
func (s *PaymentService) GetPayment(ctx context.Context, id uuid.UUID) (*domain.Payment, error) {
	if s == nil || s.repo == nil {
		return nil, errors.New("payment service is not configured")
	}
	if id == uuid.Nil {
		return nil, fmt.Errorf("%w: payment id is required", ErrValidation)
	}
	return s.repo.GetPaymentByID(ctx, id)
}

// RefundPayment refunds a successful payment up to its original amount inside one DB transaction.
func (s *PaymentService) RefundPayment(ctx context.Context, paymentID uuid.UUID, req domain.RefundRequest) (*domain.RefundResponse, error) {
	if s == nil || s.repo == nil {
		return nil, errors.New("payment service is not configured")
	}
	if paymentID == uuid.Nil {
		return nil, fmt.Errorf("%w: payment id is required", ErrValidation)
	}
	if req.PaymentID != uuid.Nil && req.PaymentID != paymentID {
		return nil, fmt.Errorf("%w: payment_id does not match path", ErrValidation)
	}
	if err := validateRefundRequest(&req); err != nil {
		return nil, err
	}

	p, err := s.repo.GetPaymentByID(ctx, paymentID)
	if err != nil {
		return nil, err
	}
	if p.Status != domain.StatusSuccess {
		return nil, fmt.Errorf("%w: only payments in %q can be refunded (current %q)", ErrValidation, domain.StatusSuccess, p.Status)
	}

	currency := strings.ToUpper(strings.TrimSpace(req.Currency))
	if p.Currency != currency {
		return nil, fmt.Errorf("%w: currency %q does not match payment currency %q", ErrValidation, currency, p.Currency)
	}

	if req.Amount.Cmp(&p.Amount) > 0 {
		return nil, fmt.Errorf("%w: refund amount exceeds payment amount", ErrValidation)
	}
	if !req.Amount.IsInt64() {
		return nil, fmt.Errorf("%w: refund amount must fit in int64", ErrValidation)
	}

	refundID, err := s.repo.RefundPaymentAtomic(ctx, paymentID, req.Amount.Int64(), strings.TrimSpace(req.IdempotencyKey))
	if err != nil {
		if errors.Is(err, repository.ErrStatusMismatch) {
			return nil, fmt.Errorf("payment is not in %q (concurrent update?): %w", domain.StatusSuccess, err)
		}
		return nil, err
	}

	return &domain.RefundResponse{
		RefundID: refundID,
		Status:   domain.StatusSuccess,
	}, nil
}

func validateCreatePaymentRequest(req *domain.CreatePaymentRequest) error {
	if req == nil {
		return fmt.Errorf("%w: request is nil", ErrValidation)
	}
	if req.MerchantID == uuid.Nil {
		return fmt.Errorf("%w: merchant_id is required", ErrValidation)
	}
	if req.Amount.Sign() <= 0 {
		return fmt.Errorf("%w: amount must be positive", ErrValidation)
	}
	if !req.Amount.IsInt64() {
		return fmt.Errorf("%w: amount must fit in int64", ErrValidation)
	}

	curr := strings.TrimSpace(strings.ToUpper(req.Currency))
	if len(curr) != 3 || !domain.ValidCurrency(curr) {
		return fmt.Errorf("%w: invalid or unsupported currency", ErrValidation)
	}

	key := strings.TrimSpace(req.IdempotencyKey)
	if key == "" {
		return fmt.Errorf("%w: idempotency_key is required", ErrValidation)
	}
	if len(key) > 255 {
		return fmt.Errorf("%w: idempotency_key too long", ErrValidation)
	}
	return nil
}

func validateRefundRequest(req *domain.RefundRequest) error {
	if req == nil {
		return fmt.Errorf("%w: request is nil", ErrValidation)
	}
	if req.Amount.Sign() <= 0 {
		return fmt.Errorf("%w: amount must be positive", ErrValidation)
	}

	curr := strings.TrimSpace(strings.ToUpper(req.Currency))
	if len(curr) != 3 || !domain.ValidCurrency(curr) {
		return fmt.Errorf("%w: invalid or unsupported currency", ErrValidation)
	}

	key := strings.TrimSpace(req.IdempotencyKey)
	if key == "" {
		return fmt.Errorf("%w: idempotency_key is required", ErrValidation)
	}
	if len(key) > 255 {
		return fmt.Errorf("%w: idempotency_key too long", ErrValidation)
	}
	return nil
}

// Compile-time checks.
var (
	_ PaymentRepository  = (*repository.PaymentRepo)(nil)
	_ PaymentServiceAPI  = (*PaymentService)(nil)
)
