package service

import (
	"context"
	"errors"
	"math/big"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sairamapuroop/PayFlowMock/internal/domain"
	"github.com/sairamapuroop/PayFlowMock/internal/psp"
	"github.com/sairamapuroop/PayFlowMock/internal/repository"
	"github.com/sairamapuroop/PayFlowMock/internal/retry"
)

const fakeAdapterName = "fake_psp"

// fakePaymentRepo is an in-memory PaymentRepository for service-level tests.
type fakePaymentRepo struct {
	mu       sync.Mutex
	payments map[uuid.UUID]*domain.Payment
}

func newFakePaymentRepo() *fakePaymentRepo {
	return &fakePaymentRepo{payments: make(map[uuid.UUID]*domain.Payment)}
}

func (f *fakePaymentRepo) clonePayment(p *domain.Payment) *domain.Payment {
	if p == nil {
		return nil
	}
	cp := *p
	cp.Amount = *new(big.Int).Set(&p.Amount)
	return &cp
}

func (f *fakePaymentRepo) CreatePayment(ctx context.Context, payment *domain.Payment) error {
	_ = ctx
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.payments[payment.ID]; exists {
		return repository.ErrConflict
	}
	f.payments[payment.ID] = f.clonePayment(payment)
	return nil
}

func (f *fakePaymentRepo) GetPaymentByID(ctx context.Context, id uuid.UUID) (*domain.Payment, error) {
	_ = ctx
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.payments[id]
	if !ok {
		return nil, repository.ErrNotFound
	}
	return f.clonePayment(p), nil
}

func (f *fakePaymentRepo) UpdatePaymentStatus(ctx context.Context, id uuid.UUID, fromStatus, toStatus domain.Status) error {
	_ = ctx
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.payments[id]
	if !ok {
		return repository.ErrNotFound
	}
	if p.Status != fromStatus {
		return repository.ErrStatusMismatch
	}
	p.Status = toStatus
	return nil
}

func (f *fakePaymentRepo) UpdatePaymentStatusWithPSP(ctx context.Context, id uuid.UUID, fromStatus, toStatus domain.Status, pspName, pspReferenceID string) error {
	_ = ctx
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.payments[id]
	if !ok {
		return repository.ErrNotFound
	}
	if p.Status != fromStatus {
		return repository.ErrStatusMismatch
	}
	p.Status = toStatus
	p.PSP = pspName
	p.PSPReferenceID = pspReferenceID
	return nil
}

func (f *fakePaymentRepo) UpdatePaymentFailed(ctx context.Context, id uuid.UUID, fromStatus domain.Status, pspName, pspReferenceID, failureReason string) error {
	_ = ctx
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.payments[id]
	if !ok {
		return repository.ErrNotFound
	}
	if p.Status != fromStatus {
		return repository.ErrStatusMismatch
	}
	p.Status = domain.StatusFailed
	p.PSP = pspName
	p.PSPReferenceID = pspReferenceID
	p.FailureReason = failureReason
	return nil
}

func (f *fakePaymentRepo) RefundPaymentAtomic(ctx context.Context, paymentID uuid.UUID, refundAmount int64, idempotencyKey string) (uuid.UUID, error) {
	_ = ctx
	_ = paymentID
	_ = refundAmount
	_ = idempotencyKey
	return uuid.Nil, errors.New("fakePaymentRepo: RefundPaymentAtomic not supported in these tests")
}

// fakePSPAdapter implements psp.PSPAdapter with configurable Charge behavior.
type fakePSPAdapter struct {
	name     string
	chargeFn func(ctx context.Context, req psp.ChargeRequest) (*psp.ChargeResponse, error)
}

func (f *fakePSPAdapter) Name() string {
	if f.name != "" {
		return f.name
	}
	return fakeAdapterName
}

func (f *fakePSPAdapter) Charge(ctx context.Context, req psp.ChargeRequest) (*psp.ChargeResponse, error) {
	if f.chargeFn == nil {
		return nil, errors.New("fakePSPAdapter: Charge not configured")
	}
	return f.chargeFn(ctx, req)
}

