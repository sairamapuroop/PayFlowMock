package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sairamapuroop/PayFlowMock/internal/domain"
	"github.com/sairamapuroop/PayFlowMock/internal/repository"
	"github.com/sairamapuroop/PayFlowMock/internal/service"
)

// PaymentHandler is the HTTP adapter for payment routes (thin: decode → service → encode).
type PaymentHandler struct {
	svc  service.PaymentServiceAPI
	pool *pgxpool.Pool
}

// NewPaymentHandler returns a handler that uses svc for payment operations and pool for /healthz DB checks.
// pool may be nil; in that case /healthz reports the database as unavailable.
func NewPaymentHandler(svc service.PaymentServiceAPI, pool *pgxpool.Pool) *PaymentHandler {
	return &PaymentHandler{svc: svc, pool: pool}
}

// Register mounts payment and health routes on r.
func (h *PaymentHandler) Register(r chi.Router) {
	if h == nil {
		return
	}
	r.Route("/v1", func(r chi.Router) {
		r.Post("/payments", h.createPayment)
		r.Get("/payments/{id}", h.getPayment)
		r.Post("/payments/{id}/refund", h.refundPayment)
	})
	r.Get("/healthz", h.healthz)
}

func (h *PaymentHandler) createPayment(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.svc == nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL_ERROR", "service unavailable")
		return
	}
	var req domain.CreatePaymentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_JSON", "request body must be valid JSON")
		return
	}
	resp, err := h.svc.CreatePayment(r.Context(), req)
	if err != nil {
		mapServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (h *PaymentHandler) getPayment(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.svc == nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL_ERROR", "service unavailable")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid payment id")
		return
	}
	p, err := h.svc.GetPayment(r.Context(), id)
	if err != nil {
		mapServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, paymentToHTTPDetail(p))
}

func (h *PaymentHandler) refundPayment(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.svc == nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL_ERROR", "service unavailable")
		return
	}
	paymentID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", "invalid payment id")
		return
	}
	var req domain.RefundRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_JSON", "request body must be valid JSON")
		return
	}
	req.PaymentID = paymentID

	resp, err := h.svc.RefundPayment(r.Context(), paymentID, req)
	if err != nil {
		mapServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// healthz returns 200 when the database is reachable; 503 otherwise.
func (h *PaymentHandler) healthz(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.pool == nil {
		writeErr(w, http.StatusServiceUnavailable, "DB_UNAVAILABLE", "database client not configured")
		return
	}
	ctx := r.Context()
	if err := h.pool.Ping(ctx); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "DB_UNAVAILABLE", "database ping failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// paymentHTTPDetail is the JSON shape for GET payment (amount as decimal string; big.Int does not marshal usefully).
type paymentHTTPDetail struct {
	ID             uuid.UUID     `json:"id"`
	MerchantID     uuid.UUID     `json:"merchant_id"`
	Amount         string        `json:"amount"`
	Currency       string        `json:"currency"`
	Status         domain.Status `json:"status"`
	IdempotencyKey string        `json:"idempotency_key"`
	PSP            string        `json:"psp,omitempty"`
	PSPReferenceID string        `json:"psp_reference_id,omitempty"`
	FailureReason  string        `json:"failure_reason,omitempty"`
	CreatedAt      string        `json:"created_at"`
	UpdatedAt      string        `json:"updated_at"`
}

func paymentToHTTPDetail(p *domain.Payment) paymentHTTPDetail {
	if p == nil {
		return paymentHTTPDetail{}
	}
	return paymentHTTPDetail{
		ID:             p.ID,
		MerchantID:     p.MerchantID,
		Amount:         p.Amount.String(),
		Currency:       p.Currency,
		Status:         p.Status,
		IdempotencyKey: p.IdempotencyKey,
		PSP:            p.PSP,
		PSPReferenceID: p.PSPReferenceID,
		FailureReason:  p.FailureReason,
		CreatedAt:      p.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
		UpdatedAt:      p.UpdatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(true)
	_ = enc.Encode(v)
}

type errBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeErr(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	var b errBody
	b.Error.Code = code
	b.Error.Message = message
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(true)
	_ = enc.Encode(b)
}

func mapServiceError(w http.ResponseWriter, err error) {
	if err == nil {
		return
	}

	switch {
	case errors.Is(err, repository.ErrNotFound):
		writeErr(w, http.StatusNotFound, "NOT_FOUND", strings.TrimSpace(err.Error()))
	case errors.Is(err, repository.ErrConflict):
		writeErr(w, http.StatusConflict, "CONFLICT", strings.TrimSpace(err.Error()))
	case errors.Is(err, service.ErrValidation),
		errors.Is(err, repository.ErrInvalidPayment),
		errors.Is(err, repository.ErrInvalidAmount),
		errors.Is(err, repository.ErrInvalidIdempotencyKey):
		writeErr(w, http.StatusBadRequest, "VALIDATION_ERROR", strings.TrimSpace(err.Error()))
	case errors.Is(err, repository.ErrStatusMismatch), errors.Is(err, repository.ErrInvalidStatusTransition):
		writeErr(w, http.StatusConflict, "INVALID_TRANSITION", strings.TrimSpace(err.Error()))
	case errors.Is(err, repository.ErrRepositoryNotConfigured):
		writeErr(w, http.StatusInternalServerError, "INTERNAL_ERROR", "repository not configured")
	default:
		writeErr(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal server error")
	}
}
