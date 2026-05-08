package repository

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sairamapuroop/PayFlowMock/internal/domain"
	"github.com/sairamapuroop/PayFlowMock/internal/merchant"
	"github.com/sairamapuroop/PayFlowMock/internal/testutil"
)

const testWebhookURL = "https://hooks.payflowmock.test/outbox"

type outboxPayloadEnvelope struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	CreatedAt string `json:"created_at"`
	Data      struct {
		PaymentID      string `json:"payment_id"`
		MerchantID     string `json:"merchant_id"`
		Amount         int64  `json:"amount"`
		Currency       string `json:"currency"`
		Status         string `json:"status"`
		PSP            string `json:"psp"`
		PSPReferenceID string `json:"psp_reference_id"`
		FailureReason  string `json:"failure_reason"`
		RefundID       string `json:"refund_id"`
		RefundAmount   int64  `json:"refund_amount"`
	} `json:"data"`
}

func newPaymentOutboxTestDeps(t *testing.T) (context.Context, *pgxpool.Pool, *PaymentRepo) {
	t.Helper()
	ctx := context.Background()
	pool := testutil.MustPool(t)
	testutil.MustMigrateUp(t, pool)
	testutil.Truncate(t, pool)

	t.Setenv("DEFAULT_WEBHOOK_URL", testWebhookURL)
	reg := merchant.NewEnvRegistry(os.Getenv)
	outbox := NewOutboxRepo(pool)
	repo := NewPaymentRepo(pool, outbox, reg)
	return ctx, pool, repo
}