func (f *fakePSPAdapter) Refund(ctx context.Context, req psp.RefundRequest) (*psp.RefundResponse, error) {
	_ = ctx
	_ = req
	return nil, errors.New("fakePSPAdapter: Refund not implemented")
}

func usdRouter(adapter psp.PSPAdapter) *psp.Router {
	return psp.NewRouter(
		map[string]psp.PSPAdapter{fakeAdapterName: adapter},
		[]psp.RoutingRule{{Currencies: []string{"USD"}, MinAmount: 0, MaxAmount: 0, PSP: fakeAdapterName}},
	)
}

func testUSDCreateRequest() domain.CreatePaymentRequest {
	var amount big.Int
	amount.SetInt64(500)
	return domain.CreatePaymentRequest{
		MerchantID:     uuid.MustParse("12345678-1234-7123-8123-123456789abc"),
		Amount:         amount,
		Currency:       "USD",
		IdempotencyKey: "idempotency-test-key",
	}
}

func fastRetryService(repo PaymentRepository, router *psp.Router) *PaymentService {
	svc := NewPaymentService(repo, router)
	if svc == nil {
		return nil
	}
	svc.retryCfg = retry.Config{
		MaxAttempts: 5,
		BaseDelay:   time.Microsecond,
		MaxDelay:    time.Millisecond,
		Multiplier:  2,
	}
	return svc
}

func TestCreatePayment_Success(t *testing.T) {
	t.Parallel()
	repo := newFakePaymentRepo()
	adapter := &fakePSPAdapter{
		chargeFn: func(ctx context.Context, req psp.ChargeRequest) (*psp.ChargeResponse, error) {
			_ = ctx
			if req.Currency != "USD" || req.Amount != 500 {
				t.Errorf("unexpected charge request: %+v", req)
			}
			return &psp.ChargeResponse{
				Status:         psp.ChargeStatusCaptured,
				PSPReferenceID: "ref_capture_1",
			}, nil
		},
	}
	svc := NewPaymentService(repo, usdRouter(adapter))
	resp, err := svc.CreatePayment(context.Background(), testUSDCreateRequest())
	if err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}
	if resp.Status != domain.StatusSuccess {
		t.Fatalf("status: got %q want %q", resp.Status, domain.StatusSuccess)
	}
	stored, err := repo.GetPaymentByID(context.Background(), resp.PaymentID)
	if err != nil {
		t.Fatalf("GetPaymentByID: %v", err)
	}
	if stored.Status != domain.StatusSuccess {
		t.Fatalf("stored status: got %q want %q", stored.Status, domain.StatusSuccess)
	}
	if stored.PSP != fakeAdapterName || stored.PSPReferenceID != "ref_capture_1" {
		t.Fatalf("stored PSP metadata: psp=%q ref=%q", stored.PSP, stored.PSPReferenceID)
	}
}

func TestCreatePayment_Declined(t *testing.T) {
	t.Parallel()
	repo := newFakePaymentRepo()
	adapter := &fakePSPAdapter{
		chargeFn: func(ctx context.Context, req psp.ChargeRequest) (*psp.ChargeResponse, error) {
			_ = ctx
			_ = req
			return &psp.ChargeResponse{Status: psp.ChargeStatusDeclined, PSPReferenceID: "ref_decl_1"}, nil
		},
	}
	svc := NewPaymentService(repo, usdRouter(adapter))
	resp, err := svc.CreatePayment(context.Background(), testUSDCreateRequest())
	if err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}
	if resp.Status != domain.StatusFailed {
		t.Fatalf("status: got %q want %q", resp.Status, domain.StatusFailed)
	}
	stored, err := repo.GetPaymentByID(context.Background(), resp.PaymentID)
	if err != nil {
		t.Fatalf("GetPaymentByID: %v", err)
	}
	if stored.Status != domain.StatusFailed || stored.FailureReason != "declined" {
		t.Fatalf("stored failure: status=%q reason=%q", stored.Status, stored.FailureReason)
	}
	if stored.PSP != fakeAdapterName || stored.PSPReferenceID != "ref_decl_1" {
		t.Fatalf("declined metadata: psp=%q ref=%q", stored.PSP, stored.PSPReferenceID)
	}
}

