package repository

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sairamapuroop/PayFlowMock/internal/domain"
	"github.com/sairamapuroop/PayFlowMock/internal/merchant"
	"github.com/sairamapuroop/PayFlowMock/internal/testutil"
)

func newOutboxTestDeps(t *testing.T) (context.Context, *pgxpool.Pool, *OutboxRepo, *PaymentRepo) {
	t.Helper()
	ctx := context.Background()
	pool := testutil.MustPool(t)
	testutil.MustMigrateUp(t, pool)
	testutil.Truncate(t, pool)
	outbox := NewOutboxRepo(pool)
	reg := merchant.NewEnvRegistry(func(string) string { return "" })
	payRepo := NewPaymentRepo(pool, outbox, reg)
	return ctx, pool, outbox, payRepo
}

func TestOutboxRepo_EnqueueTx_AtomicWithCallerTx(t *testing.T) {
	ctx, pool, outbox, payRepo := newOutboxTestDeps(t)

	merchantID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	p := testutil.NewPayment(t, merchantID, "outbox-tx-atomic")
	if err := payRepo.CreatePayment(ctx, p); err != nil {
		t.Fatalf("CreatePayment: %v", err)
	}

	eventID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	ev := &domain.OutboxEvent{
		ID:          eventID,
		PaymentID:   p.ID,
		MerchantID:  merchantID,
		EventType:   domain.EventPaymentSuccess,
		WebhookURL:  "https://example.test/hook",
		Payload:     []byte(`{}`),
		AttemptCount: 0,
	}

	t.Run("commit_makes_row_visible", func(t *testing.T) {
		testutil.Truncate(t, pool)
		if err := payRepo.CreatePayment(ctx, p); err != nil {
			t.Fatalf("CreatePayment: %v", err)
		}

		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if err := outbox.EnqueueTx(ctx, tx, ev); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("EnqueueTx: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("Commit: %v", err)
		}

		batch, err := outbox.ClaimBatch(ctx, 10)
		if err != nil {
			t.Fatalf("ClaimBatch: %v", err)
		}
		if len(batch) != 1 {
			t.Fatalf("ClaimBatch len: got %d want 1", len(batch))
		}
		if batch[0].ID != eventID {
			t.Fatalf("ID: got %v want %v", batch[0].ID, eventID)
		}
	})

	t.Run("rollback_leaves_no_row", func(t *testing.T) {
		testutil.Truncate(t, pool)
		if err := payRepo.CreatePayment(ctx, p); err != nil {
			t.Fatalf("CreatePayment: %v", err)
		}

		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if err := outbox.EnqueueTx(ctx, tx, ev); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("EnqueueTx: %v", err)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("Rollback: %v", err)
		}

		var n int
		if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM outbox_events`).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 0 {
			t.Fatalf("outbox row leaked after rollback: count=%d", n)
		}
	})
}

func TestOutboxRepo_ClaimBatch_DueAndStampsProcessing(t *testing.T) {
	ctx, pool, outbox, payRepo := newOutboxTestDeps(t)
	merchantID := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")

	now := time.Now().UTC()
	past := now.Add(-30 * time.Minute)
	future := now.Add(1 * time.Hour)
	pTime := past.Add(2 * time.Minute)
	pNear := past.Add(1 * time.Minute)
	pFar := past.Add(3 * time.Minute)

	insertOutbox := func(t *testing.T, id, paymentID uuid.UUID, status string, nextRetry time.Time) {
		t.Helper()
		_, err := pool.Exec(ctx, `
			INSERT INTO outbox_events (
				id, payment_id, merchant_id, event_type, webhook_url, payload,
				status, attempt_count, next_retry_at, created_at, updated_at
			) VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7,0,$8,$9,$10)`,
			id, paymentID, merchantID, domain.EventPaymentSuccess, "https://x.test", "{}",
			status, nextRetry, past, past,
		)
		if err != nil {
			t.Fatalf("insert outbox %s: %v", id, err)
		}
	}

	// Due PENDING rows (order by next_retry_at: near, time, far)
	idNear := uuid.MustParse("d0000001-0000-0000-0000-000000000001")
	idTime := uuid.MustParse("d0000002-0000-0000-0000-000000000002")
	idFar := uuid.MustParse("d0000003-0000-0000-0000-000000000003")
	// Excluded: future PENDING
	idFuture := uuid.MustParse("d0000004-0000-0000-0000-000000000004")
	// Excluded: non-pending
	idProcessing := uuid.MustParse("d0000005-0000-0000-0000-000000000005")
	idDelivered := uuid.MustParse("d0000006-0000-0000-0000-000000000006")
	idDead := uuid.MustParse("d0000007-0000-0000-0000-000000000007")

	ids := []uuid.UUID{
		idNear, idTime, idFar, idFuture, idProcessing, idDelivered, idDead,
	}
	for _, oid := range ids {
		p := testutil.NewPayment(t, merchantID, "claim-"+oid.String())
		if err := payRepo.CreatePayment(ctx, p); err != nil {
			t.Fatalf("CreatePayment: %v", err)
		}
		switch oid {
		case idNear:
			insertOutbox(t, oid, p.ID, domain.OutboxStatusPending, pNear)
		case idTime:
			insertOutbox(t, oid, p.ID, domain.OutboxStatusPending, pTime)
		case idFar:
			insertOutbox(t, oid, p.ID, domain.OutboxStatusPending, pFar)
		case idFuture:
			insertOutbox(t, oid, p.ID, domain.OutboxStatusPending, future)
		case idProcessing:
			insertOutbox(t, oid, p.ID, domain.OutboxStatusProcessing, past)
		case idDelivered:
			insertOutbox(t, oid, p.ID, domain.OutboxStatusDelivered, past)
		case idDead:
			insertOutbox(t, oid, p.ID, domain.OutboxStatusDead, past)
		}
	}

	batch, err := outbox.ClaimBatch(ctx, 2)
	if err != nil {
		t.Fatalf("ClaimBatch: %v", err)
	}
	if len(batch) != 2 {
		t.Fatalf("len: got %d want 2", len(batch))
	}
	if batch[0].ID != idNear || batch[1].ID != idTime {
		t.Fatalf("order/wrong ids: got [%v %v] want [%v %v]", batch[0].ID, batch[1].ID, idNear, idTime)
	}
	for _, e := range batch {
		if e.Status != domain.OutboxStatusProcessing {
			t.Fatalf("status %v: got %q want PROCESSING", e.ID, e.Status)
		}
	}

	wantStatus := func(id uuid.UUID, want string) {
		t.Helper()
		var st string
		err := pool.QueryRow(ctx, `SELECT status FROM outbox_events WHERE id = $1`, id).Scan(&st)
		if err != nil {
			t.Fatalf("status %v: %v", id, err)
		}
		if st != want {
			t.Fatalf("id %v: got status %q want %q", id, st, want)
		}
	}
	wantStatus(idTime, domain.OutboxStatusProcessing)
	wantStatus(idNear, domain.OutboxStatusProcessing)
	wantStatus(idFar, domain.OutboxStatusPending)
	wantStatus(idFuture, domain.OutboxStatusPending)
	wantStatus(idProcessing, domain.OutboxStatusProcessing)
	wantStatus(idDelivered, domain.OutboxStatusDelivered)
	wantStatus(idDead, domain.OutboxStatusDead)
}

func TestOutboxRepo_ClaimBatch_SkipLockedDisjoint(t *testing.T) {
	ctx, pool, outbox, payRepo := newOutboxTestDeps(t)
	merchantID := uuid.MustParse("eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee")
	const n = 5
	past := time.Now().UTC().Add(-time.Minute)

	var ids []uuid.UUID
	for i := 0; i < 2*n; i++ {
		p := testutil.NewPayment(t, merchantID, "skip-lock-"+uuid.NewString())
		if err := payRepo.CreatePayment(ctx, p); err != nil {
			t.Fatalf("CreatePayment: %v", err)
		}
		oid := uuid.New()
		ids = append(ids, oid)
		_, err := pool.Exec(ctx, `
			INSERT INTO outbox_events (
				id, payment_id, merchant_id, event_type, webhook_url, payload,
				status, attempt_count, next_retry_at, created_at, updated_at
			) VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7,0,$8,$9,$10)`,
			oid, p.ID, merchantID, domain.EventPaymentSuccess, "https://x.test", "{}",
			domain.OutboxStatusPending, past, past, past,
		)
		if err != nil {
			t.Fatalf("insert outbox: %v", err)
		}
	}

	var (
		mu     sync.Mutex
		first  []*domain.OutboxEvent
		second []*domain.OutboxEvent
		wg     sync.WaitGroup
	)
	start := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		b, err := outbox.ClaimBatch(ctx, n)
		if err != nil {
			t.Errorf("goroutine 1 ClaimBatch: %v", err)
			return
		}
		mu.Lock()
		first = b
		mu.Unlock()
	}()
	go func() {
		defer wg.Done()
		<-start
		b, err := outbox.ClaimBatch(ctx, n)
		if err != nil {
			t.Errorf("goroutine 2 ClaimBatch: %v", err)
			return
		}
		mu.Lock()
		second = b
		mu.Unlock()
	}()
	close(start)
	wg.Wait()

	if len(first) != n || len(second) != n {
		t.Fatalf("batch sizes: got %d and %d want %d each", len(first), len(second), n)
	}
	seen := make(map[uuid.UUID]struct{}, 2*n)
	for _, b := range [][]*domain.OutboxEvent{first, second} {
		for _, e := range b {
			if _, dup := seen[e.ID]; dup {
				t.Fatalf("duplicate claim for id %v", e.ID)
			}
			seen[e.ID] = struct{}{}
		}
	}
	if len(seen) != 2*n {
		t.Fatalf("unique ids: got %d want %d", len(seen), 2*n)
	}
}

func TestOutboxRepo_MarkLifecycleGuards(t *testing.T) {
	ctx, pool, outbox, payRepo := newOutboxTestDeps(t)
	merchantID := uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")

	newProcessingEvent := func(t *testing.T) uuid.UUID {
		t.Helper()
		p := testutil.NewPayment(t, merchantID, "mark-life-"+uuid.NewString())
		if err := payRepo.CreatePayment(ctx, p); err != nil {
			t.Fatalf("CreatePayment: %v", err)
		}
		past := time.Now().UTC().Add(-time.Minute)
		oid := uuid.New()
		_, err := pool.Exec(ctx, `
			INSERT INTO outbox_events (
				id, payment_id, merchant_id, event_type, webhook_url, payload,
				status, attempt_count, next_retry_at, created_at, updated_at
			) VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7,0,$8,$9,$10)`,
			oid, p.ID, merchantID, domain.EventPaymentSuccess, "https://x.test", "{}",
			domain.OutboxStatusPending, past, past, past,
		)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		batch, err := outbox.ClaimBatch(ctx, 1)
		if err != nil {
			t.Fatalf("ClaimBatch: %v", err)
		}
		if len(batch) != 1 || batch[0].ID != oid {
			t.Fatalf("expected one claimed event")
		}
		return oid
	}

	t.Run("MarkDelivered_happy", func(t *testing.T) {
		id := newProcessingEvent(t)
		if err := outbox.MarkDelivered(ctx, id); err != nil {
			t.Fatalf("MarkDelivered: %v", err)
		}
		var st string
		var deliveredAt *time.Time
		err := pool.QueryRow(ctx, `
			SELECT status, delivered_at FROM outbox_events WHERE id = $1`, id).Scan(&st, &deliveredAt)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if st != domain.OutboxStatusDelivered {
			t.Fatalf("status: got %q", st)
		}
		if deliveredAt == nil || deliveredAt.IsZero() {
			t.Fatal("delivered_at should be set")
		}
	})

	t.Run("MarkRetry_happy", func(t *testing.T) {
		id := newProcessingEvent(t)
		next := time.Now().UTC().Add(5 * time.Minute)
		if err := outbox.MarkRetry(ctx, id, next, "boom"); err != nil {
			t.Fatalf("MarkRetry: %v", err)
		}
		var st string
		var attempts int
		var nextAt time.Time
		var lastErr *string
		err := pool.QueryRow(ctx, `
			SELECT status, attempt_count, next_retry_at, last_error FROM outbox_events WHERE id = $1`,
			id).Scan(&st, &attempts, &nextAt, &lastErr)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if st != domain.OutboxStatusPending {
			t.Fatalf("status: got %q", st)
		}
		if attempts != 1 {
			t.Fatalf("attempt_count: got %d want 1", attempts)
		}
		if !nextAt.Equal(next) {
			t.Fatalf("next_retry_at: got %v want %v", nextAt, next)
		}
		if lastErr == nil || *lastErr != "boom" {
			t.Fatalf("last_error: got %v", lastErr)
		}
	})

	t.Run("MarkDead_happy", func(t *testing.T) {
		id := newProcessingEvent(t)
		if err := outbox.MarkDead(ctx, id, "give up"); err != nil {
			t.Fatalf("MarkDead: %v", err)
		}
		var st string
		var lastErr *string
		err := pool.QueryRow(ctx, `SELECT status, last_error FROM outbox_events WHERE id = $1`, id).Scan(&st, &lastErr)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if st != domain.OutboxStatusDead {
			t.Fatalf("status: got %q", st)
		}
		if lastErr == nil || *lastErr != "give up" {
			t.Fatalf("last_error: got %v", lastErr)
		}
	})

	wrongStates := []struct {
		name string
		fn   func(context.Context, uuid.UUID) error
	}{
		{"MarkDelivered", func(c context.Context, id uuid.UUID) error { return outbox.MarkDelivered(c, id) }},
		{"MarkRetry", func(c context.Context, id uuid.UUID) error {
			return outbox.MarkRetry(c, id, time.Now().UTC().Add(time.Minute), "x")
		}},
		{"MarkDead", func(c context.Context, id uuid.UUID) error { return outbox.MarkDead(c, id, "x") }},
	}
	for _, op := range wrongStates {
		t.Run(op.name+"_rejects_non_processing", func(t *testing.T) {
			p := testutil.NewPayment(t, merchantID, "wrong-"+uuid.NewString())
			if err := payRepo.CreatePayment(ctx, p); err != nil {
				t.Fatalf("CreatePayment: %v", err)
			}
			id := uuid.New()
			past := time.Now().UTC().Add(-time.Minute)
			_, err := pool.Exec(ctx, `
				INSERT INTO outbox_events (
					id, payment_id, merchant_id, event_type, webhook_url, payload,
					status, attempt_count, next_retry_at, created_at, updated_at
				) VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7,0,$8,$9,$10)`,
				id, p.ID, merchantID, domain.EventPaymentSuccess, "https://x.test", "{}",
				domain.OutboxStatusPending, past, past, past,
			)
			if err != nil {
				t.Fatalf("insert: %v", err)
			}
			if err := op.fn(ctx, id); !errors.Is(err, errOutboxEventNotFound) {
				t.Fatalf("got %v want errOutboxEventNotFound", err)
			}
		})
	}
}

func TestOutboxRepo_ReclaimStaleProcessing_OnlyOldRows(t *testing.T) {
	ctx, pool, outbox, payRepo := newOutboxTestDeps(t)
	merchantID := uuid.MustParse("12121212-1212-1212-1212-121212121212")

	insertProcessing := func(t *testing.T, updatedAt time.Time) uuid.UUID {
		t.Helper()
		p := testutil.NewPayment(t, merchantID, "reclaim-"+uuid.NewString())
		if err := payRepo.CreatePayment(ctx, p); err != nil {
			t.Fatalf("CreatePayment: %v", err)
		}
		id := uuid.New()
		base := time.Now().UTC().Add(-24 * time.Hour)
		_, err := pool.Exec(ctx, `
			INSERT INTO outbox_events (
				id, payment_id, merchant_id, event_type, webhook_url, payload,
				status, attempt_count, next_retry_at, created_at, updated_at
			) VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7,0,$8,$9,$10)`,
			id, p.ID, merchantID, domain.EventPaymentSuccess, "https://x.test", "{}",
			domain.OutboxStatusProcessing, base, base, updatedAt,
		)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		return id
	}

	staleAt := time.Now().UTC().Add(-2 * time.Hour)
	freshAt := time.Now().UTC().Add(-30 * time.Second)
	idStale := insertProcessing(t, staleAt)
	idFresh := insertProcessing(t, freshAt)

	n, err := outbox.ReclaimStaleProcessing(ctx, 1*time.Hour)
	if err != nil {
		t.Fatalf("ReclaimStaleProcessing: %v", err)
	}
	if n != 1 {
		t.Fatalf("rows affected: got %d want 1", n)
	}

	var staleStatus string
	var staleNext time.Time
	if err := pool.QueryRow(ctx, `SELECT status, next_retry_at FROM outbox_events WHERE id = $1`, idStale).
		Scan(&staleStatus, &staleNext); err != nil {
		t.Fatalf("stale row: %v", err)
	}
	if staleStatus != domain.OutboxStatusPending {
		t.Fatalf("stale status: got %q want PENDING", staleStatus)
	}
	if time.Now().UTC().Sub(staleNext) > 2*time.Minute || staleNext.After(time.Now().UTC().Add(1*time.Minute)) {
		t.Fatalf("stale next_retry_at not near now: %v", staleNext)
	}

	var freshStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM outbox_events WHERE id = $1`, idFresh).Scan(&freshStatus); err != nil {
		t.Fatalf("fresh row: %v", err)
	}
	if freshStatus != domain.OutboxStatusProcessing {
		t.Fatalf("fresh status: got %q want PROCESSING", freshStatus)
	}
}