func countOutbox(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM outbox_events`).Scan(&n); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	return n
}

func mustDecodeOutboxPayload(t *testing.T, raw []byte) outboxPayloadEnvelope {
	t.Helper()
	var env outboxPayloadEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if _, err := uuid.Parse(env.ID); err != nil {
		t.Fatalf("payload id not a UUID: %q", env.ID)
	}
	return env
}

func TestUpdatePaymentStatusWithPSP_EmitsSuccessOutbox(t *testing.T) {
	ctx, pool, repo := newPaymentOutboxTestDeps(t)
	merchantID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	p := testutil.NewPayment(t, merchantID, "outbox-success-1")
	if err := repo.CreatePayment(ctx, p); err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}
	if err := repo.UpdatePaymentStatus(ctx, p.ID, domain.StatusInitiated, domain.StatusProcessing); err != nil {
		t.Fatalf("to processing: %v", err)
	}

	pspName := "stripe"
	pspRef := "pi_test_success_1"
	if err := repo.UpdatePaymentStatusWithPSP(ctx, p.ID, domain.StatusProcessing, domain.StatusSuccess, pspName, pspRef); err != nil {
		t.Fatalf("UpdatePaymentStatusWithPSP: %v", err)
	}
	if countOutbox(t, ctx, pool) != 1 {
		t.Fatalf("outbox rows: got %d want 1", countOutbox(t, ctx, pool))
	}

	var eventType, url, status string
	var payload []byte
	err := pool.QueryRow(ctx,
		`SELECT event_type, webhook_url, payload, status FROM outbox_events WHERE payment_id = $1`,
		p.ID,
	).Scan(&eventType, &url, &payload, &status)
	if err != nil {
		t.Fatalf("select outbox: %v", err)
	}
	if eventType != domain.EventPaymentSuccess {
		t.Fatalf("event_type: got %q want %q", eventType, domain.EventPaymentSuccess)
	}
	if url != testWebhookURL {
		t.Fatalf("webhook_url: got %q want %q", url, testWebhookURL)
	}
	if status != domain.OutboxStatusPending {
		t.Fatalf("status: got %q want PENDING", status)
	}

	env := mustDecodeOutboxPayload(t, payload)
	if env.Type != domain.EventPaymentSuccess {
		t.Fatalf("payload.type: got %q", env.Type)
	}
	d := env.Data
	if d.PaymentID != p.ID.String() || d.MerchantID != merchantID.String() {
		t.Fatalf("payload ids: payment_id=%q merchant_id=%q", d.PaymentID, d.MerchantID)
	}
	if d.Amount != 100 || d.Currency != "USD" || d.Status != string(domain.StatusSuccess) {
		t.Fatalf("payload data: amount=%d currency=%q status=%q", d.Amount, d.Currency, d.Status)
	}
	if d.PSP != pspName || d.PSPReferenceID != pspRef {
		t.Fatalf("payload psp: psp=%q ref=%q", d.PSP, d.PSPReferenceID)
	}
	if d.FailureReason != "" || d.RefundID != "" || d.RefundAmount != 0 {
		t.Fatalf("unexpected refund/failure fields: reason=%q refund_id=%q refund_amt=%d", d.FailureReason, d.RefundID, d.RefundAmount)
	}
}

func TestUpdatePaymentFailed_EmitsFailedOutboxWithReason(t *testing.T) {
	ctx, pool, repo := newPaymentOutboxTestDeps(t)
	merchantID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	p := testutil.NewPayment(t, merchantID, "outbox-failed-1")
	if err := repo.CreatePayment(ctx, p); err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}
	if err := repo.UpdatePaymentStatus(ctx, p.ID, domain.StatusInitiated, domain.StatusProcessing); err != nil {
		t.Fatalf("to processing: %v", err)
	}

	reason := "card_declined"
	pspName := "adyen"
	pspRef := "ref_failed_1"
	if err := repo.UpdatePaymentFailed(ctx, p.ID, domain.StatusProcessing, pspName, pspRef, reason); err != nil {
		t.Fatalf("UpdatePaymentFailed: %v", err)
	}
	if countOutbox(t, ctx, pool) != 1 {
		t.Fatalf("outbox rows: got %d want 1", countOutbox(t, ctx, pool))
	}

	var eventType, url string
	var payload []byte
	if err := pool.QueryRow(ctx,
		`SELECT event_type, webhook_url, payload FROM outbox_events WHERE payment_id = $1`, p.ID,
	).Scan(&eventType, &url, &payload); err != nil {
		t.Fatalf("select outbox: %v", err)
	}
	if eventType != domain.EventPaymentFailed {
		t.Fatalf("event_type: got %q want %q", eventType, domain.EventPaymentFailed)
	}
	if url != testWebhookURL {
		t.Fatalf("webhook_url: got %q want %q", url, testWebhookURL)
	}

	env := mustDecodeOutboxPayload(t, payload)
	if env.Type != domain.EventPaymentFailed {
		t.Fatalf("payload.type: got %q", env.Type)
	}
	d := env.Data
	if d.Status != string(domain.StatusFailed) {
		t.Fatalf("payload status: got %q", d.Status)
	}
	if d.FailureReason != reason {
		t.Fatalf("failure_reason: got %q want %q", d.FailureReason, reason)
	}
	if d.PSP != pspName || d.PSPReferenceID != pspRef {
		t.Fatalf("payload psp: psp=%q ref=%q", d.PSP, d.PSPReferenceID)
	}
}

func TestRefundPaymentAtomic_EmitsRefundedOutboxWithRefundFields(t *testing.T) {
	ctx, pool, repo := newPaymentOutboxTestDeps(t)
	merchantID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	p := testutil.NewPayment(t, merchantID, "outbox-refund-1")
	if err := repo.CreatePayment(ctx, p); err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}
	if err := repo.UpdatePaymentStatus(ctx, p.ID, domain.StatusInitiated, domain.StatusProcessing); err != nil {
		t.Fatalf("to processing: %v", err)
	}
	pspName := "stripe"
	pspRef := "pi_refund_src"
	if err := repo.UpdatePaymentStatusWithPSP(ctx, p.ID, domain.StatusProcessing, domain.StatusSuccess, pspName, pspRef); err != nil {
		t.Fatalf("to success: %v", err)
	}

	refundAmt := int64(40)
	refundID, err := repo.RefundPaymentAtomic(ctx, p.ID, refundAmt, "idem-refund-outbox-1")
	if err != nil {
		t.Fatalf("RefundPaymentAtomic: %v", err)
	}

	rows := countOutbox(t, ctx, pool)
	if rows != 2 {
		t.Fatalf("outbox rows: got %d want 2 (success + refunded)", rows)
	}

	var eventType, url string
	var payload []byte
	err = pool.QueryRow(ctx,
		`SELECT event_type, webhook_url, payload FROM outbox_events WHERE payment_id = $1 AND event_type = $2`,
		p.ID, domain.EventPaymentRefunded,
	).Scan(&eventType, &url, &payload)
	if err != nil {
		t.Fatalf("select refunded outbox: %v", err)
	}
	if eventType != domain.EventPaymentRefunded {
		t.Fatalf("event_type: got %q", eventType)
	}
	if url != testWebhookURL {
		t.Fatalf("webhook_url: got %q want %q", url, testWebhookURL)
	}

	env := mustDecodeOutboxPayload(t, payload)
	if env.Type != domain.EventPaymentRefunded {
		t.Fatalf("payload.type: got %q", env.Type)
	}
	d := env.Data
	if d.Status != string(domain.StatusRefunded) {
		t.Fatalf("payload status: got %q", d.Status)
	}
	if d.RefundID != refundID.String() {
		t.Fatalf("refund_id: got %q want %q", d.RefundID, refundID)
	}
	if d.RefundAmount != refundAmt {
		t.Fatalf("refund_amount: got %d want %d", d.RefundAmount, refundAmt)
	}
	if d.Amount != 100 || d.Currency != "USD" {
		t.Fatalf("payment snapshot: amount=%d currency=%q", d.Amount, d.Currency)
	}
	if d.PSP != pspName || d.PSPReferenceID != pspRef {
		t.Fatalf("payload psp: psp=%q ref=%q", d.PSP, d.PSPReferenceID)
	}
}

func TestPaymentMutator_NoOutboxOnStatusMismatch(t *testing.T) {
	ctx, pool, repo := newPaymentOutboxTestDeps(t)
	merchantID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	p := testutil.NewPayment(t, merchantID, "outbox-mismatch-1")
	if err := repo.CreatePayment(ctx, p); err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}
	if err := repo.UpdatePaymentStatus(ctx, p.ID, domain.StatusInitiated, domain.StatusProcessing); err != nil {
		t.Fatalf("to processing: %v", err)
	}

	// Valid transition pair, but DB is already "processing"; UPDATE matches zero rows → mismatch, no outbox enqueue.
	err := repo.UpdatePaymentStatusWithPSP(ctx, p.ID, domain.StatusInitiated, domain.StatusProcessing, "x", "y")
	if !errors.Is(err, ErrStatusMismatch) {
		t.Fatalf("UpdatePaymentStatusWithPSP: got %v want %v", err, ErrStatusMismatch)
	}
	if countOutbox(t, ctx, pool) != 0 {
		t.Fatalf("outbox rows: got %d want 0", countOutbox(t, ctx, pool))
	}
}

func TestPaymentMutator_NoWebhookURL_StoresEmptyURL(t *testing.T) {
	ctx := context.Background()
	pool := testutil.MustPool(t)
	testutil.MustMigrateUp(t, pool)
	testutil.Truncate(t, pool)

	t.Setenv("DEFAULT_WEBHOOK_URL", "")
	t.Setenv("MERCHANT_WEBHOOK_URLS", "")
	reg := merchant.NewEnvRegistry(os.Getenv)
	repo := NewPaymentRepo(pool, NewOutboxRepo(pool), reg)

	merchantID := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	p := testutil.NewPayment(t, merchantID, "outbox-empty-url-1")
	if err := repo.CreatePayment(ctx, p); err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}
	if err := repo.UpdatePaymentStatus(ctx, p.ID, domain.StatusInitiated, domain.StatusProcessing); err != nil {
		t.Fatalf("to processing: %v", err)
	}
	if err := repo.UpdatePaymentStatusWithPSP(ctx, p.ID, domain.StatusProcessing, domain.StatusSuccess, "psp", "ref"); err != nil {
		t.Fatalf("UpdatePaymentStatusWithPSP: %v", err)
	}

	var webhookURL string
	if err := pool.QueryRow(ctx, `SELECT webhook_url FROM outbox_events WHERE payment_id = $1`, p.ID).
		Scan(&webhookURL); err != nil {
		t.Fatalf("select webhook_url: %v", err)
	}
	if webhookURL != "" {
		t.Fatalf("webhook_url: got %q want empty", webhookURL)
	}
}
