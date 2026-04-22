package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-migrate/migrate/v4"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/joho/godotenv"
	"github.com/rs/zerolog/log"

	"github.com/sairamapuroop/PayFlowMock/internal/cache"
	"github.com/sairamapuroop/PayFlowMock/internal/handler"
	"github.com/sairamapuroop/PayFlowMock/internal/merchant"
	"github.com/sairamapuroop/PayFlowMock/internal/middleware"
	"github.com/sairamapuroop/PayFlowMock/internal/psp"
	"github.com/sairamapuroop/PayFlowMock/internal/repository"
	"github.com/sairamapuroop/PayFlowMock/internal/service"
	"github.com/sairamapuroop/PayFlowMock/internal/worker"
	"github.com/sairamapuroop/PayFlowMock/pkg/logger"
)

func main() {
	logger.Setup("payflow")

	_ = godotenv.Load()

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal().Msg("DATABASE_URL is required")
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	migrationsDir := os.Getenv("MIGRATIONS_PATH")
	if migrationsDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			log.Fatal().Err(err).Msg("get working directory")
		}
		migrationsDir = filepath.Join(wd, "migrations")
	}

	migrationsURL, err := migrationsFileURL(migrationsDir)
	if err != nil {
		log.Fatal().Err(err).Str("dir", migrationsDir).Msg("resolve migrations path")
	}

	if err := runMigrations(databaseURL, migrationsURL); err != nil {
		log.Fatal().Err(err).Msg("database migrations")
	}

	ctx := context.Background()
	poolCfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		log.Fatal().Err(err).Msg("parse DATABASE_URL for pool")
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		log.Fatal().Err(err).Msg("create pgx pool")
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		log.Fatal().Err(err).Msg("ping database")
	}

	redisClient := cache.NewFromEnv()
	defer func() {
		if err := redisClient.Close(); err != nil {
			log.Error().Err(err).Msg("close redis client")
		}
	}()
	if err := redisClient.Ping(ctx); err != nil {
		log.Fatal().Err(err).Msg("ping redis")
	}

	outboxRepo := repository.NewOutboxRepo(pool)
	registry := merchant.NewEnvRegistry(os.Getenv)
	repo := repository.NewPaymentRepo(pool, outboxRepo, registry)
	stripeMock := psp.NewStripeMock(psp.DefaultStripeMockConfig())
	razorpayMock := psp.NewRazorpayMock(psp.DefaultRazorpayMockConfig())
	stripeAdapter := psp.NewCircuitBreakerAdapter(stripeMock)
	razorpayAdapter := psp.NewCircuitBreakerAdapter(razorpayMock)
	router := psp.DefaultRouter(stripeAdapter, razorpayAdapter)
	svc := service.NewPaymentService(repo, router)
	h := handler.NewPaymentHandler(svc, pool)

	idempotency := middleware.NewIdempotency(redisClient.Redis(), pool)
	if v := strings.TrimSpace(os.Getenv("IDEMPOTENCY_REQUEST_TIMEOUT")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			log.Fatal().Err(err).Str("IDEMPOTENCY_REQUEST_TIMEOUT", v).Msg("invalid IDEMPOTENCY_REQUEST_TIMEOUT")
		}
		idempotency.SetRequestTimeout(d)
	}

	r := chi.NewRouter()
	r.Use(idempotency.Middleware)
	h.Register(r)

	addr := ":" + port
	srv := &http.Server{
		Addr:              addr,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}

	shutdownCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	whCfg := worker.DefaultConfig()
	if v := strings.TrimSpace(os.Getenv("WEBHOOK_POLL_INTERVAL")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			log.Fatal().Err(err).Str("WEBHOOK_POLL_INTERVAL", v).Msg("invalid WEBHOOK_POLL_INTERVAL")
		}
		whCfg.PollInterval = d
	}
	webhookWorker := worker.New(outboxRepo, registry, whCfg)

	var workerWg sync.WaitGroup
	workerWg.Add(1)
	go func() {
		defer workerWg.Done()
		webhookWorker.Run(shutdownCtx)
	}()

	go func() {
		log.Info().Str("addr", addr).Msg("http server listening")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal().Err(err).Msg("http server")
		}
	}()

	<-shutdownCtx.Done()
	log.Info().Msg("shutting down")

	workerWg.Wait()

	cancelCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(cancelCtx); err != nil {
		log.Error().Err(err).Msg("http shutdown")
	}
}

func migrationsFileURL(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	// migrate file source expects a URL; absolute POSIX path after file:///
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
