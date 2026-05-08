package testutil

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/joho/godotenv"

	"github.com/sairamapuroop/PayFlowMock/internal/domain"
)

const testDatabaseURLEnv = "TEST_DATABASE_URL"

var loadTestDotenvOnce sync.Once

// loadTestEnvFromDotenv loads `<module>/.env` once (same keys as testDatabaseURLEnv) so
// `go test` picks up TEST_DATABASE_URL without exporting it in the shell.
func loadTestEnvFromDotenv() {
	loadTestDotenvOnce.Do(func() {
		root, err := moduleRootFromFile()
		if err != nil {
			return
		}
		_ = godotenv.Load(filepath.Join(root, ".env"))
	})
}

func moduleRootFromFile() (string, error) {
	_, file, _, ok := runtime.Caller(1)
	if !ok {
		return "", errors.New("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("go.mod not found")
		}
		dir = parent
	}
}

// MustPool returns a pgx pool using TEST_DATABASE_URL, or skips the test if unset.
func MustPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test (-short)")
	}
	loadTestEnvFromDotenv()
	u := os.Getenv(testDatabaseURLEnv)
	if u == "" {
		t.Skipf("set %s to run integration tests", testDatabaseURLEnv)
	}
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(u)
	if err != nil {
		t.Fatalf("parse %s: %v", testDatabaseURLEnv, err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pgx pool: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping database: %v", err)
	}
	return pool
}

// MustMigrateUp applies all up migrations the same way as cmd/server.
// It re-reads TEST_DATABASE_URL (must be the DSN used for pool). Migration files
// come from MIGRATIONS_PATH, or <module>/migrations/up when that directory exists,
// else <module>/migrations.
func MustMigrateUp(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if pool == nil {
		t.Fatal("pool is nil")
	}
	loadTestEnvFromDotenv()
	u := os.Getenv(testDatabaseURLEnv)
	if u == "" {
		t.Skipf("set %s to run integration tests", testDatabaseURLEnv)
	}
	dir := migrationsDir(t)
	migrateURL, err := migrationsFileURL(dir)
	if err != nil {
		t.Fatalf("migrations path: %v", err)
	}
	if err := runMigrations(u, migrateURL); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
}

// Truncate clears app tables between integration subtests (order safe via CASCADE).
func Truncate(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `TRUNCATE payments, refunds, outbox_events, idempotency_keys RESTART IDENTITY CASCADE`)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
}

// NewPayment returns a valid initiated payment (100 USD minor units) for repository tests.
func NewPayment(t *testing.T, merchantID uuid.UUID, idempotencyKey string) *domain.Payment {
	t.Helper()
	id, err := domain.NewID()
	if err != nil {
		t.Fatalf("new payment id: %v", err)
	}
	now := time.Now().UTC()
	return &domain.Payment{
		ID:             id,
		MerchantID:     merchantID,
		Amount:         *big.NewInt(100),
		Currency:       "USD",
		Status:         domain.StatusInitiated,
		IdempotencyKey: idempotencyKey,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
}

func migrationsDir(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("MIGRATIONS_PATH"); p != "" {
		return p
	}
	root := moduleRoot(t)
	up := filepath.Join(root, "migrations", "up")
	if st, err := os.Stat(up); err == nil && st.IsDir() {
		return up
	}
	return filepath.Join(root, "migrations")
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(1)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found when resolving module root")
		}
		dir = parent
	}
}

func migrationsFileURL(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("file://%s", filepath.ToSlash(abs)), nil
}

func runMigrations(databaseURL, migrationsURL string) error {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return fmt.Errorf("open sql for migrations: %w", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		return fmt.Errorf("ping for migrations: %w", err)
	}

	driver, err := pgxmigrate.WithInstance(db, &pgxmigrate.Config{})
	if err != nil {
		return fmt.Errorf("migrate pgx driver: %w", err)
	}

	m, err := migrate.NewWithDatabaseInstance(migrationsURL, "pgx5", driver)
	if err != nil {
		return fmt.Errorf("migrate instance: %w", err)
	}
	defer m.Close()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}

// combinedMigrationsDir builds a temp dir containing paired *.up.sql / *.down.sql files.
// Up migrations live under migrations/up and downs under migrations/down; golang-migrate requires both in one dir for Down().
func combinedMigrationsDir(t *testing.T) string {
	t.Helper()
	root := moduleRoot(t)
	upDir := filepath.Join(root, "migrations", "up")
	downDir := filepath.Join(root, "migrations", "down")
	ents, err := os.ReadDir(upDir)
	if err != nil {
		t.Fatalf("read up migrations: %v", err)
	}
	var ups []string
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		ups = append(ups, e.Name())
	}
	sort.Strings(ups)
	if len(ups) == 0 {
		t.Fatal("no *.up.sql files in migrations/up")
	}
	dst := t.TempDir()
	for _, name := range ups {
		stem := strings.TrimSuffix(name, ".up.sql")
		upPath := filepath.Join(upDir, name)
		downPath := filepath.Join(downDir, stem+".down.sql")
		if _, err := os.Stat(downPath); err != nil {
			t.Fatalf("missing down migration for %s: %v", stem, err)
		}
		if err := copyFile(filepath.Join(dst, name), upPath); err != nil {
			t.Fatalf("copy %s: %v", name, err)
		}
		if err := copyFile(filepath.Join(dst, stem+".down.sql"), downPath); err != nil {
			t.Fatalf("copy %s.down.sql: %v", stem, err)
		}
	}
	return dst
}

func copyFile(dst, src string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}

// MustMigrateDownAll runs migrate Down until no migrations remain (same DB URL as MustMigrateUp).
func MustMigrateDownAll(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if pool == nil {
		t.Fatal("pool is nil")
	}
	loadTestEnvFromDotenv()
	u := os.Getenv(testDatabaseURLEnv)
	if u == "" {
		t.Skipf("set %s to run integration tests", testDatabaseURLEnv)
	}
	dir := combinedMigrationsDir(t)
	migrateURL, err := migrationsFileURL(dir)
	if err != nil {
		t.Fatalf("migrations path: %v", err)
	}
	if err := runMigrateDownAll(u, migrateURL); err != nil {
		t.Fatalf("migrate down: %v", err)
	}
}

func runMigrateDownAll(databaseURL, migrationsURL string) error {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return fmt.Errorf("open sql for migrations: %w", err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		return fmt.Errorf("ping for migrations: %w", err)
	}
	driver, err := pgxmigrate.WithInstance(db, &pgxmigrate.Config{})
	if err != nil {
		return fmt.Errorf("migrate pgx driver: %w", err)
	}
	m, err := migrate.NewWithDatabaseInstance(migrationsURL, "pgx5", driver)
	if err != nil {
		return fmt.Errorf("migrate instance: %w", err)
	}
	defer m.Close()
	for {
		err := m.Down()
		if errors.Is(err, migrate.ErrNoChange) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("migrate down step: %w", err)
		}
	}
}
