package repository

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sairamapuroop/PayFlowMock/internal/testutil"
)

func TestMigrations_UpDownUp(t *testing.T) {
	ctx := context.Background()
	pool := testutil.MustPool(t)
	testutil.MustMigrateUp(t, pool)

	wantTables := []string{"payments", "refunds", "idempotency_keys", "outbox_events"}
	for _, name := range wantTables {
		if !tableExists(ctx, t, pool, name) {
			t.Fatalf("after migrate up: table %q should exist", name)
		}
	}

	testutil.MustMigrateDownAll(t, pool)

	for _, name := range wantTables {
		if tableExists(ctx, t, pool, name) {
			t.Fatalf("after migrate down: table %q should not exist", name)
		}
	}

	testutil.MustMigrateUp(t, pool)
	for _, name := range wantTables {
		if !tableExists(ctx, t, pool, name) {
			t.Fatalf("after second migrate up: table %q should exist", name)
		}
	}
}

func TestRefunds_ForeignKeyViolatesForMissingPayment(t *testing.T) {
	ctx := context.Background()
	pool := testutil.MustPool(t)
	testutil.MustMigrateUp(t, pool)
	testutil.Truncate(t, pool)

	missingPaymentID := uuid.New()
	key := "fk-refund-" + missingPaymentID.String()

	_, err := pool.Exec(ctx, `
		INSERT INTO refunds (payment_id, amount, status, idempotency_key)
		VALUES ($1, 100, 'INITIATED', $2)
	`, missingPaymentID, key)
	if err == nil {
		t.Fatal("expected foreign key violation for bogus payment_id")
	}

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Fatalf("expected Postgres FK violation (23503), got: %v", err)
	}
}

func TestOutboxEvents_ForeignKeyViolatesForMissingPayment(t *testing.T) {
	ctx := context.Background()
	pool := testutil.MustPool(t)
	testutil.MustMigrateUp(t, pool)
	testutil.Truncate(t, pool)

	missingPaymentID := uuid.New()
	merchantID := uuid.New()

	_, err := pool.Exec(ctx, `
		INSERT INTO outbox_events (id, payment_id, merchant_id, event_type, webhook_url, payload)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb)
	`, uuid.New(), missingPaymentID, merchantID, "PAYMENT_SUCCESS", "https://example.test/hook", "{}")
	if err == nil {
		t.Fatal("expected foreign key violation for bogus payment_id on outbox_events")
	}

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Fatalf("expected Postgres FK violation (23503), got: %v", err)
	}
}

func tableExists(ctx context.Context, t *testing.T, pool *pgxpool.Pool, table string) bool {
	t.Helper()
	var exists bool
	err := pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = 'public' AND table_name = $1
		)
	`, table).Scan(&exists)
	if err != nil {
		t.Fatalf("lookup table %q: %v", table, err)
	}
	return exists
}