func TestCreatePayment_RetryableFailureThenSuccess(t *testing.T) {
	t.Parallel()
	repo := newFakePaymentRepo()
	var calls atomic.Int32
	adapter := &fakePSPAdapter{
		chargeFn: func(ctx context.Context, req psp.ChargeRequest) (*psp.ChargeResponse, error) {
			_ = ctx
			_ = req
			n := calls.Add(1)
			if n < 3 {
				return nil, psp.NewError(psp.CodeServiceUnavailable, "temporary", true)
			}
			return &psp.ChargeResponse{Status: psp.ChargeStatusCaptured, PSPReferenceID: "ref_after_retry"}, nil
		},
	}
	svc := fastRetryService(repo, usdRouter(adapter))
	resp, err := svc.CreatePayment(context.Background(), testUSDCreateRequest())
	if err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("charge attempts: got %d want 3", calls.Load())
	}
	if resp.Status != domain.StatusSuccess {
		t.Fatalf("status: got %q want %q", resp.Status, domain.StatusSuccess)
	}
	stored, err := repo.GetPaymentByID(context.Background(), resp.PaymentID)
	if err != nil {
		t.Fatalf("GetPaymentByID: %v", err)
	}
	if stored.PSPReferenceID != "ref_after_retry" {
		t.Fatalf("PSPReferenceID: got %q", stored.PSPReferenceID)
	}
}

func TestCreatePayment_NonRetryableFailure(t *testing.T) {
	t.Parallel()
	repo := newFakePaymentRepo()
	var calls atomic.Int32
	adapter := &fakePSPAdapter{
		chargeFn: func(ctx context.Context, req psp.ChargeRequest) (*psp.ChargeResponse, error) {
			_ = ctx
			_ = req
			calls.Add(1)
			return nil, psp.NewError(psp.CodeCardDeclined, "do not retry", false)
		},
	}
	svc := fastRetryService(repo, usdRouter(adapter))
	resp, err := svc.CreatePayment(context.Background(), testUSDCreateRequest())
	if err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("charge attempts: got %d want 1", calls.Load())
	}
	if resp.Status != domain.StatusFailed {
		t.Fatalf("status: got %q want %q", resp.Status, domain.StatusFailed)
	}
	stored, err := repo.GetPaymentByID(context.Background(), resp.PaymentID)
	if err != nil {
		t.Fatalf("GetPaymentByID: %v", err)
	}
	wantReason := psp.CodeCardDeclined + ": do not retry"
	if stored.FailureReason != wantReason {
		t.Fatalf("failure reason: got %q want %q", stored.FailureReason, wantReason)
	}
}

func TestCreatePayment_NoRoute(t *testing.T) {
	t.Parallel()
	repo := newFakePaymentRepo()
	// No rules → Select always returns ErrNoRoute; adapter is never used.
	svc := NewPaymentService(repo, psp.NewRouter(map[string]psp.PSPAdapter{fakeAdapterName: &fakePSPAdapter{}}, nil))
	resp, err := svc.CreatePayment(context.Background(), testUSDCreateRequest())
	if err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}
	if resp.Status != domain.StatusFailed {
		t.Fatalf("status: got %q want %q", resp.Status, domain.StatusFailed)
	}
	stored, err := repo.GetPaymentByID(context.Background(), resp.PaymentID)
	if err != nil {
		t.Fatalf("GetPaymentByID: %v", err)
	}
	if stored.Status != domain.StatusFailed {
		t.Fatalf("stored status: got %q", stored.Status)
	}
	if stored.FailureReason != psp.ErrNoRoute.Error() {
		t.Fatalf("failure reason: got %q want %q", stored.FailureReason, psp.ErrNoRoute.Error())
	}
	if stored.PSP != "" {
		t.Fatalf("expected empty PSP on no-route, got %q", stored.PSP)
	}
}
