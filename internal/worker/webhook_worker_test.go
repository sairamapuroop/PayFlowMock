package worker

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sairamapuroop/PayFlowMock/internal/domain"
	"github.com/sairamapuroop/PayFlowMock/internal/merchant"
	"github.com/sairamapuroop/PayFlowMock/internal/repository"
	"github.com/sairamapuroop/PayFlowMock/internal/testutil"
)

type stubRegistry struct {
	secret string
}

func (stubRegistry) WebhookURL(uuid.UUID) (string, bool) {
	return "", false
}

func (s stubRegistry) Secret(uuid.UUID) (string, bool) {
	if strings.TrimSpace(s.secret) == "" {
		return "", false
	}
	return s.secret, true
}

func newWorkerTestDeps(t *testing.T) (context.Context, *pgxpool.Pool, *repository.OutboxRepo, *repository.PaymentRepo) {
	t.Helper()
	ctx := context.Background()
	pool := testutil.MustPool(t)
	testutil.MustMigrateUp(t, pool)
	testutil.Truncate(t, pool)
	outbox := repository.NewOutboxRepo(pool)
	reg := merchant.NewEnvRegistry(func(string) string { return "" })
	payRepo := repository.NewPaymentRepo(pool, outbox, reg)
	return ctx, pool, outbox, payRepo
}

func insertOutboxRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, p *domain.Payment, merchantID, eventID uuid.UUID, webhookURL string, payload []byte, status string, attemptCount int, nextRetry, createdAt, updatedAt time.Time) {
	t.Helper()
	_, err := pool.Exec(ctx, `
		INSERT INTO outbox_events (
			id, payment_id, merchant_id, event_type, webhook_url, payload,
			status, attempt_count, next_retry_at, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7,$8,$9,$10,$11)`,
		eventID, p.ID, merchantID, domain.EventPaymentSuccess, webhookURL, string(payload),
		status, attemptCount, nextRetry, createdAt, updatedAt,
	)
	if err != nil {
		t.Fatalf("insert outbox: %v", err)
	}
}

func expectOutboxStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id uuid.UUID, wantStatus string) {
	t.Helper()
	var st string
	if err := pool.QueryRow(ctx, `SELECT status FROM outbox_events WHERE id = $1`, id).Scan(&st); err != nil {
		t.Fatalf("select status: %v", err)
	}
	if st != wantStatus {
		t.Fatalf("status: got %q want %q", st, wantStatus)
	}
}

