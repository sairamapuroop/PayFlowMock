package worker

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"github.com/sairamapuroop/PayFlowMock/internal/domain"
	"github.com/sairamapuroop/PayFlowMock/internal/merchant"
	"github.com/sairamapuroop/PayFlowMock/internal/repository"
)

// Config controls webhook delivery polling and retry behavior.
type Config struct {
	PollInterval time.Duration
	BatchSize    int
	MaxAttempts  int
	// WorkerPool bounds concurrent HTTP deliveries per poll tick.
	WorkerPool int
	HTTPTimeout time.Duration
	// StaleReclaimMul scales PollInterval to decide when PROCESSING rows are reset (worker crash safety).
	StaleReclaimMul int
	BaseDelay       time.Duration
	MaxBackoff      time.Duration
	BackoffMultiplier float64
}

// DefaultConfig returns plan defaults: 5s poll, batch 20, max 10 attempts, pool 8, 10s HTTP timeout, backoff capped at 10m.
func DefaultConfig() Config {
	return Config{
		PollInterval:      5 * time.Second,
		BatchSize:         20,
		MaxAttempts:       10,
		WorkerPool:        8,
		HTTPTimeout:       10 * time.Second,
		StaleReclaimMul:   2,
		BaseDelay:         100 * time.Millisecond,
		MaxBackoff:        10 * time.Minute,
		BackoffMultiplier: 2.0,
	}
}

func (c Config) withDefaults() Config {
	if c.PollInterval <= 0 {
		c.PollInterval = 5 * time.Second
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 20
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 10
	}
	if c.WorkerPool <= 0 {
		c.WorkerPool = 8
	}
	if c.HTTPTimeout <= 0 {
		c.HTTPTimeout = 10 * time.Second
	}
	if c.StaleReclaimMul <= 0 {
		c.StaleReclaimMul = 2
	}
	if c.BaseDelay <= 0 {
		c.BaseDelay = 100 * time.Millisecond
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = 10 * time.Minute
	}
	if c.BackoffMultiplier <= 0 {
		c.BackoffMultiplier = 2.0
	}
	return c
}

// Worker drains the transactional outbox by POSTing signed JSON payloads to merchant webhook URLs.
type Worker struct {
	outbox    *repository.OutboxRepo
	registry  merchant.Registry
	client    *http.Client
	pollEvery time.Duration
	batchSize int
	maxAttempts int
	poolSize  int
	cfg       Config
}

// New constructs a Worker. registry may be nil (no HMAC signing).
func New(outbox *repository.OutboxRepo, registry merchant.Registry, cfg Config) *Worker {
	cfg = cfg.withDefaults()
	return &Worker{
		outbox:      outbox,
		registry:    registry,
		client:      &http.Client{Timeout: cfg.HTTPTimeout},
		pollEvery:   cfg.PollInterval,
		batchSize:   cfg.BatchSize,
		maxAttempts: cfg.MaxAttempts,
		poolSize:    cfg.WorkerPool,
		cfg:         cfg,
	}
}

// Run polls the outbox until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.pollEvery)
	defer ticker.Stop()

	staleAfter := time.Duration(w.cfg.StaleReclaimMul) * w.pollEvery

	for {
		if err := ctx.Err(); err != nil {
			return
		}
		w.runOnce(ctx, staleAfter)

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *Worker) runOnce(ctx context.Context, staleAfter time.Duration) {
	if _, err := w.outbox.ReclaimStaleProcessing(ctx, staleAfter); err != nil {
		log.Error().Err(err).Msg("outbox reclaim stale processing")
	}

	events, err := w.outbox.ClaimBatch(ctx, w.batchSize)
	if err != nil {
		log.Error().Err(err).Msg("outbox claim batch")
		return
	}
	if len(events) == 0 {
		return
	}

	sem := make(chan struct{}, w.poolSize)
	var wg sync.WaitGroup
	for _, evt := range events {
		evt := evt
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			w.handleOne(ctx, evt)
		}()
	}
	wg.Wait()
}

func (w *Worker) handleOne(ctx context.Context, evt *domain.OutboxEvent) {
	start := time.Now()
	zl := log.With().
		Str("event_id", evt.ID.String()).
		Str("payment_id", evt.PaymentID.String()).
		Str("merchant_id", evt.MerchantID.String()).
		Str("event_type", evt.EventType).
		Int("attempt", evt.AttemptCount+1).
		Logger()

	if strings.TrimSpace(evt.WebhookURL) == "" {
		if err := w.outbox.MarkDead(ctx, evt.ID, "no_webhook_url_for_merchant"); err != nil {
			zl.Error().Err(err).Msg("mark dead (no webhook url)")
		}
		return
	}

	httpStatus, err := w.deliver(ctx, evt)
	latency := time.Since(start)
	if ctx.Err() != nil {
		return
	}

	if err == nil {
		if derr := w.outbox.MarkDelivered(ctx, evt.ID); derr != nil {
			zl.Error().Err(derr).Int("http_status", httpStatus).Int64("latency_ms", latency.Milliseconds()).Msg("mark delivered")
		} else {
			zl.Info().Int("http_status", httpStatus).Int64("latency_ms", latency.Milliseconds()).Msg("webhook delivered")
		}
		return
	}

	lastErr := err.Error()
	evtLog := zl.Warn().Err(err).Int64("latency_ms", latency.Milliseconds())
	if httpStatus > 0 {
		evtLog = evtLog.Int("http_status", httpStatus)
	}
	evtLog.Msg("webhook delivery failed")

	if evt.AttemptCount+1 >= w.maxAttempts {
		if derr := w.outbox.MarkDead(ctx, evt.ID, lastErr); derr != nil {
			zl.Error().Err(derr).Msg("mark dead")
		}
		return
	}

	nextAt := time.Now().UTC().Add(w.nextBackoffDelay(evt.AttemptCount))
	if derr := w.outbox.MarkRetry(ctx, evt.ID, nextAt, lastErr); derr != nil {
		zl.Error().Err(derr).Time("next_retry_at", nextAt).Msg("mark retry")
	}
}

// nextBackoffDelay applies exponential backoff with full jitter (same formula as internal/retry).
func (w *Worker) nextBackoffDelay(attempt int) time.Duration {
	exp := float64(w.cfg.BaseDelay) * math.Pow(w.cfg.BackoffMultiplier, float64(attempt))
	capped := math.Min(exp, float64(w.cfg.MaxBackoff))
	if capped <= 0 {
		return 0
	}
	return time.Duration(rand.Float64() * capped)
}

func (w *Worker) deliver(ctx context.Context, evt *domain.OutboxEvent) (httpStatus int, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSpace(evt.WebhookURL), bytes.NewReader(evt.Payload))
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-PayFlow-Event-Id", evt.ID.String())
	req.Header.Set("X-PayFlow-Event-Type", evt.EventType)
	req.Header.Set("Idempotency-Key", evt.ID.String())
	if sig := w.signPayload(evt.MerchantID, evt.Payload); sig != "" {
		req.Header.Set("X-PayFlow-Signature", sig)
	}

	resp, err := w.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, fmt.Errorf("http status %d", resp.StatusCode)
}

func (w *Worker) signPayload(merchantID uuid.UUID, payload []byte) string {
	if w.registry == nil {
		return ""
	}
	secret, ok := w.registry.Secret(merchantID)
	if !ok || strings.TrimSpace(secret) == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(payload)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
