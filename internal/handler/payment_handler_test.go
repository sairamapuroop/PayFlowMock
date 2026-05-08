package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sairamapuroop/PayFlowMock/internal/domain"
	"github.com/sairamapuroop/PayFlowMock/internal/repository"
	"github.com/sairamapuroop/PayFlowMock/internal/testutil"
)

// stubPaymentService implements service.PaymentServiceAPI with configurable errors for handler tests.
type stubPaymentService struct {
	createErr error
	getErr    error
	refundErr error
}

func (s *stubPaymentService) CreatePayment(ctx context.Context, req domain.CreatePaymentRequest) (*domain.CreatePaymentResponse, error) {
	if s.createErr != nil {
		return nil, s.createErr
	}
	return nil, repository.ErrRepositoryNotConfigured
}

func (s *stubPaymentService) GetPayment(ctx context.Context, id uuid.UUID) (*domain.Payment, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	return nil, repository.ErrNotFound
}

func (s *stubPaymentService) RefundPayment(ctx context.Context, paymentID uuid.UUID, req domain.RefundRequest) (*domain.RefundResponse, error) {
	if s.refundErr != nil {
		return nil, s.refundErr
	}
	return nil, repository.ErrNotFound
}

func routerWithHandler(stub *stubPaymentService, pool *pgxpool.Pool) chi.Router {
	r := chi.NewRouter()
	h := NewPaymentHandler(stub, pool)
	h.Register(r)
	return r
}

func decodeErrBody(t *testing.T, body io.Reader) (code, message string) {
	t.Helper()
	var out struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(body).Decode(&out); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return out.Error.Code, out.Error.Message
}

func TestHandler_InvalidJSON_PostPayment(t *testing.T) {
	stub := &stubPaymentService{}
	r := routerWithHandler(stub, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/payments", strings.NewReader("{not-json"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusBadRequest)
	}
	code, _ := decodeErrBody(t, rec.Body)
	if code != "INVALID_JSON" {
		t.Fatalf("error code: got %q, want INVALID_JSON", code)
	}
}

func TestHandler_InvalidJSON_Refund(t *testing.T) {
	stub := &stubPaymentService{}
	r := routerWithHandler(stub, nil)
	pid := uuid.MustParse("11111111-1111-1111-1111-111111111111")

	req := httptest.NewRequest(http.MethodPost, "/v1/payments/"+pid.String()+"/refund", strings.NewReader("{"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusBadRequest)
	}
	code, _ := decodeErrBody(t, rec.Body)
	if code != "INVALID_JSON" {
		t.Fatalf("error code: got %q, want INVALID_JSON", code)
	}
}

func TestHandler_InvalidUUID_GetPayment(t *testing.T) {
	stub := &stubPaymentService{}
	r := routerWithHandler(stub, nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/payments/not-a-uuid", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusBadRequest)
	}
	code, msg := decodeErrBody(t, rec.Body)
	if code != "VALIDATION_ERROR" {
		t.Fatalf("error code: got %q, want VALIDATION_ERROR", code)
	}
	if !strings.Contains(msg, "invalid") {
		t.Fatalf("message should mention invalid id, got %q", msg)
	}
}

func TestHandler_MapServiceError_Validation(t *testing.T) {
	stub := &stubPaymentService{createErr: repository.ErrInvalidAmount}
	r := routerWithHandler(stub, nil)

	// amount must be a JSON number so big.Int unmarshals (string values are rejected by encoding/json).
	body := bytes.NewBufferString(`{"merchant_id":"550e8400-e29b-41d4-a716-446655440000","amount":100,"currency":"USD","idempotency_key":"k"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/payments", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusBadRequest)
	}
	code, _ := decodeErrBody(t, rec.Body)
	if code != "VALIDATION_ERROR" {
		t.Fatalf("error code: got %q, want VALIDATION_ERROR", code)
	}
}

func TestHandler_MapServiceError_NotFound(t *testing.T) {
	stub := &stubPaymentService{getErr: repository.ErrNotFound}
	r := routerWithHandler(stub, nil)
	id := uuid.MustParse("22222222-2222-2222-2222-222222222222")

	req := httptest.NewRequest(http.MethodGet, "/v1/payments/"+id.String(), nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusNotFound)
	}
	code, _ := decodeErrBody(t, rec.Body)
	if code != "NOT_FOUND" {
		t.Fatalf("error code: got %q, want NOT_FOUND", code)
	}
}

func TestHealthz_Smoke(t *testing.T) {
	pool := testutil.MustPool(t)
	testutil.MustMigrateUp(t, pool)

	stub := &stubPaymentService{}
	r := routerWithHandler(stub, pool)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
	var out map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if out["status"] != "ok" {
		t.Fatalf("status field: got %q, want ok", out["status"])
	}
}