func signTestPayload(secret string, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(payload)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func requireJSONEqual(t *testing.T, a, b []byte) {
	t.Helper()
	var va, vb any
	if err := json.Unmarshal(a, &va); err != nil {
		t.Fatalf("unmarshal a: %v", err)
	}
	if err := json.Unmarshal(b, &vb); err != nil {
		t.Fatalf("unmarshal b: %v", err)
	}
	if !reflect.DeepEqual(va, vb) {
		t.Fatalf("JSON not equal: %s vs %s", a, b)
	}
}

func testWorkerCfg() Config {
	cfg := DefaultConfig()
	cfg.PollInterval = 50 * time.Millisecond
	cfg.StaleReclaimMul = 2
	cfg.BatchSize = 10
	cfg.WorkerPool = 4
	cfg.HTTPTimeout = 5 * time.Second
	return cfg
}

func TestWorker_DeliverSuccess_MarksDelivered_HeadersAndSignature(t *testing.T) {
	ctx, pool, outbox, payRepo := newWorkerTestDeps(t)
	merchantID := uuid.MustParse("a1111111-1111-1111-1111-111111111111")
	p := testutil.NewPayment(t, merchantID, "worker-ok-1")
	if err := payRepo.CreatePayment(ctx, p); err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}

	payload := []byte(`{"hello":"world"}`)
	eventID := uuid.MustParse("b1111111-1111-1111-1111-111111111111")
	secret := "signing-secret-test"
	past := time.Now().UTC().Add(-time.Minute)

	var (
		gotMethod      string
		gotBody        []byte
		hdrContentType string
		hdrEventID     string
		hdrEventType   string
		hdrIdem        string
		hdrSig         string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		hdrContentType = r.Header.Get("Content-Type")
		hdrEventID = r.Header.Get("X-PayFlow-Event-Id")
		hdrEventType = r.Header.Get("X-PayFlow-Event-Type")
		hdrIdem = r.Header.Get("Idempotency-Key")
		hdrSig = r.Header.Get("X-PayFlow-Signature")
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	insertOutboxRow(t, ctx, pool, p, merchantID, eventID, srv.URL, payload, domain.OutboxStatusPending, 0, past, past, past)

	w := New(outbox, stubRegistry{secret: secret}, testWorkerCfg())
	staleAfter := time.Duration(w.cfg.StaleReclaimMul) * w.pollEvery
	w.runOnce(ctx, staleAfter)

	if gotMethod != http.MethodPost {
		t.Fatalf("method: got %q want POST", gotMethod)
	}
	requireJSONEqual(t, payload, gotBody)
	if hdrContentType != "application/json" {
		t.Fatalf("Content-Type: got %q", hdrContentType)
	}
	if hdrEventID != eventID.String() {
		t.Fatalf("X-PayFlow-Event-Id: got %q want %q", hdrEventID, eventID)
	}
	if hdrEventType != domain.EventPaymentSuccess {
		t.Fatalf("X-PayFlow-Event-Type: got %q want %q", hdrEventType, domain.EventPaymentSuccess)
	}
	if hdrIdem != eventID.String() {
		t.Fatalf("Idempotency-Key: got %q want %q", hdrIdem, eventID)
	}
	wantSig := signTestPayload(secret, gotBody)
	if hdrSig != wantSig {
		t.Fatalf("X-PayFlow-Signature: got %q want %q", hdrSig, wantSig)
	}

	expectOutboxStatus(t, ctx, pool, eventID, domain.OutboxStatusDelivered)
	var deliveredAt time.Time
	if err := pool.QueryRow(ctx, `SELECT delivered_at FROM outbox_events WHERE id = $1`, eventID).Scan(&deliveredAt); err != nil {
		t.Fatalf("delivered_at: %v", err)
	}
	if deliveredAt.IsZero() {
		t.Fatalf("delivered_at should be set")
	}
}

func TestWorker_Non2xxResponse_MarksRetry(t *testing.T) {
	ctx, pool, outbox, payRepo := newWorkerTestDeps(t)
	merchantID := uuid.MustParse("c2222222-2222-2222-2222-222222222222")
	p := testutil.NewPayment(t, merchantID, "worker-retry-1")
	if err := payRepo.CreatePayment(ctx, p); err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}
	eventID := uuid.MustParse("d2222222-2222-2222-2222-222222222222")
	past := time.Now().UTC().Add(-time.Minute)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	insertOutboxRow(t, ctx, pool, p, merchantID, eventID, srv.URL, []byte(`{}`), domain.OutboxStatusPending, 0, past, past, past)

	w := New(outbox, stubRegistry{secret: "x"}, testWorkerCfg())
	w.runOnce(ctx, time.Duration(w.cfg.StaleReclaimMul)*w.pollEvery)

	var st string
	var attempts int
	var nextRetry time.Time
	var lastErr *string
	if err := pool.QueryRow(ctx,
		`SELECT status, attempt_count, next_retry_at, last_error FROM outbox_events WHERE id = $1`,
		eventID,
	).Scan(&st, &attempts, &nextRetry, &lastErr); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if st != domain.OutboxStatusPending {
		t.Fatalf("status: got %q want PENDING", st)
	}
	if attempts != 1 {
		t.Fatalf("attempt_count: got %d want 1", attempts)
	}
	if !nextRetry.After(time.Now().UTC()) {
		t.Fatalf("next_retry_at should be in the future: %v", nextRetry)
	}
	if lastErr == nil || !strings.Contains(*lastErr, "500") {
		t.Fatalf("last_error: got %v want http 500 mention", lastErr)
	}
}

func TestWorker_NoSecret_OmitsSignatureHeader(t *testing.T) {
	ctx, pool, outbox, payRepo := newWorkerTestDeps(t)
	merchantID := uuid.MustParse("e3333333-3333-3333-3333-333333333333")
	p := testutil.NewPayment(t, merchantID, "worker-nosig-1")
	if err := payRepo.CreatePayment(ctx, p); err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}
	eventID := uuid.MustParse("f3333333-3333-3333-3333-333333333333")
	past := time.Now().UTC().Add(-time.Minute)

	var hdrSig string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdrSig = r.Header.Get("X-PayFlow-Signature")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	insertOutboxRow(t, ctx, pool, p, merchantID, eventID, srv.URL, []byte(`{}`), domain.OutboxStatusPending, 0, past, past, past)

	w := New(outbox, nil, testWorkerCfg())
	w.runOnce(ctx, time.Duration(w.cfg.StaleReclaimMul)*w.pollEvery)

	if hdrSig != "" {
		t.Fatalf("X-PayFlow-Signature: got %q want empty", hdrSig)
	}
	expectOutboxStatus(t, ctx, pool, eventID, domain.OutboxStatusDelivered)
}

