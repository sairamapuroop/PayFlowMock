package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/sairamapuroop/PayFlowMock/internal/domain"
	"github.com/sairamapuroop/PayFlowMock/internal/psp"
	"github.com/sairamapuroop/PayFlowMock/internal/repository"
	"github.com/sairamapuroop/PayFlowMock/internal/retry"
)

// PaymentRepository is the persistence contract for payment flows (implemented by *repository.PaymentRepo).
type PaymentRepository interface {
	CreatePayment(ctx context.Context, payment *domain.Payment) error
	GetPaymentByID(ctx context.Context, id uuid.UUID) (*domain.Payment, error)
	UpdatePaymentStatus(ctx context.Context, id uuid.UUID, fromStatus, toStatus domain.Status) error
	UpdatePaymentStatusWithPSP(ctx context.Context, id uuid.UUID, fromStatus, toStatus domain.Status, pspName, pspReferenceID string) error
	UpdatePaymentFailed(ctx context.Context, id uuid.UUID, fromStatus domain.Status, pspName, pspReferenceID, failureReason string) error
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
	repo     PaymentRepository
	router   *psp.Router
	retryCfg retry.Config
}

// NewPaymentService returns a service backed by repo and the PSP router (retry + circuit breaker live on adapters).
func NewPaymentService(repo PaymentRepository, router *psp.Router) *PaymentService {
	if repo == nil || router == nil {
		return nil
	}
	return &PaymentService{
		repo:     repo,
		router:   router,
		retryCfg: retry.DefaultConfig(),
	}
}

var (
	// ErrValidation indicates the client sent invalid input.
	ErrValidation = errors.New("validation error")
)

// CreatePayment validates input, persists a new payment, routes to a PSP, charges with retries, and updates status.
func (s *PaymentService) CreatePayment(ctx context.Context, req domain.CreatePaymentRequest) (*domain.CreatePaymentResponse, error) {
	if s == nil || s.repo == nil || s.router == nil {
		return nil, errors.New("payment service is not configured")
	}
	if err := validateCreatePaymentRequest(&req); err != nil {
		return nil, err
	}

	id, err := domain.NewID()
	if err != nil {
		return nil, fmt.Errorf("generate payment id: %w", err)
	}

	amountInt64 := req.Amount.Int64()
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

	adapter, routeErr := s.router.Select(payment.Currency, amountInt64)
	if routeErr != nil {
		reason := routeErr.Error()
		if err := s.repo.UpdatePaymentFailed(ctx, id, domain.StatusProcessing, "", "", reason); err != nil {
			return nil, err
		}
		return &domain.CreatePaymentResponse{PaymentID: id, Status: domain.StatusFailed}, nil
	}

	pspName := adapter.Name()
	chargeReq := psp.ChargeRequest{
		PaymentID:      id,
		Amount:         amountInt64,
		Currency:       payment.Currency,
		IdempotencyKey: payment.IdempotencyKey,
	}

	resp, chargeErr := retry.Do(ctx, s.retryCfg, func() (*psp.ChargeResponse, error) {
		return adapter.Charge(ctx, chargeReq)
	})
	if chargeErr != nil {
		if err := s.repo.UpdatePaymentFailed(ctx, id, domain.StatusProcessing, pspName, "", formatChargeFailureReason(chargeErr)); err != nil {
			return nil, err
		}
		return &domain.CreatePaymentResponse{PaymentID: id, Status: domain.StatusFailed}, nil
	}

	switch resp.Status {
	case psp.ChargeStatusCaptured, psp.ChargeStatusAuthorized:
		if err := s.repo.UpdatePaymentStatusWithPSP(ctx, id, domain.StatusProcessing, domain.StatusSuccess, pspName, resp.PSPReferenceID); err != nil {
			return nil, err
		}
		return &domain.CreatePaymentResponse{PaymentID: id, Status: domain.StatusSuccess}, nil
	case psp.ChargeStatusDeclined, psp.ChargeStatusError:
		reason := chargeDeclineReason(resp)
		if err := s.repo.UpdatePaymentFailed(ctx, id, domain.StatusProcessing, pspName, resp.PSPReferenceID, reason); err != nil {
			return nil, err
		}
		return &domain.CreatePaymentResponse{PaymentID: id, Status: domain.StatusFailed}, nil
	default:
		reason := fmt.Sprintf("unexpected_psp_status:%s", resp.Status)
		if err := s.repo.UpdatePaymentFailed(ctx, id, domain.StatusProcessing, pspName, resp.PSPReferenceID, reason); err != nil {
			return nil, err
		}
		return &domain.CreatePaymentResponse{PaymentID: id, Status: domain.StatusFailed}, nil
	}
}

func formatChargeFailureReason(err error) string {
	if err == nil {
		return ""
	}
	var pe *psp.Error
	if errors.As(err, &pe) {
		if pe.Message != "" {
			return fmt.Sprintf("%s: %s", pe.Code, pe.Message)
		}
		return pe.Code
	}
	return err.Error()
}

func chargeDeclineReason(resp *psp.ChargeResponse) string {
	if resp == nil {
		return "declined"
	}
	if resp.Status == psp.ChargeStatusError {
		return "charge_error"
	}
	return "declined"
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