func TestWorker_FinalAttempt_MarksDead(t *testing.T) {
	ctx, pool, outbox, payRepo := newWorkerTestDeps(t)
	merchantID := uuid.MustParse("04444444-4444-4444-4444-444444444444")
	p := testutil.NewPayment(t, merchantID, "worker-dead-1")
	if err := payRepo.CreatePayment(ctx, p); err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}
	eventID := uuid.MustParse("14444444-4444-4444-4444-444444444444")
	past := time.Now().UTC().Add(-time.Minute)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)

	insertOutboxRow(t, ctx, pool, p, merchantID, eventID, srv.URL, []byte(`{}`), domain.OutboxStatusPending, 2, past, past, past)

	cfg := testWorkerCfg()
	cfg.MaxAttempts = 3
	w := New(outbox, stubRegistry{secret: "x"}, cfg)
	w.runOnce(ctx, time.Duration(w.cfg.StaleReclaimMul)*w.pollEvery)

	expectOutboxStatus(t, ctx, pool, eventID, domain.OutboxStatusDead)
	var attempts int
	var lastErr *string
	if err := pool.QueryRow(ctx, `SELECT attempt_count, last_error FROM outbox_events WHERE id = $1`, eventID).
		Scan(&attempts, &lastErr); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempt_count: got %d want 2 (unchanged on dead)", attempts)
	}
	if lastErr == nil || !strings.Contains(*lastErr, "502") {
		t.Fatalf("last_error: got %v", lastErr)
	}
}

func TestWorker_EmptyWebhookURL_MarksDead(t *testing.T) {
	ctx, pool, outbox, payRepo := newWorkerTestDeps(t)
	merchantID := uuid.MustParse("a5555555-5555-5555-5555-555555555555")
	p := testutil.NewPayment(t, merchantID, "worker-empty-url-1")
	if err := payRepo.CreatePayment(ctx, p); err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}
	eventID := uuid.MustParse("b5555555-5555-5555-5555-555555555555")
	past := time.Now().UTC().Add(-time.Minute)

	insertOutboxRow(t, ctx, pool, p, merchantID, eventID, "", []byte(`{}`), domain.OutboxStatusPending, 0, past, past, past)

	w := New(outbox, stubRegistry{secret: "x"}, testWorkerCfg())
	w.runOnce(ctx, time.Duration(w.cfg.StaleReclaimMul)*w.pollEvery)

	expectOutboxStatus(t, ctx, pool, eventID, domain.OutboxStatusDead)
	var lastErr *string
	if err := pool.QueryRow(ctx, `SELECT last_error FROM outbox_events WHERE id = $1`, eventID).Scan(&lastErr); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if lastErr == nil || *lastErr != "no_webhook_url_for_merchant" {
		t.Fatalf("last_error: got %v want no_webhook_url_for_merchant", lastErr)
	}
}

func TestWorker_ReclaimsStaleProcessingThenDelivers(t *testing.T) {
	ctx, pool, outbox, payRepo := newWorkerTestDeps(t)
	merchantID := uuid.MustParse("c6666666-6666-6666-6666-666666666666")
	p := testutil.NewPayment(t, merchantID, "worker-reclaim-1")
	if err := payRepo.CreatePayment(ctx, p); err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}
	eventID := uuid.MustParse("d6666666-6666-6666-6666-666666666666")
	base := time.Now().UTC().Add(-24 * time.Hour)
	staleUpdated := time.Now().UTC().Add(-2 * time.Hour)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	payload := []byte(`{"reclaimed":true}`)
	_, err := pool.Exec(ctx, `
		INSERT INTO outbox_events (
			id, payment_id, merchant_id, event_type, webhook_url, payload,
			status, attempt_count, next_retry_at, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7,0,$8,$9,$10)`,
		eventID, p.ID, merchantID, domain.EventPaymentSuccess, srv.URL, string(payload),
		domain.OutboxStatusProcessing, base, base, staleUpdated,
	)
	if err != nil {
		t.Fatalf("insert stale processing: %v", err)
	}

	cfg := testWorkerCfg()
	w := New(outbox, stubRegistry{secret: "reclaim-secret"}, cfg)
	staleAfter := time.Duration(cfg.StaleReclaimMul) * cfg.PollInterval
	w.runOnce(ctx, staleAfter)

	expectOutboxStatus(t, ctx, pool, eventID, domain.OutboxStatusDelivered)
}
